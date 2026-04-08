package tools

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/bmatcuk/doublestar/v4"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/cwdctx"
)

const defaultGlobMaxResults = 200

type GlobTool struct{}

type globArgs struct {
	Pattern    string `json:"pattern"`
	Path       string `json:"path,omitempty"`
	MaxResults int    `json:"max_results,omitempty"`
}

func (t *GlobTool) Info() agent.ToolInfo {
	return agent.ToolInfo{
		Name:        "glob",
		Description: "Find files by path pattern (e.g. '**/*.csv', 'reports/*.pdf', 'src/**/*.go'). Matches file NAMES/paths — not file contents. Use grep to search inside files.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"pattern":     map[string]any{"type": "string", "description": "Glob pattern"},
				"path":        map[string]any{"type": "string", "description": "Base directory (default: current dir)"},
				"max_results": map[string]any{"type": "integer", "description": fmt.Sprintf("Max number of results (default: %d)", defaultGlobMaxResults)},
			},
		},
		Required: []string{"pattern"},
	}
}

func (t *GlobTool) Run(ctx context.Context, argsJSON string) (agent.ToolResult, error) {
	var args globArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return agent.ToolResult{Content: fmt.Sprintf("invalid arguments: %v", err), IsError: true}, nil
	}

	root := args.Path
	if root == "" {
		root = "."
	}
	root = cwdctx.ResolvePath(ctx, root)

	pattern := args.Pattern

	// When the model embeds an absolute path in the pattern (e.g.
	// "/Users/hu/projects/repo/{README*,*.md}"), rg --glob and doublestar
	// both expect a relative pattern. Split into root + relative pattern.
	if filepath.IsAbs(pattern) && args.Path == "" {
		splitRoot, splitPat := splitAbsPattern(pattern)
		if splitRoot != "" {
			root = splitRoot
			pattern = splitPat
		}
	}

	maxResults := args.MaxResults
	if maxResults <= 0 {
		maxResults = defaultGlobMaxResults
	}

	var (
		matches []string
		err     error
	)

	if _, lookErr := exec.LookPath("rg"); lookErr == nil {
		matches, err = globWithRg(ctx, root, pattern, maxResults)
	} else {
		matches, err = globFallback(ctx, root, pattern, maxResults)
	}

	if err != nil {
		if ctx.Err() != nil {
			return agent.ToolResult{Content: fmt.Sprintf("glob cancelled: %v", ctx.Err()), IsError: true}, nil
		}
		return agent.ToolResult{Content: fmt.Sprintf("glob error: %v", err), IsError: true}, nil
	}

	if len(matches) == 0 {
		return agent.ToolResult{Content: "no files matched"}, nil
	}

	truncated := false
	if len(matches) > maxResults {
		matches = matches[:maxResults]
		truncated = true
	}

	content := strings.Join(matches, "\n")
	if truncated {
		content += fmt.Sprintf("\n[results truncated at %d; use a more specific pattern or increase max_results]", maxResults)
	}

	return agent.ToolResult{Content: content}, nil
}

// splitAbsPattern splits an absolute glob pattern into (root, relativePattern).
// It finds the deepest directory prefix that contains no glob metacharacters
// and returns it as root, with the remainder as the relative pattern.
//
// Examples:
//
//	"/a/b/c/{*.md,*.go}"   → ("/a/b/c", "{*.md,*.go}")
//	"/a/b/*/README.md"     → ("/a/b", "*/README.md")
//	"/a/b/**/*.go"         → ("/a/b", "**/*.go")
//	"/a/b/c/file.txt"      → ("/a/b/c", "file.txt")
func splitAbsPattern(pattern string) (root, rel string) {
	// Find first glob metacharacter position
	metaIdx := -1
	for i, ch := range pattern {
		if ch == '*' || ch == '?' || ch == '[' || ch == '{' {
			metaIdx = i
			break
		}
	}
	if metaIdx < 0 {
		// No metacharacter — treat as plain path; split at last separator
		return filepath.Dir(pattern), filepath.Base(pattern)
	}
	// Split at the last separator before the first metacharacter
	prefix := pattern[:metaIdx]
	lastSep := strings.LastIndex(prefix, string(filepath.Separator))
	if lastSep <= 0 {
		return "", pattern // can't split meaningfully
	}
	return pattern[:lastSep], pattern[lastSep+1:]
}

// globWithRg uses `rg --files --glob <pattern>` for fast, gitignore-aware, cancellable file discovery.
func globWithRg(ctx context.Context, root, pattern string, maxResults int) ([]string, error) {
	args := []string{
		"--files",
		"--glob", pattern,
		"--hidden",
		"--sort=modified",
		root,
	}
	cmd := exec.CommandContext(ctx, "rg", args...)
	output, err := cmd.Output()

	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			// exit code 1 = no matches, not an error
			return nil, nil
		}
		return nil, err
	}

	var matches []string
	scanner := bufio.NewScanner(bytes.NewReader(output))
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		rel, relErr := filepath.Rel(root, line)
		if relErr != nil {
			rel = line
		}
		matches = append(matches, rel)
		if len(matches) > maxResults {
			// collect one extra to detect truncation, then stop
			break
		}
	}

	return matches, scanner.Err()
}

// errGlobLimit is a sentinel error used to stop GlobWalk once the result cap is reached.
var errGlobLimit = fmt.Errorf("glob result limit reached")

// globFallback uses doublestar.GlobWalk when rg is not available.
// It respects context cancellation and caps results at maxResults+1 for truncation detection.
func globFallback(ctx context.Context, root, pattern string, maxResults int) ([]string, error) {
	fsys := os.DirFS(root)
	var matches []string
	walkErr := doublestar.GlobWalk(fsys, pattern,
		func(path string, d fs.DirEntry) error {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if d.IsDir() {
				return nil
			}
			matches = append(matches, path)
			if len(matches) > maxResults {
				return errGlobLimit
			}
			return nil
		},
		doublestar.WithNoFollow(),
		doublestar.WithNoHidden(),
	)
	if walkErr != nil && walkErr != errGlobLimit {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, walkErr
	}
	return matches, nil
}

func (t *GlobTool) RequiresApproval() bool { return true }

func (t *GlobTool) IsReadOnlyCall(string) bool { return true }

func (t *GlobTool) IsSafeArgs(argsJSON string) bool {
	var args globArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return false
	}
	path := args.Path
	if path == "" {
		path = "."
	}
	return isPathUnderCWD(path)
}

func (t *GlobTool) IsSafeArgsWithContext(ctx context.Context, argsJSON string) bool {
	var args globArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return false
	}
	path := args.Path
	if path == "" {
		path = "."
	}
	return isPathUnderSessionCWD(ctx, path)
}
