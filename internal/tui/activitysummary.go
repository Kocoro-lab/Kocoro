package tui

import "fmt"

// recentToolEntry records a single child tool invocation for activity summarization.
type recentToolEntry struct {
	name     string
	keyArg   string
	isRead   bool
	isSearch bool
}

// classifyTool returns (isRead, isSearch) for a given tool name.
func classifyTool(name string) (isRead, isSearch bool) {
	switch name {
	case "file_read", "directory_list":
		return true, false
	case "grep", "glob", "web_search":
		return false, true
	default:
		return false, false
	}
}

// summarizeActivity produces a human-readable activity string from recent tool calls.
// If tools is empty, returns fallbackDesc.
// Collapses 2+ consecutive trailing search/read operations into a summary.
func summarizeActivity(tools []recentToolEntry, fallbackDesc string) string {
	if len(tools) == 0 {
		return fallbackDesc
	}

	// Count consecutive search/read from the tail
	var searchCount, readCount int
	for i := len(tools) - 1; i >= 0; i-- {
		if tools[i].isSearch {
			searchCount++
		} else if tools[i].isRead {
			readCount++
		} else {
			break
		}
	}

	total := searchCount + readCount
	if total >= 2 {
		return formatSearchReadSummary(searchCount, readCount)
	}

	// Single tool: show name(keyArg)
	last := tools[len(tools)-1]
	return fmt.Sprintf("%s(%s)", last.name, last.keyArg)
}

// formatSearchReadSummary builds "Searching for N patterns, reading M files…" style text.
func formatSearchReadSummary(searchCount, readCount int) string {
	var parts []string
	if searchCount > 0 {
		noun := "pattern"
		if searchCount != 1 {
			noun = "patterns"
		}
		parts = append(parts, fmt.Sprintf("Searching for %d %s", searchCount, noun))
	}
	if readCount > 0 {
		noun := "file"
		if readCount != 1 {
			noun = "files"
		}
		verb := "reading"
		if searchCount == 0 {
			verb = "Reading"
		}
		parts = append(parts, fmt.Sprintf("%s %d %s", verb, readCount, noun))
	}

	result := ""
	for i, p := range parts {
		if i > 0 {
			result += ", "
		}
		result += p
	}
	return result + "\u2026"
}
