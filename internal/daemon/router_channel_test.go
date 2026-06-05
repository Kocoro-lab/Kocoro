package daemon

import "testing"

func TestActiveRouteKeysForChannel(t *testing.T) {
	sc := NewSessionCache(t.TempDir())
	// Seed routes via the exported route-locking API; LockRouteWithManager
	// leaves the entry in sc.routes (never deleted), so UnlockRoute keeps it.
	sc.LockRouteWithManager("agent:default:slack:C1:thread:99", sc.SessionsDir(""))
	sc.UnlockRoute("agent:default:slack:C1:thread:99")
	sc.LockRouteWithManager("agent:default:slack:C2:thread:5", sc.SessionsDir(""))
	sc.UnlockRoute("agent:default:slack:C2:thread:5")

	got := sc.ActiveRouteKeysForChannel("slack", "C1")
	foundC1, foundC2 := false, false
	for _, k := range got {
		if k == "agent:default:slack:C1:thread:99" {
			foundC1 = true
		}
		if k == "agent:default:slack:C2:thread:5" {
			foundC2 = true
		}
	}
	if !foundC1 {
		t.Fatalf("C1 route should match, got %v", got)
	}
	if foundC2 {
		t.Fatalf("C2 route must NOT match a C1 query, got %v", got)
	}
}
