//go:build darwin && cgo

package koe

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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
	for _, want := range []string{"do_task", "cancel", "get_status", "control_app", "switch_agent", "end_call"} {
		if !names[want] {
			t.Errorf("missing tool %q", want)
		}
	}
	if len(defs) != 6 {
		t.Errorf("got %d tools, want exactly 6", len(defs))
	}
}

// TestEndCallDescriptionSignalsDismissIntent keeps end_call scoped to a real
// conversation-ending dismiss (not a topic change or a single-task cancel), tells
// the model to judge from audio (the input transcription garbles short phrases),
// and to stay silent. It also pins the re-activation contract (double-tap Option).
func TestEndCallDescriptionSignalsDismissIntent(t *testing.T) {
	var desc string
	for _, d := range ToolDefs() {
		if d.Name == "end_call" {
			desc = d.Description
		}
	}
	if desc == "" {
		t.Fatal("end_call tool missing")
	}
	for _, want := range []string{"闭嘴", "goodbye", "double-tap the Option", "Say NOTHING", "cancel", "unsure"} {
		if !strings.Contains(desc, want) {
			t.Fatalf("end_call description missing %q", want)
		}
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
	for _, want := range []string{"vary", "one obvious step", "never quiz the user", "context digest", "stable public knowledge", "nature of the information"} {
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
	req, _, clarify, err := d.PrepareDoTask([]byte(`{"task":"check NVDA"}`), "zh", false)
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
	req, _, clarify, err := d.PrepareDoTask([]byte(`{"task":"summarize this window"}`), "zh", false)
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
	_, _, clarify, err := d.PrepareDoTask([]byte(`{"task":"ask nonexistent zzz to check x","agent":"nonexistent zzz"}`), "zh", false)
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
	r := MapDoTaskOutcome(DoTaskOutcome{Kind: OutcomeCompleted, Reply: long, SpokenSummary: "查完了。"}, nil, "zh")
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
	same := MapDoTaskOutcome(DoTaskOutcome{Kind: OutcomeCompleted, Reply: "查完了。", SpokenSummary: "查完了。"}, nil, "zh")
	if same.Context != "" {
		t.Fatalf("reply identical to spoken line must not attach a digest, got %q", same.Context)
	}
	if inj := MapDoTaskOutcome(DoTaskOutcome{Kind: OutcomeInjected}, nil, "zh"); inj.Context != "" {
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
	}, nil, "zh")
	if r.Status != "cancelled" {
		t.Fatalf("cancelled run status = %q, want cancelled", r.Status)
	}
	if r.Say != "" || r.SpokenSummary != "" || r.Context != "" {
		t.Fatalf("cancelled run must carry no speech or digest, got say=%q spoken=%q ctx=%q", r.Say, r.SpokenSummary, r.Context)
	}
}

// TestMapDoTaskOutcomePartialDoesNotVoiceProgress: a partial run (soft timeout /
// max-iter / idle stop, NOT user_cancelled) returns only the tail of whatever was
// streaming — often a tool preamble / progress line ("正在整理成结构化报告。").
// Voicing that (or seeding it as a recap digest) reads a progress narration aloud
// as if it were the finished result. A partial outcome must speak a safe status
// line instead and claim no completion. Repro 2026-07-10: a timed-out MARL research
// delegation had its progress line "已收集足够信息，现在整理成结构化报告。" projected
// into spoken_summary.
func TestMapDoTaskOutcomePartialDoesNotVoiceProgress(t *testing.T) {
	progress := "已收集足够信息，现在整理成结构化报告。"
	// Every partial finish reason (idle/deadline timeout, iteration limit, or an
	// unlabelled soft stop) leaves Reply/SpokenSummary as an untrustworthy progress
	// tail. None of them may be voiced or seeded as a digest.
	for _, failure := range []string{"deadline_exceeded", "iteration_limit", ""} {
		r := MapDoTaskOutcome(DoTaskOutcome{
			Kind:          OutcomeCompleted,
			Reply:         progress,
			SpokenSummary: progress, // mechanical fallback over the partial lastText
			Partial:       true,
			FailureCode:   failure,
		}, nil, "zh")
		if r.Status != "failed" {
			t.Fatalf("partial(%q) status = %q, want failed", failure, r.Status)
		}
		if strings.Contains(r.Say, progress) || strings.Contains(r.SpokenSummary, progress) {
			t.Fatalf("partial(%q) must NOT voice the progress line, got say=%q spoken=%q", failure, r.Say, r.SpokenSummary)
		}
		if r.Context != "" {
			t.Fatalf("partial(%q) must not seed a recap digest, got %q", failure, r.Context)
		}
		if want := fallbackSay("zh", "incomplete"); r.Say == "" || r.Say != want {
			t.Fatalf("partial(%q) say = %q, want the safe canned line %q", failure, r.Say, want)
		}
	}
}

// TestMapDoTaskOutcomeCancelBeatsPartial: a user-cancelled run may ALSO be flagged
// partial (the cancel force-stops mid-stream). The cancel guard runs first, so it
// must win — status "cancelled" with no speech, never the partial "incomplete"
// line, so Koe stays silent (the model already acknowledged the stop).
func TestMapDoTaskOutcomeCancelBeatsPartial(t *testing.T) {
	r := MapDoTaskOutcome(DoTaskOutcome{
		Kind:        OutcomeCompleted,
		Reply:       "正在整理…",
		Partial:     true,
		FailureCode: "user_cancelled",
	}, nil, "zh")
	if r.Status != "cancelled" {
		t.Fatalf("cancelled+partial status = %q, want cancelled", r.Status)
	}
	if r.Say != "" || r.SpokenSummary != "" || r.Context != "" {
		t.Fatalf("cancelled+partial must stay silent, got say=%q spoken=%q ctx=%q", r.Say, r.SpokenSummary, r.Context)
	}
}

func TestMapDoTaskOutcome(t *testing.T) {
	if got := MapDoTaskOutcome(DoTaskOutcome{Kind: OutcomeCompleted, Reply: "long done", SpokenSummary: "done"}, nil, "zh"); got.Status != "ok" || got.SpokenSummary != "done" || got.Say != "done" {
		t.Errorf("completed: %+v", got)
	}
	if got := MapDoTaskOutcome(DoTaskOutcome{Kind: OutcomeCompleted, Reply: "done"}, nil, "zh"); got.SpokenSummary != "done" {
		t.Errorf("completed without spoken summary: %+v", got)
	}
	// injected MUST carry an empty say so the front brain doesn't double-speak.
	if got := MapDoTaskOutcome(DoTaskOutcome{Kind: OutcomeInjected}, nil, "zh"); got.Status != "injected" || got.Say != "" {
		t.Errorf("injected → %+v", got)
	}
	if got := MapDoTaskOutcome(DoTaskOutcome{Kind: OutcomeRejected, Reason: "cwd_conflict"}, nil, "zh"); got.Status != "failed" {
		t.Errorf("rejected → %+v", got)
	}
	if got := MapDoTaskOutcome(DoTaskOutcome{}, fmt.Errorf("boom"), "zh"); got.Status != "failed" {
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
	t.Setenv("KOE_TASK_LEDGER", "0")
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
	t.Setenv("KOE_TASK_LEDGER", "0")
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
	t.Setenv("KOE_TASK_LEDGER", "0")
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

func TestToolDefsLedgerSchema(t *testing.T) {
	t.Setenv("KOE_TASK_LEDGER", "1")
	on := ToolDefs()
	t.Setenv("KOE_TASK_LEDGER", "0")
	off := ToolDefs()
	find := func(defs []ToolDef, name string) ToolDef {
		for _, def := range defs {
			if def.Name == name {
				return def
			}
		}
		t.Fatalf("tool %q missing", name)
		return ToolDef{}
	}
	if !strings.Contains(string(find(on, "do_task").Parameters), `"relationship"`) ||
		!strings.Contains(string(find(on, "cancel").Parameters), `"task_id"`) {
		t.Fatal("ledger tool schemas must expose relationship and task identity")
	}
	if strings.Contains(string(find(off, "do_task").Parameters), `"relationship"`) ||
		strings.Contains(string(find(off, "cancel").Parameters), `"task_id"`) {
		t.Fatal("ledger rollback must restore legacy schemas")
	}
}

func TestPrepareDoTaskRelationship(t *testing.T) {
	t.Setenv("KOE_TASK_LEDGER", "1")
	newDispatcher := func() (*Dispatcher, *CallState) {
		state := NewCallState("burst-p", "")
		return NewDispatcher(NewDaemonClient(""), NewAgentResolver(nil, NoopSemanticMatcher{}), state, nil), state
	}

	t.Run("parallel independent calls use separate lanes", func(t *testing.T) {
		dispatcher, _ := newDispatcher()
		first, firstTask, clarify, err := dispatcher.PrepareDoTask([]byte(`{"task":"check Tokyo weather","relationship":"new"}`), "en", false)
		if err != nil || clarify != nil || firstTask == nil || first.ThreadID != "burst-p" {
			t.Fatalf("first task: req=%+v task=%+v clarify=%+v err=%v", first, firstTask, clarify, err)
		}
		second, secondTask, clarify, err := dispatcher.PrepareDoTask([]byte(`{"task":"check Osaka weather","relationship":"new"}`), "en", false)
		if err != nil || clarify != nil || secondTask == nil || second.ThreadID != "burst-p.t02" {
			t.Fatalf("second task: req=%+v task=%+v clarify=%+v err=%v", second, secondTask, clarify, err)
		}
	})

	t.Run("second same-response omitted relationship still forks", func(t *testing.T) {
		dispatcher, _ := newDispatcher()
		_, first, _, _ := dispatcher.PrepareDoTask([]byte(`{"task":"check Tokyo weather"}`), "en", false)
		req, second, clarify, err := dispatcher.PrepareDoTask([]byte(`{"task":"check Osaka news"}`), "en", true)
		if err != nil || clarify != nil || first == nil || second == nil || first.ID == second.ID || req.ThreadID != "burst-p.t02" {
			t.Fatalf("same-response split failed: first=%+v second=%+v req=%+v clarify=%+v err=%v", first, second, req, clarify, err)
		}
	})

	t.Run("follow-up targets task identity", func(t *testing.T) {
		dispatcher, state := newDispatcher()
		state.BeginTask("check Tokyo weather", "")
		target := state.BeginTask("sort unread email", "")
		req, task, clarify, err := dispatcher.PrepareDoTask([]byte(`{"task":"only include urgent messages","relationship":"follow_up","task_id":"`+target.ID+`"}`), "en", false)
		if err != nil || clarify != nil || task == nil || task.ID != target.ID || req.ThreadID != target.ThreadID {
			t.Fatalf("targeted follow-up drifted: req=%+v task=%+v clarify=%+v err=%v", req, task, clarify, err)
		}
	})

	t.Run("ambiguous follow-up clarifies", func(t *testing.T) {
		dispatcher, state := newDispatcher()
		state.BeginTask("check Tokyo weather", "")
		state.BeginTask("sort unread email", "")
		_, task, clarify, err := dispatcher.PrepareDoTask([]byte(`{"task":"change that","relationship":"follow_up","task_id":"t99"}`), "en", false)
		if err != nil || task != nil || clarify == nil || clarify.Status != "clarify" {
			t.Fatalf("want task clarification: task=%+v clarify=%+v err=%v", task, clarify, err)
		}
	})
}

func TestDispatchLedgerStatusAndTargetedCancel(t *testing.T) {
	t.Setenv("KOE_TASK_LEDGER", "1")
	cancelled := make(chan string, 2)
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req CancelRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		cancelled <- req.RouteKey
		w.WriteHeader(http.StatusOK)
	}))
	defer mock.Close()

	state := NewCallState("burst-c", "")
	dispatcher := NewDispatcher(NewDaemonClient(mock.URL), NewAgentResolver(nil, NoopSemanticMatcher{}), state, nil)
	first := state.BeginTask("check Tokyo weather", "")
	second := state.BeginTask("sort unread email", "")

	status, err := dispatcher.Dispatch(context.Background(), "get_status", []byte(`{}`))
	if err != nil || !strings.Contains(string(status), `"task_id":"t01"`) || !strings.Contains(string(status), `"task_id":"t02"`) {
		t.Fatalf("ledger status missing tasks: %s err=%v", status, err)
	}
	ambiguous, _ := dispatcher.Dispatch(context.Background(), "cancel", []byte(`{}`))
	if !strings.Contains(string(ambiguous), `"clarify"`) {
		t.Fatalf("ambiguous cancel must not kill all tasks: %s", ambiguous)
	}
	select {
	case route := <-cancelled:
		t.Fatalf("ambiguous cancel unexpectedly hit %q", route)
	default:
	}

	out, _ := dispatcher.Dispatch(context.Background(), "cancel", []byte(`{"task_id":"`+second.ID+`"}`))
	if !strings.Contains(string(out), `"status":"ok"`) {
		t.Fatalf("targeted cancel failed: %s", out)
	}
	select {
	case route := <-cancelled:
		if route != routeKeyFor("", second.ThreadID) {
			t.Fatalf("cancel route=%q, want %q", route, routeKeyFor("", second.ThreadID))
		}
	case <-time.After(time.Second):
		t.Fatal("targeted cancel did not reach daemon")
	}
	if got, _ := state.TaskByID(second.ID); got.State != TaskCancelled {
		t.Fatalf("cancelled task state=%s", got.State)
	}
	if got, _ := state.TaskByID(first.ID); got.State != TaskRunning {
		t.Fatalf("unrelated task state=%s, want running", got.State)
	}
}

func TestPerCallAgentOverrideRequiresUserNamedAgent(t *testing.T) {
	newDispatcher := func() *Dispatcher {
		state := NewCallState("burst-agent", "")
		return NewDispatcher(NewDaemonClient(""), NewAgentResolver(fixtureAgents(), NoopSemanticMatcher{}), state, nil)
	}

	dispatcher := newDispatcher()
	req, _, clarify, err := dispatcher.PrepareDoTask([]byte(`{"task":"check Tokyo weather","agent":"finance"}`), "en", false)
	if err != nil || clarify != nil || req.Agent != "" {
		t.Fatalf("model-invented override was not ignored: req=%+v clarify=%+v err=%v", req, clarify, err)
	}

	dispatcher = newDispatcher()
	req, _, clarify, err = dispatcher.PrepareDoTask([]byte(`{"task":"ask finance to check NVDA","agent":"finance"}`), "en", false)
	if err != nil || clarify != nil || req.Agent != "finance" {
		t.Fatalf("explicitly named override was not honored: req=%+v clarify=%+v err=%v", req, clarify, err)
	}

	t.Setenv("KOE_AGENT_OVERRIDE_GUARD", "0")
	dispatcher = newDispatcher()
	req, _, _, _ = dispatcher.PrepareDoTask([]byte(`{"task":"check Tokyo weather","agent":"finance"}`), "en", false)
	if req.Agent != "finance" {
		t.Fatalf("rollback flag did not restore model override: %+v", req)
	}
}
