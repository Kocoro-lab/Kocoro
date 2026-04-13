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

	filename := fmt.Sprintf("tool_result_%s_%s.txt", sessionID, callID)
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

// cleanupSpills removes all spill files for a given session ID.
func cleanupSpills(shannonDir, sessionID string) {
	if shannonDir == "" {
		return
	}
	dir := filepath.Join(shannonDir, "tmp")
	pattern := filepath.Join(dir, fmt.Sprintf("tool_result_%s_*.txt", sessionID))
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
