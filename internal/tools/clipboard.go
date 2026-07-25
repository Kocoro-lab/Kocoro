package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"runtime"
	"strings"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
)

type ClipboardTool struct{}

type clipboardArgs struct {
	Action      string `json:"action"`
	Description string `json:"description,omitempty"`
	Content     string `json:"content,omitempty"`
}

func (t *ClipboardTool) Info() agent.ToolInfo {
	return agent.ToolInfo{
		Name: "clipboard",
		Description: "Read or write the system clipboard (macOS only). Use action 'read' to get clipboard contents, 'write' to set them." +
			agent.DescriptionGuidance,
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"action":      map[string]any{"type": "string", "description": "Action: 'read' or 'write'"},
				"description": agent.DescriptionFieldSpec,
				"content":     map[string]any{"type": "string", "description": "Content to write (required for write action)"},
			},
		},
		Required: []string{"action", "description"},
	}
}

func (t *ClipboardTool) Run(ctx context.Context, argsJSON string) (agent.ToolResult, error) {
	if runtime.GOOS != "darwin" {
		return agent.ToolResult{Content: "clipboard tool is only available on macOS", IsError: true}, nil
	}
	var args clipboardArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return agent.ValidationError(fmt.Sprintf("invalid arguments: %v", err)), nil
	}
	if strings.TrimSpace(args.Action) == "" {
		return agent.ValidationError("clipboard: missing required `action` parameter"), nil
	}
	if strings.TrimSpace(args.Description) == "" {
		return agent.ValidationError("clipboard: missing required `description` parameter"), nil
	}

	switch args.Action {
	case "read":
		cmd := exec.CommandContext(ctx, "pbpaste")
		output, err := cmd.CombinedOutput()
		if err != nil {
			return agent.ToolResult{Content: fmt.Sprintf("clipboard read error: %v", err), IsError: true}, nil
		}
		return agent.ToolResult{Content: string(output)}, nil

	case "write":
		if args.Content == "" {
			return agent.ValidationError("clipboard: content is required for write action"), nil
		}
		cmd := exec.CommandContext(ctx, "pbcopy")
		cmd.Stdin = bytes.NewReader([]byte(args.Content))
		if err := cmd.Run(); err != nil {
			return agent.ToolResult{Content: fmt.Sprintf("clipboard write error: %v", err), IsError: true}, nil
		}
		return agent.ToolResult{Content: fmt.Sprintf("wrote %d bytes to clipboard", len(args.Content))}, nil

	default:
		return agent.ValidationError(fmt.Sprintf("clipboard: unknown action: %q (use 'read' or 'write')", args.Action)), nil
	}
}

func (t *ClipboardTool) RequiresApproval() bool { return true }

func (t *ClipboardTool) IsReadOnlyCall(argsJSON string) bool {
	var args struct {
		Action string `json:"action"`
	}
	if json.Unmarshal([]byte(argsJSON), &args) != nil {
		return false
	}
	return args.Action == "read"
}
