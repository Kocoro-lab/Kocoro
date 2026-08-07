package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/cwdctx"
	"github.com/Kocoro-lab/ShanClaw/internal/permissions"
	"github.com/Kocoro-lab/ShanClaw/internal/skills"
)

// Bash keeps a bespoke description field because approval cards require a
// short user-facing goal. Execution policy stays in runtime validation and
// permissions; the provider schema carries only the model decisions needed
// to call the tool correctly.
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
	// ConcurrencyEnabled gates IsConcurrencySafeCall — when true (the
	// Phase C default since 2026-05-15) bash invocations that pass the
	// static read-only analyzer can share a concurrent batch with other
	// tools. When false, the method always returns false so the agent
	// loop's partition dispatcher keeps bash on its historical size-1
	// serial path. Wired from config.AgentConfig.BashConcurrencyEnabled
	// by register.go (RegisterLocalTools + CloneWithRuntimeConfig).
	ConcurrencyEnabled bool
	// LegacyGUIAutomationDisabled is set on daemon run-local registries that
	// expose computer_use. It blocks shell-level GUI input injectors so the
	// model cannot bypass computer_use target binding, leases, verification,
	// or IME-safe text insertion after a semantic/visual path fails.
	LegacyGUIAutomationDisabled bool
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

var legacyGUIAutomationDeniedCommands = []string{
	"osascript",
	"osascript *",
	"*/osascript",
	"*/osascript *",
	"cliclick",
	"cliclick *",
	"*/cliclick",
	"*/cliclick *",
}

func commandMatchesDeniedPatterns(command string, patterns []string) bool {
	decision, reason := permissions.CheckCommand(
		command,
		&permissions.PermissionsConfig{DeniedCommands: patterns},
	)
	return decision == "deny" && strings.Contains(reason, "denied command pattern")
}

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
		Description: `Execute a bounded shell command for scripts, data processing, automation, tests, and system operations.

Each command runs in a fresh shell from the session working directory; cd, exports, and aliases do not persist. Prefer dedicated tools for file reads/writes/edits, path or content search, and directory listing because they have safer permissions and better result shaping.

Provide a short, non-technical description of the user-facing goal in the reply language. Quote paths with spaces and prefer absolute paths. For complex multiline code, write a script with file_write and run it instead of nesting a heredoc. Independent read-only calls may run in parallel; chain dependent commands with &&. Do not bypass hooks or signing, start a long-lived server, use unbounded polling/sleep loops, or perform destructive operations without the required user authority. Runtime validation, approval, timeouts, output caps, GUI-injection denial, and side-effect serialization remain authoritative.`,
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command": map[string]any{"type": "string", "description": "Shell command to execute"},
				"description": map[string]any{
					"type":        "string",
					"description": "Required 5-15 word summary of the user-facing goal, for a non-technical user, in the reply language. Describe the intent rather than shell syntax.",
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
		return agent.ValidationError(fmt.Sprintf("invalid arguments: %v", err)), nil
	}
	if strings.TrimSpace(args.Command) == "" {
		return agent.ValidationError("bash: missing required `command` parameter"), nil
	}
	if strings.TrimSpace(args.Description) == "" {
		return agent.ValidationError("bash: missing required `description` parameter"), nil
	}
	if t.LegacyGUIAutomationDisabled &&
		commandMatchesDeniedPatterns(args.Command, legacyGUIAutomationDeniedCommands) {
		return agent.BusinessError(
			"legacy GUI automation through bash is disabled; use computer_use"), nil
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

	// The bash tool runs POSIX shell syntax via `sh -c`. On Windows `sh` is not
	// present on a stock host (it ships with Git Bash / WSL); surface a clear,
	// actionable error here rather than the cryptic `exec: "sh": executable file
	// not found` the agent would otherwise see on every call. No effect on
	// macOS/Linux, where sh always resolves.
	if runtime.GOOS == "windows" {
		if _, err := exec.LookPath("sh"); err != nil {
			return agent.ToolResult{
				Content: "bash tool requires a POSIX shell (sh) on PATH; on Windows install Git Bash or WSL, or use a different tool",
				IsError: true,
			}, nil
		}
	}

	cmd := exec.CommandContext(ctx, "sh", "-c", args.Command)
	// Put sh and any children it spawns into a new process group so we can
	// kill the whole tree on timeout (platform-specific: POSIX Setpgid+Kill
	// vs Windows taskkill /T). Without it, exec's default Cancel kills only
	// sh's PID and backgrounded grandchildren survive as orphans.
	setBashProcGroup(cmd)
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

// IsConcurrencySafeCall reports whether this specific bash invocation is safe
// to run concurrently with other tool calls. Gated by ConcurrencyEnabled —
// when on (the Phase C default since 2026-05-15), delegates to the pure
// analyzer IsCommandConcurrencySafe. When off, returns false unconditionally
// so the dispatcher matches pre-Phase-A serial behavior.
//
// Parse failures and unknown JSON shapes default to false (fail-closed).
func (t *BashTool) IsConcurrencySafeCall(argsStr string) bool {
	if !t.ConcurrencyEnabled {
		return false
	}
	var args bashArgs
	if err := json.Unmarshal([]byte(argsStr), &args); err != nil {
		return false
	}
	return IsCommandConcurrencySafe(args.Command)
}

// HasMaterialSideEffect reuses the strict read-only static analysis
// (whitelisted leading token, no shell metacharacters) DIRECTLY — not via
// IsConcurrencySafeCall, whose contract is batch scheduling and whose
// ConcurrencyEnabled gate is a batching knob that must not change side-effect
// classification. `git status` / `ls` / `go version` are not material side
// effects regardless of how they are scheduled; anything the analyzer cannot
// prove read-only is.
func (t *BashTool) HasMaterialSideEffect(argsStr string) bool {
	var args bashArgs
	if err := json.Unmarshal([]byte(argsStr), &args); err != nil {
		return true
	}
	return !IsCommandConcurrencySafe(args.Command)
}

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
