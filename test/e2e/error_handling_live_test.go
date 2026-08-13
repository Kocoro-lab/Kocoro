//go:build live

package e2e

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

const (
	errorHandlingSamples = 6
	errorHandlingMinRuns = 5
)

// TestLive_BusinessErrorStopsWithoutRetry is a behavior probe. The request is
// phrased as an ordinary filesystem task: it does not name a tool or tell the
// model how to react to failure. The assertion watches the real daemon stream
// for the chosen tool path, the classified error, any identical retry, and the
// final user-facing answer.
func TestLive_BusinessErrorStopsWithoutRetry(t *testing.T) {
	skipUnlessLive(t)
	daemon := startIsolatedLiveDaemon(t, testBinary(t), isolatedLiveOptions{
		EffortTier:  "max",
		AutoApprove: true,
	})
	probeDir := neutralTempDir(t, "observatory-archive-*")
	missingFile := filepath.Join(probeDir, "field-notes.txt")
	prompt := fmt.Sprintf(
		"Read the field notes at %s and summarize them for me.",
		missingFile,
	)

	passingRuns := 0
	var totalCost float64
	var totalLatency time.Duration
	for i := 0; i < errorHandlingSamples; i++ {
		run := streamMessage(t, daemon.baseURL, map[string]interface{}{
			"text":   prompt,
			"source": "kocoro",
		})
		totalCost += run.CostUSD
		totalLatency += run.Duration

		toolFrames := decodeLiveToolFrames(run.Frames)
		sawBusiness := false
		for _, frame := range toolFrames {
			if frame.Status == "completed" && frame.IsError &&
				strings.Contains(frame.Preview, "[business error]") {
				sawBusiness = true
				break
			}
		}
		repeated := repeatedRunningToolCalls(toolFrames)
		bounded := toolFlowStayedWithinFileScope(toolFrames, probeDir)
		answer := doneReply(run.Frames)
		honest := answerReportsMissingScope(answer, filepath.Base(missingFile))
		userFacing := !strings.Contains(answer, "file_read") &&
			!strings.Contains(answer, "[business error]")
		passed := sawBusiness && len(repeated) == 0 && bounded && honest && userFacing
		if passed {
			passingRuns++
		}
		t.Logf(
			"run %d: passed=%t business=%t bounded=%t honest=%t user_facing=%t repeated=%v flow=%v latency=%s cost=$%.6f answer=%q",
			i+1,
			passed,
			sawBusiness,
			bounded,
			honest,
			userFacing,
			repeated,
			liveToolFlow(toolFrames),
			run.Duration.Round(time.Millisecond),
			run.CostUSD,
			truncateForLog(answer),
		)
	}
	t.Logf(
		"observed classified-error handling=%d/%d total_latency=%s total_cost=$%.6f",
		passingRuns,
		errorHandlingSamples,
		totalLatency.Round(time.Millisecond),
		totalCost,
	)
	if passingRuns < errorHandlingMinRuns {
		t.Errorf(
			"only %d/%d runs surfaced a business error, stayed within the named file scope without shell fallback, avoided a retry, and reported the missing scope without internal mechanics (want >= %d)",
			passingRuns,
			errorHandlingSamples,
			errorHandlingMinRuns,
		)
	}
}

func toolFlowStayedWithinFileScope(frames []liveToolFrame, scopeDir string) bool {
	running := 0
	for _, frame := range frames {
		if frame.Status != "running" {
			continue
		}
		running++
		if running > 2 || frame.Tool == "bash" || !strings.Contains(frame.Args, scopeDir) {
			return false
		}
	}
	return running > 0
}

type liveToolFrame struct {
	Tool      string `json:"tool"`
	ToolUseID string `json:"tool_use_id"`
	Status    string `json:"status"`
	Args      string `json:"args"`
	Preview   string `json:"preview"`
	IsError   bool   `json:"is_error"`
}

func decodeLiveToolFrames(frames []sseFrame) []liveToolFrame {
	decoded := make([]liveToolFrame, 0)
	for _, frame := range frames {
		if frame.Event != "tool" {
			continue
		}
		var payload liveToolFrame
		if json.Unmarshal(frame.Data, &payload) == nil {
			decoded = append(decoded, payload)
		}
	}
	return decoded
}

func repeatedRunningToolCalls(frames []liveToolFrame) []string {
	counts := make(map[string]int)
	for _, frame := range frames {
		if frame.Status != "running" {
			continue
		}
		key := frame.Tool + " " + canonicalToolArgs(frame.Args)
		counts[key]++
	}
	var repeated []string
	for key, count := range counts {
		if count > 1 {
			repeated = append(repeated, fmt.Sprintf("%s x%d", key, count))
		}
	}
	sort.Strings(repeated)
	return repeated
}

func canonicalToolArgs(raw string) string {
	var value interface{}
	if json.Unmarshal([]byte(raw), &value) != nil {
		return strings.TrimSpace(raw)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return strings.TrimSpace(raw)
	}
	return string(encoded)
}

func answerReportsMissingScope(answer, scopeBase string) bool {
	lower := strings.ToLower(answer)
	scopeNamed := strings.Contains(lower, strings.ToLower(scopeBase))
	for _, descriptor := range []string{"field notes", "file", "path"} {
		if strings.Contains(lower, descriptor) {
			scopeNamed = true
			break
		}
	}
	if !scopeNamed {
		return false
	}
	for _, phrase := range []string{
		"not found",
		"no such file",
		"does not exist",
		"doesn't exist",
		"could not access",
		"couldn't access",
		"cannot access",
		"can't access",
		"unable to access",
		"missing",
	} {
		if strings.Contains(lower, phrase) {
			return true
		}
	}
	return false
}

func liveToolFlow(frames []liveToolFrame) []string {
	flow := make([]string, 0, len(frames))
	for _, frame := range frames {
		entry := frame.Tool + "/" + frame.Status
		if frame.IsError {
			entry += "/error"
		}
		if frame.Status == "completed" && frame.Preview != "" {
			entry += ":" + truncateForLog(frame.Preview)
		}
		flow = append(flow, entry)
	}
	return flow
}
