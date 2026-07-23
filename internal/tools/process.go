package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
)

type ProcessTool struct{}

type processArgs struct {
	Action      string `json:"action"`
	Description string `json:"description,omitempty"`
	PID         int    `json:"pid,omitempty"`
	Port        int    `json:"port,omitempty"`
}

func (t *ProcessTool) Info() agent.ToolInfo {
	return agent.ToolInfo{
		Name: "process",
		Description: "Manage processes and ports. Actions: 'list' (ps aux), 'ports' (listening ports), 'kill' (kill a PID)." +
			agent.DescriptionGuidance,
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"action":      map[string]any{"type": "string", "description": "Action: 'list', 'ports', or 'kill'"},
				"description": agent.DescriptionFieldSpec,
				"pid":         map[string]any{"type": "integer", "description": "Process ID (required for kill)"},
				"port":        map[string]any{"type": "integer", "description": "Filter by port number (optional for ports)"},
			},
		},
		Required: []string{"action", "description"},
	}
}

func (t *ProcessTool) Run(ctx context.Context, argsJSON string) (agent.ToolResult, error) {
	var args processArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return agent.ValidationError(fmt.Sprintf("invalid arguments: %v", err)), nil
	}
	if strings.TrimSpace(args.Action) == "" {
		return agent.ValidationError("process: missing required `action` parameter"), nil
	}
	if strings.TrimSpace(args.Description) == "" {
		return agent.ValidationError("process: missing required `description` parameter"), nil
	}

	switch args.Action {
	case "list":
		cmd := exec.CommandContext(ctx, "ps", "aux")
		output, err := cmd.CombinedOutput()
		if err != nil {
			return agent.ToolResult{Content: fmt.Sprintf("ps error: %v\n%s", err, string(output)), IsError: true}, nil
		}
		result := string(output)
		if len(result) > 30000 {
			result = result[:30000] + "\n... (truncated)"
		}
		return agent.ToolResult{Content: result}, nil

	case "ports":
		cmdArgs := []string{"-i", "-P", "-n"}
		if args.Port > 0 {
			cmdArgs = []string{"-i", fmt.Sprintf(":%d", args.Port), "-P", "-n"}
		}
		cmd := exec.CommandContext(ctx, "lsof", cmdArgs...)
		output, err := cmd.CombinedOutput()
		if err != nil {
			// lsof exits 1 when no results found
			if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
				return agent.ToolResult{Content: "no matching ports found"}, nil
			}
			return agent.ToolResult{Content: fmt.Sprintf("lsof error: %v\n%s", err, string(output)), IsError: true}, nil
		}
		result := string(output)
		if len(result) > 30000 {
			result = result[:30000] + "\n... (truncated)"
		}
		return agent.ToolResult{Content: result}, nil

	case "kill":
		if args.PID == 0 {
			return agent.ValidationError("process: pid is required for kill action"), nil
		}
		cmd := exec.CommandContext(ctx, "kill", fmt.Sprintf("%d", args.PID))
		output, err := cmd.CombinedOutput()
		if err != nil {
			return agent.ToolResult{Content: fmt.Sprintf("kill error: %v\n%s", err, string(output)), IsError: true}, nil
		}
		return agent.ToolResult{Content: fmt.Sprintf("sent SIGTERM to PID %d", args.PID)}, nil

	default:
		return agent.ValidationError(fmt.Sprintf("process: unknown action: %q (use 'list', 'ports', or 'kill')", args.Action)), nil
	}
}

func (t *ProcessTool) RequiresApproval() bool { return true }

func (t *ProcessTool) IsReadOnlyCall(string) bool { return false }

func (t *ProcessTool) IsSafeArgs(argsJSON string) bool {
	var args processArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return false
	}
	return args.Action == "list" || args.Action == "ports"
}
