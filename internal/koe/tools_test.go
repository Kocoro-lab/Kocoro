package koe

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestToolDefsShape(t *testing.T) {
	defs := ToolDefs()
	names := map[string]bool{}
	for _, d := range defs {
		names[d.Name] = true
		if d.Type != "function" {
			t.Errorf("tool %q type = %q, want function", d.Name, d.Type)
		}
	}
	for _, want := range []string{"do_task", "cancel", "get_status", "control_app", "switch_agent"} {
		if !names[want] {
			t.Errorf("missing tool %q", want)
		}
	}
	if len(defs) != 5 {
		t.Errorf("got %d tools, want exactly 5", len(defs))
	}
}

// TestCancelDescriptionRequiresExplicitStopRequest: live 2026-07-02 13:57 the
// S2S model heard 5.8s of background noise mid-task and called cancel, killing a
// 53s task. The description must set an explicit-request bar for cancelling.
func TestCancelDescriptionRequiresExplicitStopRequest(t *testing.T) {
	var desc string
	for _, d := range ToolDefs() {
		if d.Name == "cancel" {
			desc = d.Description
		}
	}
	for _, want := range []string{"clearly and explicitly", "not addressed to you"} {
		if !strings.Contains(desc, want) {
			t.Fatalf("cancel description missing %q", want)
		}
	}
}

// TestDoTaskDescriptionMatchesPersonaContract keeps the tool description in sync
// with the persona: varied content-free acknowledgement (no single mandated
// phrase) and the information-source split (conversation-internal one-step
// replies may be answered directly; the outside world goes through do_task).
func TestDoTaskDescriptionMatchesPersonaContract(t *testing.T) {
	var desc string
	for _, d := range ToolDefs() {
		if d.Name == "do_task" {
			desc = d.Description
		}
	}
	for _, want := range []string{"vary", "one obvious step", "never quiz the user", "context digest"} {
		if !strings.Contains(desc, want) {
			t.Fatalf("do_task description missing %q", want)
		}
	}
	if strings.Contains(desc, `Chinese utterance -> "我来处理"`) {
		t.Fatal("do_task description must not mandate a single fixed acknowledgement phrase anymore")
	}
}

func TestBurstRouteKey(t *testing.T) {
	// MUST equal the keys Plan A Task 3 pins daemon-side.
	if got := burstRouteKey("finance", "burst-123"); got != "agent:finance:koe:burst-123" {
		t.Errorf("burstRouteKey(finance,burst-123) = %q, want agent:finance:koe:burst-123", got)
	}
	if got := burstRouteKey("", "burst-123"); got != "default:koe:burst-123" {
		t.Errorf("burstRouteKey(\"\",burst-123) = %q, want default:koe:burst-123", got)
	}
}

func TestPrepareDoTaskUsesBoundAgent(t *testing.T) {
	state := NewCallState("burst-1", "finance")
	d := NewDispatcher(nil, NewAgentResolver(fixtureAgents(), NoopSemanticMatcher{}), state, nil)
	req, clarify, err := d.PrepareDoTask([]byte(`{"task":"check NVDA"}`))
	if err != nil || clarify != nil {
		t.Fatalf("PrepareDoTask err=%v clarify=%v", err, clarify)
	}
	if req.Agent != "finance" || req.ThreadID != "burst-1" || req.Text != "check NVDA" {
		t.Errorf("req = %+v", req)
	}
}

func TestPrepareDoTaskCarriesCallContext(t *testing.T) {
	state := NewCallState("burst-1", "finance")
	state.SetCallContext(StartCallRequest{
		CWD: "/Users/hu/project",
		ForegroundHint: &ForegroundHint{
			PID:      123,
			AppName:  "Mail",
			BundleID: "com.apple.mail",
		},
	})
	d := NewDispatcher(nil, NewAgentResolver(fixtureAgents(), NoopSemanticMatcher{}), state, nil)
	req, clarify, err := d.PrepareDoTask([]byte(`{"task":"summarize this window"}`))
	if err != nil || clarify != nil {
		t.Fatalf("PrepareDoTask err=%v clarify=%v", err, clarify)
	}
	if req.CWD != "/Users/hu/project" || req.ForegroundHint == nil {
		t.Fatalf("request did not carry call context: %+v", req)
	}
	if req.ForegroundHint.PID != 123 || req.ForegroundHint.AppName != "Mail" || req.ForegroundHint.BundleID != "com.apple.mail" {
		t.Fatalf("foreground hint = %+v", req.ForegroundHint)
	}
}

func TestPrepareDoTaskClarifyOnUnknownAgent(t *testing.T) {
	state := NewCallState("burst-1", "default")
	d := NewDispatcher(nil, NewAgentResolver(fixtureAgents(), NoopSemanticMatcher{}), state, nil)
	_, clarify, err := d.PrepareDoTask([]byte(`{"task":"x","agent":"nonexistent zzz"}`))
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if clarify == nil || clarify.Status != "clarify" {
		t.Errorf("expected clarify SayResult, got %+v", clarify)
	}
}

// TestMapDoTaskOutcomeAttachesContextDigest: the completed result must carry a
// capped digest of the full reply so the Realtime model can answer recaps and
// follow-ups directly. Live 2026-07-02: 2 of 4 delegations in one call were
// re-fetch recaps because Koe only ever held the two spoken sentences.
func TestMapDoTaskOutcomeAttachesContextDigest(t *testing.T) {
	long := strings.Repeat("详情内容", 300) // 1200 runes, over the cap
	r := MapDoTaskOutcome(DoTaskOutcome{Kind: OutcomeCompleted, Reply: long, SpokenSummary: "查完了。"}, nil)
	if r.Context == "" {
		t.Fatal("completed result must carry a context digest of the reply")
	}
	if got := len([]rune(r.Context)); got > defaultVoiceContextCap+1 {
		t.Fatalf("context digest not capped: %d runes", got)
	}
	if !strings.HasPrefix(long, strings.TrimSuffix(r.Context, "…")) {
		t.Fatal("context digest must be a prefix of the reply")
	}

	// No added information → no digest (don't waste session tokens).
	same := MapDoTaskOutcome(DoTaskOutcome{Kind: OutcomeCompleted, Reply: "查完了。", SpokenSummary: "查完了。"}, nil)
	if same.Context != "" {
		t.Fatalf("reply identical to spoken line must not attach a digest, got %q", same.Context)
	}
	if inj := MapDoTaskOutcome(DoTaskOutcome{Kind: OutcomeInjected}, nil); inj.Context != "" {
		t.Fatal("injected outcome must not attach a digest")
	}
}

// TestMapDoTaskOutcomeCancelledStaysSilent: a user-cancelled run's reply is the
// tail of whatever was streaming when the run died — live 2026-07-02 13:57 Koe
// read the killed run's progress line "正在将报告要点写入桌面 Markdown 文件。"
// right after announcing the cancel. A cancelled outcome must carry status only:
// the model already acknowledged the stop in its own words when the cancel tool
// returned, so there is nothing to voice (and no canned phrase either).
func TestMapDoTaskOutcomeCancelledStaysSilent(t *testing.T) {
	r := MapDoTaskOutcome(DoTaskOutcome{
		Kind:        OutcomeCompleted,
		Reply:       "正在将报告要点写入桌面 Markdown 文件。",
		FailureCode: "user_cancelled",
	}, nil)
	if r.Status != "cancelled" {
		t.Fatalf("cancelled run status = %q, want cancelled", r.Status)
	}
	if r.Say != "" || r.SpokenSummary != "" || r.Context != "" {
		t.Fatalf("cancelled run must carry no speech or digest, got say=%q spoken=%q ctx=%q", r.Say, r.SpokenSummary, r.Context)
	}
}

func TestMapDoTaskOutcome(t *testing.T) {
	if got := MapDoTaskOutcome(DoTaskOutcome{Kind: OutcomeCompleted, Reply: "long done", SpokenSummary: "done"}, nil); got.Status != "ok" || got.SpokenSummary != "done" || got.Say != "done" {
		t.Errorf("completed: %+v", got)
	}
	if got := MapDoTaskOutcome(DoTaskOutcome{Kind: OutcomeCompleted, Reply: "done"}, nil); got.SpokenSummary != "done" {
		t.Errorf("completed without spoken summary: %+v", got)
	}
	// injected MUST carry an empty say so the front brain doesn't double-speak.
	if got := MapDoTaskOutcome(DoTaskOutcome{Kind: OutcomeInjected}, nil); got.Status != "injected" || got.Say != "" {
		t.Errorf("injected → %+v", got)
	}
	if got := MapDoTaskOutcome(DoTaskOutcome{Kind: OutcomeRejected, Reason: "cwd_conflict"}, nil); got.Status != "failed" {
		t.Errorf("rejected → %+v", got)
	}
	if got := MapDoTaskOutcome(DoTaskOutcome{}, fmt.Errorf("boom")); got.Status != "failed" {
		t.Errorf("transport error → %+v", got)
	}
}

func TestDispatchRejectsDoTask(t *testing.T) {
	d := NewDispatcher(nil, NewAgentResolver(fixtureAgents(), NoopSemanticMatcher{}), NewCallState("b", "default"), nil)
	if _, err := d.Dispatch(context.Background(), "do_task", []byte(`{"task":"x"}`)); err == nil {
		t.Error("Dispatch(do_task) must error — do_task is async (PrepareDoTask + goroutine), not a fast tool")
	}
}

func TestDispatchCancelUsesBurstKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var got map[string]any
		json.NewDecoder(r.Body).Decode(&got)
		if got["route_key"] != "agent:finance:koe:burst-1" {
			t.Errorf("cancel route_key = %v, want agent:finance:koe:burst-1", got["route_key"])
		}
		json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer srv.Close()

	state := NewCallState("burst-1", "finance")
	d := NewDispatcher(NewDaemonClient(srv.URL), NewAgentResolver(fixtureAgents(), NoopSemanticMatcher{}), state, nil)
	if _, err := d.Dispatch(context.Background(), "cancel", []byte(`{"reason":"user_cancel"}`)); err != nil {
		t.Fatalf("cancel: %v", err)
	}
}

func TestDispatchCancelUsesInFlightPerCallAgentRoute(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var got map[string]any
		json.NewDecoder(r.Body).Decode(&got)
		if got["route_key"] != "agent:finance:koe:burst-1" {
			t.Errorf("cancel route_key = %v, want agent:finance:koe:burst-1", got["route_key"])
		}
		json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer srv.Close()

	state := NewCallState("burst-1", "")
	state.SetInFlightForAgent("check NVDA", "finance")
	d := NewDispatcher(NewDaemonClient(srv.URL), NewAgentResolver(fixtureAgents(), NoopSemanticMatcher{}), state, nil)
	if _, err := d.Dispatch(context.Background(), "cancel", []byte(`{"reason":"user_cancel"}`)); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if got := state.InFlight(); got != "" {
		t.Fatalf("cancel should clear all in-flight state, got %q", got)
	}
}

func TestDispatchCancelCancelsAllInFlightRoutes(t *testing.T) {
	var routes []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var got map[string]any
		json.NewDecoder(r.Body).Decode(&got)
		routes = append(routes, got["route_key"].(string))
		json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer srv.Close()

	state := NewCallState("burst-1", "")
	state.SetInFlightForAgent("check NVDA", "finance")
	state.SetInFlightForAgent("review contract", "legal")
	d := NewDispatcher(NewDaemonClient(srv.URL), NewAgentResolver(fixtureAgents(), NoopSemanticMatcher{}), state, nil)
	if _, err := d.Dispatch(context.Background(), "cancel", []byte(`{"reason":"user_cancel"}`)); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	want := []string{"agent:finance:koe:burst-1", "agent:legal:koe:burst-1"}
	if strings.Join(routes, ",") != strings.Join(want, ",") {
		t.Fatalf("cancel routes = %v, want %v", routes, want)
	}
}

func TestDispatchSwitchAgentRebinds(t *testing.T) {
	state := NewCallState("burst-1", "default")
	d := NewDispatcher(nil, NewAgentResolver(fixtureAgents(), NoopSemanticMatcher{}), state, nil)
	if _, err := d.Dispatch(context.Background(), "switch_agent", []byte(`{"agent":"finance"}`)); err != nil {
		t.Fatalf("switch_agent: %v", err)
	}
	if got := state.BoundAgent(); got != "finance" {
		t.Errorf("bound agent = %q, want finance", got)
	}
}

func TestDispatchControlAppStub(t *testing.T) {
	var seen string
	state := NewCallState("burst-1", "default")
	d := NewDispatcher(nil, NewAgentResolver(fixtureAgents(), NoopSemanticMatcher{}), state,
		func(ctx context.Context, action string) error { seen = action; return nil })
	if _, err := d.Dispatch(context.Background(), "control_app", []byte(`{"action":"open_settings"}`)); err != nil {
		t.Fatalf("control_app: %v", err)
	}
	if seen != "open_settings" {
		t.Errorf("control_app action = %q, want open_settings", seen)
	}
}

// TestDoTaskDescriptionUsesOneSelfFraming pins the schema-level framing: the tool
// description the model reads when deciding whether to call do_task must NOT
// contradict the one-self persona ("back-brain" / "delegate to" frame it as
// someone else, which let the model improvise must-delegate intents like math),
// and must give a concrete reason to delegate.
func TestDoTaskDescriptionUsesOneSelfFraming(t *testing.T) {
	var doTask, controlApp, switchAgent string
	for _, d := range ToolDefs() {
		switch d.Name {
		case "do_task":
			doTask = strings.ToLower(d.Description)
		case "control_app":
			controlApp = strings.ToLower(d.Description)
		case "switch_agent":
			switchAgent = strings.ToLower(d.Description)
		}
	}
	if doTask == "" {
		t.Fatal("do_task tool not found")
	}
	for _, banned := range []string{"back-brain", "back brain", "delegate to"} {
		if strings.Contains(doTask, banned) {
			t.Errorf("do_task description must not contain %q (contradicts one-self persona)", banned)
		}
	}
	// "own hands" (not the removed "own tools" lecture sentence): first-person
	// framing survives in the description head after the one-self trim (2026-07-02).
	for _, want := range []string{"calculate precisely", "never answer", "own hands", "long or multi-part", "content/results to show in kocoro desktop"} {
		if !strings.Contains(doTask, want) {
			t.Errorf("do_task description missing %q", want)
		}
	}
	for _, want := range []string{"never use this to display", "use do_task for content/results"} {
		if !strings.Contains(controlApp, want) {
			t.Errorf("control_app description missing %q", want)
		}
	}
	if strings.Contains(switchAgent, "back-brain") {
		t.Error("switch_agent description must not contain 'back-brain'")
	}
}
