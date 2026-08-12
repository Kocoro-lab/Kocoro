package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
const progressNoteEffortTier = "agent:\n  effort_tier: max\n"

// With 100% vs 0% separation, 4 samples requiring 2 hits passes ~99% of the
// time on a healthy prompt and catches a fully-collapsed one every time.
//
// An earlier 8/3 threshold was calibrated against a different measurement
// (67% vs 17%, taken on one developer's pinned model) and would have failed on
// the product defaults. Recalibrating it is the whole reason this file pins
// both the tier and the sample size instead of inheriting whatever the machine
// happens to be configured for.
const (
	progressNoteSamples = 4
	progressNoteMinRuns = 2
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
	daemon := startIsolatedLiveDaemon(t, bin, progressNoteEffortTier)
	probeDir := writeProgressNoteFixture(t)

	prompt := fmt.Sprintf(
		"Go through %s step by step: first list the files, then read the first "+
			"lines of the largest one, then summarize what you found.", probeDir)

	runsWithNote := 0
	for i := 0; i < progressNoteSamples; i++ {
		resp := httpPost(t, daemon.baseURL+"/message", map[string]interface{}{"text": prompt})
		sessionID, _ := resp["session_id"].(string)
		if sessionID == "" {
			t.Fatalf("run %d: no session_id in response: %v", i, resp)
		}
		session := httpGet(t, fmt.Sprintf("%s/sessions/%s", daemon.baseURL, sessionID))
		notes := midRunAssistantNotes(session)
		if len(notes) > 0 {
			runsWithNote++
			t.Logf("run %d: %d mid-run note(s), first: %q", i, len(notes), truncateForLog(notes[0]))
		} else {
			t.Logf("run %d: silent until the final answer", i)
		}
	}

	if runsWithNote < progressNoteMinRuns {
		t.Errorf("only %d/%d multi-step runs surfaced a mid-run progress note (want >= %d); "+
			"the ## Communication progress-note trigger has probably narrowed again",
			runsWithNote, progressNoteSamples, progressNoteMinRuns)
	}
}

// midRunAssistantNotes returns user-visible text emitted while the turn is still
// working: text blocks on assistant messages that also carry a tool call. The
// final answer is excluded by construction, since it has no tool_use beside it.
func midRunAssistantNotes(session map[string]interface{}) []string {
	messages, _ := session["messages"].([]interface{})
	var notes []string
	for _, raw := range messages {
		message, ok := raw.(map[string]interface{})
		if !ok || message["role"] != "assistant" {
			continue
		}
		blocks, ok := message["content"].([]interface{})
		if !ok {
			continue // plain string content is the final answer
		}
		var text []string
		hasToolUse := false
		for _, rawBlock := range blocks {
			block, ok := rawBlock.(map[string]interface{})
			if !ok {
				continue
			}
			// tool_use blocks carry an input object and omit an explicit type.
			if _, isToolUse := block["input"]; isToolUse {
				hasToolUse = true
				continue
			}
			if block["type"] == "text" {
				if body := strings.TrimSpace(fmt.Sprint(block["text"])); body != "" {
					text = append(text, body)
				}
			}
		}
		if hasToolUse {
			notes = append(notes, text...)
		}
	}
	return notes
}

func writeProgressNoteFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
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
