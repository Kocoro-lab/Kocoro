package agents

import (
	"os"
	"path/filepath"
	"strings"
)

// AgentOwnerFile is the device-local sidecar recording which verified
// principal (Cloud user id) an agent's definition belongs to for SYNC
// purposes. It scopes agent-sync push/pull to the owning account after a
// principal switch; the runtime (listing, routing, execution) deliberately
// stays cross-account shared. The file is NEVER synced, is not part of
// agentDefinitionFiles (must not drive the LWW clock), and an absent file
// means "unstamped" — grandfathered to whichever verified principal touches
// the agent's sync first.
const AgentOwnerFile = "_owner"

// ReadAgentOwner returns the owning principal id, or "" when the agent is
// unstamped (missing/unreadable/empty sidecar — all fail open to the
// grandfather semantics).
func ReadAgentOwner(agentsDir, name string) string {
	data, err := os.ReadFile(filepath.Join(agentsDir, name, AgentOwnerFile))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// WriteAgentOwner stamps (or, with an empty owner, clears) the agent's sync
// owner. Callers already serialize per-agent mutations on the route lock, so
// a plain write is sufficient.
func WriteAgentOwner(agentsDir, name, owner string) error {
	path := filepath.Join(agentsDir, name, AgentOwnerFile)
	if owner == "" {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Join(agentsDir, name), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(owner+"\n"), 0o600)
}
