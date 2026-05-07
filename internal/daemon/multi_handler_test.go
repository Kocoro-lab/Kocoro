package daemon

import (
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
)

// spyHandler counts each callback. Used by all multiHandler tests.
type spyHandler struct {
	toolCalls      int
	toolResults    int
	text           int
	preamble       int
	streamDelta    int
	approvalCalls  int
	approvalReturn bool // what OnApprovalNeeded returns
	usage          int
	cloudAgent     int
	cloudProgress  int
	cloudPlan      int
}

func (s *spyHandler) OnToolCall(name, args string) { s.toolCalls++ }
func (s *spyHandler) OnToolResult(name, args string, r agent.ToolResult, e time.Duration) {
	s.toolResults++
}
func (s *spyHandler) OnText(t string)        { s.text++ }
func (s *spyHandler) OnPreamble(t string)    { s.preamble++ }
func (s *spyHandler) OnStreamDelta(d string) { s.streamDelta++ }
func (s *spyHandler) OnApprovalNeeded(t, a string) bool {
	s.approvalCalls++
	return s.approvalReturn
}
func (s *spyHandler) OnUsage(u agent.TurnUsage)                        {}
func (s *spyHandler) OnCloudAgent(id, status, msg string)              { s.cloudAgent++ }
func (s *spyHandler) OnCloudProgress(done, total int)                  { s.cloudProgress++ }
func (s *spyHandler) OnCloudPlan(pt, content string, needsReview bool) { s.cloudPlan++ }

// usageSpy wraps spyHandler with a real OnUsage that increments the counter.
type usageSpy struct {
	spyHandler
}

func (u *usageSpy) OnUsage(turn agent.TurnUsage) { u.usage++ }

func TestMultiHandlerFansOutBaseMethods(t *testing.T) {
	a, b := &usageSpy{}, &usageSpy{}
	m := &multiHandler{handlers: []agent.EventHandler{a, b}}

	m.OnToolCall("bash", "ls")
	m.OnToolResult("bash", "ls", agent.ToolResult{}, 0)
	m.OnText("hi")
	m.OnStreamDelta("d")
	m.OnUsage(agent.TurnUsage{})
	m.OnCloudAgent("id", "running", "msg")
	m.OnCloudProgress(1, 2)
	m.OnCloudPlan("research", "content", false)

	for _, s := range []*usageSpy{a, b} {
		if s.toolCalls != 1 || s.toolResults != 1 || s.text != 1 || s.streamDelta != 1 ||
			s.usage != 1 || s.cloudAgent != 1 || s.cloudProgress != 1 || s.cloudPlan != 1 {
			t.Fatalf("spy counts off: %+v", s.spyHandler)
		}
	}
}

func TestMultiHandlerApprovalORsResults(t *testing.T) {
	a := &usageSpy{}
	a.approvalReturn = false
	b := &usageSpy{}
	b.approvalReturn = true
	m := &multiHandler{handlers: []agent.EventHandler{a, b}}

	if !m.OnApprovalNeeded("bash", "rm -rf /") {
		t.Fatal("want OR=true when any handler returns true")
	}
	if a.approvalCalls != 1 || b.approvalCalls != 1 {
		t.Fatalf("approval calls: a=%d b=%d, want 1 each", a.approvalCalls, b.approvalCalls)
	}

	// When all return false, result is false.
	c := &usageSpy{}
	c.approvalReturn = false
	d := &usageSpy{}
	d.approvalReturn = false
	m2 := &multiHandler{handlers: []agent.EventHandler{c, d}}
	if m2.OnApprovalNeeded("bash", "ls") {
		t.Fatal("want false when all handlers return false")
	}
}

// Confirms multiHandler can be assigned to agent.EventHandler — both the loop
// and tests pass it via that interface. A compile-time check keeps signature
// drift loud.
func TestMultiHandlerSatisfiesEventHandlerInterface(t *testing.T) {
	var _ agent.EventHandler = (*multiHandler)(nil)
}

// sessIDSpy implements agent.EventHandler (via embedded spyHandler) AND SetSessionID.
// Used to verify multiHandler.SetSessionID propagates via type assertion.
type sessIDSpy struct {
	usageSpy
	receivedID string
}

func (s *sessIDSpy) SetSessionID(id string) { s.receivedID = id }

// plainSpy does NOT implement SetSessionID — used to verify the type assertion
// skips handlers cleanly rather than panicking.
type plainSpy struct {
	usageSpy
}

func TestMultiHandlerSetSessionIDPropagatesToImplementers(t *testing.T) {
	setter := &sessIDSpy{}
	plain := &plainSpy{}
	m := &multiHandler{handlers: []agent.EventHandler{setter, plain}}

	m.SetSessionID("sess_abc")

	if setter.receivedID != "sess_abc" {
		t.Fatalf("setter.receivedID = %q, want sess_abc", setter.receivedID)
	}
	// plain has no SetSessionID; surviving the call without panic is the assertion.
	// Also verify the plain spy still receives base callbacks — the SetSessionID
	// bypass must not break the normal fan-out.
	m.OnText("x")
	if plain.text != 1 {
		t.Fatalf("plain.text = %d, want 1 — SetSessionID bypass must not break fan-out", plain.text)
	}
}

// RunAgent type-asserts the top-level handler against the optional SetSessionID
// interface. multiHandler must itself satisfy that optional interface so the
// RunAgent assertion succeeds when the injected handler is a *multiHandler.
func TestMultiHandlerItselfImplementsSetSessionID(t *testing.T) {
	m := &multiHandler{}
	_, ok := interface{}(m).(interface{ SetSessionID(string) })
	if !ok {
		t.Fatal("multiHandler does not satisfy SetSessionID(string) interface")
	}
}

// runStatusSpy implements agent.RunStatusHandler (and agent.EventHandler via the
// embedded usageSpy). Used to verify multiHandler.OnRunStatus propagates via
// type assertion.
type runStatusSpy struct {
	usageSpy
	calls []string // "code:detail"
}

func (s *runStatusSpy) OnRunStatus(code, detail string) {
	s.calls = append(s.calls, code+":"+detail)
}

func TestMultiHandlerOnRunStatusPropagatesToImplementers(t *testing.T) {
	rsh := &runStatusSpy{}
	plain := &plainSpy{}
	m := &multiHandler{handlers: []agent.EventHandler{rsh, plain}}

	m.OnRunStatus("idle_soft", "15s idle")

	if len(rsh.calls) != 1 || rsh.calls[0] != "idle_soft:15s idle" {
		t.Fatalf("rsh.calls = %+v", rsh.calls)
	}
	// plain has no OnRunStatus — call must not panic and not affect rsh.
}

// Verify multiHandler satisfies agent.RunStatusHandler. The watchdog in the
// agent loop does `a.handler.(RunStatusHandler)` type assertion at
// runner.go:917-935; with multiHandler in the chain, the assertion must
// succeed so the watchdog can emit soft/hard idle events.
func TestMultiHandlerSatisfiesRunStatusHandlerInterface(t *testing.T) {
	var _ agent.RunStatusHandler = (*multiHandler)(nil) // compile-time check
}
