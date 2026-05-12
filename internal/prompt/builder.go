package prompt

import (
	"fmt"
	"runtime"
	"strings"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/skills"
)

// Layer character budgets.
const (
	maxMemoryChars       = 2000
	maxInstructionsChars = 16000
)

// UserInstructionsTag is the opening XML tag this package wraps around
// instructions.md / rules/*.md content in the user-message StableContext.
// Exported so the agent persona (internal/agent/loop.go:defaultPersona) can
// reference the exact same literal — renaming the wrapper there forces a
// compile error in callers that haven't tracked the change. Issue #125
// round 4: mechanically lock the semantic coupling between persona-note
// text and emit site.
const UserInstructionsTag = "<user_instructions>"

// DeferredToolSummary is a lightweight name+description pair for deferred tool listings.
// Mirrors agent.ToolSummary but avoids importing the agent package from prompt.
type DeferredToolSummary struct {
	Name        string
	Description string
}

// PromptOptions configures the system prompt assembly.
type PromptOptions struct {
	BasePrompt   string   // persona + core operational rules
	Memory       string   // from LoadMemory (~500 tokens budget) — rendered in VolatileContext
	Instructions string   // from LoadInstructions (~4000 tokens budget) — rendered in StableContext so it joins the cacheable prefix
	// LocalToolNames is the deterministic-ordered list of locally-registered
	// tool names (built-ins like file_read, bash, etc.). Rendered in the
	// system prompt's "## Available Tools" line. Excludes MCP and gateway
	// tools so the line stays byte-stable across users with different MCP
	// configurations — see issue #107.
	LocalToolNames []string
	// MCPToolNames is the list of names from MCP-origin tools. Rendered in
	// BuildToolListing for injection into the user message (StableContext),
	// not in the system prompt — they vary per user.
	MCPToolNames []string
	// GatewayToolNames is the list of names from gateway-origin tools.
	// Same routing rationale as MCPToolNames.
	GatewayToolNames []string
	MCPContext   string   // context from MCP servers (auth info, usage hints)
	Skills       []*skills.Skill
	CWD          string // current working directory
	SessionInfo  string // optional session context (currently unused by agent loop)
	MemoryDir    string // directory containing MEMORY.md for agent memory writes
	// StickyContext holds session-scoped facts injected verbatim into StableContext.
	// Never truncated or compacted. Use for key transactional data (IDs, amounts, names)
	// that must survive context compaction. Populated by the daemon runner with session
	// source/channel/task metadata, or by callers needing persistent session facts.
	StickyContext string
	// DeferredTools lists tools available via tool_search (deferred mode only).
	// Rendered in BuildToolListing for injection into StableContext (user
	// message, BP #3). Excluded from the system prompt for BP #1 byte
	// stability. See issue #107. Empty when not in deferred mode.
	DeferredTools []DeferredToolSummary
	// ModelID is the model identifier (e.g., "claude-sonnet-4-20250514").
	// Injected into volatile context so the model knows its own identity.
	ModelID string
	// ContextWindow is the model's context window size in tokens.
	// Injected into volatile context when > 0.
	ContextWindow int
	// OutputFormat controls formatting guidance: "markdown" (default, GFM) or
	// "plain" (for cloud-distributed sessions where Shannon Cloud handles
	// final channel rendering). Empty defaults to "markdown".
	OutputFormat string
}

// PromptParts separates the system prompt into cacheable and volatile sections.
// The gateway caches System as a single block. StableContext and VolatileContext
// are injected into the user message with a <!-- cache_break --> separator.
//
// Layer semantics:
//   - System         : persona, core rules, tool names, skills — gateway-cached.
//   - StableContext  : shared org-wide instructions (instructions.md + rules/*.md +
//                      project overrides) and sticky session facts. Changes only
//                      across sessions or on file edits. Sits before the
//                      cache_break marker in the user message so providers that
//                      reuse the pre-break prefix can hit on it.
//   - VolatileContext: memory (mutated by memory_append mid-session), date/time,
//                      CWD, MCP server context, output format guidance. Sits
//                      after the cache_break marker and is re-sent each turn.
type PromptParts struct {
	System          string // static: persona + rules + guidance + tool names + skills (cached by gateway)
	StableContext   string // per-session cacheable prefix: shared instructions + sticky facts (before cache_break)
	VolatileContext string // changes per-turn: memory, date/time, CWD, MCP, format guidance (after cache_break)
}

// BuildSystemPrompt assembles prompt parts from layers.
// System contains only content that is stable across turns.
// Shared instructions and sticky facts go to StableContext (cached prefix).
// Volatile content (memory, date/time, CWD, MCP) goes to VolatileContext.
//
// Note: an attempt to move VolatileContext into System (after a
// `<!-- volatile -->` marker) was reverted — it caused tools cache to break
// every minute because the system_volatile bytes sit BEFORE the tools
// cache_control. Baseline placement (volatile in user_1 after cache_break) is
// actually optimal: it only pollutes the rolling marker cache, leaving system
// + tools + user_1.stable caches intact.
func BuildSystemPrompt(opts PromptOptions) PromptParts {
	system := buildStaticSystem(opts)
	stable := buildStableContext(opts)
	volatile := buildVolatileContext(opts)
	return PromptParts{
		System:          system,
		StableContext:   stable,
		VolatileContext: volatile,
	}
}

// buildStaticSystem assembles content that never changes between turns in a session.
func buildStaticSystem(opts PromptOptions) string {
	var sb strings.Builder

	// 1. Base prompt (persona + core rules — unlimited)
	sb.WriteString(opts.BasePrompt)

	// Language policy. Byte-stable across all sessions and users so it joins
	// the cacheable system prefix. Pairs with the shorter per-turn reminder in
	// VolatileContext which re-anchors the rule against long-session drift.
	sb.WriteString("\n\n## Language\n")
	sb.WriteString("Match the user's language on first contact and stay consistent for the rest of the session. " +
		"If the user writes primarily in Chinese, respond in Chinese; if in English, respond in English; " +
		"follow the same rule for any other language. Only switch response language when the user explicitly asks " +
		"(e.g. \"please reply in English\"). Mixed-language user input — such as one English technical term inside a " +
		"Chinese sentence — is NOT a language-switch signal; continue in the established language. " +
		"Code identifiers, file paths, CLI commands, and technical terms (API names, library names, error messages) " +
		"remain in their original form regardless of response language. " +
		"Maintain full orthographic correctness — all accents, diacritics, and special characters.")

	// 2. Available Tools — only locally-registered tools, byte-stable across
	// users. MCP and gateway tools are listed in the user message (BuildToolListing)
	// to keep BP #1 (system_stable) byte-identical across tenants with different
	// MCP configurations. See issue #107 / docs/cache-strategy.md.
	sb.WriteString("\n\n## Available Tools\n")
	if len(opts.LocalToolNames) > 0 {
		sb.WriteString("You have these tools: ")
		sb.WriteString(strings.Join(opts.LocalToolNames, ", "))
		sb.WriteString(".")
	}

	// Parallel tool-use nudge: agent loops that fire N tool calls across N
	// iterations grow msgs past Anthropic's ~20-block auto-lookback window,
	// causing CHR decay in long sessions. Batching independent calls into
	// ONE response collapses N iterations → 1, keeping the rolling marker
	// reachable. Only add when tools are actually registered — tool-less
	// agents would just pay extra cached-prefix tokens.
	// Gate the nudge on LocalToolNames only — MCP/Gateway tool names are
	// per-user and live outside the system prompt (issue #107). Including
	// them here would create a theoretical BP #1 drift surface for the
	// degenerate "MCP-only, zero local tools" agent (does not exist in
	// production but worth keeping out of the byte-equality contract).
	if len(opts.LocalToolNames) > 0 {
		sb.WriteString("\n\nWhen you need independent pieces of information " +
			"(read multiple files, check several conditions, fetch data from different sources), " +
			"prefer calling ALL the tools in a SINGLE response with multiple parallel tool_use blocks " +
			"rather than across sequential turns. This amortizes prompt-cache cost and reduces latency.\n" +
			"Example — INEFFICIENT (3 turns):\n" +
			"  turn 1: file_read A\n" +
			"  turn 2: file_read B\n" +
			"  turn 3: file_read C\n" +
			"Example — EFFICIENT (1 turn, 3 parallel tool_use blocks in one response):\n" +
			"  turn 1: file_read A + file_read B + file_read C\n" +
			"Only sequence when later calls genuinely depend on earlier results.")
	}

	// Text output — stable across sessions/users/format. See
	// docs/superpowers/specs/2026-05-07-agent-preamble-output-design.md.
	// Byte-equal across invocations to keep BP #1 (system_stable) cacheable.
	// Wording aligned with Claude Code's latest "# Text output" section
	// (2026-05-07 iteration after observing Claude 4 over-applied "silence is correct").
	sb.WriteString("\n\n## Text output (does not apply to tool calls)\n")
	sb.WriteString("Assume users can't see most tool calls or thinking — only your text output. " +
		"Before your first tool call, state in one sentence what you're about to do. " +
		"While working, give short updates at key moments: when you find something, " +
		"when you change direction, or when you hit a blocker. " +
		"Brief is good — silent is not. One sentence per update is almost always enough.\n\n" +
		"Don't narrate your internal deliberation. User-facing text should be relevant " +
		"communication to the user, not a running commentary on your thought process. " +
		"State results and decisions directly, and focus user-facing text on relevant updates for the user.\n\n" +
		"When you do write updates, write so the reader can pick up cold: complete sentences, " +
		"no unexplained jargon or shorthand from earlier in the session. " +
		"But keep it tight — a clear sentence is better than a clear paragraph.\n\n" +
		"For routine task-completion summaries, use one or two sentences: what changed and what's next. " +
		"Do not add extra wrap-up prose when the user asked for a richer answer.\n\n" +
		"Don't open with conversational interjections like \"Done!\", \"Got it\", \"Sure\", or \"Great question\" — " +
		"lead with the substance (\"Reading the four files in parallel.\") instead.\n\n" +
		"Avoid markdown headers, tables, and heavy formatting in updates, since some channels strip rich text.\n\n" +
		"Do not use a colon before a tool call. " +
		"Text like \"Let me read the file:\" followed immediately by a tool_use block must be written as " +
		"\"Let me read the file.\" with a period — the trailing colon implies inline content that never arrives.")

	// Skills and dynamic tool listings (MCP, gateway, deferred) are emitted
	// in the user message (StableContext via BuildToolListing) to keep this
	// system prompt byte-stable across users. See issue #107.

	// 4. macOS automation guidance (only on darwin with relevant tools)
	if guidance := macOSAutomationGuidance(opts.LocalToolNames); guidance != "" {
		sb.WriteString("\n\n")
		sb.WriteString(guidance)
	}

	// 5. Memory Persistence guidance (stable — depends only on memoryDir presence)
	if opts.MemoryDir != "" {
		sb.WriteString("\n\n## Memory Persistence\n")
		sb.WriteString("Your current memory is shown in the context section below. When you discover something worth remembering across future conversations, use the `memory_append` tool to add new entries.\n")
		sb.WriteString("IMPORTANT: NEVER use file_write or file_edit on MEMORY.md — they race under concurrent sessions. The memory_append tool is flock-protected and safe.\n")
		sb.WriteString("Good candidates for memory:\n")
		sb.WriteString("- Decisions the user made (technical, design, or preferences)\n")
		sb.WriteString("- User corrections about how they want to work\n")
		sb.WriteString("- Important facts about projects, people, or systems\n")
		sb.WriteString("- Patterns, gotchas, or insights you discovered together\n")
		sb.WriteString("- Configuration or reference information that was hard to find\n\n")
		sb.WriteString("Keep entries as short one-line bullets. Do NOT save ephemeral task status, code snippets, or things already documented in project files. Your context is automatically compacted in long sessions — anything not written to memory may be lost.")
	}

	return sb.String()
}

// buildStableContext assembles the cacheable per-session prefix: shared
// instructions followed by sticky session facts. Placed before the
// <!-- cache_break --> marker in the user message so providers that reuse the
// pre-break prefix have a chance to cache-hit on it within a session.
//
// Ordering: instructions come first because they're the more stable of the
// two — file-backed and rarely edited — while sticky facts vary per session
// source. Putting the stabler content first gives the gateway/provider more
// opportunity to extend a cached prefix. Whether that actually produces a
// cross-session cache hit depends on upstream gateway/provider behavior and
// on the rest of the prompt state matching, not just the instructions text.
//
// Truncation: shared instructions are bounded by maxInstructionsChars to keep
// the cached prefix within a predictable budget. Oversized content is trimmed
// with a [truncated] marker telling the author to reduce file content.
func buildStableContext(opts PromptOptions) string {
	var sb strings.Builder

	if inst := strings.TrimSpace(opts.Instructions); inst != "" {
		// Wrap in <user_instructions> rather than a bare `## Instructions`
		// markdown header. The user-role placement (chosen for BP #3 cache
		// economics — see commit 7c897b6) means Claude treats the block
		// through its prompt-injection lens; a directive markdown header in
		// user content is a textbook injection signature.
		//
		// We do NOT use <system-reminder> here — that tag is Anthropic's
		// internal vocabulary for trusted system signals, and Claude 4.X
		// is trained to flag user-supplied content wearing that tag as a
		// forged-system-signal injection (the opposite of what we want).
		// <user_instructions> is a neutral user-domain tag with no such
		// training collision; it gives the model a clear semantic boundary
		// ("this block is the user's persistent rules, not an injection")
		// while staying inside the cacheable user-message prefix. Issue #125.
		sb.WriteString(UserInstructionsTag + "\n")
		sb.WriteString(sanitizeUserBlock(truncate(inst, maxInstructionsChars)))
		sb.WriteString("\n</user_instructions>")
	}

	if sticky := strings.TrimSpace(opts.StickyContext); sticky != "" {
		if sb.Len() > 0 {
			sb.WriteString("\n\n")
		}
		// Wrap for parity with the instructions block above. Sticky facts
		// are data (customer/order info), not directives — so they don't
		// currently trip Claude's injection sensor — but applying the same
		// trust-channel wrapper across every framework-injected block keeps
		// the user-role surface uniform. Issue #125.
		sb.WriteString("<system-reminder>\n## Session Facts\n")
		sb.WriteString(sanitizeUserBlock(sticky))
		sb.WriteString("\n</system-reminder>")
	}

	// Per-user dynamic tool catalog. Routed here (BP #3, per-session cache)
	// so it never pollutes BP #1 (system_stable, cross-user shared cache).
	// See issue #107.
	if listing := BuildToolListing(opts); listing != "" {
		if sb.Len() > 0 {
			sb.WriteString("\n\n")
		}
		sb.WriteString(listing)
	}

	// Guarantee a non-empty stable prefix so the gateway attaches a third
	// cache_control breakpoint (on the user message stable block). When this
	// is empty the gateway's Anthropic provider falls through its
	// empty-text-block guard and skips the breakpoint entirely, leaving the
	// user message uncached. The literal text is stable across all sessions
	// (no time, no IDs) so the extra bytes go into a shareable cached prefix.
	if sb.Len() == 0 {
		sb.WriteString("## Session\nActive agent context.")
	}

	return sb.String()
}

// buildVolatileContext assembles content that changes between turns.
// Placed after the <!-- cache_break --> marker in the user message.
func buildVolatileContext(opts PromptOptions) string {
	var sb strings.Builder

	// Date/time + CWD + model identity + session info
	sb.WriteString("## Context\n")
	sb.WriteString("Current date: " + time.Now().Format("2006-01-02 15:04 MST"))
	if opts.CWD != "" {
		sb.WriteString("\nWorking directory: " + opts.CWD)
	}
	if opts.ModelID != "" {
		sb.WriteString("\nModel: " + opts.ModelID)
	}
	if opts.ContextWindow > 0 {
		sb.WriteString(fmt.Sprintf("\nContext window: %d tokens", opts.ContextWindow))
	}
	if opts.SessionInfo != "" {
		sb.WriteString("\n" + opts.SessionInfo)
	}

	// Output formatting guidance
	sb.WriteString("\n\n## Output Format\n")
	sb.WriteString(formatGuidance(opts.OutputFormat))

	// Per-turn language reminder. Short reinforcement of the full policy in the
	// System section. Byte-stable (same text every turn) so it does not fragment
	// any per-turn cache, but positioning near the user message anchors against
	// drift when long sessions accumulate English tool output.
	//
	// On turn 0 "the language already established" is vacuous — nothing has been
	// established yet. The static System section's "Match the user's language on
	// first contact" rule handles that case; this reminder takes over from turn 1
	// onward when there's actually an established language to stay consistent with.
	sb.WriteString("\n\n## Language\n")
	sb.WriteString("Respond in the language already established with the user in this session. " +
		"If the user has not asked for a different language, stay consistent — do not switch even when tool output, " +
		"skill descriptions, or system messages arrive in a different language. Keep code and technical identifiers in their original form.")

	// Memory — stays volatile: memory_append can mutate MEMORY.md during a
	// turn, so the block must be re-read and re-sent each Run(). Instructions
	// live in StableContext (cacheable prefix), not here.
	if mem := strings.TrimSpace(opts.Memory); mem != "" {
		sb.WriteString("\n\n## Memory\n")
		sb.WriteString(truncate(mem, maxMemoryChars))
	}

	// MCP server context
	if mcp := strings.TrimSpace(opts.MCPContext); mcp != "" {
		sb.WriteString("\n\n## MCP Server Context\n")
		sb.WriteString(mcp)
	}

	return sb.String()
}

// formatGuidance returns output formatting instructions based on the profile.
func formatGuidance(format string) string {
	switch format {
	case "plain":
		return "Format responses as plain text. Use short paragraphs and simple bullet points. " +
			"Avoid markdown tables, fenced code blocks, headers, bold/italic, and other rich formatting. " +
			"Use indentation or blank lines for structure. Keep lines short and readable."
	default: // "markdown" or empty
		return "Format text responses using GitHub-flavored markdown (GFM): " +
			"use headers, fenced code blocks with language tags, lists, bold/italic, and tables where appropriate."
	}
}

// truncate limits s to maxChars, appending [truncated] if trimmed.
func truncate(s string, maxChars int) string {
	r := []rune(s)
	if len(r) <= maxChars {
		return s
	}
	return string(r[:maxChars]) + "\n[truncated]"
}

// sanitizeUserBlock strips wrapper closing tags from user-supplied content
// so the envelope around it cannot be terminated early. We strip BOTH
// `</user_instructions>` (used to wrap instructions.md) and
// `</system-reminder>` (used to wrap sticky facts) — defense in depth, since
// the same body could in principle be wrapped under either tag depending on
// the call site and we don't want either to leak out as plain user content.
//
// The asymmetry — strip closers but not openers — is deliberate. An opener
// leaking through produces a nested but still well-formed wrapper (the body
// stays inside the outer envelope). A closer leaking through truncates the
// wrapper and the rest of the body escapes into plain user content, which
// is the exact failure mode this PR exists to prevent. Stripping only
// closers fixes the dangerous case without spending cycles on the safe one.
// Issue #125.
func sanitizeUserBlock(s string) string {
	s = strings.ReplaceAll(s, "</user_instructions>", "")
	s = strings.ReplaceAll(s, "</system-reminder>", "")
	return s
}

// macOSAutomationGuidance returns workflow guidance for macOS automation tools,
// or empty string if not on darwin or no relevant tools are registered.
// Each bullet is conditional on the actual tool presence to avoid emitting
// guidance for tools the session won't use.
func macOSAutomationGuidance(toolNames []string) string {
	if runtime.GOOS != "darwin" {
		return ""
	}
	has := func(name string) bool {
		for _, n := range toolNames {
			if n == name {
				return true
			}
		}
		return false
	}
	var bullets strings.Builder
	if has("accessibility") {
		bullets.WriteString("- Prefer `accessibility` (AX API) over `computer` for UI interactions — faster, no screenshot needed.\n")
		bullets.WriteString("- After annotate or read_tree, click elements by ref (e.g. ref=\"e14\"). Only use coordinate clicks as a last resort.\n")
		bullets.WriteString("- Always include the app parameter. Use the exact name as shown in the Dock.\n")
		bullets.WriteString("- Ensure the target app is frontmost before typing. Use accessibility click on the target field first.\n")
	}
	if has("computer") && has("accessibility") {
		bullets.WriteString("- Fall back to `computer` only when AX fails or the target is a canvas/web element.\n")
	}
	if has("browser") {
		bullets.WriteString("- For interacting with web page elements, use `browser` (DOM-level access). Use accessibility only for native macOS UI.\n")
	}
	if has("wait_for") {
		bullets.WriteString("- Use `wait_for` to poll for UI state instead of bash sleep.\n")
	}
	if bullets.Len() == 0 {
		return ""
	}
	return "## macOS Automation\n" + bullets.String()
}

// BuildToolListing emits a per-user tool catalog (MCP + gateway + deferred)
// for injection into the user message's StableContext. Returns "" when
// nothing dynamic is registered.
//
// Routing rationale (issue #107): these names vary per user (different MCP
// configs, different gateway tool sets) and would break BP #1 (system_stable)
// cross-user byte stability if rendered into the system prompt. The user
// message's StableContext is a per-session cache (BP #3), which already does
// not share across users — putting the listing there is zero-cost relative
// to the original BP #1 placement, while letting BP #1 become byte-stable.
//
// The model still discovers MCP/gateway tools from the tools[] array (their
// authoritative source); this listing is a discovery hint that mirrors what
// the deprecated "## Available Tools" prose used to provide.
func BuildToolListing(opts PromptOptions) string {
	if len(opts.MCPToolNames) == 0 && len(opts.GatewayToolNames) == 0 && len(opts.DeferredTools) == 0 {
		return ""
	}

	var sb strings.Builder
	// Wrap for parity with the instructions and sticky-facts blocks in
	// buildStableContext (issue #125). Tool catalogs are pure data — names
	// + short descriptions — so they aren't directive-shaped, but the
	// uniform <system-reminder> wrapping signals "framework-supplied
	// context" across every user-role injection point.
	sb.WriteString("<system-reminder>\n## Dynamic Tools\n")
	sb.WriteString("These tools are also available — they vary per user/configuration. " +
		"Discover full schemas through the tools[] array; the names below are a quick reference.\n")

	if len(opts.MCPToolNames) > 0 {
		sb.WriteString("\nMCP tools: ")
		sb.WriteString(strings.Join(opts.MCPToolNames, ", "))
		sb.WriteString(".")
	}
	if len(opts.GatewayToolNames) > 0 {
		sb.WriteString("\nGateway tools: ")
		sb.WriteString(strings.Join(opts.GatewayToolNames, ", "))
		sb.WriteString(".")
	}
	if len(opts.DeferredTools) > 0 {
		sb.WriteString("\n\nDeferred tools (load via `tool_search` before calling):\n")
		for _, dt := range opts.DeferredTools {
			desc := dt.Description
			runes := []rune(desc)
			if len(runes) > 60 {
				desc = string(runes[:57]) + "..."
			}
			sb.WriteString("- ")
			sb.WriteString(dt.Name)
			sb.WriteString(": ")
			sb.WriteString(desc)
			sb.WriteString("\n")
		}
	}
	sb.WriteString("\n</system-reminder>")

	return sb.String()
}
