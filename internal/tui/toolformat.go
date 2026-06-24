package tui

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
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
	case "use_skill":
		key = strVal(m, "skill_name")
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

// toolFriendlyLabels maps raw tool names to plain-language labels for the tool
// status lines, so a non-technical user sees "Searching the web" rather than
// "web_search(...)". Unknown tools keep their raw call form (friendlyToolLabel
// returns the name unchanged).
var toolFriendlyLabels = map[string]string{
	"web_search":      "Searching the web",
	"web_fetch":       "Reading a web page",
	"bash":            "Running a command",
	"use_skill":       "Using a skill",
	"file_read":       "Reading a file",
	"file_write":      "Writing a file",
	"file_edit":       "Editing a file",
	"glob":            "Finding files",
	"grep":            "Searching in files",
	"directory_list":  "Listing files",
	"http":            "Calling a service",
	"screenshot":      "Taking a screenshot",
	"browser":         "Browsing",
	"memory_append":   "Saving to memory",
	"memory_recall":   "Recalling memory",
	"schedule_create": "Scheduling a task",
	"cloud_delegate":  "Delegating to the cloud",
	"generate_image":  "Generating an image",
	"edit_image":      "Editing an image",
}

// friendlyToolLabel returns a plain-language label for a tool, or the raw name
// for tools not in the map.
func friendlyToolLabel(name string) string {
	if label, ok := toolFriendlyLabels[name]; ok {
		return label
	}
	return name
}

// formatToolCallLabel renders "Searching the web: market" for a known tool, or
// the raw "name(args)" form for unknown tools. Shared by the live pending line
// and the completed-result line so both read the same.
func formatToolCallLabel(name, keyArg string) string {
	label := friendlyToolLabel(name)
	switch {
	case label != name && keyArg != "":
		return label + ": " + keyArg
	case label != name:
		return label
	default:
		return fmt.Sprintf("%s(%s)", name, keyArg)
	}
}

// formatCompactToolResult formats a single-line tool result.
func formatCompactToolResult(toolName string, args string, isError bool, content string, elapsed time.Duration) string {
	keyArg := toolKeyArg(toolName, args)
	dimStyle := styleDim()
	successIcon := styleSuccess().Render("✓")
	errorIcon := styleError().Render("✗")

	icon := successIcon
	brief := toolResultBrief(toolName, content, elapsed)
	if isError {
		icon = errorIcon
		brief = truncate(content, 60)
	}

	line := fmt.Sprintf("⏵ %s  %s", formatToolCallLabel(toolName, keyArg), icon)
	if brief != "" {
		line += "  " + brief
	}
	return dimStyle.Render(line)
}

// expandedHeadLines / expandedTailLines bound how much multi-line tool output
// the Ctrl+O expanded view shows. Workload: a `bash`/`grep`/`file_read` result
// with a long stack trace or many matches. Symptom when it binds: the middle is
// elided with a "… +N lines" marker. Override: there is none today — bump these
// consts if power users complain the head/tail window is too tight.
const (
	expandedHeadLines = 8
	expandedTailLines = 4
)

// truncateHeadTail keeps the first head and last tail lines of content,
// replacing the elided middle with a "… +N lines" marker. Unlike the old
// strings.Fields flattening, it PRESERVES line structure — a stack trace, diff,
// or grep result stays readable instead of collapsing into one run-on line.
func truncateHeadTail(content string, head, tail int) string {
	content = strings.TrimRight(content, "\n")
	if content == "" {
		return ""
	}
	lines := strings.Split(content, "\n")
	if len(lines) <= head+tail {
		return strings.Join(lines, "\n")
	}
	hidden := len(lines) - head - tail
	out := make([]string, 0, head+tail+1)
	out = append(out, lines[:head]...)
	out = append(out, fmt.Sprintf("… +%d lines", hidden))
	out = append(out, lines[len(lines)-tail:]...)
	return strings.Join(out, "\n")
}

// formatExpandedToolResult formats the full expanded tool result. Multi-line
// output is preserved (head/tail windowed), each line indented under the
// compact header.
func formatExpandedToolResult(toolName string, args string, isError bool, content string, elapsed time.Duration) string {
	compact := formatCompactToolResult(toolName, args, isError, content, elapsed)
	dimStyle := styleDim()
	bodyStyle := dimStyle
	if isError {
		bodyStyle = styleError()
	}

	var sb strings.Builder
	sb.WriteString(compact)
	sb.WriteString("\n")
	sb.WriteString(dimStyle.Render("  Args: " + truncate(args, 200)))

	body := truncateHeadTail(content, expandedHeadLines, expandedTailLines)
	if body != "" {
		label := "  Result:"
		if isError {
			label = "  Error:"
		}
		sb.WriteString("\n")
		sb.WriteString(dimStyle.Render(label))
		for _, ln := range strings.Split(body, "\n") {
			sb.WriteString("\n")
			sb.WriteString(bodyStyle.Render("  " + ln))
		}
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
	dim := styleDim()
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
	dimStyle := styleDim()
	successIcon := styleSuccess().Render("✓")
	errorIcon := styleError().Render("✗")

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
