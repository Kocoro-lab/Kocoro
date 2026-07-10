package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// preflightDumpMu serializes appends so concurrent sessions in one daemon
// process cannot interleave JSONL rows.
var preflightDumpMu sync.Mutex

// preflightDumpEnabled reports whether this process opted into persisting
// injected <private_memory> blocks. Read per call (not at init) so tests can
// flip it with t.Setenv.
func preflightDumpEnabled() bool {
	return os.Getenv("SHANNON_PREFLIGHT_DUMP") == "1"
}

// dumpPreflightContext appends one JSONL row with the injected
// <private_memory> block to <shannonDir>/logs/preflight_dump.jsonl. The block
// exists only in the in-flight user message and the memory_preflight audit
// row is content-free by design, so without this opt-in dump a "did the
// answer model ever see fact X" incident can never be attributed. The file
// holds private memory content: 0600, opt-in via SHANNON_PREFLIGHT_DUMP=1
// only, delete after debugging. Best-effort — a debug affordance must never
// fail the run it is observing.
func dumpPreflightContext(shannonDir, sessionID, injected string) {
	row, err := json.Marshal(struct {
		Timestamp string `json:"timestamp"`
		SessionID string `json:"session_id"`
		Context   string `json:"context"`
	}{
		Timestamp: time.Now().Format(time.RFC3339Nano),
		SessionID: sessionID,
		Context:   injected,
	})
	if err != nil {
		return
	}
	logDir := filepath.Join(shannonDir, "logs")
	if err := os.MkdirAll(logDir, 0700); err != nil {
		return
	}
	preflightDumpMu.Lock()
	defer preflightDumpMu.Unlock()
	f, err := os.OpenFile(filepath.Join(logDir, "preflight_dump.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(row, '\n'))
}
