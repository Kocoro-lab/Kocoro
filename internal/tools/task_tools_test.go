package tools

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/tasks"
)

func newTestTaskStore(t *testing.T) *tasks.Store {
	t.Helper()
	return tasks.NewStore(filepath.Join(t.TempDir(), "tasks"))
}

func testProvider(store *tasks.Store) func() *tasks.Store {
	return func() *tasks.Store { return store }
}

// ─── task_create ─────────────────────────────────────────────────────────────

func TestTaskCreateTool_Info(t *testing.T) {
	tool := &TaskCreateTool{storeProvider: testProvider(newTestTaskStore(t))}
	info := tool.Info()

	if info.Name != "task_create" {
		t.Errorf("expected name %q, got %q", "task_create", info.Name)
	}
	if len(info.Required) != 2 {
		t.Errorf("expected 2 required params, got %d", len(info.Required))
	}
}

func TestTaskCreateTool_Run(t *testing.T) {
	store := newTestTaskStore(t)
	tool := &TaskCreateTool{storeProvider: testProvider(store)}

	result, err := tool.Run(context.Background(), `{"subject":"Fix login","description":"Investigate the auth bug"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("expected success, got error: %s", result.Content)
	}
	if !strings.Contains(result.Content, "#1") {
		t.Errorf("expected task ID #1 in content, got: %s", result.Content)
	}
	if !strings.Contains(result.Content, "Fix login") {
		t.Errorf("expected subject in content, got: %s", result.Content)
	}

	// Verify persisted with correct subject/status.
	task, err := store.Get("1")
	if err != nil {
		t.Fatalf("task not persisted: %v", err)
	}
	if task.Subject != "Fix login" {
		t.Errorf("expected subject %q, got %q", "Fix login", task.Subject)
	}
	if task.Status != tasks.StatusPending {
		t.Errorf("expected status %q, got %q", tasks.StatusPending, task.Status)
	}
}

func TestTaskCreateTool_InvalidJSON(t *testing.T) {
	tool := &TaskCreateTool{storeProvider: testProvider(newTestTaskStore(t))}

	result, err := tool.Run(context.Background(), `not-json`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Error("expected IsError for invalid JSON")
	}
}

func TestTaskCreateTool_MissingSubject(t *testing.T) {
	tool := &TaskCreateTool{storeProvider: testProvider(newTestTaskStore(t))}

	result, err := tool.Run(context.Background(), `{"description":"some work"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Error("expected IsError for missing subject")
	}
}

func TestTaskCreateTool_MissingDescription(t *testing.T) {
	tool := &TaskCreateTool{storeProvider: testProvider(newTestTaskStore(t))}

	result, err := tool.Run(context.Background(), `{"subject":"Do something"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Error("expected IsError for missing description")
	}
}

func TestTaskCreateTool_WithOwner(t *testing.T) {
	store := newTestTaskStore(t)
	tool := &TaskCreateTool{storeProvider: testProvider(store)}

	result, err := tool.Run(context.Background(), `{"subject":"Research API","description":"deep dive","owner":"scout-1"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("expected success, got error: %s", result.Content)
	}

	task, err := store.Get("1")
	if err != nil {
		t.Fatalf("task not found: %v", err)
	}
	if task.Owner != "scout-1" {
		t.Errorf("expected owner %q, got %q", "scout-1", task.Owner)
	}
}

// ─── task_list ────────────────────────────────────────────────────────────────

func TestTaskListTool_Empty(t *testing.T) {
	tool := &TaskListTool{storeProvider: testProvider(newTestTaskStore(t))}

	result, err := tool.Run(context.Background(), `{}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error result: %s", result.Content)
	}
	if result.Content != "No tasks." {
		t.Errorf("expected %q, got %q", "No tasks.", result.Content)
	}
}

func TestTaskListTool_WithTasks(t *testing.T) {
	store := newTestTaskStore(t)
	createTool := &TaskCreateTool{storeProvider: testProvider(store)}

	subjects := []string{"Task A", "Task B", "Task C"}
	for _, s := range subjects {
		_, err := createTool.Run(context.Background(), `{"subject":"`+s+`","description":"desc"}`)
		if err != nil {
			t.Fatalf("create failed: %v", err)
		}
	}

	listTool := &TaskListTool{storeProvider: testProvider(store)}
	result, err := listTool.Run(context.Background(), `{}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error result: %s", result.Content)
	}

	for _, id := range []string{"#1", "#2", "#3"} {
		if !strings.Contains(result.Content, id) {
			t.Errorf("expected %s in list output, got: %s", id, result.Content)
		}
	}
}

// ─── task_update ─────────────────────────────────────────────────────────────

func TestTaskUpdateTool_ChangeStatus(t *testing.T) {
	store := newTestTaskStore(t)
	createTool := &TaskCreateTool{storeProvider: testProvider(store)}
	_, err := createTool.Run(context.Background(), `{"subject":"Work item","description":"do it"}`)
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	updateTool := &TaskUpdateTool{storeProvider: testProvider(store)}
	result, err := updateTool.Run(context.Background(), `{"id":"1","status":"in_progress"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error result: %s", result.Content)
	}

	task, err := store.Get("1")
	if err != nil {
		t.Fatalf("get failed: %v", err)
	}
	if task.Status != tasks.StatusInProgress {
		t.Errorf("expected status %q, got %q", tasks.StatusInProgress, task.Status)
	}
}

func TestTaskUpdateTool_SetOwner(t *testing.T) {
	store := newTestTaskStore(t)
	createTool := &TaskCreateTool{storeProvider: testProvider(store)}
	_, err := createTool.Run(context.Background(), `{"subject":"Owned task","description":"do it"}`)
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	updateTool := &TaskUpdateTool{storeProvider: testProvider(store)}
	result, err := updateTool.Run(context.Background(), `{"id":"1","owner":"worker-agent"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error result: %s", result.Content)
	}

	task, err := store.Get("1")
	if err != nil {
		t.Fatalf("get failed: %v", err)
	}
	if task.Owner != "worker-agent" {
		t.Errorf("expected owner %q, got %q", "worker-agent", task.Owner)
	}
}

func TestTaskUpdateTool_NotFound(t *testing.T) {
	tool := &TaskUpdateTool{storeProvider: testProvider(newTestTaskStore(t))}

	result, err := tool.Run(context.Background(), `{"id":"999","status":"completed"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Error("expected IsError for non-existent task")
	}
}

func TestTaskUpdateTool_InvalidStatus(t *testing.T) {
	store := newTestTaskStore(t)
	createTool := &TaskCreateTool{storeProvider: testProvider(store)}
	_, err := createTool.Run(context.Background(), `{"subject":"Task","description":"desc"}`)
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	updateTool := &TaskUpdateTool{storeProvider: testProvider(store)}
	result, err := updateTool.Run(context.Background(), `{"id":"1","status":"broken"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Error("expected IsError for invalid status")
	}
}

func TestTaskUpdateTool_MissingID(t *testing.T) {
	tool := &TaskUpdateTool{storeProvider: testProvider(newTestTaskStore(t))}

	result, err := tool.Run(context.Background(), `{"status":"completed"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Error("expected IsError for missing ID")
	}
}

// ─── task_get ────────────────────────────────────────────────────────────────

func TestTaskGetTool_Found(t *testing.T) {
	store := newTestTaskStore(t)
	createTool := &TaskCreateTool{storeProvider: testProvider(store)}
	_, err := createTool.Run(context.Background(), `{"subject":"My task","description":"Do the thing"}`)
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	getTool := &TaskGetTool{storeProvider: testProvider(store)}
	result, err := getTool.Run(context.Background(), `{"id":"1"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error result: %s", result.Content)
	}
	if !strings.Contains(result.Content, "My task") {
		t.Errorf("expected subject in content, got: %s", result.Content)
	}
	if !strings.Contains(result.Content, "Do the thing") {
		t.Errorf("expected description in content, got: %s", result.Content)
	}
}

func TestTaskGetTool_NotFound(t *testing.T) {
	tool := &TaskGetTool{storeProvider: testProvider(newTestTaskStore(t))}

	result, err := tool.Run(context.Background(), `{"id":"999"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Error("expected IsError for non-existent task")
	}
}

func TestTaskGetTool_MissingID(t *testing.T) {
	tool := &TaskGetTool{storeProvider: testProvider(newTestTaskStore(t))}

	result, err := tool.Run(context.Background(), `{}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Error("expected IsError for missing ID")
	}
}

func TestTaskCreateTool_HasPrompt(t *testing.T) {
	tool := &TaskCreateTool{storeProvider: testProvider(newTestTaskStore(t))}
	info := tool.Info()

	if info.Prompt == "" {
		t.Error("expected non-empty Prompt for task_create tool")
	}
}

func TestTaskTools_AllHavePrompt(t *testing.T) {
	store := newTestTaskStore(t)
	provider := testProvider(store)

	tools := []agent.Tool{
		&TaskCreateTool{storeProvider: provider},
		&TaskUpdateTool{storeProvider: provider},
		&TaskListTool{storeProvider: provider},
		&TaskGetTool{storeProvider: provider},
	}

	for _, tool := range tools {
		info := tool.Info()
		if info.Prompt == "" {
			t.Errorf("tool %q has empty Prompt", info.Name)
		}
	}
}
