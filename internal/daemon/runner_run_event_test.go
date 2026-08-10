package daemon

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/session"
)

func TestRunAgentPersistsUniversalRunEventLogForDesktop(t *testing.T) {
	gw := &fakeGatewayBackend{reply: "finished without echoing private input"}
	server := httptest.NewServer(gw.handler())
	defer server.Close()
	deps := runAgentContractTestDeps(t, server.URL)
	defer deps.SessionCache.CloseAll()
	const sessionID = "desktop-run-events-001"

	result, err := RunAgent(context.Background(), deps, RunAgentRequest{
		Text: "private-user-prompt", Source: "desktop", SessionID: sessionID, NewSession: true,
	}, nullEventHandler{})
	if err != nil {
		t.Fatal(err)
	}
	if result.SessionID != sessionID {
		t.Fatalf("session id = %q", result.SessionID)
	}
	waitForTitlePersisted(t, filepath.Join(deps.ShannonDir, "sessions", sessionID+".json"))
	runRoot := filepath.Join(deps.ShannonDir, "sessions", ".run-events", sessionID)
	runEntries, err := os.ReadDir(runRoot)
	if err != nil || len(runEntries) != 1 {
		t.Fatalf("run entries=%v err=%v", runEntries, err)
	}
	runID := runEntries[0].Name()
	attemptEntries, err := os.ReadDir(filepath.Join(runRoot, runID))
	if err != nil {
		t.Fatal(err)
	}
	var attemptID string
	for _, entry := range attemptEntries {
		if strings.HasSuffix(entry.Name(), ".jsonl") {
			attemptID = strings.TrimSuffix(entry.Name(), ".jsonl")
		}
	}
	if !session.IsValidRunID(runID) || !session.IsValidAttemptID(attemptID) {
		t.Fatalf("run=%q attempt=%q entries=%v", runID, attemptID, attemptEntries)
	}
	eventLog, err := session.NewStore(filepath.Join(deps.ShannonDir, "sessions")).OpenRunEventLog(sessionID, runID, attemptID)
	if err != nil {
		t.Fatal(err)
	}
	records, err := eventLog.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) < 2 || records[len(records)-1].Event.Type != agent.RunTraceEventTerminal {
		t.Fatalf("records = %+v", records)
	}
	for index, record := range records {
		if record.Event.Seq != int64(index+1) || record.RunID != runID || record.AttemptID != attemptID {
			t.Fatalf("record[%d] = %+v", index, record)
		}
	}
	wire, err := json.Marshal(records)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"private-user-prompt", "finished without echoing private input", "tool_call_id", "args_sha256", "result_sha256"} {
		if strings.Contains(string(wire), forbidden) {
			t.Fatalf("run event log leaked %q: %s", forbidden, wire)
		}
	}
}
