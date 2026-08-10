package session

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/fslock"
)

const (
	RunEventSchemaVersion           = 1
	RunEventIncompleteSchemaVersion = 1
	runEventDirName                 = ".run-events"

	RunEventIncompleteCategoryLock       = "lock_failed"
	RunEventIncompleteCategoryOpen       = "open_failed"
	RunEventIncompleteCategoryScan       = "scan_failed"
	RunEventIncompleteCategoryValidation = "validation_failed"
	RunEventIncompleteCategoryEncoding   = "encoding_failed"
	RunEventIncompleteCategoryWrite      = "write_failed"
	RunEventIncompleteCategorySync       = "sync_failed"
	RunEventIncompleteCategoryClear      = "marker_clear_failed"
)

var (
	validRunIDPattern     = regexp.MustCompile(`^run1_[0-9a-f]{32}$`)
	validAttemptIDPattern = regexp.MustCompile(`^att1_[0-9a-f]{32}$`)
)

func IsValidRunID(id string) bool     { return validRunIDPattern.MatchString(id) }
func IsValidAttemptID(id string) bool { return validAttemptIDPattern.MatchString(id) }

// RunEventRecord is one content-free observation from a single AgentLoop
// attempt. RunID is stable across interrupted recovery; AttemptID changes for
// every reconstructed loop, so the loop-local sequence may safely restart at 1.
type RunEventRecord struct {
	SchemaVersion int                 `json:"schema_version"`
	SessionID     string              `json:"session_id"`
	RunID         string              `json:"run_id"`
	AttemptID     string              `json:"attempt_id"`
	RecordedAt    time.Time           `json:"recorded_at"`
	Event         agent.RunTraceEvent `json:"event"`
}

// RunEventIncompleteMarker is a content-free durable signal that at least one
// append for this attempt did not complete. Category is deliberately bounded
// to implementation-owned values; raw errors and event content are never
// persisted here.
type RunEventIncompleteMarker struct {
	SchemaVersion int       `json:"schema_version"`
	MarkedAt      time.Time `json:"marked_at"`
	Category      string    `json:"category"`
}

// RunEventLog owns one per-attempt append-only JSONL file. It is observation
// only: recovery and side-effect replay decisions remain authoritative in the
// session checkpoint, never in this log.
type RunEventLog struct {
	path           string
	lockPath       string
	incompletePath string
	sessionID      string
	runID          string
	attemptID      string
}

func (s *Store) OpenRunEventLog(sessionID, runID, attemptID string) (*RunEventLog, error) {
	if _, err := s.safeSessionPath(sessionID); err != nil {
		return nil, err
	}
	if !validRunIDPattern.MatchString(runID) {
		return nil, fmt.Errorf("invalid run id %q", runID)
	}
	if !validAttemptIDPattern.MatchString(attemptID) {
		return nil, fmt.Errorf("invalid attempt id %q", attemptID)
	}
	dir := filepath.Join(s.dir, runEventDirName, sessionID, runID)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("create run event directory: %w", err)
	}
	return &RunEventLog{
		path:           filepath.Join(dir, attemptID+".jsonl"),
		lockPath:       filepath.Join(dir, attemptID+".lock"),
		incompletePath: filepath.Join(dir, attemptID+".incomplete"),
		sessionID:      sessionID,
		runID:          runID,
		attemptID:      attemptID,
	}, nil
}

func (l *RunEventLog) withLock(fn func() error) error {
	if l == nil {
		return errors.New("run event log is nil")
	}
	lockFile, err := os.OpenFile(l.lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return fmt.Errorf("open run event lock: %w", err)
	}
	defer lockFile.Close()
	if err := fslock.Lock(lockFile.Fd()); err != nil {
		return fmt.Errorf("lock run event log: %w", err)
	}
	defer fslock.Unlock(lockFile.Fd())
	return fn()
}

func (l *RunEventLog) validateRecord(record RunEventRecord) error {
	if record.SchemaVersion != RunEventSchemaVersion {
		return fmt.Errorf("run event schema version %d is not supported", record.SchemaVersion)
	}
	if record.SessionID != l.sessionID || record.RunID != l.runID || record.AttemptID != l.attemptID {
		return fmt.Errorf("run event identity does not match log path")
	}
	if record.RecordedAt.IsZero() {
		return errors.New("run event recorded_at is required")
	}
	if record.Event.Seq <= 0 {
		return fmt.Errorf("run event sequence %d must be positive", record.Event.Seq)
	}
	return nil
}

func canonicalRunEvent(record RunEventRecord) ([]byte, error) {
	return json.Marshal(record)
}

func (l *RunEventLog) markIncomplete(category string) {
	if l == nil || l.incompletePath == "" {
		return
	}
	marker := RunEventIncompleteMarker{
		SchemaVersion: RunEventIncompleteSchemaVersion,
		MarkedAt:      time.Now().UTC(),
		Category:      category,
	}
	payload, err := json.Marshal(marker)
	if err != nil {
		return
	}
	payload = append(payload, '\n')
	file, err := os.OpenFile(l.incompletePath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return
	}
	defer file.Close()
	// O_CREATE does not narrow an existing marker's permissions.
	if err := file.Chmod(0600); err != nil {
		return
	}
	for len(payload) > 0 {
		written, writeErr := file.Write(payload)
		if writeErr != nil || written == 0 {
			return
		}
		payload = payload[written:]
	}
	_ = file.Sync()
}

func (l *RunEventLog) clearIncomplete() error {
	err := os.Remove(l.incompletePath)
	if err == nil || os.IsNotExist(err) {
		return nil
	}
	return fmt.Errorf("clear run event incomplete marker: %w", err)
}

func (l *RunEventLog) scanAndRepair(file *os.File) ([]RunEventRecord, map[int64][]byte, error) {
	if _, err := file.Seek(0, 0); err != nil {
		return nil, nil, err
	}
	data, err := io.ReadAll(file)
	if err != nil {
		return nil, nil, err
	}
	completeLen := len(data)
	if len(data) > 0 && data[len(data)-1] != '\n' {
		lastNewline := bytes.LastIndexByte(data, '\n')
		completeLen = lastNewline + 1
		if err := file.Truncate(int64(completeLen)); err != nil {
			return nil, nil, fmt.Errorf("truncate incomplete run event tail: %w", err)
		}
		if err := file.Sync(); err != nil {
			return nil, nil, fmt.Errorf("sync repaired run event log: %w", err)
		}
		data = data[:completeLen]
	}

	bySeq := make(map[int64][]byte)
	unique := make(map[int64]RunEventRecord)
	for lineIndex, line := range bytes.Split(data, []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		var record RunEventRecord
		if err := json.Unmarshal(line, &record); err != nil {
			return nil, nil, fmt.Errorf("decode run event line %d: %w", lineIndex+1, err)
		}
		if err := l.validateRecord(record); err != nil {
			return nil, nil, fmt.Errorf("validate run event line %d: %w", lineIndex+1, err)
		}
		canonical, err := canonicalRunEvent(record)
		if err != nil {
			return nil, nil, err
		}
		if prior, exists := bySeq[record.Event.Seq]; exists {
			if !bytes.Equal(prior, canonical) {
				return nil, nil, fmt.Errorf("conflicting duplicate run event sequence %d", record.Event.Seq)
			}
			continue
		}
		bySeq[record.Event.Seq] = canonical
		unique[record.Event.Seq] = record
	}

	seqs := make([]int64, 0, len(unique))
	for seq := range unique {
		seqs = append(seqs, seq)
	}
	sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })
	records := make([]RunEventRecord, 0, len(seqs))
	for index, seq := range seqs {
		want := int64(index + 1)
		if seq != want {
			return nil, nil, fmt.Errorf("run event sequence gap: got %d, want %d", seq, want)
		}
		records = append(records, unique[seq])
	}
	return records, bySeq, nil
}

// Append writes a contiguous batch, suppressing identical retry overlap and
// rejecting conflicting duplicates or sequence gaps. A partial final JSON line
// from a crashed write is truncated before validation and append.
func (l *RunEventLog) Append(batch []RunEventRecord) error {
	if len(batch) == 0 {
		return nil
	}
	category := RunEventIncompleteCategoryLock
	markedUnderLock := false
	err := l.withLock(func() error {
		fail := func(err error) error {
			markedUnderLock = true
			l.markIncomplete(category)
			return err
		}
		category = RunEventIncompleteCategoryOpen
		file, err := os.OpenFile(l.path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0600)
		if err != nil {
			return fail(fmt.Errorf("open run event log: %w", err))
		}
		defer file.Close()
		category = RunEventIncompleteCategoryScan
		records, bySeq, err := l.scanAndRepair(file)
		if err != nil {
			return fail(err)
		}
		lastSeq := int64(len(records))
		var encoded bytes.Buffer
		for _, record := range batch {
			category = RunEventIncompleteCategoryValidation
			if err := l.validateRecord(record); err != nil {
				return fail(err)
			}
			category = RunEventIncompleteCategoryEncoding
			canonical, err := canonicalRunEvent(record)
			if err != nil {
				return fail(err)
			}
			if prior, exists := bySeq[record.Event.Seq]; exists {
				if !bytes.Equal(prior, canonical) {
					category = RunEventIncompleteCategoryValidation
					return fail(fmt.Errorf("conflicting duplicate run event sequence %d", record.Event.Seq))
				}
				continue
			}
			if record.Event.Seq != lastSeq+1 {
				category = RunEventIncompleteCategoryValidation
				return fail(fmt.Errorf("run event sequence gap: got %d, want %d", record.Event.Seq, lastSeq+1))
			}
			encoded.Write(canonical)
			encoded.WriteByte('\n')
			bySeq[record.Event.Seq] = canonical
			lastSeq = record.Event.Seq
		}
		payload := encoded.Bytes()
		category = RunEventIncompleteCategoryWrite
		for len(payload) > 0 {
			written, err := file.Write(payload)
			if err != nil {
				return fail(fmt.Errorf("append run events: %w", err))
			}
			if written == 0 {
				return fail(io.ErrShortWrite)
			}
			payload = payload[written:]
		}
		category = RunEventIncompleteCategorySync
		if err := file.Sync(); err != nil {
			return fail(fmt.Errorf("sync run event log: %w", err))
		}
		category = RunEventIncompleteCategoryClear
		if err := l.clearIncomplete(); err != nil {
			return fail(err)
		}
		return nil
	})
	if err != nil && !markedUnderLock {
		// Lock/open failures may prevent serialized marker creation. This second
		// path is intentionally best-effort and still stores only the category.
		l.markIncomplete(category)
	}
	return err
}

// ReadIncompleteMarker reports whether this attempt has a durable incomplete
// marker. A present but malformed marker returns present=true with an error so
// offline consumers fail closed instead of treating corruption as complete.
func (l *RunEventLog) ReadIncompleteMarker() (RunEventIncompleteMarker, bool, error) {
	var marker RunEventIncompleteMarker
	present := false
	err := l.withLock(func() error {
		payload, err := os.ReadFile(l.incompletePath)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read run event incomplete marker: %w", err)
		}
		present = true
		if err := json.Unmarshal(payload, &marker); err != nil {
			return fmt.Errorf("decode run event incomplete marker: %w", err)
		}
		if marker.SchemaVersion != RunEventIncompleteSchemaVersion {
			return fmt.Errorf("run event incomplete marker schema version %d is not supported", marker.SchemaVersion)
		}
		if marker.MarkedAt.IsZero() || marker.Category == "" {
			return errors.New("run event incomplete marker is missing required fields")
		}
		return nil
	})
	return marker, present, err
}

func (l *RunEventLog) ReadAll() ([]RunEventRecord, error) {
	var records []RunEventRecord
	err := l.withLock(func() error {
		file, err := os.OpenFile(l.path, os.O_CREATE|os.O_RDWR, 0600)
		if err != nil {
			return err
		}
		defer file.Close()
		loaded, _, err := l.scanAndRepair(file)
		if err != nil {
			return err
		}
		records = loaded
		return nil
	})
	return records, err
}
