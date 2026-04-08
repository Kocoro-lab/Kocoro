package agent

import (
	"fmt"
	"strings"
)

// taskReminderItem is a lightweight struct for reminder rendering,
// avoiding a dependency on the tasks package.
type taskReminderItem struct {
	ID      string
	Status  string
	Subject string
}

// buildTaskReminder generates a periodic nudge with the current task list.
func buildTaskReminder(items []taskReminderItem) string {
	var sb strings.Builder
	sb.WriteString("<system-reminder>\n")
	sb.WriteString("The task tools haven't been used recently. If you're working on tasks that would benefit from tracking progress, consider using task_create to add new tasks and task_update to update task status (set to in_progress when starting, completed when done). Also consider cleaning up the task list if it has become stale. Only use these if relevant to the current work. This is just a gentle reminder - ignore if not applicable. Make sure that you NEVER mention this reminder to the user\n")
	if len(items) > 0 {
		sb.WriteString("\nHere are the existing tasks:\n\n")
		for _, item := range items {
			fmt.Fprintf(&sb, "#%s [%s] %s\n", item.ID, item.Status, item.Subject)
		}
	}
	sb.WriteString("</system-reminder>")
	return sb.String()
}

// parseTaskLine extracts ID, status, and subject from task_list output format:
// "#1 [pending] Fix auth bug" or "#1 [pending] Fix auth bug (owner: worker)"
func parseTaskLine(line string) taskReminderItem {
	item := taskReminderItem{Subject: line}
	if len(line) < 2 || line[0] != '#' {
		return item
	}
	rest := line[1:]
	spIdx := strings.IndexByte(rest, ' ')
	if spIdx < 0 {
		return item
	}
	item.ID = rest[:spIdx]
	rest = rest[spIdx+1:]
	if len(rest) < 2 || rest[0] != '[' {
		return item
	}
	closeIdx := strings.IndexByte(rest, ']')
	if closeIdx < 0 {
		return item
	}
	item.Status = rest[1:closeIdx]
	if closeIdx+2 < len(rest) {
		item.Subject = strings.TrimSpace(rest[closeIdx+2:])
	}
	return item
}
