package tools

import (
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
)

func assertReadOnly(t *testing.T, tool agent.Tool, argsJSON string, expected bool) {
	t.Helper()
	checker, ok := tool.(agent.ReadOnlyChecker)
	if !ok {
		if expected {
			t.Errorf("%s: does not implement ReadOnlyChecker but expected read-only", tool.Info().Name)
		}
		return
	}
	got := checker.IsReadOnlyCall(argsJSON)
	if got != expected {
		t.Errorf("%s(%s): IsReadOnlyCall = %v, want %v", tool.Info().Name, argsJSON, got, expected)
	}
}

func TestReadOnly_AlwaysReadOnly(t *testing.T) {
	alwaysReadOnly := []agent.Tool{
		&FileReadTool{},
		&GlobTool{},
		&GrepTool{},
		&DirectoryListTool{},
		&ThinkTool{},
		&SystemInfoTool{},
		&ScreenshotTool{},
		&SessionSearchTool{},
		&WaitTool{},
	}
	for _, tool := range alwaysReadOnly {
		assertReadOnly(t, tool, `{}`, true)
	}
}

func TestReadOnly_AlwaysWrite(t *testing.T) {
	alwaysWrite := []agent.Tool{
		&BashTool{},
		&FileWriteTool{},
		&FileEditTool{},
		&HTTPTool{},
		&ComputerTool{},
		&AppleScriptTool{},
		&BrowserTool{},
		&CloudDelegateTool{},
		&MemoryAppendTool{},
		&NotifyTool{},
		&ProcessTool{},
		&GhosttyTool{tabs: newTabRegistry()},
		newUseSkillTool(nil),
	}
	for _, tool := range alwaysWrite {
		assertReadOnly(t, tool, `{}`, false)
	}
}

func TestReadOnly_Accessibility(t *testing.T) {
	tool := &AccessibilityTool{}

	readActions := []string{"read_tree", "annotate", "find", "get_value"}
	for _, action := range readActions {
		assertReadOnly(t, tool, `{"action":"`+action+`"}`, true)
	}

	writeActions := []string{"click", "press", "set_value", "scroll"}
	for _, action := range writeActions {
		assertReadOnly(t, tool, `{"action":"`+action+`"}`, false)
	}

	// Unknown action → false
	assertReadOnly(t, tool, `{"action":"unknown"}`, false)

	// Missing action field → false
	assertReadOnly(t, tool, `{}`, false)

	// Malformed JSON → false
	assertReadOnly(t, tool, `not-json`, false)
}

func TestReadOnly_Clipboard(t *testing.T) {
	tool := &ClipboardTool{}

	assertReadOnly(t, tool, `{"action":"read"}`, true)
	assertReadOnly(t, tool, `{"action":"write"}`, false)
	assertReadOnly(t, tool, `{"action":"unknown"}`, false)

	// Missing action → false
	assertReadOnly(t, tool, `{}`, false)

	// Malformed JSON → false
	assertReadOnly(t, tool, `not-json`, false)
}

func TestReadOnly_ScheduleTool(t *testing.T) {
	listTool := &ScheduleTool{action: "list"}
	assertReadOnly(t, listTool, `{}`, true)

	for _, action := range []string{"create", "update", "remove"} {
		tool := &ScheduleTool{action: action}
		assertReadOnly(t, tool, `{}`, false)
	}
}

func TestReadOnly_MCPTool_NotImplemented(t *testing.T) {
	tool := &MCPTool{}
	if _, ok := agent.Tool(tool).(agent.ReadOnlyChecker); ok {
		t.Error("MCPTool should NOT implement ReadOnlyChecker")
	}
}

func TestReadOnly_ServerTool_NotImplemented(t *testing.T) {
	tool := &ServerTool{}
	if _, ok := agent.Tool(tool).(agent.ReadOnlyChecker); ok {
		t.Error("ServerTool should NOT implement ReadOnlyChecker")
	}
}
