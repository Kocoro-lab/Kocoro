package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
)

const (
	testRunID     = "run1_0123456789abcdef0123456789abcdef"
	testAttemptID = "att1_0123456789abcdef0123456789abcdef"
)

func testRunEvent(sessionID string, seq int64, typ agent.RunTraceEventType) RunEventRecord {
	return RunEventRecord{
		SchemaVersion: RunEventSchemaVersion,
		SessionID:     sessionID,
		RunID:         testRunID,
		AttemptID:     testAttemptID,
		RecordedAt:    time.Date(2026, 8, 10, 8, 0, int(seq), 0, time.UTC),
		Event:         agent.RunTraceEvent{Seq: seq, Iteration: int(seq), Type: typ},
	}
}

func openTestRunEventLog(t *testing.T) (*RunEventLog, string) {
	t.Helper()
	const sessionID = "session-run-events-001"
	log, err := NewStore(t.TempDir()).OpenRunEventLog(sessionID, testRunID, testAttemptID, DefaultRunEventRetention)
	if err != nil {
		t.Fatal(err)
	}
	return log, sessionID
}

func TestRunEventLogAppendReadAndRetryOverlap(t *testing.T) {
	log, sessionID := openTestRunEventLog(t)
	batch := []RunEventRecord{
		testRunEvent(sessionID, 1, agent.RunTraceEventModelResponse),
		testRunEvent(sessionID, 2, agent.RunTraceEventTerminal),
	}
	if err := log.Append(batch); err != nil {
		t.Fatal(err)
	}
	if err := log.Append(batch); err != nil {
		t.Fatalf("identical retry overlap: %v", err)
	}
	if log.scanCount != 1 {
		t.Fatalf("steady-state appends rescanned the JSONL file %d times, want 1", log.scanCount)
	}
	records, err := log.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 || records[0].Event.Seq != 1 || records[1].Event.Seq != 2 {
		t.Fatalf("records = %+v", records)
	}
}

func TestRunEventLogAppendRescansAfterAnotherInstanceAppends(t *testing.T) {
	dir := t.TempDir()
	const sessionID = "session-run-events-cache-001"
	logA, err := NewStore(dir).OpenRunEventLog(sessionID, testRunID, testAttemptID, DefaultRunEventRetention)
	if err != nil {
		t.Fatal(err)
	}
	logB, err := NewStore(dir).OpenRunEventLog(sessionID, testRunID, testAttemptID, DefaultRunEventRetention)
	if err != nil {
		t.Fatal(err)
	}
	if err := logA.Append([]RunEventRecord{testRunEvent(sessionID, 1, agent.RunTraceEventModelResponse)}); err != nil {
		t.Fatal(err)
	}
	if err := logB.Append([]RunEventRecord{testRunEvent(sessionID, 2, agent.RunTraceEventToolOutcome)}); err != nil {
		t.Fatal(err)
	}
	if err := logA.Append([]RunEventRecord{testRunEvent(sessionID, 3, agent.RunTraceEventTerminal)}); err != nil {
		t.Fatal(err)
	}
	if logA.scanCount != 2 {
		t.Fatalf("logA scan count = %d, want initial scan plus external-size refresh", logA.scanCount)
	}
}

func TestRunEventLogRejectsConflictAndGap(t *testing.T) {
	log, sessionID := openTestRunEventLog(t)
	first := testRunEvent(sessionID, 1, agent.RunTraceEventModelResponse)
	if err := log.Append([]RunEventRecord{first}); err != nil {
		t.Fatal(err)
	}
	conflict := first
	conflict.Event.Type = agent.RunTraceEventRetry
	if err := log.Append([]RunEventRecord{conflict}); err == nil || !strings.Contains(err.Error(), "conflicting duplicate") {
		t.Fatalf("conflict error = %v", err)
	}
	gap := testRunEvent(sessionID, 3, agent.RunTraceEventTerminal)
	if err := log.Append([]RunEventRecord{gap}); err == nil || !strings.Contains(err.Error(), "sequence gap") {
		t.Fatalf("gap error = %v", err)
	}
}

func TestRunEventLogAppendFailureMarksIncompleteAndSuccessfulAppendClears(t *testing.T) {
	log, sessionID := openTestRunEventLog(t)
	first := testRunEvent(sessionID, 1, agent.RunTraceEventModelResponse)
	if err := log.Append([]RunEventRecord{first}); err != nil {
		t.Fatal(err)
	}
	// A pre-existing marker may have been created by an older build or with a
	// permissive umask. Rewriting it must restore the 0600 contract.
	if err := os.WriteFile(log.incompletePath, []byte("legacy marker"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(log.incompletePath, 0644); err != nil {
		t.Fatal(err)
	}

	gap := testRunEvent(sessionID, 3, agent.RunTraceEventToolOutcome)
	gap.Event.Tool = &agent.RunTraceToolOutcome{Name: "private-tool-name"}
	if err := log.Append([]RunEventRecord{gap}); err == nil {
		t.Fatal("gap append succeeded")
	}
	marker, present, err := log.ReadIncompleteMarker()
	if err != nil || !present {
		t.Fatalf("marker=%+v present=%t err=%v", marker, present, err)
	}
	if marker.SchemaVersion != RunEventIncompleteSchemaVersion || marker.MarkedAt.IsZero() ||
		marker.Category != RunEventIncompleteCategoryValidation {
		t.Fatalf("marker = %+v", marker)
	}
	raw, err := os.ReadFile(log.incompletePath)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	if len(fields) != 3 || fields["schema_version"] == nil || fields["marked_at"] == nil || fields["category"] == nil {
		t.Fatalf("unexpected marker fields: %s", raw)
	}
	if strings.Contains(string(raw), "private-tool-name") || strings.Contains(string(raw), "sequence gap") {
		t.Fatalf("marker leaked event or error content: %s", raw)
	}
	info, err := os.Stat(log.incompletePath)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0600 {
		t.Fatalf("marker permissions = %o, want 600", got)
	}

	second := testRunEvent(sessionID, 2, agent.RunTraceEventTerminal)
	if err := log.Append([]RunEventRecord{second}); err != nil {
		t.Fatal(err)
	}
	marker, present, err = log.ReadIncompleteMarker()
	if err != nil || present {
		t.Fatalf("marker after recovery=%+v present=%t err=%v", marker, present, err)
	}
}

func TestRunEventLogReadIncompleteMarkerFailsClosedOnCorruption(t *testing.T) {
	log, _ := openTestRunEventLog(t)
	if err := os.WriteFile(log.incompletePath, []byte("not-json\n"), 0600); err != nil {
		t.Fatal(err)
	}
	_, present, err := log.ReadIncompleteMarker()
	if err == nil || !present || !strings.Contains(err.Error(), "decode run event incomplete marker") {
		t.Fatalf("present=%t err=%v", present, err)
	}
}

func TestRunEventLogRepairsTruncatedTail(t *testing.T) {
	log, sessionID := openTestRunEventLog(t)
	first := testRunEvent(sessionID, 1, agent.RunTraceEventModelResponse)
	if err := log.Append([]RunEventRecord{first}); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(log.path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(`{"schema_version":1,"event":`); err != nil {
		t.Fatal(err)
	}
	file.Close()
	second := testRunEvent(sessionID, 2, agent.RunTraceEventTerminal)
	if err := log.Append([]RunEventRecord{second}); err != nil {
		t.Fatalf("append after truncated tail: %v", err)
	}
	records, err := log.ReadAll()
	if err != nil || len(records) != 2 || records[1].Event.Seq != 2 {
		t.Fatalf("records=%+v err=%v", records, err)
	}
}

func TestRunEventLogRejectsCorruptCompleteLine(t *testing.T) {
	log, _ := openTestRunEventLog(t)
	if err := os.WriteFile(log.path, []byte("not-json\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := log.ReadAll(); err == nil || !strings.Contains(err.Error(), "decode run event line") {
		t.Fatalf("corrupt-line error = %v", err)
	}
}

func TestRunEventLogConcurrentIdenticalAppendIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	storeA := NewStore(dir)
	storeB := NewStore(dir)
	const sessionID = "session-run-events-002"
	logA, err := storeA.OpenRunEventLog(sessionID, testRunID, testAttemptID, DefaultRunEventRetention)
	if err != nil {
		t.Fatal(err)
	}
	logB, err := storeB.OpenRunEventLog(sessionID, testRunID, testAttemptID, DefaultRunEventRetention)
	if err != nil {
		t.Fatal(err)
	}
	record := testRunEvent(sessionID, 1, agent.RunTraceEventTerminal)
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, log := range []*RunEventLog{logA, logB} {
		wg.Add(1)
		go func(log *RunEventLog) {
			defer wg.Done()
			errs <- log.Append([]RunEventRecord{record})
		}(log)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	records, err := logA.ReadAll()
	if err != nil || len(records) != 1 {
		t.Fatalf("records=%+v err=%v", records, err)
	}
}

func TestStoreDeleteRemovesRunEventDirectory(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	const sessionID = "session-run-events-delete-001"
	if err := store.Save(&Session{ID: sessionID, CreatedAt: time.Now(), UpdatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	log, err := store.OpenRunEventLog(sessionID, testRunID, testAttemptID, DefaultRunEventRetention)
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Append([]RunEventRecord{testRunEvent(sessionID, 1, agent.RunTraceEventTerminal)}); err != nil {
		t.Fatal(err)
	}
	eventDir := filepath.Join(dir, runEventDirName, sessionID)
	if err := store.Delete(sessionID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(eventDir); !os.IsNotExist(err) {
		t.Fatalf("run event directory remains: %v", err)
	}
}

func TestOpenRunEventLogPrunesOldestAttemptsAcrossRuns(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	const sessionID = "session-run-events-retention-001"
	for i := 1; i <= 3; i++ {
		runID := fmt.Sprintf("run1_%032x", i)
		attemptID := fmt.Sprintf("att1_%032x", i)
		if _, err := store.OpenRunEventLog(sessionID, runID, attemptID, 2); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Millisecond)
	}
	attempts, err := collectRunEventAttempts(filepath.Join(dir, runEventDirName, sessionID))
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 2 {
		t.Fatalf("retained attempts = %d, want 2: %+v", len(attempts), attempts)
	}
	if attempts[0].attemptID == fmt.Sprintf("att1_%032x", 1) || attempts[1].attemptID == fmt.Sprintf("att1_%032x", 1) {
		t.Fatal("oldest attempt was not pruned")
	}
}

func TestSweepStaleRunEventsRemovesAttemptFilesOnly(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	const sessionID = "session-run-events-age-001"
	log, err := store.OpenRunEventLog(sessionID, testRunID, testAttemptID, DefaultRunEventRetention)
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Append([]RunEventRecord{testRunEvent(sessionID, 1, agent.RunTraceEventTerminal)}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(log.incompletePath, []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	for _, path := range []string{log.path, log.incompletePath, log.lockPath} {
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}
	removed, err := SweepStaleRunEvents(dir, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 3 {
		t.Fatalf("removed = %d, want 3", removed)
	}
	for _, path := range []string{log.path, log.incompletePath, log.lockPath} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("stale file remains %s: %v", path, err)
		}
	}
}

func TestNewStoreSweepsOrphanRunEventDirectories(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, runEventDirName)
	orphan := filepath.Join(root, "orphan-session", testRunID)
	live := filepath.Join(root, "live-session", testRunID)
	for _, path := range []string{orphan, live} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(live, testAttemptID+".lock"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "live-session.json"), []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	_ = NewStore(dir)
	if _, err := os.Stat(filepath.Join(root, "orphan-session")); !os.IsNotExist(err) {
		t.Fatalf("orphan run-event directory remains: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "live-session")); err != nil {
		t.Fatalf("live run-event directory removed: %v", err)
	}
}
