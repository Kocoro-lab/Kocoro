package agents

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAgentOwnerRoundTrip(t *testing.T) {
	dir := t.TempDir()

	if got := ReadAgentOwner(dir, "agt"); got != "" {
		t.Fatalf("owner of missing agent = %q, want empty (unstamped)", got)
	}

	if err := WriteAgentOwner(dir, "agt", "user-a"); err != nil {
		t.Fatalf("WriteAgentOwner: %v", err)
	}
	if got := ReadAgentOwner(dir, "agt"); got != "user-a" {
		t.Fatalf("owner = %q, want user-a", got)
	}

	// Re-stamp overwrites (pull materialize onto a foreign-owned agent).
	if err := WriteAgentOwner(dir, "agt", "user-b"); err != nil {
		t.Fatalf("WriteAgentOwner (restamp): %v", err)
	}
	if got := ReadAgentOwner(dir, "agt"); got != "user-b" {
		t.Fatalf("owner after restamp = %q, want user-b", got)
	}

	// Clearing with an empty owner removes the sidecar entirely.
	if err := WriteAgentOwner(dir, "agt", ""); err != nil {
		t.Fatalf("WriteAgentOwner (clear): %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "agt", AgentOwnerFile)); !os.IsNotExist(err) {
		t.Fatal("clearing the owner must remove the sidecar file")
	}
	if got := ReadAgentOwner(dir, "agt"); got != "" {
		t.Fatalf("owner after clear = %q, want empty", got)
	}
}

// The sidecar is device-local metadata: it must not be part of the LWW
// definition-file set, or stamping would masquerade as a definition edit and
// win sync rounds it should not.
func TestAgentOwnerFileNotInLWWSet(t *testing.T) {
	if AgentOwnerFile != "_owner" {
		t.Fatalf("AgentOwnerFile = %q, want _owner", AgentOwnerFile)
	}
	// Whitespace and newlines from hand edits are tolerated.
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "agt"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "agt", AgentOwnerFile), []byte(" user-a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := ReadAgentOwner(dir, "agt"); got != "user-a" {
		t.Fatalf("owner with surrounding whitespace = %q, want user-a", got)
	}
}
