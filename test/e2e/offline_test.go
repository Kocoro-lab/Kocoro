package e2e

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/agents"
	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/schedule"
	"github.com/Kocoro-lab/ShanClaw/internal/session"
)

func TestOffline_OneShotPrintsFallbackPreambleBeforeTool(t *testing.T) {
	var calls atomic.Int32
	fakeGateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/completions" {
			_ = json.NewEncoder(w).Encode([]any{})
			return
		}
		switch calls.Add(1) {
		case 1:
			args := map[string]any{
				"pattern":     "fallbackPreambleFromToolCalls",
				"path":        filepath.Join(repoRoot(), "internal", "agent", "preamble.go"),
				"output_mode": "content",
				"description": "Locate the preamble helper.",
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"model":         "e2e-silent-tool-model",
				"output_text":   "",
				"finish_reason": "tool_use",
				"tool_calls": []map[string]any{{
					"id":        "toolu_e2e_preamble",
					"name":      "grep",
					"arguments": args,
				}},
				"content_blocks": []map[string]any{{
					"type":  "tool_use",
					"id":    "toolu_e2e_preamble",
					"name":  "grep",
					"input": args,
				}},
				"usage": map[string]any{"input_tokens": 20, "output_tokens": 10, "total_tokens": 30},
			})
		case 2:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"model":         "e2e-silent-tool-model",
				"output_text":   "Located.",
				"finish_reason": "end_turn",
				"usage":         map[string]any{"input_tokens": 20, "output_tokens": 5, "total_tokens": 25},
			})
		case 3:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"model":         "e2e-silent-tool-model",
				"output_text":   "Preamble E2E",
				"finish_reason": "end_turn",
				"usage":         map[string]any{"input_tokens": 10, "output_tokens": 3, "total_tokens": 13},
			})
		default:
			http.Error(w, "unexpected completion request", http.StatusInternalServerError)
		}
	}))
	defer fakeGateway.Close()

	tempHome := t.TempDir()
	shannonDir := filepath.Join(tempHome, ".shannon")
	if err := os.MkdirAll(shannonDir, 0o700); err != nil {
		t.Fatalf("mkdir shannon dir: %v", err)
	}
	configYAML := "provider: gateway\n" +
		"endpoint: " + fakeGateway.URL + "\n" +
		"auto_update_check: false\n" +
		"agent:\n" +
		"  skill_discovery: false\n" +
		"mcp_servers:\n" +
		"  playwright:\n" +
		"    disabled: true\n"
	if err := os.WriteFile(filepath.Join(shannonDir, "config.yaml"), []byte(configYAML), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cmd := exec.Command(testBinary(t), "-y", "locate the preamble helper")
	cmd.Dir = repoRoot()
	cmd.Env = append(os.Environ(), "HOME="+tempHome)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("shan one-shot: %v\n%s", err, output)
	}

	text := string(output)
	preambleAt := strings.Index(text, "Locate the preamble helper.")
	toolAt := strings.Index(text, "⏵ grep")
	finalAt := strings.Index(text, "Located.")
	if preambleAt < 0 || toolAt < 0 || finalAt < 0 {
		t.Fatalf("missing preamble, tool, or final output:\n%s", text)
	}
	if !(preambleAt < toolAt && toolAt < finalAt) {
		t.Fatalf("unexpected output order (preamble=%d tool=%d final=%d):\n%s", preambleAt, toolAt, finalAt, text)
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("completion calls = %d, want 3 (tool turn, final turn, and one-shot title)", got)
	}
}

// ---------- Agent loading & builtin ----------

func TestOffline_BuiltinAgentsPresent(t *testing.T) {
	dir := t.TempDir()
	if err := agents.EnsureBuiltins(dir, "test"); err != nil {
		t.Fatalf("EnsureBuiltins: %v", err)
	}

	entries, err := agents.ListAgents(dir)
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}

	found := map[string]agents.AgentEntry{}
	for _, e := range entries {
		found[e.Name] = e
	}

	for _, name := range []string{"explorer", "reviewer"} {
		e, ok := found[name]
		if !ok {
			t.Errorf("expected builtin agent %q not found", name)
			continue
		}
		if !e.Builtin {
			t.Errorf("agent %q should be builtin", name)
		}
		if e.Override {
			t.Errorf("agent %q should not be an override", name)
		}
	}
}

func TestOffline_UserOverrideTakesPriority(t *testing.T) {
	dir := t.TempDir()
	if err := agents.EnsureBuiltins(dir, "test"); err != nil {
		t.Fatalf("EnsureBuiltins: %v", err)
	}

	overrideDir := filepath.Join(dir, "explorer")
	if err := os.MkdirAll(overrideDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(overrideDir, "AGENT.md"), []byte("Custom explorer"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	entries, err := agents.ListAgents(dir)
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}

	for _, e := range entries {
		if e.Name == "explorer" {
			if !e.Override {
				t.Error("explorer should be marked as override")
			}
			return
		}
	}
	t.Error("explorer not found in agent list")
}

func TestOffline_BuiltinResurfacesAfterOverrideRemoval(t *testing.T) {
	dir := t.TempDir()
	if err := agents.EnsureBuiltins(dir, "test"); err != nil {
		t.Fatalf("EnsureBuiltins: %v", err)
	}

	overrideDir := filepath.Join(dir, "explorer")
	os.MkdirAll(overrideDir, 0o755)
	os.WriteFile(filepath.Join(overrideDir, "AGENT.md"), []byte("Custom"), 0o644)
	os.RemoveAll(overrideDir)

	entries, err := agents.ListAgents(dir)
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}

	for _, e := range entries {
		if e.Name == "explorer" {
			if !e.Builtin {
				t.Error("explorer should be builtin after override removal")
			}
			if e.Override {
				t.Error("explorer should not be override after removal")
			}
			return
		}
	}
	t.Error("explorer not found")
}

func TestOffline_ExplorerHasReadOnlyToolFilter(t *testing.T) {
	dir := t.TempDir()
	if err := agents.EnsureBuiltins(dir, "test"); err != nil {
		t.Fatalf("EnsureBuiltins: %v", err)
	}

	ag, err := agents.LoadAgent(dir, "explorer")
	if err != nil {
		t.Fatalf("LoadAgent explorer: %v", err)
	}
	if ag.Config == nil || len(ag.Config.Tools.Allow) == 0 {
		t.Fatal("explorer should have a tool allow list")
	}

	for _, tool := range ag.Config.Tools.Allow {
		if tool == "file_write" || tool == "file_edit" {
			t.Errorf("explorer allow list should not contain %q", tool)
		}
	}
}

// ---------- Schedule CRUD ----------

func TestOffline_ScheduleCRUD(t *testing.T) {
	dir := t.TempDir()
	mgr := schedule.NewManager(filepath.Join(dir, "schedules.json"))

	// Create
	id, err := mgr.Create("", "0 0 28 2 *", "yearly check", false)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// List
	items, err := mgr.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	found := false
	for _, item := range items {
		if item.ID == id {
			found = true
			if item.Cron != "0 0 28 2 *" {
				t.Errorf("cron mismatch: %q", item.Cron)
			}
			if item.Prompt != "yearly check" {
				t.Errorf("prompt mismatch: %q", item.Prompt)
			}
		}
	}
	if !found {
		t.Fatal("created schedule not found in list")
	}

	// Update
	newCron := "0 9 * * 1-5"
	newPrompt := "weekday check"
	if err := mgr.Update(id, &schedule.UpdateOpts{Cron: &newCron, Prompt: &newPrompt}); err != nil {
		t.Fatalf("update: %v", err)
	}
	items, _ = mgr.List()
	for _, item := range items {
		if item.ID == id {
			if item.Cron != "0 9 * * 1-5" {
				t.Errorf("updated cron mismatch: %q", item.Cron)
			}
			if item.Prompt != "weekday check" {
				t.Errorf("updated prompt mismatch: %q", item.Prompt)
			}
		}
	}

	// Remove
	if err := mgr.Remove(id); err != nil {
		t.Fatalf("remove: %v", err)
	}
	items, _ = mgr.List()
	for _, item := range items {
		if item.ID == id {
			t.Error("schedule should be removed")
		}
	}
}

// ---------- Session CRUD ----------

func TestOffline_SessionCreateResumeSearch(t *testing.T) {
	dir := t.TempDir()
	mgr := session.NewManager(dir)

	sess := mgr.NewSession()
	sess.Messages = append(sess.Messages,
		client.Message{Role: "user", Content: client.NewTextContent("remember pineapple")},
		client.Message{Role: "assistant", Content: client.NewTextContent("I will remember pineapple")},
	)
	if err := mgr.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}

	mgr2 := session.NewManager(dir)
	resumed, err := mgr2.Resume(sess.ID)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if len(resumed.Messages) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(resumed.Messages))
	}

	results, err := mgr2.Search("pineapple", 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(results) == 0 {
		t.Error("expected search results for 'pineapple'")
	}
}

func TestOffline_SessionTruncate(t *testing.T) {
	dir := t.TempDir()
	mgr := session.NewManager(dir)

	sess := mgr.NewSession()
	sess.Messages = append(sess.Messages,
		client.Message{Role: "user", Content: client.NewTextContent("msg1")},
		client.Message{Role: "assistant", Content: client.NewTextContent("reply1")},
		client.Message{Role: "user", Content: client.NewTextContent("msg2")},
		client.Message{Role: "assistant", Content: client.NewTextContent("reply2")},
	)
	mgr.Save()

	if err := mgr.TruncateMessages(sess.ID, 2); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	loaded, err := mgr.Load(sess.ID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(loaded.Messages) != 2 {
		t.Errorf("expected 2 messages after truncate, got %d", len(loaded.Messages))
	}
}

// ---------- MCP Server ----------

func TestOffline_MCPServe_ToolsList(t *testing.T) {
	bin := testBinary(t)

	input := `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`
	cmd := exec.Command(bin, "mcp", "serve")
	cmd.Stdin = strings.NewReader(input)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout

	if err := cmd.Run(); err != nil {
		t.Fatalf("mcp serve failed: %v", err)
	}

	var resp struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON response: %v\nraw: %s", err, stdout.String())
	}
	if len(resp.Result.Tools) == 0 {
		t.Error("expected at least one tool from MCP serve")
	}

	toolNames := map[string]bool{}
	for _, tool := range resp.Result.Tools {
		toolNames[tool.Name] = true
	}
	for _, name := range []string{"file_read", "bash", "glob", "grep"} {
		if !toolNames[name] {
			t.Errorf("expected tool %q in MCP tools list", name)
		}
	}
}
