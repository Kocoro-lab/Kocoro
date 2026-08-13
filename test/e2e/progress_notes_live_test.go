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

// progressNoteEffortTier pins the run to the Max reasoning tier, because that
// is where this behavior is observable at all. Measured rates for the same
// probe, 6 runs per cell:
//
//	                        default tier      effort_tier: max
//	main's prompt              1/6 = 17%           6/6 = 100%
//	the compressed prompt      0/6 =  0%           0/6 =   0%
//
// At the default tier both arms sit near the floor, so a threshold there would
// either pass everything or flake on healthy builds. Max separates them
// completely. It is also a shipped product tier (Deep/Max in the UI), not an
// internal knob, so this measures a configuration real users select.
const progressNoteEffortTier = "max"

// Six real-provider samples are still a bounded release probe, not a population
// estimate. Require a clear majority and report the observed rate, latency, and
// cost instead of deriving an unsupported confidence claim from 6/6.
const (
	progressNoteSamples = 6
	progressNoteMinRuns = 4
)

// TestLive_MidRunProgressNotesReachTheUser is a behavior contract, not a string
// assertion: it runs real multi-step turns and checks that the user hears
// something before the final answer.
//
// It exists because no test in this repository could see the regression it
// guards. The prompt still contained a progress-note rule, so every phrase
// assertion passed; the ## Text output section had simply been compressed into
// one bullet, and the model went silent through ordinary multi-step work. Three
// attempts to fix that by rewording the bullet recovered 0/6, 2/6 and 2/6 runs;
// restoring the section verbatim recovered 6/6. The behavior comes from the
// section as a whole, which is exactly the kind of thing only a running test
// can tell you.
func TestLive_MidRunProgressNotesReachTheUser(t *testing.T) {
	skipUnlessLive(t)
	bin := testBinary(t)
	daemon := startIsolatedLiveDaemon(t, bin, isolatedLiveOptions{
		EffortTier:  progressNoteEffortTier,
		AutoApprove: true, // bounded fixture reads; no external or paid tools requested
	})
	probeDir := writeProgressNoteFixture(t)

	prompt := fmt.Sprintf(
		"Go through %s step by step: first list the files, then read the first "+
			"lines of the largest one, then summarize what you found.", probeDir)

	runsWithNote := 0
	var totalCost float64
	var totalLatency time.Duration
	for i := 0; i < progressNoteSamples; i++ {
		run := streamMessage(t, daemon.baseURL, map[string]interface{}{"text": prompt, "source": "kocoro"})
		totalCost += run.CostUSD
		totalLatency += run.Duration
		notes := deliveredMidRunNotes(run.Frames)
		if len(notes) > 0 {
			runsWithNote++
			t.Logf("run %d: delivered=%d latency=%s cost=$%.6f first=%q", i+1, len(notes), run.Duration.Round(time.Millisecond), run.CostUSD, truncateForLog(notes[0]))
		} else {
			t.Logf("run %d: delivered=0 latency=%s cost=$%.6f events=%v", i+1, run.Duration.Round(time.Millisecond), run.CostUSD, sseEventNames(run.Frames))
		}
	}
	t.Logf("observed progress delivery=%d/%d total_latency=%s total_cost=$%.6f", runsWithNote, progressNoteSamples, totalLatency.Round(time.Millisecond), totalCost)

	if runsWithNote < progressNoteMinRuns {
		t.Errorf("only %d/%d multi-step runs surfaced a mid-run progress note (want >= %d); "+
			"the ## Text output progress-note contract has probably regressed",
			runsWithNote, progressNoteSamples, progressNoteMinRuns)
	}
}

// deliveredMidRunNotes reads the real per-request wire. A note counts only if
// assistant_text reached the SSE client before done and a later tool event
// proves the run was still working after that delivery.
func deliveredMidRunNotes(frames []sseFrame) []string {
	var notes []string
	for i, frame := range frames {
		if frame.Event != "assistant_text" {
			continue
		}
		toolAfter := false
		for _, later := range frames[i+1:] {
			if later.Event == "done" {
				break
			}
			if later.Event == "tool" {
				toolAfter = true
				break
			}
		}
		if !toolAfter {
			continue
		}
		var payload struct {
			Text string `json:"text"`
		}
		if json.Unmarshal(frame.Data, &payload) == nil {
			if text := strings.TrimSpace(payload.Text); text != "" {
				notes = append(notes, text)
			}
		}
	}
	return notes
}

func writeProgressNoteFixture(t *testing.T) string {
	t.Helper()
	dir := neutralTempDir(t, "project-notes-*")
	files := map[string]string{
		"notes.md":   "latency review notes\n",
		"a.txt":      "alpha\n",
		"report.txt": strings.Repeat("99th percentile latency sample line\n", 400),
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatalf("write fixture %s: %v", name, err)
		}
	}
	return dir
}

func truncateForLog(s string) string {
	const max = 90
	runes := []rune(strings.ReplaceAll(s, "\n", " "))
	if len(runes) <= max {
		return string(runes)
	}
	return string(runes[:max]) + "…"
}
