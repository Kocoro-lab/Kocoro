package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/cwdctx"
	"github.com/Kocoro-lab/ShanClaw/internal/skills"
)

type BashTool struct {
	approvalFn        func(command string) bool
	ExtraSafeCommands []string
	CWD               string // working directory for commands; if empty and no session CWD is set, bash runs in an isolated temp dir (NOT the process cwd)
	MaxOutput         int    // max output chars; 0 = use default 30000
	// DefaultTimeoutSecs is the fallback timeout (in seconds) when the
	// per-call `timeout` arg is absent or zero. 0 = use built-in default 120.
	// Wired from config.Tools.BashTimeout by register.go.
	DefaultTimeoutSecs int
	// SecretsStore, when set, supplies per-skill API keys as env vars
	// for skills activated via use_skill in the current run. Values are
	// fetched lazily at execution time and scoped to bash child processes
	// only — they never enter prompt context or session transcripts.
	SecretsStore *skills.SecretsStore
}

type bashArgs struct {
	Command        string `json:"command"`
	Timeout        int    `json:"timeout,omitempty"`
	MaxOutputChars int    `json:"max_output_chars,omitempty"`
}

var safeCommands = []string{
	"ls", "pwd", "which", "echo", "cat", "head", "tail", "wc",
	"git status", "git diff", "git log", "git branch", "git show",
	"go build", "go test", "go vet", "go fmt", "go mod",
	"make", "cargo build", "cargo test", "npm test", "npm run",
	"python -m pytest", "python -m py_compile",
}

// shellOperators are characters that chain or redirect commands.
// Any command containing these is never auto-approved.
var shellOperators = []string{"&&", "||", ";", "|", ">", "<", "`", "$(", "${", "&"}

func isSafeCommand(cmd string, extraSafe []string) bool {
	trimmed := strings.TrimSpace(cmd)
	// Reject commands containing shell operators
	for _, op := range shellOperators {
		if strings.Contains(trimmed, op) {
			return false
		}
	}
	for _, safe := range safeCommands {
		if trimmed == safe || strings.HasPrefix(trimmed, safe+" ") {
			return true
		}
	}
	for _, safe := range extraSafe {
		if trimmed == safe || strings.HasPrefix(trimmed, safe+" ") {
			return true
		}
	}
	return false
}

func (t *BashTool) Info() agent.ToolInfo {
	return agent.ToolInfo{
		Name:               "bash",
		MaxResultSizeChars: 30000,
		Description: `Execute a shell command. Use for running scripts, data processing, file management, automation, and system operations.

Each command runs in a fresh shell. The starting directory is the session CWD, but cd/export/aliases from one bash call do NOT persist to later calls.

IMPORTANT: Avoid using this tool to run cat, head, tail, sed, awk, grep, find, or ls commands unless explicitly instructed or after verifying that a dedicated tool cannot accomplish your task. Use the appropriate dedicated tool instead:
- Read files: file_read (NOT cat/head/tail)
- Edit files: file_edit (NOT sed/awk)
- Write files: file_write (NOT echo > / cat <<EOF)
- File search: glob (NOT find)
- Content search: grep (NOT bash grep/rg)
- List directory: directory_list (NOT ls)

While bash can do similar things, the dedicated tools have better permission handling, output truncation, and result shaping.

Instructions:
- Always quote file paths that contain spaces with double quotes (e.g., cd "path with spaces/file").
- Prefer absolute paths over cd to keep the working directory stable.
- For multi-line Python with embedded quotes or regex, write a script via file_write then run python3 /path/to/script.py — heredoc+quote nesting is a frequent source of shell syntax errors.
- When issuing multiple commands:
  - If independent and can run in parallel, make multiple bash tool calls in a single response. Example: "git status" and "git diff" together — send a single response with two bash calls in parallel.
  - If commands depend on each other, chain with && in a single bash call.
  - Use ';' only when sequential execution is needed and earlier failures don't matter.
  - DO NOT use newlines to separate commands (newlines inside quoted strings are fine).
- For git commands:
  - Prefer creating a new commit over amending an existing commit.
  - Before destructive operations (git reset --hard, git push --force, git checkout --), consider safer alternatives. Only use destructive operations when truly the best approach.
  - Never skip hooks (--no-verify) or bypass signing unless the user explicitly asked. If a hook fails, investigate and fix the underlying issue.
- Avoid unnecessary sleep commands:
  - Do not sleep between commands that can run immediately — just run them.
  - Do not retry failing commands in a sleep loop — diagnose the root cause.
  - If polling an external process, use a check command rather than sleeping first.
  - If you must sleep, keep duration short (1-5 seconds).`,
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command":          map[string]any{"type": "string", "description": "Shell command to execute"},
				"timeout":          map[string]any{"type": "integer", "description": "Timeout in seconds (default: 120)"},
				"max_output_chars": map[string]any{"type": "integer", "description": "Maximum output characters to return. Use this for noisy commands."},
			},
		},
		Required: []string{"command"},
	}
}

func (t *BashTool) Run(ctx context.Context, argsJSON string) (agent.ToolResult, error) {
	var args bashArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return agent.ToolResult{Content: fmt.Sprintf("invalid arguments: %v", err), IsError: true}, nil
	}

	// Timeout precedence: per-call args > tool default (from config) > 120s fallback.
	timeout := 120 * time.Second
	if t.DefaultTimeoutSecs > 0 {
		timeout = time.Duration(t.DefaultTimeoutSecs) * time.Second
	}
	if args.Timeout > 0 {
		timeout = time.Duration(args.Timeout) * time.Second
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "sh", "-c", args.Command)
	dir := t.CWD
	if dir == "" {
		dir = cwdctx.FromContext(ctx)
	}
	// When no CWD is set (neither via tool config nor via session context),
	// do NOT let Go's exec package inherit the daemon process's cwd — that
	// would leak the `shan daemon start` directory into every scopeless
	// request. Run in the OS temp dir instead so the command has no
	// project-shaped filesystem around it.
	if dir == "" {
		dir = os.TempDir()
	}
	cmd.Dir = dir
	if envPairs := collectActivatedSkillEnv(ctx, t.SecretsStore); len(envPairs) > 0 {
		cmd.Env = append(os.Environ(), envPairs...)
	}
	output, err := cmd.CombinedOutput()

	result := string(output)
	maxOut := t.MaxOutput
	if maxOut <= 0 {
		maxOut = 30000
	}
	if args.MaxOutputChars > 0 {
		maxOut = args.MaxOutputChars
	}
	if r := []rune(result); len(r) > maxOut {
		keepHead := maxOut * 3 / 4
		keepTail := maxOut / 4
		result = string(r[:keepHead]) + "\n\n[... truncated " +
			strconv.Itoa(len(r)-maxOut) + " chars ...]\n\n" +
			string(r[len(r)-keepTail:])
	}

	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			timeoutSecs := int(timeout.Seconds())
			return agent.TransientError(fmt.Sprintf("command timed out after %ds\n%s", timeoutSecs, result)), nil
		}
		return agent.ToolResult{
			Content: fmt.Sprintf("exit code: %v\n%s", err, result),
			IsError: true,
		}, nil
	}

	return agent.ToolResult{Content: result}, nil
}

func (t *BashTool) RequiresApproval() bool { return true }

func (t *BashTool) IsReadOnlyCall(string) bool { return false }

func (t *BashTool) IsSafe(command string) bool {
	return isSafeCommand(command, t.ExtraSafeCommands)
}

func (t *BashTool) IsSafeArgs(argsJSON string) bool {
	var args bashArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return false
	}
	return isSafeCommand(args.Command, t.ExtraSafeCommands)
}

// collectActivatedSkillEnv returns KEY=VALUE pairs for every secret of every
// skill activated in the current agent run. Returns nil when no skill has
// been activated, no store is configured, or the store has no values.
// Called on every bash execution so newly-activated skills become visible
// to subsequent commands without restart.
func collectActivatedSkillEnv(ctx context.Context, store *skills.SecretsStore) []string {
	if store == nil {
		return nil
	}
	set := skills.ActivatedFromContext(ctx)
	if set == nil {
		return nil
	}
	names := set.Names()
	if len(names) == 0 {
		return nil
	}
	var envPairs []string
	for _, name := range names {
		for k, v := range store.Get(name) {
			envPairs = append(envPairs, k+"="+v)
		}
	}
	return envPairs
}
