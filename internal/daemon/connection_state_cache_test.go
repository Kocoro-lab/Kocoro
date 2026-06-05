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
