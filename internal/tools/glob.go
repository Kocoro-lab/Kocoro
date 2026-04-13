package tools

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
				"path":        map[string]any{"type": "string", "description": "Base directory (default: current dir). Required when no session working directory is set."},
				"max_results": map[string]any{"type": "integer", "description": fmt.Sprintf("Max number of results (default: %d)", defaultGlobMaxResults)},
			},
		},
		Required: []string{"pattern"},
	}
}

// normalizeGlobTarget is the single source of truth for turning (pattern,
// path) into the (root, relPattern) pair that the scanner will actually use.
// Both Run and the approval safe-check call this so they can never see a
// different "real target".
//
// When pattern is absolute and path is empty, the absolute directory prefix
// of pattern is lifted into root. Otherwise (pattern, path) are returned as
// given, with an empty path normalized to ".".
func normalizeGlobTarget(pattern, path string) (root, relPattern string) {
	if filepath.IsAbs(pattern) && path == "" {
		if splitRoot, splitPat := splitAbsPattern(pattern); splitRoot != "" {
			return splitRoot, splitPat
		}
	}
	if path == "" {
		path = "."
	}
	return path, pattern
}

func (t *GlobTool) Run(ctx context.Context, argsJSON string) (agent.ToolResult, error) {
	var args globArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return agent.ToolResult{Content: fmt.Sprintf("invalid arguments: %v", err), IsError: true}, nil
	}

	root, pattern := normalizeGlobTarget(args.Pattern, args.Path)
	resolvedRoot, err := cwdctx.ResolveFilesystemPath(ctx, root)
	if err != nil {
		if errors.Is(err, cwdctx.ErrNoSessionCWD) {
			return agent.ValidationError(
				"glob: no session working directory is set. Pass an absolute 'path' argument (e.g. /Users/you/project) or an absolute glob pattern.",
			), nil
		}
		return agent.ValidationError(fmt.Sprintf("glob: %v", err)), nil
	}
	root = resolvedRoot

	maxResults := args.MaxResults
	if maxResults <= 0 {
		maxResults = defaultGlobMaxResults
	}

	var matches []string

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
	metaIdx := -1
	for i, ch := range pattern {
		if ch == '*' || ch == '?' || ch == '[' || ch == '{' {
			metaIdx = i
			break
		}
	}
	if metaIdx < 0 {
		return filepath.Dir(pattern), filepath.Base(pattern)
	}
	prefix := pattern[:metaIdx]
	lastSep := strings.LastIndex(prefix, string(filepath.Separator))
	switch {
	case lastSep < 0:
		return "", pattern
	case lastSep == 0:
		// Pattern like "/{a,b}" or "/*.go" — root is the filesystem root.
		return string(filepath.Separator), pattern[1:]
	default:
		return pattern[:lastSep], pattern[lastSep+1:]
	}
}

// globWithRg uses `rg --files --glob <pattern>` for fast, gitignore-aware,
// cancellable file discovery.
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
			break
		}
	}

	return matches, scanner.Err()
}

// errGlobLimit is a sentinel used to stop GlobWalk once the result cap is reached.
var errGlobLimit = fmt.Errorf("glob result limit reached")

// globFallback uses doublestar.GlobWalk when rg is not available. Respects
// ctx cancellation and caps results at maxResults+1 for truncation detection.
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
	)
	if walkErr != nil && !errors.Is(walkErr, errGlobLimit) {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, walkErr
	}
	return matches, nil
}

func (t *GlobTool) RequiresApproval() bool { return true }

func (t *GlobTool) IsReadOnlyCall(string) bool { return true }

// IsSafeArgs and IsSafeArgsWithContext must inspect the SAME (root, pattern)
// pair that Run will actually scan. If they only looked at args.Path, a caller
// could smuggle an absolute directory into args.Pattern (e.g.
// {"pattern":"/etc/*.conf"}), pass the safe-check with path="." ("safe,
// under session CWD"), and then have Run split the pattern and scan the
// smuggled root. Both entry points run through normalizeGlobTarget so there
// is exactly one notion of "real target".
func (t *GlobTool) IsSafeArgs(argsJSON string) bool {
	var args globArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return false
	}
	root, _ := normalizeGlobTarget(args.Pattern, args.Path)
	return isPathUnderCWD(root)
}

func (t *GlobTool) IsSafeArgsWithContext(ctx context.Context, argsJSON string) bool {
	var args globArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return false
	}
	root, _ := normalizeGlobTarget(args.Pattern, args.Path)
	return isPathUnderSessionCWD(ctx, root)
}
