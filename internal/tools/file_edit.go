package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/cwdctx"
)

type FileEditTool struct{}

type fileEditArgs struct {
	Path       string `json:"path"`
	OldString  string `json:"old_string"`
	NewString  string `json:"new_string"`
	ReplaceAll bool   `json:"replace_all,omitempty"` // when true, replaces every occurrence; default false (must be unique)
}

func (t *FileEditTool) Info() agent.ToolInfo {
	return agent.ToolInfo{
		Name:        "file_edit",
		Description: "Replace an exact string in a file. By default old_string must appear exactly once (use the smallest snippet that's clearly unique — usually 2-4 adjacent lines is sufficient; don't paste 10+ lines just to disambiguate). Pass replace_all=true to rename / refactor every occurrence in one call.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":        map[string]any{"type": "string", "description": "File path to edit"},
				"old_string":  map[string]any{"type": "string", "description": "Exact string to find. Must be unique unless replace_all=true."},
				"new_string":  map[string]any{"type": "string", "description": "Replacement string"},
				"replace_all": map[string]any{"type": "boolean", "description": "When true, replace every occurrence of old_string. When false (default), old_string must appear exactly once. Use replace_all only when the target is unambiguous globally (variable rename, refactor)."},
			},
		},
		Required: []string{"path", "old_string", "new_string"},
	}
}

func (t *FileEditTool) Run(ctx context.Context, argsJSON string) (agent.ToolResult, error) {
	var args fileEditArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return agent.ToolResult{Content: fmt.Sprintf("invalid arguments: %v", err), IsError: true}, nil
	}
	resolved, resolveErr := cwdctx.ResolveFilesystemPath(ctx, args.Path)
	if resolveErr != nil {
		if errors.Is(resolveErr, cwdctx.ErrNoSessionCWD) {
			return agent.ValidationError(
				"file_edit: no session working directory is set. Pass an absolute path.",
			), nil
		}
		return agent.ValidationError(fmt.Sprintf("file_edit: %v", resolveErr)), nil
	}
	args.Path = resolved

	// Enforce read-before-edit
	if err := agent.CheckReadBeforeWrite(ctx, args.Path); err != nil {
		return agent.ToolResult{Content: err.Error(), IsError: true}, nil
	}

	// Block file_edit on the agent's MEMORY.md — use memory_append instead.
	// file_edit is a read-modify-write that races under concurrent sessions.
	if agent.IsMemoryFile(ctx, args.Path) {
		return agent.ToolResult{
			Content: "Cannot edit MEMORY.md with file_edit — it races under concurrent sessions. Use the memory_append tool instead.",
			IsError: true,
		}, nil
	}

	data, err := os.ReadFile(args.Path)
	if err != nil {
		if os.IsPermission(err) {
			return agent.PermissionError(fmt.Sprintf("cannot read %s: permission denied", args.Path)), nil
		}
		return agent.ToolResult{Content: fmt.Sprintf("error reading file: %v", err), IsError: true}, nil
	}

	if args.OldString == "" {
		return agent.ValidationError("old_string must not be empty"), nil
	}

	content := string(data)
	count := strings.Count(content, args.OldString)
	if count == 0 {
		return agent.ValidationError("old_string not found in file"), nil
	}
	if !args.ReplaceAll && count > 1 {
		return agent.ValidationError(fmt.Sprintf("old_string found %d times (must be unique unless replace_all=true)", count)), nil
	}

	var newContent string
	if args.ReplaceAll {
		newContent = strings.ReplaceAll(content, args.OldString, args.NewString)
	} else {
		newContent = strings.Replace(content, args.OldString, args.NewString, 1)
	}
	// Preserve original file permissions
	perm := os.FileMode(0644)
	if info, err := os.Stat(args.Path); err == nil {
		perm = info.Mode().Perm()
	}
	if err := os.WriteFile(args.Path, []byte(newContent), perm); err != nil {
		if os.IsPermission(err) {
			return agent.PermissionError(fmt.Sprintf("cannot write %s: permission denied", args.Path)), nil
		}
		return agent.ToolResult{Content: fmt.Sprintf("error writing file: %v", err), IsError: true}, nil
	}

	noun := "occurrence"
	if count > 1 {
		noun = "occurrences"
	}
	return agent.ToolResult{Content: fmt.Sprintf("edited %s: replaced %d %s", args.Path, count, noun)}, nil
}

func (t *FileEditTool) RequiresApproval() bool { return true }

func (t *FileEditTool) IsReadOnlyCall(string) bool { return false }
