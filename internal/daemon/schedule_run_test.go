package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/schedule"
)

// drainScheduleRun collects every schedule_run event currently in the bus's
// ring buffer and returns parsed payloads in arrival order.
func drainScheduleRun(t *testing.T, bus *EventBus) []map[string]any {
	t.Helper()
	out := []map[string]any{}
	for _, evt := range bus.EventsSince(0) {
		if evt.Type != EventScheduleRun {
			continue
		}
		var p map[string]any
		if err := json.Unmarshal(evt.Payload, &p); err != nil {
			t.Fatalf("unmarshal schedule_run payload: %v", err)
		}
		out = append(out, p)
	}
	return out
}

// Test 5: happy path emits started then succeeded with the terminal session_id.
func TestScheduleRun_HappyPath(t *testing.T) {
	bus := NewEventBus()
	deps := &ServerDeps{EventBus: bus}
	s := &Scheduler{deps: deps}
	sched := schedule.Schedule{ID: "sch-1", Agent: "bot"}

	s.runWithLifecycle(sched, func() (*RunAgentResult, error) {
		return &RunAgentResult{SessionID: "sess-99"}, nil
	})

	events := drainScheduleRun(t, bus)
	if len(events) != 2 {
		t.Fatalf("expected 2 events (started, succeeded), got %d", len(events))
	}
	if events[0]["phase"] != "started" {
		t.Errorf("event[0].phase: got %v, want started", events[0]["phase"])
	}
	if events[0]["schedule_id"] != "sch-1" {
		t.Errorf("event[0].schedule_id: got %v, want sch-1", events[0]["schedule_id"])
	}
	if events[0]["session_id"] != "" {
		t.Errorf("event[0].session_id must be empty for started, got %v", events[0]["session_id"])
	}
	if events[1]["phase"] != "succeeded" {
		t.Errorf("event[1].phase: got %v, want succeeded", events[1]["phase"])
	}
	if events[1]["session_id"] != "sess-99" {
		t.Errorf("event[1].session_id: got %v, want sess-99", events[1]["session_id"])
	}
	if events[1]["agent"] != "bot" {
		t.Errorf("event[1].agent: got %v, want bot", events[1]["agent"])
	}
	if _, ok := events[1]["error"]; ok {
		t.Errorf("succeeded events must not carry an error field, got %v", events[1]["error"])
	}
}

// Test 6: RunAgent error must surface as a failed event with a redacted error.
func TestScheduleRun_Failure(t *testing.T) {
	bus := NewEventBus()
	deps := &ServerDeps{EventBus: bus}
	s := &Scheduler{deps: deps}
	sched := schedule.Schedule{ID: "sch-2", Agent: "bot"}

	s.runWithLifecycle(sched, func() (*RunAgentResult, error) {
		return nil, errors.New("agent boom")
	})

	events := drainScheduleRun(t, bus)
	if len(events) != 2 {
		t.Fatalf("expected 2 events (started, failed), got %d", len(events))
	}
	if events[0]["phase"] != "started" || events[1]["phase"] != "failed" {
		t.Fatalf("phases: %v, %v", events[0]["phase"], events[1]["phase"])
	}
	gotErr, _ := events[1]["error"].(string)
	if !strings.Contains(gotErr, "agent boom") {
		t.Errorf("error: got %q, want substring 'agent boom'", gotErr)
	}
	if events[1]["session_id"] != "" {
		t.Errorf("failure before session resolution must carry empty session_id, got %v", events[1]["session_id"])
	}
}

// Test 6b: failure with a populated result must carry the session_id so
// Desktop can click-through into the partially-progressed session.
func TestScheduleRun_FailureWithSession(t *testing.T) {
	bus := NewEventBus()
	deps := &ServerDeps{EventBus: bus}
	s := &Scheduler{deps: deps}
	sched := schedule.Schedule{ID: "sch-3", Agent: ""}

	s.runWithLifecycle(sched, func() (*RunAgentResult, error) {
		return &RunAgentResult{SessionID: "sess-mid"}, errors.New("midway failure")
	})

	events := drainScheduleRun(t, bus)
	if len(events) != 2 || events[1]["phase"] != "failed" {
		t.Fatalf("unexpected events: %v", events)
	}
	if events[1]["session_id"] != "sess-mid" {
		t.Errorf("failed event must carry the result's session_id; got %v", events[1]["session_id"])
	}
	if events[1]["agent"] != "" {
		t.Errorf("default-agent runs must report agent='' on the event, got %v", events[1]["agent"])
	}
}

// Test 7: a panic inside RunAgent must surface as a failed event; the goroutine
// must return cleanly (the runWithLifecycle helper recovers).
func TestScheduleRun_Panic(t *testing.T) {
	bus := NewEventBus()
	deps := &ServerDeps{EventBus: bus}
	s := &Scheduler{deps: deps}
	sched := schedule.Schedule{ID: "sch-4", Agent: "bot"}

	s.runWithLifecycle(sched, func() (*RunAgentResult, error) {
		panic("forced panic")
	})

	events := drainScheduleRun(t, bus)
	if len(events) != 2 {
		t.Fatalf("expected 2 events (started, failed), got %d", len(events))
	}
	if events[1]["phase"] != "failed" {
		t.Errorf("phase: got %v, want failed", events[1]["phase"])
	}
	gotErr, _ := events[1]["error"].(string)
	if !strings.Contains(gotErr, "panic") {
		t.Errorf("panic error must mention 'panic', got %q", gotErr)
	}
}

// emitScheduleRun without a bus must be a silent no-op so test fixtures
// passing nil deps continue to work.
func TestEmitScheduleRun_NilBusSafe(t *testing.T) {
	// nil deps
	(&Scheduler{}).emitScheduleRun("started", schedule.Schedule{ID: "x"}, "", nil)
	// nil bus
	(&Scheduler{deps: &ServerDeps{}}).emitScheduleRun("started", schedule.Schedule{ID: "x"}, "", nil)
}

// Schedule events must survive ring-buffer replay alongside approval events.
func TestScheduleRun_RingReplay(t *testing.T) {
	bus := NewEventBus()
	deps := &ServerDeps{EventBus: bus}
	s := &Scheduler{deps: deps}
	sched := schedule.Schedule{ID: "sch-replay", Agent: "bot"}

	s.runWithLifecycle(sched, func() (*RunAgentResult, error) {
		return &RunAgentResult{SessionID: "sess-r"}, nil
	})

	// Subscribe AFTER emission; SubscribeWithReplay(0) must hand back both events.
	missed, ch := bus.SubscribeWithReplay(0)
	defer bus.Unsubscribe(ch)
	if len(missed) != 2 {
		t.Fatalf("expected 2 replayed events, got %d", len(missed))
	}
	var lastID uint64
	for _, evt := range missed {
		if evt.ID <= lastID {
			t.Fatalf("ring event IDs must be monotonic; got %d after %d", evt.ID, lastID)
		}
		lastID = evt.ID
	}
}

// --- Task 8: usage block on schedule_run succeeded events ------------------

func TestEmitScheduleRun_SucceededIncludesUsage(t *testing.T) {
	bus := NewEventBus()
	deps := &ServerDeps{EventBus: bus}
	s := &Scheduler{deps: deps}
	sched := schedule.Schedule{ID: "s1", Agent: "a"}

	// Cost uses 0.5 — IEEE-754 exact, no float-equality flakiness after JSON round-trip.
	s.emitScheduleRunWithUsage("succeeded", sched, "sess-1", nil, ScheduleRunUsage{
		InputTokens: 1234, OutputTokens: 56, TotalTokens: 1290, CostUSD: 0.5,
	})

	events := drainScheduleRun(t, bus)
	if len(events) != 1 {
		t.Fatalf("want 1 schedule_run event, got %d", len(events))
	}
	usage, ok := events[0]["usage"].(map[string]any)
	if !ok {
		t.Fatalf("schedule_run.usage missing or wrong type: %+v", events[0])
	}
	if int(usage["input_tokens"].(float64)) != 1234 {
		t.Errorf("input_tokens want 1234, got %v", usage["input_tokens"])
	}
	if int(usage["output_tokens"].(float64)) != 56 {
		t.Errorf("output_tokens want 56, got %v", usage["output_tokens"])
	}
	if usage["cost_usd"].(float64) != 0.5 {
		t.Errorf("cost_usd want 0.5, got %v", usage["cost_usd"])
	}
}

func TestEmitScheduleRun_FailedOmitsUsage(t *testing.T) {
	bus := NewEventBus()
	deps := &ServerDeps{EventBus: bus}
	s := &Scheduler{deps: deps}
	sched := schedule.Schedule{ID: "s1"}

	// Failed run before any usage is collected — usage should be omitted, not zero-filled.
	s.emitScheduleRunWithUsage("failed", sched, "", fmt.Errorf("boom"), ScheduleRunUsage{})

	events := drainScheduleRun(t, bus)
	if len(events) != 1 {
		t.Fatalf("want 1 event, got %d", len(events))
	}
	if _, present := events[0]["usage"]; present {
		t.Errorf("failed events with zero usage must omit the usage key, got %+v", events[0])
	}
	if events[0]["error"] != "boom" {
		t.Errorf("error field missing/wrong: %v", events[0]["error"])
	}
}
