package e2e

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/agents"
	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/schedule"
	"github.com/Kocoro-lab/ShanClaw/internal/session"
)

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

	for _, name := range []string{"scout", "checker"} {
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

	overrideDir := filepath.Join(dir, "scout")
	if err := os.MkdirAll(overrideDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(overrideDir, "AGENT.md"), []byte("Custom scout"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	entries, err := agents.ListAgents(dir)
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}

	for _, e := range entries {
		if e.Name == "scout" {
			if !e.Override {
				t.Error("scout should be marked as override")
			}
			return
		}
	}
	t.Error("scout not found in agent list")
}

func TestOffline_BuiltinResurfacesAfterOverrideRemoval(t *testing.T) {
	dir := t.TempDir()
	if err := agents.EnsureBuiltins(dir, "test"); err != nil {
		t.Fatalf("EnsureBuiltins: %v", err)
	}

	overrideDir := filepath.Join(dir, "scout")
	os.MkdirAll(overrideDir, 0o755)
	os.WriteFile(filepath.Join(overrideDir, "AGENT.md"), []byte("Custom"), 0o644)
	os.RemoveAll(overrideDir)

	entries, err := agents.ListAgents(dir)
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}

	for _, e := range entries {
		if e.Name == "scout" {
			if !e.Builtin {
				t.Error("scout should be builtin after override removal")
			}
			if e.Override {
				t.Error("scout should not be override after removal")
			}
			return
		}
	}
	t.Error("scout not found")
}

func TestOffline_ScoutHasReadOnlyToolFilter(t *testing.T) {
	dir := t.TempDir()
	if err := agents.EnsureBuiltins(dir, "test"); err != nil {
		t.Fatalf("EnsureBuiltins: %v", err)
	}

	ag, err := agents.LoadAgent(dir, "scout")
	if err != nil {
		t.Fatalf("LoadAgent scout: %v", err)
	}
	if ag.Config == nil || len(ag.Config.Tools.Allow) == 0 {
		t.Fatal("scout should have a tool allow list")
	}

	for _, tool := range ag.Config.Tools.Allow {
		if tool == "file_write" || tool == "file_edit" {
			t.Errorf("scout allow list should not contain %q", tool)
		}
	}
}

// ---------- Schedule CRUD ----------

func TestOffline_ScheduleCRUD(t *testing.T) {
	dir := t.TempDir()
	mgr := schedule.NewManager(filepath.Join(dir, "schedules.json"))

	// Create
	id, err := mgr.Create("", "0 0 31 2 *", "never runs")
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
			if item.Cron != "0 0 31 2 *" {
				t.Errorf("cron mismatch: %q", item.Cron)
			}
			if item.Prompt != "never runs" {
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


