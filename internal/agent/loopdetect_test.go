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

	// 2nd consecutive identical call: nudge (consecDupThreshold=2)
	ld.Record("web_search", `{"q":"test"}`, false, "", "", false)
	action, msg := ld.Check("web_search")
	if action != LoopNudge {
		t.Errorf("2 consecutive identical calls should nudge, got %v", action)
	}
	if msg == "" {
		t.Error("nudge should have a message")
	}
}

func TestLoopDetector_ConsecutiveDup_ForceStop(t *testing.T) {
	ld := NewLoopDetector()

	// 3 consecutive identical calls: force stop (consecDupThreshold+1)
	for range 3 {
		ld.Record("web_search", `{"q":"test"}`, false, "", "", false)
	}
	action, _ := ld.Check("web_search")
	if action != LoopForceStop {
		t.Errorf("3 consecutive identical calls should force stop, got %v", action)
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

	// 3 spread-out identical calls: window-based nudge (exactDupThreshold=3)
	ld.Record("file_read", `{"file":"main.go"}`, false, "", "", false)
	ld.Record("file_edit", `{"old":"a","new":"b"}`, false, "", "", false)
	ld.Record("file_read", `{"file":"main.go"}`, false, "", "", false)
	ld.Record("file_edit", `{"old":"b","new":"c"}`, false, "", "", false)
	ld.Record("file_read", `{"file":"main.go"}`, false, "", "", false)

	action, _ := ld.Check("file_read")
	if action != LoopNudge {
		t.Errorf("3 spread-out identical calls should trigger window nudge, got %v", action)
	}
}

func TestLoopDetector_WindowDup_ForceStop(t *testing.T) {
	ld := NewLoopDetector()

	// 6 spread-out identical calls: window force stop (2× exactDupThreshold)
	for range 6 {
		ld.Record("file_read", `{"file":"main.go"}`, false, "", "", false)
		ld.Record("file_edit", `{"x":"y"}`, false, "", "", false)
	}
	action, _ := ld.Check("file_read")
	if action != LoopForceStop {
		t.Errorf("6 spread-out identical calls should force stop, got %v", action)
	}
}

func TestLoopDetector_SameToolError_Nudge(t *testing.T) {
	ld := NewLoopDetector()

	// 3 errors: no trigger (threshold is 4)
	for i := range 3 {
		ld.Record("file_edit", fmt.Sprintf(`{"file":"f%d"}`, i), true, "permission denied", "", false)
	}
	action, _ := ld.Check("file_edit")
	if action != LoopContinue {
		t.Errorf("3 errors should not trigger, got %v", action)
	}

	// 4th error: nudge
	ld.Record("file_edit", `{"file":"f4"}`, true, "permission denied", "", false)
	action, msg := ld.Check("file_edit")
	if action != LoopNudge {
		t.Errorf("4 errors should trigger nudge, got %v", action)
	}
	if msg == "" {
		t.Error("nudge should have a message")
	}
}

func TestLoopDetector_SameToolError_ForceStop(t *testing.T) {
	ld := NewLoopDetector()

	// 8 errors: force stop (2× threshold of 4)
	for i := range 8 {
		ld.Record("file_edit", fmt.Sprintf(`{"file":"f%d"}`, i), true, "permission denied", "", false)
	}
	action, _ := ld.Check("file_edit")
	if action != LoopForceStop {
		t.Errorf("8 errors should trigger force stop, got %v", action)
	}
}

func TestLoopDetector_NoProgress_Nudge(t *testing.T) {
	ld := NewLoopDetector()

	// 7 calls with different args: no trigger (threshold is 8)
	// Use think (not in any tool family, not semi-repeatable) to test pure
	// NoProgress detection. bash is semi-repeatable (threshold 12) so it
	// wouldn't trigger at 8.
	for i := range 7 {
		ld.Record("think", fmt.Sprintf(`{"thought":"idea%d"}`, i), false, "", "", false)
	}
	action, _ := ld.Check("think")
	if action != LoopContinue {
		t.Errorf("7 calls should not trigger, got %v", action)
	}

	// 8th call: nudge
	ld.Record("think", `{"thought":"idea8"}`, false, "", "", false)
	action, _ = ld.Check("think")
	if action != LoopNudge {
		t.Errorf("8 calls should trigger nudge, got %v", action)
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
	ld.Record("screenshot", `{}`, false, "", "", false)
	ld.Record("screenshot", `{}`, false, "", "", false)
	action, _ := ld.Check("screenshot")
	if action != LoopNudge {
		t.Errorf("2 consecutive identical screenshot calls should nudge, got %v", action)
	}
}

func TestLoopDetector_SlidingWindow(t *testing.T) {
	ld := NewLoopDetector()
	ld.historySize = 5 // small window for testing

	// Fill window with 2 consecutive bash duplicates (triggers consecutive nudge)
	ld.Record("bash", `{"cmd":"ls"}`, false, "", "", false)
	ld.Record("bash", `{"cmd":"ls"}`, false, "", "", false)
	action, _ := ld.Check("bash")
	if action != LoopNudge {
		t.Error("2 consecutive exact dups should nudge")
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
	// 3 web_search calls with varied but same-topic queries → family nudge at 3
	ld.Record("web_search", `{"query":"world climate today March 2 2026 major headlines"}`, false, "", "", false)
	ld.Record("web_search", `{"query":"world climate March 2 2026 top headlines latest"}`, false, "", "", false)
	ld.Record("web_search", `{"query":"world climate today March 2 2026 breaking news"}`, false, "", "", false)
	action, msg := ld.Check("web_search")
	if action != LoopNudge {
		t.Errorf("3 same-topic web searches should nudge, got %v", action)
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

	// 2 same-topic (from web_search) + 1 different (web_fetch URL) → not yet 3
	action, _ := ld.Check("web_fetch")
	if action != LoopContinue {
		t.Errorf("2 same-topic + 1 different should continue, got %v", action)
	}

	// Add one more same-topic search (only date differs) → 3 same-topic in family → nudge
	ld.Record("web_search", `{"query":"latest golang tutorial today"}`, false, "", "", false)
	action, _ = ld.Check("web_search")
	if action != LoopNudge {
		t.Errorf("3 same-topic family calls should nudge, got %v", action)
	}
}

func TestLoopDetector_WebFamily_ResultSigDedup(t *testing.T) {
	ld := NewLoopDetector()
	// 3 calls returning the same domains → no new info → nudge
	ld.Record("web_search", `{"query":"ai research papers"}`, false, "", "reuters.com,bbc.com", false)
	ld.Record("web_search", `{"query":"ai research latest papers"}`, false, "", "reuters.com,bbc.com", false)
	ld.Record("web_search", `{"query":"ai research papers review"}`, false, "", "reuters.com,bbc.com", false)
	action, _ := ld.Check("web_search")
	if action != LoopNudge {
		t.Errorf("3 calls with same result signature should nudge, got %v", action)
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
// 8 web_search calls with varied "world news" queries, then web_fetch calls.
// The detector should catch it much earlier than the original ~15 calls.
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

	if firstNudge == 0 || firstNudge > 3 {
		t.Errorf("expected first nudge by call 3, got %d", firstNudge)
	}
	if firstForceStop == 0 || firstForceStop > 7 {
		t.Errorf("expected force stop by call 7, got %d", firstForceStop)
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

	// 3 searches on same topic (all normalize to "climate world")
	ld.Record("web_search", `{"query":"world climate today March 2 2026"}`, false, "", "", false)
	ld.Record("web_search", `{"query":"world climate March 2 2026 latest"}`, false, "", "", false)
	ld.Record("web_search", `{"query":"world climate today latest headlines"}`, false, "", "", false)

	// Should already be nudging (3 same-topic)
	action, _ := ld.Check("web_search")
	if action != LoopNudge {
		t.Errorf("expected nudge after 3 same-topic searches, got %v", action)
	}

	// Switch to web_fetch then back — same-topic counter continues via family lookup
	// Only filler/date variations so topic hash stays the same
	ld.Record("web_fetch", `{"url":"https://reuters.com/world/climate"}`, false, "", "", false)
	ld.Record("web_search", `{"query":"world climate 2026"}`, false, "", "", false)
	ld.Record("web_search", `{"query":"world climate today"}`, false, "", "", false)
	ld.Record("web_search", `{"query":"world climate latest"}`, false, "", "", false)

	// 3 original + 3 more same-topic web_search = 6 same-topic + web_fetch = 7 family
	// But force stop requires progressCount >= 7, so need 7 same-topic.
	// The 6 web_search calls all share "climate world" topic.
	// web_fetch has different topic (URL). So progressCount = 6, not 7.
	// 6 >= 5 → stronger nudge (not force stop).
	action, _ = ld.Check("web_search")
	if action != LoopNudge {
		t.Errorf("expected nudge after 6 same-topic web calls, got %v", action)
	}

	// One more same-topic → 7 same-topic → force stop
	ld.Record("web_search", `{"query":"world climate current"}`, false, "", "", false)
	action, _ = ld.Check("web_search")
	if action != LoopForceStop {
		t.Errorf("expected force stop after 7 same-topic web calls, got %v", action)
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

func TestLoopDetector_SleepDetection_Nudge(t *testing.T) {
	ld := NewLoopDetector()

	// 1 sleep call: no trigger
	ld.Record("bash", `{"command":"sleep 5"}`, false, "", "", false)
	action, _ := ld.Check("bash")
	if action != LoopContinue {
		t.Errorf("1 sleep call should not trigger, got %v", action)
	}

	// 2nd sleep call: nudge
	ld.Record("bash", `{"command":"sleep 5 && curl http://localhost:8080"}`, false, "", "", false)
	action, msg := ld.Check("bash")
	if action != LoopNudge {
		t.Errorf("2 sleep calls should nudge, got %v", action)
	}
	if msg == "" {
		t.Error("nudge should have a message")
	}
}

func TestLoopDetector_SleepDetection_ForceStop(t *testing.T) {
	ld := NewLoopDetector()

	// 4 sleep calls: force stop
	ld.Record("bash", `{"command":"sleep 5"}`, false, "", "", false)
	ld.Record("bash", `{"command":"sleep 1 && echo done"}`, false, "", "", false)
	ld.Record("bash", `{"command":"while true; do sleep 1; done"}`, false, "", "", false)
	ld.Record("bash", `{"command":"sleep 10"}`, false, "", "", false)
	action, _ := ld.Check("bash")
	if action != LoopForceStop {
		t.Errorf("4 sleep calls should force stop, got %v", action)
	}
}

func TestLoopDetector_SleepDetection_NoFalsePositive(t *testing.T) {
	ld := NewLoopDetector()

	// bash commands without sleep: no trigger
	ld.Record("bash", `{"command":"echo hello"}`, false, "", "", false)
	ld.Record("bash", `{"command":"cat sleep.log"}`, false, "", "", false)
	ld.Record("bash", `{"command":"grep sleeper main.go"}`, false, "", "", false)
	ld.Record("bash", `{"command":"ls -la"}`, false, "", "", false)
	action, _ := ld.Check("bash")
	if action != LoopContinue {
		t.Errorf("non-sleep bash commands should not trigger, got %v", action)
	}
}

func TestLoopDetector_SleepDetection_IgnoreNonBash(t *testing.T) {
	ld := NewLoopDetector()

	// sleep in non-bash tool args: no trigger (different args to avoid dup detection)
	ld.Record("file_read", `{"command":"sleep 5"}`, false, "", "", false)
	ld.Record("grep", `{"command":"sleep 10"}`, false, "", "", false)
	ld.Record("file_read", `{"command":"sleep 15"}`, false, "", "", false)
	ld.Record("grep", `{"command":"sleep 20"}`, false, "", "", false)
	action, _ := ld.Check("grep")
	if action != LoopContinue {
		t.Errorf("sleep in non-bash tool args should not trigger, got %v", action)
	}
}

func TestLoopDetector_SearchEscalation_Nudge(t *testing.T) {
	ld := NewLoopDetector()

	// 4 consecutive unproductive search calls: no trigger yet
	for i := 0; i < 4; i++ {
		ld.Record("grep", fmt.Sprintf(`{"pattern":"term%d"}`, i), false, "", "", true)
	}
	action, _ := ld.Check("grep")
	if action != LoopContinue {
		t.Errorf("4 unproductive search calls should not trigger, got %v", action)
	}

	// 5th unproductive search call: nudge
	ld.Record("grep", `{"pattern":"term5"}`, false, "", "", true)
	action, msg := ld.Check("grep")
	if action != LoopNudge {
		t.Errorf("5 unproductive search calls should nudge, got %v", action)
	}
	if msg == "" {
		t.Error("nudge should have a message")
	}
}

func TestLoopDetector_SearchEscalation_ForceStop(t *testing.T) {
	ld := NewLoopDetector()

	// 8 consecutive unproductive search calls (mixed grep/glob): force stop
	for i := 0; i < 8; i++ {
		tool := "grep"
		if i%2 == 1 {
			tool = "glob"
		}
		ld.Record(tool, fmt.Sprintf(`{"pattern":"term%d"}`, i), false, "", "", true)
	}
	action, _ := ld.Check("glob")
	if action != LoopForceStop {
		t.Errorf("8 unproductive search calls should force stop, got %v", action)
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

	// 5 unproductive mixed grep+glob calls: nudge
	ld.Record("grep", `{"pattern":"foo"}`, false, "", "", true)
	ld.Record("glob", `{"pattern":"**/*.go"}`, false, "", "", true)
	ld.Record("grep", `{"pattern":"bar"}`, false, "", "", true)
	ld.Record("glob", `{"pattern":"**/*.ts"}`, false, "", "", true)
	ld.Record("grep", `{"pattern":"baz"}`, false, "", "", true)

	action, msg := ld.Check("grep")
	if action != LoopNudge {
		t.Errorf("5 unproductive mixed search calls should nudge, got %v", action)
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

	// Simulate 3 browser calls with the same URL (same topic hash) but different
	// extra fields to produce different ArgsHash and avoid ConsecutiveDup detector.
	ld.Record("browser", `{"action":"navigate","url":"https://jd.com/search?q=huawei","wait":1}`, false, "", "", false)
	ld.Record("browser", `{"action":"navigate","url":"https://jd.com/search?q=huawei","wait":2}`, false, "", "", false)
	ld.Record("browser", `{"action":"navigate","url":"https://jd.com/search?q=huawei","wait":3}`, false, "", "", false)
	action, msg := ld.Check("browser")
	if action != LoopNudge {
		t.Errorf("3 same-topic browser calls should nudge, got %v", action)
	}
	if !strings.Contains(msg, "same topic") {
		t.Errorf("expected 'same topic' in message, got: %s", msg)
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
// gets the elevated NoProgress threshold (12) instead of the generic (8),
// so multi-step scripting workflows (fetch → process → install → build)
// aren't killed before completing. The exact-dup, same-error, and sleep
// detectors still catch real loops at their own lower thresholds.
func TestLoopDetector_SemiRepeatable_BashHigherThreshold(t *testing.T) {
	ld := NewLoopDetector()

	// 8 distinct bash calls — would nudge with the generic threshold,
	// but should be Continue with the semi-repeatable threshold of 12.
	for i := range 8 {
		ld.Record("bash", fmt.Sprintf(`{"command":"step_%d"}`, i), false, "", "", false)
	}
	action, _ := ld.Check("bash")
	if action != LoopContinue {
		t.Errorf("8 distinct bash calls should Continue (semi-repeatable threshold 12), got %v", action)
	}

	// 11 calls — still under 12.
	for i := 8; i < 11; i++ {
		ld.Record("bash", fmt.Sprintf(`{"command":"step_%d"}`, i), false, "", "", false)
	}
	action, _ = ld.Check("bash")
	if action != LoopContinue {
		t.Errorf("11 distinct bash calls should Continue, got %v", action)
	}

	// 12th call → nudge.
	ld.Record("bash", `{"command":"step_12"}`, false, "", "", false)
	action, _ = ld.Check("bash")
	if action != LoopNudge {
		t.Errorf("12 bash calls should nudge, got %v", action)
	}
}

// TestLoopDetector_SemiRepeatable_NonBashUnchanged verifies that the generic
// NoProgress threshold (8) still applies to non-semi-repeatable tools like
// file_write, think, etc. — unchanged from before.
func TestLoopDetector_SemiRepeatable_NonBashUnchanged(t *testing.T) {
	ld := NewLoopDetector()

	for i := range 8 {
		ld.Record("think", fmt.Sprintf(`{"thought":"idea_%d"}`, i), false, "", "", false)
	}
	action, _ := ld.Check("think")
	if action != LoopNudge {
		t.Errorf("8 think calls should nudge at generic threshold, got %v", action)
	}
}
