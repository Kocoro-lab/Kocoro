package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// readApprovalEvents drains both approval_request and approval_resolved events
// emitted by the bus. Returns parsed maps in arrival order. Tests use it to
// inspect payload field values without coupling to specific event ordering.
func readApprovalEvents(t *testing.T, bus *EventBus, want int, timeout time.Duration) []approvalEvent {
	t.Helper()
	ch := bus.Subscribe()
	defer bus.Unsubscribe(ch)
	got := make([]approvalEvent, 0, want)
	deadline := time.After(timeout)
	for len(got) < want {
		select {
		case evt := <-ch:
			if evt.Type != EventApprovalRequest && evt.Type != EventApprovalResolved {
				continue
			}
			var payload map[string]any
			if err := json.Unmarshal(evt.Payload, &payload); err != nil {
				t.Fatalf("unmarshal %s payload: %v", evt.Type, err)
			}
			got = append(got, approvalEvent{typ: evt.Type, payload: payload})
		case <-deadline:
			t.Fatalf("timeout waiting for %d approval events; got %d", want, len(got))
		}
	}
	return got
}

type approvalEvent struct {
	typ     string
	payload map[string]any
}

// Test 1: approval_request must carry the full payload Desktop needs to
// render an inbox card.
func TestApprovalRequest_FullPayload(t *testing.T) {
	bus := NewEventBus()
	broker := NewApprovalBroker(func(req ApprovalRequest) error { return nil })
	WireApprovalBusHooks(broker, bus, nil)

	ch := bus.Subscribe()
	defer bus.Unsubscribe(ch)

	go func() {
		time.Sleep(40 * time.Millisecond)
		// Resolve via the same path Desktop /approval uses so the test
		// exercises the broker→bus loop end to end.
		broker.mu.Lock()
		var reqID string
		for id := range broker.pending {
			reqID = id
			break
		}
		broker.mu.Unlock()
		broker.Resolve(reqID, DecisionAllow, nil)
	}()

	meta := ApprovalRequestMeta{
		MessageID: "msg-1",
		SessionID: "sess-1",
		Source:    "slack",
		Channel:   "C123",
		ThreadID:  "T456",
		Agent:     "bot",
	}
	args := `{"command":"git status","description":"check repo state"}`
	if d := broker.Request(context.Background(), meta, "bash", args); d != DecisionAllow {
		t.Fatalf("expected allow, got %s", d)
	}

	select {
	case evt := <-ch:
		if evt.Type != EventApprovalRequest {
			t.Fatalf("expected first event %s, got %s", EventApprovalRequest, evt.Type)
		}
		var payload map[string]any
		if err := json.Unmarshal(evt.Payload, &payload); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		for _, field := range []string{"request_id", "session_id", "agent", "tool", "title", "source", "channel", "args", "ts"} {
			if _, ok := payload[field]; !ok {
				t.Errorf("missing field %q in approval_request payload", field)
			}
		}
		if payload["session_id"] != "sess-1" {
			t.Errorf("session_id: got %v, want sess-1", payload["session_id"])
		}
		if payload["source"] != "slack" {
			t.Errorf("source: got %v, want slack", payload["source"])
		}
		if payload["channel"] != "C123" {
			t.Errorf("channel: got %v, want C123", payload["channel"])
		}
		if payload["agent"] != "bot" {
			t.Errorf("agent: got %v, want bot", payload["agent"])
		}
		if payload["tool"] != "bash" {
			t.Errorf("tool: got %v, want bash", payload["tool"])
		}
		// Title comes from args.description.
		if payload["title"] != "check repo state" {
			t.Errorf("title: got %v, want %q", payload["title"], "check repo state")
		}
	case <-time.After(time.Second):
		t.Fatal("approval_request event never arrived")
	}
}

// approvalTitle falls back to tool name when args has no usable description.
func TestApprovalTitleFallback(t *testing.T) {
	cases := []struct {
		name, tool, args, want string
	}{
		{"empty args", "bash", "", "bash"},
		{"non-json args", "bash", "git status", "bash"},
		{"empty description", "bash", `{"command":"ls","description":""}`, "bash"},
		{"whitespace description", "bash", `{"description":"   "}`, "bash"},
		{"populated description", "bash", `{"description":"list directory"}`, "list directory"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := approvalTitle(tc.tool, tc.args); got != tc.want {
				t.Errorf("approvalTitle(%q, %q) = %q, want %q", tc.tool, tc.args, got, tc.want)
			}
		})
	}
}

// Approval request must NOT emit if sendFn fails; otherwise an inbox card
// would appear for a request that never reached the foreground client.
func TestApprovalRequest_NotEmittedOnSendFailure(t *testing.T) {
	bus := NewEventBus()
	broker := NewApprovalBroker(func(req ApprovalRequest) error { return context.DeadlineExceeded })
	WireApprovalBusHooks(broker, bus, nil)

	if d := broker.Request(context.Background(), ApprovalRequestMeta{}, "bash", `{}`); d != DecisionDeny {
		t.Fatalf("expected deny on send failure, got %s", d)
	}
	if events := bus.EventsSince(0); len(events) != 0 {
		t.Fatalf("expected 0 bus events when sendFn fails, got %d (types: %v)", len(events), eventTypes(events))
	}
}

// Test 2: POST /approval emits approval_resolved with resolved_by=kocoro+ts.
func TestApprovalResolved_LocalPath(t *testing.T) {
	srv := NewServer(0, nil, nil, "test")
	// The pending request must be registered or handleApproval falls back to
	// a no-op resolve. Register a synthetic pending entry; the inherited
	// onCleanup is a no-op for resolved (vs cancelled) entries.
	pa := &pendingApproval{ch: make(chan ApprovalDecision, 1)}
	srv.approvalBroker.mu.Lock()
	srv.approvalBroker.pending["apr_test1"] = pa
	srv.approvalBroker.mu.Unlock()

	body := bytes.NewBufferString(`{"request_id":"apr_test1","decision":"allow"}`)
	req := httptest.NewRequest(http.MethodPost, "/approval", body)
	w := httptest.NewRecorder()

	bus := srv.eventBus
	ch := bus.Subscribe()
	defer bus.Unsubscribe(ch)

	srv.handleApproval(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", w.Code)
	}

	select {
	case evt := <-ch:
		if evt.Type != EventApprovalResolved {
			t.Fatalf("expected %s, got %s", EventApprovalResolved, evt.Type)
		}
		var payload map[string]any
		if err := json.Unmarshal(evt.Payload, &payload); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if payload["request_id"] != "apr_test1" {
			t.Errorf("request_id: got %v", payload["request_id"])
		}
		if payload["decision"] != "allow" {
			t.Errorf("decision: got %v", payload["decision"])
		}
		if payload["resolved_by"] != "kocoro" {
			t.Errorf("resolved_by: got %v, want kocoro", payload["resolved_by"])
		}
		if ts, ok := payload["ts"].(string); !ok || ts == "" {
			t.Errorf("ts: missing or empty (%v)", payload["ts"])
		}
	case <-time.After(time.Second):
		t.Fatal("approval_resolved event never arrived")
	}
}

// Test 4: approval timeout/cancel must emit a cleanup approval_resolved so
// reconnecting Desktop clients dismiss the stale card.
func TestApprovalCleanup_OnContextCancel(t *testing.T) {
	bus := NewEventBus()
	broker := NewApprovalBroker(func(req ApprovalRequest) error { return nil })
	WireApprovalBusHooks(broker, bus, nil)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(40 * time.Millisecond)
		cancel()
	}()

	if d := broker.Request(ctx, ApprovalRequestMeta{Channel: "ch1"}, "bash", `{"command":"ls"}`); d != DecisionDeny {
		t.Fatalf("expected deny on ctx cancel, got %s", d)
	}

	// Expect: approval_request (post-send), then cleanup approval_resolved.
	all := bus.EventsSince(0)
	if got := eventTypes(all); !sliceEqual(got, []string{EventApprovalRequest, EventApprovalResolved}) {
		t.Fatalf("expected events [request, resolved], got %v", got)
	}
	var resolved map[string]any
	if err := json.Unmarshal(all[1].Payload, &resolved); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resolved["decision"] != "deny" {
		t.Errorf("cleanup decision: got %v, want deny", resolved["decision"])
	}
	if resolved["resolved_by"] != "daemon" {
		t.Errorf("cleanup resolved_by: got %v, want daemon", resolved["resolved_by"])
	}
	if ts, ok := resolved["ts"].(string); !ok || ts == "" {
		t.Errorf("cleanup ts: missing or empty (%v)", resolved["ts"])
	}
}

// CancelAll must emit a cleanup approval_resolved for every pending request
// whose approval_request was already published. Used on WS disconnect.
func TestApprovalCleanup_CancelAll(t *testing.T) {
	bus := NewEventBus()
	broker := NewApprovalBroker(func(req ApprovalRequest) error { return nil })
	WireApprovalBusHooks(broker, bus, nil)

	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			broker.Request(context.Background(), ApprovalRequestMeta{}, "bash", `{}`)
		}()
	}
	// Let the requests register + emit approval_request.
	time.Sleep(100 * time.Millisecond)
	broker.CancelAll()
	wg.Wait()

	resolvedCount := 0
	requestCount := 0
	for _, evt := range bus.EventsSince(0) {
		switch evt.Type {
		case EventApprovalRequest:
			requestCount++
		case EventApprovalResolved:
			var p map[string]any
			_ = json.Unmarshal(evt.Payload, &p)
			if p["resolved_by"] == "daemon" && p["decision"] == "deny" {
				resolvedCount++
			}
		}
	}
	if requestCount != 3 {
		t.Errorf("approval_request count: got %d, want 3", requestCount)
	}
	if resolvedCount != 3 {
		t.Errorf("daemon-cleanup approval_resolved count: got %d, want 3", resolvedCount)
	}
}

// Daemon-cleanup paths (ctx cancel + CancelAll) must notify Cloud so the
// gateway clears the channel approval card, in addition to the local bus emit.
// Notify runs async, so collect payloads over a buffered channel.
func TestApprovalCleanup_NotifiesCloud(t *testing.T) {
	t.Run("ctx cancel", func(t *testing.T) {
		bus := NewEventBus()
		got := make(chan ApprovalResolvedPayload, 1)
		notify := func(p ApprovalResolvedPayload) error { got <- p; return nil }
		broker := NewApprovalBroker(func(req ApprovalRequest) error { return nil })
		WireApprovalBusHooks(broker, bus, notify)

		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			time.Sleep(40 * time.Millisecond)
			cancel()
		}()
		if d := broker.Request(ctx, ApprovalRequestMeta{Channel: "feishu"}, "publish_to_web", `{}`); d != DecisionDeny {
			t.Fatalf("expected deny on ctx cancel, got %s", d)
		}

		select {
		case p := <-got:
			if p.Decision != DecisionDeny || p.ResolvedBy != "daemon" {
				t.Errorf("cloud notify payload = %+v, want deny/daemon", p)
			}
		case <-time.After(time.Second):
			t.Fatal("cloud notifier never fired on ctx cancel")
		}
	})

	t.Run("CancelAll", func(t *testing.T) {
		bus := NewEventBus()
		got := make(chan ApprovalResolvedPayload, 3)
		notify := func(p ApprovalResolvedPayload) error { got <- p; return nil }
		broker := NewApprovalBroker(func(req ApprovalRequest) error { return nil })
		WireApprovalBusHooks(broker, bus, notify)

		var wg sync.WaitGroup
		for i := 0; i < 3; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				broker.Request(context.Background(), ApprovalRequestMeta{}, "bash", `{}`)
			}()
		}
		// Wait until all 3 entries reach pa.emitted before CancelAll: it only
		// fires onCleanup (and thus notify) for emitted entries, so a fixed
		// sleep could race a slow goroutine still between sendFn and the emit
		// critical section, dropping the count below 3 on a loaded CI box.
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			broker.mu.Lock()
			emitted := 0
			for _, pa := range broker.pending {
				if pa.emitted {
					emitted++
				}
			}
			broker.mu.Unlock()
			if emitted == 3 {
				break
			}
			time.Sleep(time.Millisecond)
		}
		broker.CancelAll()
		wg.Wait()

		collectDeadline := time.After(time.Second)
		for i := 0; i < 3; i++ {
			select {
			case p := <-got:
				if p.Decision != DecisionDeny || p.ResolvedBy != "daemon" {
					t.Errorf("cloud notify payload = %+v, want deny/daemon", p)
				}
			case <-collectDeadline:
				t.Fatalf("cloud notifier fired only %d/3 times on CancelAll", i)
			}
		}
	})
}

// Test 3: Cloud-relayed approval_response must emit approval_resolved with
// the Cloud-provided resolved_by (or "external" when blank) plus ts.
func TestApprovalResolved_CloudPath(t *testing.T) {
	bus := NewEventBus()
	broker := NewApprovalBroker(func(req ApprovalRequest) error { return nil })
	c := &Client{}
	c.SetEventBus(bus)
	c.SetApprovalBroker(broker)

	// Register a pending so the broker treats Resolve as legitimate.
	pa := &pendingApproval{ch: make(chan ApprovalDecision, 1)}
	broker.mu.Lock()
	broker.pending["apr_cloud1"] = pa
	broker.mu.Unlock()

	// Build the WS-side ApprovalResponse payload Cloud sends; reuse the same
	// dispatch the Listen loop runs (encoded as ServerMessage envelope so the
	// test exercises the production path without spinning up websocket).
	respPayload, _ := json.Marshal(ApprovalResponse{
		RequestID:  "apr_cloud1",
		Decision:   DecisionAllow,
		ResolvedBy: "slack",
	})
	emitApprovalResponse(c, ServerMessage{Type: MsgTypeApprovalResponse, Payload: respPayload})

	all := bus.EventsSince(0)
	if len(all) != 1 || all[0].Type != EventApprovalResolved {
		t.Fatalf("expected one approval_resolved event, got %v", eventTypes(all))
	}
	var payload map[string]any
	_ = json.Unmarshal(all[0].Payload, &payload)
	if payload["resolved_by"] != "slack" {
		t.Errorf("resolved_by: got %v, want slack", payload["resolved_by"])
	}
	if ts, ok := payload["ts"].(string); !ok || ts == "" {
		t.Errorf("ts: missing or empty (%v)", payload["ts"])
	}

	// Blank ResolvedBy falls back to "external".
	pa2 := &pendingApproval{ch: make(chan ApprovalDecision, 1)}
	broker.mu.Lock()
	broker.pending["apr_cloud2"] = pa2
	broker.mu.Unlock()
	respPayload2, _ := json.Marshal(ApprovalResponse{RequestID: "apr_cloud2", Decision: DecisionDeny})
	emitApprovalResponse(c, ServerMessage{Type: MsgTypeApprovalResponse, Payload: respPayload2})

	all = bus.EventsSince(0)
	last := all[len(all)-1]
	var p2 map[string]any
	_ = json.Unmarshal(last.Payload, &p2)
	if p2["resolved_by"] != "external" {
		t.Errorf("blank Cloud resolved_by must default to 'external', got %v", p2["resolved_by"])
	}
}

// P1 regression: a CancelAll racing with sendFn-success must never leave an
// orphan approval_request on the bus. Either both events appear (CancelAll
// won the broker mutex after the emit critical section, fires cleanup) or
// neither does (CancelAll deleted pa before the emit critical section, so
// onRequest is skipped). Anything else is a stale inbox card.
func TestApprovalRequest_CancelAllDuringEmitNoOrphan(t *testing.T) {
	// Use a sendFn we can block to force the race window deterministically:
	// CancelAll fires while sendFn is in-flight, well before the emit
	// critical section runs. Without the broker-mutex fix, this scenario
	// surfaces an approval_request with no matching approval_resolved.
	sendStarted := make(chan struct{})
	sendDone := make(chan struct{})
	sendFn := func(req ApprovalRequest) error {
		close(sendStarted)
		<-sendDone
		return nil
	}
	bus := NewEventBus()
	broker := NewApprovalBroker(sendFn)
	WireApprovalBusHooks(broker, bus, nil)

	resultCh := make(chan ApprovalDecision, 1)
	go func() {
		resultCh <- broker.Request(context.Background(), ApprovalRequestMeta{Source: "slack"}, "bash", `{"command":"ls"}`)
	}()

	<-sendStarted
	// CancelAll runs while sendFn is suspended; pa.emitted is still false.
	broker.CancelAll()
	close(sendDone)

	select {
	case d := <-resultCh:
		if d != DecisionDeny {
			t.Fatalf("expected DecisionDeny on CancelAll race, got %s", d)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Request goroutine never returned after CancelAll")
	}

	var requests, resolves int
	for _, evt := range bus.EventsSince(0) {
		switch evt.Type {
		case EventApprovalRequest:
			requests++
		case EventApprovalResolved:
			resolves++
		}
	}
	if requests != resolves {
		t.Fatalf("orphan card leak: %d approval_request vs %d approval_resolved", requests, resolves)
	}
}

// P2 regression: title is parsed from args.description (model-controlled)
// and must run through redactAndTruncate on the bus copy so a long string or
// embedded secret cannot bypass the ring-buffer cap that args goes through.
func TestApprovalRequest_TitleRedactedAndTruncated(t *testing.T) {
	bus := NewEventBus()
	broker := NewApprovalBroker(func(req ApprovalRequest) error { return nil })
	WireApprovalBusHooks(broker, bus, nil)

	long := strings.Repeat("X", approvalRequestTitleCap+50)
	desc := "leaking AKIAIOSFODNN7EXAMPLE then " + long
	args := `{"description":` + strconv.Quote(desc) + `}`

	go func() {
		time.Sleep(30 * time.Millisecond)
		broker.mu.Lock()
		var id string
		for k := range broker.pending {
			id = k
			break
		}
		broker.mu.Unlock()
		broker.Resolve(id, DecisionAllow, nil)
	}()
	_ = broker.Request(context.Background(), ApprovalRequestMeta{}, "bash", args)

	var payload map[string]any
	for _, evt := range bus.EventsSince(0) {
		if evt.Type == EventApprovalRequest {
			_ = json.Unmarshal(evt.Payload, &payload)
			break
		}
	}
	if payload == nil {
		t.Fatal("approval_request never reached bus")
	}
	title, _ := payload["title"].(string)
	if len(title) > approvalRequestTitleCap {
		t.Errorf("title len = %d, want ≤ %d", len(title), approvalRequestTitleCap)
	}
	if strings.Contains(title, "AKIAIOSFODNN7EXAMPLE") {
		t.Errorf("title leaked AWS key fragment: %q", title)
	}
}

// Test 8: approval_request + approval_resolved must survive ring buffer
// replay so a Desktop client reconnecting after a missed event still sees
// (or correctly omits) the inbox card.
func TestApprovalEvents_RingReplay(t *testing.T) {
	bus := NewEventBus()
	broker := NewApprovalBroker(func(req ApprovalRequest) error { return nil })
	WireApprovalBusHooks(broker, bus, nil)

	go func() {
		time.Sleep(20 * time.Millisecond)
		broker.mu.Lock()
		var id string
		for k := range broker.pending {
			id = k
			break
		}
		broker.mu.Unlock()
		broker.Resolve(id, DecisionAllow, nil)
	}()
	_ = broker.Request(context.Background(), ApprovalRequestMeta{Source: "schedule"}, "bash", `{}`)

	missed, ch := bus.SubscribeWithReplay(0)
	defer bus.Unsubscribe(ch)
	types := make([]string, 0, len(missed))
	for _, e := range missed {
		types = append(types, e.Type)
	}
	// approval_resolved on the Resolve path goes through ingress (not the
	// broker), so only approval_request is in the ring. The Resolve happens
	// silently here because no ingress emitted the resolution event — the
	// invariant is that the ring buffer DOES contain approval_request, with
	// monotonically-increasing IDs and well-formed payload.
	if len(missed) == 0 || missed[0].Type != EventApprovalRequest {
		t.Fatalf("expected ring replay to include approval_request, got %v", types)
	}
	if missed[0].ID == 0 {
		t.Error("ring buffer events must carry non-zero IDs")
	}
}

// P1 regression: ingress (HTTP /approval, WS approval_response) racing with
// a daemon-cleanup path (ctx.Done / timeout / CancelAll) must never produce
// two approval_resolved events for the same request_id. Conflicting terminal
// states (allow/kocoro alongside deny/daemon) leave Desktop unable to tell
// which decision is authoritative.
func TestApprovalEvents_NoConflictingTerminalState(t *testing.T) {
	// Mirror handleApproval's claim-first ordering so the test exercises the
	// same gating production uses.
	sendUserAllow := func(broker *ApprovalBroker, bus *EventBus, reqID string) {
		broker.Resolve(reqID, DecisionAllow, func() {
			emitBusJSON(bus, EventApprovalResolved, map[string]any{
				"request_id":  reqID,
				"decision":    string(DecisionAllow),
				"resolved_by": "kocoro",
				"ts":          nowISO(),
			})
		})
	}

	const iterations = 50
	for i := 0; i < iterations; i++ {
		bus := NewEventBus()
		broker := NewApprovalBroker(func(req ApprovalRequest) error { return nil })
		WireApprovalBusHooks(broker, bus, nil)

		ctx, cancel := context.WithCancel(context.Background())
		reqIDCh := make(chan string, 1)
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = broker.Request(ctx, ApprovalRequestMeta{}, "bash", `{}`)
		}()

		// Wait for the request to register so we can target its reqID.
		go func() {
			for {
				broker.mu.Lock()
				for id := range broker.pending {
					broker.mu.Unlock()
					reqIDCh <- id
					return
				}
				broker.mu.Unlock()
				time.Sleep(100 * time.Microsecond)
			}
		}()
		var reqID string
		select {
		case reqID = <-reqIDCh:
		case <-time.After(2 * time.Second):
			cancel()
			wg.Wait()
			t.Fatalf("iter %d: request did not register in pending", i)
		}

		// Block both racers on a shared start signal so they actually race
		// instead of running in fixed program order.
		start := make(chan struct{})
		var raceWg sync.WaitGroup
		raceWg.Add(2)
		go func() {
			defer raceWg.Done()
			<-start
			sendUserAllow(broker, bus, reqID)
		}()
		go func() {
			defer raceWg.Done()
			<-start
			cancel()
		}()
		close(start)
		raceWg.Wait()
		wg.Wait()

		// Count terminal events per request_id; the invariant is "≤1".
		seen := map[string]int{}
		for _, evt := range bus.EventsSince(0) {
			if evt.Type != EventApprovalResolved {
				continue
			}
			var p map[string]any
			if err := json.Unmarshal(evt.Payload, &p); err != nil {
				t.Fatalf("iter %d: unmarshal: %v", i, err)
			}
			id, _ := p["request_id"].(string)
			seen[id]++
		}
		for id, n := range seen {
			if n > 1 {
				t.Fatalf("iter %d: request_id %s has %d terminal events (want ≤1)", i, id, n)
			}
		}
	}
}

// emitApprovalResponse re-uses Client.Listen's MsgTypeApprovalResponse switch
// arm so tests exercise the production emission path without a websocket.
// Keep in sync with internal/daemon/client.go.
func emitApprovalResponse(c *Client, sm ServerMessage) {
	var resp ApprovalResponse
	if err := json.Unmarshal(sm.Payload, &resp); err != nil {
		return
	}
	resolvedBy := resp.ResolvedBy
	if resolvedBy == "" {
		resolvedBy = "external"
	}
	if c.broker != nil {
		c.broker.Resolve(resp.RequestID, resp.Decision, func() {
			emitBusJSON(c.eventBus, EventApprovalResolved, map[string]any{
				"request_id":  resp.RequestID,
				"decision":    string(resp.Decision),
				"resolved_by": resolvedBy,
				"ts":          nowISO(),
			})
		})
	}
}

// P1 regression: approval_resolved emitted via Resolve's beforeDeliver
// callback must land on the bus with an event ID strictly less than any
// event the Request goroutine — and thus the agent loop — can emit after
// pa.ch wakes it. The buffered channel send inside Resolve is non-blocking
// so the receiver becomes runnable immediately and can resume on another
// P; without serializing the bus emit before the channel send under the
// broker mutex, Desktop subscribers would observe the next tool_status
// before approval_resolved and see the inbox card linger past the moment
// the tool was already running.
//
// The 5 ms pause inside beforeDeliver widens the race window so a broken
// implementation (emit AFTER channel send) loses deterministically, while
// the correct implementation passes deterministically because the channel
// send is guaranteed to happen-after the callback returns.
func TestApprovalResolve_BusEmitBeforeAgentWake(t *testing.T) {
	const postWakeMarker = "test_post_wake_marker"

	bus := NewEventBus()
	broker := NewApprovalBroker(func(req ApprovalRequest) error { return nil })
	WireApprovalBusHooks(broker, bus, nil)

	requestDone := make(chan struct{})
	go func() {
		_ = broker.Request(context.Background(), ApprovalRequestMeta{}, "bash", `{}`)
		// Stand-in for the agent loop's first post-approval event
		// (tool_status running, delta, etc.). The exact type is
		// irrelevant; the invariant is ID(approval_resolved) < ID(marker).
		emitBusJSON(bus, postWakeMarker, map[string]any{})
		close(requestDone)
	}()

	var reqID string
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		broker.mu.Lock()
		for id := range broker.pending {
			reqID = id
			break
		}
		broker.mu.Unlock()
		if reqID != "" {
			break
		}
		time.Sleep(100 * time.Microsecond)
	}
	if reqID == "" {
		t.Fatalf("request never registered in pending")
	}

	// beforeDeliver sleeps to widen the window: a broken implementation
	// that emits AFTER releasing the broker mutex (e.g. reverting to the
	// pre-fix caller-side pattern in handleApproval) gives the Request
	// goroutine's defer-on-mu and subsequent post-wake marker emit a 5 ms
	// head start. With the fix, beforeDeliver runs under the broker mutex
	// before pa.ch is written, and Request.broker.mu.Lock in its defer
	// blocks until the mutex is released — guaranteeing the marker emit
	// is sequenced after approval_resolved.
	if !broker.Resolve(reqID, DecisionAllow, func() {
		time.Sleep(5 * time.Millisecond)
		emitBusJSON(bus, EventApprovalResolved, map[string]any{
			"request_id":  reqID,
			"decision":    string(DecisionAllow),
			"resolved_by": "kocoro",
			"ts":          nowISO(),
		})
	}) {
		t.Fatal("Resolve returned false on a freshly-registered request")
	}

	select {
	case <-requestDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Request goroutine never returned")
	}

	var resolvedID, markerID uint64
	for _, evt := range bus.EventsSince(0) {
		switch evt.Type {
		case EventApprovalResolved:
			resolvedID = evt.ID
		case postWakeMarker:
			markerID = evt.ID
		}
	}
	if resolvedID == 0 {
		t.Fatal("approval_resolved missing from bus")
	}
	if markerID == 0 {
		t.Fatal("post-wake marker missing from bus")
	}
	if resolvedID >= markerID {
		t.Errorf("approval_resolved ID (%d) must precede post-wake marker ID (%d); beforeDeliver must run under broker lock before pa.ch is written", resolvedID, markerID)
	}
}

// P1 regression for CancelAll: cleanup approval_resolved events must land
// on the bus before any post-deny event the Request goroutine — woken by
// CancelAll's pa.ch <- DecisionDeny — can trigger. The same multi-P race
// applies as Resolve: emit must precede the channel send, otherwise
// Desktop sees agent denial events before the card-dismiss event.
func TestApprovalCancelAll_BusEmitBeforeAgentWake(t *testing.T) {
	const postWakeMarker = "test_post_wake_marker_cancelall"

	bus := NewEventBus()
	broker := NewApprovalBroker(func(req ApprovalRequest) error { return nil })
	WireApprovalBusHooks(broker, bus, nil)

	requestDone := make(chan struct{})
	go func() {
		_ = broker.Request(context.Background(), ApprovalRequestMeta{}, "bash", `{}`)
		emitBusJSON(bus, postWakeMarker, map[string]any{})
		close(requestDone)
	}()

	// Wait until pa.emitted is true so CancelAll fires onCleanup for this entry.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		broker.mu.Lock()
		ready := false
		for _, pa := range broker.pending {
			if pa.emitted {
				ready = true
				break
			}
		}
		broker.mu.Unlock()
		if ready {
			break
		}
		time.Sleep(100 * time.Microsecond)
	}

	broker.CancelAll()

	select {
	case <-requestDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Request goroutine never returned after CancelAll")
	}

	var cleanupID, markerID uint64
	for _, evt := range bus.EventsSince(0) {
		switch evt.Type {
		case EventApprovalResolved:
			cleanupID = evt.ID
		case postWakeMarker:
			markerID = evt.ID
		}
	}
	if cleanupID == 0 {
		t.Fatal("cleanup approval_resolved missing from bus")
	}
	if markerID == 0 {
		t.Fatal("post-wake marker missing from bus")
	}
	if cleanupID >= markerID {
		t.Errorf("cleanup approval_resolved ID (%d) must precede post-wake marker ID (%d); CancelAll must emit onCleanup before pa.ch is written", cleanupID, markerID)
	}
}

// P2 regression: when a tool has no flags, the bus approval_request payload
// must OMIT the "flags" key (matching the wire `json:"flags,omitempty"`
// semantics) rather than emit "flags": null. A naive UI client doing
// payload.flags.includes(...) crashes on null; the wire side has been
// correct via omitempty since approval flags were introduced, but the bus
// side passed nil through map[string]any which JSON-encodes as null.
func TestApprovalRequest_FlagsOmittedWhenEmpty(t *testing.T) {
	bus := NewEventBus()
	broker := NewApprovalBroker(func(req ApprovalRequest) error { return nil })
	WireApprovalBusHooks(broker, bus, nil)

	go func() {
		time.Sleep(30 * time.Millisecond)
		broker.mu.Lock()
		var id string
		for k := range broker.pending {
			id = k
			break
		}
		broker.mu.Unlock()
		broker.Resolve(id, DecisionAllow, nil)
	}()
	// bash is not in DisallowsAutoApproval so req.Flags stays nil.
	_ = broker.Request(context.Background(), ApprovalRequestMeta{}, "bash", `{}`)

	var rawPayload []byte
	for _, evt := range bus.EventsSince(0) {
		if evt.Type == EventApprovalRequest {
			rawPayload = evt.Payload
			break
		}
	}
	if rawPayload == nil {
		t.Fatal("approval_request never reached bus")
	}
	if bytes.Contains(rawPayload, []byte(`"flags"`)) {
		t.Errorf("flags must be omitted when empty (not emitted as null); got payload: %s", string(rawPayload))
	}

	// 2026-05-18 update: this symmetric case used to assert publish_to_web
	// emitted a non-empty flags array (with ApprovalFlagAlwaysAllowDisabled).
	// The deny-list is now empty so publish_to_web behaves like any other
	// tool — flags stay omitted. We keep the second probe in place so the
	// "flags omitted everywhere" invariant is exercised end-to-end with a
	// formerly-high-risk tool name, to catch a future regression that
	// emits an unintended flag for it.
	bus2 := NewEventBus()
	broker2 := NewApprovalBroker(func(req ApprovalRequest) error { return nil })
	WireApprovalBusHooks(broker2, bus2, nil)
	go func() {
		time.Sleep(30 * time.Millisecond)
		broker2.mu.Lock()
		var id string
		for k := range broker2.pending {
			id = k
			break
		}
		broker2.mu.Unlock()
		broker2.Resolve(id, DecisionAllow, nil)
	}()
	_ = broker2.Request(context.Background(), ApprovalRequestMeta{}, "publish_to_web", `{}`)

	var rawPayload2 []byte
	for _, evt := range bus2.EventsSince(0) {
		if evt.Type == EventApprovalRequest {
			rawPayload2 = evt.Payload
			break
		}
	}
	if rawPayload2 == nil {
		t.Fatal("approval_request for publish_to_web never reached bus")
	}
	if bytes.Contains(rawPayload2, []byte(`"flags"`)) {
		t.Errorf("publish_to_web is no longer deny-listed; flags must be omitted, got payload: %s", string(rawPayload2))
	}
}

func eventTypes(events []Event) []string {
	out := make([]string, len(events))
	for i, e := range events {
		out[i] = e.Type
	}
	return out
}

func sliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
