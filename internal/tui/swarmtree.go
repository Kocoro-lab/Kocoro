package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
)

// swarmAgentEntry represents a single subagent tracked in the swarm view.
type swarmAgentEntry struct {
	id           string
	agentType    string
	description  string
	status       string        // "running" | "completed" | "error"
	activity     string        // current tool or status text (raw fallback)
	started      time.Time
	elapsed      time.Duration // populated on completion
	tokens       int
	toolUseCount int               // cumulative tool calls in this child
	tokenCount   int               // cumulative tokens (input+output)
	recentTools  []recentToolEntry // sliding window for activity summarization
	taskID       string               // links to task store ID for unified view
}

// formatDurationCompact formats a duration into a compact human-readable string.
// e.g. 92s → "1m32s", 45s → "45s", 3661s → "1h1m1s"
func formatDurationCompact(d time.Duration) string {
	d = d.Round(time.Second)
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	s := int(d.Seconds()) % 60
	if h > 0 {
		return fmt.Sprintf("%dh%dm%ds", h, m, s)
	}
	if m > 0 {
		return fmt.Sprintf("%dm%ds", m, s)
	}
	return fmt.Sprintf("%ds", s)
}

// renderSummaryLine produces a compact one-line summary of subagent status.
// e.g., "Running 3 explorer agents… (Tab to expand)"
func renderSummaryLine(agents []swarmAgentEntry, width int) string {
	if len(agents) == 0 {
		return ""
	}

	dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("243"))
	successStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("42"))

	// Count running vs completed
	var running int
	allSameType := true
	firstType := agents[0].agentType
	for _, ag := range agents {
		if ag.status == "running" {
			running++
		}
		if ag.agentType != firstType {
			allSameType = false
		}
	}

	total := len(agents)
	var text string

	if running == 0 {
		// All completed
		maxElapsed := agents[0].elapsed
		for _, ag := range agents[1:] {
			if ag.elapsed > maxElapsed {
				maxElapsed = ag.elapsed
			}
		}
		noun := "agents"
		if total == 1 {
			noun = "agent"
		}
		text = successStyle.Render(fmt.Sprintf("\u2713 %d %s completed (%s)", total, noun, formatDurationCompact(maxElapsed)))
	} else if total == 1 {
		text = fmt.Sprintf("Running %s agent\u2026", agents[0].agentType)
	} else if allSameType {
		text = fmt.Sprintf("Running %d %s agents\u2026", total, firstType)
	} else {
		text = fmt.Sprintf("Running %d agents\u2026", total)
	}

	hint := dimStyle.Render(" (Tab for details)")
	if running == 0 {
		hint = ""
	}

	return text + hint
}

// renderSwarmTree renders a per-agent tree with tool count, token count, and activity summary.
func renderSwarmTree(agents []swarmAgentEntry, width int) string {
	if len(agents) == 0 {
		return ""
	}

	nameStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("117")).Bold(true)
	dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("243"))
	successStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	errorStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	treeStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("243"))

	var sb strings.Builder

	for i, ag := range agents {
		var treeChar string
		if i == len(agents)-1 {
			treeChar = "└─"
		} else {
			treeChar = "├─"
		}

		name := "@" + ag.agentType

		// Activity / status text
		var activityPart string
		switch ag.status {
		case "completed":
			activityPart = successStyle.Render("✓ Done")
			if ag.elapsed > 0 {
				activityPart += dimStyle.Render(" " + formatDurationCompact(ag.elapsed))
			}
		case "error":
			activityPart = errorStyle.Render("✗ Error")
		default:
			activityPart = dimStyle.Render(summarizeActivity(ag.recentTools, ag.description))
			elapsed := time.Since(ag.started)
			if elapsed >= time.Second {
				activityPart += dimStyle.Render(fmt.Sprintf(" · %.0fs", elapsed.Seconds()))
			}
		}

		// Stats: "· N tools · Xk"
		statsText := ""
		if ag.toolUseCount > 0 {
			statsText = fmt.Sprintf(" · %d tools", ag.toolUseCount)
		}
		if ag.tokenCount > 0 {
			statsText += fmt.Sprintf(" · %s", formatTokenCount(ag.tokenCount))
		}

		// Build line with responsive layout.
		// Stats (tool count + token count) are only shown on wider terminals (>= 70).
		prefix := fmt.Sprintf("  %s %s: ", treeStyle.Render(treeChar), nameStyle.Render(name))
		prefixWidth := lipgloss.Width(prefix)

		statsWidth := lipgloss.Width(statsText)
		activityWidth := lipgloss.Width(activityPart)
		available := width - prefixWidth

		var line string
		wideEnough := width >= 70
		if wideEnough && available >= activityWidth+statsWidth {
			line = prefix + activityPart + dimStyle.Render(statsText)
		} else if available >= activityWidth {
			line = prefix + activityPart
		} else {
			maxLen := available
			if maxLen < 10 {
				maxLen = 10
			}
			line = prefix + dimStyle.Render(truncate(summarizeActivity(ag.recentTools, ag.description), maxLen))
		}

		sb.WriteString(line)
		if i < len(agents)-1 {
			sb.WriteString("\n")
		}
	}

	return sb.String()
}

// renderCompactTree renders a minimal per-agent progress display (always visible).
// Shows: tree char + @type: activity (no stats). Stats are in the full tree (Tab).
func renderCompactTree(agents []swarmAgentEntry, width int) string {
	if len(agents) == 0 {
		return ""
	}

	nameStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("117")).Bold(true)
	dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("243"))
	successStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	errorStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	treeStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("243"))

	var sb strings.Builder

	for i, ag := range agents {
		var treeChar string
		if i == len(agents)-1 {
			treeChar = "└─"
		} else {
			treeChar = "├─"
		}

		name := "@" + ag.agentType

		var activityPart string
		switch ag.status {
		case "completed":
			activityPart = successStyle.Render("✓ Done")
		case "error":
			activityPart = errorStyle.Render("✗ Error")
		default:
			activityPart = dimStyle.Render(summarizeActivity(ag.recentTools, ag.description))
		}

		prefix := fmt.Sprintf("  %s %s: ", treeStyle.Render(treeChar), nameStyle.Render(name))
		line := prefix + activityPart

		// Truncate if exceeds width
		if lipgloss.Width(line) > width {
			prefixWidth := lipgloss.Width(prefix)
			maxActivity := width - prefixWidth
			if maxActivity < 10 {
				maxActivity = 10
			}
			line = prefix + dimStyle.Render(truncate(summarizeActivity(ag.recentTools, ag.description), maxActivity))
		}

		sb.WriteString(line)
		if i < len(agents)-1 {
			sb.WriteString("\n")
		}
	}

	return sb.String()
}

// formatTokenCount formats a token count as a human-readable string.
// Values under 1000 are shown as-is; 1000–9999 as "1.0k"–"9.9k"; 10000+ as "10k"–etc.
func formatTokenCount(tokens int) string {
	if tokens < 1000 {
		return fmt.Sprintf("%d", tokens)
	}
	k := float64(tokens) / 1000.0
	if k < 10 {
		return fmt.Sprintf("%.1fk", k)
	}
	return fmt.Sprintf("%.0fk", k)
}
