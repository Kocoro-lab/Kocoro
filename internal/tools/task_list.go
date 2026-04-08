package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/tasks"
)

type TaskListTool struct {
	storeProvider func() *tasks.Store
}

func (t *TaskListTool) Info() agent.ToolInfo {
	return agent.ToolInfo{
		Name:        "task_list",
		Description: "List all tasks with their current status.",
		Prompt:      "Check after completing each task to find your next work item. Use to verify all tasks are done before reporting completion.",
		Parameters: map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		},
	}
}

func (t *TaskListTool) Run(ctx context.Context, argsJSON string) (agent.ToolResult, error) {
	store := t.storeProvider()
	if store == nil {
		return agent.ToolResult{Content: "task store not configured", IsError: true}, nil
	}

	all, err := store.List()
	if err != nil {
		return agent.ToolResult{Content: fmt.Sprintf("failed to list tasks: %v", err), IsError: true}, nil
	}
	if len(all) == 0 {
		return agent.ToolResult{Content: "No tasks."}, nil
	}

	var b strings.Builder
	for _, task := range all {
		fmt.Fprintf(&b, "#%s [%s] %s", task.ID, task.Status, task.Subject)
		if task.Owner != "" {
			fmt.Fprintf(&b, " (owner: %s)", task.Owner)
		}
		b.WriteByte('\n')
	}
	return agent.ToolResult{Content: strings.TrimRight(b.String(), "\n")}, nil
}

func (t *TaskListTool) RequiresApproval() bool { return false }

func (t *TaskListTool) IsReadOnlyCall(string) bool { return true }
