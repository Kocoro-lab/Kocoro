package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

const (
	// spillThreshold is the char count above which a tool result is written
	// to disk and replaced with a short preview in conversation context.
	spillThreshold = 50000
	// spillPreviewChars is the number of leading runes kept as an in-context preview.
	spillPreviewChars = 2000
	// aggregateCapThreshold is the per-turn cap on the SUM of all tool result
	// content lengths. Even when no single result triggers spillThreshold,
	// 10 parallel tools returning 30K each puts 300K into one user message —
	// uncapped cache_creation pressure. Mirrors CC's
	// MAX_TOOL_RESULTS_PER_MESSAGE_CHARS at
	// claude-code-source/src/constants/toolLimits.ts:49.
	aggregateCapThreshold = 200_000
	// minAggregateSpillSize: don't spill anything smaller than this when
	// reducing the aggregate — at small sizes the spill preview header
	// (~150B) is a meaningful fraction of the savings.
	minAggregateSpillSize = 5_000
)

// spillToDisk writes content to a temp file under ~/.shannon/tmp/ and returns
// a short preview string for in-context use. The caller should use the preview
// as the tool result instead of the full content.
//
// shannonDir must be an absolute path. An empty shannonDir is rejected because
// filepath.Join("", "tmp") yields the relative path "tmp", which would cause
// spill files to land in whatever cwd the process happens to be in (e.g. the
// repo root during tests).
func spillToDisk(shannonDir, sessionID, callID, content string) (preview string, err error) {
	if shannonDir == "" {
		return "", fmt.Errorf("spill: shannonDir is empty; refusing to write to process cwd")
	}
	dir := filepath.Join(shannonDir, "tmp")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("spill mkdir: %w", err)
	}

	filename := fmt.Sprintf("tool_result_%s_%s.txt", safeSpillSessionID(sessionID), callID)
	path := filepath.Join(dir, filename)

	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return "", fmt.Errorf("spill write: %w", err)
	}

	runes := []rune(content)
	previewRunes := runes
	if len(previewRunes) > spillPreviewChars {
		previewRunes = previewRunes[:spillPreviewChars]
	}

	preview = fmt.Sprintf("[Output saved to disk: %s (%s chars)]\n\nPreview (first %d chars):\n%s",
		path, strconv.Itoa(len(runes)), len(previewRunes), string(previewRunes))
	return preview, nil
}

// applyAggregateCap enforces the per-turn aggregate char-count cap on a
// batch's tool results. When the SUM of all execResults[*].result.Content
// lengths exceeds aggregateCapThreshold, the largest spill-eligible result
// is written to disk and replaced with a short preview, repeating until the
// total is under the cap or nothing remains worth spilling.
//
// The per-result spillThreshold (50K) and this aggregate cap (200K) are
// independent: a single 60K result is already spilled by the per-result
// path, but 10×30K results — each below 50K — are only caught here.
//
// Mutates execResults in place. Safe for any execResults length; no-op for
// empty/single-element batches. spillToDisk failures are silently ignored
// so a transient disk error doesn't stall the agent loop — worst case is
// the message goes through uncapped.
func applyAggregateCap(execResults []toolExecResult, shannonDir, sessionID string) {
	if len(execResults) < 2 {
		return
	}
	total := 0
	for i := range execResults {
		total += len(execResults[i].result.Content)
	}
	if total <= aggregateCapThreshold {
		return
	}
	for total > aggregateCapThreshold {
		maxIdx := -1
		maxLen := 0
		for i := range execResults {
			n := len(execResults[i].result.Content)
			if n >= minAggregateSpillSize && n > maxLen {
				maxIdx = i
				maxLen = n
			}
		}
		if maxIdx == -1 {
			return // nothing left worth spilling
		}
		original := execResults[maxIdx].result.Content
		spilled, err := spillToDisk(shannonDir, sessionID, generateCallID(), original)
		if err != nil {
			return
		}
		execResults[maxIdx].result.Content = spilled
		total = total - len(original) + len(spilled)
	}
}

func applyPerResultSpill(content, toolName, shannonDir, sessionID string, policy map[string]int) string {
	threshold := perToolResultSpillThreshold(toolName, policy)
	if threshold <= 0 || len([]rune(content)) <= threshold {
		return content
	}
	spilled, err := spillToDisk(shannonDir, sessionID, generateCallID(), content)
	if err != nil {
		return content
	}
	return spilled
}

func perToolResultSpillThreshold(toolName string, policy map[string]int) int {
	maxChars := resolveToolResultMax(toolName, ToolResultBudgetOptions{ToolMaxResultSizeChars: policy})
	if maxChars == UnlimitedToolResultSizeChars {
		return spillThreshold
	}
	if maxChars > 0 && maxChars < spillThreshold {
		return maxChars
	}
	return spillThreshold
}

func contextResultMaxChars(toolName string, cloudResult bool, defaultMax int, policy map[string]int) int {
	if cloudResult {
		return 60000
	}
	if defaultMax <= 0 {
		defaultMax = 30000
	}
	maxChars := resolveToolResultMax(toolName, ToolResultBudgetOptions{ToolMaxResultSizeChars: policy})
	if maxChars == UnlimitedToolResultSizeChars {
		return defaultMax
	}
	if maxChars > 0 && maxChars < defaultMax {
		return maxChars
	}
	return defaultMax
}

// cleanupSpills removes all spill files for a given session ID.
func cleanupSpills(shannonDir, sessionID string) {
	if shannonDir == "" {
		return
	}
	dir := filepath.Join(shannonDir, "tmp")
	pattern := filepath.Join(dir, fmt.Sprintf("tool_result_%s_*.txt", safeSpillSessionID(sessionID)))
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return
	}
	for _, m := range matches {
		os.Remove(m)
	}
	// Remove tmp dir if empty (best-effort).
	os.Remove(dir)
}
