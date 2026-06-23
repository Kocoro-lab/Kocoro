package tui

import (
	"fmt"
	"math/rand"
	"regexp"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/Kocoro-lab/ShanClaw/internal/session"
)

// Color palette for the startup header. Routed through the shared adaptive
// palette (theme.go) so the header stays readable on light terminals too.
var (
	borderColor = colorAccent // frog green — box border
	accentColor = colorAccent // frog green — section headers
	dimColor    = colorDim    // medium gray — secondary text
	infoColor   = colorInfo   // blue — activity header
)

const (
	headerTotalFrames = 12 // startup: crouch→jump→land→blink (~1s total)
	headerTickMs      = 80 // ms per frame
	headerLeftWidth   = 16 // left column visual width
)

// Tips shown in the info section of the startup header.
var headerTips = []string{
	"Try /research for deep analysis",
	"Use /sessions to resume work",
	"Type /help to see all commands",
	"Use /model to switch model tier",
	"Try /swarm for multi-agent tasks",
}

// headerFrameTick returns a tea.Cmd that sends a headerTickMsg after the tick interval.
func headerFrameTick() tea.Cmd {
	return tea.Tick(time.Duration(headerTickMs)*time.Millisecond, func(time.Time) tea.Msg {
		return headerTickMsg{}
	})
}

// renderStartupHeader builds the animated two-column startup header for the given frame.
// tipIdx and cwd should be pre-computed by the caller (no I/O inside this function).
func renderStartupHeader(frame int, width int, version string, modelTier string, endpoint string, cwd string, sessions []session.SessionSummary, tipIdx int) string {
	if width < 50 {
		width = 50
	}
	if width > 100 {
		width = 100
	}

	innerWidth := width - 2                        // inside box borders (│ on each side)
	rightWidth := innerWidth - headerLeftWidth - 1 // -1 for middle divider

	// --- Build left column lines: the Kocoro brand swirl (draws in on startup). ---
	// renderKocoroGrid returns 8 lines, each exactly headerLeftWidth (16) cols.
	leftLines := renderKocoroGrid(frame)

	// --- Build right column lines (all immediate) ---
	var rightLines []string
	dimStyle := lipgloss.NewStyle().Foreground(dimColor)
	modelStyle := lipgloss.NewStyle().Foreground(accentColor).Bold(true)

	// Tips.
	tipHeader := lipgloss.NewStyle().Foreground(accentColor).Bold(true).Render("Tips")
	rightLines = append(rightLines, " "+tipHeader)
	rightLines = append(rightLines, " "+dimStyle.Render(truncateStr(headerTips[tipIdx%len(headerTips)], rightWidth-3)))

	// Divider.
	rightLines = append(rightLines, " "+dimStyle.Render(strings.Repeat("─", rightWidth-2)))

	// Recent activity.
	actHeader := lipgloss.NewStyle().Foreground(infoColor).Bold(true).Render("Recent activity")
	rightLines = append(rightLines, " "+actHeader)

	if len(sessions) == 0 {
		rightLines = append(rightLines, " "+dimStyle.Render("No recent sessions"))
		rightLines = append(rightLines, "")
	} else {
		s := sessions[0]
		titleStyle := lipgloss.NewStyle().Foreground(colorSecondary)
		rightLines = append(rightLines, " "+titleStyle.Render(truncateStr(s.Title, rightWidth-4)))
		rightLines = append(rightLines, " "+dimStyle.Render(fmt.Sprintf("%s, %d msgs", timeAgo(s.UpdatedAt), s.MsgCount)))
	}

	// Model / version / endpoint / cwd — moved here from the (now icon-filled)
	// left column. Two lines pad the right column to the icon's 8-line height.
	rightLines = append(rightLines, " "+dimStyle.Render(truncateStr(cwd, rightWidth-3)))
	info := modelStyle.Render(modelTier) + dimStyle.Render(" · v"+version)
	if budget := rightWidth - lipgloss.Width(info) - 5; budget >= 6 {
		info += dimStyle.Render(" · " + truncateStr(endpoint, budget))
	}
	rightLines = append(rightLines, " "+info)

	// Equalize line counts between columns.
	for len(leftLines) < len(rightLines) {
		leftLines = append(leftLines, "")
	}
	for len(rightLines) < len(leftLines) {
		rightLines = append(rightLines, "")
	}

	// --- Assemble box ---
	bdr := lipgloss.NewStyle().Foreground(borderColor)

	var sb strings.Builder

	// Top border with title.
	titlePart := "─ Kocoro CLI "
	titleVisWidth := lipgloss.Width(titlePart)
	remaining := innerWidth - titleVisWidth
	if remaining < 0 {
		remaining = 0
	}
	sb.WriteString(bdr.Render("╭"+titlePart+strings.Repeat("─", remaining)+"╮") + "\n")

	// Content rows: │ left │ right │
	divider := bdr.Render("│")
	for i := range leftLines {
		left := padToWidth(leftLines[i], headerLeftWidth)
		right := padToWidth(rightLines[i], rightWidth)
		sb.WriteString(bdr.Render("│") + left + divider + right + bdr.Render("│") + "\n")
	}

	// Bottom border.
	sb.WriteString(bdr.Render("╰" + strings.Repeat("─", innerWidth) + "╯"))

	return sb.String()
}

// padToWidth pads a (possibly ANSI-styled) string so its visible width
// reaches targetWidth. Uses lipgloss.Width which correctly handles
// ANSI escape codes and double-width CJK characters.
func padToWidth(styled string, targetWidth int) string {
	visible := lipgloss.Width(styled)
	if visible >= targetWidth {
		return styled
	}
	return styled + strings.Repeat(" ", targetWidth-visible)
}

// ansiRe matches ANSI escape sequences.
var ansiRe = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// stripAnsi removes ANSI escape codes from a string for width calculation.
func stripAnsi(s string) string {
	return ansiRe.ReplaceAllString(s, "")
}

// truncateStr truncates a string with "..." if its DISPLAY WIDTH exceeds
// maxLen cells (CJK/wide runes count as 2). The "..." counts toward the budget.
func truncateStr(s string, maxLen int) string {
	if maxLen <= 3 {
		maxLen = 4
	}
	return truncateCells(s, maxLen, "...")
}

// timeAgo returns a human-readable relative time string.
func timeAgo(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		mins := int(d.Minutes())
		if mins == 1 {
			return "1 min ago"
		}
		return fmt.Sprintf("%d min ago", mins)
	case d < 24*time.Hour:
		hours := int(d.Hours())
		if hours == 1 {
			return "1h ago"
		}
		return fmt.Sprintf("%dh ago", hours)
	case d < 48*time.Hour:
		return "yesterday"
	default:
		days := int(d.Hours() / 24)
		return fmt.Sprintf("%d days ago", days)
	}
}

// pickTipIdx returns a stable random tip index for a session.
func pickTipIdx() int {
	return rand.Intn(len(headerTips))
}
