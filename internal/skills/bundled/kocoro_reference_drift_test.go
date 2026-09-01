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
	const anchor = "prunes it automatically after the next successful catalog rebuild ("
	src := string(data)
	start := strings.Index(src, anchor)
	if start < 0 {
		t.Fatalf("agents.md lost the prune-trigger sentence (anchor %q)", anchor)
	}
	rest := src[start+len(anchor):]
	end := strings.Index(rest, ")")
	if end < 0 {
		t.Fatal("prune-trigger list parenthetical is unterminated")
	}
	triggers := rest[:end]
	for _, want := range []string{"/integrations/refresh", "sign-in", "account switch", "key rotation"} {
		if !strings.Contains(triggers, want) {
			t.Errorf("prune-trigger list %q missing %q", triggers, want)
		}
	}
}
