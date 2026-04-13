package tools

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/cwdctx"
)

const (
	defaultGrepMaxResults = 250
	grepPerFileMaxCount   = 50
	grepTimeout           = 30 * time.Second
)

type GrepTool struct{}

type grepArgs struct {
	Pattern    string `json:"pattern"`
	Path       string `json:"path,omitempty"`
	Glob       string `json:"glob,omitempty"`
	MaxResults int    `json:"max_results,omitempty"`
}

func (t *GrepTool) Info() agent.ToolInfo {
	return agent.ToolInfo{
		Name:        "grep",
		Description: "Search file CONTENTS using a regex pattern. Returns matching lines with filenames and line numbers. Use glob to find files by name pattern instead.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"pattern":     map[string]any{"type": "string", "description": "Regex pattern to search"},
				"path":        map[string]any{"type": "string", "description": "Directory or file to search. Required when no session working directory is set."},
				"glob":        map[string]any{"type": "string", "description": "File glob filter (e.g. '*.csv', '*.txt', '*.go')"},
				"max_results": map[string]any{"type": "integer", "description": fmt.Sprintf("Global cap on total match lines returned (default: %d)", defaultGrepMaxResults)},
			},
		},
		Required: []string{"pattern"},
	}
}

func (t *GrepTool) Run(ctx context.Context, argsJSON string) (agent.ToolResult, error) {
	var args grepArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return agent.ToolResult{Content: fmt.Sprintf("invalid arguments: %v", err), IsError: true}, nil
	}

	path := args.Path
	if path == "" {
		path = "."
	}
	resolved, err := cwdctx.ResolveFilesystemPath(ctx, path)
	if err != nil {
		if errors.Is(err, cwdctx.ErrNoSessionCWD) {
			return agent.ValidationError(
				"grep: no session working directory is set. Pass an absolute 'path' argument (e.g. /Users/you/project).",
			), nil
		}
		return agent.ValidationError(fmt.Sprintf("grep: %v", err)), nil
	}
	path = resolved

	maxResults := args.MaxResults
	if maxResults <= 0 {
		maxResults = defaultGrepMaxResults
	}

	// Derive a timeout-bounded context so a runaway search cannot hang the
	// agent loop. 30s is enough for rg to scan a reasonable project; anything
	// longer points at scanning too broad a root, which should fail loudly.
	runCtx, cancel := context.WithTimeout(ctx, grepTimeout)
	defer cancel()

	// Keep rg's per-file --max-count low so a single massive file doesn't
	// balloon the output buffer. The real global cap is applied during
	// output parsing below.
	cmdArgs := []string{
		"-n",
		"--max-count", fmt.Sprintf("%d", grepPerFileMaxCount),
	}
	if args.Glob != "" {
		cmdArgs = append(cmdArgs, "--glob", args.Glob)
	}
	cmdArgs = append(cmdArgs, args.Pattern, path)

	bin := "rg"
	if _, err := exec.LookPath("rg"); err != nil {
		bin = "grep"
		// -m caps matches per file; without it a pathological input could
		// have CombinedOutput() buffer tens of MB before the scanner cap
		// kicks in below. Both GNU and BSD grep accept -m.
		cmdArgs = []string{"-rn", "-I", "-m", fmt.Sprintf("%d", grepPerFileMaxCount), args.Pattern, path}
	}

	cmd := exec.CommandContext(runCtx, bin, cmdArgs...)
	output, cmdErr := cmd.CombinedOutput()
	result := string(output)

	if cmdErr != nil {
		if runCtx.Err() == context.DeadlineExceeded {
			return agent.ToolResult{
				Content: fmt.Sprintf("grep timed out after %s scanning %s. Narrow the search with a more specific path or glob filter.", grepTimeout, path),
				IsError: true,
			}, nil
		}
		if runCtx.Err() != nil {
			return agent.ToolResult{Content: fmt.Sprintf("grep cancelled: %v", runCtx.Err()), IsError: true}, nil
		}
		if exitErr, ok := cmdErr.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			return agent.ToolResult{Content: "no matches found"}, nil
		}
		// Exit code 2 in rg/grep covers multiple failure modes: bad regex,
		// missing paths, permission errors, etc. Classify by stderr content.
		lower := strings.ToLower(result)
		switch {
		case strings.Contains(lower, "regex") || strings.Contains(lower, "syntax") || strings.Contains(lower, "parse error"):
			return agent.ValidationError(fmt.Sprintf("invalid regex pattern: %s", result)), nil
		case strings.Contains(lower, "permission denied"):
			return agent.PermissionError(fmt.Sprintf("grep: %s", result)), nil
		case strings.Contains(lower, "no such file") || strings.Contains(lower, "not found"):
			return agent.ValidationError(fmt.Sprintf("path not found: %s", result)), nil
		default:
			return agent.ToolResult{Content: fmt.Sprintf("grep error: %v\n%s", cmdErr, result), IsError: true}, nil
		}
	}

	// Apply global line cap by scanning output line-by-line. This is the real
	// defense against a search that matches thousands of files and would
	// otherwise dump megabytes of lines into agent context.
	var (
		capped     []string
		total      int
		truncated  bool
		scanner    = bufio.NewScanner(bytes.NewReader(output))
		scanBuffer = make([]byte, 0, 64*1024)
	)
	scanner.Buffer(scanBuffer, 1024*1024) // handle long lines up to 1 MiB
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		total++
		if total > maxResults {
			truncated = true
			continue
		}
		capped = append(capped, line)
	}

	if total == 0 {
		return agent.ToolResult{Content: "no matches found"}, nil
	}

	content := strings.Join(capped, "\n")
	if truncated {
		content += fmt.Sprintf("\n[results truncated at %d of %d matches; narrow the search with a more specific pattern or path]", maxResults, total)
	}

	return agent.ToolResult{Content: content}, nil
}

func (t *GrepTool) RequiresApproval() bool { return true }

func (t *GrepTool) IsReadOnlyCall(string) bool { return true }

func (t *GrepTool) IsSafeArgs(argsJSON string) bool {
	var args grepArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return false
	}
	path := args.Path
	if path == "" {
		path = "."
	}
	return isPathUnderCWD(path)
}

func (t *GrepTool) IsSafeArgsWithContext(ctx context.Context, argsJSON string) bool {
	var args grepArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return false
	}
	path := args.Path
	if path == "" {
		path = "."
	}
	return isPathUnderSessionCWD(ctx, path)
}
