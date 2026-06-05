package daemon

import (
	"strings"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
)

// channelLabel renders a short channel reference for the failure line. ThreadID
// for Slack is "<channel>-<ts>"; surface the channel head when present, else the
// platform.
func channelLabel(p ReplyDeliveryResultPayload) string {
	if p.ThreadID != "" {
		if i := strings.IndexByte(p.ThreadID, '-'); i > 0 {
			return p.Channel + " " + p.ThreadID[:i]
		}
	}
	return p.Channel
}

// formatDeliveryFailure builds the user-facing-internal line for a failed
// delivery. PERMANENT also carries the reactive Gap-3 inference ("will not
// receive … until re-added"); TRANSIENT stays cautious (no removal claim — the
// Cloud replay backstop may still deliver it). Always "delivered"/"not
// delivered", never "read".
func formatDeliveryFailure(p ReplyDeliveryResultPayload) string {
	reason := p.Reason
	if reason == "" {
		reason = "delivery failed"
	}
	where := channelLabel(p)
	if p.Class == ClassPermanent {
		return "reply to " + where + " FAILED: " + reason +
			" — the user did not see it, and the bot will not receive or deliver messages there until re-added/re-authorized."
	}
	return "reply to " + where + " may not have been delivered (" + reason + "); a retry is in progress."
}

// newDeliveryResultConsumer returns the handler wired to the Client's
// onReplyDeliveryResult callback. Success is transmitted but silent on
// injection (so a future ok=true can clear reactive state without polluting
// context); failure enqueues one line onto the originating route for next-turn
// injection. The event is Trusted (daemon-authored text; the platform Reason is
// already classified Cloud-side).
func newDeliveryResultConsumer(store *SystemEventStore, idx *ReplyRouteIndex) func(ReplyDeliveryResultPayload, string) {
	return func(p ReplyDeliveryResultPayload, messageID string) {
		if p.OK {
			return // silent on success
		}
		routeKey := idx.Get(messageID)
		if routeKey == "" {
			return // can't bind to a route — drop (binding poll reconciles)
		}
		store.Enqueue(routeKey, agent.SystemEvent{
			Text:       formatDeliveryFailure(p),
			Trusted:    true,
			ContextKey: "delivery-fail:" + p.Channel + ":" + p.ThreadID,
			TS:         time.Now(),
		})
	}
}

// HandleReplyDeliveryResult is the cmd-facing entry point: builds the consumer
// and applies it once. Exported so cmd/daemon.go stays a one-liner.
func HandleReplyDeliveryResult(store *SystemEventStore, idx *ReplyRouteIndex, p ReplyDeliveryResultPayload, messageID string) {
	newDeliveryResultConsumer(store, idx)(p, messageID)
}
