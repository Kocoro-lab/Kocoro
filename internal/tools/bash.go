package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/cwdctx"
	"github.com/Kocoro-lab/ShanClaw/internal/skills"
)

// Bash has a bespoke `description` schema (more detailed than the shared
// agent.DescriptionFieldSpec used by other tools) because it landed first
// (PR 4). The wider rollout to file_read / file_write / http / browser /
// process / etc. completed in PR 7 via the shared helper; bash kept its
// original wording to avoid invalidating the prompt cache. Future schema
// cleanup can converge bash onto the shared spec if a cache-rebuild cost
// is acceptable.
type BashTool struct {
	approvalFn        func(command string) bool
	ExtraSafeCommands []string
	CWD               string // working directory for commands; if empty and no session CWD is set, bash runs in an isolated temp dir (NOT the process cwd)
	MaxOutput         int    // max output chars; 0 = use default 30000
	// DefaultTimeoutSecs is the fallback timeout (in seconds) when the
	// per-call `timeout` arg is absent or zero. 0 = use built-in default 120.
	// Wired from config.Tools.BashTimeout by register.go.
	DefaultTimeoutSecs int
	// MaxTimeoutSecs is the hard ceiling for per-call timeout. 0 = use
	// built-in default 600. Wired from config.Tools.BashMaxTimeout by
	// register.go. Clamping is logged so operators can discover when their
	// configured timeout was capped.
	MaxTimeoutSecs int
	// SecretsStore, when set, supplies per-skill API keys as env vars
	// for skills activated via use_skill in the current run. Values are
	// fetched lazily at execution time and scoped to bash child processes
	// only — they never enter prompt context or session transcripts.
	SecretsStore *skills.SecretsStore
}

type bashArgs struct {
	Command string `json:"command"`
	// Description is a short natural-language summary of what the command does,
	// written in the end-user's UI language. Surfaced in approval prompts, tool
	// status cards, and session history. Required in the schema so non-technical
	// users can read every bash invocation; the daemon does not block execution
	// when it's missing (older sessions / safety net), only the UI degrades.
	Description    string `json:"description,omitempty"`
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

macOS Spotlight (` + "`" + `mdfind` + "`" + `): only fall back to it when glob/grep cannot answer the question — metadata searches, bundle-ID lookups, or files outside the session CWD. Naive mdfind can freeze the system (Spotlight may trigger a reindex). Always follow this template:

    ` + "`" + `{ timeout 15 mdfind "keyword1" 2>/dev/null; timeout 15 mdfind "keyword2" 2>/dev/null; } | sort -u | head -100` + "`" + `

- Never pass ` + "`" + `-onlyin /` + "`" + `, ` + "`" + `-onlyin /Users` + "`" + `, ` + "`" + `-onlyin $HOME` + "`" + `, or ` + "`" + `-onlyin ~` + "`" + ` — wide Spotlight scopes are slow and may trigger a reindex. Either omit ` + "`" + `-onlyin` + "`" + ` or scope to a small subdir (e.g., ` + "`" + `-onlyin ~/Documents/project-x` + "`" + `).
- Split OR queries into separate mdfind calls then ` + "`" + `sort -u` + "`" + `; never run ` + "`" + `mdfind "A OR B"` + "`" + ` directly.
- Always pair with ` + "`" + `timeout 15` + "`" + ` and ` + "`" + `| head -100` + "`" + `. If mdfind returns zero results or times out (exit 124), follow up with a focused ` + "`" + `find <subtree> -iname "*keyword*" 2>/dev/null | head -100` + "`" + ` as a separate call — do not chain it via ` + "`" + `||` + "`" + ` in the same pipeline, since ` + "`" + `head` + "`" + ` always exits 0 and the fallback would never fire.

While bash can do similar things, the dedicated tools have better permission handling, output truncation, and result shaping.

Instructions:
- ALWAYS write a clear, short, non-technical "description" (5-15 words) for every bash call. The end user — often non-technical — sees this description, not the command, on approval prompts and history cards. Write in the user's UI language (中文 for Chinese conversations, English for English, etc.). Describe the user-facing GOAL, not the shell syntax. Example: '查找最大的 10 个文件', not 'Run find piped to du and sort'.
- Always quote file paths that contain spaces with double quotes (e.g., cd "path with spaces/file").
- Prefer absolute paths over cd to keep the working directory stable.
- For multi-line Python with embedded quotes or regex, write a script via file_write then run python3 /path/to/script.py — heredoc+quote nesting is a frequent source of shell syntax errors.
- When issuing multiple commands:
  - Multiple bash calls in a single turn execute SEQUENTIALLY, not in parallel — the daemon batches non-read-only tools as size-1 to avoid side-effect collisions (port binds, file writes, etc.). You can still emit multiple bash calls in one response to save round-trips, but do NOT promise the user parallel timing or invent fake "rate limits" / "blocked" / "restricted" explanations when you expected parallel and saw serial. Read the actual tool results — if each bash returned successfully, the operation succeeded.
  - If commands depend on each other, chain with && in a single bash call.
  - Use ';' only when sequential execution is needed and earlier failures don't matter.
  - DO NOT use newlines to separate commands (newlines inside quoted strings are fine).
- For git commands:
  - Prefer creating a new commit over amending an existing commit.
  - Before destructive operations (git reset --hard, git push --force, git checkout --), consider safer alternatives. Only use destructive operations when truly the best approach.
  - Never skip hooks (--no-verify) or bypass signing unless the user explicitly asked. If a hook fails, investigate and fix the underlying issue.
- Do NOT start long-running server processes (python -m http.server, flask run, npm start, vite, etc.). This tool uses CombinedOutput() and blocks until the process closes its pipes, so a server that runs forever will hang the call until timeout. Need local HTTP to serve a file to a browser? Just pass file:///abs/path.html to browser_navigate — the daemon bridges file:// to a loopback HTTP endpoint automatically, no manual server needed.
- Silent commands that consume wall time (sleep, wait, sync, busy loops, network probes) produce no stdout but DO execute. The tool prepends ` + "`" + `[command ran for Ns]` + "`" + ` to any result that took ≥ 1 second, so a result like ` + "`" + `[command ran for 2.0s]\ntask1_done` + "`" + ` is sleep+echo working correctly — the sleep was NOT blocked or skipped. An absence of stdout for a slept command is normal; never fabricate "rate limited" / "blocked" / "skipped" explanations to fill that gap.
- Avoid unnecessary sleep commands:
  - Do not sleep between commands that can run immediately — just run them.
  - Do not retry failing commands in a sleep loop — diagnose the root cause.
  - If polling an external process, use a check command rather than sleeping first.
  - If you must sleep, keep duration short (1-5 seconds).`,
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command": map[string]any{"type": "string", "description": "Shell command to execute"},
				"description": map[string]any{
					"type": "string",
					"description": "REQUIRED. A short (5-15 word) natural-language summary of WHAT this command does, written for a non-technical end user. " +
						"Use the user's UI language (中文 if the user wrote in Chinese, English if they wrote in English, etc.). " +
						"Describe the user-facing INTENT, not the shell syntax. The user will see this — not the command — when approving the call. " +
						"Examples: '查找最大的 10 个文件', 'Commit current changes', '检查 git 状态', 'Install npm dependencies'. " +
						"Do NOT just rephrase the command (e.g. avoid 'Run find with du and sort'); describe the goal in plain language.",
				},
				"timeout":          map[string]any{"type": "integer", "description": "Timeout in seconds (default: 120)"},
				"max_output_chars": map[string]any{"type": "integer", "description": "Maximum output characters to return. Use this for noisy commands."},
			},
		},
		Required: []string{"command", "description"},
	}
}

func (t *BashTool) Run(ctx context.Context, argsJSON string) (agent.ToolResult, error) {
	var args bashArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return agent.ToolResult{Content: fmt.Sprintf("invalid arguments: %v", err), IsError: true}, nil
	}

	// Timeout precedence: per-call args > tool default (from config) > 120s fallback,
	// then hard-capped at MaxTimeoutSecs.
	//
	// Hardcoded-limit policy compliance (CLAUDE.md):
	//   - User workload: 10-min default ceiling covers macOS test suites,
	//     longest legit `brew install`/`xcodebuild clean build`, multi-step
	//     bash scripts. Anything longer is almost always a hung server or
	//     polling loop the model misclassified as foreground work.
	//   - Symptom when it binds: a user's bash command is SIGKILL'd at the
	//     cap and the result carries "[note: process killed after Xs by
	//     context-cancel]". The clamping itself emits a one-shot log line
	//     to stderr ("[bash] requested timeout Xs > cap Ys; clamping").
	//   - Override path: `tools.bash_max_timeout` (seconds) in
	//     ~/.shannon/config.yaml or per-project .shannon/config.yaml. Set
	//     to a higher value for slow integration suites; never 0/unset to
	//     disable (the cap protects UI cards from looking frozen for
	//     unbounded minutes before SIGKILL fires).
	maxBashTimeout := 600 * time.Second
	if t.MaxTimeoutSecs > 0 {
		maxBashTimeout = time.Duration(t.MaxTimeoutSecs) * time.Second
	}
	timeout := 120 * time.Second
	if t.DefaultTimeoutSecs > 0 {
		timeout = time.Duration(t.DefaultTimeoutSecs) * time.Second
	}
	if args.Timeout > 0 {
		timeout = time.Duration(args.Timeout) * time.Second
	}
	if timeout > maxBashTimeout {
		fmt.Fprintf(os.Stderr, "[bash] requested timeout %v > cap %v; clamping (override via tools.bash_max_timeout)\n", timeout, maxBashTimeout)
		timeout = maxBashTimeout
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "sh", "-c", args.Command)
	// Put sh and any children it spawns into a new process group so we can
	// SIGKILL the whole tree on timeout. Without Setpgid, exec's default
	// Cancel kills only sh's PID — long-running grandchildren (e.g.
	// `python -m http.server` backgrounded from sh) survive as orphans
	// and keep ports bound until the user kills them by hand.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return os.ErrProcessDone
	}
	// Cap how long Wait() blocks after Cancel fires. Without WaitDelay, a
	// stuck child that ignores SIGKILL (zombie, uninterruptible sleep) can
	// keep CombinedOutput pinned forever.
	cmd.WaitDelay = 2 * time.Second
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
	start := time.Now()
	output, err := cmd.CombinedOutput()
	elapsed := time.Since(start)

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

	// Prepend elapsed-time annotation when the command consumed meaningful
	// wall time. Gives the model unambiguous evidence that "silent" commands
	// (sleep, wait, sync, network probes) actually executed — without this,
	// models can misperceive an empty-stdout success as "the command was
	// blocked or skipped" and fabricate restrictions to explain it.
	if elapsed >= time.Second {
		result = fmt.Sprintf("[command ran for %.1fs]\n%s", elapsed.Seconds(), result)
	}

	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			timeoutSecs := int(timeout.Seconds())
			return agent.TransientError(fmt.Sprintf("command timed out after %ds\n%s", timeoutSecs, result)), nil
		}
		// ErrWaitDelay fires when the foreground process exited normally but
		// stdout/stderr pipes were still held by a background subprocess
		// (e.g. `python -m http.server &`). The foreground command itself
		// finished — its output is already captured in `result` — so this
		// is not a real failure. Promote to success with a note so the
		// model doesn't mis-read the Go-internal error as a command error.
		if errors.Is(err, exec.ErrWaitDelay) {
			return agent.ToolResult{
				Content: strings.TrimRight(result, "\n") +
					"\n\n[note: bash returned early because a background subprocess kept stdout/stderr open after the foreground command finished. The foreground command itself completed — its output is shown above.]",
			}, nil
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
