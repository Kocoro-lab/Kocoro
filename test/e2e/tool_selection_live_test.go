//go:build live

package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	toolSelectionSamples = 7
	toolSelectionMinRuns = 5
)

func TestLive_DedicatedContentSearchAvoidsShell(t *testing.T) {
	skipUnlessLive(t)
	bin := testBinary(t)
	daemon := startIsolatedLiveDaemon(t, bin, isolatedLiveOptions{
		EffortTier:  "max",
		AutoApprove: true,
	})
	probeDir := writeToolSelectionFixture(t)
	prompt := fmt.Sprintf(
		"Search every file under %s for the exact string QUASAR-TELEMETRY-731. "+
			"Report each matching file path and matching line.",
		probeDir,
	)

	dedicatedRuns := 0
	var totalCost float64
	var totalLatency time.Duration
	for i := 0; i < toolSelectionSamples; i++ {
		run := streamMessage(t, daemon.baseURL, map[string]interface{}{
			"text":   prompt,
			"source": "kocoro",
		})
		totalCost += run.CostUSD
		totalLatency += run.Duration
		tools := runningToolNames(run.Frames)
		usedGrep := containsString(tools, "grep")
		usedBash := containsString(tools, "bash")
		flow := liveToolFlowWithArgs(decodeLiveToolFrames(run.Frames))
		answer := doneReply(run.Frames)
		correct := strings.Contains(answer, "QUASAR-TELEMETRY-731") &&
			strings.Contains(answer, "nested/telemetry.txt")
		if usedGrep && !usedBash && correct {
			dedicatedRuns++
		}
		t.Logf(
			"run %d: dedicated=%t correct=%t tools=%v flow=%v latency=%s cost=$%.6f",
			i+1,
			usedGrep && !usedBash,
			correct,
			tools,
			flow,
			run.Duration.Round(time.Millisecond),
			run.CostUSD,
		)
	}
	t.Logf(
		"observed dedicated content search=%d/%d total_latency=%s total_cost=$%.6f",
		dedicatedRuns,
		toolSelectionSamples,
		totalLatency.Round(time.Millisecond),
		totalCost,
	)
	if dedicatedRuns < toolSelectionMinRuns {
		t.Errorf(
			"only %d/%d runs used grep without bash and returned the real match (want >= %d)",
			dedicatedRuns,
			toolSelectionSamples,
			toolSelectionMinRuns,
		)
	}
}

func liveToolFlowWithArgs(frames []liveToolFrame) []string {
	flow := make([]string, 0, len(frames))
	for _, frame := range frames {
		entry := frame.Tool + "/" + frame.Status
		if frame.Status == "running" && frame.Args != "" {
			entry += ":" + truncateForLog(frame.Args)
		}
		if frame.Status == "completed" && frame.Preview != "" {
			entry += ":" + truncateForLog(frame.Preview)
		}
		flow = append(flow, entry)
	}
	return flow
}

func writeToolSelectionFixture(t *testing.T) string {
	t.Helper()
	dir := neutralTempDir(t, "observatory-data-*")
	files := map[string]string{
		"README.md":            "fixture root\n",
		"notes/ordinary.txt":   "nothing relevant here\n",
		"nested/telemetry.txt": "status=QUASAR-TELEMETRY-731\n",
		"nested/archive.log":   "QUASAR-TELEMETRY-730\n",
	}
	for name, body := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatalf("create fixture directory: %v", err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("write fixture %s: %v", name, err)
		}
	}
	return dir
}

func runningToolNames(frames []sseFrame) []string {
	var names []string
	for _, frame := range frames {
		if frame.Event != "tool" {
			continue
		}
		var payload struct {
			Tool   string `json:"tool"`
			Status string `json:"status"`
		}
		if json.Unmarshal(frame.Data, &payload) == nil &&
			payload.Status == "running" && payload.Tool != "" {
			names = append(names, payload.Tool)
		}
	}
	return names
}

func doneReply(frames []sseFrame) string {
	for _, frame := range frames {
		if frame.Event != "done" {
			continue
		}
		var payload struct {
			Reply string `json:"reply"`
		}
		if json.Unmarshal(frame.Data, &payload) == nil {
			return payload.Reply
		}
	}
	return ""
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
