package daemon

import (
	"strings"
	"sync"
	"time"
)

// Axis/Change consts mirror the Cloud channels package (kept local to avoid a
// channels→daemon import cycle; values MUST match the wire payload exactly).
const (
	AxisTransport  = "transport"
	AxisBinding    = "binding"
	AxisMembership = "membership"

	ChangeJoin           = "join"
	ChangeLeave          = "leave"
	ChangeKicked         = "kicked"
	ChangeBan            = "ban"
	ChangeInstallRevoked = "install_revoked"
	ChangeTokenRevoked   = "token_revoked"
	ChangeDisconnected   = "disconnected"
)

type channelState struct {
	change string
	at     time.Time
}

// ConnectionStateCache holds the latest known connection state per platform and
// per (platform,channel). It is the daemon's primary connection-awareness store
// (the 60s ListChannelBindings poll is demoted to a reconciliation backstop that
// also writes here). Bounded by the number of channels/platforms a user is
// bound to; daemon restart wipes it (the poll re-seeds).
type ConnectionStateCache struct {
	mu       sync.RWMutex
	channels map[string]channelState // key "<platform>:<channelID>"
	binding  map[string]channelState // key "<platform>"
}

func NewConnectionStateCache() *ConnectionStateCache {
	return &ConnectionStateCache{channels: map[string]channelState{}, binding: map[string]channelState{}}
}

// Apply folds one event into the cache. Membership events update the channel
// map; binding/transport events update the platform map. nil-safe.
func (c *ConnectionStateCache) Apply(p ChannelStateEventPayload, now time.Time) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	switch p.Axis {
	case AxisMembership:
		if p.ChannelID != "" {
			c.channels[p.Platform+":"+p.ChannelID] = channelState{change: p.Change, at: now}
		}
	case AxisBinding, AxisTransport:
		c.binding[p.Platform] = channelState{change: p.Change, at: now}
	}
}

// MarkChannelHealthy clears a channel's negative state (a re-join, or the
// binding poll confirming membership). Used by reconciliation. nil-safe.
func (c *ConnectionStateCache) MarkChannelHealthy(platform, channelID string) {
	if c == nil || channelID == "" {
		return
	}
	c.mu.Lock()
	delete(c.channels, platform+":"+channelID)
	c.mu.Unlock()
}

// MarkPlatformHealthy clears a platform's negative binding state. nil-safe.
func (c *ConnectionStateCache) MarkPlatformHealthy(platform string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	delete(c.binding, platform)
	c.mu.Unlock()
}

// ChannelLine renders a one-line Session-Facts fragment for a specific channel's
// negative state, or "" when healthy/unknown. nil-safe.
func (c *ConnectionStateCache) ChannelLine(platform, channelID string) string {
	if c == nil || channelID == "" {
		return ""
	}
	c.mu.RLock()
	st, ok := c.channels[platform+":"+channelID]
	c.mu.RUnlock()
	if !ok {
		return ""
	}
	return changePhrase(platform, st.change)
}

// PlatformLine renders a platform-level binding fragment, or "". nil-safe.
func (c *ConnectionStateCache) PlatformLine(platform string) string {
	if c == nil {
		return ""
	}
	c.mu.RLock()
	st, ok := c.binding[platform]
	c.mu.RUnlock()
	if !ok {
		return ""
	}
	return changePhrase(platform, st.change)
}

// Preamble renders all currently-degraded platforms as new-session lines.
// Healthy/unknown platforms are omitted. nil-safe.
func (c *ConnectionStateCache) Preamble() []string {
	if c == nil {
		return nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	var out []string
	for platform, st := range c.binding {
		out = append(out, title(platform)+": "+changePhrase(platform, st.change))
	}
	return out
}

func changePhrase(platform, change string) string {
	switch change {
	case ChangeKicked, ChangeLeave, ChangeBan:
		return "the bot was removed from this channel and can no longer read or post here until re-added"
	case ChangeInstallRevoked:
		return title(platform) + " app authorization was revoked; the bot cannot send or receive until re-installed"
	case ChangeTokenRevoked:
		return title(platform) + " authorization token was revoked; re-authorize to restore"
	case ChangeDisconnected:
		return title(platform) + " connection dropped; reconnecting"
	default:
		return ""
	}
}

func title(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
