package daemon

import (
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
)

// newChannelStateConsumer returns the handler wired to the Client's
// onChannelStateEvent callback. It folds the event into the cache (primary
// surface, rendered per-run into Session Facts) AND enqueues an immediate S0
// notice onto every active route on the affected channel so a mid-conversation
// removal is surfaced on the next turn. Platform-derived → Trusted=false.
func newChannelStateConsumer(
	cache *ConnectionStateCache,
	store *SystemEventStore,
	routesForChannel func(platform, channelID string) []string,
	now func() time.Time,
) func(ChannelStateEventPayload) {
	return func(p ChannelStateEventPayload) {
		cache.Apply(p, now())
		text := channelStateNotice(p)
		if text == "" {
			return
		}
		ev := agent.SystemEvent{
			Text:       text,
			Trusted:    false, // platform-derived
			ContextKey: "chan-state:" + p.Platform + ":" + p.ChannelID + ":" + p.Change,
			TS:         now(),
		}
		for _, rk := range routesForChannel(p.Platform, p.ChannelID) {
			store.Enqueue(rk, ev)
		}
	}
}

// channelStateNotice renders the immediate S0 line for an event, or "" for
// changes not worth interrupting on (join is positive; transport blips noisy).
func channelStateNotice(p ChannelStateEventPayload) string {
	switch p.Change {
	case ChangeKicked, ChangeLeave, ChangeBan:
		return "You were removed from " + p.Platform + " " + p.ChannelID + "; you can no longer read or reply there until re-added."
	case ChangeInstallRevoked:
		return p.Platform + " app authorization was revoked; you cannot send or receive on " + p.Platform + " until it is re-installed."
	case ChangeTokenRevoked:
		return p.Platform + " authorization was revoked; re-authorize to restore " + p.Platform + " connectivity."
	default:
		return ""
	}
}
