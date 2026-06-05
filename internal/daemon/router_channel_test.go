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

func TestActiveRouteKeysForChannel_NoSubstringLeak(t *testing.T) {
	sc := NewSessionCache(t.TempDir())
	sc.LockRouteWithManager("agent:default:slack:C1:thread:99", sc.SessionsDir(""))
	sc.UnlockRoute("agent:default:slack:C1:thread:99")
	sc.LockRouteWithManager("agent:default:slack:C123:thread:5", sc.SessionsDir(""))
	sc.UnlockRoute("agent:default:slack:C123:thread:5")
	// also a route where the channel id is the LAST segment (suffix case)
	sc.LockRouteWithManager("agent:default:slack:C1", sc.SessionsDir(""))
	sc.UnlockRoute("agent:default:slack:C1")

	got := sc.ActiveRouteKeysForChannel("slack", "C1")
	for _, k := range got {
		if k == "agent:default:slack:C123:thread:5" {
			t.Fatalf("C1 query must NOT match C123 route (substring leak): %v", got)
		}
	}
	// C1 (segment + suffix) MUST match:
	foundSeg, foundSuffix := false, false
	for _, k := range got {
		if k == "agent:default:slack:C1:thread:99" {
			foundSeg = true
		}
		if k == "agent:default:slack:C1" {
			foundSuffix = true
		}
	}
	if !foundSeg || !foundSuffix {
		t.Fatalf("C1 segment and suffix routes must match, got %v", got)
	}
}

func TestActiveRouteKeysForChannel_PlatformSegmentBounded(t *testing.T) {
	sc := NewSessionCache(t.TempDir())
	// A real Slack route.
	sc.LockRouteWithManager("agent:default:slack:C1", sc.SessionsDir(""))
	sc.UnlockRoute("agent:default:slack:C1")
	// A DIFFERENT-platform route whose key merely CONTAINS the substring "slack"
	// (e.g. a Telegram channel named "slackmirror"). A platform-level Slack event
	// (channelID == "", as emitted for token_revoked / install_revoked) must NOT
	// inject onto it — the platform token must match as a delimited segment, not
	// a bare substring.
	sc.LockRouteWithManager("agent:default:telegram:slackmirror", sc.SessionsDir(""))
	sc.UnlockRoute("agent:default:telegram:slackmirror")

	got := sc.ActiveRouteKeysForChannel("slack", "") // platform-level event
	foundSlack, leakedTelegram := false, false
	for _, k := range got {
		if k == "agent:default:slack:C1" {
			foundSlack = true
		}
		if k == "agent:default:telegram:slackmirror" {
			leakedTelegram = true
		}
	}
	if !foundSlack {
		t.Fatalf("real slack route must match a platform-level slack event, got %v", got)
	}
	if leakedTelegram {
		t.Fatalf("telegram route containing substring \"slack\" must NOT match a slack event (cross-platform leak), got %v", got)
	}
}
