package daemon

import (
	"strings"
	"testing"
	"time"
)

func TestConnectionStateCache_SetGetRender(t *testing.T) {
	c := NewConnectionStateCache()
	now := time.Date(2026, 6, 5, 10, 0, 0, 0, time.UTC)

	c.Apply(ChannelStateEventPayload{Axis: AxisMembership, Platform: "slack", ChannelID: "C1", Change: ChangeKicked, TS: "2026-06-05T10:00:00Z"}, now)
	if got := c.ChannelLine("slack", "C1"); got == "" || !strings.Contains(got, "removed") {
		t.Fatalf("channel line should reflect removal: %q", got)
	}

	c.Apply(ChannelStateEventPayload{Axis: AxisBinding, Platform: "feishu", Change: ChangeInstallRevoked, TS: "2026-06-05T10:00:00Z"}, now)
	if got := c.PlatformLine("feishu"); got == "" || !strings.Contains(got, "authorization") {
		t.Fatalf("platform line should reflect revoked auth: %q", got)
	}

	if got := c.ChannelLine("slack", "C-unknown"); got != "" {
		t.Fatalf("unknown channel should render empty, got %q", got)
	}
}

// TestConnectionStateCache_TransportDoesNotMaskBinding locks that a transient
// transport "disconnected" event does NOT overwrite an actionable binding
// revocation. Before the fix both axes shared one slot (last-write-wins), so a
// larkws blip replaced the "re-authorize to restore" guidance with the
// ignore-me "reconnecting" line.
func TestConnectionStateCache_TransportDoesNotMaskBinding(t *testing.T) {
	c := NewConnectionStateCache()
	now := time.Date(2026, 6, 5, 10, 0, 0, 0, time.UTC)
	c.Apply(ChannelStateEventPayload{Axis: AxisBinding, Platform: "feishu", Change: ChangeTokenRevoked, TS: "x"}, now)
	c.Apply(ChannelStateEventPayload{Axis: AxisTransport, Platform: "feishu", Change: ChangeDisconnected, TS: "y"}, now)

	got := c.PlatformLine("feishu")
	if !strings.Contains(got, "re-authorize") {
		t.Fatalf("transient transport disconnect masked the binding revocation: %q", got)
	}
	// Preamble must reflect the actionable state too.
	pre := strings.Join(c.Preamble(), "\n")
	if !strings.Contains(pre, "re-authorize") {
		t.Fatalf("preamble masked the binding revocation: %q", pre)
	}
}

// TestConnectionStateCache_TransportShownWithoutBinding verifies a transport
// disconnect still surfaces when there is no binding-axis state to take
// precedence — separating the axes must not silently drop transport notices.
func TestConnectionStateCache_TransportShownWithoutBinding(t *testing.T) {
	c := NewConnectionStateCache()
	now := time.Date(2026, 6, 5, 10, 0, 0, 0, time.UTC)
	c.Apply(ChannelStateEventPayload{Axis: AxisTransport, Platform: "slack", Change: ChangeDisconnected, TS: "x"}, now)
	if got := c.PlatformLine("slack"); !strings.Contains(got, "connection dropped") {
		t.Fatalf("transport state should surface when no binding state present: %q", got)
	}
}

// TestConnectionStateCache_PreambleDeterministicOrder locks a stable line order
// across multiple degraded platforms (map iteration is otherwise random),
// keeping the new-session preamble byte-stable.
func TestConnectionStateCache_PreambleDeterministicOrder(t *testing.T) {
	build := func() []string {
		c := NewConnectionStateCache()
		now := time.Date(2026, 6, 5, 10, 0, 0, 0, time.UTC)
		c.Apply(ChannelStateEventPayload{Axis: AxisBinding, Platform: "slack", Change: ChangeTokenRevoked, TS: "x"}, now)
		c.Apply(ChannelStateEventPayload{Axis: AxisBinding, Platform: "feishu", Change: ChangeInstallRevoked, TS: "x"}, now)
		c.Apply(ChannelStateEventPayload{Axis: AxisTransport, Platform: "wecom", Change: ChangeDisconnected, TS: "x"}, now)
		return c.Preamble()
	}
	first := strings.Join(build(), "\n")
	for i := 0; i < 20; i++ {
		if got := strings.Join(build(), "\n"); got != first {
			t.Fatalf("preamble order not deterministic:\n%q\nvs\n%q", first, got)
		}
	}
}

func TestConnectionStateCache_Preamble(t *testing.T) {
	c := NewConnectionStateCache()
	now := time.Date(2026, 6, 5, 10, 0, 0, 0, time.UTC)
	c.Apply(ChannelStateEventPayload{Axis: AxisBinding, Platform: "slack", Change: ChangeTokenRevoked, TS: "2026-06-05T10:00:00Z"}, now)
	lines := c.Preamble()
	if len(lines) == 0 || !strings.Contains(lines[0], "Slack") {
		t.Fatalf("preamble should mention Slack auth state: %v", lines)
	}
}

func TestConnectionStateCache_MarkHealthyAndNilSafe(t *testing.T) {
	var c *ConnectionStateCache
	c.Apply(ChannelStateEventPayload{Axis: AxisMembership, Platform: "slack", ChannelID: "C1", Change: ChangeKicked}, time.Now()) // no panic
	if c.ChannelLine("slack", "C1") != "" {
		t.Fatal("nil cache should render empty")
	}
	c2 := NewConnectionStateCache()
	now := time.Now()
	c2.Apply(ChannelStateEventPayload{Axis: AxisMembership, Platform: "slack", ChannelID: "C1", Change: ChangeKicked, TS: "x"}, now)
	c2.MarkChannelHealthy("slack", "C1")
	if c2.ChannelLine("slack", "C1") != "" {
		t.Fatal("MarkChannelHealthy should clear the channel state")
	}
}
