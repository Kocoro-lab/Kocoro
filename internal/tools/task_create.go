package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/tasks"
)

type TaskCreateTool struct {
	storeProvider func() *tasks.Store
}

type taskCreateArgs struct {
	Subject     string `json:"subject"`
	Description string `json:"description"`
	Owner       string `json:"owner,omitempty"`
}

func (t *TaskCreateTool) Info() agent.ToolInfo {
	return agent.ToolInfo{
		Name:        "task_create",
		Description: "Create a task to track work progress.",
		Prompt: `Use this tool to create a structured task list for your current session.

## When to Use
- Complex multi-step tasks requiring 3+ distinct steps
- User provides multiple tasks (numbered or comma-separated)
- After receiving new instructions — capture requirements as tasks immediately
- When you start working on a task — create it, then set in_progress

## When NOT to Use
- Single, straightforward task
- Trivial task completable in < 3 steps
- Purely conversational or informational request

## Fields
- subject: Brief, actionable title in imperative form (e.g., "Fix auth bug in login flow")
- description: What needs to be done

All tasks are created with status pending.

## Tips
- Create tasks with clear, specific subjects describing the outcome
- After creating, use task_update to set in_progress before starting each
- Mark completed immediately after finishing — do not batch`,
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"subject": map[string]any{
					"type":        "string",
					"description": "Brief title for the task",
				},
				"description": map[string]any{
					"type":        "string",
					"description": "What needs to be done",
				},
				"owner": map[string]any{
					"type":        "string",
					"description": "Agent name to own this task (e.g., 'scout-1'). Used when delegating to sub-agents.",
				},
			},
		},
		Required: []string{"subject", "description"},
	}
}

func (t *TaskCreateTool) Run(ctx context.Context, argsJSON string) (agent.ToolResult, error) {
	store := t.storeProvider()
	if store == nil {
		return agent.ToolResult{Content: "task store not configured", IsError: true}, nil
	}

	var args taskCreateArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return agent.ValidationError(fmt.Sprintf("invalid arguments: %v", err)), nil
	}
	if args.Subject == "" {
		return agent.ValidationError("subject is required"), nil
	}
	if args.Description == "" {
		return agent.ValidationError("description is required"), nil
	}

	id, err := store.Create(tasks.Task{
		Subject:     args.Subject,
		Description: args.Description,
		Status:      tasks.StatusPending,
		Owner:       args.Owner,
	})
	if err != nil {
		return agent.ToolResult{Content: fmt.Sprintf("failed to create task: %v", err), IsError: true}, nil
	}

	return agent.ToolResult{Content: fmt.Sprintf("Created task #%s: %s", id, args.Subject)}, nil
}

func (t *TaskCreateTool) RequiresApproval() bool { return false }
