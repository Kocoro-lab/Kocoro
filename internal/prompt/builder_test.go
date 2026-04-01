package prompt

import (
	"strings"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/skills"
)

func TestBuildSystemPrompt_SystemIsStatic(t *testing.T) {
	// Two calls with different volatile content must produce identical System fields
	opts1 := PromptOptions{
		BasePrompt: "You are Shannon.",
		ToolNames:  []string{"bash", "file_read"},
		Memory:     "User prefers Go.",
		CWD:        "/home/user/project",
	}
	opts2 := PromptOptions{
		BasePrompt: "You are Shannon.",
		ToolNames:  []string{"bash", "file_read"},
		Memory:     "User prefers Rust now.",
		CWD:        "/tmp/other",
	}

	parts1 := BuildSystemPrompt(opts1)
	parts2 := BuildSystemPrompt(opts2)

	if parts1.System != parts2.System {
		t.Errorf("System field changed between calls with different volatile content.\nFirst:\n%s\nSecond:\n%s", parts1.System, parts2.System)
	}
}

func TestBuildSystemPrompt_VolatileContainsMemory(t *testing.T) {
	parts := BuildSystemPrompt(PromptOptions{
		BasePrompt: "Base.",
		Memory:     "User prefers Go.",
	})

	if strings.Contains(parts.System, "User prefers Go.") {
		t.Error("System should not contain memory content")
	}
	if !strings.Contains(parts.VolatileContext, "User prefers Go.") {
		t.Error("VolatileContext should contain memory content")
	}
}

func TestBuildSystemPrompt_VolatileContainsInstructions(t *testing.T) {
	parts := BuildSystemPrompt(PromptOptions{
		BasePrompt:   "Base.",
		Instructions: "Always use gofmt.",
	})

	if strings.Contains(parts.System, "Always use gofmt.") {
		t.Error("System should not contain instructions")
	}
	if !strings.Contains(parts.VolatileContext, "Always use gofmt.") {
		t.Error("VolatileContext should contain instructions")
	}
}

func TestBuildSystemPrompt_VolatileContainsCWD(t *testing.T) {
	parts := BuildSystemPrompt(PromptOptions{
		BasePrompt: "Base.",
		CWD:        "/tmp/test",
	})

	if strings.Contains(parts.System, "/tmp/test") {
		t.Error("System should not contain CWD")
	}
	if !strings.Contains(parts.VolatileContext, "/tmp/test") {
		t.Error("VolatileContext should contain CWD")
	}
}

func TestBuildSystemPrompt_VolatileContainsDateTime(t *testing.T) {
	parts := BuildSystemPrompt(PromptOptions{
		BasePrompt: "Base.",
	})

	if strings.Contains(parts.System, "Current date:") {
		t.Error("System should not contain date/time")
	}
	if !strings.Contains(parts.VolatileContext, "Current date:") {
		t.Error("VolatileContext should contain date/time")
	}
}

func TestBuildSystemPrompt_VolatileContainsMCPContext(t *testing.T) {
	parts := BuildSystemPrompt(PromptOptions{
		BasePrompt: "Base.",
		MCPContext: "Playwright: connected to Chrome on port 9222",
	})

	if strings.Contains(parts.System, "Playwright") {
		t.Error("System should not contain MCP context")
	}
	if !strings.Contains(parts.VolatileContext, "Playwright") {
		t.Error("VolatileContext should contain MCP context")
	}
}

func TestBuildSystemPrompt_StableContextContainsStickyFacts(t *testing.T) {
	parts := BuildSystemPrompt(PromptOptions{
		BasePrompt:    "Base.",
		StickyContext: "Customer: Alice. Order #8891.",
	})

	if strings.Contains(parts.System, "Alice") {
		t.Error("System should not contain sticky context")
	}
	if strings.Contains(parts.VolatileContext, "Alice") {
		t.Error("VolatileContext should not contain sticky context")
	}
	if !strings.Contains(parts.StableContext, "Customer: Alice. Order #8891.") {
		t.Error("StableContext should contain sticky facts")
	}
}

func TestBuildSystemPrompt_EmptyStickyContext(t *testing.T) {
	parts := BuildSystemPrompt(PromptOptions{
		BasePrompt: "Base.",
	})

	if parts.StableContext != "" {
		t.Errorf("StableContext should be empty, got: %q", parts.StableContext)
	}
}

func TestBuildSystemPrompt_SystemContainsToolNames(t *testing.T) {
	parts := BuildSystemPrompt(PromptOptions{
		BasePrompt: "Base.",
		ToolNames:  []string{"file_read", "bash"},
	})

	if !strings.Contains(parts.System, "file_read") {
		t.Error("System should contain tool names")
	}
}

func TestBuildSystemPrompt_SystemContainsServerToolNames(t *testing.T) {
	parts := BuildSystemPrompt(PromptOptions{
		BasePrompt:  "Base.",
		ServerTools: []string{"web_search"},
	})

	if !strings.Contains(parts.System, "web_search") {
		t.Error("System should contain server tool names")
	}
}

func TestBuildSystemPrompt_SystemContainsSkills(t *testing.T) {
	parts := BuildSystemPrompt(PromptOptions{
		BasePrompt: "Base.",
		Skills: []*skills.Skill{
			{Name: "pdf", Description: "Extract text from PDFs"},
		},
	})

	if !strings.Contains(parts.System, "## Available Skills") {
		t.Error("System should contain skills section")
	}
	if !strings.Contains(parts.System, "| pdf") {
		t.Error("System should contain skill entry")
	}
}

func TestBuildSystemPrompt_SystemContainsMemoryPersistenceGuidance(t *testing.T) {
	parts := BuildSystemPrompt(PromptOptions{
		BasePrompt: "Base.",
		MemoryDir:  "/home/user/.shannon/agents/test/",
	})

	if !strings.Contains(parts.System, "## Memory Persistence") {
		t.Error("System should contain memory persistence guidance")
	}
}

func TestBuildSystemPrompt_MinimalOptions(t *testing.T) {
	parts := BuildSystemPrompt(PromptOptions{
		BasePrompt: "Base only.",
	})

	if !strings.HasPrefix(parts.System, "Base only.") {
		t.Errorf("System should start with base prompt")
	}
	if strings.Contains(parts.System, "## Memory") {
		t.Error("System should not have Memory section")
	}
}

func TestBuildSystemPrompt_MemoryTruncation(t *testing.T) {
	bigMemory := strings.Repeat("m", maxMemoryChars+500)
	parts := BuildSystemPrompt(PromptOptions{
		BasePrompt: "Base.",
		Memory:     bigMemory,
	})

	if !strings.Contains(parts.VolatileContext, "[truncated]") {
		t.Error("expected truncation marker in volatile context memory")
	}
}

func TestBuildSystemPrompt_InstructionsTruncation(t *testing.T) {
	bigInstructions := strings.Repeat("i", maxInstructionsChars+1000)
	parts := BuildSystemPrompt(PromptOptions{
		BasePrompt:   "Base.",
		Instructions: bigInstructions,
	})

	if !strings.Contains(parts.VolatileContext, "[truncated]") {
		t.Error("expected truncation marker in volatile context instructions")
	}
}

func TestBuildSystemPrompt_DeferredToolsInStaticSystem(t *testing.T) {
	parts := BuildSystemPrompt(PromptOptions{
		BasePrompt: "Base.",
		ToolNames:  []string{"bash", "file_read", "tool_search"},
		DeferredTools: []DeferredToolSummary{
			{Name: "playwright_click", Description: "Click an element"},
			{Name: "playwright_type", Description: "Type text"},
		},
	})

	if !strings.Contains(parts.System, "## Deferred Tools") {
		t.Error("System should contain Deferred Tools section")
	}
	if !strings.Contains(parts.System, "playwright_click: Click an element") {
		t.Error("System should list deferred tool summaries")
	}
	if !strings.Contains(parts.System, "tool_search") {
		t.Error("System should mention tool_search in available tools")
	}
}

func TestBuildSystemPrompt_NoDeferredSection_WhenEmpty(t *testing.T) {
	parts := BuildSystemPrompt(PromptOptions{
		BasePrompt: "Base.",
		ToolNames:  []string{"bash", "file_read"},
	})

	if strings.Contains(parts.System, "Deferred Tools") {
		t.Error("System should not contain Deferred Tools section when empty")
	}
}

func TestTruncate(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		max      int
		expected string
	}{
		{"under limit", "hello", 10, "hello"},
		{"at limit", "hello", 5, "hello"},
		{"over limit", "hello world", 5, "hello\n[truncated]"},
		{"empty", "", 10, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := truncate(tt.input, tt.max)
			if got != tt.expected {
				t.Errorf("truncate(%q, %d) = %q, want %q", tt.input, tt.max, got, tt.expected)
			}
		})
	}
}
