package cmd

import (
	"encoding/json"
	"log"

	"github.com/Kocoro-lab/ShanClaw/internal/daemon"
)

// lifecycleEventSender is the narrow interface emitLifecycleReceived needs
// from *daemon.Client. Defined here so tests can substitute a recording fake
// without standing up a real WebSocket server.
type lifecycleEventSender interface {
	SendEvent(messageID, eventType, message string, data map[string]interface{}) error
}

// emitLifecycleReceived emits a MESSAGE_LIFECYCLE event with state="received"
// for an inbound IM message. No-op when wsClient is nil, messageID is empty,
// or IMStatusContext is empty — these guards let every entry point call the
// helper unconditionally without filtering for IM vs non-IM sources at the
// call site (non-IM messages arrive with empty IMStatusContext).
func emitLifecycleReceived(wsClient lifecycleEventSender, messageID string, ctx json.RawMessage) {
	if wsClient == nil || messageID == "" || len(ctx) == 0 {
		return
	}
	if err := wsClient.SendEvent(messageID, daemon.EventTypeMessageLifecycle, "", map[string]interface{}{
		"state":             daemon.LifecycleReceived,
		"im_status_context": ctx,
	}); err != nil {
		log.Printf("daemon: lifecycle received emit failed: %v", err)
	}
}
