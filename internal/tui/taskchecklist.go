package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

func renderChecklist(tasks []checklistTask, width int) string {
	if len(tasks) == 0 {
		return ""
	}

	dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("243"))
	successStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	activeStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("76"))

	var sb strings.Builder

	// Header
	headerWidth := width - 12
	if headerWidth < 10 {
		headerWidth = 10
	}
	sb.WriteString(dimStyle.Render(fmt.Sprintf("  Tasks %s", strings.Repeat("─", headerWidth))))
	sb.WriteString("\n")

	for _, task := range tasks {
		var icon string
		var style lipgloss.Style

		switch task.status {
		case "completed":
			icon = successStyle.Render("✔")
			style = dimStyle
		case "in_progress":
			icon = activeStyle.Render("▪")
			style = lipgloss.NewStyle()
		default: // pending
			icon = dimStyle.Render("▫")
			style = dimStyle
		}

		subject := task.subject
		maxSubject := width - 8
		if r := []rune(subject); len(r) > maxSubject {
			subject = string(r[:maxSubject-3]) + "..."
		}

		sb.WriteString(fmt.Sprintf("  %s %s\n", icon, style.Render(subject)))
	}

	return sb.String()
}
