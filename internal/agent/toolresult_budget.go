package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

// ToolResultReplacementState stores stable replacement text by tool_use_id.
// It is session-scoped: once a tool_result is replaced, later turns replay the
// exact same bytes so prompt-cache prefixes do not drift.
type ToolResultReplacementState struct {
	Seen         map[string]bool
	Replacements map[string]string
}

func NewToolResultReplacementState(replacements map[string]string) *ToolResultReplacementState {
	out := &ToolResultReplacementState{
		Seen:         make(map[string]bool),
		Replacements: make(map[string]string),
	}
	for k, v := range replacements {
		if strings.TrimSpace(k) != "" && v != "" {
			out.Seen[k] = true
			out.Replacements[k] = v
		}
	}
	return out
}

func (s *ToolResultReplacementState) Snapshot() map[string]string {
	if s == nil || len(s.Replacements) == 0 {
		return nil
	}
	out := make(map[string]string, len(s.Replacements))
	for k, v := range s.Replacements {
		out[k] = v
	}
	return out
}

type ToolResultBudgetOptions struct {
	ShannonDir             string
	SessionID              string
	AggregateCapChars      int
	MinCandidateChars      int
	PreviewChars           int
	ToolMaxResultSizeChars map[string]int
}

type budgetCandidate struct {
	msgIdx    int
	blockIdx  int
	toolUseID string
	toolName  string
	content   string
	length    int
}

func defaultToolResultBudgetOptions(shannonDir, sessionID string) ToolResultBudgetOptions {
	return ToolResultBudgetOptions{
		ShannonDir:        shannonDir,
		SessionID:         sessionID,
		AggregateCapChars: aggregateCapThreshold,
		MinCandidateChars: minAggregateSpillSize,
		PreviewChars:      spillPreviewChars,
	}
}

func applyToolResultBudget(messages []client.Message, state *ToolResultReplacementState, opts ToolResultBudgetOptions) ([]client.Message, bool) {
	if state == nil {
		state = NewToolResultReplacementState(nil)
	}
	if opts.AggregateCapChars <= 0 {
		opts.AggregateCapChars = aggregateCapThreshold
	}
	if opts.MinCandidateChars <= 0 {
		opts.MinCandidateChars = minAggregateSpillSize
	}
	if opts.PreviewChars <= 0 {
		opts.PreviewChars = spillPreviewChars
	}

	toolNames := toolUseNameByID(messages)
	out := messages
	changed := false

	for i, msg := range messages {
		if msg.Role != "user" || !msg.Content.HasBlocks() {
			continue
		}
		blocks := msg.Content.Blocks()
		total := 0
		candidates := make([]budgetCandidate, 0, len(blocks))

		for j, block := range blocks {
			if block.Type != "tool_result" || block.ToolUseID == "" {
				continue
			}
			if replacement := state.Replacements[block.ToolUseID]; replacement != "" {
				if content, ok := block.ToolContent.(string); ok {
					_ = rehydrateStoredToolResultReplacement(opts, block.ToolUseID, content)
				}
				if client.ToolResultText(block) != replacement {
					out = cloneMessagesForBudget(out, messages)
					newBlocks := cloneBlocks(out[i].Content.Blocks())
					newBlocks[j].ToolContent = replacement
					out[i].Content = client.NewBlockContent(newBlocks)
					changed = true
				}
				total += utf8.RuneCountInString(replacement)
				continue
			}

			content, ok := block.ToolContent.(string)
			if !ok {
				continue
			}
			toolName := toolNames[block.ToolUseID]
			total += utf8.RuneCountInString(content)
			if state.Seen[block.ToolUseID] {
				continue
			}
			state.Seen[block.ToolUseID] = true
			if !toolResultBudgetEligible(toolName, opts) {
				continue
			}
			n := utf8.RuneCountInString(content)
			minSize := opts.MinCandidateChars
			if maxChars := resolveToolResultMax(toolName, opts); maxChars > 0 && maxChars < minSize {
				minSize = maxChars
			}
			if n >= minSize {
				candidates = append(candidates, budgetCandidate{
					msgIdx:    i,
					blockIdx:  j,
					toolUseID: block.ToolUseID,
					toolName:  toolName,
					content:   content,
					length:    n,
				})
			}
		}

		sort.SliceStable(candidates, func(a, b int) bool {
			return candidates[a].length > candidates[b].length
		})
		selectedIDs := make(map[string]bool)
		for total > opts.AggregateCapChars && len(candidates) > 0 {
			c := candidates[0]
			candidates = candidates[1:]
			selectedIDs[c.toolUseID] = true
			replacement, err := buildToolResultReplacement(opts, c.toolUseID, c.content)
			if err != nil {
				continue
			}
			state.Replacements[c.toolUseID] = replacement
			out = cloneMessagesForBudget(out, messages)
			newBlocks := cloneBlocks(out[c.msgIdx].Content.Blocks())
			newBlocks[c.blockIdx].ToolContent = replacement
			oldContent := out[c.msgIdx].Content
			out[c.msgIdx].Content = client.NewBlockContent(newBlocks)
			client.LogCacheCompactEvent("budget", c.msgIdx, oldContent, out[c.msgIdx].Content)
			total = total - c.length + utf8.RuneCountInString(replacement)
			changed = true
		}
		for _, c := range candidates {
			if !selectedIDs[c.toolUseID] {
				state.Seen[c.toolUseID] = true
			}
		}
	}

	return out, changed
}

func toolUseNameByID(messages []client.Message) map[string]string {
	out := make(map[string]string)
	for _, msg := range messages {
		if msg.Role != "assistant" || !msg.Content.HasBlocks() {
			continue
		}
		for _, block := range msg.Content.Blocks() {
			if block.Type == "tool_use" && block.ID != "" {
				out[block.ID] = block.Name
			}
		}
	}
	return out
}

func resolveToolResultMax(toolName string, opts ToolResultBudgetOptions) int {
	if opts.ToolMaxResultSizeChars != nil {
		if n, ok := opts.ToolMaxResultSizeChars[toolName]; ok {
			if n == UnlimitedToolResultSizeChars {
				return UnlimitedToolResultSizeChars
			}
			if n > 0 {
				return n
			}
		}
	}
	return DefaultMaxToolResultSizeChars
}

func toolResultBudgetEligible(toolName string, opts ToolResultBudgetOptions) bool {
	switch toolName {
	case "cloud_delegate", "think", "computer", "screenshot":
		return false
	}
	if strings.HasPrefix(toolName, "browser_") {
		return false
	}
	return resolveToolResultMax(toolName, opts) != UnlimitedToolResultSizeChars
}

func cloneMessagesForBudget(current, original []client.Message) []client.Message {
	if len(current) > 0 && len(original) > 0 && &current[0] != &original[0] {
		return current
	}
	out := make([]client.Message, len(original))
	copy(out, original)
	return out
}

func cloneBlocks(blocks []client.ContentBlock) []client.ContentBlock {
	out := make([]client.ContentBlock, len(blocks))
	copy(out, blocks)
	return out
}

var unsafeToolResultFilenameChars = regexp.MustCompile(`[^a-zA-Z0-9_.-]+`)

func safeToolResultBudgetID(toolUseID string) string {
	toolUseID = strings.TrimSpace(toolUseID)
	if toolUseID == "" {
		return "unknown"
	}
	safe := unsafeToolResultFilenameChars.ReplaceAllString(toolUseID, "_")
	if len(safe) > 120 {
		safe = safe[:120]
	}
	return safe
}

// safeSpillSessionID strips any character that could escape the spill
// directory when concatenated into a filename. Session IDs come from
// generateID() (`YYYY-MM-DD-<hex>`), but Manager.Resume accepts
// caller-supplied IDs — defense-in-depth keeps a malicious `id="../../etc"`
// from traversing out of `~/.shannon/tmp`.
func safeSpillSessionID(sessionID string) string {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return "unknown"
	}
	safe := unsafeToolResultFilenameChars.ReplaceAllString(sessionID, "_")
	if len(safe) > 120 {
		safe = safe[:120]
	}
	return safe
}

func toolResultReplacementPath(opts ToolResultBudgetOptions, toolUseID string) (string, bool) {
	if opts.ShannonDir == "" || opts.SessionID == "" {
		return "", false
	}
	dir := filepath.Join(opts.ShannonDir, "tmp")
	return filepath.Join(dir, fmt.Sprintf("tool_result_%s_%s.txt", safeSpillSessionID(opts.SessionID), safeToolResultBudgetID(toolUseID))), true
}

func rehydrateStoredToolResultReplacement(opts ToolResultBudgetOptions, toolUseID, content string) error {
	path, ok := toolResultReplacementPath(opts, toolUseID)
	if !ok {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("tool result budget mkdir: %w", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return fmt.Errorf("tool result budget write: %w", err)
	}
	return nil
}

func buildToolResultReplacement(opts ToolResultBudgetOptions, toolUseID, content string) (string, error) {
	path := ""
	if replacementPath, ok := toolResultReplacementPath(opts, toolUseID); ok {
		path = replacementPath
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return "", fmt.Errorf("tool result budget mkdir: %w", err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			return "", fmt.Errorf("tool result budget write: %w", err)
		}
	}

	runes := []rune(content)
	preview := runes
	if len(preview) > opts.PreviewChars {
		preview = preview[:opts.PreviewChars]
	}
	location := "not written to disk"
	if path != "" {
		location = path
	}
	return fmt.Sprintf("[Tool result omitted from context: %s (%d chars)]\n\nPreview (first %d chars):\n%s",
		location, len(runes), len(preview), string(preview)), nil
}
