package agent

import (
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

// timeBasedClearedMarker is the literal string that replaces a tool_result's
// content when time-based compaction fires. The placeholder lets the model
// see "this was a tool result here, but its content was cleared" rather than
// just deleting the block (which would orphan the tool_use_id).
const timeBasedClearedMarker = "[Old tool result content cleared]"

// compactableTools enumerates tools whose results are eligible for
// time-based clearing. Other tools' results are kept verbatim because their
// content is the task payload, not retrievable redundancy:
//
//   - browser_* / computer / screenshot: the page snapshot or pixel buffer
//     IS what the model is reasoning about; clearing it blinds the agent.
//   - cloud_delegate: the result is the deliverable for the user.
//   - think: internal reasoning is not retrievable from anywhere else.
//
// Whitelist (rather than blacklist) is the conservative default — adding a
// tool here is opt-in.
var compactableTools = map[string]bool{
	"file_read":      true,
	"file_write":     true,
	"file_edit":      true,
	"bash":           true,
	"grep":           true,
	"glob":           true,
	"http":           true,
	"directory_list": true,
}

// TimeBasedCompactConfig controls when time-based microcompaction fires.
//
// The default policy is disabled. When enabled, the 60-minute seed is an
// operator-tunable idle-history threshold; it is intentionally independent of
// Cloud's prompt-cache TTL policy.
type TimeBasedCompactConfig struct {
	// Enabled is the master switch. When false, time-based compaction is a no-op.
	Enabled bool
	// GapThresholdMinutes triggers compaction when (now - lastAssistantAt)
	// exceeds this value. Default 60.
	GapThresholdMinutes int
	// KeepRecent retains this many most-recent compactable tool results.
	// Older results are cleared. Default 5 — enough for the model to keep a
	// recent working context, few enough that long-idle sessions actually
	// shed bytes.
	KeepRecent int
}

// DefaultTimeBasedCompactConfig is the safe default — disabled until the
// operator opts in.
func DefaultTimeBasedCompactConfig() TimeBasedCompactConfig {
	return TimeBasedCompactConfig{
		Enabled:             false,
		GapThresholdMinutes: 60,
		KeepRecent:          5,
	}
}

// evaluateTimeBasedCompactTrigger returns the measured gap when the trigger
// should fire, or zero + false when it should not. Extracted from the action
// so callers can consult the predicate without coupling to the clearing
// behavior.
//
// Returns zero + false when:
//   - cfg.Enabled is false
//   - lastAssistantAt is zero (no prior assistant response in this loop yet)
//   - gap < cfg.GapThresholdMinutes
func evaluateTimeBasedCompactTrigger(lastAssistantAt time.Time, cfg TimeBasedCompactConfig) (gap time.Duration, fire bool) {
	if !cfg.Enabled {
		return 0, false
	}
	if lastAssistantAt.IsZero() {
		return 0, false
	}
	gap = time.Since(lastAssistantAt)
	if gap < time.Duration(cfg.GapThresholdMinutes)*time.Minute {
		return gap, false
	}
	return gap, true
}

// collectCompactableToolUseIDs returns ordered tool_use IDs from assistant
// messages whose tool name is in compactableTools. The order matters because
// timeBasedCompact uses tail-keep semantics ("keep the last N").
func collectCompactableToolUseIDs(messages []client.Message) []string {
	var ids []string
	for _, m := range messages {
		if m.Role != "assistant" || !m.Content.HasBlocks() {
			continue
		}
		for _, b := range m.Content.Blocks() {
			if b.Type == "tool_use" && b.ID != "" && compactableTools[b.Name] {
				ids = append(ids, b.ID)
			}
		}
	}
	return ids
}

// timeBasedCompact applies time-based microcompaction in place when the
// trigger fires. Returns the number of tool_results cleared.
//
// The single-line semantics are: when the gap since the last assistant
// response exceeds the threshold (so the prompt cache has expired and the
// prefix will be rewritten anyway), replace older compactable tool_result
// contents with the marker string, keeping the most recent KeepRecent ones
// intact.
//
// Skips blocks whose content already equals the marker (idempotent — re-runs
// don't redirty bytes).
func timeBasedCompact(messages []client.Message, lastAssistantAt time.Time, cfg TimeBasedCompactConfig) int {
	if _, fire := evaluateTimeBasedCompactTrigger(lastAssistantAt, cfg); !fire {
		return 0
	}

	ids := collectCompactableToolUseIDs(messages)

	// Floor at 1: a tail-slice with KeepRecent <= 0 would clear every
	// result, leaving the model with zero working context. That's never the
	// right call even for an aggressive operator config — a misconfiguration
	// shouldn't nuke the session.
	keepRecent := max(cfg.KeepRecent, 1)
	if len(ids) <= keepRecent {
		return 0
	}

	keepFrom := len(ids) - keepRecent
	clearSet := make(map[string]bool, keepFrom)
	for _, id := range ids[:keepFrom] {
		clearSet[id] = true
	}

	cleared := 0
	for i, m := range messages {
		if m.Role != "user" || !m.Content.HasBlocks() {
			continue
		}
		blocks := m.Content.Blocks()
		touched := false
		newBlocks := make([]client.ContentBlock, len(blocks))
		for j, b := range blocks {
			if b.Type == "tool_result" && clearSet[b.ToolUseID] {
				if existing, _ := b.ToolContent.(string); existing != timeBasedClearedMarker {
					b.ToolContent = timeBasedClearedMarker
					touched = true
					cleared++
				}
			}
			newBlocks[j] = b
		}
		if touched {
			oldContent := messages[i].Content
			messages[i].Content = client.NewBlockContent(newBlocks)
			client.LogCacheCompactEvent("tbclear", i, oldContent, messages[i].Content)
		}
	}
	return cleared
}
