package tui

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
)

// toolKeyArg extracts the most meaningful argument from a tool's JSON args.
func toolKeyArg(toolName string, argsJSON string) string {
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(argsJSON), &m); err != nil {
		return truncate(argsJSON, 40)
	}

	var key string
	switch toolName {
	case "bash":
		key = strVal(m, "command")
	case "file_read", "file_write", "file_edit", "directory_list":
		key = strVal(m, "path")
	case "glob":
		key = strVal(m, "pattern")
	case "grep":
		key = strVal(m, "pattern")
		if path := strVal(m, "path"); path != "" {
			key += ", " + path
		}
	case "http", "web_fetch", "browser_navigate":
		key = strVal(m, "url")
	case "web_search":
		key = strVal(m, "query")
	case "screenshot":
		key = "screen"
	case "computer":
		key = strVal(m, "action")
	case "applescript":
		key = strVal(m, "script")
	case "notify":
		key = strVal(m, "message")
	case "publish_to_web":
		if purpose := strVal(m, "purpose"); purpose != "" {
			key = "purpose: " + purpose
		}
	default:
		for _, f := range []string{"query", "path", "url", "command", "name"} {
			if v := strVal(m, f); v != "" {
				key = v
				break
			}
		}
	}

	if key == "" {
		return truncate(argsJSON, 40)
	}
	return truncate(key, 50)
}

// toolResultBrief extracts a short detail from the result.
func toolResultBrief(toolName string, content string, elapsed time.Duration) string {
	var parts []string
	if elapsed > 100*time.Millisecond {
		parts = append(parts, fmt.Sprintf("%.1fs", elapsed.Seconds()))
	}
	switch {
	case strings.HasPrefix(content, "wrote "):
		parts = append(parts, strings.SplitN(content, " to ", 2)[0])
	case strings.HasPrefix(content, "exit ") && len(content) >= 6:
		parts = append(parts, content[:6])
	}
	return strings.Join(parts, "  ")
}

// formatCompactToolResult formats a single-line tool result.
func formatCompactToolResult(toolName string, args string, isError bool, content string, elapsed time.Duration) string {
	keyArg := toolKeyArg(toolName, args)
	dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("243"))
	successIcon := lipgloss.NewStyle().Foreground(lipgloss.Color("42")).Render("✓")
	errorIcon := lipgloss.NewStyle().Foreground(lipgloss.Color("196")).Render("✗")

	icon := successIcon
	brief := toolResultBrief(toolName, content, elapsed)
	if isError {
		icon = errorIcon
		brief = truncate(content, 60)
	}

	line := fmt.Sprintf("⏵ %s(%s)  %s", toolName, keyArg, icon)
	if brief != "" {
		line += "  " + brief
	}
	return dimStyle.Render(line)
}

// formatExpandedToolResult formats the full expanded tool result.
func formatExpandedToolResult(toolName string, args string, isError bool, content string, elapsed time.Duration) string {
	compact := formatCompactToolResult(toolName, args, isError, content, elapsed)
	dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("243"))

	// Flatten newlines so expanded view stays compact
	flat := strings.Join(strings.Fields(content), " ")

	var sb strings.Builder
	sb.WriteString(compact)
	sb.WriteString("\n")
	sb.WriteString(dimStyle.Render(fmt.Sprintf("  Args: %s", truncate(args, 200))))
	sb.WriteString("\n")
	if isError {
		sb.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color("196")).Render(fmt.Sprintf("  Error: %s", truncate(flat, 200))))
	} else {
		sb.WriteString(dimStyle.Render(fmt.Sprintf("  Result: %s", truncate(flat, 200))))
	}
	return sb.String()
}

// maxResponseDisplayLines is the max visible lines for LLM text responses.
const maxResponseDisplayLines = 40

// truncateLongResponse trims rendered text exceeding the line limit.
func truncateLongResponse(rendered string) string {
	lines := strings.Split(rendered, "\n")
	if len(lines) <= maxResponseDisplayLines {
		return rendered
	}
	kept := strings.Join(lines[:maxResponseDisplayLines], "\n")
	hidden := len(lines) - maxResponseDisplayLines
	dim := lipgloss.NewStyle().Foreground(lipgloss.Color("243"))
	notice := dim.Render(fmt.Sprintf("  ... (%d more lines — /copy for full text)", hidden))
	return kept + "\n" + notice
}

// formatToolSummary renders a single collapsed summary line for a set of tool results.
func formatToolSummary(results []toolResultEntry) string {
	total := len(results)
	if total == 0 {
		return ""
	}
	var errCount int
	for _, r := range results {
		if r.isError {
			errCount++
		}
	}
	dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("243"))
	successIcon := lipgloss.NewStyle().Foreground(lipgloss.Color("42")).Render("✓")
	errorIcon := lipgloss.NewStyle().Foreground(lipgloss.Color("196")).Render("✗")

	var line string
	if errCount == 0 {
		line = fmt.Sprintf("⏵ %d tools used  %s", total, successIcon)
	} else {
		okCount := total - errCount
		line = fmt.Sprintf("⏵ %d tools used  %s%d %s%d", total, successIcon, okCount, errorIcon, errCount)
	}
	return dimStyle.Render(line)
}

func strVal(m map[string]interface{}, key string) string {
	if v, ok := m[key]; ok {
		return fmt.Sprintf("%v", v)
	}
	return ""
}
