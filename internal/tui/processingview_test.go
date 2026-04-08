package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/textarea"

	"github.com/Kocoro-lab/ShanClaw/internal/config"
	"github.com/Kocoro-lab/ShanClaw/internal/session"
)

func newProcessingTestModel(t *testing.T) *Model {
	t.Helper()
	sessions := session.NewManager(t.TempDir())
	sessions.NewSession()
	m := &Model{
		cfg: &config.Config{
			ModelTier: "opus",
			Endpoint:  "https://api.test.com",
		},
		sessions:            sessions,
		textarea:            textarea.New(),
		width:               100,
		version:             "dev",
		headerCWD:           "/tmp/project",
		markdownCache:       map[string]string{},
		state:               stateProcessing,
		processingStartTime: time.Now(),
		spinnerTexts:        []string{"Thinking…"},
		headerDone:          true,
	}
	return m
}

func TestProcessingView_NoSubagents_ShowsSpinner(t *testing.T) {
	m := newProcessingTestModel(t)
	view := m.View()
	// Should contain spinner text (one of the default texts)
	if !strings.Contains(view, "opus") {
		t.Errorf("expected model tier in view, got:\n%s", view)
	}
}

func TestProcessingView_NoSubagents_ShowsToolProgress(t *testing.T) {
	m := newProcessingTestModel(t)
	m.liveTools = []liveToolEntry{
		{name: "grep", keyArg: "TODO", started: time.Now()},
		{name: "file_read", keyArg: "main.go", started: time.Now()},
	}
	view := m.View()
	if !strings.Contains(view, "grep") {
		t.Errorf("expected live tool in view, got:\n%s", view)
	}
}

func TestProcessingView_WithSubagents_Collapsed_ShowsSummaryLine(t *testing.T) {
	m := newProcessingTestModel(t)
	m.swarmAgents = []swarmAgentEntry{
		{id: "1", agentType: "scout", status: "running", description: "Search code", started: time.Now()},
		{id: "2", agentType: "scout", status: "running", description: "Read docs", started: time.Now()},
		{id: "3", agentType: "scout", status: "running", description: "Check tests", started: time.Now()},
	}
	view := m.View()
	if !strings.Contains(view, "Running 3 scout agents") {
		t.Errorf("expected summary line in collapsed view, got:\n%s", view)
	}
	if !strings.Contains(view, "Tab for details") {
		t.Errorf("expected Tab hint in collapsed view")
	}
	// Compact tree is always visible — should show tree connectors
	if !strings.Contains(view, "├─") && !strings.Contains(view, "└─") {
		t.Errorf("expected compact tree connectors in collapsed view")
	}
}

func TestProcessingView_WithSubagents_Expanded_ShowsTree(t *testing.T) {
	m := newProcessingTestModel(t)
	m.expandedView = "expanded"
	m.swarmAgents = []swarmAgentEntry{
		{id: "1", agentType: "scout", status: "running", description: "Search code",
			toolUseCount: 5, tokenCount: 3200,
			recentTools: []recentToolEntry{
				{name: "grep", keyArg: "TODO", isSearch: true},
				{name: "grep", keyArg: "FIXME", isSearch: true},
			},
			started: time.Now()},
		{id: "2", agentType: "scout", status: "completed", elapsed: 30 * time.Second,
			toolUseCount: 10, tokenCount: 8100},
	}
	view := m.View()
	// Should show tree connectors
	if !strings.Contains(view, "├─") {
		t.Errorf("expected ├─ tree connector in expanded view")
	}
	if !strings.Contains(view, "└─") {
		t.Errorf("expected └─ tree connector in expanded view")
	}
	// Should show stats
	if !strings.Contains(view, "5 tools") {
		t.Errorf("expected tool count in expanded view")
	}
	if !strings.Contains(view, "3.2k") {
		t.Errorf("expected token count in expanded view")
	}
	// Should show activity summary
	if !strings.Contains(view, "Searching") {
		t.Errorf("expected activity summary in expanded view")
	}
	// Should show completed agent
	if !strings.Contains(view, "Done") {
		t.Errorf("expected Done for completed agent")
	}
	// Should show [expanded] hint
	if !strings.Contains(view, "[expanded]") {
		t.Errorf("expected [expanded] view hint in status bar")
	}
}

func TestProcessingView_Expanded_WithTasks(t *testing.T) {
	m := newProcessingTestModel(t)
	m.expandedView = "expanded"
	m.swarmAgents = []swarmAgentEntry{
		{id: "1", agentType: "scout", status: "running", started: time.Now()},
	}
	m.checklistTasks = []checklistTask{
		{subject: "Setup fixtures", status: "completed"},
		{subject: "Implement parser", status: "in_progress"},
		{subject: "Write tests", status: "pending"},
	}
	view := m.View()
	// Should show both tree and checklist
	if !strings.Contains(view, "@scout") {
		t.Errorf("expected agent tree in expanded view")
	}
	if !strings.Contains(view, "Setup fixtures") {
		t.Errorf("expected task checklist in expanded view")
	}
}

func TestProcessingView_Collapsed_NoSubagents_ShowsChecklist(t *testing.T) {
	m := newProcessingTestModel(t)
	m.checklistTasks = []checklistTask{
		{subject: "Do something", status: "in_progress"},
	}
	view := m.View()
	if !strings.Contains(view, "Do something") {
		t.Errorf("expected checklist when no subagents in collapsed view")
	}
}

func TestProcessingView_TokenAggregation(t *testing.T) {
	m := newProcessingTestModel(t)
	m.tokenInput = 1000
	m.tokenOutput = 500
	m.swarmAgents = []swarmAgentEntry{
		{id: "1", agentType: "scout", status: "running", tokenCount: 3000, started: time.Now()},
		{id: "2", agentType: "scout", status: "running", tokenCount: 2000, started: time.Now()},
	}
	view := m.View()
	// Total = 1000 + 500 + 3000 + 2000 = 6500 → "6.5k"
	if !strings.Contains(view, "6.5k") {
		t.Errorf("expected aggregated token count 6.5k in view, got:\n%s", view)
	}
}
