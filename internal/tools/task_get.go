package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/tasks"
)

type TaskGetTool struct {
	storeProvider func() *tasks.Store
}

type taskGetArgs struct {
	ID string `json:"id"`
}

func (t *TaskGetTool) Info() agent.ToolInfo {
	return agent.ToolInfo{
		Name:        "task_get",
		Description: "Get details of a specific task by ID.",
		Prompt:      "Use to check a task's current state before updating it.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"id": map[string]any{
					"type":        "string",
					"description": "Task ID to retrieve",
				},
			},
		},
		Required: []string{"id"},
	}
}

func (t *TaskGetTool) Run(ctx context.Context, argsJSON string) (agent.ToolResult, error) {
	store := t.storeProvider()
	if store == nil {
		return agent.ToolResult{Content: "task store not configured", IsError: true}, nil
	}

	var args taskGetArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return agent.ValidationError(fmt.Sprintf("invalid arguments: %v", err)), nil
	}
	if args.ID == "" {
		return agent.ValidationError("id is required"), nil
	}

	task, err := store.Get(args.ID)
	if err != nil {
		return agent.ToolResult{Content: fmt.Sprintf("task not found: %v", err), IsError: true}, nil
	}

	out := fmt.Sprintf("Task #%s\nSubject: %s\nStatus: %s\nDescription: %s",
		task.ID, task.Subject, task.Status, task.Description)
	if task.Owner != "" {
		out += fmt.Sprintf("\nOwner: %s", task.Owner)
	}
	return agent.ToolResult{Content: out}, nil
}

func (t *TaskGetTool) RequiresApproval() bool { return false }

func (t *TaskGetTool) IsReadOnlyCall(string) bool { return true }
