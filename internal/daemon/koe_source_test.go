package daemon

import "testing"

func TestIsKoeSource(t *testing.T) {
	cases := map[string]bool{
		"koe":         true,
		"KOE":         true,
		" koe ":       true,
		"koe-reachy":  true,
		"koe-bot":     true,
		" KOE-Reachy": true,
		"koer":        false, // must not match a "koe" prefix without the hyphen
		"kocoro":      false,
		"slack":       false,
		"":            false,
	}
	for input, want := range cases {
		if got := isKoeSource(input); got != want {
			t.Errorf("isKoeSource(%q) = %v, want %v", input, got, want)
		}
	}
	if ChannelKoe != "koe" {
		t.Errorf("ChannelKoe = %q, want \"koe\"", ChannelKoe)
	}
}

func TestKoeMessagingClassification(t *testing.T) {
	for _, src := range []string{"koe", "koe-reachy", " KOE "} {
		if !IsMessagingPlatform(src) {
			t.Errorf("IsMessagingPlatform(%q) = false, want true", src)
		}
		if got := kindOf(src); got != SessionKindIM {
			t.Errorf("kindOf(%q) = %q, want %q", src, got, SessionKindIM)
		}
		if isInteractiveSource(src) {
			t.Errorf("isInteractiveSource(%q) = true, want false (koe is a burst session, not the user's interactive chat)", src)
		}
	}
}

func TestKoeRouteKey(t *testing.T) {
	// Bound agent + burst-id thread → dedicated burst lane.
	bound := ComputeRouteKey(RunAgentRequest{Source: "koe", ThreadID: "burst-123", Agent: "finance"})
	if bound != "agent:finance:koe:burst-123" {
		t.Errorf("bound koe route = %q, want agent:finance:koe:burst-123", bound)
	}
	// Default agent (no Agent field) → default burst lane.
	def := ComputeRouteKey(RunAgentRequest{Source: "koe", ThreadID: "burst-123"})
	if def != "default:koe:burst-123" {
		t.Errorf("default koe route = %q, want default:koe:burst-123", def)
	}
	// koe is never bypassed (must persist + resume across a burst's do_task calls).
	if shouldBypassNamedAgentRoute("koe") {
		t.Error("shouldBypassNamedAgentRoute(koe) = true, want false")
	}
	if shouldBypassRouteCache("koe") {
		t.Error("shouldBypassRouteCache(koe) = true, want false")
	}
}

func TestKoeOutputFormat(t *testing.T) {
	for _, src := range []string{"koe", "koe-reachy", " KOE "} {
		if got := outputFormatForSource(src); got != "koe" {
			t.Errorf("outputFormatForSource(%q) = %q, want \"koe\"", src, got)
		}
	}
}
