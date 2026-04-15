package agent

import (
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

// SetRunMessagesForTest injects a run-messages snapshot for tests that
// exercise downstream code (e.g., the daemon's session checkpoint helper)
// without running a full AgentLoop. Not for production use.
func SetRunMessagesForTest(a *AgentLoop, msgs []client.Message) {
	a.runMessages = msgs
	// Metadata parallels — fill with zero values so indexed access is safe.
	a.runMsgInjected = make([]bool, len(msgs))
	a.runMsgTimestamps = make([]time.Time, len(msgs))
}
