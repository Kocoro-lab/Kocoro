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
// grandfather semantics). Reads are lock-free: WriteAgentOwner replaces the
// file atomically (temp+rename), so a read never observes a torn value.
func ReadAgentOwner(agentsDir, name string) string {
	if ValidateAgentName(name) != nil {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(agentsDir, name, AgentOwnerFile))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// WriteAgentOwner stamps (or, with an empty owner, clears) the agent's sync
// owner. The write is atomic (temp+rename, per the repo's Atomic Writes
// convention) so a concurrent lock-free ReadAgentOwner can never observe an
// empty or truncated value and misread a stamped agent as unstamped — which
// would let the OTHER account's push grandfather-claim it mid-restamp. The
// agent directory must already exist (every caller writes after the
// definition files); a missing dir surfaces as an error rather than creating
// a ghost directory.
func WriteAgentOwner(agentsDir, name, owner string) error {
	if err := ValidateAgentName(name); err != nil {
		return err
	}
	path := filepath.Join(agentsDir, name, AgentOwnerFile)
	if owner == "" {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	return AtomicWrite(path, []byte(owner+"\n"))
}
