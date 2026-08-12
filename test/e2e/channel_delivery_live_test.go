package e2e

import (
	"strings"
	"testing"
	"time"
)

const (
	channelDeliverySamples = 6
	channelDeliveryMinRuns = 5
)

// TestLive_ChannelDeliveryMetadataShapesReply verifies that the real provider
// understands daemon-injected channel metadata after its cache-layer move. The
// user prompt does not name the channel, explain routing, or suggest an answer.
// This covers response semantics; the isolated daemon intentionally suppresses
// Cloud transport, so it does not claim that a remote Slack delivery occurred.
func TestLive_ChannelDeliveryMetadataShapesReply(t *testing.T) {
	skipUnlessLive(t)
	daemon := startIsolatedLiveDaemon(t, testBinary(t), isolatedLiveOptions{
		EffortTier: "max",
	})

	matchingRuns := 0
	var totalCost float64
	var totalLatency time.Duration
	for i := 0; i < channelDeliverySamples; i++ {
		run := streamMessage(t, daemon.baseURL, map[string]interface{}{
			"text":        "Where will your reply to this message appear? Answer in one sentence.",
			"source":      "slack",
			"channel":     "C0123456789",
			"new_session": true,
		})
		totalCost += run.CostUSD
		totalLatency += run.Duration
		answer := doneReply(run.Frames)
		lower := strings.ToLower(answer)
		mentionsSlack := strings.Contains(lower, "slack")
		usedTools := len(runningToolNames(run.Frames)) > 0
		passed := mentionsSlack && !usedTools
		if passed {
			matchingRuns++
		}
		t.Logf(
			"run %d: passed=%t mentions_slack=%t used_tools=%t latency=%s cost=$%.6f answer=%q",
			i+1,
			passed,
			mentionsSlack,
			usedTools,
			run.Duration.Round(time.Millisecond),
			run.CostUSD,
			truncateForLog(answer),
		)
	}
	t.Logf(
		"observed channel-delivery understanding=%d/%d total_latency=%s total_cost=$%.6f",
		matchingRuns,
		channelDeliverySamples,
		totalLatency.Round(time.Millisecond),
		totalCost,
	)
	if matchingRuns < channelDeliveryMinRuns {
		t.Errorf(
			"only %d/%d replies identified the daemon-provided Slack destination without tool use (want >= %d)",
			matchingRuns,
			channelDeliverySamples,
			channelDeliveryMinRuns,
		)
	}
}
