package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/cwdctx"
)

type FileWriteTool struct{}

type fileWriteArgs struct {
	Path        string `json:"path"`
	Content     string `json:"content"`
	Description string `json:"description,omitempty"`
}

func (t *FileWriteTool) Info() agent.ToolInfo {
	return agent.ToolInfo{
		Name: "file_write",
		Description: "Write complete content to a file (overwrites entirely). Use for creating new files or as fallback when file_edit fails due to non-unique text. Always file_read first if the file already exists." +
			agent.DescriptionGuidance,
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":        map[string]any{"type": "string", "description": "File path to write"},
				"content":     map[string]any{"type": "string", "description": "Content to write"},
				"description": agent.DescriptionFieldSpec,
			},
		},
		Required: []string{"path", "content", "description"},
	}
}

func (t *FileWriteTool) Run(ctx context.Context, argsJSON string) (agent.ToolResult, error) {
	var args fileWriteArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return agent.ValidationError(fmt.Sprintf("invalid arguments: %v", err)), nil
	}
	if strings.TrimSpace(args.Path) == "" {
		return agent.ValidationError("file_write: missing required `path` parameter"), nil
	}
	// Reject calls that omit content (or pass an empty string). Without this
	// guard, os.WriteFile happily writes 0 bytes and returns "wrote 0 bytes"
	// with IsError=false — the model reads that as a successful write and
	// keeps looping. See the 2026-05-13 stuck-loop incident. Only "" is
	// rejected (not whitespace-only): writing a file whose content is "\n" is
	// legitimate.
	if args.Content == "" {
		return agent.ValidationError(
			"file_write: missing required `content` parameter. " +
				"To intentionally truncate a file, use `bash` with `: > path`.",
		), nil
	}
	if strings.TrimSpace(args.Description) == "" {
		return agent.ValidationError("file_write: missing required `description` parameter"), nil
	}
	resolved, resolveErr := cwdctx.ResolveFilesystemPath(ctx, args.Path)
	if resolveErr != nil {
		if errors.Is(resolveErr, cwdctx.ErrNoSessionCWD) {
			return agent.ValidationError(
				"file_write: no session working directory is set. Pass an absolute path.",
			), nil
		}
		return agent.ValidationError(fmt.Sprintf("file_write: %v", resolveErr)), nil
	}
	args.Path = resolved

	// Block file_write on the agent's MEMORY.md — always use memory_append.
	// Check unconditionally (not just for existing files) so first-write
	// scenarios also go through the flock-protected bounded-append path.
	if agent.IsMemoryFile(ctx, args.Path) {
		return agent.ToolResult{
			Content: "Cannot write MEMORY.md with file_write — use the memory_append tool instead.",
			IsError: true,
		}, nil
	}

	// Enforce read-before-write for existing files (new files are fine)
	if _, err := os.Stat(args.Path); err == nil {
		if err := agent.CheckReadBeforeWrite(ctx, args.Path); err != nil {
			return agent.ToolResult{Content: err.Error(), IsError: true}, nil
		}
	}

	if err := os.MkdirAll(filepath.Dir(args.Path), 0755); err != nil {
		if os.IsPermission(err) {
			return agent.PermissionError(fmt.Sprintf("cannot create directory %s: permission denied", filepath.Dir(args.Path))), nil
		}
		return agent.ToolResult{Content: fmt.Sprintf("error creating directory: %v", err), IsError: true}, nil
	}

	if err := os.WriteFile(args.Path, []byte(args.Content), 0644); err != nil {
		if os.IsPermission(err) {
			return agent.PermissionError(fmt.Sprintf("cannot write %s: permission denied", args.Path)), nil
		}
		return agent.ToolResult{Content: fmt.Sprintf("error writing file: %v", err), IsError: true}, nil
	}

	return agent.ToolResult{Content: fmt.Sprintf("wrote %d bytes to %s", len(args.Content), args.Path)}, nil
}

func (t *FileWriteTool) RequiresApproval() bool { return true }

func (t *FileWriteTool) IsReadOnlyCall(string) bool { return false }
