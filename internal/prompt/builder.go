package prompt

import (
	"fmt"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/skills"
)

// Layer character budgets.
const (
	maxMemoryChars       = 2000
	maxInstructionsChars = 16000
)

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
	ToolNames    []string // from ToolRegistry.SortedNames(), deterministic
	ServerTools  []string // server tool names (optional)
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
	// Rendered in the static system prompt. Empty when not in deferred mode.
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

	// 2. Available Tools (stable once session starts)
	sb.WriteString("\n\n## Available Tools\n")
	if len(opts.ToolNames) > 0 {
		sb.WriteString("You have these tools: ")
		sb.WriteString(strings.Join(opts.ToolNames, ", "))
		sb.WriteString(".")
	}
	if len(opts.ServerTools) > 0 {
		if len(opts.ToolNames) > 0 {
			sb.WriteString("\n")
		}
		sb.WriteString("You also have server-side tools: ")
		sb.WriteString(strings.Join(opts.ServerTools, ", "))
		sb.WriteString(".")
	}

	// 2b. Sub-agent guidance (only when subagent is available)
	if hasSubagent(opts.ToolNames) {
		sb.WriteString("\n\n## Using Sub-Agents\n")
		sb.WriteString("Sub-agents run full agent loops in isolation — powerful but not free.\n\n")
		sb.WriteString("**When to spawn:**\n")
		sb.WriteString("- Simple, specific tasks (read a file, search a term, run one command) → use direct tools.\n")
		sb.WriteString("- Broad tasks across multiple items or areas → spawn sub-agents immediately.\n")
		sb.WriteString("- Launch multiple agents concurrently in a SINGLE response for parallel execution.\n\n")
		sb.WriteString("**Sub-agent rules:**\n")
		sb.WriteString("- Once you delegate, commit fully — don't also do the work yourself.\n")
		sb.WriteString("- Sub-agents cannot see this conversation. Include ALL necessary context in the prompt.\n")
		sb.WriteString("- Sub-agents cannot spawn further sub-agents. Do not ask them to delegate.\n\n")
		sb.WriteString("**Writing the prompt:**\n")
		sb.WriteString("- Use absolute paths (e.g. /Users/name/project/file.txt), never relative.\n")
		sb.WriteString("- Tell the sub-agent to return a concise report: what was done, key findings, and relevant file paths (absolute).")
	}

	// 2c. Task tracking guidance (only when task tools are available)
	if hasTasks(opts.ToolNames) {
		sb.WriteString("\n\n## Task Tracking\n")
		sb.WriteString("Break down and manage your work with the task_create tool. These tools help you plan work and show progress to the user. Mark each task completed as soon as you finish it — do not batch.\n\n")
		sb.WriteString("**When to use:** 3+ step tasks, user provides multiple tasks, complex work needing planning.\n")
		sb.WriteString("**Skip for:** single actions, trivial changes, quick questions.\n\n")
		sb.WriteString("Workflow: create tasks → set in_progress before starting each → set completed immediately after finishing. ")
		sb.WriteString("Keep exactly one task in_progress at a time. Check task_list after completing each task.")
	}

	// 3. Available Skills (stable once session starts)
	if len(opts.Skills) > 0 {
		sb.WriteString("\n\n## Available Skills\n\n")
		sb.WriteString("You can activate a skill by calling the `use_skill` tool with the skill name.\n")
		sb.WriteString("Only activate a skill when the user's request matches the skill's purpose.\n\n")
		sb.WriteString("| Skill | Description |\n")
		sb.WriteString("|-------|-------------|\n")
		for _, s := range opts.Skills {
			sb.WriteString(fmt.Sprintf("| %s | %s |\n", s.Name, s.Description))
		}
	}

	// 3b. Deferred Tools (only in deferred mode)
	if len(opts.DeferredTools) > 0 {
		sb.WriteString("\n\n## Deferred Tools\n\n")
		sb.WriteString("The following tools are available via tool_search. When you need one,\n")
		sb.WriteString("call tool_search to load its schema, then IMMEDIATELY call the loaded\n")
		sb.WriteString("tool in your next response. NEVER stop to describe what you loaded or\n")
		sb.WriteString("ask the user what to do after loading — the user's request is already\n")
		sb.WriteString("stated above. Treat tool_search as a transparent preparation step,\n")
		sb.WriteString("not as an action to report on.\n\n")
		for _, dt := range opts.DeferredTools {
			sb.WriteString(fmt.Sprintf("- %s: %s\n", dt.Name, dt.Description))
		}
	}

	// 4. macOS automation guidance (only on darwin with relevant tools)
	if guidance := macOSAutomationGuidance(opts.ToolNames); guidance != "" {
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

func hasSubagent(toolNames []string) bool {
	return slices.Contains(toolNames, "subagent")
}

func hasTasks(toolNames []string) bool {
	return slices.Contains(toolNames, "task_create")
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
		sb.WriteString("## Instructions\n")
		sb.WriteString(truncate(inst, maxInstructionsChars))
	}

	if sticky := strings.TrimSpace(opts.StickyContext); sticky != "" {
		if sb.Len() > 0 {
			sb.WriteString("\n\n")
		}
		sb.WriteString("## Session Facts\n")
		sb.WriteString(sticky)
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

	// Context management warning
	sb.WriteString("\n\n## Context Management\n")
	sb.WriteString("When working with tool results, write down any important information you might need later in your response, as old tool results may be cleared automatically to manage context window size.")

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

// macOSAutomationGuidance returns workflow guidance for macOS automation tools,
// or empty string if not on darwin or no relevant tools are registered.
func macOSAutomationGuidance(toolNames []string) string {
	if runtime.GOOS != "darwin" {
		return ""
	}
	hasMacTools := false
	for _, name := range toolNames {
		if name == "accessibility" || name == "computer" || name == "wait_for" {
			hasMacTools = true
			break
		}
	}
	if !hasMacTools {
		return ""
	}
	return `## macOS Automation

When controlling macOS applications:

1. **Orient before acting**: Use accessibility annotate to see what's on screen before clicking. It returns a labeled screenshot with numbered elements.
2. **Use refs, not coordinates**: After annotate or read_tree, click elements by ref (e.g. ref="e14"). Only use coordinate clicks as a last resort.
3. **Specify app name**: Always include the app parameter. Use the exact name as shown in the Dock (e.g. "Finder", "Safari", "Google Chrome", "Notes").
4. **Wait, don't sleep**: After launching apps or navigating, use wait_for instead of bash sleep. Example: wait_for with condition="titleContains" value="Google".
5. **Browser tool for web content**: For interacting with web page elements, use the browser tool (DOM-level access). Use accessibility only for native macOS UI.
6. **Focus before typing**: Ensure the target app is frontmost before using computer type/hotkey. Use accessibility click on the target field first.
7. **CJK/emoji text**: computer type handles Chinese, Japanese, Korean, and emoji automatically via clipboard paste.`
}
