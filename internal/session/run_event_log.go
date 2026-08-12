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
	"strings"
	"sync"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/fslock"
)

const (
	RunEventSchemaVersion           = 1
	RunEventIncompleteSchemaVersion = 1
	runEventDirName                 = ".run-events"
	// DefaultRunEventRetention covers roughly a month of daily attempts for a
	// long-lived interactive session. When it binds, only older observation-only
	// logs are deleted; session history and recovery state are untouched. Raise
	// agent.run_event_retention when a longer diagnostic window is required.
	DefaultRunEventRetention = 32

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
	mu             sync.Mutex
	path           string
	lockPath       string
	incompletePath string
	sessionID      string
	runID          string
	attemptID      string
	cacheReady     bool
	cachedSize     int64
	lastSeq        int64
	bySeq          map[int64][]byte
	scanCount      int // package tests pin steady-state append complexity
}

func (s *Store) OpenRunEventLog(sessionID, runID, attemptID string, maxAttempts int) (*RunEventLog, error) {
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
	log := &RunEventLog{
		path:           filepath.Join(dir, attemptID+".jsonl"),
		lockPath:       filepath.Join(dir, attemptID+".lock"),
		incompletePath: filepath.Join(dir, attemptID+".incomplete"),
		sessionID:      sessionID,
		runID:          runID,
		attemptID:      attemptID,
	}
	// Materialize the attempt identity before pruning so the current attempt is
	// counted and explicitly protected even before its first event is flushed.
	lockFile, err := os.OpenFile(log.lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("create run event lock: %w", err)
	}
	if err := lockFile.Close(); err != nil {
		return nil, fmt.Errorf("close run event lock: %w", err)
	}
	if maxAttempts > 0 {
		pruneRunEventAttempts(filepath.Join(s.dir, runEventDirName, sessionID), runID, attemptID, maxAttempts)
	}
	return log, nil
}

type runEventAttemptFiles struct {
	runID     string
	attemptID string
	modTime   time.Time
	paths     []string
	lockPath  string
}

func collectRunEventAttempts(sessionRoot string) ([]runEventAttemptFiles, error) {
	runs, err := os.ReadDir(sessionRoot)
	if err != nil {
		return nil, err
	}
	groups := make(map[string]*runEventAttemptFiles)
	for _, run := range runs {
		if !run.IsDir() || !IsValidRunID(run.Name()) {
			continue
		}
		runDir := filepath.Join(sessionRoot, run.Name())
		files, err := os.ReadDir(runDir)
		if err != nil {
			continue
		}
		for _, file := range files {
			if file.IsDir() {
				continue
			}
			name := file.Name()
			var attemptID string
			switch {
			case strings.HasSuffix(name, ".jsonl"):
				attemptID = strings.TrimSuffix(name, ".jsonl")
			case strings.HasSuffix(name, ".incomplete"):
				attemptID = strings.TrimSuffix(name, ".incomplete")
			case strings.HasSuffix(name, ".lock"):
				attemptID = strings.TrimSuffix(name, ".lock")
			default:
				continue
			}
			if !IsValidAttemptID(attemptID) {
				continue
			}
			key := run.Name() + "\x00" + attemptID
			group := groups[key]
			if group == nil {
				group = &runEventAttemptFiles{
					runID: run.Name(), attemptID: attemptID,
					lockPath: filepath.Join(runDir, attemptID+".lock"),
				}
				groups[key] = group
			}
			path := filepath.Join(runDir, name)
			group.paths = append(group.paths, path)
			if info, infoErr := file.Info(); infoErr == nil && info.ModTime().After(group.modTime) {
				group.modTime = info.ModTime()
			}
		}
	}
	attempts := make([]runEventAttemptFiles, 0, len(groups))
	for _, group := range groups {
		attempts = append(attempts, *group)
	}
	sort.Slice(attempts, func(i, j int) bool {
		if attempts[i].modTime.Equal(attempts[j].modTime) {
			if attempts[i].runID == attempts[j].runID {
				return attempts[i].attemptID < attempts[j].attemptID
			}
			return attempts[i].runID < attempts[j].runID
		}
		return attempts[i].modTime.Before(attempts[j].modTime)
	})
	return attempts, nil
}

func removeRunEventAttempt(attempt runEventAttemptFiles) int {
	hadLock := false
	for _, path := range attempt.paths {
		if path == attempt.lockPath {
			hadLock = true
			break
		}
	}
	lockFile, err := os.OpenFile(attempt.lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return 0
	}
	if err := fslock.TryLock(lockFile.Fd()); err != nil {
		lockFile.Close()
		return 0
	}
	removed := 0
	for _, path := range attempt.paths {
		if path == attempt.lockPath {
			continue
		}
		if err := os.Remove(path); err == nil {
			removed++
		}
	}
	_ = fslock.Unlock(lockFile.Fd())
	_ = lockFile.Close()
	if err := os.Remove(attempt.lockPath); err == nil && hadLock {
		removed++
	}
	return removed
}

func pruneRunEventAttempts(sessionRoot, protectedRunID, protectedAttemptID string, keep int) {
	if keep <= 0 {
		return
	}
	attempts, err := collectRunEventAttempts(sessionRoot)
	if err != nil || len(attempts) <= keep {
		return
	}
	remaining := len(attempts)
	for _, attempt := range attempts {
		if remaining <= keep {
			break
		}
		if attempt.runID == protectedRunID && attempt.attemptID == protectedAttemptID {
			continue
		}
		if removeRunEventAttempt(attempt) > 0 {
			remaining--
		}
	}
}

// SweepStaleRunEvents removes observation-only attempt files older than
// maxAge from one sessions directory. It never touches session JSON or replay
// state. Active attempts are skipped when their advisory lock is held.
func SweepStaleRunEvents(sessionsDir string, maxAge time.Duration) (int, error) {
	if sessionsDir == "" || maxAge <= 0 {
		return 0, nil
	}
	root := filepath.Join(filepath.Clean(sessionsDir), runEventDirName)
	sessions, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	cutoff := time.Now().Add(-maxAge)
	removed := 0
	for _, entry := range sessions {
		if !entry.IsDir() {
			continue
		}
		attempts, err := collectRunEventAttempts(filepath.Join(root, entry.Name()))
		if err != nil {
			continue
		}
		for _, attempt := range attempts {
			if !attempt.modTime.IsZero() && attempt.modTime.Before(cutoff) {
				removed += removeRunEventAttempt(attempt)
			}
		}
	}
	return removed, nil
}

// SweepOrphanRunEvents removes per-session observation directories whose
// authoritative session JSON no longer exists. NewStore gates this to the
// first construction per sessions directory so it runs before normal traffic.
func (s *Store) SweepOrphanRunEvents() {
	root := filepath.Join(s.dir, runEventDirName)
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		sessionPath, err := s.safeSessionPath(entry.Name())
		if err != nil {
			continue
		}
		if _, err := os.Stat(sessionPath); os.IsNotExist(err) {
			_ = os.RemoveAll(filepath.Join(root, entry.Name()))
		}
	}
}

func (l *RunEventLog) withLock(fn func() error) error {
	if l == nil {
		return errors.New("run event log is nil")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
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

// refreshAppendCache scans only on first use or after another RunEventLog
// instance changed the file size. The daemon normally owns one instance per
// attempt, so steady-state appends validate against this in-memory canonical
// index instead of reparsing the full JSONL file on every checkpoint. The
// advisory file lock remains the cross-process authority; a concurrent writer
// changes the size and forces a rescan before this instance appends again.
func (l *RunEventLog) refreshAppendCache(file *os.File) error {
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if l.cacheReady && info.Size() == l.cachedSize {
		return nil
	}
	records, bySeq, err := l.scanAndRepair(file)
	if err != nil {
		l.cacheReady = false
		return err
	}
	info, err = file.Stat()
	if err != nil {
		l.cacheReady = false
		return err
	}
	l.cacheReady = true
	l.cachedSize = info.Size()
	l.lastSeq = int64(len(records))
	l.bySeq = bySeq
	return nil
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
	l.scanCount++
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
		if err := l.refreshAppendCache(file); err != nil {
			return fail(err)
		}
		lastSeq := l.lastSeq
		pendingBySeq := make(map[int64][]byte)
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
			prior, exists := l.bySeq[record.Event.Seq]
			if !exists {
				prior, exists = pendingBySeq[record.Event.Seq]
			}
			if exists {
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
			pendingBySeq[record.Event.Seq] = canonical
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
		for seq, canonical := range pendingBySeq {
			l.bySeq[seq] = canonical
		}
		l.lastSeq = lastSeq
		if info, statErr := file.Stat(); statErr == nil {
			l.cachedSize = info.Size()
		} else {
			// The append is durable, but without a size identity the next call
			// must rebuild the cache before trusting it.
			l.cacheReady = false
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
		if info, statErr := file.Stat(); statErr == nil {
			l.cacheReady = true
			l.cachedSize = info.Size()
			l.lastSeq = int64(len(loaded))
			l.bySeq = make(map[int64][]byte, len(loaded))
			for _, record := range loaded {
				canonical, encodeErr := canonicalRunEvent(record)
				if encodeErr != nil {
					l.cacheReady = false
					return encodeErr
				}
				l.bySeq[record.Event.Seq] = canonical
			}
		}
		records = loaded
		return nil
	})
	return records, err
}
