package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/tasks"
)

type TaskUpdateTool struct {
	storeProvider func() *tasks.Store
}

type taskUpdateArgs struct {
	ID     string `json:"id"`
	Status string `json:"status,omitempty"`
	Owner  string `json:"owner,omitempty"`
}

func (t *TaskUpdateTool) Info() agent.ToolInfo {
	return agent.ToolInfo{
		Name:        "task_update",
		Description: "Update a task's status or owner.",
		Prompt: `Update task status to track work progress.

## Status Workflow
pending → in_progress → completed

- Set in_progress BEFORE starting work on a task
- Set completed IMMEDIATELY after finishing
- Keep exactly one task in_progress at a time
- After completing, check task_list to find the next task

## Completion Rules
- ONLY mark completed when you have FULLY accomplished it
- If you encounter errors or blockers, keep as in_progress
- Never mark completed if tests are failing or implementation is partial

## Fields
- id: Task ID to update (required)
- status: New status (pending, in_progress, completed)
- owner: Agent name claiming this task`,
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"id": map[string]any{
					"type":        "string",
					"description": "Task ID to update",
				},
				"status": map[string]any{
					"type":        "string",
					"enum":        []string{"pending", "in_progress", "completed"},
					"description": "New status",
				},
				"owner": map[string]any{
					"type":        "string",
					"description": "Agent name claiming this task",
				},
			},
		},
		Required: []string{"id"},
	}
}

func (t *TaskUpdateTool) Run(ctx context.Context, argsJSON string) (agent.ToolResult, error) {
	store := t.storeProvider()
	if store == nil {
		return agent.ToolResult{Content: "task store not configured", IsError: true}, nil
	}

	var args taskUpdateArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return agent.ValidationError(fmt.Sprintf("invalid arguments: %v", err)), nil
	}
	if args.ID == "" {
		return agent.ValidationError("id is required"), nil
	}

	var updates tasks.TaskUpdates
	if args.Status != "" {
		updates.Status = &args.Status
	}
	if args.Owner != "" {
		updates.Owner = &args.Owner
	}

	updated, err := store.Update(args.ID, updates)
	if err != nil {
		return agent.ToolResult{Content: fmt.Sprintf("update failed: %v", err), IsError: true}, nil
	}

	return agent.ToolResult{Content: fmt.Sprintf("Updated task #%s: [%s] %s", updated.ID, updated.Status, updated.Subject)}, nil
}

func (t *TaskUpdateTool) RequiresApproval() bool { return false }
