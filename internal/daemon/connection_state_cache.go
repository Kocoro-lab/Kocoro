package daemon

import (
	"sort"
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
	channels map[string]channelState // key "<platform>:<channelID>" (membership axis)
	binding  map[string]channelState // key "<platform>" (binding axis — actionable, sticky)
	// transport is kept SEPARATE from binding so a transient transport
	// "disconnected" blip cannot overwrite an actionable install/token
	// revocation. binding takes precedence at render time; transport only shows
	// when there is no binding-axis state for the platform.
	transport map[string]channelState // key "<platform>" (transport axis — transient)
}

func NewConnectionStateCache() *ConnectionStateCache {
	return &ConnectionStateCache{
		channels:  map[string]channelState{},
		binding:   map[string]channelState{},
		transport: map[string]channelState{},
	}
}

// Apply folds one event into the cache. Membership events update the channel
// map; binding and transport events update their own platform maps (kept
// separate so a transient transport blip can't mask a binding revocation).
// nil-safe.
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
	case AxisBinding:
		c.binding[p.Platform] = channelState{change: p.Change, at: now}
	case AxisTransport:
		c.transport[p.Platform] = channelState{change: p.Change, at: now}
	}
}

// MarkChannelHealthy clears a channel's negative state. RESERVED: nothing in
// production calls this yet — the 60s poll reconciles only the binding axis
// (MarkPlatformHealthy), so a channel's kicked/left state currently clears only
// when Cloud pushes a membership `join` event (Apply overwrites to a change
// changePhrase renders empty). Wire this into a future per-channel
// reconciliation pass to also recover from a missed join. nil-safe.
func (c *ConnectionStateCache) MarkChannelHealthy(platform, channelID string) {
	if c == nil || channelID == "" {
		return
	}
	c.mu.Lock()
	delete(c.channels, platform+":"+channelID)
	c.mu.Unlock()
}

// MarkPlatformHealthy clears a platform's negative binding AND transport state.
// nil-safe.
func (c *ConnectionStateCache) MarkPlatformHealthy(platform string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	delete(c.binding, platform)
	delete(c.transport, platform)
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

// PlatformLine renders a platform-level connection fragment, or "". The
// actionable binding state (install/token revoked) takes precedence over a
// transient transport disconnect for the same platform. nil-safe.
func (c *ConnectionStateCache) PlatformLine(platform string) string {
	if c == nil {
		return ""
	}
	c.mu.RLock()
	st, ok := c.binding[platform]
	if !ok {
		st, ok = c.transport[platform]
	}
	c.mu.RUnlock()
	if !ok {
		return ""
	}
	return changePhrase(platform, st.change)
}

// Preamble renders all currently-degraded platforms as new-session lines, one
// per platform (binding state preferred over transport), in a deterministic
// (platform-sorted) order so the new-session prompt stays byte-stable.
// Healthy/unknown platforms are omitted. nil-safe.
func (c *ConnectionStateCache) Preamble() []string {
	if c == nil {
		return nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	// Union of degraded platforms; binding wins over transport per platform.
	states := make(map[string]channelState, len(c.binding)+len(c.transport))
	for p, st := range c.transport {
		states[p] = st
	}
	for p, st := range c.binding {
		states[p] = st
	}
	platforms := make([]string, 0, len(states))
	for p := range states {
		platforms = append(platforms, p)
	}
	sort.Strings(platforms)
	out := make([]string, 0, len(platforms))
	for _, p := range platforms {
		out = append(out, title(p)+": "+changePhrase(p, states[p].change))
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
