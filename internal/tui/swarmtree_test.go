package tui

import (
	"strings"
	"testing"
	"time"
)

// containsStr is a helper for substring checks in TUI rendering tests.
func containsStr(s, sub string) bool {
	return strings.Contains(s, sub)
}

func TestRenderSummaryLine_SingleType(t *testing.T) {
	agents := []swarmAgentEntry{
		{agentType: "scout", status: "running"},
		{agentType: "scout", status: "running"},
		{agentType: "scout", status: "running"},
	}
	got := renderSummaryLine(agents, 80)
	if !containsStr(got, "Running 3 scout agents") {
		t.Errorf("expected 'Running 3 scout agents', got %q", got)
	}
	if !containsStr(got, "Tab for details") {
		t.Errorf("expected Tab hint, got %q", got)
	}
}

func TestRenderSummaryLine_MixedTypes(t *testing.T) {
	agents := []swarmAgentEntry{
		{agentType: "scout", status: "running"},
		{agentType: "coder", status: "running"},
	}
	got := renderSummaryLine(agents, 80)
	if !containsStr(got, "Running 2 agents") {
		t.Errorf("expected 'Running 2 agents', got %q", got)
	}
}

func TestRenderSummaryLine_SingleAgent(t *testing.T) {
	agents := []swarmAgentEntry{
		{agentType: "scout", status: "running"},
	}
	got := renderSummaryLine(agents, 80)
	if !containsStr(got, "Running scout agent") {
		t.Errorf("expected 'Running scout agent', got %q", got)
	}
}

func TestRenderSummaryLine_AllCompleted(t *testing.T) {
	agents := []swarmAgentEntry{
		{agentType: "scout", status: "completed", elapsed: 92 * time.Second},
		{agentType: "scout", status: "completed", elapsed: 90 * time.Second},
	}
	got := renderSummaryLine(agents, 80)
	if !containsStr(got, "2 agents completed") {
		t.Errorf("expected completion summary, got %q", got)
	}
}

func TestRenderSummaryLine_SingleCompleted(t *testing.T) {
	agents := []swarmAgentEntry{
		{agentType: "scout", status: "completed", elapsed: 15 * time.Second},
	}
	got := renderSummaryLine(agents, 80)
	if containsStr(got, "1 agents") {
		t.Errorf("should use singular 'agent', got %q", got)
	}
	if !containsStr(got, "1 agent completed") {
		t.Errorf("expected '1 agent completed', got %q", got)
	}
}

func TestRenderSwarmTree_WithStats(t *testing.T) {
	agents := []swarmAgentEntry{
		{
			id: "1", agentType: "scout", status: "running",
			toolUseCount: 7, tokenCount: 4200,
			recentTools: []recentToolEntry{
				{name: "grep", keyArg: "TODO", isSearch: true},
				{name: "grep", keyArg: "FIXME", isSearch: true},
				{name: "file_read", keyArg: "main.go", isRead: true},
			},
			started: time.Now(),
		},
	}
	got := renderSwarmTree(agents, 100)
	if !containsStr(got, "7 tools") {
		t.Errorf("expected tool count, got %q", got)
	}
	if !containsStr(got, "4.2k") {
		t.Errorf("expected token count, got %q", got)
	}
	if !containsStr(got, "Searching") {
		t.Errorf("expected summarized activity, got %q", got)
	}
}

func TestRenderSwarmTree_NarrowWidth(t *testing.T) {
	agents := []swarmAgentEntry{
		{
			id: "1", agentType: "scout", status: "running",
			toolUseCount: 7, tokenCount: 4200,
			recentTools:  []recentToolEntry{{name: "bash", keyArg: "ls"}},
			started:      time.Now(),
		},
	}
	got := renderSwarmTree(agents, 50)
	// Narrow: should NOT contain stats
	if containsStr(got, "tools") {
		t.Errorf("expected stats hidden at narrow width, got %q", got)
	}
	if !containsStr(got, "@scout") {
		t.Errorf("expected agent name, got %q", got)
	}
}

func TestRenderCompactTree_ShowsAllAgents(t *testing.T) {
	agents := []swarmAgentEntry{
		{
			id: "1", agentType: "scout", status: "running",
			description: "search patterns",
			recentTools: []recentToolEntry{{name: "grep", keyArg: "TODO", isSearch: true}},
			started:     time.Now(),
		},
		{
			id: "2", agentType: "scout", status: "completed",
			description: "read configs",
			elapsed:     30 * time.Second,
		},
	}
	got := renderCompactTree(agents, 80)
	if !containsStr(got, "@scout") {
		t.Errorf("expected agent name, got %q", got)
	}
	if !containsStr(got, "Done") {
		t.Errorf("expected completion marker, got %q", got)
	}
}

func TestRenderCompactTree_RunningShowsActivity(t *testing.T) {
	agents := []swarmAgentEntry{
		{
			id: "1", agentType: "coder", status: "running",
			description: "implement feature",
			recentTools: []recentToolEntry{
				{name: "file_read", keyArg: "main.go", isRead: true},
			},
			started: time.Now(),
		},
	}
	got := renderCompactTree(agents, 80)
	if !containsStr(got, "main.go") {
		t.Errorf("expected file reference in activity, got %q", got)
	}
}

func TestFormatTokenCount(t *testing.T) {
	tests := []struct {
		input int
		want  string
	}{
		{500, "500"},
		{1000, "1.0k"},
		{4200, "4.2k"},
		{12345, "12k"},
		{0, "0"},
	}
	for _, tt := range tests {
		got := formatTokenCount(tt.input)
		if got != tt.want {
			t.Errorf("formatTokenCount(%d) = %q, want %q", tt.input, got, tt.want)
		}
	}
}
