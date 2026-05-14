package agent

import (
	"fmt"
	"strings"
	"testing"
)

func TestLoopDetector_ConsecutiveDup_Nudge(t *testing.T) {
	ld := NewLoopDetector()

	// 1 call: no trigger
	ld.Record("web_search", `{"q":"test"}`, false, "", "", false)
	action, _ := ld.Check("web_search")
	if action != LoopContinue {
		t.Errorf("1 call should not trigger, got %v", action)
	}

	// 2nd consecutive identical call: no trigger yet (consecDupThreshold=3)
	ld.Record("web_search", `{"q":"test"}`, false, "", "", false)
	action, _ = ld.Check("web_search")
	if action != LoopContinue {
		t.Errorf("2 consecutive identical calls should not trigger (consecDupThreshold=3), got %v", action)
	}

	// 3rd consecutive identical call: nudge (consecDupThreshold=3)
	ld.Record("web_search", `{"q":"test"}`, false, "", "", false)
	action, msg := ld.Check("web_search")
	if action != LoopNudge {
		t.Errorf("3 consecutive identical calls should nudge, got %v", action)
	}
	if msg == "" {
		t.Error("nudge should have a message")
	}
}

func TestLoopDetector_ConsecutiveDup_ForceStop(t *testing.T) {
	ld := NewLoopDetector()

	// 4 consecutive identical calls: force stop (consecDupThreshold+1=4)
	for range 4 {
		ld.Record("web_search", `{"q":"test"}`, false, "", "", false)
	}
	action, _ := ld.Check("web_search")
	if action != LoopForceStop {
		t.Errorf("4 consecutive identical calls should force stop, got %v", action)
	}
}

func TestLoopDetector_NonConsecutiveDup_NoFalsePositive(t *testing.T) {
	ld := NewLoopDetector()

	// read → edit → read: NOT consecutive, 2 in window < exactDupThreshold(3)
	ld.Record("file_read", `{"file":"main.go"}`, false, "", "", false)
	ld.Record("file_edit", `{"file":"main.go","old":"a","new":"b"}`, false, "", "", false)
	ld.Record("file_read", `{"file":"main.go"}`, false, "", "", false)

	action, _ := ld.Check("file_read")
	if action != LoopContinue {
		t.Errorf("read-edit-read should not trigger (non-consecutive), got %v", action)
	}
}

func TestLoopDetector_WindowDup_Nudge(t *testing.T) {
	ld := NewLoopDetector()

	// 5 spread-out identical calls: window-based nudge (exactDupThreshold=5)
	ld.Record("file_read", `{"file":"main.go"}`, false, "", "", false)
	ld.Record("file_edit", `{"old":"a","new":"b"}`, false, "", "", false)
	ld.Record("file_read", `{"file":"main.go"}`, false, "", "", false)
	ld.Record("file_edit", `{"old":"b","new":"c"}`, false, "", "", false)
	ld.Record("file_read", `{"file":"main.go"}`, false, "", "", false)
	ld.Record("file_edit", `{"old":"c","new":"d"}`, false, "", "", false)
	ld.Record("file_read", `{"file":"main.go"}`, false, "", "", false)
	ld.Record("file_edit", `{"old":"d","new":"e"}`, false, "", "", false)
	ld.Record("file_read", `{"file":"main.go"}`, false, "", "", false)

	action, _ := ld.Check("file_read")
	if action != LoopNudge {
		t.Errorf("5 spread-out identical calls should trigger window nudge, got %v", action)
	}
}

func TestLoopDetector_WindowDup_ForceStop(t *testing.T) {
	ld := NewLoopDetector()

	// 10 spread-out identical calls: window force stop (2× exactDupThreshold=10)
	for range 10 {
		ld.Record("file_read", `{"file":"main.go"}`, false, "", "", false)
		ld.Record("file_edit", `{"x":"y"}`, false, "", "", false)
	}
	action, _ := ld.Check("file_read")
	if action != LoopForceStop {
		t.Errorf("10 spread-out identical calls should force stop, got %v", action)
	}
}

func TestLoopDetector_SameToolError_Nudge(t *testing.T) {
	ld := NewLoopDetector()

	// 5 errors: no trigger (threshold is 6)
	for i := range 5 {
		ld.Record("file_edit", fmt.Sprintf(`{"file":"f%d"}`, i), true, "permission denied", "", false)
	}
	action, _ := ld.Check("file_edit")
	if action != LoopContinue {
		t.Errorf("5 errors should not trigger, got %v", action)
	}

	// 6th error: nudge
	ld.Record("file_edit", `{"file":"f5"}`, true, "permission denied", "", false)
	action, msg := ld.Check("file_edit")
	if action != LoopNudge {
		t.Errorf("6 errors should trigger nudge, got %v", action)
	}
	if msg == "" {
		t.Error("nudge should have a message")
	}
}

func TestLoopDetector_SameToolError_ForceStop(t *testing.T) {
	ld := NewLoopDetector()

	// 12 errors: force stop (2× threshold of 6)
	for i := range 12 {
		ld.Record("file_edit", fmt.Sprintf(`{"file":"f%d"}`, i), true, "permission denied", "", false)
	}
	action, _ := ld.Check("file_edit")
	if action != LoopForceStop {
		t.Errorf("12 errors should trigger force stop, got %v", action)
	}
}

func TestLoopDetector_NoProgress_Nudge(t *testing.T) {
	ld := NewLoopDetector()

	// 11 calls with different args: no trigger (threshold is 12)
	// Use think (not in any tool family, not semi-repeatable) to test pure
	// NoProgress detection. bash is semi-repeatable (threshold 16) so it
	// wouldn't trigger at 12.
	for i := range 11 {
		ld.Record("think", fmt.Sprintf(`{"thought":"idea%d"}`, i), false, "", "", false)
	}
	action, _ := ld.Check("think")
	if action != LoopContinue {
		t.Errorf("11 calls should not trigger, got %v", action)
	}

	// 12th call: nudge
	ld.Record("think", `{"thought":"idea12"}`, false, "", "", false)
	action, _ = ld.Check("think")
	if action != LoopNudge {
		t.Errorf("12 calls should trigger nudge, got %v", action)
	}
}

func TestLoopDetector_GUIExemptFromNoProgress(t *testing.T) {
	ld := NewLoopDetector()

	// 10 screenshot calls with different args: should NOT trigger NoProgress
	for i := range 10 {
		ld.Record("screenshot", fmt.Sprintf(`{"delay":%d}`, i), false, "", "", false)
	}
	action, _ := ld.Check("screenshot")
	if action != LoopContinue {
		t.Errorf("screenshot should be exempt from NoProgress, got %v", action)
	}
}

func TestLoopDetector_GUIConsecutiveDupStillDetected(t *testing.T) {
	ld := NewLoopDetector()

	// Even GUI tools should trigger consecutive-duplicate detection
	// consecDupThreshold=3 → nudge at 3 consecutive identical calls
	ld.Record("screenshot", `{}`, false, "", "", false)
	ld.Record("screenshot", `{}`, false, "", "", false)
	action, _ := ld.Check("screenshot")
	if action != LoopContinue {
		t.Errorf("2 consecutive identical screenshot calls should not trigger (consecDupThreshold=3), got %v", action)
	}

	ld.Record("screenshot", `{}`, false, "", "", false)
	action, _ = ld.Check("screenshot")
	if action != LoopNudge {
		t.Errorf("3 consecutive identical screenshot calls should nudge, got %v", action)
	}
}

func TestLoopDetector_SlidingWindow(t *testing.T) {
	ld := NewLoopDetector()
	ld.historySize = 5 // small window for testing

	// Fill window with 3 consecutive bash duplicates (triggers consecutive nudge at consecDupThreshold=3)
	ld.Record("bash", `{"cmd":"ls"}`, false, "", "", false)
	ld.Record("bash", `{"cmd":"ls"}`, false, "", "", false)
	ld.Record("bash", `{"cmd":"ls"}`, false, "", "", false)
	action, _ := ld.Check("bash")
	if action != LoopNudge {
		t.Error("3 consecutive exact dups should nudge")
	}

	// Push old records out of window with 5 different calls
	for i := range 5 {
		ld.Record("file_read", fmt.Sprintf(`{"file":"f%d"}`, i), false, "", "", false)
	}

	// bash dups should have fallen out of window
	action, _ = ld.Check("bash")
	if action != LoopContinue {
		t.Error("old records should have fallen out of sliding window")
	}
}

func TestLoopDetector_MixedWorkflow_NoFalsePositive(t *testing.T) {
	ld := NewLoopDetector()

	// Normal coding workflow: read, edit, read, edit, bash
	ld.Record("file_read", `{"file":"main.go"}`, false, "", "", false)
	ld.Record("file_edit", `{"file":"main.go","old":"a","new":"b"}`, false, "", "", false)
	ld.Record("file_read", `{"file":"main.go"}`, false, "", "", false)
	ld.Record("file_edit", `{"file":"main.go","old":"b","new":"c"}`, false, "", "", false)
	ld.Record("bash", `{"cmd":"go test"}`, false, "", "", false)

	for _, name := range []string{"file_read", "file_edit", "bash"} {
		action, _ := ld.Check(name)
		if action != LoopContinue {
			t.Errorf("normal workflow should not trigger for %s, got %v", name, action)
		}
	}
}

func TestLoopDetector_DifferentArgsNoDuplicate(t *testing.T) {
	ld := NewLoopDetector()

	// Same tool, different args each time — should not trigger
	for i := range 5 {
		ld.Record("file_read", fmt.Sprintf(`{"file":"file%d.go"}`, i), false, "", "", false)
	}
	action, _ := ld.Check("file_read")
	if action != LoopContinue {
		t.Errorf("different args should not trigger, got %v", action)
	}
}

func TestLoopDetector_ErrorsOnlyCountForSameTool(t *testing.T) {
	ld := NewLoopDetector()

	// Errors spread across different tools: no trigger for any single tool
	ld.Record("bash", `{"cmd":"a"}`, true, "fail", "", false)
	ld.Record("file_edit", `{"a":"b"}`, true, "fail", "", false)
	ld.Record("grep", `{"p":"c"}`, true, "fail", "", false)
	ld.Record("bash", `{"cmd":"b"}`, true, "fail", "", false)
	ld.Record("file_edit", `{"a":"c"}`, true, "fail", "", false)

	for _, name := range []string{"bash", "file_edit", "grep"} {
		action, _ := ld.Check(name)
		if action != LoopContinue {
			t.Errorf("spread errors should not trigger for %s, got %v", name, action)
		}
	}
}

func TestLoopDetector_WebFamily_SameTopicNudge(t *testing.T) {
	ld := NewLoopDetector()
	// 5 web_search calls all normalizing to the "climate world" topic
	// (only date / filler words differ) → family nudge at 5 (v2 threshold).
	ld.Record("web_search", `{"query":"world climate today March 2 2026 major headlines"}`, false, "", "", false)
	ld.Record("web_search", `{"query":"world climate March 2 2026 top headlines latest"}`, false, "", "", false)
	ld.Record("web_search", `{"query":"world climate today March 2 2026 breaking news"}`, false, "", "", false)
	ld.Record("web_search", `{"query":"world climate latest update March 2 2026"}`, false, "", "", false)
	ld.Record("web_search", `{"query":"world climate top headlines current March 2 2026"}`, false, "", "", false)
	action, msg := ld.Check("web_search")
	if action != LoopNudge {
		t.Errorf("5 same-topic web searches should nudge (FamilyNoProgress v2 threshold), got %v", action)
	}
	if msg == "" {
		t.Error("nudge should have a message")
	}
}

func TestLoopDetector_WebFamily_CrossToolTopicInheritance(t *testing.T) {
	ld := NewLoopDetector()
	// 2 web_search on same topic (only filler/date differences), then web_fetch.
	// Family-level topic lookup should inherit the topic hash from web_search.
	ld.Record("web_search", `{"query":"golang tutorial 2026"}`, false, "", "", false)
	ld.Record("web_search", `{"query":"golang tutorial latest"}`, false, "", "", false)
	ld.Record("web_fetch", `{"url":"https://go.dev/doc/tutorial"}`, false, "", "", false)

	// 2 same-topic (from web_search) + 1 different (web_fetch URL) → not yet 5
	action, _ := ld.Check("web_fetch")
	if action != LoopContinue {
		t.Errorf("2 same-topic + 1 different should continue, got %v", action)
	}

	// Add more same-topic searches until nudge at 5 same-topic in family (v2 threshold).
	// All queries normalize to the "golang tutorial" topic (date/filler stripped).
	ld.Record("web_search", `{"query":"latest golang tutorial today"}`, false, "", "", false)
	ld.Record("web_search", `{"query":"golang tutorial top latest"}`, false, "", "", false)
	ld.Record("web_search", `{"query":"golang tutorial current update"}`, false, "", "", false)
	action, _ = ld.Check("web_search")
	if action != LoopNudge {
		t.Errorf("5 same-topic family calls should nudge (v2 threshold), got %v", action)
	}
}

func TestLoopDetector_WebFamily_ResultSigDedup(t *testing.T) {
	ld := NewLoopDetector()
	// 5 calls returning the same domains → no new info → nudge at 5 (v2 threshold)
	ld.Record("web_search", `{"query":"ai research papers"}`, false, "", "reuters.com,bbc.com", false)
	ld.Record("web_search", `{"query":"ai research latest papers"}`, false, "", "reuters.com,bbc.com", false)
	ld.Record("web_search", `{"query":"ai research papers review"}`, false, "", "reuters.com,bbc.com", false)
	ld.Record("web_search", `{"query":"ai research 2026"}`, false, "", "reuters.com,bbc.com", false)
	ld.Record("web_search", `{"query":"latest ai research papers"}`, false, "", "reuters.com,bbc.com", false)
	action, _ := ld.Check("web_search")
	if action != LoopNudge {
		t.Errorf("5 calls with same result signature should nudge, got %v", action)
	}
}

func TestLoopDetector_WebFamily_AlternatingSearchFetchStillNudges(t *testing.T) {
	ld := NewLoopDetector()

	// Mixed web workflows should still nudge when alternating tools keep
	// returning the same source and no new information is being gathered.
	// v2: nudge at 5 same-result-sig calls in the family.
	ld.Record("web_search", `{"query":"go tutorial official"}`, false, "", "go.dev", false)
	ld.Record("web_fetch", `{"url":"https://go.dev/doc/tutorial"}`, false, "", "go.dev", false)
	ld.Record("web_search", `{"query":"golang tutorial latest official"}`, false, "", "go.dev", false)
	ld.Record("web_fetch", `{"url":"https://go.dev/doc/effective_go"}`, false, "", "go.dev", false)
	ld.Record("web_search", `{"query":"golang official tutorial guide"}`, false, "", "go.dev", false)

	action, _ := ld.Check("web_search")
	if action != LoopNudge {
		t.Errorf("alternating web_search/web_fetch with the same result signature should nudge, got %v", action)
	}
}

func TestLoopDetector_WebFamily_ForceStopAt7(t *testing.T) {
	ld := NewLoopDetector()
	// 7 web calls with same topic → force stop
	for i := 0; i < 7; i++ {
		ld.Record("web_search", `{"query":"climate change report"}`, false, "", "", false)
	}
	action, _ := ld.Check("web_search")
	if action != LoopForceStop {
		t.Errorf("7 same-topic web calls should force stop, got %v", action)
	}
}

func TestLoopDetector_WebFamily_7DifferentTopicsNoForceStop(t *testing.T) {
	ld := NewLoopDetector()
	// 7 web family calls on DIFFERENT topics should NOT force stop
	// (legitimate multi-source research)
	for i := 0; i < 4; i++ {
		ld.Record("web_search", fmt.Sprintf(`{"query":"topic%d search"}`, i), false, "", "", false)
	}
	for i := 0; i < 3; i++ {
		ld.Record("web_fetch", fmt.Sprintf(`{"url":"https://example%d.com/page"}`, i), false, "", "", false)
	}
	action, _ := ld.Check("web_fetch")
	if action == LoopForceStop {
		t.Error("7 web family calls with different topics should NOT force stop")
	}
}

func TestLoopDetector_WebFamily_DifferentTopicsUnder7(t *testing.T) {
	ld := NewLoopDetector()
	// 4 web calls with different topics — should NOT trigger (under 7 total, no topic match)
	ld.Record("web_search", `{"query":"golang concurrency patterns"}`, false, "", "", false)
	ld.Record("web_search", `{"query":"python machine learning tutorial"}`, false, "", "", false)
	ld.Record("web_search", `{"query":"rust ownership explained"}`, false, "", "", false)
	ld.Record("web_search", `{"query":"javascript async await"}`, false, "", "", false)
	action, _ := ld.Check("web_search")
	if action != LoopContinue {
		t.Errorf("4 different-topic web calls should continue, got %v", action)
	}
}

func TestLoopDetector_NonWebToolUnchanged(t *testing.T) {
	ld := NewLoopDetector()
	// 5 file_read calls with different args — should NOT trigger (threshold still 8)
	for i := 0; i < 5; i++ {
		ld.Record("file_read", fmt.Sprintf(`{"file":"file%d.go"}`, i), false, "", "", false)
	}
	action, _ := ld.Check("file_read")
	if action != LoopContinue {
		t.Errorf("5 file_read calls should not trigger (threshold 8), got %v", action)
	}
}

// TestLoopDetector_RealWorldWebLoop replays the actual bug that prompted this fix:
// many web_search calls with varied "world news" queries, then web_fetch calls.
// v2 thresholds: nudge fires at 5 same-topic, force-stop fires at 12 same-topic.
func TestLoopDetector_RealWorldWebLoop(t *testing.T) {
	ld := NewLoopDetector()

	searches := []string{
		`{"query":"world news today March 2 2026"}`,
		`{"query":"world news today March 2 2026 major headlines"}`,
		`{"query":"world news March 2 2026 top headlines Reuters BBC Al Jazeera"}`,
		`{"query":"world news today March 2 2026 top headlines Reuters AP BBC"}`,
		`{"query":"world news March 2 2026 Reuters AP BBC Al Jazeera"}`,
		`{"query":"world news March 2 2026 top headlines"}`,
		`{"query":"world news today March 2 2026 top headlines"}`,
		`{"query":"world news March 2 2026 top headlines Reuters AP BBC Al Jazeera CNN"}`,
		`{"query":"world news March 2 2026 latest updates"}`,
		`{"query":"world news March 2 2026 breaking"}`,
		`{"query":"world news March 2 2026 Reuters AP"}`,
		`{"query":"world news March 2 2026 BBC CNN Al Jazeera"}`,
		`{"query":"world news March 2 2026 top stories"}`,
	}

	var firstNudge, firstForceStop int
	for i, args := range searches {
		ld.Record("web_search", args, false, "", "reuters.com,bbc.com", false)
		action, _ := ld.Check("web_search")
		if action == LoopNudge && firstNudge == 0 {
			firstNudge = i + 1
		}
		if action == LoopForceStop && firstForceStop == 0 {
			firstForceStop = i + 1
		}
	}

	// v2: nudge at progressCount>=5, force-stop at progressCount>=12
	if firstNudge == 0 || firstNudge > 5 {
		t.Errorf("expected first nudge by call 5, got %d", firstNudge)
	}
	if firstForceStop == 0 || firstForceStop > 12 {
		t.Errorf("expected force stop by call 12, got %d", firstForceStop)
	}
}

// TestLoopDetector_RealWorldWebLoop_CrossTool verifies that switching from
// web_search to web_fetch doesn't reset the family counter.
func TestLoopDetector_ToolModeSwitch_NudgeOnGUIAfterSuccess(t *testing.T) {
	ld := NewLoopDetector()

	// Successful non-GUI call followed by GUI call → nudge
	ld.Record("applescript", `{"script":"create event"}`, false, "", "", false)
	action, _ := ld.Check("applescript")
	if action != LoopContinue {
		t.Errorf("single successful call should continue, got %v", action)
	}

	ld.Record("screenshot", `{"target":"screen"}`, false, "", "", false)
	action, msg := ld.Check("screenshot")
	if action != LoopNudge {
		t.Errorf("GUI call after successful non-GUI should nudge, got %v", action)
	}
	if msg == "" {
		t.Error("nudge should have a message")
	}
}

func TestLoopDetector_ToolModeSwitch_NoNudgeAfterError(t *testing.T) {
	ld := NewLoopDetector()

	// Failed non-GUI call followed by GUI call → no nudge (GUI verification warranted)
	ld.Record("applescript", `{"script":"create event"}`, true, "calendar not found", "", false)
	ld.Record("screenshot", `{"target":"screen"}`, false, "", "", false)
	action, _ := ld.Check("screenshot")
	if action != LoopContinue {
		t.Errorf("GUI after failed non-GUI should continue (verification warranted), got %v", action)
	}
}

func TestLoopDetector_ToolModeSwitch_NoNudgeForGUIOnlyTask(t *testing.T) {
	ld := NewLoopDetector()

	// Task starts with GUI tools — no non-GUI success to trigger on
	ld.Record("screenshot", `{"target":"screen"}`, false, "", "", false)
	ld.Record("computer", `{"action":"click","coordinate":[100,200]}`, false, "", "", false)
	ld.Record("screenshot", `{"target":"screen"}`, false, "", "", false)
	action, _ := ld.Check("screenshot")
	if action != LoopContinue {
		t.Errorf("GUI-only task should not trigger mode switch, got %v", action)
	}
}

func TestLoopDetector_ToolModeSwitch_NudgeOnlyOnce(t *testing.T) {
	ld := NewLoopDetector()

	// Successful non-GUI → GUI nudge → second GUI should NOT nudge again
	ld.Record("applescript", `{"script":"create event"}`, false, "", "", false)
	ld.Record("screenshot", `{"target":"screen"}`, false, "", "", false)
	action, _ := ld.Check("screenshot")
	if action != LoopNudge {
		t.Errorf("first GUI after success should nudge, got %v", action)
	}

	ld.Record("computer", `{"action":"click","coordinate":[100,200]}`, false, "", "", false)
	action, _ = ld.Check("computer")
	if action != LoopContinue {
		t.Errorf("second GUI should not re-nudge (already nudged), got %v", action)
	}
}

func TestLoopDetector_ToolModeSwitch_ResetsOnNewNonGUI(t *testing.T) {
	ld := NewLoopDetector()

	// Success → GUI nudge → new GUI-adjacent success → GUI nudge again (new mode switch)
	ld.Record("applescript", `{"script":"create event"}`, false, "", "", false)
	ld.Record("screenshot", `{"target":"screen"}`, false, "", "", false)
	action, _ := ld.Check("screenshot")
	if action != LoopNudge {
		t.Errorf("first mode switch should nudge, got %v", action)
	}

	// New GUI-adjacent success resets the detector
	ld.Record("browser", `{"action":"navigate","url":"http://example.com"}`, false, "", "", false)
	ld.Record("screenshot", `{"target":"screen"}`, false, "", "", false)
	action, _ = ld.Check("screenshot")
	if action != LoopNudge {
		t.Errorf("new mode switch after reset should nudge again, got %v", action)
	}
}

func TestLoopDetector_ToolModeSwitch_NoNudgeAfterNonGUITool(t *testing.T) {
	ld := NewLoopDetector()

	// Non-GUI tool (bash, file_read, etc.) success → screenshot should NOT trigger
	// mode switch since these aren't GUI-adjacent tools.
	ld.Record("bash", `{"command":"echo hello"}`, false, "", "", false)
	ld.Record("screenshot", `{"target":"screen"}`, false, "", "", false)
	action, _ := ld.Check("screenshot")
	if action != LoopContinue {
		t.Errorf("screenshot after bash should not trigger mode switch, got %v", action)
	}
}

func TestLoopDetector_RealWorldWebLoop_CrossTool(t *testing.T) {
	ld := NewLoopDetector()

	// All queries normalize to the "climate world" topic — only filler / date
	// variations so the topic hash stays stable across all calls.
	// 5 searches on same topic → nudge at 5 (v2 threshold)
	ld.Record("web_search", `{"query":"world climate today March 2 2026"}`, false, "", "", false)
	ld.Record("web_search", `{"query":"world climate March 2 2026 latest"}`, false, "", "", false)
	ld.Record("web_search", `{"query":"world climate today latest headlines"}`, false, "", "", false)
	ld.Record("web_search", `{"query":"world climate top breaking news"}`, false, "", "", false)
	ld.Record("web_search", `{"query":"world climate current update major"}`, false, "", "", false)

	action, _ := ld.Check("web_search")
	if action != LoopNudge {
		t.Errorf("expected nudge after 5 same-topic searches, got %v", action)
	}

	// Switch to web_fetch then back — same-topic counter continues via family lookup.
	// web_fetch URL is treated as its own topic, then we add more same-topic searches.
	ld.Record("web_fetch", `{"url":"https://reuters.com/world/climate"}`, false, "", "", false)
	ld.Record("web_search", `{"query":"world climate today"}`, false, "", "", false)
	ld.Record("web_search", `{"query":"world climate latest"}`, false, "", "", false)
	ld.Record("web_search", `{"query":"world climate top current"}`, false, "", "", false)

	// 5 original + 3 more same-topic web_search = 8 same-topic → stronger nudge (stage 1)
	action, _ = ld.Check("web_search")
	if action != LoopNudge {
		t.Errorf("expected nudge after 8 same-topic web calls, got %v", action)
	}

	// Add more same-topic calls until force stop at progressCount >= 12.
	ld.Record("web_search", `{"query":"world climate breaking"}`, false, "", "", false)
	ld.Record("web_search", `{"query":"world climate update"}`, false, "", "", false)
	ld.Record("web_search", `{"query":"world climate news today"}`, false, "", "", false)
	ld.Record("web_search", `{"query":"world climate headlines major"}`, false, "", "", false)
	action, _ = ld.Check("web_search")
	if action != LoopForceStop {
		t.Errorf("expected force stop after 12 same-topic web calls, got %v", action)
	}
}

func TestLoopDetector_SuccessAfterError_NudgeOnPostRecoveryGUI(t *testing.T) {
	ld := NewLoopDetector()

	// Tool fails, then succeeds with different args, then agent goes to GUI → nudge
	ld.Record("applescript", `{"script":"tell calendar \"Calendar\""}`, true, "calendar not found", "", false)
	ld.Record("applescript", `{"script":"get name of every calendar"}`, false, "", "", false)
	ld.Record("applescript", `{"script":"tell calendar \"日历\""}`, false, "", "", false)

	// Now agent switches to GUI to verify — should nudge
	ld.Record("screenshot", `{"target":"screen"}`, false, "", "", false)
	action, msg := ld.Check("screenshot")
	if action != LoopNudge {
		t.Errorf("GUI after recovery should nudge, got %v", action)
	}
	if msg == "" {
		t.Error("nudge should have a message")
	}
}

func TestLoopDetector_SuccessAfterError_NoNudgeIfNoRecovery(t *testing.T) {
	ld := NewLoopDetector()

	// Tool fails, no retry yet, agent takes screenshot → no nudge from this detector
	// (ToolModeSwitch won't fire either since last non-GUI was an error)
	ld.Record("applescript", `{"script":"tell calendar \"Calendar\""}`, true, "not found", "", false)
	ld.Record("screenshot", `{"target":"screen"}`, false, "", "", false)
	action, _ := ld.Check("screenshot")
	if action != LoopContinue {
		t.Errorf("no recovery happened, should continue, got %v", action)
	}
}

func TestLoopDetector_SuccessAfterError_ResetsOnNewWork(t *testing.T) {
	ld := NewLoopDetector()

	// Recovery happens, then agent moves on to genuinely different work
	ld.Record("applescript", `{"script":"tell calendar \"Calendar\""}`, true, "not found", "", false)
	ld.Record("applescript", `{"script":"tell calendar \"日历\""}`, false, "", "", false)

	// Agent moves to a different non-GUI tool → recovery state resets
	ld.Record("bash", `{"command":"echo done"}`, false, "", "", false)
	ld.Record("file_read", `{"path":"notes.md"}`, false, "", "", false)

	// GUI now should NOT nudge for recovery (agent moved on)
	// Note: ToolModeSwitch may nudge since file_read succeeded — that's a different detector
	// We specifically check that the nudge message does NOT mention recovery
	ld.Record("screenshot", `{"target":"screen"}`, false, "", "", false)
	action, msg := ld.Check("screenshot")
	// It may nudge from ToolModeSwitch, but NOT from SuccessAfterError
	if action == LoopNudge && strings.Contains(msg, "recovered") {
		t.Errorf("recovery should have reset, but got recovery nudge: %s", msg)
	}
}

// TestLoopDetector_ParallelSleepBashTask_NoFalsePositive is the
// regression guard for the removed sleep detector. The user case that
// motivated the removal: "run 3 sleep commands in parallel (same turn)"
// produced 3 bash records with `sleep N` and tripped the old 4-count
// threshold once the model retried once. Now: a legitimate parallel
// sleep task must not force-stop or nudge through any detector. Real
// polling is covered by ExactDup (same command repeated) and NoProgress
// (many bash calls without progress) elsewhere.
func TestLoopDetector_ParallelSleepBashTask_NoFalsePositive(t *testing.T) {
	ld := NewLoopDetector()
	// 3 distinct parallel-sleep commands (one assistant turn, 3 tool_use).
	ld.Record("bash", `{"command":"sleep 2 && echo task1_done"}`, false, "", "", false)
	ld.Record("bash", `{"command":"sleep 2 && echo task2_done"}`, false, "", "", false)
	ld.Record("bash", `{"command":"sleep 2 && echo task3_done"}`, false, "", "", false)
	action, msg := ld.Check("bash")
	if action != LoopContinue {
		t.Errorf("3 parallel distinct sleep commands must not fire any detector, got %v (msg=%q)", action, msg)
	}
	// Plus a fourth distinct sleep (e.g. cleanup verify). Still fine.
	ld.Record("bash", `{"command":"sleep 1 && ls /tmp/output"}`, false, "", "", false)
	action, msg = ld.Check("bash")
	if action != LoopContinue {
		t.Errorf("4th distinct sleep command must still continue (no naked sleep counter), got %v (msg=%q)", action, msg)
	}
}

// TestLoopDetector_TrueSleepPolling_StillCaughtByExistingDetectors
// confirms that removing the dedicated sleep detector does not blind us
// to genuine polling: same `sleep N && check_status` repeated trips the
// existing ConsecutiveDup / ExactDup detectors via the args hash. After
// 3 identical calls the ConsecutiveDup detector nudges (threshold = 3),
// after 4 it force-stops (consecDupThreshold + 1). For bash specifically
// this is the right behavior — the model is asking the same question of
// the system repeatedly and getting the same answer.
func TestLoopDetector_TrueSleepPolling_StillCaughtByExistingDetectors(t *testing.T) {
	cmd := `{"command":"sleep 5 && curl -s http://localhost:8080/status"}`

	t.Run("3 identical → nudge", func(t *testing.T) {
		ld := NewLoopDetector()
		for i := 0; i < 3; i++ {
			ld.Record("bash", cmd, false, "", "", false)
		}
		action, msg := ld.Check("bash")
		if action != LoopNudge {
			t.Errorf("3 identical polling calls should nudge, got action=%v msg=%q", action, msg)
		}
	})

	t.Run("4 identical → force-stop", func(t *testing.T) {
		ld := NewLoopDetector()
		for i := 0; i < 4; i++ {
			ld.Record("bash", cmd, false, "", "", false)
		}
		action, msg := ld.Check("bash")
		if action != LoopForceStop {
			t.Errorf("4 identical polling calls should force-stop, got action=%v msg=%q", action, msg)
		}
	})
}

func TestLoopDetector_SearchEscalation_Nudge(t *testing.T) {
	ld := NewLoopDetector()

	// 6 consecutive unproductive search calls: no trigger yet (threshold is 7)
	for i := 0; i < 6; i++ {
		ld.Record("grep", fmt.Sprintf(`{"pattern":"term%d"}`, i), false, "", "", true)
	}
	action, _ := ld.Check("grep")
	if action != LoopContinue {
		t.Errorf("6 unproductive search calls should not trigger, got %v", action)
	}

	// 7th unproductive search call: nudge
	ld.Record("grep", `{"pattern":"term6"}`, false, "", "", true)
	action, msg := ld.Check("grep")
	if action != LoopNudge {
		t.Errorf("7 unproductive search calls should nudge, got %v", action)
	}
	if msg == "" {
		t.Error("nudge should have a message")
	}
}

func TestLoopDetector_SearchEscalation_ForceStop(t *testing.T) {
	ld := NewLoopDetector()

	// 12 consecutive unproductive search calls (mixed grep/glob): force stop
	for i := 0; i < 12; i++ {
		tool := "grep"
		if i%2 == 1 {
			tool = "glob"
		}
		ld.Record(tool, fmt.Sprintf(`{"pattern":"term%d"}`, i), false, "", "", true)
	}
	action, _ := ld.Check("glob")
	if action != LoopForceStop {
		t.Errorf("12 unproductive search calls should force stop, got %v", action)
	}
}

func TestLoopDetector_SearchEscalation_NoFalsePositive(t *testing.T) {
	ld := NewLoopDetector()

	// grep interspersed with file_edit: no consecutive run builds up
	ld.Record("grep", `{"pattern":"foo"}`, false, "", "", false)
	ld.Record("file_edit", `{"file":"main.go","old":"a","new":"b"}`, false, "", "", false)
	ld.Record("grep", `{"pattern":"bar"}`, false, "", "", false)
	ld.Record("file_edit", `{"file":"main.go","old":"b","new":"c"}`, false, "", "", false)
	ld.Record("grep", `{"pattern":"baz"}`, false, "", "", false)

	action, _ := ld.Check("grep")
	if action != LoopContinue {
		t.Errorf("grep interspersed with edits should not trigger search escalation, got %v", action)
	}
}

func TestLoopDetector_SearchEscalation_MixedSearchTools(t *testing.T) {
	ld := NewLoopDetector()

	// 7 unproductive mixed grep+glob calls: nudge (v2 threshold)
	ld.Record("grep", `{"pattern":"foo"}`, false, "", "", true)
	ld.Record("glob", `{"pattern":"**/*.go"}`, false, "", "", true)
	ld.Record("grep", `{"pattern":"bar"}`, false, "", "", true)
	ld.Record("glob", `{"pattern":"**/*.ts"}`, false, "", "", true)
	ld.Record("grep", `{"pattern":"baz"}`, false, "", "", true)
	ld.Record("glob", `{"pattern":"**/*.json"}`, false, "", "", true)
	ld.Record("grep", `{"pattern":"qux"}`, false, "", "", true)

	action, msg := ld.Check("grep")
	if action != LoopNudge {
		t.Errorf("7 unproductive mixed search calls should nudge, got %v", action)
	}
	if msg == "" {
		t.Error("nudge should have a message")
	}
}

func TestLoopDetector_SearchEscalation_ProductiveResets(t *testing.T) {
	ld := NewLoopDetector()

	// 2 unproductive, then 1 productive, then 1 more unproductive.
	// Trailing unproductive streak is only 1, well below the nudge threshold of 5.
	ld.Record("grep", `{"pattern":"a"}`, false, "", "", true)
	ld.Record("grep", `{"pattern":"b"}`, false, "", "", true)
	ld.Record("grep", `{"pattern":"c"}`, false, "", "", false) // productive — resets streak
	ld.Record("grep", `{"pattern":"d"}`, false, "", "", true)

	action, _ := ld.Check("grep")
	if action != LoopContinue {
		t.Errorf("productive search should reset streak, expected continue, got %v", action)
	}
}

func TestLoopDetector_SearchEscalation_ProductiveSearchesDontHitNoProgress(t *testing.T) {
	ld := NewLoopDetector()

	// Repeated productive grep calls with different args are normal during
	// repository exploration and should not trigger the generic NoProgress path.
	for i := 0; i < 8; i++ {
		ld.Record("grep", fmt.Sprintf(`{"pattern":"term%d"}`, i), false, "", "", false)
	}

	action, _ := ld.Check("grep")
	if action != LoopContinue {
		t.Errorf("productive search calls should not hit NoProgress, got %v", action)
	}
}

func TestLoopDetector_BrowserFamilyNoProgress(t *testing.T) {
	ld := NewLoopDetector()

	// Simulate 5 browser calls with the same URL (same topic hash) but different
	// extra fields to produce different ArgsHash and avoid ConsecutiveDup detector.
	// v2: FamilyNoProgress nudge at progressCount >= 5.
	ld.Record("browser", `{"action":"navigate","url":"https://jd.com/search?q=huawei","wait":1}`, false, "", "", false)
	ld.Record("browser", `{"action":"navigate","url":"https://jd.com/search?q=huawei","wait":2}`, false, "", "", false)
	ld.Record("browser", `{"action":"navigate","url":"https://jd.com/search?q=huawei","wait":3}`, false, "", "", false)
	ld.Record("browser", `{"action":"navigate","url":"https://jd.com/search?q=huawei","wait":4}`, false, "", "", false)
	ld.Record("browser", `{"action":"navigate","url":"https://jd.com/search?q=huawei","wait":5}`, false, "", "", false)
	action, msg := ld.Check("browser")
	if action != LoopNudge {
		t.Errorf("5 same-topic browser calls should nudge, got %v", action)
	}
	if strings.Contains(msg, "searched") || strings.Contains(msg, "query") {
		t.Errorf("browser-family nudge should not use search vocabulary, got: %s", msg)
	}
	if !strings.Contains(msg, "UI action") {
		t.Errorf("expected browser-family nudge to mention 'UI action', got: %s", msg)
	}
}

// TestFamilyNoProgressMessage_VocabularyByFamily asserts the helper emits
// family-appropriate wording at each stage. Protects against the regression
// where browser callers received search-vocabulary nudges ("You've searched
// the same topic…") after FamilyNoProgress was extended to cover browser_*.
func TestFamilyNoProgressMessage_VocabularyByFamily(t *testing.T) {
	cases := []struct {
		family        string
		stage         int
		forbidSubstrs []string
		wantSubstrs   []string
	}{
		{"browser", 0, []string{"searched", "query"}, []string{"UI action", "selector"}},
		{"browser", 1, []string{"searched", "query"}, []string{"UI action"}},
		{"browser", 2, []string{"searched", "query"}, []string{"UI action", "browser-family"}},
		{"gui", 0, []string{"searched", "query"}, []string{"UI action"}},
		{"search", 0, nil, []string{"searched the same topic"}},
		{"web", 2, nil, []string{"web calls", "same topic"}},
	}
	for _, tc := range cases {
		msg := familyNoProgressMessage(tc.family, 3, 4, tc.stage)
		for _, forbid := range tc.forbidSubstrs {
			if strings.Contains(msg, forbid) {
				t.Errorf("family=%s stage=%d: message must not contain %q, got: %s", tc.family, tc.stage, forbid, msg)
			}
		}
		for _, want := range tc.wantSubstrs {
			if !strings.Contains(msg, want) {
				t.Errorf("family=%s stage=%d: message must contain %q, got: %s", tc.family, tc.stage, want, msg)
			}
		}
	}
}

func TestBrowserInToolFamilies(t *testing.T) {
	family := toolFamily("browser")
	if family != "browser" {
		t.Errorf("browser family should be 'browser', got %q", family)
	}
}

// TestLoopDetector_BrowserToolsRepeatable ensures that browser_* MCP tools
// are treated as repeatable GUI tools. Before the fix, `repeatableGUITools`
// was keyed on the literal string "browser", but real tool names are
// "browser_navigate", "browser_snapshot", etc., so the NoProgress detector
// (8+ same tool → nudge) would fire on legit multi-page browsing sessions.
func TestLoopDetector_BrowserToolsRepeatable(t *testing.T) {
	ld := NewLoopDetector()

	// 9 browser_navigate calls to different URLs — progressCount stays at 1
	// per topic, so the FamilyNoProgress detector won't fire. But before the
	// fix the outer NoProgress detector (line 355) WOULD nudge at 8 because
	// repeatableTools["browser_navigate"] == false. After the fix it stays Continue.
	urls := []string{"a", "b", "c", "d", "e", "f", "g", "h", "i"}
	for _, u := range urls {
		ld.Record("browser_navigate", fmt.Sprintf(`{"url":"https://example.com/%s"}`, u), false, "", "", false)
	}
	action, msg := ld.Check("browser_navigate")
	if action != LoopContinue {
		t.Fatalf("browser_navigate x9 to different URLs should Continue (it is a repeatable GUI tool), got %v: %s", action, msg)
	}
}

func TestLoopDetector_BrowserSnapshotInterleavedRepeatable(t *testing.T) {
	ld := NewLoopDetector()
	// Realistic multi-step pattern: snapshot → click → snapshot → click → ...
	// Each snapshot has the same args but is separated by clicks, so it is
	// not a consecutive duplicate. Over 10 steps we accumulate 5 snapshots,
	// under the consecutive-dup threshold and under the no-progress threshold
	// of 8 same-name calls — must stay Continue.
	for i := range 5 {
		ld.Record("browser_snapshot", `{}`, false, "", "", false)
		ld.Record("browser_click", fmt.Sprintf(`{"ref":"e%d"}`, i), false, "", "", false)
	}
	action, msg := ld.Check("browser_click")
	if action != LoopContinue {
		t.Fatalf("interleaved browser_snapshot/browser_click should Continue, got %v: %s", action, msg)
	}
}

// TestLoopDetector_SemiRepeatable_BashHigherThreshold verifies that bash
// gets the elevated NoProgress threshold (16) instead of the generic (12),
// so multi-step scripting workflows (fetch → process → install → build)
// aren't killed before completing. The exact-dup, same-error, and sleep
// detectors still catch real loops at their own lower thresholds.
func TestLoopDetector_SemiRepeatable_BashHigherThreshold(t *testing.T) {
	ld := NewLoopDetector()

	// 12 distinct bash calls — would nudge with the generic threshold (12),
	// but should be Continue with the semi-repeatable threshold of 16.
	for i := range 12 {
		ld.Record("bash", fmt.Sprintf(`{"command":"step_%d"}`, i), false, "", "", false)
	}
	action, _ := ld.Check("bash")
	if action != LoopContinue {
		t.Errorf("12 distinct bash calls should Continue (semi-repeatable threshold 16), got %v", action)
	}

	// 15 calls — still under 16.
	for i := 12; i < 15; i++ {
		ld.Record("bash", fmt.Sprintf(`{"command":"step_%d"}`, i), false, "", "", false)
	}
	action, _ = ld.Check("bash")
	if action != LoopContinue {
		t.Errorf("15 distinct bash calls should Continue, got %v", action)
	}

	// 16th call → nudge.
	ld.Record("bash", `{"command":"step_16"}`, false, "", "", false)
	action, _ = ld.Check("bash")
	if action != LoopNudge {
		t.Errorf("16 bash calls should nudge, got %v", action)
	}
}

// TestLoopDetector_SemiRepeatable_NonBashUnchanged verifies that the generic
// NoProgress threshold (12) still applies to non-semi-repeatable tools like
// file_write, think, etc. — unchanged from the v1 bash-only relaxation.
func TestLoopDetector_SemiRepeatable_NonBashUnchanged(t *testing.T) {
	ld := NewLoopDetector()

	for i := range 12 {
		ld.Record("think", fmt.Sprintf(`{"thought":"idea_%d"}`, i), false, "", "", false)
	}
	action, _ := ld.Check("think")
	if action != LoopNudge {
		t.Errorf("12 think calls should nudge at generic threshold, got %v", action)
	}
}

// TestLoopDetector_BrowserMultiToolFlowNoFalsePositive verifies that a
// realistic mixed browser workflow does not trigger FamilyNoProgress just
// because every call on the same page produces the same URL-only result
// signature. Before the fix, navigate → click → click → upload on
// chatgpt.com would emit a "same topic/UI action 3 times" nudge because
// extractResultSignature collapses every browser-family call on that URL
// to the same hash.
func TestLoopDetector_BrowserMultiToolFlowNoFalsePositive(t *testing.T) {
	ld := NewLoopDetector()

	// All four calls return snapshots whose URL set boils down to
	// https://chatgpt.com/ → identical result signatures.
	sameResultSig := "https://chatgpt.com"
	ld.Record("browser_navigate", `{"url":"https://chatgpt.com"}`, false, "", sameResultSig, false)
	ld.Record("browser_click", `{"ref":"e120","element":"plus"}`, false, "", sameResultSig, false)
	ld.Record("browser_click", `{"ref":"e513","element":"photos"}`, false, "", sameResultSig, false)
	ld.Record("browser_file_upload", `{"paths":["/tmp/x.png"]}`, false, "", sameResultSig, false)

	action, msg := ld.Check("browser_file_upload")
	if action != LoopContinue {
		t.Errorf("mixed browser workflow with unique tool names must not nudge, got %v (%q)", action, msg)
	}
}

// TestLoopDetector_BrowserSameToolStillDetected guards the opposite case:
// true repetition of the same browser tool on the same page should still
// trip the detector. Ensures the same-name scoping doesn't break real-loop
// detection.
func TestLoopDetector_BrowserSameToolStillDetected(t *testing.T) {
	ld := NewLoopDetector()
	sameResultSig := "https://chatgpt.com"
	// Three back-to-back identical calls hit the consecutive-duplicate
	// detector at threshold 3 → nudge. A fourth would force-stop, which is
	// also correct behavior but not what this test locks in.
	for i := 0; i < 3; i++ {
		ld.Record("browser_click", `{"ref":"e120","element":"plus"}`, false, "", sameResultSig, false)
	}
	action, _ := ld.Check("browser_click")
	if action != LoopNudge {
		t.Errorf("3 consecutive identical browser_click calls should nudge, got %v", action)
	}
}

// TestLoopDetector_BrowserSnapshotConsecutiveDupStillForceStops preserves the
// load-bearing polling guard after the repeatable-result-only relaxation:
// repeated browser_snapshot calls with identical args must still be stopped by
// the duplicate detectors instead of silently inheriting the raised threshold.
// With consecDupThreshold=3, force-stop fires at consecDupThreshold+1=4.
func TestLoopDetector_BrowserSnapshotConsecutiveDupStillForceStops(t *testing.T) {
	ld := NewLoopDetector()
	const pageURL = "https://example.com/app"
	for range 4 {
		ld.Record("browser_snapshot", `{}`, false, "", pageURL, false)
	}
	action, msg := ld.Check("browser_snapshot")
	if action != LoopForceStop {
		t.Fatalf("4 identical browser_snapshot calls must still force-stop via duplicate detection, got %v: %s", action, msg)
	}
}

// TestIsReadMCPName locks the read-verb whitelist used to populate
// the loop detector's batchTolerant set. Read-only MCP tools must match
// (eligible for uniqueness-gated NoProgress relief); write-capable tools
// must NOT match (stay under the count-based guard), because the
// permission engine does not gate MCP calls and a write loop with
// unique arguments could otherwise create many remote records.
func TestIsReadMCPName(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		// Direct read-verb prefix.
		{"list_calendars", true},
		{"get_events", true},
		{"search_gmail_messages", true},
		{"query_database", true},
		{"fetch_profile", true},
		{"describe_table", true},
		{"find_files", true},
		// Namespaced read-verbs (vendor prefix + separator + verb).
		{"API-query-data-source", true},
		{"google_gmail_search_messages", true},
		{"notion_list_pages", true},
		{"Notion_Search_Databases", true}, // case-insensitive
		// Write verbs must stay OUT.
		{"create_notion_page", false},
		{"update_page_properties", false},
		{"delete_event", false},
		{"send_gmail_message", false},
		{"modify_permissions", false},
		{"remove_label", false},
		{"insert_row", false},
		{"append_content_to_page", false},
		{"archive_thread", false},
		// Namespaced writes must also stay out.
		{"google_calendar_create_event", false},
		{"notion_create_comment", false},
		{"drive_upload_file", false},
		// Compound-verb names: a read verb AND a write verb in the first
		// three tokens must return false — the write blacklist dominates.
		// This is the defensive half of the heuristic: destructive suffixes
		// must not sneak through on a position-0 read-verb match.
		{"lookup_and_delete_all_records", false}, // lookup + delete
		{"get_or_create_item", false},            // get + create
		{"find_and_remove_entry", false},         // find + remove
		{"list-and-archive", false},              // list + archive
		// Data-transfer / property-mutation verbs (GitHub/Linear/Notion/
		// Slack MCP patterns). Each pairs a position-0 read with a
		// write verb that earlier versions of writeVerbs missed.
		{"get_and_add_member", false},        // get + add
		{"list_and_set_properties", false},   // list + set
		{"search_and_replace", false},        // search + replace
		{"get_and_write_cache", false},       // get + write
		{"find_and_patch_record", false},     // find + patch
		{"query_and_put_result", false},      // query + put
		{"list_and_clear_flags", false},      // list + clear
		{"get_and_post_update", false},       // get + post
		{"list_and_push_changes", false},     // list + push
		{"fetch_and_publish_item", false},    // fetch + publish
		{"get_and_submit_form", false},       // get + submit
		{"list_and_drop_table", false},       // list + drop
		{"find_and_prune_entries", false},    // find + prune
		// "run"/"execute" are in writeVerbs (fail-closed on ambiguous
		// action verbs). Snowflake/ClickHouse "run_query" used to be
		// accepted as SELECT convention, but a Medium review finding
		// pointed out that ambiguity should fall on the safe side —
		// the server is free to rename to "query_database" if it wants
		// NoProgress relief.
		{"run_query", false},       // run is a write verb (fail closed)
		{"execute_script", false},  // execute is a write verb (fail closed)
		{"transform_data", false},  // no read verb
		{"process_batch", false},   // no read verb
		// Pathological: write name with a read-verb at position 4+ must
		// NOT match (token scan stops at position 3).
		{"request_write_access_and_get_token_afterwards", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isReadMCPName(tt.name); got != tt.want {
				t.Errorf("isReadMCPName(%q) = %v, want %v", tt.name, got, tt.want)
			}
		})
	}
}

// TestLoopDetector_NoProgress_BashUniqueArgs_NoNudge covers the Task 5
// benchmark pattern: ~15 bash calls during a multi-step investigation, each
// with distinct argsJSON. Pre-gate, this force-stops via maxNudges escalation.
// With bash in the batchTolerant set and ≥50% unique argsHashes, NoProgress
// must treat this as a legitimate batch and stay Continue.
func TestLoopDetector_NoProgress_BashUniqueArgs_NoNudge(t *testing.T) {
	ld := NewLoopDetector()
	ld.batchTolerant = map[string]bool{"bash": true}

	for i := range 15 {
		ld.Record("bash", fmt.Sprintf(`{"cmd":"step_%d"}`, i), false, "", "", false)
		action, msg := ld.Check("bash")
		if action != LoopContinue {
			t.Fatalf("call %d: unique-args bash on a batch-tolerant tool should stay Continue, got %v (%s)", i+1, action, msg)
		}
	}
}

// TestLoopDetector_NoProgress_MCPUniqueArgs_NoNudge covers the Task 6
// benchmark pattern: 16 MCP-tool calls each querying a distinct UUID during a
// legitimate Notion database enumeration. Pre-gate, this hit the generic
// NoProgress threshold at count=8. With the MCP tool registered in
// batchTolerant, unique-args enumeration stays Continue.
func TestLoopDetector_NoProgress_MCPUniqueArgs_NoNudge(t *testing.T) {
	ld := NewLoopDetector()
	ld.batchTolerant = map[string]bool{"API-query-data-source": true}

	for i := range 16 {
		ld.Record("API-query-data-source", fmt.Sprintf(`{"id":"uuid-%d"}`, i), false, "", "", false)
		action, msg := ld.Check("API-query-data-source")
		if action != LoopContinue {
			t.Fatalf("call %d: unique-args MCP tool on batch-tolerant list should stay Continue, got %v (%s)", i+1, action, msg)
		}
	}
}

// TestLoopDetector_NoProgress_MCPIdenticalArgs_StillStops locks the invariant
// that batch-tolerance does NOT relax the identical-args case. Regardless of
// which layered detector catches it (ConsecutiveDup fires earliest at 2
// consecutive identical calls; ExactDup at 3 spread out; NoProgress at 8),
// the outcome must be "not Continue" — identical-args spin is always caught.
func TestLoopDetector_NoProgress_MCPIdenticalArgs_StillStops(t *testing.T) {
	ld := NewLoopDetector()
	ld.batchTolerant = map[string]bool{"API-query-data-source": true}

	for range 8 {
		ld.Record("API-query-data-source", `{"id":"same-uuid"}`, false, "", "", false)
	}
	action, msg := ld.Check("API-query-data-source")
	if action == LoopContinue {
		t.Fatalf("identical-args calls must be stopped by some detector despite batch-tolerance, got Continue (%s)", msg)
	}
}

// TestLoopDetector_NoProgress_GenericToolUniqueArgs_StillNudges_Regression
// pins the core constraint of Phase 1: the uniqueness gate must NOT relax
// generic NoProgress detection for tools outside the batchTolerant set.
// `think` (not in batchTolerant, not semi-repeatable) called 12 times with
// distinct argsJSON must still nudge — catching "spinning on thought
// variations without progress" is the generic path's load-bearing role.
// v2: noProgressThreshold=12, so nudge fires at call 12.
func TestLoopDetector_NoProgress_GenericToolUniqueArgs_StillNudges_Regression(t *testing.T) {
	ld := NewLoopDetector()
	// Explicitly NOT populating batchTolerant — this test must behave the
	// same whether the field is nil or empty.

	for i := range 12 {
		ld.Record("think", fmt.Sprintf(`{"thought":"idea%d"}`, i), false, "", "", false)
	}
	action, msg := ld.Check("think")
	if action != LoopNudge {
		t.Fatalf("12 unique-args think calls must still nudge (generic path unchanged), got %v (%s)", action, msg)
	}
}

// TestLoopDetector_NoProgress_BashMixedArgsRatio_GateIsolated exercises the
// NoProgress uniqueness gate without letting ConsecutiveDup / ExactDup fire
// first. The sequence uses 8 distinct argsHashes each appearing exactly twice
// (16 calls, 50% unique) interleaved so no hash runs ≥3 times in a row and
// ExactDup's "same-arg 3 times in window" threshold is not tripped.
//
// On a batch-tolerant bash, the gate suppresses the nudge at count≥12.
// Without batch-tolerance (Generic path), the same stream must nudge — this
// sub-test covers the non-relaxation invariant at the threshold boundary.
func TestLoopDetector_NoProgress_BashMixedArgsRatio_GateIsolated(t *testing.T) {
	// Build a non-consecutive pattern to keep ConsecutiveDup (need ≥2 back-to-back)
	// and ExactDup (need ≥3 of the same argsHash in the window) quiet.
	// Pattern: 1,2,3,4,5,6,7,8,1,2,3,4,5,6,7,8 — each hash appears twice,
	// separated by 7 others. ExactDup threshold is 3 so two appearances is
	// safe; ConsecutiveDup needs adjacency so interleaving avoids it.
	pattern := []int{0, 1, 2, 3, 4, 5, 6, 7, 0, 1, 2, 3, 4, 5, 6, 7}

	t.Run("gated_when_batch_tolerant", func(t *testing.T) {
		ld := NewLoopDetector()
		ld.batchTolerant = map[string]bool{"bash": true}
		for _, i := range pattern {
			ld.Record("bash", fmt.Sprintf(`{"cmd":"script_%d"}`, i), false, "", "", false)
		}
		action, msg := ld.Check("bash")
		if action != LoopContinue {
			t.Fatalf("50%% unique on batch-tolerant bash should be gated (Continue), got %v (%s)", action, msg)
		}
	})

	t.Run("not_gated_when_not_batch_tolerant", func(t *testing.T) {
		ld := NewLoopDetector()
		// Explicitly empty batchTolerant — same sequence, no gate.
		for _, i := range pattern {
			ld.Record("bash", fmt.Sprintf(`{"cmd":"script_%d"}`, i), false, "", "", false)
		}
		action, msg := ld.Check("bash")
		if action != LoopNudge {
			t.Fatalf("same sequence without batch-tolerance should nudge at count≥12, got %v (%s)", action, msg)
		}
	})
}

// TestLoopDetector_NoProgress_FileReadUniqueArgs_NoNudge covers the
// "user asked me to read N screenshots" fan-out pattern. In production this
// triggered the NoProgress nudge "You've called file_read 13 times.
// Summarize what you've learned and try a different approach" — the model
// then fabricated an "exceeded context window" excuse to drop the task even
// though the actual prompt was 22% of the 1M-token window (see PR #140
// session 2026-05-14-9f885aef91c0). The fix: file_read must be batch-tolerant
// the same way bash and read-verb MCP tools are, so the uniqueness gate
// recognizes 13 distinct paths as legitimate fan-out instead of a stuck loop.
func TestLoopDetector_NoProgress_FileReadUniqueArgs_NoNudge(t *testing.T) {
	ld := NewLoopDetector()
	ld.batchTolerant = map[string]bool{"file_read": true}

	for i := range 13 {
		ld.Record("file_read", fmt.Sprintf(`{"path":"/Users/me/Desktop/shot_%02d.png"}`, i), false, "", "", false)
		action, msg := ld.Check("file_read")
		if action != LoopContinue {
			t.Fatalf("call %d: unique-path file_read on batch-tolerant tool must stay Continue, got %v (%s)", i+1, action, msg)
		}
	}
}

// TestLoopDetector_NoProgress_GlobGrepUniqueArgs_NoNudge guards the same
// fan-out invariant for glob and grep. Repository exploration legitimately
// runs many distinct patterns; NoProgress nudges here forced premature
// surrender in audit-log incidents.
func TestLoopDetector_NoProgress_GlobGrepUniqueArgs_NoNudge(t *testing.T) {
	for _, name := range []string{"glob", "grep"} {
		t.Run(name, func(t *testing.T) {
			ld := NewLoopDetector()
			ld.batchTolerant = map[string]bool{name: true}
			for i := range 13 {
				ld.Record(name, fmt.Sprintf(`{"pattern":"q%02d*.go"}`, i), false, "", "", false)
				action, msg := ld.Check(name)
				if action != LoopContinue {
					t.Fatalf("%s call %d: unique-args on batch-tolerant tool must stay Continue, got %v (%s)", name, i+1, action, msg)
				}
			}
		})
	}
}

// TestLoopDetector_NoProgress_FileReadIdenticalArgs_StillStops locks the
// invariant that batch-tolerance must NOT relax identical-args spin
// detection. 13 file_read calls on the SAME path indicates the model is
// stuck, not enumerating — some detector must still catch it.
func TestLoopDetector_NoProgress_FileReadIdenticalArgs_StillStops(t *testing.T) {
	ld := NewLoopDetector()
	ld.batchTolerant = map[string]bool{"file_read": true}

	for range 13 {
		ld.Record("file_read", `{"path":"/same/file.png"}`, false, "", "", false)
	}
	action, msg := ld.Check("file_read")
	if action == LoopContinue {
		t.Fatalf("identical-path file_read ×13 must still be stopped by some detector, got Continue (%s)", msg)
	}
}

// TestLoopDetector_UseSkill_RepeatedNeverFiresAnyDup documents production
// issue: 9 force-stops in audit log on use_skill same-args ×3, iter=3,
// killing queries before they were processed. use_skill is an idempotent
// metadata load (see internal/tools/skill.go) — repeating it is harmless.
// After the fix, ×5 same-args should return LoopContinue from Check
// (neither ConsecutiveDup nor ExactDup fires).
func TestLoopDetector_UseSkill_RepeatedNeverFiresAnyDup(t *testing.T) {
	ld := NewLoopDetector()
	for range 5 {
		ld.Record("use_skill", `{"skill_name":"kocoro"}`, false, "", "", false)
	}
	action, msg := ld.Check("use_skill")
	if action != LoopContinue {
		t.Fatalf("use_skill ×5 same-args must return LoopContinue (idempotent metadata load), got %v: %s", action, msg)
	}
}

// TestLoopDetector_UseSkill_ExemptionScopedToSelf guards against the
// dupExempt entry leaking into other tools. After 5 use_skill calls
// (which would normally trip ExactDup), records 4 same-args web_search
// calls — those must still force-stop. This catches the regression
// where an over-broad exemption (e.g. checking against the whole
// dupExemptTools map outside the name-scoped path) would suppress
// legitimate signals on adjacent tools.
// With consecDupThreshold=3, force-stop fires at consecCount >= 4.
func TestLoopDetector_UseSkill_ExemptionScopedToSelf(t *testing.T) {
	ld := NewLoopDetector()
	for range 5 {
		ld.Record("use_skill", `{"skill_name":"kocoro"}`, false, "", "", false)
	}
	for range 4 {
		ld.Record("web_search", `{"q":"climate"}`, false, "", "", false)
	}
	action, _ := ld.Check("web_search")
	if action != LoopForceStop {
		t.Fatalf("web_search ×4 same args must still force-stop after use_skill exemption activity, got %v", action)
	}
}

// TestNudgeWindow_RollsOff documents the rolling-window semantics: nudges
// older than `nudgeWindow` iterations age out. A long workflow with widely
// spaced harmless nudges should never trigger maxNudges escalation.
func TestNudgeWindow_RollsOff(t *testing.T) {
	w := newNudgeWindow(3, 5) // 3 max, 5-iter window
	if w.recordAndCheck(1) {
		t.Fatal("1 nudge in window should not escalate")
	}
	if w.recordAndCheck(2) {
		t.Fatal("2 nudges in window should not escalate")
	}
	// iter 3-7: no nudges. By iter 8, the iter-1 and iter-2 nudges should age out (cutoff = 8 - 5 + 1 = 4).
	if w.recordAndCheck(8) {
		t.Fatal("3rd nudge at iter 8 (window=5) should not escalate — first two aged out")
	}
}

func TestNudgeWindow_BurstEscalates(t *testing.T) {
	w := newNudgeWindow(3, 5)
	if w.recordAndCheck(1) {
		t.Fatal("1st nudge should not escalate")
	}
	if w.recordAndCheck(2) {
		t.Fatal("2nd nudge should not escalate")
	}
	if !w.recordAndCheck(3) {
		t.Fatal("3rd nudge in 5-iter window should escalate")
	}
}

// TestConsecutiveDup_FailFailSuccessRetry locks the invariant that a flaky
// retry pattern (fail, fail, succeed) on the same args does NOT force-stop.
// Real Playwright selectors race page-load timing — the model must be
// allowed to retry without being killed at attempt 3. Rule 1: tail-success
// after any error in the run → skip detector (model recovered).
func TestConsecutiveDup_FailFailSuccessRetry(t *testing.T) {
	ld := NewLoopDetector()
	ld.Record("browser_click", `{"ref":"e1"}`, true, "element not found", "", false)
	ld.Record("browser_click", `{"ref":"e1"}`, true, "element not found", "", false)
	ld.Record("browser_click", `{"ref":"e1"}`, false, "", "", false)
	action, msg := ld.Check("browser_click")
	if action != LoopContinue {
		t.Fatalf("fail-fail-success retry must return LoopContinue (tail recovery), got %v: %s", action, msg)
	}
}

// TestConsecutiveDup_ThreeSuccessfulSameArgsStillStops confirms the
// legitimate "spinning on identical successful results" case still
// triggers. Rule 1 doesn't apply (no error in run), Rule 2 doesn't apply
// (not all errors) → original strict threshold.
// With consecDupThreshold=3: nudge at 3, force-stop at 4.
func TestConsecutiveDup_ThreeSuccessfulSameArgsStillStops(t *testing.T) {
	ld := NewLoopDetector()
	for range 4 {
		ld.Record("web_search", `{"q":"climate"}`, false, "", "", false)
	}
	action, _ := ld.Check("web_search")
	if action != LoopForceStop {
		t.Fatalf("4 successful identical web_search must still force-stop, got %v", action)
	}
}

// TestConsecutiveDup_SixErrorsNudgesNotForceStop: 6 same-args fails uses
// Rule 2's 2x threshold (6 nudge, 7 force-stop). At 6 errors → nudge, not force-stop.
// consecDupThreshold=3 → all-errors budget = 2x = 6/7.
func TestConsecutiveDup_SixErrorsNudgesNotForceStop(t *testing.T) {
	ld := NewLoopDetector()
	for range 6 {
		ld.Record("browser_click", `{"ref":"e1"}`, true, "element not found", "", false)
	}
	action, _ := ld.Check("browser_click")
	if action != LoopNudge {
		t.Fatalf("6 same-args consecutive errors should nudge (error budget 6/7), got %v", action)
	}
}

// TestConsecutiveDup_FiveAllErrorsForceStops: 7 same-args all-error hits
// the 2x force-stop budget. No tail success, no recovery — real stuck loop.
// consecDupThreshold=3 → all-errors budget = 2x = 6 nudge / 7 force-stop.
func TestConsecutiveDup_FiveAllErrorsForceStops(t *testing.T) {
	ld := NewLoopDetector()
	for range 7 {
		ld.Record("browser_click", `{"ref":"e1"}`, true, "element not found", "", false)
	}
	action, _ := ld.Check("browser_click")
	if action != LoopForceStop {
		t.Fatalf("7 same-args consecutive errors should force-stop (2x budget), got %v", action)
	}
}

// TestExactDup_RecoveryThenSecondSpinNotSkipped locks the invariant that
// `latestRecoveredAfterSameArgsErrors` requires the LATEST call to be
// success. A pattern of "errors → success → more errors" should NOT be
// treated as recovered when the latest call is back to error — the
// detector must count the new error streak fresh.
//
// Regression guard: if a future refactor of latestRecoveredAfterSameArgsErrors
// caches "we recovered earlier" and skips ExactDup forever, this test breaks.
func TestExactDup_RecoveryThenSecondSpinNotSkipped(t *testing.T) {
	ld := NewLoopDetector()
	// Phase 1: 4 same-args errors
	for range 4 {
		ld.Record("browser_click", `{"ref":"e1"}`, true, "element not found", "", false)
	}
	// Phase 2: 1 success (the "recovery" the helper looks for)
	ld.Record("browser_click", `{"ref":"e1"}`, false, "", "", false)
	// Phase 3: latest is now an error again. ExactDup must count the
	// 5 same-args errors (4 + this new one) at strict v2 threshold (5 nudge)
	// — exactRecovered must be false because latest is an error.
	ld.Record("browser_click", `{"ref":"e1"}`, true, "element not found", "", false)
	action, _ := ld.Check("browser_click")
	if action == LoopContinue {
		t.Fatalf("post-recovery spin (4err + success + 1err) must NOT be silently skipped, got LoopContinue")
	}
	// Either LoopNudge or LoopForceStop is acceptable here — what we're
	// proving is that recovery does NOT cache and disable the detector.
}

// TestExactDup_SixAllErrorsSpreadNotForceStop: 6 spread-out same-args
// failures (with intervening different-tool calls) is past the old
// exactDupThreshold*2=6 force-stop trigger. With all-error 2x budget,
// the new threshold for all-errors is 6 nudge / 12 force-stop.
// 6 errors → should nudge, not force-stop.
func TestExactDup_SixAllErrorsSpreadNotForceStop(t *testing.T) {
	ld := NewLoopDetector()
	for i := 0; i < 6; i++ {
		ld.Record("browser_click", `{"ref":"e1"}`, true, "element not found", "", false)
		ld.Record("browser_snapshot", `{}`, false, "",
			fmt.Sprintf("https://example.com/state%d", i), false)
	}
	action, msg := ld.Check("browser_click")
	if action == LoopForceStop {
		t.Fatalf("6 spread-out same-args errors should not force-stop (2x all-error budget), got: %s", msg)
	}
}

// TestExactDup_SixAllSuccessSpreadStillForceStops: 10 spread-out same-args
// successes (no errors) uses the original threshold → force-stop at 2×exactDupThreshold=10.
// This is real spin, not flaky retry.
func TestExactDup_SixAllSuccessSpreadStillForceStops(t *testing.T) {
	ld := NewLoopDetector()
	for i := 0; i < 10; i++ {
		ld.Record("file_read", `{"file":"main.go"}`, false, "", "", false)
		ld.Record("file_edit",
			fmt.Sprintf(`{"old":"a%d","new":"b%d"}`, i, i), false, "", "", false)
	}
	action, _ := ld.Check("file_read")
	if action != LoopForceStop {
		t.Fatalf("10 spread-out same-args successes must still force-stop (2×exactDupThreshold budget), got %v", action)
	}
}

// TestExactDup_MixedSuccessAndErrorsUsesStrictThreshold: if ANY of the
// repeats succeeded, we no longer have "all errors" — use the strict
// threshold. Mixed means the tool sometimes works; continuing to call it
// with identical args is spin.
// Final call in sequence is an error (so tail-success recovery skip does
// NOT apply — recovery requires tail=success AND errCount>0).
// With exactDupThreshold=5: nudge fires at dupCount >= 5 (strict, mixed).
func TestExactDup_MixedSuccessAndErrorsUsesStrictThreshold(t *testing.T) {
	ld := NewLoopDetector()
	// 5 same-args repeats with mixed success/error, tail=error → strict threshold → nudge at 5
	ld.Record("browser_click", `{"ref":"e1"}`, true, "element not found", "", false)
	ld.Record("browser_snapshot", `{}`, false, "", "sigA", false)
	ld.Record("browser_click", `{"ref":"e1"}`, false, "", "", false)
	ld.Record("browser_snapshot", `{}`, false, "", "sigB", false)
	ld.Record("browser_click", `{"ref":"e1"}`, true, "element not found", "", false)
	ld.Record("browser_snapshot", `{}`, false, "", "sigC", false)
	ld.Record("browser_click", `{"ref":"e1"}`, false, "", "", false)
	ld.Record("browser_snapshot", `{}`, false, "", "sigD", false)
	ld.Record("browser_click", `{"ref":"e1"}`, true, "element not found", "", false)
	action, _ := ld.Check("browser_click")
	if action != LoopNudge {
		t.Fatalf("5 mixed same-args repeats (tail=error) should nudge (strict threshold), got %v", action)
	}
}

// TestExactDup_FailFailSuccessSpreadRetrySkipsOnRecoveredTail documents the
// spread-out retry shape the comments describe for ExactDup: the model retries
// the same browser_click across intervening snapshots, then succeeds. The
// first success after a same-args error streak is recovery, not spin.
func TestExactDup_FailFailSuccessSpreadRetrySkipsOnRecoveredTail(t *testing.T) {
	ld := NewLoopDetector()
	ld.Record("browser_click", `{"ref":"e1"}`, true, "element not found", "", false)
	ld.Record("browser_snapshot", `{}`, false, "", "https://example.com/state1", false)
	ld.Record("browser_click", `{"ref":"e1"}`, true, "element not found", "", false)
	ld.Record("browser_snapshot", `{}`, false, "", "https://example.com/state2", false)
	ld.Record("browser_click", `{"ref":"e1"}`, false, "", "", false)
	action, msg := ld.Check("browser_click")
	if action != LoopContinue {
		t.Fatalf("fail-snapshot-fail-snapshot-success must return LoopContinue (spread recovery), got %v: %s", action, msg)
	}
}

// TestFamilyNoProgress_RepeatableVaryingArgsUnder15Silent: 14 varying-args
// browser_snapshot calls on a stable URL. Pre-fix: FamilyNoProgress main
// path force-stops at progressCount=7. Post-fix: repeatable + no topic
// signal → force-stop-only-at-15 → silent until pathological threshold.
//
// Covers form-fill-equivalent workloads (7-14 same-page ops should all
// continue — no intermediate nudges that might stack into Task 2's
// rolling-window escalation).
func TestFamilyNoProgress_RepeatableVaryingArgsUnder15Silent(t *testing.T) {
	ld := NewLoopDetector()
	const url = "https://app.example.com/dashboard"
	for i := 0; i < 14; i++ {
		args := fmt.Sprintf(`{"wait":%d}`, i)
		ld.Record("browser_snapshot", args, false, "", url, false)
	}
	action, msg := ld.Check("browser_snapshot")
	if action != LoopContinue {
		t.Fatalf("14 varying-args repeatable calls on stable URL must be silent (force-stop-only-at-15), got %v: %s", action, msg)
	}
}

// TestFamilyNoProgress_RepeatableFormFillContinues: 10 click + 10 type on a
// stable URL — representative of a large form fill. Must continue silently
// (no nudge — nudges feed Task 2 rolling-window escalation).
func TestFamilyNoProgress_RepeatableFormFillContinues(t *testing.T) {
	ld := NewLoopDetector()
	const url = "https://app.example.com/settings"
	for i := 0; i < 10; i++ {
		ld.Record("browser_click",
			fmt.Sprintf(`{"ref":"e%d"}`, i), false, "", url, false)
		ld.Record("browser_type",
			fmt.Sprintf(`{"ref":"e%d","text":"v%d"}`, i, i), false, "", url, false)
	}
	action, _ := ld.Check("browser_click")
	if action != LoopContinue {
		t.Fatalf("10 varying-args browser_click on stable URL (form fill) must continue, got %v", action)
	}
}

// TestFamilyNoProgress_RepeatableResultOnly_SelfTopicOnlySilentBelow15 covers
// repeatable tools whose args include a URL, so the latest topic hash matches
// the current call itself but no prior calls. That is still result-only: the
// strong topic signal is absent, so stable result_sig should stay silent until
// the raised threshold instead of force-stopping at 7.
func TestFamilyNoProgress_RepeatableResultOnly_SelfTopicOnlySilentBelow15(t *testing.T) {
	ld := NewLoopDetector()
	const resultSig = "https://app.example.com/search"
	for i := 0; i < 7; i++ {
		args := fmt.Sprintf(`{"url":"https://app.example.com/search?q=item-%d"}`, i)
		ld.Record("browser_navigate", args, false, "", resultSig, false)
	}
	action, msg := ld.Check("browser_navigate")
	if action != LoopContinue {
		t.Fatalf("7 browser_navigate calls with self-only topic match and stable result_sig must stay silent below 15, got %v: %s", action, msg)
	}
}

// TestFamilyNoProgress_RepeatableVaryingArgsExtremeForceStops: 15
// varying-args snapshots on stable URL — past the raised force-stop
// threshold. Real pathological polling still caught.
func TestFamilyNoProgress_RepeatableVaryingArgsExtremeForceStops(t *testing.T) {
	ld := NewLoopDetector()
	const url = "https://app.example.com/status"
	for i := 0; i < 15; i++ {
		args := fmt.Sprintf(`{"wait":%d}`, i)
		ld.Record("browser_snapshot", args, false, "", url, false)
	}
	action, _ := ld.Check("browser_snapshot")
	if action != LoopForceStop {
		t.Fatalf("15 varying-args same-URL snapshots must still force-stop (pathological polling), got %v", action)
	}
}

// TestFamilyNoProgress_NonRepeatableOriginalThresholds: web_search family
// must still hit force-stop at 12 same-topic calls (v2 threshold).
// Raised thresholds apply uniformly; repeatable tools have a separate
// result-only path with a higher threshold.
func TestFamilyNoProgress_NonRepeatableOriginalThresholds(t *testing.T) {
	ld := NewLoopDetector()
	// All 12 queries normalize to the "change climate effects" topic
	// (only filler words differ).
	fillers := []string{"today", "latest", "top", "current", "major", "breaking",
		"news", "update", "headlines", "recent", "today latest", "top current"}
	for _, f := range fillers {
		args := fmt.Sprintf(`{"q":"climate change effects %s"}`, f)
		ld.Record("web_search", args, false, "", "", false)
	}
	action, _ := ld.Check("web_search")
	if action != LoopForceStop {
		t.Fatalf("12 same-topic web_search must still force-stop (v2 threshold), got %v", action)
	}
}

// TestLoopDetector_Record_FlagsEmptyThinkInput verifies Record() sets
// IsEmptyThinkInput on the ToolCallRecord for `think` calls whose args parse
// to an empty / whitespace-only thought. The flag drives the Check() rule
// that force-stops after two consecutive empty think calls. See plan
// 2026-05-14-thinking-blocks-cc-alignment.md Phase 0.2.
func TestLoopDetector_Record_FlagsEmptyThinkInput(t *testing.T) {
	cases := []struct {
		name      string
		toolName  string
		argsJSON  string
		wantEmpty bool
	}{
		{"think empty object", "think", `{}`, true},
		{"think empty thought", "think", `{"thought":""}`, true},
		{"think whitespace thought", "think", `{"thought":"   "}`, true},
		{"think tab/newline thought", "think", `{"thought":"\t\n"}`, true},
		{"think real thought", "think", `{"thought":"plan A then B"}`, false},
		{"think malformed JSON not flagged", "think", `not json`, false},
		{"non-think empty args not flagged", "file_read", `{}`, false},
		{"non-think tool with thought-like arg not flagged", "bash", `{"thought":""}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ld := NewLoopDetector()
			ld.Record(tc.toolName, tc.argsJSON, false, "", "", false)
			if len(ld.history) != 1 {
				t.Fatalf("history len = %d, want 1", len(ld.history))
			}
			got := ld.history[0].IsEmptyThinkInput
			if got != tc.wantEmpty {
				t.Errorf("IsEmptyThinkInput = %v, want %v (tool=%q args=%q)", got, tc.wantEmpty, tc.toolName, tc.argsJSON)
			}
		})
	}
}

// TestLoopDetector_EmptyThinkForceStop verifies the Phase 0 rule: two
// consecutive `think` calls with empty input force-stop immediately. The
// rule defends against Sonnet 4.6 / Opus 4.7 emitting ritual `think({})`
// calls after native thinking blocks (saw 4 consecutive in one production
// hang before the fix). See plan 2026-05-14-thinking-blocks-cc-alignment.md.
func TestLoopDetector_EmptyThinkForceStop_OneNotEnough(t *testing.T) {
	ld := NewLoopDetector()
	ld.Record("think", `{}`, false, "", "", false)
	action, _ := ld.Check("think")
	if action == LoopForceStop {
		t.Errorf("single empty think must not force-stop, got %v", action)
	}
}

func TestLoopDetector_EmptyThinkForceStop_TwoConsecutiveFires(t *testing.T) {
	ld := NewLoopDetector()
	ld.Record("think", `{}`, false, "", "", false)
	ld.Record("think", `{"thought":"   "}`, false, "", "", false)
	action, msg := ld.Check("think")
	if action != LoopForceStop {
		t.Fatalf("two consecutive empty think calls must force-stop, got %v (msg=%q)", action, msg)
	}
	if msg == "" {
		t.Error("force-stop should include an explanatory message")
	}
}

func TestLoopDetector_EmptyThinkForceStop_InterleavedToolBreaksStreak(t *testing.T) {
	ld := NewLoopDetector()
	ld.Record("think", `{}`, false, "", "", false)
	ld.Record("file_read", `{"path":"/x"}`, false, "", "", false)
	ld.Record("think", `{}`, false, "", "", false)
	action, _ := ld.Check("think")
	if action == LoopForceStop {
		t.Errorf("non-consecutive empty think must not force-stop (file_read broke streak), got %v", action)
	}
}

func TestLoopDetector_EmptyThinkForceStop_NonEmptyBreaksStreak(t *testing.T) {
	ld := NewLoopDetector()
	ld.Record("think", `{}`, false, "", "", false)
	ld.Record("think", `{"thought":"actual reasoning"}`, false, "", "", false)
	ld.Record("think", `{}`, false, "", "", false)
	action, _ := ld.Check("think")
	if action == LoopForceStop {
		t.Errorf("non-empty think between two empties must reset the streak, got %v", action)
	}
}

func TestLoopDetector_EmptyThinkForceStop_NonThinkUnaffected(t *testing.T) {
	// Two consecutive empty-args calls to a non-think tool must NOT trip
	// the empty-think rule — only the dup-detector budget applies.
	ld := NewLoopDetector()
	ld.Record("file_read", `{}`, false, "", "", false)
	ld.Record("file_read", `{}`, false, "", "", false)
	action, _ := ld.Check("file_read")
	if action == LoopForceStop {
		t.Errorf("empty-args calls on non-think tools must not trip the empty-think rule, got %v", action)
	}
}
