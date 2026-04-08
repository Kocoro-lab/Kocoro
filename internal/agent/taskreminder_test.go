package agent

import (
	"strings"
	"testing"
)

func TestBuildTaskReminder_Empty(t *testing.T) {
	reminder := buildTaskReminder(nil)
	if !strings.Contains(reminder, "task tools haven't been used recently") {
		t.Error("expected base reminder text")
	}
	if strings.Contains(reminder, "existing tasks") {
		t.Error("should not mention existing tasks when list is empty")
	}
}

func TestBuildTaskReminder_WithTasks(t *testing.T) {
	items := []taskReminderItem{
		{ID: "1", Status: "completed", Subject: "Fix auth bug"},
		{ID: "2", Status: "in_progress", Subject: "Add tests"},
		{ID: "3", Status: "pending", Subject: "Update docs"},
	}
	reminder := buildTaskReminder(items)
	if !strings.Contains(reminder, "#1 [completed] Fix auth bug") {
		t.Error("expected task #1 in reminder")
	}
	if !strings.Contains(reminder, "#2 [in_progress] Add tests") {
		t.Error("expected task #2 in reminder")
	}
	if !strings.Contains(reminder, "existing tasks") {
		t.Error("expected 'existing tasks' header when tasks present")
	}
	if !strings.Contains(reminder, "<system-reminder>") {
		t.Error("expected system-reminder tags")
	}
}

func TestParseTaskLine(t *testing.T) {
	tests := []struct {
		line string
		want taskReminderItem
	}{
		{"#1 [pending] Fix auth bug", taskReminderItem{ID: "1", Status: "pending", Subject: "Fix auth bug"}},
		{"#2 [in_progress] Add tests (owner: worker)", taskReminderItem{ID: "2", Status: "in_progress", Subject: "Add tests (owner: worker)"}},
		{"garbage", taskReminderItem{Subject: "garbage"}},
		{"", taskReminderItem{Subject: ""}},
	}
	for _, tt := range tests {
		got := parseTaskLine(tt.line)
		if got != tt.want {
			t.Errorf("parseTaskLine(%q) = %+v, want %+v", tt.line, got, tt.want)
		}
	}
}
