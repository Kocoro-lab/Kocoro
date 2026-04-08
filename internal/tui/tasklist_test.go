package tui

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func createTestTasks(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "tasks")
	os.MkdirAll(dir, 0700)

	testTasks := []struct {
		id, subject, status, owner string
	}{
		{"1", "Set up project", "completed", "scout-1"},
		{"2", "Research API docs", "in_progress", "scout-2"},
		{"3", "Write unit tests", "in_progress", "scout-3"},
		{"4", "Update docs", "pending", ""},
	}

	for _, task := range testTasks {
		data, _ := json.Marshal(map[string]string{
			"id": task.id, "subject": task.subject,
			"status": task.status, "description": "d", "owner": task.owner,
		})
		os.WriteFile(filepath.Join(dir, task.id+".json"), data, 0600)
	}
	return dir
}

func TestRenderTaskList_TasksWithAgents(t *testing.T) {
	dir := createTestTasks(t)
	agents := []swarmAgentEntry{
		{id: "a1", agentType: "scout", status: "completed", taskID: "1", elapsed: 12 * time.Second},
		{id: "a2", agentType: "scout", status: "running", taskID: "2",
			recentTools: []recentToolEntry{{name: "bash", keyArg: `file "/tmp/doc.pdf"`}}},
		{id: "a3", agentType: "scout", status: "running", taskID: "3",
			recentTools: []recentToolEntry{
				{name: "file_read", keyArg: "api.go", isRead: true},
				{name: "file_read", keyArg: "handler.go", isRead: true},
			}},
	}

	result := renderTaskList(dir, agents, 80)

	if !strings.Contains(result, "Tasks") {
		t.Error("expected Tasks header")
	}
	if !strings.Contains(result, "Research API docs") {
		t.Error("expected task #2 subject")
	}
	if !strings.Contains(result, "@scout-2") {
		t.Error("expected owner @scout-2")
	}
	if !strings.Contains(result, "↳") {
		t.Error("expected activity sub-line indicator")
	}
}

func TestRenderTaskList_TasksWithoutAgents(t *testing.T) {
	dir := createTestTasks(t)
	result := renderTaskList(dir, nil, 80)

	if !strings.Contains(result, "Tasks") {
		t.Error("expected Tasks header")
	}
	if !strings.Contains(result, "Research API docs") {
		t.Error("expected task subject")
	}
	if strings.Contains(result, "↳") {
		t.Error("expected no activity lines without agents")
	}
}

func TestRenderTaskList_NoTasks(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "empty-tasks")
	os.MkdirAll(dir, 0700)
	agents := []swarmAgentEntry{
		{id: "a1", agentType: "scout", status: "running"},
	}

	result := renderTaskList(dir, agents, 80)

	if result != "" {
		t.Errorf("expected empty string for no tasks, got %q", result)
	}
}

func TestRenderTaskList_Empty(t *testing.T) {
	result := renderTaskList("", nil, 80)
	if result != "" {
		t.Errorf("expected empty string, got %q", result)
	}
}
