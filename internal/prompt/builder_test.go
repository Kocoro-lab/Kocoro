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

func TestBuildSystemPrompt_StableContextContainsInstructions(t *testing.T) {
	parts := BuildSystemPrompt(PromptOptions{
		BasePrompt:   "Base.",
		Instructions: "Always use gofmt.",
	})

	if strings.Contains(parts.System, "Always use gofmt.") {
		t.Error("System should not contain instructions")
	}
	if strings.Contains(parts.VolatileContext, "Always use gofmt.") {
		t.Error("VolatileContext should not contain instructions (must live in StableContext so it joins the cacheable prefix)")
	}
	if !strings.Contains(parts.StableContext, "## Instructions") {
		t.Error("StableContext should contain the Instructions section header")
	}
	if !strings.Contains(parts.StableContext, "Always use gofmt.") {
		t.Error("StableContext should contain instructions body")
	}
}

// TestBuildSystemPrompt_InstructionsOnlyStillEmitsStableContext guards the
// cache-break assembly path: when only instructions are present (no sticky
// facts), StableContext must still be non-empty so assembleUserMessage emits
// the <!-- cache_break --> marker. Without this, instructions would silently
// fall back behind the marker and lose their caching benefit.
func TestBuildSystemPrompt_InstructionsOnlyStillEmitsStableContext(t *testing.T) {
	parts := BuildSystemPrompt(PromptOptions{
		BasePrompt:   "Base.",
		Instructions: "Never push to main without review.",
	})

	if parts.StableContext == "" {
		t.Fatal("StableContext should be non-empty when instructions are set (cache_break depends on this)")
	}
	if !strings.Contains(parts.StableContext, "Never push to main without review.") {
		t.Error("StableContext should contain instructions body")
	}
	if strings.Contains(parts.StableContext, "## Session Facts") {
		t.Error("StableContext should not emit an empty Session Facts header when sticky is empty")
	}
}

// TestBuildSystemPrompt_InstructionsBeforeStickyFacts locks in the ordering
// contract: the more-stable content (file-backed instructions) must precede
// sticky session facts inside StableContext so a cache-prefix can extend
// across sessions that share an instructions.md but differ in session source.
func TestBuildSystemPrompt_InstructionsBeforeStickyFacts(t *testing.T) {
	parts := BuildSystemPrompt(PromptOptions{
		BasePrompt:    "Base.",
		Instructions:  "Always use gofmt.",
		StickyContext: "Customer: Alice. Order #8891.",
	})

	instIdx := strings.Index(parts.StableContext, "## Instructions")
	factsIdx := strings.Index(parts.StableContext, "## Session Facts")
	if instIdx < 0 {
		t.Fatal("StableContext missing Instructions header")
	}
	if factsIdx < 0 {
		t.Fatal("StableContext missing Session Facts header")
	}
	if instIdx >= factsIdx {
		t.Errorf("Instructions must precede Session Facts in StableContext, got Instructions@%d Facts@%d", instIdx, factsIdx)
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

func TestBuildSystemPrompt_EmptyStableContext(t *testing.T) {
	// Neither instructions nor sticky facts → StableContext must be empty
	// (so assembleUserMessage skips the cache_break marker).
	parts := BuildSystemPrompt(PromptOptions{
		BasePrompt: "Base.",
	})

	if parts.StableContext != "" {
		t.Errorf("StableContext should be empty when neither instructions nor sticky facts are set, got: %q", parts.StableContext)
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

	if !strings.Contains(parts.StableContext, "[truncated]") {
		t.Error("expected truncation marker in stable context instructions")
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

func TestBuildSystemPrompt_OutputFormatDefault(t *testing.T) {
	// Empty OutputFormat defaults to markdown (GFM)
	parts := BuildSystemPrompt(PromptOptions{BasePrompt: "Base."})
	if !strings.Contains(parts.VolatileContext, "GitHub-flavored markdown") {
		t.Error("default OutputFormat should produce GFM guidance in volatile context")
	}
	if strings.Contains(parts.System, "GitHub-flavored markdown") {
		t.Error("formatting guidance should NOT be in static System (moved to volatile)")
	}
}

func TestBuildSystemPrompt_OutputFormatMarkdown(t *testing.T) {
	parts := BuildSystemPrompt(PromptOptions{BasePrompt: "Base.", OutputFormat: "markdown"})
	if !strings.Contains(parts.VolatileContext, "GitHub-flavored markdown") {
		t.Error("markdown format should produce GFM guidance")
	}
}

func TestBuildSystemPrompt_OutputFormatPlain(t *testing.T) {
	parts := BuildSystemPrompt(PromptOptions{BasePrompt: "Base.", OutputFormat: "plain"})
	if !strings.Contains(parts.VolatileContext, "plain text") {
		t.Error("plain format should produce plain text guidance")
	}
	if strings.Contains(parts.VolatileContext, "GitHub-flavored") {
		t.Error("plain format should NOT contain GFM guidance")
	}
}

func TestBuildSystemPrompt_VolatileContainsContextManagement(t *testing.T) {
	parts := BuildSystemPrompt(PromptOptions{BasePrompt: "Base."})
	if !strings.Contains(parts.VolatileContext, "tool results may be cleared") {
		t.Error("VolatileContext should contain context management warning")
	}
}

func TestBuildSystemPrompt_SubagentGuidanceContainsOutputSpec(t *testing.T) {
	parts := BuildSystemPrompt(PromptOptions{
		BasePrompt: "Base.",
		ToolNames:  []string{"subagent", "bash"},
	})
	if !strings.Contains(parts.System, "absolute paths") {
		t.Error("sub-agent guidance should mention absolute paths")
	}
	if !strings.Contains(parts.System, "concise report") {
		t.Error("sub-agent guidance should specify output format")
	}
}

func TestBuildSystemPrompt_SubagentGuidanceAbsentWithoutTool(t *testing.T) {
	parts := BuildSystemPrompt(PromptOptions{
		BasePrompt: "Base.",
		ToolNames:  []string{"bash", "file_read"},
	})
	if strings.Contains(parts.System, "Sub-Agents") {
		t.Error("sub-agent guidance should not appear without subagent tool")
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
