package daemon

import (
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/session"
)

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

func TestKoeBannerAndTitlePreservation(t *testing.T) {
	for _, src := range []string{"koe", "koe-reachy"} {
		if shouldEmitReplyBanner(src) {
			t.Errorf("shouldEmitReplyBanner(%q) = true, want false (Koe voices the reply itself)", src)
		}
		// koe must NOT be an autonomous-local source: that gate also disables the
		// smart-title upgrade, which we want to KEEP for voice bursts.
		if isAutonomousLocalSource(src) {
			t.Errorf("isAutonomousLocalSource(%q) = true, want false (smart titles must stay enabled)", src)
		}
	}
}

func TestKoeCacheSource(t *testing.T) {
	if got := cacheSourceFromDaemonSource("koe"); got != "koe" {
		t.Errorf("cacheSourceFromDaemonSource(\"koe\") = %q, want \"koe\"", got)
	}
	if got := cacheSourceFromDaemonSource(" KOE "); got != "koe" {
		t.Errorf("cacheSourceFromDaemonSource(\" KOE \") = %q, want \"koe\" (normalized)", got)
	}
	if got := cacheSourceFromDaemonSource("koe-reachy"); got != "koe-reachy" {
		t.Errorf("cacheSourceFromDaemonSource(\"koe-reachy\") = %q, want \"koe-reachy\"", got)
	}
}

func TestStampSessionOrigin(t *testing.T) {
	// koe with empty Channel: Source MUST be persisted (else misclassified interactive).
	koe := &session.Session{}
	stampSessionOrigin(koe, RunAgentRequest{Source: "koe", ThreadID: "burst-1"})
	if koe.Source != "koe" {
		t.Errorf("koe burst: sess.Source = %q, want \"koe\"", koe.Source)
	}

	// Interactive source with empty Channel: Source must STAY empty (stays interactive).
	desktop := &session.Session{}
	stampSessionOrigin(desktop, RunAgentRequest{Source: "desktop"})
	if desktop.Source != "" {
		t.Errorf("desktop: sess.Source = %q, want \"\" (interactive sources stay unstamped)", desktop.Source)
	}

	// IM source with a Channel: both Source and Channel persisted (unchanged behavior).
	slack := &session.Session{}
	stampSessionOrigin(slack, RunAgentRequest{Source: "slack", Channel: "C123"})
	if slack.Source != "slack" || slack.Channel != "C123" {
		t.Errorf("slack: Source=%q Channel=%q, want \"slack\"/\"C123\"", slack.Source, slack.Channel)
	}

	// Empty source: no-op, no panic.
	empty := &session.Session{}
	stampSessionOrigin(empty, RunAgentRequest{})
	if empty.Source != "" {
		t.Errorf("empty source: sess.Source = %q, want \"\"", empty.Source)
	}
}
