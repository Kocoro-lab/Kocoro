package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
)

const maxLiveTools = 6

// renderToolProgress renders a live list of tool calls with status indicators.
// stalled=true turns the running indicator red.
func renderToolProgress(tools []liveToolEntry, width int, stalled bool) string {
	if len(tools) == 0 {
		return ""
	}

	dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("243"))
	successIcon := lipgloss.NewStyle().Foreground(lipgloss.Color("42")).Render("✓")
	errorIcon := lipgloss.NewStyle().Foreground(lipgloss.Color("196")).Render("✗")
	runningIcon := lipgloss.NewStyle().Foreground(lipgloss.Color("76")).Render("▸")
	stalledIcon := lipgloss.NewStyle().Foreground(lipgloss.Color("196")).Render("▸")

	var sb strings.Builder

	// Show last maxLiveTools entries; if more, show a count line first
	start := 0
	if len(tools) > maxLiveTools {
		hidden := len(tools) - maxLiveTools
		sb.WriteString(dimStyle.Render(fmt.Sprintf("  … +%d earlier tools", hidden)))
		sb.WriteString("\n")
		start = hidden
	}

	for i := start; i < len(tools); i++ {
		t := tools[i]
		var icon string
		var timing string

		if t.done {
			if t.isError {
				icon = errorIcon
			} else {
				icon = successIcon
			}
			if t.elapsed >= 100*time.Millisecond {
				timing = fmt.Sprintf("%.1fs", t.elapsed.Seconds())
			}
		} else {
			if stalled {
				icon = stalledIcon
			} else {
				icon = runningIcon
			}
			elapsed := time.Since(t.started)
			if elapsed >= time.Second {
				timing = fmt.Sprintf("%.0fs…", elapsed.Seconds())
			} else {
				timing = "…"
			}
		}

		toolDesc := fmt.Sprintf("%s(%s)", t.name, t.keyArg)
		// Truncate tool description to fit width
		maxDesc := width - 12 // icon(2) + padding(4) + timing(6)
		if maxDesc < 20 {
			maxDesc = 20
		}
		if r := []rune(toolDesc); len(r) > maxDesc {
			toolDesc = string(r[:maxDesc-3]) + "..."
		}

		line := fmt.Sprintf("  %s %s", icon, dimStyle.Render(toolDesc))
		if timing != "" {
			// Right-align timing
			lineWidth := lipgloss.Width(line)
			gap := width - lineWidth - lipgloss.Width(timing) - 2
			if gap < 1 {
				gap = 1
			}
			line += strings.Repeat(" ", gap) + dimStyle.Render(timing)
		}

		sb.WriteString(line)
		if i < len(tools)-1 {
			sb.WriteString("\n")
		}
	}

	return sb.String()
}
