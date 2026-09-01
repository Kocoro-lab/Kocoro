package bundled

import (
	"strings"
	"testing"
)

// The kocoro skill references are the agent's ground truth for daemon
// behavior. The always-allow prune fires on every successful integration
// catalog rebuild — POST /integrations/refresh AND every verified-principal
// transition (sign-in, account switch, same-account key rotation via
// forcePrincipalEpoch). The recovery list in agents.md must name all of them:
// the same sentence cites "key rotation in progress" as the cause of a stuck
// entry, so omitting it from the recovery list reads as "key rotation does
// not self-heal", which is false.
func TestKocoroAgentsReferenceNamesAllPruneTriggers(t *testing.T) {
	data, err := FS.ReadFile("skills/kocoro/references/agents.md")
	if err != nil {
		t.Fatalf("read embedded agents.md: %v", err)
	}
	// Anchor on the behavior keyword, then scan its whole line so benign
	// rewording around it cannot break the pin.
	var line string
	for _, l := range strings.Split(string(data), "\n") {
		if strings.Contains(l, "prunes it automatically") {
			line = l
			break
		}
	}
	if line == "" {
		t.Fatal(`agents.md lost the "prunes it automatically" self-heal sentence`)
	}
	for _, want := range []string{"/integrations/refresh", "sign-in", "account switch", "key rotation"} {
		if !strings.Contains(line, want) {
			t.Errorf("prune-trigger line missing %q:\n%s", want, line)
		}
	}
}
