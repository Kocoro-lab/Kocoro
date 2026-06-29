package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

type HookEvent string

const (
	PreToolUse   HookEvent = "PreToolUse"
	PostToolUse  HookEvent = "PostToolUse"
	SessionStart HookEvent = "SessionStart"
	Stop         HookEvent = "Stop"
)

const (
	defaultTimeout = 10 * time.Second
	maxOutputBytes = 10 * 1024 // 10KB
	exitCodeDeny   = 2
)

// HookConfig uses PascalCase event names in both YAML and JSON to match the
// established schema (e.g. "PreToolUse" in config.yaml). This is intentionally
// inconsistent with the snake_case used by other config fields; clients doing a
// GET /config → PATCH /config round-trip must preserve these PascalCase keys.
type HookConfig struct {
	PreToolUse   []HookEntry `yaml:"PreToolUse"   json:"PreToolUse"`
	PostToolUse  []HookEntry `yaml:"PostToolUse"  json:"PostToolUse"`
	SessionStart []HookEntry `yaml:"SessionStart" json:"SessionStart"`
	Stop         []HookEntry `yaml:"Stop"         json:"Stop"`
}

type HookEntry struct {
	Matcher string `yaml:"matcher" json:"matcher"`
	Command string `yaml:"command" json:"command"`
}

type HookInput struct {
	Event        HookEvent       `json:"event"`
	ToolName     string          `json:"tool_name,omitempty"`
	ToolInput    json.RawMessage `json:"tool_input"`
	ToolResponse json.RawMessage `json:"tool_response"`
	SessionID    string          `json:"session_id"`
}

type HookRunner struct {
	config      HookConfig
	timeout     time.Duration
	allowedDirs []string // additional allowed absolute path prefixes (for testing)

	mu     sync.Mutex
	inHook bool // recursion guard
}

func NewHookRunner(config HookConfig) *HookRunner {
	return &HookRunner{
		config:  config,
		timeout: defaultTimeout,
	}
}

// RunPreToolUse runs matching PreToolUse hooks.
// Returns: decision ("allow"/"deny"/""), reason string, error.
// If any hook exits with code 2, returns "deny" with stderr as reason.
// If any hook exits with non-zero (not 2), logs warning but doesn't block.
func (h *HookRunner) RunPreToolUse(ctx context.Context, toolName string, toolInput string, sessionID string) (string, string, error) {
	if h == nil {
		return "", "", nil
	}
	if h.enterHook() {
		return "", "", nil // skip recursive invocations
	}
	defer h.exitHook()

	entries := h.matchEntries(h.config.PreToolUse, toolName)
	if len(entries) == 0 {
		return "", "", nil
	}

	input := HookInput{
		Event:        PreToolUse,
		ToolName:     toolName,
		ToolInput:    toRawJSON(toolInput),
		ToolResponse: nullJSON(),
		SessionID:    sessionID,
	}

	for _, entry := range entries {
		exitCode, _, stderr, err := h.runHook(ctx, entry, input)
		if err != nil {
			log.Printf("[hooks] warning: PreToolUse hook %q failed: %v", entry.Command, err)
			continue
		}
		if exitCode == exitCodeDeny {
			reason := strings.TrimSpace(stderr)
			if reason == "" {
				reason = "blocked by hook"
			}
			return "deny", reason, nil
		}
		if exitCode != 0 {
			log.Printf("[hooks] warning: PreToolUse hook %q exited with code %d: %s", entry.Command, exitCode, strings.TrimSpace(stderr))
		}
	}

	return "allow", "", nil
}

// RunPostToolUse runs matching PostToolUse hooks (fire-and-forget, errors logged).
func (h *HookRunner) RunPostToolUse(ctx context.Context, toolName string, toolInput string, toolResponse string, sessionID string) error {
	if h == nil {
		return nil
	}
	if h.enterHook() {
		return nil
	}
	defer h.exitHook()

	entries := h.matchEntries(h.config.PostToolUse, toolName)
	if len(entries) == 0 {
		return nil
	}

	input := HookInput{
		Event:        PostToolUse,
		ToolName:     toolName,
		ToolInput:    toRawJSON(toolInput),
		ToolResponse: toRawJSON(toolResponse),
		SessionID:    sessionID,
	}

	for _, entry := range entries {
		exitCode, _, stderr, err := h.runHook(ctx, entry, input)
		if err != nil {
			log.Printf("[hooks] warning: PostToolUse hook %q failed: %v", entry.Command, err)
			continue
		}
		if exitCode != 0 {
			log.Printf("[hooks] warning: PostToolUse hook %q exited with code %d: %s", entry.Command, exitCode, strings.TrimSpace(stderr))
		}
	}

	return nil
}

// RunSessionStart runs all SessionStart hooks.
func (h *HookRunner) RunSessionStart(ctx context.Context, sessionID string) error {
	if h == nil {
		return nil
	}
	if h.enterHook() {
		return nil
	}
	defer h.exitHook()

	if len(h.config.SessionStart) == 0 {
		return nil
	}

	input := HookInput{
		Event:        SessionStart,
		ToolInput:    nullJSON(),
		ToolResponse: nullJSON(),
		SessionID:    sessionID,
	}

	for _, entry := range h.config.SessionStart {
		exitCode, _, stderr, err := h.runHook(ctx, entry, input)
		if err != nil {
			log.Printf("[hooks] warning: SessionStart hook %q failed: %v", entry.Command, err)
			continue
		}
		if exitCode != 0 {
			log.Printf("[hooks] warning: SessionStart hook %q exited with code %d: %s", entry.Command, exitCode, strings.TrimSpace(stderr))
		}
	}

	return nil
}

// RunStop runs all Stop hooks.
func (h *HookRunner) RunStop(ctx context.Context, sessionID string) error {
	if h == nil {
		return nil
	}
	if h.enterHook() {
		return nil
	}
	defer h.exitHook()

	if len(h.config.Stop) == 0 {
		return nil
	}

	input := HookInput{
		Event:        Stop,
		ToolInput:    nullJSON(),
		ToolResponse: nullJSON(),
		SessionID:    sessionID,
	}

	for _, entry := range h.config.Stop {
		exitCode, _, stderr, err := h.runHook(ctx, entry, input)
		if err != nil {
			log.Printf("[hooks] warning: Stop hook %q failed: %v", entry.Command, err)
			continue
		}
		if exitCode != 0 {
			log.Printf("[hooks] warning: Stop hook %q exited with code %d: %s", entry.Command, exitCode, strings.TrimSpace(stderr))
		}
	}

	return nil
}

func (h *HookRunner) runHook(ctx context.Context, entry HookEntry, input HookInput) (exitCode int, stdout string, stderr string, err error) {
	cmdPath, err := resolveCommand(entry.Command, h.allowedDirs)
	if err != nil {
		return -1, "", "", err
	}

	inputJSON, err := json.Marshal(input)
	if err != nil {
		return -1, "", "", fmt.Errorf("failed to marshal hook input: %w", err)
	}

	hookCtx, cancel := context.WithTimeout(ctx, h.timeout)
	defer cancel()

	cmd := exec.CommandContext(hookCtx, cmdPath)
	cmd.Stdin = bytes.NewReader(inputJSON)

	// Create a new process group so we can kill the entire tree on timeout
	// (platform-specific: POSIX Setpgid+Kill vs Windows taskkill /T).
	setProcGroupKill(cmd)

	var stdoutBuf, stderrBuf bytes.Buffer
	cmd.Stdout = &limitedWriter{buf: &stdoutBuf, limit: maxOutputBytes}
	cmd.Stderr = &limitedWriter{buf: &stderrBuf, limit: maxOutputBytes}

	if runErr := cmd.Run(); runErr != nil {
		if hookCtx.Err() == context.DeadlineExceeded {
			return -1, stdoutBuf.String(), stderrBuf.String(), fmt.Errorf("hook timed out after %v", h.timeout)
		}
		if exitErr, ok := runErr.(*exec.ExitError); ok {
			return exitErr.ExitCode(), stdoutBuf.String(), stderrBuf.String(), nil
		}
		return -1, stdoutBuf.String(), stderrBuf.String(), runErr
	}

	return 0, stdoutBuf.String(), stderrBuf.String(), nil
}

// matchEntries returns hook entries whose matcher regex matches the tool name.
// An empty matcher matches all tools.
func (h *HookRunner) matchEntries(entries []HookEntry, toolName string) []HookEntry {
	var matched []HookEntry
	for _, e := range entries {
		if e.Matcher == "" {
			matched = append(matched, e)
			continue
		}
		re, err := regexp.Compile(e.Matcher)
		if err != nil {
			log.Printf("[hooks] warning: invalid matcher regex %q: %v", e.Matcher, err)
			continue
		}
		if re.MatchString(toolName) {
			matched = append(matched, e)
		}
	}
	return matched
}

// resolveCommand validates and resolves the command path.
// Allowed: relative paths (starting with . or no / prefix), or paths under ~/.shannon/.
// Additional directories can be allowed via extraAllowed (used for testing).
// Rejected: absolute paths outside allowed directories.
func resolveCommand(command string, extraAllowed []string) (string, error) {
	if command == "" {
		return "", fmt.Errorf("hook command must not be empty")
	}

	// Expand ~ prefix
	if strings.HasPrefix(command, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("failed to resolve home directory: %w", err)
		}
		command = filepath.Join(home, command[2:])
	}

	// Reject bare command names that would be resolved via PATH
	if !filepath.IsAbs(command) && !strings.HasPrefix(command, "./") && !strings.HasPrefix(command, "../") && !strings.Contains(command, string(filepath.Separator)) {
		return "", fmt.Errorf("bare command %q rejected: use absolute path or ./ prefix", command)
	}

	// If it's an absolute path, it must be under ~/.shannon/ or an extra allowed dir
	if filepath.IsAbs(command) {
		shannonDir := shannonDirPath()
		allowed := false

		if shannonDir != "" && (strings.HasPrefix(command, shannonDir+string(filepath.Separator)) || command == shannonDir) {
			allowed = true
		}
		for _, dir := range extraAllowed {
			if strings.HasPrefix(command, dir+string(filepath.Separator)) || command == dir {
				allowed = true
				break
			}
		}

		if !allowed {
			target := shannonDir
			if target == "" {
				target = "~/.shannon"
			}
			return "", fmt.Errorf("absolute path %q rejected: must be under %s", command, target)
		}
	}

	return command, nil
}

func shannonDirPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".shannon")
}

// enterHook attempts to set the recursion guard. Returns true if already inside a hook.
func (h *HookRunner) enterHook() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.inHook {
		return true
	}
	h.inHook = true
	return false
}

func (h *HookRunner) exitHook() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.inHook = false
}

// toRawJSON converts a string to json.RawMessage.
// If the string is valid JSON, it's used as-is. Otherwise it's marshaled as a JSON string.
func toRawJSON(s string) json.RawMessage {
	if s == "" {
		return json.RawMessage("null")
	}
	if json.Valid([]byte(s)) {
		return json.RawMessage(s)
	}
	data, err := json.Marshal(s)
	if err != nil {
		return json.RawMessage("null")
	}
	return data
}

func nullJSON() json.RawMessage {
	return json.RawMessage("null")
}

// limitedWriter wraps a bytes.Buffer and stops writing after limit bytes.
type limitedWriter struct {
	buf   *bytes.Buffer
	limit int
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	totalLen := len(p)
	remaining := w.limit - w.buf.Len()
	if remaining <= 0 {
		return totalLen, nil // discard but report success
	}
	toWrite := p
	if len(toWrite) > remaining {
		toWrite = toWrite[:remaining]
	}
	if _, err := w.buf.Write(toWrite); err != nil {
		return 0, err
	}
	// Report all bytes as written even if we truncated, so exec doesn't fail
	return totalLen, nil
}

// Ensure limitedWriter satisfies io.Writer.
var _ io.Writer = (*limitedWriter)(nil)
