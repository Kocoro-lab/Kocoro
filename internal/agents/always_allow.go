package agents

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"

	"github.com/Kocoro-lab/ShanClaw/internal/fslock"
	"gopkg.in/yaml.v3"
)

// ErrToolNotPersistable is returned when a tool cannot be persisted to a
// per-agent always-allow list. The list of non-persistable tools is currently
// empty (see highRiskTools below); this error and the surrounding plumbing
// are preserved so a future genuinely-irreversible tool (account deletion,
// payment authorization) can be added without rewiring callers.
var ErrToolNotPersistable = errors.New("tool cannot be persisted as always-allow")

// highRiskTools mirrors internal/agent/tools.go autoApprovalDenyList to avoid
// a cross-package dependency from agents into agent (would form a cycle via
// instructions). Drift between the two is guarded by TestHighRiskListConsistency
// in internal/agent/tools_test.go.
//
// As of 2026-05-18 this list is empty — see autoApprovalDenyList for the
// product-decision context. publish_to_web, generate_image, and edit_image
// now can be persisted as "always allow" once the user opts in; runtime
// approval still prompts the first time because their RequiresApproval()
// returns true.
var highRiskTools = []string{}

// HighRiskTools returns a copy of the tools that cannot be persisted to a
// per-agent always-allow list. Exposed for cross-package consistency tests.
func HighRiskTools() []string {
	out := make([]string, len(highRiskTools))
	copy(out, highRiskTools)
	return out
}

func isHighRiskTool(toolName string) bool {
	for _, t := range highRiskTools {
		if toolName == t {
			return true
		}
	}
	return false
}

// IsToolAlwaysAllowable reports whether a tool may be persisted to an agent's
// permissions.always_allow_tools list. The current policy permits every tool
// (highRiskTools is empty); the function and the call sites remain so a
// future tool that truly demands per-call approval can be denied persistence
// without rewriting callers.
func IsToolAlwaysAllowable(toolName string) bool {
	return !isHighRiskTool(toolName)
}

// AppendAlwaysAllowTool adds a tool name to the agent's
// permissions.always_allow_tools list in <agentsDir>/<agentName>/config.yaml.
//
// Concurrency: serialized via flock on <agentDir>/.config.lock. The lock file
// is persistent (never deleted) — see schedule.go for rationale.
//
// First-write: creates the agent directory and an otherwise-empty config.yaml.
//
// Idempotent: duplicate calls are no-ops (the list is deduplicated and sorted).
//
// Defense-in-depth: high-risk tools (isHighRiskTool) are rejected with
// ErrToolNotPersistable. The runtime check at internal/agent/loop.go
// checkPermissionAndApproval also refuses to honor such entries.
func AppendAlwaysAllowTool(agentsDir, agentName, tool string) error {
	if err := ValidateAgentName(agentName); err != nil {
		return err
	}
	if tool == "" {
		return fmt.Errorf("tool name is empty")
	}
	if isHighRiskTool(tool) {
		return fmt.Errorf("%w: %s", ErrToolNotPersistable, tool)
	}

	dir := filepath.Join(agentsDir, agentName)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("mkdir agent dir: %w", err)
	}

	unlock, err := lockAgentConfig(dir)
	if err != nil {
		return err
	}
	defer unlock()

	raw, err := readAgentConfigRaw(dir)
	if err != nil {
		return err
	}

	tools := readAlwaysAllowTools(raw, agentName)
	for _, t := range tools {
		if t == tool {
			return nil // already present
		}
	}
	tools = append(tools, tool)
	sort.Strings(tools)
	setAlwaysAllowTools(raw, tools)

	return writeAgentConfigRaw(dir, raw)
}

// RemoveAlwaysAllowTool removes a tool name from the agent's
// permissions.always_allow_tools list. No-op if the tool is not present, the
// list is empty, or config.yaml does not exist.
//
// If the resulting list is empty AND no other permissions sub-fields are set,
// the permissions: top-level key is dropped to keep YAML clean.
func RemoveAlwaysAllowTool(agentsDir, agentName, tool string) error {
	if err := ValidateAgentName(agentName); err != nil {
		return err
	}
	if tool == "" {
		return fmt.Errorf("tool name is empty")
	}

	dir := filepath.Join(agentsDir, agentName)
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return nil
	}

	unlock, err := lockAgentConfig(dir)
	if err != nil {
		return err
	}
	defer unlock()

	raw, err := readAgentConfigRaw(dir)
	if err != nil {
		return err
	}

	tools := readAlwaysAllowTools(raw, agentName)
	filtered := make([]string, 0, len(tools))
	removed := false
	for _, t := range tools {
		if t == tool {
			removed = true
			continue
		}
		filtered = append(filtered, t)
	}
	if !removed {
		return nil
	}
	setAlwaysAllowTools(raw, filtered)

	return writeAgentConfigRaw(dir, raw)
}

// lockAgentConfig acquires an exclusive flock on <agentDir>/.config.lock and
// returns the release function. The lock file is created on first use and is
// never deleted (deleting it would let two callers grab locks on different
// inodes — see schedule.go for the analogous reasoning).
func lockAgentConfig(agentDir string) (release func(), err error) {
	lockPath := filepath.Join(agentDir, ".config.lock")
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}
	if err := fslock.Lock(f.Fd()); err != nil {
		f.Close()
		return nil, fmt.Errorf("flock: %w", err)
	}
	return func() {
		fslock.Unlock(f.Fd())
		f.Close()
	}, nil
}

// readAgentConfigRaw parses config.yaml into a generic map, preserving unknown
// keys so we never lose fields a future version of the schema may have added.
// Returns an empty map if config.yaml does not exist.
func readAgentConfigRaw(agentDir string) (map[string]interface{}, error) {
	path := filepath.Join(agentDir, "config.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return make(map[string]interface{}), nil
		}
		return nil, fmt.Errorf("read config: %w", err)
	}
	var raw map[string]interface{}
	if len(data) > 0 {
		if err := yaml.Unmarshal(data, &raw); err != nil {
			return nil, fmt.Errorf("parse config: %w", err)
		}
	}
	if raw == nil {
		raw = make(map[string]interface{})
	}
	return raw, nil
}

// writeAgentConfigRaw marshals the raw map to YAML and atomically replaces
// config.yaml. If the resulting map is empty, deletes config.yaml entirely.
func writeAgentConfigRaw(agentDir string, raw map[string]interface{}) error {
	path := filepath.Join(agentDir, "config.yaml")
	if len(raw) == 0 {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	data, err := yaml.Marshal(raw)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	return AtomicWrite(path, data)
}

// readAlwaysAllowTools extracts the string list at permissions.always_allow_tools
// from a raw config map. Returns nil if the path is missing or empty.
//
// Hand-edited config.yaml may have the wrong YAML shape — e.g. a scalar string
// instead of a list. We log a warning (so the user notices) and treat it as
// empty; the next write will canonicalize the value back to a list, which is
// the same forgiving behavior as the rest of WriteAgentConfig.
func readAlwaysAllowTools(raw map[string]interface{}, agentName string) []string {
	perms, _ := raw["permissions"].(map[string]interface{})
	if perms == nil {
		return nil
	}
	rawVal, hasKey := perms["always_allow_tools"]
	if !hasKey || rawVal == nil {
		return nil
	}
	list, ok := rawVal.([]interface{})
	if !ok {
		log.Printf("agents: %s config.yaml permissions.always_allow_tools is %T, expected list; treating as empty (next write will canonicalize)", agentName, rawVal)
		return nil
	}
	if len(list) == 0 {
		return nil
	}
	out := make([]string, 0, len(list))
	for _, v := range list {
		if s, ok := v.(string); ok && s != "" {
			out = append(out, s)
		} else {
			log.Printf("agents: %s config.yaml permissions.always_allow_tools contains non-string entry %v (%T); dropping", agentName, v, v)
		}
	}
	return out
}

// setAlwaysAllowTools writes the list back into the raw config map. If tools
// is empty and permissions has no other keys, the permissions block is dropped
// entirely to keep the YAML output clean.
func setAlwaysAllowTools(raw map[string]interface{}, tools []string) {
	perms, _ := raw["permissions"].(map[string]interface{})

	if len(tools) == 0 {
		if perms == nil {
			return
		}
		delete(perms, "always_allow_tools")
		if len(perms) == 0 {
			delete(raw, "permissions")
		}
		return
	}

	if perms == nil {
		perms = make(map[string]interface{})
		raw["permissions"] = perms
	}
	// Re-encode as []interface{} so yaml.Marshal produces a clean string list.
	items := make([]interface{}, len(tools))
	for i, t := range tools {
		items[i] = t
	}
	perms["always_allow_tools"] = items
}
