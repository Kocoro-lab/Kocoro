package tools

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/cwdctx"
)

const (
	// defaultGrepMaxResults is the single source of truth for the global cap on
	// grep output lines. Bumped 250 → 500 because "find all TODOs in this repo"
	// routinely returns 400-800 hits and 250 was silently truncating. Callers
	// can override per-invocation via the `max_results` tool argument; there is
	// no viper key (the previous tools.grep_max_results default was never read
	// by this tool and was removed for clarity).
	defaultGrepMaxResults = 500
	grepPerFileMaxCount   = 50
	grepTimeout           = 30 * time.Second
)

var grepVCSDirs = []string{".git", ".svn", ".hg", ".bzr", ".jj", ".sl"}

type GrepTool struct{}

type grepArgs struct {
	Pattern       string `json:"pattern"`
	Description   string `json:"description,omitempty"`
	Path          string `json:"path,omitempty"`
	Glob          string `json:"glob,omitempty"`
	OutputMode    string `json:"output_mode,omitempty"` // "files_with_matches" (default), "content", "count"
	MaxResults    int    `json:"max_results,omitempty"`
	Type          string `json:"type,omitempty"`
	HeadLimit     int    `json:"head_limit,omitempty"`
	Offset        int    `json:"offset,omitempty"`
	Context       int    `json:"context,omitempty"`
	BeforeContext int    `json:"before_context,omitempty"`
	AfterContext  int    `json:"after_context,omitempty"`
	IgnoreCase    bool   `json:"ignore_case,omitempty"`
	Multiline     bool   `json:"multiline,omitempty"`
}

func (t *GrepTool) Info() agent.ToolInfo {
	return agent.ToolInfo{
		Name:               "grep",
		MaxResultSizeChars: 20000,
		Description: "Search file CONTENTS using a regex pattern. By default returns matching FILE PATHS only (output_mode=files_with_matches) — keeps results small. Set output_mode=content to get matching lines as file:line:text, or output_mode=count for per-file match counts. Use glob to filter files by name pattern." +
			agent.DescriptionGuidance,
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"pattern":     map[string]any{"type": "string", "description": "Regex pattern to search"},
				"description": agent.DescriptionFieldSpec,
				"path":        map[string]any{"type": "string", "description": "Directory or file to search. Required when no session working directory is set."},
				"glob":    map[string]any{"type": "string", "description": "File glob filter (e.g. '*.csv', '*.txt', '*.go'). Only honored with rg; ignored on grep fallback."},
				"output_mode": map[string]any{
					"type":        "string",
					"enum":        []string{"files_with_matches", "content", "count"},
					"description": "files_with_matches (default): paths only. content: file:line:text — use when you need to read match context. count: per-file match counts.",
				},
				"max_results":    map[string]any{"type": "integer", "description": fmt.Sprintf("Global cap on output lines (default: %d). In files_with_matches mode caps file paths; in content mode caps match lines; in count mode caps file:count entries.", defaultGrepMaxResults)},
				"type":           map[string]any{"type": "string", "description": "ripgrep file type filter, e.g. go, js, ts, py. Requires rg."},
				"head_limit":     map[string]any{"type": "integer", "description": "Return only this many output lines after offset."},
				"offset":         map[string]any{"type": "integer", "description": "Skip this many output lines before returning results."},
				"context":        map[string]any{"type": "integer", "description": "Include N lines before and after each content match."},
				"before_context": map[string]any{"type": "integer", "description": "Include N lines before each content match."},
				"after_context":  map[string]any{"type": "integer", "description": "Include N lines after each content match."},
				"ignore_case":    map[string]any{"type": "boolean", "description": "Case-insensitive search."},
				"multiline":      map[string]any{"type": "boolean", "description": "Allow multiline regex matching. Requires rg."},
			},
		},
		Required: []string{"pattern", "description"},
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

	mode := args.OutputMode
	if mode == "" {
		mode = "files_with_matches"
	}
	if mode != "files_with_matches" && mode != "content" && mode != "count" {
		return agent.ValidationError(fmt.Sprintf("invalid output_mode %q: must be 'files_with_matches', 'content', or 'count'", mode)), nil
	}

	maxResults := args.MaxResults
	if maxResults <= 0 {
		maxResults = defaultGrepMaxResults
	}

	// Derive a timeout-bounded context so a runaway search cannot hang the
	// agent loop. 30s is enough for rg to scan a reasonable project; anything
	// longer points at scanning too broad a root, which should fail loudly.
	runCtx, cancel := context.WithTimeout(ctx, grepTimeout)
	defer cancel()

	// Pick binary first so per-mode flags can branch on it. grep fallback
	// loses --glob support (GNU/BSD grep don't accept it); document this in
	// the schema rather than emulate via --include.
	bin := "rg"
	if _, err := exec.LookPath("rg"); err != nil {
		bin = "grep"
	}

	// Per-mode cmd args. Per-file --max-count caps massive single files
	// before the global line cap below; only relevant in content mode.
	var cmdArgs []string
	if bin == "rg" {
		cmdArgs = append(cmdArgs, "--hidden")
		for _, dir := range grepVCSDirs {
			cmdArgs = append(cmdArgs, "--glob", "!"+dir, "--glob", "!"+dir+"/**", "--glob", "!**/"+dir+"/**")
		}
		// Prevent minified/base64 lines from dominating tool output.
		cmdArgs = append(cmdArgs, "--max-columns", "500")
	}
	switch mode {
	case "files_with_matches":
		if bin == "rg" {
			cmdArgs = append(cmdArgs, "-l")
		} else {
			cmdArgs = append(cmdArgs, "-rl", "-I")
		}
	case "content":
		if bin == "rg" {
			cmdArgs = append(cmdArgs, "-n", "--max-count", fmt.Sprintf("%d", grepPerFileMaxCount))
		} else {
			cmdArgs = append(cmdArgs, "-rn", "-I", "-m", fmt.Sprintf("%d", grepPerFileMaxCount))
		}
	case "count":
		if bin == "rg" {
			cmdArgs = append(cmdArgs, "-c")
		} else {
			cmdArgs = append(cmdArgs, "-rc", "-I")
		}
	}
	if args.Glob != "" && bin == "rg" {
		for _, pattern := range splitGrepGlobPatterns(args.Glob) {
			cmdArgs = append(cmdArgs, "--glob", pattern)
		}
	}
	if args.IgnoreCase {
		cmdArgs = append(cmdArgs, "-i")
	}
	if args.Context > 0 {
		cmdArgs = append(cmdArgs, "-C", strconv.Itoa(args.Context))
	}
	if args.BeforeContext > 0 {
		cmdArgs = append(cmdArgs, "-B", strconv.Itoa(args.BeforeContext))
	}
	if args.AfterContext > 0 {
		cmdArgs = append(cmdArgs, "-A", strconv.Itoa(args.AfterContext))
	}
	if args.Type != "" && bin == "rg" {
		cmdArgs = append(cmdArgs, "--type", args.Type)
	}
	if args.Multiline && bin == "rg" {
		cmdArgs = append(cmdArgs, "-U", "--multiline-dotall")
	}
	if bin == "rg" && strings.HasPrefix(args.Pattern, "-") {
		cmdArgs = append(cmdArgs, "-e", args.Pattern)
	} else {
		cmdArgs = append(cmdArgs, args.Pattern)
	}
	cmdArgs = append(cmdArgs, path)

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
		scanner    = bufio.NewScanner(bytes.NewReader(output))
		scanBuffer = make([]byte, 0, 64*1024)
	)
	scanner.Buffer(scanBuffer, 1024*1024) // handle long lines up to 1 MiB
	var lines []string
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		lines = append(lines, line)
	}
	if err := scanner.Err(); err != nil {
		return agent.ToolResult{Content: fmt.Sprintf("grep output scan error: %v", err), IsError: true}, nil
	}
	if len(lines) == 0 {
		return agent.ToolResult{Content: "no matches found"}, nil
	}
	if mode == "files_with_matches" {
		sortGrepFilesByMTime(lines, path)
	}

	offset := args.Offset
	if offset < 0 {
		offset = 0
	}
	limit := maxResults
	if args.HeadLimit > 0 && args.HeadLimit < limit {
		limit = args.HeadLimit
	}
	capped, truncated := windowGrepLines(lines, offset, limit)
	for i, line := range capped {
		capped[i] = relativizeGrepOutputLine(ctx, path, mode, line)
	}

	content := strings.Join(capped, "\n")
	if truncated {
		unit := "matches"
		switch mode {
		case "files_with_matches":
			unit = "files"
		case "count":
			unit = "files"
		}
		content += fmt.Sprintf("\n[results truncated at %d of %d %s; narrow the search with a more specific pattern or path]", limit, len(lines), unit)
	}

	return agent.ToolResult{Content: content}, nil
}

func splitGrepGlobPatterns(glob string) []string {
	var out []string
	for _, raw := range strings.Fields(glob) {
		if strings.Contains(raw, "{") && strings.Contains(raw, "}") {
			out = append(out, raw)
			continue
		}
		for _, part := range strings.Split(raw, ",") {
			part = strings.TrimSpace(part)
			if part != "" {
				out = append(out, part)
			}
		}
	}
	return out
}

func windowGrepLines(lines []string, offset, limit int) ([]string, bool) {
	if offset < 0 {
		offset = 0
	}
	if offset >= len(lines) {
		return nil, false
	}
	start := offset
	end := len(lines)
	if limit > 0 && start+limit < end {
		end = start + limit
	}
	return lines[start:end], end < len(lines)
}

func sortGrepFilesByMTime(lines []string, searchRoot string) {
	type entry struct {
		line  string
		mtime time.Time
	}
	entries := make([]entry, len(lines))
	for i, line := range lines {
		path := line
		if !filepath.IsAbs(path) {
			path = filepath.Join(searchRoot, path)
		}
		if info, err := os.Stat(path); err == nil {
			entries[i] = entry{line: line, mtime: info.ModTime()}
		} else {
			entries[i] = entry{line: line}
		}
	}
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].mtime.Equal(entries[j].mtime) {
			return entries[i].line < entries[j].line
		}
		return entries[i].mtime.After(entries[j].mtime)
	})
	for i := range entries {
		lines[i] = entries[i].line
	}
}

func relativizeGrepOutputLine(ctx context.Context, searchRoot, mode, line string) string {
	switch mode {
	case "files_with_matches":
		return relativizeGrepPath(ctx, searchRoot, line)
	case "content":
		path, rest, ok := splitGrepContentLine(line)
		if !ok {
			return line
		}
		return relativizeGrepPath(ctx, searchRoot, path) + rest
	case "count":
		idx := strings.LastIndex(line, ":")
		if idx <= 0 {
			return line
		}
		return relativizeGrepPath(ctx, searchRoot, line[:idx]) + line[idx:]
	default:
		return line
	}
}

func splitGrepContentLine(line string) (path, rest string, ok bool) {
	for i := 0; i < len(line); i++ {
		if line[i] != ':' && line[i] != '-' {
			continue
		}
		j := i + 1
		if j >= len(line) || line[j] < '0' || line[j] > '9' {
			continue
		}
		for j < len(line) && line[j] >= '0' && line[j] <= '9' {
			j++
		}
		if j < len(line) && (line[j] == ':' || line[j] == '-') {
			return line[:i], line[i:], true
		}
	}
	return "", "", false
}

func relativizeGrepPath(ctx context.Context, searchRoot, path string) string {
	if path == "" {
		return path
	}
	if !filepath.IsAbs(path) {
		return filepath.ToSlash(filepath.Clean(path))
	}
	base := cwdctx.FromContext(ctx)
	if base == "" {
		base = searchRoot
		if info, err := os.Stat(base); err == nil && !info.IsDir() {
			base = filepath.Dir(base)
		}
	}
	if rel, err := filepath.Rel(base, path); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		if rel == "." {
			return filepath.ToSlash(filepath.Base(path))
		}
		return filepath.ToSlash(rel)
	}
	return filepath.ToSlash(path)
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
