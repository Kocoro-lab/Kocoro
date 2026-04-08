package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/Kocoro-lab/ShanClaw/internal/tasks"
)

// clearTaskStore removes all task JSON files from the store directory.
// Called at the start of each new agent run to prevent stale tasks from showing.
func clearTaskStore(dir string) {
	if dir == "" {
		return
	}
	store := tasks.NewStore(dir)
	_ = store.Clear()
}

// renderTaskList produces a unified task-centric view.
// When agents are provided, correlates by taskID and shows agent activity under matching tasks.
// Returns "" when there are no tasks or dir is empty.
func renderTaskList(dir string, agents []swarmAgentEntry, width int) string {
	if dir == "" {
		return ""
	}
	store := tasks.NewStore(dir)
	all, err := store.List()
	if err != nil || len(all) == 0 {
		return ""
	}

	// Build agent lookup: taskID → agent entry
	agentByTask := make(map[string]*swarmAgentEntry)
	for i := range agents {
		if agents[i].taskID != "" {
			agentByTask[agents[i].taskID] = &agents[i]
		}
	}

	dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("243"))
	successStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	activeStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("76"))

	var completed, inProgress int
	for _, t := range all {
		switch t.Status {
		case tasks.StatusCompleted:
			completed++
		case tasks.StatusInProgress:
			inProgress++
		}
	}

	var sb strings.Builder

	// Header
	header := fmt.Sprintf("  Tasks %d/%d", completed, len(all))
	if inProgress > 0 {
		header += fmt.Sprintf(" (%d active)", inProgress)
	}
	lineWidth := width - len([]rune(header)) - 2
	if lineWidth < 4 {
		lineWidth = 4
	}
	sb.WriteString(dimStyle.Render(header+" "+strings.Repeat("─", lineWidth)) + "\n")

	// Render all tasks (up to maxShow), non-completed first then completed
	const maxShow = 8
	shown := 0

	// Non-completed tasks first
	for _, t := range all {
		if shown >= maxShow {
			break
		}
		if t.Status == tasks.StatusCompleted {
			continue
		}
		renderTaskLine(&sb, t, agentByTask, dimStyle, successStyle, activeStyle, width)
		shown++
	}

	// Then completed tasks
	for _, t := range all {
		if shown >= maxShow {
			break
		}
		if t.Status != tasks.StatusCompleted {
			continue
		}
		renderCompletedLine(&sb, t, agentByTask, dimStyle, successStyle, width)
		shown++
	}

	// Overflow
	remaining := len(all) - shown
	if remaining > 0 {
		sb.WriteString(dimStyle.Render(fmt.Sprintf("  +%d more\n", remaining)))
	}

	return sb.String()
}

func renderTaskLine(sb *strings.Builder, t tasks.Task, agentByTask map[string]*swarmAgentEntry, dimStyle, successStyle, activeStyle lipgloss.Style, width int) {
	// Icon
	var icon string
	switch t.Status {
	case tasks.StatusInProgress:
		icon = activeStyle.Render("▪")
	default:
		icon = dimStyle.Render("▫")
	}

	// Owner suffix
	ownerSuffix := ""
	if t.Owner != "" {
		ownerSuffix = fmt.Sprintf(" (@%s)", t.Owner)
	}

	// Subject (truncated)
	subject := t.Subject
	maxSubject := width - 8 - len(ownerSuffix)
	if maxSubject < 10 {
		maxSubject = 10
	}
	if r := []rune(subject); len(r) > maxSubject {
		subject = string(r[:maxSubject-3]) + "..."
	}

	// Task line
	line := fmt.Sprintf("  %s %s", icon, subject)
	if ownerSuffix != "" {
		line += dimStyle.Render(ownerSuffix)
	}
	sb.WriteString(line + "\n")

	// Activity sub-line for in_progress tasks with matched running agent
	if t.Status == tasks.StatusInProgress {
		if ag, ok := agentByTask[t.ID]; ok && ag.status == "running" {
			activity := summarizeActivity(ag.recentTools, ag.description)
			if activity != "" {
				maxActivity := width - 8
				if maxActivity < 10 {
					maxActivity = 10
				}
				if len(activity) > maxActivity {
					activity = activity[:maxActivity-3] + "..."
				}
				sb.WriteString(dimStyle.Render("    ↳ "+activity) + "\n")
			}
		}
	}
}

func renderCompletedLine(sb *strings.Builder, t tasks.Task, agentByTask map[string]*swarmAgentEntry, dimStyle, successStyle lipgloss.Style, width int) {
	ownerSuffix := ""
	if t.Owner != "" {
		ownerSuffix = fmt.Sprintf(" (@%s)", t.Owner)
	}

	// Elapsed from matched agent
	elapsedStr := ""
	if ag, ok := agentByTask[t.ID]; ok && ag.elapsed > 0 {
		elapsedStr = " " + formatDurationCompact(ag.elapsed)
	}

	subject := t.Subject
	maxSubject := width - 8 - len(ownerSuffix) - len(elapsedStr)
	if maxSubject < 10 {
		maxSubject = 10
	}
	if r := []rune(subject); len(r) > maxSubject {
		subject = string(r[:maxSubject-3]) + "..."
	}

	line := fmt.Sprintf("  %s %s", successStyle.Render("✔"), dimStyle.Render(subject))
	if ownerSuffix != "" {
		line += dimStyle.Render(ownerSuffix)
	}
	if elapsedStr != "" {
		line += dimStyle.Render(elapsedStr)
	}
	sb.WriteString(line + "\n")
}
