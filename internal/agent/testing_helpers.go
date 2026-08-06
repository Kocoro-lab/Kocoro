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

// SetCompactionCheckpointMessagesForTest injects a compacted live-state
// snapshot for downstream persistence tests without running a full compaction.
func SetCompactionCheckpointMessagesForTest(a *AgentLoop, msgs []client.Message) {
	a.compactionCheckpointMessages = msgs
}

// SetLastSystemPromptEstimateForTest seeds the system-prompt estimate that
// external compaction drivers add to their overhead, without running a turn.
func SetLastSystemPromptEstimateForTest(a *AgentLoop, tokens int) {
	a.lastSystemPromptEst.Store(int64(tokens))
}
