package prompt

import (
	"runtime"
	"strings"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/skills"
)

// TestBuildSystemPrompt_NudgesParallelToolUse verifies the system prompt
// encourages batching independent tool calls into a single response. This
// cuts block churn in the agent loop — the dominant long-session CHR drag
// once msgs * 1.5 exceeds Anthropic's ~20-block auto-lookback.
func TestBuildSystemPrompt_NudgesParallelToolUse(t *testing.T) {
	parts := BuildSystemPrompt(PromptOptions{
		BasePrompt:     "Base.",
		LocalToolNames: []string{"file_read", "bash", "grep"},
	})

	// Text signals — must mention parallelism AND the mechanism (tool_use block / single response).
	// Case-insensitive: nudge may emphasize words in uppercase.
	lower := strings.ToLower(parts.System)
	for _, keyword := range []string{"parallel", "single response", "tool_use"} {
		if !strings.Contains(lower, keyword) {
			t.Errorf("system prompt missing %q — should nudge parallel tool use to reduce block churn", keyword)
		}
	}
}

// TestBuildSystemPrompt_ParallelNudgeOnlyWhenToolsPresent verifies the nudge
// is omitted when no tools are available — adding it would waste tokens and
// pollute the cached prefix for tool-less agents.
func TestBuildSystemPrompt_ParallelNudgeOnlyWhenToolsPresent(t *testing.T) {
	parts := BuildSystemPrompt(PromptOptions{
		BasePrompt: "You answer questions without tools.",
	})
	if strings.Contains(parts.System, "parallel tool_use") || strings.Contains(parts.System, "SINGLE response") {
		t.Errorf("parallel nudge should be absent when no tools are registered:\n%s", parts.System)
	}
}

func TestBuildSystemPrompt_SystemIsStatic(t *testing.T) {
	// Two calls with different volatile content must produce identical System fields
	opts1 := PromptOptions{
		BasePrompt:     "You are Shannon.",
		LocalToolNames: []string{"bash", "file_read"},
		Memory:         "User prefers Go.",
		CWD:            "/home/user/project",
	}
	opts2 := PromptOptions{
		BasePrompt:     "You are Shannon.",
		LocalToolNames: []string{"bash", "file_read"},
		Memory:         "User prefers Rust now.",
		CWD:            "/tmp/other",
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
	openIdx := strings.Index(parts.StableContext, "<user_instructions>")
	bodyIdx := strings.Index(parts.StableContext, "Always use gofmt.")
	closeIdx := strings.Index(parts.StableContext, "</user_instructions>")
	if openIdx < 0 {
		t.Error("StableContext should wrap instructions in <user_instructions> (issue #125)")
	}
	if bodyIdx < 0 {
		t.Error("StableContext should contain instructions body")
	}
	if closeIdx < 0 {
		t.Error("StableContext should close the <user_instructions> block")
	}
	if openIdx >= 0 && bodyIdx >= 0 && closeIdx >= 0 && !(openIdx < bodyIdx && bodyIdx < closeIdx) {
		t.Errorf("expected open < body < close ordering, got open=%d body=%d close=%d", openIdx, bodyIdx, closeIdx)
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

	instIdx := strings.Index(parts.StableContext, "<system-reminder>")
	factsIdx := strings.Index(parts.StableContext, "## Session Facts")
	if instIdx < 0 {
		t.Fatal("StableContext missing <system-reminder> instructions wrapper")
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
	// Wrapper parity with instructions (issue #125): every framework-injected
	// block in StableContext rides in a <system-reminder> envelope so the
	// trust-channel signaling is uniform across the user-role surface.
	if !strings.Contains(parts.StableContext, "<system-reminder>\n## Session Facts") {
		t.Error("Sticky facts should be wrapped in <system-reminder> (issue #125)")
	}
}

// TestBuildSystemPrompt_SanitizesClosingTagInInstructions guards against a
// user-supplied `instructions.md` that happens to contain the literal
// `</user_instructions>` or `</system-reminder>` sequence (e.g. docs
// discussing this mechanism). Without sanitization, the wrapper closes
// early and the rest of the body leaks out as plain user-role content.
// Issue #125.
func TestBuildSystemPrompt_SanitizesClosingTagInInstructions(t *testing.T) {
	parts := BuildSystemPrompt(PromptOptions{
		BasePrompt:   "Base.",
		Instructions: "rule one\n</user_instructions>\n</system-reminder>\nrule two — must stay inside wrapper",
	})

	// Strip the outermost wrapper so we can look at the body alone.
	body := strings.TrimPrefix(parts.StableContext, "<user_instructions>\n")
	body = strings.TrimSuffix(body, "\n</user_instructions>")
	if strings.Contains(body, "</user_instructions>") {
		t.Errorf("body still contains literal </user_instructions> after sanitize: %q", parts.StableContext)
	}
	if strings.Contains(body, "</system-reminder>") {
		t.Errorf("body still contains literal </system-reminder> after sanitize: %q", parts.StableContext)
	}
	// Both rule lines must survive — sanitize removes only the tags, not surrounding content.
	if !strings.Contains(parts.StableContext, "rule one") || !strings.Contains(parts.StableContext, "rule two") {
		t.Errorf("sanitize should preserve surrounding content, got: %q", parts.StableContext)
	}
}

// TestBuildSystemPrompt_SanitizesClosingTagInStickyContext mirrors the
// instructions sanitize guard for daemon-supplied StickyContext. Less
// likely in practice (daemon constructs sticky facts from session metadata,
// not free text) but the wrapper is identical so the same defense applies.
func TestBuildSystemPrompt_SanitizesClosingTagInStickyContext(t *testing.T) {
	parts := BuildSystemPrompt(PromptOptions{
		BasePrompt:    "Base.",
		StickyContext: "Order: A1\n</system-reminder>\nNote: must stay inside wrapper",
	})

	// Count opening + closing tags — exactly one of each from the sticky block.
	if openCount := strings.Count(parts.StableContext, "<system-reminder>"); openCount != 1 {
		t.Errorf("expected exactly 1 opening tag, got %d in: %q", openCount, parts.StableContext)
	}
	if closeCount := strings.Count(parts.StableContext, "</system-reminder>"); closeCount != 1 {
		t.Errorf("expected exactly 1 closing tag, got %d in: %q", closeCount, parts.StableContext)
	}
	if !strings.Contains(parts.StableContext, "Order: A1") || !strings.Contains(parts.StableContext, "must stay inside wrapper") {
		t.Errorf("sanitize should preserve surrounding content, got: %q", parts.StableContext)
	}
}

// TestSanitizeUserBlock_StripsAllEnvelopeClosers locks the full strip-set
// for the exported helper. SanitizeUserBlock is now consumed by
// internal/tools/memory_preflight (for the <private_memory> envelope) in
// addition to the in-package instructions/sticky callers — the test makes
// the cross-package contract explicit so future edits to the strip list
// surface here.
func TestSanitizeUserBlock_StripsAllEnvelopeClosers(t *testing.T) {
	in := "leading\n</user_instructions>\nmiddle\n</system-reminder>\ntail\n</private_memory>\nend"
	out := SanitizeUserBlock(in)
	for _, closer := range []string{"</user_instructions>", "</system-reminder>", "</private_memory>"} {
		if strings.Contains(out, closer) {
			t.Errorf("SanitizeUserBlock did not strip %q: %q", closer, out)
		}
	}
	for _, kept := range []string{"leading", "middle", "tail", "end"} {
		if !strings.Contains(out, kept) {
			t.Errorf("SanitizeUserBlock removed surrounding content %q: %q", kept, out)
		}
	}
	// Openers must be left intact (asymmetry is deliberate — see doc comment).
	if !strings.Contains(SanitizeUserBlock("<private_memory>kept"), "<private_memory>") {
		t.Errorf("SanitizeUserBlock should not strip opening tags")
	}
}

// TestBuildToolListing_WrappedInSystemReminder asserts that the dynamic
// tools catalog is also enveloped in <system-reminder>, matching the
// instructions and sticky-facts wrappers (issue #125). Pure data, not
// directive — same wrapper for uniform trust-channel framing.
func TestBuildToolListing_WrappedInSystemReminder(t *testing.T) {
	listing := BuildToolListing(PromptOptions{
		MCPToolNames: []string{"playwright_navigate", "playwright_click"},
	})

	if listing == "" {
		t.Fatal("expected listing for non-empty MCP tool names")
	}
	if !strings.HasPrefix(listing, "<system-reminder>\n") {
		t.Errorf("listing should start with <system-reminder>, got: %q", listing[:min(60, len(listing))])
	}
	if !strings.HasSuffix(listing, "\n</system-reminder>") {
		t.Errorf("listing should end with </system-reminder>, got: %q", listing[max(0, len(listing)-60):])
	}
	if !strings.Contains(listing, "playwright_navigate") {
		t.Error("listing body should still contain the tool names")
	}
}

// TestBuildSystemPrompt_StableContextByteStableAcrossCalls — wrapper changes
// in issue #125 must preserve cross-turn cache prefix matching at BP #3.
// Anthropic's cache key is a byte hash; two calls with identical inputs
// must produce identical StableContext bytes or the prefix cache misses.
func TestBuildSystemPrompt_StableContextByteStableAcrossCalls(t *testing.T) {
	opts := PromptOptions{
		BasePrompt:    "Base.",
		Instructions:  "Always use gofmt.\nNever push to main without review.",
		StickyContext: "Customer: Alice. Order #8891.",
		MCPToolNames:  []string{"playwright_navigate"},
	}

	first := BuildSystemPrompt(opts).StableContext
	for i := 0; i < 5; i++ {
		got := BuildSystemPrompt(opts).StableContext
		if got != first {
			t.Fatalf("call %d produced different StableContext bytes (would break BP #3 cache)\n--- first ---\n%s\n--- got ---\n%s", i+1, first, got)
		}
	}
}

func TestBuildSystemPrompt_EmptyStableContext(t *testing.T) {
	// Neither instructions nor sticky facts → StableContext falls back to a
	// stable placeholder so assembleUserMessage still emits the cache_break
	// marker and the gateway attaches its third cache_control breakpoint.
	parts := BuildSystemPrompt(PromptOptions{
		BasePrompt: "Base.",
	})

	if parts.StableContext == "" {
		t.Fatal("StableContext should fall back to a non-empty placeholder to preserve the third cache breakpoint")
	}
	if !strings.Contains(parts.StableContext, "Active agent context.") {
		t.Errorf("StableContext should contain the session placeholder, got: %q", parts.StableContext)
	}
}

func TestBuildSystemPrompt_SystemContainsToolNames(t *testing.T) {
	parts := BuildSystemPrompt(PromptOptions{
		BasePrompt:     "Base.",
		LocalToolNames: []string{"file_read", "bash"},
	})

	if !strings.Contains(parts.System, "file_read") {
		t.Error("System should contain local tool names")
	}
}

// TestBuildSystemPrompt_SystemExcludesGatewayToolNames asserts gateway tool
// names are NOT in the system prompt — they're routed to BuildToolListing for
// user-message injection (issue #107). Was previously assertion-of-presence;
// flipped to assertion-of-absence.
func TestBuildSystemPrompt_SystemExcludesGatewayToolNames(t *testing.T) {
	parts := BuildSystemPrompt(PromptOptions{
		BasePrompt:       "Base.",
		GatewayToolNames: []string{"web_search"},
	})

	if strings.Contains(parts.System, "web_search") {
		t.Error("System must not contain gateway tool names (per-user drift source)")
	}
}

func TestBuildSystemPrompt_SystemContainsSkills(t *testing.T) {
	parts := BuildSystemPrompt(PromptOptions{
		BasePrompt: "Base.",
		Skills: []*skills.Skill{
			{Name: "pdf", Description: "Extract text from PDFs"},
		},
	})

	if strings.Contains(parts.System, "## Available Skills") {
		t.Error("system prompt should not contain skill listing (moved to user message)")
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

// TestBuildSystemPrompt_DeferredToolsExcludedFromSystem asserts deferred
// tools are NOT rendered in the system prompt — they vary per user (only
// appear when total tool count > 30) so they break BP #1 byte stability
// (issue #107). Routed to BuildToolListing instead.
func TestBuildSystemPrompt_DeferredToolsExcludedFromSystem(t *testing.T) {
	parts := BuildSystemPrompt(PromptOptions{
		BasePrompt:     "Base.",
		LocalToolNames: []string{"bash", "file_read", "tool_search"},
		DeferredTools: []DeferredToolSummary{
			{Name: "playwright_click", Description: "Click an element"},
			{Name: "playwright_type", Description: "Type text"},
		},
	})

	if strings.Contains(parts.System, "## Deferred Tools") {
		t.Error("System must not contain Deferred Tools section (per-user drift source)")
	}
	if strings.Contains(parts.System, "playwright_click") {
		t.Error("System must not contain deferred tool names")
	}
	if !strings.Contains(parts.System, "tool_search") {
		t.Error("System should still mention tool_search (it's a local tool)")
	}
}

func TestBuildSystemPrompt_NoDeferredSection_WhenEmpty(t *testing.T) {
	parts := BuildSystemPrompt(PromptOptions{
		BasePrompt:     "Base.",
		LocalToolNames: []string{"bash", "file_read"},
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

func TestBuildSystemPrompt_SkillsListCompact(t *testing.T) {
	opts := PromptOptions{
		BasePrompt: "You are Shannon.",
		Skills: []*skills.Skill{
			{Name: "skill-a", Description: strings.Repeat("long description words ", 20)},
			{Name: "skill-b", Description: "short desc"},
		},
	}
	p := BuildSystemPrompt(opts)
	// Skills must NOT appear in system prompt — they are injected as a user message instead.
	if strings.Contains(p.System, "## Available Skills") {
		t.Error("system prompt should not contain skill listing (moved to user message)")
	}
	for _, s := range opts.Skills {
		if strings.Contains(p.System, s.Name) {
			t.Fatalf("skill %s should not appear in system prompt", s.Name)
		}
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

func TestMacOSAutomationGuidance_NoStrandedHeader(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("darwin-only guidance")
	}
	// computer present but none of the bullet-emitting conditions match
	// → no stranded "## macOS Automation\n" header
	tests := []struct {
		name  string
		tools []string
	}{
		{"only-computer", []string{"computer"}},
		{"computer-and-wait_for", []string{"computer", "wait_for"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out := macOSAutomationGuidance(tc.tools)
			// "only-computer" currently produces zero bullets → must return ""
			// "computer-and-wait_for" produces wait_for bullet → must include it
			if tc.name == "only-computer" && out != "" {
				t.Fatalf("expected empty string for tools=%v, got %q", tc.tools, out)
			}
			if tc.name == "computer-and-wait_for" {
				if !strings.Contains(out, "## macOS Automation") {
					t.Fatalf("expected section header for tools=%v, got %q", tc.tools, out)
				}
				if !strings.Contains(out, "wait_for") {
					t.Fatalf("expected wait_for bullet for tools=%v, got %q", tc.tools, out)
				}
			}
		})
	}
}

func TestMacOSAutomationGuidance_AccessibilityOnly(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("darwin-only guidance")
	}
	out := macOSAutomationGuidance([]string{"accessibility"})
	if !strings.Contains(out, "## macOS Automation") {
		t.Fatalf("expected header, got %q", out)
	}
	if !strings.Contains(out, "accessibility") {
		t.Fatalf("expected accessibility bullet, got %q", out)
	}
	// Should NOT include the AX fallback bullet (requires both accessibility+computer)
	if strings.Contains(out, "Fall back to `computer`") {
		t.Fatalf("unexpected fallback bullet when only accessibility present: %q", out)
	}
}

// TestBuildSystemPrompt_MacOSGuidanceEmitted is an integration-level test
// for the BuildSystemPrompt → macOSAutomationGuidance path. Catches the
// regression class where macOSAutomationGuidance reads a stale field that
// the caller no longer populates (existing macOS unit tests bypass
// BuildSystemPrompt and call the helper directly, so they don't catch
// wiring bugs at the call site).
func TestBuildSystemPrompt_MacOSGuidanceEmitted(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("darwin-only guidance")
	}
	parts := BuildSystemPrompt(PromptOptions{
		BasePrompt:     "Base.",
		LocalToolNames: []string{"accessibility", "bash"},
	})
	if !strings.Contains(parts.System, "## macOS Automation") {
		t.Error("macOS guidance must appear when accessibility is in LocalToolNames")
	}
}

// TestBuildSystemPrompt_BP1ByteStableAcrossMCPConfigs locks in the cross-user
// cache-share invariant from issue #107: two users running the same agent on
// the same OS but with different MCP server sets must produce byte-identical
// System (BP #1) content.
func TestBuildSystemPrompt_BP1ByteStableAcrossMCPConfigs(t *testing.T) {
	userA := BuildSystemPrompt(PromptOptions{
		BasePrompt:     "Persona prompt.",
		LocalToolNames: []string{"bash", "file_read", "file_write"},
		MCPToolNames:   []string{"mcp_gmail_send", "mcp_gmail_search"},
	})
	userB := BuildSystemPrompt(PromptOptions{
		BasePrompt:     "Persona prompt.",
		LocalToolNames: []string{"bash", "file_read", "file_write"},
		MCPToolNames:   []string{"mcp_notion_create", "mcp_notion_query"},
	})

	if userA.System != userB.System {
		t.Errorf("System (BP #1) must be byte-identical across users with different MCP configs.\n"+
			"User A System len=%d\nUser B System len=%d\nDiff would expose per-user drift in BP #1.",
			len(userA.System), len(userB.System))
	}
}

// TestBuildSystemPrompt_SystemExcludesMCPNames guards that MCP tool names
// never appear in the system prompt — even if the caller mistakenly populates
// them. Catches regressions where someone adds them back to the prose line.
func TestBuildSystemPrompt_SystemExcludesMCPNames(t *testing.T) {
	parts := BuildSystemPrompt(PromptOptions{
		BasePrompt:     "Base.",
		LocalToolNames: []string{"bash"},
		MCPToolNames:   []string{"mcp_gmail_send"},
	})
	if strings.Contains(parts.System, "mcp_gmail_send") {
		t.Error("System must not contain MCP tool names (per-user drift source — see issue #107)")
	}
}

func TestBuildToolListing_EmptyWhenNoDynamicTools(t *testing.T) {
	got := BuildToolListing(PromptOptions{
		LocalToolNames: []string{"bash", "file_read"},
	})
	if got != "" {
		t.Errorf("expected empty listing when no MCP/gateway/deferred tools; got %q", got)
	}
}

func TestBuildToolListing_IncludesMCPNames(t *testing.T) {
	got := BuildToolListing(PromptOptions{
		MCPToolNames: []string{"mcp_gmail_send", "mcp_gmail_search"},
	})
	if !strings.Contains(got, "mcp_gmail_send") || !strings.Contains(got, "mcp_gmail_search") {
		t.Errorf("listing missing MCP tool names; got %q", got)
	}
	if !strings.Contains(got, "## Dynamic Tools") {
		t.Errorf("listing missing section heading; got %q", got)
	}
}

func TestBuildToolListing_IncludesGatewayNames(t *testing.T) {
	got := BuildToolListing(PromptOptions{
		GatewayToolNames: []string{"web_search", "web_fetch"},
	})
	if !strings.Contains(got, "web_search") || !strings.Contains(got, "web_fetch") {
		t.Errorf("listing missing gateway tool names; got %q", got)
	}
}

func TestBuildToolListing_IncludesDeferredTools(t *testing.T) {
	got := BuildToolListing(PromptOptions{
		DeferredTools: []DeferredToolSummary{
			{Name: "playwright_click", Description: "Click an element"},
		},
	})
	if !strings.Contains(got, "playwright_click") {
		t.Errorf("listing missing deferred tool name; got %q", got)
	}
	if !strings.Contains(got, "tool_search") {
		t.Errorf("listing should mention tool_search for loading deferred schemas; got %q", got)
	}
}

func TestBuildToolListing_DeferredDescriptionTruncated(t *testing.T) {
	longDesc := strings.Repeat("x", 200)
	got := BuildToolListing(PromptOptions{
		DeferredTools: []DeferredToolSummary{
			{Name: "long_tool", Description: longDesc},
		},
	})
	if !strings.Contains(got, "...") {
		t.Errorf("expected truncation marker in long deferred description; got %q", got)
	}
}

func TestBuildSystemPrompt_StableContextContainsToolListing(t *testing.T) {
	parts := BuildSystemPrompt(PromptOptions{
		BasePrompt:   "Base.",
		MCPToolNames: []string{"mcp_gmail_send"},
	})
	if !strings.Contains(parts.StableContext, "mcp_gmail_send") {
		t.Errorf("StableContext should contain MCP tool listing; got %q", parts.StableContext)
	}
	if !strings.Contains(parts.StableContext, "## Dynamic Tools") {
		t.Errorf("StableContext should contain ## Dynamic Tools heading")
	}
}

func TestBuildSystemPrompt_StableContextOmitsToolListingWhenEmpty(t *testing.T) {
	parts := BuildSystemPrompt(PromptOptions{
		BasePrompt: "Base.",
	})
	if strings.Contains(parts.StableContext, "## Dynamic Tools") {
		t.Error("StableContext should not have ## Dynamic Tools when no dynamic tools present")
	}
}

// TestBuildSystemPrompt_CommunicatingSection_Present verifies the static
// system prompt includes the user-communication section that instructs the
// model to emit preamble text blocks. Asserts the section header and several
// load-bearing phrases (mid-sentence anchors that survive minor wording
// edits but break if the section is dropped).
func TestBuildSystemPrompt_CommunicatingSection_Present(t *testing.T) {
	parts := BuildSystemPrompt(PromptOptions{
		BasePrompt:     "Base.",
		LocalToolNames: []string{"file_read", "bash"},
	})

	required := []string{
		"## Text output (does not apply to tool calls)",
		"Before your first tool call, state in one sentence what you're about to do",
		"give short updates at key moments",
		"Brief is good — silent is not",
		"Don't narrate your internal deliberation",
		"Don't open with conversational interjections",
		"For routine task-completion summaries",
		"Do not use a colon before a tool call",
	}
	for _, phrase := range required {
		if !strings.Contains(parts.System, phrase) {
			t.Errorf("system prompt missing required phrase %q", phrase)
		}
	}
}

// TestBuildSystemPrompt_CommunicatingSection_ByteStableAcrossInvocations
// verifies that two invocations with identical input produce byte-equal
// System fields. Cache-stability prerequisite: the section must contain no
// per-invocation variables (time, IDs, randomness).
func TestBuildSystemPrompt_CommunicatingSection_ByteStableAcrossInvocations(t *testing.T) {
	opts := PromptOptions{
		BasePrompt:     "You are Shannon.",
		LocalToolNames: []string{"bash", "file_read"},
	}
	a := BuildSystemPrompt(opts).System
	b := BuildSystemPrompt(opts).System
	if a != b {
		t.Fatalf("System differs between identical invocations.\nA len=%d\nB len=%d", len(a), len(b))
	}
}

// TestBuildSystemPrompt_CommunicatingSection_ByteStableAcrossOutputFormat
// verifies that the System field is byte-identical when OutputFormat differs
// between "plain" and "markdown". This locks D2 of the spec: the
// communication section must NOT branch on OutputFormat, otherwise BP #1
// cache fragments across cloud-distributed (plain) vs TUI/Desktop (markdown)
// users.
func TestBuildSystemPrompt_CommunicatingSection_ByteStableAcrossOutputFormat(t *testing.T) {
	base := PromptOptions{
		BasePrompt:     "You are Shannon.",
		LocalToolNames: []string{"bash", "file_read"},
	}
	plain := base
	plain.OutputFormat = "plain"
	markdown := base
	markdown.OutputFormat = "markdown"

	plainParts := BuildSystemPrompt(plain).System
	mdParts := BuildSystemPrompt(markdown).System
	if plainParts != mdParts {
		t.Fatalf("System must be byte-equal across OutputFormat values (D2). plain len=%d, markdown len=%d", len(plainParts), len(mdParts))
	}
}
