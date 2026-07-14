package test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/tools"
)

// encodeToolCall writes a scripted "call this tool" LLM response.
func encodeToolCall(w http.ResponseWriter, name, args string) {
	json.NewEncoder(w).Encode(client.CompletionResponse{
		Model:        "test-model",
		FinishReason: "tool_use",
		FunctionCall: &client.FunctionCall{Name: name, Arguments: json.RawMessage(args)},
		Usage:        client.Usage{InputTokens: 1, OutputTokens: 1, TotalTokens: 2},
	})
}

func encodeDone(w http.ResponseWriter, text string) {
	json.NewEncoder(w).Encode(client.CompletionResponse{
		Model:        "test-model",
		OutputText:   text,
		FinishReason: "end_turn",
		Usage:        client.Usage{InputTokens: 1, OutputTokens: 1, TotalTokens: 2},
	})
}

func mustToolArgs(t *testing.T, args map[string]any) string {
	t.Helper()
	b, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// autoApproveHandler auto-approves every tool so the full loop actually executes
// write tools (file_edit requires approval; without a handler the loop refuses
// it). Mirrors the daemon's localhost auto-approve path.
type autoApproveHandler struct{}

func (autoApproveHandler) OnToolCall(string, string, string)                                    {}
func (autoApproveHandler) OnToolResult(string, string, string, agent.ToolResult, time.Duration) {}
func (autoApproveHandler) OnText(string)                                                        {}
func (autoApproveHandler) OnPreamble(string)                                                    {}
func (autoApproveHandler) OnStreamDelta(string)                                                 {}
func (autoApproveHandler) OnApprovalNeeded(string, string) bool                                 { return true }
func (autoApproveHandler) OnUsage(agent.TurnUsage)                                              {}
func (autoApproveHandler) OnCloudAgent(string, string, string)                                  {}
func (autoApproveHandler) OnCloudProgress(int, int)                                             {}
func (autoApproveHandler) OnCloudPlan(string, string, bool)                                     {}

// TestE2E_FuzzyEdit_SmartQuotesSucceedsInLoop drives the FULL agent loop
// (real FileReadTool + FileEditTool + ReadTracker) against a scripted gateway.
// The file holds Unicode smart quotes; the model edits with ASCII straight
// quotes — the exact case that used to fail. With the fix, the punctuation
// fuzzy match makes the edit succeed end-to-end and the file is actually
// modified on disk.
func TestE2E_FuzzyEdit_SmartQuotesSucceedsInLoop(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.md")
	original := "intro\n“value”: “hello”\noutro\n" // smart quotes on disk
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	readArgs := mustToolArgs(t, map[string]any{
		"path": path, "description": "read the file",
	})
	editArgs := mustToolArgs(t, map[string]any{
		"path": path, "old_string": `"value": "hello"`, "new_string": `"value": "world"`,
		"description": "change hello to world",
	})

	call := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call++
		switch call {
		case 1:
			encodeToolCall(w, "file_read", readArgs)
		case 2:
			// ASCII straight quotes, deliberately mismatching the smart quotes.
			encodeToolCall(w, "file_edit", editArgs)
		default:
			encodeDone(w, "done")
		}
	}))
	defer server.Close()

	gw := client.NewGatewayClient(server.URL, "")
	reg := agent.NewToolRegistry()
	reg.Register(&tools.FileReadTool{})
	reg.Register(&tools.FileEditTool{})
	loop := agent.NewAgentLoop(gw, reg, "medium", "", 25, 2000, 200, nil, nil, nil)
	loop.SetHandler(autoApproveHandler{})

	if _, _, err := loop.Run(context.Background(), "edit the file", nil, nil); err != nil {
		t.Fatalf("loop returned error: %v", err)
	}

	got, _ := os.ReadFile(path)
	want := "intro\n\"value\": \"world\"\noutro\n"
	if string(got) != want {
		t.Errorf("smart-quote file was not edited end-to-end:\n  want %q\n  got  %q", want, string(got))
	}
	if call < 3 {
		t.Errorf("expected >=3 loop turns (read, edit, done); got %d — the edit likely failed", call)
	}
}

// TestE2E_FailedEdit_BreaksDeadlock drives the full loop through the R1 deadlock
// scenario: read → edit that cannot match → re-read. With the fix, the failed
// edit invalidates the file's read dedup, so the re-read feeds REAL content back
// to the model instead of the "unchanged since last read" stub. We assert on the
// request body of the turn AFTER the re-read to see what the loop actually sent.
func TestE2E_FailedEdit_BreaksDeadlock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(path, []byte("line one\nline two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	readArgs := mustToolArgs(t, map[string]any{
		"path": path, "description": "read",
	})
	editArgs := mustToolArgs(t, map[string]any{
		"path": path, "old_string": "ABSENT_TOKEN_QZX_9", "new_string": "x", "description": "edit",
	})
	rereadArgs := mustToolArgs(t, map[string]any{
		"path": path, "description": "re-read",
	})

	call := 0
	var afterRereadBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call++
		switch call {
		case 1:
			encodeToolCall(w, "file_read", readArgs)
		case 2:
			// old_string absent even fuzzily → fails, invalidates this file's dedup.
			encodeToolCall(w, "file_edit", editArgs)
		case 3:
			// Re-read with identical args. Pre-fix this returned the stub.
			encodeToolCall(w, "file_read", rereadArgs)
		default:
			body, _ := io.ReadAll(r.Body)
			afterRereadBody = string(body)
			encodeDone(w, "done")
		}
	}))
	defer server.Close()

	gw := client.NewGatewayClient(server.URL, "")
	reg := agent.NewToolRegistry()
	reg.Register(&tools.FileReadTool{})
	reg.Register(&tools.FileEditTool{})
	loop := agent.NewAgentLoop(gw, reg, "medium", "", 25, 2000, 200, nil, nil, nil)
	loop.SetHandler(autoApproveHandler{})

	if _, _, err := loop.Run(context.Background(), "edit", nil, nil); err != nil {
		t.Fatalf("loop returned error: %v", err)
	}

	if call < 4 {
		t.Fatalf("expected >=4 loop turns (read, edit, re-read, done); got %d", call)
	}
	// A stub RESULT — and only a stub — carries this redirect phrase. The
	// file_read tool DESCRIPTION mentions "unchanged since last read", so
	// matching that phrase would false-positive on every request body (the
	// tool schema is always present). The redirect text appears only when an
	// actual stub is returned to the model.
	if strings.Contains(afterRereadBody, "refer to the earlier file_read result") {
		t.Errorf("re-read after a failed edit hit the dedup stub — deadlock NOT broken")
	}
	if !strings.Contains(afterRereadBody, "line two") {
		t.Errorf("re-read after a failed edit did not surface real file content to the model")
	}
}
