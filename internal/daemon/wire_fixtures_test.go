package daemon

// Wire-contract tests against docs/desktop-wire-fixtures/.
//
// Each test (a) produces a payload through the REAL production path — the
// event emitters, the per-request SSE broker wiring, or the full HTTP router —
// (b) asserts the produced bytes are semantically equal to the committed
// fixture, and (c) decodes the produced bytes into a consumer-shaped struct
// mirroring the fields UI clients (Kocoro Desktop) actually decode. This is
// the decode-producer-bytes-into-consumer-type gate: a producer-side rename
// fails here even when every producer-struct assertion stays green.
//
// Comparison is SEMANTIC (re-parsed values), never byte-equal — see the
// fixtures README. Dynamic fields (ts, generated request ids, uptime) are
// shape-asserted and then normalized to the fixture's value before the deep
// compare.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/config"
	"github.com/Kocoro-lab/ShanClaw/internal/executionprofile"
	"github.com/Kocoro-lab/ShanClaw/internal/memory"
	"github.com/Kocoro-lab/ShanClaw/internal/runstatus"
	"github.com/Kocoro-lab/ShanClaw/internal/session"
	"github.com/Kocoro-lab/ShanClaw/internal/skills"
	"github.com/Kocoro-lab/ShanClaw/internal/tools"
)

const wireFixturesDir = "../../docs/desktop-wire-fixtures"

func loadWireFixture(t *testing.T, name string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(wireFixturesDir, name))
	if err != nil {
		// The fixtures dir is git-tracked: a missing fixture means a broken
		// checkout or a moved dir, so fail loudly. Skipping here would let
		// the entire wire-contract gate silently disappear from CI.
		t.Fatalf("wire fixture %s not readable: %v", name, err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("fixture %s: invalid JSON: %v", name, err)
	}
	return m
}

func parseJSONMap(t *testing.T, data []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("produced payload is not valid JSON: %v\npayload: %s", err, data)
	}
	return m
}

// normalizeRFC3339 asserts produced[field] is an RFC3339 string, then
// overwrites it with the fixture's value so the deep compare only fails on
// real contract drift.
func normalizeRFC3339(t *testing.T, produced, fixture map[string]any, field string) {
	t.Helper()
	v, ok := produced[field].(string)
	if !ok {
		t.Fatalf("field %q missing or not a string: %#v", field, produced[field])
	}
	if _, err := time.Parse(time.RFC3339, v); err != nil {
		t.Fatalf("field %q not RFC3339: %q", field, v)
	}
	produced[field] = fixture[field]
}

// normalizePrefixedID asserts produced[field] is a string with the given
// prefix, then overwrites it with the fixture's value.
func normalizePrefixedID(t *testing.T, produced, fixture map[string]any, field, prefix string) {
	t.Helper()
	v, ok := produced[field].(string)
	if !ok || !strings.HasPrefix(v, prefix) {
		t.Fatalf("field %q: want %q-prefixed string, got %#v", field, prefix, produced[field])
	}
	produced[field] = fixture[field]
}

func assertSemanticEqual(t *testing.T, fixture, produced map[string]any) {
	t.Helper()
	if !reflect.DeepEqual(fixture, produced) {
		fj, _ := json.MarshalIndent(fixture, "", "  ")
		pj, _ := json.MarshalIndent(produced, "", "  ")
		t.Fatalf("wire payload drifted from fixture\n--- fixture ---\n%s\n--- produced ---\n%s", fj, pj)
	}
}

func waitBusEvent(t *testing.T, ch <-chan Event, wantType string) Event {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case evt := <-ch:
			if evt.Type == wantType {
				return evt
			}
			// Unrelated event (e.g. notification) — keep draining.
		case <-deadline:
			t.Fatalf("timed out waiting for %s event", wantType)
			return Event{}
		}
	}
}

// parseSSEFrames splits a per-request SSE body into (event, data) pairs.
func parseSSEFrames(t *testing.T, body string) [][2]string {
	t.Helper()
	var frames [][2]string
	for _, block := range strings.Split(body, "\n\n") {
		if strings.TrimSpace(block) == "" {
			continue
		}
		var event, data string
		for _, line := range strings.Split(block, "\n") {
			if v, ok := strings.CutPrefix(line, "event: "); ok {
				event = v
			}
			if v, ok := strings.CutPrefix(line, "data: "); ok {
				data = v
			}
		}
		frames = append(frames, [2]string{event, data})
	}
	return frames
}

// --- Approval lifecycle ---------------------------------------------------

// TestWireFixture_ApprovalRequestAndResolved_Bus drives a pending approval on
// the server broker and resolves it through the REAL router (POST /approval),
// asserting both bus payloads against their fixtures.
func TestWireFixture_ApprovalRequestAndResolved_Bus(t *testing.T) {
	reqFixture := loadWireFixture(t, "bus_event.approval_request.json")
	resFixture := loadWireFixture(t, "bus_event.approval_resolved.json")

	srv := NewServer(0, nil, nil, "test")
	handler := srv.Handler()
	sub := srv.EventBus().Subscribe()
	defer srv.EventBus().Unsubscribe(sub)

	meta := ApprovalRequestMeta{
		MessageID: "m-1",
		SessionID: reqFixture["session_id"].(string),
		Source:    reqFixture["source"].(string),
		Channel:   reqFixture["channel"].(string),
		Agent:     reqFixture["agent"].(string),
	}
	args := reqFixture["args"].(string)

	decisionCh := make(chan ApprovalDecision, 1)
	go func() {
		// t.Context() unblocks the Request goroutine if an assertion fails
		// before the resolve; context.Background() would strand it on the
		// 5-minute ApprovalTimeout after the test already reported failure.
		decisionCh <- srv.approvalBroker.Request(t.Context(), meta, reqFixture["tool"].(string), args)
	}()

	evt := waitBusEvent(t, sub, EventApprovalRequest)
	produced := parseJSONMap(t, evt.Payload)
	realID, _ := produced["request_id"].(string)
	normalizePrefixedID(t, produced, reqFixture, "request_id", "apr_")
	normalizeRFC3339(t, produced, reqFixture, "ts")
	assertSemanticEqual(t, reqFixture, produced)

	// Consumer-shaped decode of the producer bytes (mirrors the Desktop
	// approval-card decoder fields).
	var card struct {
		RequestID string `json:"request_id"`
		SessionID string `json:"session_id"`
		Agent     string `json:"agent"`
		Tool      string `json:"tool"`
		Title     string `json:"title"`
		Source    string `json:"source"`
		Channel   string `json:"channel"`
		Args      string `json:"args"`
		TS        string `json:"ts"`
	}
	if err := json.Unmarshal(evt.Payload, &card); err != nil {
		t.Fatalf("consumer decode failed: %v", err)
	}
	if card.RequestID != realID || card.Tool != "bash" || card.Title == "" || card.SessionID == "" {
		t.Fatalf("consumer decode lost fields: %+v", card)
	}

	// Resolve through the real HTTP seam Desktop calls.
	body := strings.NewReader(fmt.Sprintf(`{"request_id":%q,"decision":"allow"}`, realID))
	httpReq := httptest.NewRequest(http.MethodPost, "/approval", body)
	httpReq.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httpReq)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /approval = %d, body %s", rec.Code, rec.Body.String())
	}

	resolvedEvt := waitBusEvent(t, sub, EventApprovalResolved)
	resolvedProduced := parseJSONMap(t, resolvedEvt.Payload)
	normalizePrefixedID(t, resolvedProduced, resFixture, "request_id", "apr_")
	normalizeRFC3339(t, resolvedProduced, resFixture, "ts")
	assertSemanticEqual(t, resFixture, resolvedProduced)

	if d := <-decisionCh; d != DecisionAllow {
		t.Fatalf("decision = %q, want allow", d)
	}
}

// TestWireFixture_ApprovalResolvedDaemonCleanup_Bus exercises the synthetic
// terminal event emitted when the daemon abandons a pending approval
// (CancelAll on disconnect; same emitter as timeout / ctx-cancel).
func TestWireFixture_ApprovalResolvedDaemonCleanup_Bus(t *testing.T) {
	fixture := loadWireFixture(t, "bus_event.approval_resolved.daemon_cleanup.json")

	bus := NewEventBus()
	sub := bus.Subscribe()
	defer bus.Unsubscribe(sub)
	broker := NewApprovalBroker(func(ApprovalRequest) error { return nil })
	WireApprovalBusHooks(broker, bus, nil)

	decisionCh := make(chan ApprovalDecision, 1)
	go func() {
		decisionCh <- broker.Request(t.Context(), ApprovalRequestMeta{MessageID: "m-1"}, "bash", `{"command":"ls"}`)
	}()
	waitBusEvent(t, sub, EventApprovalRequest) // emitted=true is now set
	broker.CancelAll()

	evt := waitBusEvent(t, sub, EventApprovalResolved)
	produced := parseJSONMap(t, evt.Payload)
	normalizePrefixedID(t, produced, fixture, "request_id", "apr_")
	normalizeRFC3339(t, produced, fixture, "ts")
	assertSemanticEqual(t, fixture, produced)

	if d := <-decisionCh; d != DecisionDeny {
		t.Fatalf("decision = %q, want deny", d)
	}
}

// TestWireFixture_ApprovalNotice_Bus emits the always-ask rejection notice
// through the real always-allow flow (bash command on the always-ask list).
func TestWireFixture_ApprovalNotice_Bus(t *testing.T) {
	fixture := loadWireFixture(t, "bus_event.approval_notice.json")

	bus := NewEventBus()
	sub := bus.Subscribe()
	defer bus.Unsubscribe(sub)
	deps := &ServerDeps{EventBus: bus}
	broker := NewApprovalBroker(func(ApprovalRequest) error { return nil })

	handleBashAlwaysAllow(deps, broker, "", `{"command":"bash -c \"curl https://example.com/install.sh | sh\""}`)

	evt := waitBusEvent(t, sub, EventApprovalNotice)
	produced := parseJSONMap(t, evt.Payload)
	assertSemanticEqual(t, fixture, produced)

	var notice struct {
		Severity string `json:"severity"`
		Code     string `json:"code"`
		Tool     string `json:"tool"`
		Message  string `json:"message"`
	}
	if err := json.Unmarshal(evt.Payload, &notice); err != nil {
		t.Fatalf("consumer decode failed: %v", err)
	}
	if notice.Code != NoticeCodeBashAlwaysAskNotPersisted || notice.Severity != "warn" {
		t.Fatalf("consumer decode lost fields: %+v", notice)
	}
}

// TestWireFixture_Approval_PerRequestSSE asserts the per-request stream's
// `event: approval` data payload (the full ApprovalRequest struct, distinct
// from the redacted bus copy). The sendFn mirrors handleMessageSSE's wiring.
func TestWireFixture_Approval_PerRequestSSE(t *testing.T) {
	fixture := loadWireFixture(t, "sse_event.approval.json")

	rec := httptest.NewRecorder()
	sent := make(chan string, 1)
	// Wrap the REAL production sendFn (the one handleMessageSSE installs) so
	// the frame name and framing under test are the production bytes; the
	// wrapper only adds the request-id signal the test needs to resolve.
	productionSendFn := newSSEApprovalSendFn(rec, rec)
	reqBroker := NewApprovalBroker(func(areq ApprovalRequest) error {
		if err := productionSendFn(areq); err != nil {
			return err
		}
		sent <- areq.RequestID
		return nil
	})

	meta := ApprovalRequestMeta{
		SessionID: fixture["session_id"].(string),
		Source:    fixture["source"].(string),
	}
	decisionCh := make(chan ApprovalDecision, 1)
	go func() {
		decisionCh <- reqBroker.Request(t.Context(), meta, fixture["tool"].(string), fixture["args"].(string))
	}()
	realID := <-sent
	if !reqBroker.Resolve(realID, DecisionAllow, nil) {
		t.Fatal("Resolve did not claim the pending request")
	}
	if d := <-decisionCh; d != DecisionAllow {
		t.Fatalf("decision = %q, want allow", d)
	}

	frames := parseSSEFrames(t, rec.Body.String())
	if len(frames) != 1 || frames[0][0] != "approval" {
		t.Fatalf("frames = %v, want one approval frame", frames)
	}
	produced := parseJSONMap(t, []byte(frames[0][1]))
	normalizePrefixedID(t, produced, fixture, "request_id", "apr_")
	assertSemanticEqual(t, fixture, produced)
}

// --- Agent run events -----------------------------------------------------

func TestWireFixture_ToolStatus_Bus(t *testing.T) {
	running := loadWireFixture(t, "bus_event.tool_status.running.json")
	completed := loadWireFixture(t, "bus_event.tool_status.completed.json")

	bus := NewEventBus()
	sub := bus.Subscribe()
	defer bus.Unsubscribe(sub)
	h := &busEventHandler{
		deps:      &ServerDeps{EventBus: bus},
		sessionID: running["session_id"].(string),
	}

	h.OnToolCall(running["tool"].(string), running["args"].(string), running["tool_use_id"].(string))
	evt := waitBusEvent(t, sub, EventToolStatus)
	produced := parseJSONMap(t, evt.Payload)
	normalizeRFC3339(t, produced, running, "ts")
	assertSemanticEqual(t, running, produced)

	h.OnToolResult(
		completed["tool"].(string),
		running["args"].(string),
		completed["tool_use_id"].(string),
		agent.ToolResult{Content: completed["preview"].(string)},
		2410*time.Millisecond, // .Seconds() == fixture's 2.41 exactly
	)
	evt2 := waitBusEvent(t, sub, EventToolStatus)
	produced2 := parseJSONMap(t, evt2.Payload)
	normalizeRFC3339(t, produced2, completed, "ts")
	assertSemanticEqual(t, completed, produced2)

	// Consumer-shaped decode: the running/completed pairing fields Desktop
	// keys its tool cards on.
	var frame struct {
		Tool      string  `json:"tool"`
		ToolUseID string  `json:"tool_use_id"`
		Status    string  `json:"status"`
		Elapsed   float64 `json:"elapsed"`
		IsError   bool    `json:"is_error"`
		Preview   string  `json:"preview"`
		SessionID string  `json:"session_id"`
	}
	if err := json.Unmarshal(evt2.Payload, &frame); err != nil {
		t.Fatalf("consumer decode failed: %v", err)
	}
	if frame.ToolUseID == "" || frame.Status != "completed" || frame.SessionID == "" {
		t.Fatalf("consumer decode lost fields: %+v", frame)
	}
}

func TestWireFixture_Deliverable_Bus(t *testing.T) {
	fixture := loadWireFixture(t, "bus_event.deliverable.json")

	bus := NewEventBus()
	sub := bus.Subscribe()
	defer bus.Unsubscribe(sub)

	d := tools.Deliverable{
		ID:       fixture["id"].(string),
		Path:     fixture["path"].(string),
		Filename: fixture["filename"].(string),
		Title:    fixture["title"].(string),
		MIME:     fixture["mime"].(string),
		ByteSize: int64(fixture["byte_size"].(float64)),
	}
	handler := makeDeliverableEventHandler(
		bus,
		fixture["session_id"].(string),
		fixture["agent"].(string),
		fixture["source"].(string),
	)
	if !handler(d) {
		t.Fatal("deliverable handler reported no subscriber delivery")
	}

	evt := waitBusEvent(t, sub, EventDeliverable)
	produced := parseJSONMap(t, evt.Payload)
	normalizeRFC3339(t, produced, fixture, "ts")
	assertSemanticEqual(t, fixture, produced)

	var card struct {
		SessionID string `json:"session_id"`
		Agent     string `json:"agent"`
		Source    string `json:"source"`
		ID        string `json:"id"`
		Path      string `json:"path"`
		Filename  string `json:"filename"`
		Title     string `json:"title"`
		MIME      string `json:"mime"`
		ByteSize  int64  `json:"byte_size"`
		TS        string `json:"ts"`
	}
	if err := json.Unmarshal(evt.Payload, &card); err != nil {
		t.Fatalf("consumer decode failed: %v", err)
	}
	if card.ID == "" || !strings.HasPrefix(card.ID, "dlv_") || card.Path == "" || card.ByteSize == 0 {
		t.Fatalf("consumer decode lost fields: %+v", card)
	}
}

func TestWireFixture_Tool_PerRequestSSE(t *testing.T) {
	running := loadWireFixture(t, "sse_event.tool.running.json")
	completed := loadWireFixture(t, "sse_event.tool.completed.json")

	rec := httptest.NewRecorder()
	h := &sseEventHandler{w: rec, flusher: rec, ctx: context.Background()}

	h.OnToolCall(running["tool"].(string), running["args"].(string), running["tool_use_id"].(string))
	h.OnToolResult(
		completed["tool"].(string),
		running["args"].(string),
		completed["tool_use_id"].(string),
		agent.ToolResult{Content: completed["preview"].(string)},
		2410*time.Millisecond,
	)

	frames := parseSSEFrames(t, rec.Body.String())
	if len(frames) != 2 || frames[0][0] != "tool" || frames[1][0] != "tool" {
		t.Fatalf("frames = %v, want two tool frames", frames)
	}
	assertSemanticEqual(t, running, parseJSONMap(t, []byte(frames[0][1])))
	assertSemanticEqual(t, completed, parseJSONMap(t, []byte(frames[1][1])))
}

// TestWireFixture_Usage_PerRequestSSE pins the live usage payload. The
// capability advertises that web_search_calls is present even when its value
// is zero, so consumers do not need transport-specific missing-field rules.
func TestWireFixture_Usage_PerRequestSSE(t *testing.T) {
	fixture := loadWireFixture(t, "sse_event.usage.json")
	rec := httptest.NewRecorder()
	h := &sseEventHandler{
		w:       rec,
		flusher: rec,
		ctx:     context.Background(),
	}
	h.OnUsage(agent.TurnUsage{
		InputTokens:    1200,
		OutputTokens:   180,
		TotalTokens:    1380,
		CostUSD:        0.0123,
		LLMCalls:       1,
		WebSearchCalls: 0,
		Model:          "gpt-5.6-luna",
	})

	frames := parseSSEFrames(t, rec.Body.String())
	if len(frames) != 1 || frames[0][0] != "usage" {
		t.Fatalf("usage SSE frames = %#v, want one usage frame", frames)
	}
	produced := parseJSONMap(t, []byte(frames[0][1]))
	assertSemanticEqual(t, fixture, produced)

	var consumer struct {
		WebSearchCalls int `json:"web_search_calls"`
	}
	if err := json.Unmarshal([]byte(frames[0][1]), &consumer); err != nil {
		t.Fatalf("consumer decode failed: %v", err)
	}
	if consumer.WebSearchCalls != 0 {
		t.Fatalf("consumer decoded web_search_calls = %d, want 0", consumer.WebSearchCalls)
	}
}

// TestWireFixture_Done_PerRequestSSE pins the `event: done` payload.
// handleMessageSSE marshals *RunAgentResult directly (mustJSON(result)), so
// serializing the producer type IS the production path; running a full
// RunAgent here would require an LLM.
//
// RunAgentResult also carries reply_to_message_id and pending_ack_message_ids
// (both omitempty): set only when the run absorbed mid-run injected follow-ups
// and answers/acks them under their own cloud ids (WS reply addressing). They are
// absent from this typical fixture; Desktop's done consumer ignores them (it
// renders from the disk-refreshed transcript), but the consumer struct below
// lists them so the additive fields stay decode-checked.
func TestWireFixture_Done_PerRequestSSE(t *testing.T) {
	fixture := loadWireFixture(t, "sse_event.done.json")

	result := &RunAgentResult{
		Reply:     fixture["reply"].(string),
		SessionID: fixture["session_id"].(string),
		Agent:     fixture["agent"].(string),
		Usage: RunAgentUsage{
			InputTokens:    18432,
			OutputTokens:   956,
			TotalTokens:    19388,
			CostUSD:        0.0712,
			WebSearchCalls: 1,
		},
	}
	raw := []byte(mustJSON(result))
	produced := parseJSONMap(t, raw)
	assertSemanticEqual(t, fixture, produced)

	var done struct {
		Reply                string   `json:"reply"`
		ReplyToMessageID     string   `json:"reply_to_message_id"`
		PendingAckMessageIDs []string `json:"pending_ack_message_ids"`
		SessionID            string   `json:"session_id"`
		Agent                string   `json:"agent"`
		Usage                struct {
			InputTokens    int     `json:"input_tokens"`
			OutputTokens   int     `json:"output_tokens"`
			TotalTokens    int     `json:"total_tokens"`
			CostUSD        float64 `json:"cost_usd"`
			WebSearchCalls int     `json:"web_search_calls"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &done); err != nil {
		t.Fatalf("consumer decode failed: %v", err)
	}
	if done.Reply == "" || done.Usage.TotalTokens != 19388 || done.Usage.WebSearchCalls != 1 {
		t.Fatalf("consumer decode lost fields: %+v", done)
	}
}

// TestWireFixture_DonePartial_PerRequestSSE pins the explicit soft-stop
// metadata on the terminal per-request payload. handleMessageSSE marshals the
// RunAgentResult directly, so this exercises the same producer encoding as the
// live done frame rather than a parallel test-only map.
func TestWireFixture_DonePartial_PerRequestSSE(t *testing.T) {
	fixture := loadWireFixture(t, "sse_event.done.partial.json")

	result := &RunAgentResult{
		Reply:     fixture["reply"].(string),
		SessionID: fixture["session_id"].(string),
		Agent:     fixture["agent"].(string),
		Usage: RunAgentUsage{
			InputTokens:    18432,
			OutputTokens:   956,
			TotalTokens:    19388,
			CostUSD:        0.0712,
			WebSearchCalls: 1,
		},
		Partial:     true,
		FailureCode: runstatus.CodeIterationLimit,
	}
	raw := []byte(mustJSON(result))
	assertSemanticEqual(t, fixture, parseJSONMap(t, raw))

	var consumer struct {
		Reply       string `json:"reply"`
		SessionID   string `json:"session_id"`
		Partial     bool   `json:"partial"`
		FailureCode string `json:"failure_code"`
	}
	if err := json.Unmarshal(raw, &consumer); err != nil {
		t.Fatalf("consumer decode failed: %v", err)
	}
	if consumer.Reply == "" || consumer.SessionID == "" || !consumer.Partial || consumer.FailureCode != "iteration_limit" {
		t.Fatalf("consumer decode lost partial metadata: %+v", consumer)
	}
}

type wireFixtureProbeTool struct{}

func (*wireFixtureProbeTool) Info() agent.ToolInfo {
	return agent.ToolInfo{Name: "wire_fixture_probe", Description: "Return deterministic fixture evidence."}
}
func (*wireFixtureProbeTool) RequiresApproval() bool     { return false }
func (*wireFixtureProbeTool) IsReadOnlyCall(string) bool { return true }
func (*wireFixtureProbeTool) Run(context.Context, string) (agent.ToolResult, error) {
	return agent.ToolResult{Content: "fixture probe complete"}, nil
}

// TestWireFixture_AgentReplyClean_Bus verifies the complementary omitempty
// contract through the real RunAgent producer: a clean persisted reply must
// not put partial or failure_code on the broadcast bus.
func TestWireFixture_AgentReplyClean_Bus(t *testing.T) {
	fixture := loadWireFixture(t, "bus_event.agent_reply.json")
	reply := fixture["text"].(string)
	gw := &fakeGatewayBackend{reply: reply}
	ts := httptest.NewServer(gw.handler())
	defer ts.Close()

	deps := runAgentContractTestDeps(t, ts.URL)
	deps.EventBus = NewEventBus()
	defer deps.SessionCache.CloseAll()
	sub := deps.EventBus.Subscribe()
	defer deps.EventBus.Unsubscribe(sub)

	// heartbeat is intentionally autonomous: it suppresses detached smart-title
	// and suggestion work, so neither can outlive RunAgent and race cleanup of
	// the BypassRouting temporary session directory.
	result, err := RunAgent(context.Background(), deps, RunAgentRequest{
		Text:          "finish this request",
		Source:        "heartbeat",
		BypassRouting: true,
	}, nullEventHandler{})
	if err != nil {
		t.Fatalf("RunAgent clean result error: %v", err)
	}
	if result == nil || result.Partial || result.FailureCode != runstatus.CodeNone {
		t.Fatalf("RunAgent returned unexpected clean status: %+v", result)
	}

	evt := waitBusEvent(t, sub, EventAgentReply)
	produced := parseJSONMap(t, evt.Payload)
	if sessionID, ok := produced["session_id"].(string); !ok || sessionID == "" {
		t.Fatalf("agent_reply session_id missing or empty: %#v", produced["session_id"])
	}
	produced["session_id"] = fixture["session_id"]
	assertSemanticEqual(t, fixture, produced)
	for _, key := range []string{"partial", "failure_code"} {
		if _, exists := produced[key]; exists {
			t.Fatalf("clean agent_reply unexpectedly contains %q: %#v", key, produced)
		}
	}
}

func TestWireFixture_AgentError_Bus(t *testing.T) {
	fixture := loadWireFixture(t, "bus_event.agent_error.json")
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "synthetic upstream rejection", http.StatusTeapot)
	}))
	defer ts.Close()

	deps := runAgentContractTestDeps(t, ts.URL)
	deps.EventBus = NewEventBus()
	defer deps.SessionCache.CloseAll()
	sub := deps.EventBus.Subscribe()
	defer deps.EventBus.Unsubscribe(sub)

	result, err := RunAgent(context.Background(), deps, RunAgentRequest{
		Text:          "fail this fixture run",
		Source:        fixture["source"].(string),
		BypassRouting: true,
	}, nullEventHandler{})
	if err == nil || result == nil || result.FailureCode != runstatus.CodeUnexpected {
		t.Fatalf("RunAgent hard-error result=%+v err=%v", result, err)
	}

	evt := waitBusEvent(t, sub, EventAgentError)
	produced := parseJSONMap(t, evt.Payload)
	if sessionID, ok := produced["session_id"].(string); !ok || sessionID == "" {
		t.Fatalf("agent_error session_id missing or empty: %#v", produced["session_id"])
	}
	produced["session_id"] = fixture["session_id"]
	assertSemanticEqual(t, fixture, produced)

	var consumer struct {
		Error         string `json:"error"`
		FriendlyError string `json:"friendly_error"`
		FailureCode   string `json:"failure_code"`
	}
	if err := json.Unmarshal(evt.Payload, &consumer); err != nil {
		t.Fatalf("consumer decode failed: %v", err)
	}
	if consumer.Error == "" || consumer.FriendlyError == "" || consumer.FailureCode == "" {
		t.Fatalf("consumer decode lost error fields: %+v", consumer)
	}
}

// TestWireFixture_AgentReplyPartial_Bus drives a real RunAgent through its
// max-iteration soft-stop path and captures the EventBus payload emitted only
// after the transcript is saved. The scripted gateway supplies one tool call
// followed by the loop's terminal no-tool synthesis response.
func TestWireFixture_AgentReplyPartial_Bus(t *testing.T) {
	fixture := loadWireFixture(t, "bus_event.agent_reply.partial.json")
	var calls atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/completions" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{}`))
			return
		}
		resp := client.CompletionResponse{
			Provider:     "anthropic",
			Model:        "test-model",
			FinishReason: "end_turn",
			OutputText:   fixture["text"].(string),
		}
		if calls.Add(1) == 1 {
			resp.FinishReason = "tool_use"
			resp.OutputText = ""
			resp.ToolCalls = []client.FunctionCall{{
				ID:        "toolu_wire_partial_1",
				Name:      "wire_fixture_probe",
				Arguments: json.RawMessage(`{}`),
			}}
		}
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
			w.Header().Set("Content-Type", "text/event-stream")
			raw, err := json.Marshal(struct {
				Type string `json:"type"`
				client.CompletionResponse
			}{Type: "done", CompletionResponse: resp})
			if err != nil {
				t.Errorf("marshal streaming gateway response: %v", err)
				return
			}
			_, _ = fmt.Fprintf(w, "data: %s\n\n", raw)
			_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Errorf("encode gateway response: %v", err)
		}
	}))
	defer ts.Close()

	deps := runAgentContractTestDeps(t, ts.URL)
	deps.Config.Agent.MaxIterations = 1
	deps.EventBus = NewEventBus()
	deps.Registry.Register(&wireFixtureProbeTool{})
	defer deps.SessionCache.CloseAll()
	sub := deps.EventBus.Subscribe()
	defer deps.EventBus.Unsubscribe(sub)

	// heartbeat is intentionally autonomous: it suppresses detached smart-title
	// and suggestion work, so neither can outlive RunAgent and race cleanup of
	// the BypassRouting temporary session directory.
	result, err := RunAgent(context.Background(), deps, RunAgentRequest{
		Text:          "continue until the requested work is complete",
		Source:        fixture["source"].(string),
		BypassRouting: true,
	}, nullEventHandler{})
	if err != nil {
		t.Fatalf("RunAgent soft-stop error: %v", err)
	}
	if result == nil || !result.Partial || result.FailureCode != runstatus.CodeIterationLimit {
		t.Fatalf("RunAgent result did not carry iteration-limit partial status after %d gateway calls: %+v", calls.Load(), result)
	}

	evt := waitBusEvent(t, sub, EventAgentReply)
	produced := parseJSONMap(t, evt.Payload)
	if sessionID, ok := produced["session_id"].(string); !ok || sessionID == "" {
		t.Fatalf("agent_reply session_id missing or empty: %#v", produced["session_id"])
	}
	produced["session_id"] = fixture["session_id"]
	assertSemanticEqual(t, fixture, produced)

	var consumer struct {
		Text        string `json:"text"`
		Partial     *bool  `json:"partial"`
		FailureCode string `json:"failure_code"`
	}
	if err := json.Unmarshal(evt.Payload, &consumer); err != nil {
		t.Fatalf("consumer decode failed: %v", err)
	}
	if consumer.Text == "" || consumer.Partial == nil || !*consumer.Partial || consumer.FailureCode != "iteration_limit" {
		t.Fatalf("consumer decode lost partial metadata: %+v", consumer)
	}
}

// TestWireFixture_DoneWithDeliverable pins the stronger idempotency receipt a
// client requires before deleting its retained source after persisting a deliverable.
func TestWireFixture_DoneWithDeliverable(t *testing.T) {
	fixture := loadWireFixture(t, "sse_event.done.with_deliverable.json")
	deliverable := fixture["deliverables"].([]any)[0].(map[string]any)
	result := &RunAgentResult{
		Reply:     fixture["reply"].(string),
		SessionID: fixture["session_id"].(string),
		Agent:     fixture["agent"].(string),
		Usage: RunAgentUsage{
			InputTokens: 9240, OutputTokens: 312, TotalTokens: 9552, CostUSD: 0.0318,
		},
		Deliverables: []session.DeliverableReceipt{{
			ID:       deliverable["id"].(string),
			Path:     deliverable["path"].(string),
			Filename: deliverable["filename"].(string),
			Title:    deliverable["title"].(string),
			MIME:     deliverable["mime"].(string),
			ByteSize: int64(deliverable["byte_size"].(float64)),
		}},
	}
	produced := parseJSONMap(t, []byte(mustJSON(result)))
	assertSemanticEqual(t, fixture, produced)

	var consumer struct {
		Reply        string `json:"reply"`
		SessionID    string `json:"session_id"`
		Deliverables []struct {
			Path     string `json:"path"`
			Filename string `json:"filename"`
			MIME     string `json:"mime"`
			ByteSize int64  `json:"byte_size"`
		} `json:"deliverables"`
	}
	if err := json.Unmarshal([]byte(mustJSON(result)), &consumer); err != nil {
		t.Fatalf("decode done-with-deliverable as Desktop consumer: %v", err)
	}
	if consumer.Reply != "" || len(consumer.Deliverables) != 1 || consumer.Deliverables[0].ByteSize <= 0 {
		t.Fatalf("consumer decoded unexpected result: %+v", consumer)
	}
}

func TestWireFixture_CloudProgress_Bus(t *testing.T) {
	fixture := loadWireFixture(t, "bus_event.cloud_progress.json")

	bus := NewEventBus()
	sub := bus.Subscribe()
	defer bus.Unsubscribe(sub)
	h := &busEventHandler{
		deps:      &ServerDeps{EventBus: bus},
		sessionID: fixture["session_id"].(string),
	}
	h.OnCloudProgress(2, 5)

	evt := waitBusEvent(t, sub, EventCloudProgress)
	assertSemanticEqual(t, fixture, parseJSONMap(t, evt.Payload))
}

func TestWireFixture_SuggestionReady_Bus(t *testing.T) {
	fixture := loadWireFixture(t, "bus_event.suggestion_ready.json")

	payload := suggestionReadyPayload(
		fixture["session_id"].(string),
		fixture["agent"].(string),
		fixture["text"].(string),
	)
	assertSemanticEqual(t, fixture, parseJSONMap(t, payload))
}

// --- HTTP responses (full-router seam) ------------------------------------

func TestWireFixture_HTTPComputerUseTopology(t *testing.T) {
	fixture := loadWireFixture(t, "http_get.computer_use_topology.response.json")
	original := readDisplayTopologyVia
	defer func() { readDisplayTopologyVia = original }()
	readDisplayTopologyVia = func(context.Context) (tools.DisplayTopologyV1, error) {
		return canonicalHelperDisplayTopology(t), nil
	}

	rec := httptest.NewRecorder()
	NewServer(0, nil, nil, "test").Handler().ServeHTTP(
		rec,
		authorizedComputerUseTopologyRequest(t))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /local/computer-use/topology = %d: %s", rec.Code, rec.Body.Bytes())
	}
	assertSemanticEqual(t, fixture, parseJSONMap(t, rec.Body.Bytes()))

	// Decode the bytes emitted by the real HTTP route through the same strict
	// versioned contract Desktop vendors. This rejects missing/null scalar
	// fields instead of allowing Go zero values to masquerade as valid wire data.
	topology, err := tools.DecodeDisplayTopologyV1(rec.Body.Bytes())
	if err != nil {
		t.Fatalf("consumer strict decode failed: %v", err)
	}
	if topology.TopologyID != "topo_mixed_001" || topology.Displays[1].QuartzBounds.X != -1600 {
		t.Fatalf("consumer topology lost authority or mixed coordinates: %+v", topology)
	}
}

func TestWireFixture_HTTPComputerUseAppPolicyUpdate(t *testing.T) {
	requestFixture := loadWireFixture(t, "computer_use.app_policy.update.request.json")
	responseFixture := loadWireFixture(t, "computer_use.app_policy.update.response.json")
	requestPayload, err := json.Marshal(requestFixture)
	if err != nil {
		t.Fatal(err)
	}
	deps := &ServerDeps{ShannonDir: t.TempDir()}
	server := NewServer(0, nil, deps, "test")
	t.Setenv(localPresenceEnv, "wire-fixture-presence")
	req := httptest.NewRequest(http.MethodPut, "/local/computer-use/app-policy", bytes.NewReader(requestPayload))
	req.Header.Set(localPresenceHeader, "wire-fixture-presence")
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT /local/computer-use/app-policy = %d: %s", rec.Code, rec.Body.Bytes())
	}
	assertSemanticEqual(t, responseFixture, parseJSONMap(t, rec.Body.Bytes()))

	var snapshot ComputerUseAppPolicySnapshot
	if err := decodeStrictComputerUseAppPolicyJSON(rec.Body.Bytes(), &snapshot); err != nil {
		t.Fatalf("consumer strict decode failed: %v", err)
	}
	entry := findAppPolicyEntry(snapshot.Entries, "com.example.editor")
	if snapshot.SchemaVersion != 1 || snapshot.Revision != 1 || entry == nil ||
		entry.Decision != ComputerUseAppPolicyBlocked || entry.Source != ComputerUseAppPolicySourceUser {
		t.Fatalf("consumer lost app policy fields: %+v", snapshot)
	}

	// The revoke fixture is published to Desktop as part of this contract, so it
	// must be emitted through the real DELETE path too — otherwise its shape can
	// drift away from the Go type with nothing to catch it.
	revokeFixture := loadWireFixture(t, "computer_use.app_policy.revoke.request.json")
	revokePayload, err := json.Marshal(revokeFixture)
	if err != nil {
		t.Fatal(err)
	}
	revokeReq := httptest.NewRequest(
		http.MethodDelete, "/local/computer-use/app-policy", bytes.NewReader(revokePayload))
	revokeReq.Header.Set(localPresenceHeader, "wire-fixture-presence")
	revokeRec := httptest.NewRecorder()
	server.Handler().ServeHTTP(revokeRec, revokeReq)
	if revokeRec.Code != http.StatusOK {
		t.Fatalf("DELETE /local/computer-use/app-policy = %d: %s",
			revokeRec.Code, revokeRec.Body.Bytes())
	}
	var revoked ComputerUseAppPolicySnapshot
	if err := decodeStrictComputerUseAppPolicyJSON(revokeRec.Body.Bytes(), &revoked); err != nil {
		t.Fatalf("consumer strict decode failed after revoke: %v", err)
	}
	if revoked.Revision <= snapshot.Revision {
		t.Fatalf("revoke did not advance revision: %d -> %d", snapshot.Revision, revoked.Revision)
	}
	if entry := findAppPolicyEntry(revoked.Entries, "com.example.editor"); entry != nil &&
		entry.Source == ComputerUseAppPolicySourceUser {
		t.Fatalf("revoked user rule survived: %+v", entry)
	}
}

func TestWireFixture_HTTPConsequentialRiskConfirmation(t *testing.T) {
	detailFixture := loadWireFixture(t, "computer_use.risk_intent.detail.response.json")
	for _, decision := range []struct {
		name            string
		requestFixture  string
		responseFixture string
		wantDecision    ConsequentialRiskDecision
	}{
		{
			name:            "allow",
			requestFixture:  "computer_use.risk_intent.allow.request.json",
			responseFixture: "computer_use.risk_intent.allow.response.json",
			wantDecision:    ConsequentialRiskDecisionAllowed,
		},
		{
			name:            "deny",
			requestFixture:  "computer_use.risk_intent.deny.request.json",
			responseFixture: "computer_use.risk_intent.deny.response.json",
			wantDecision:    ConsequentialRiskDecisionDenied,
		},
	} {
		t.Run(decision.name, func(t *testing.T) {
			broker, _ := newConsequentialRiskBrokerFixture(t, sequentialConsequentialRiskRandom(1))
			intent, _, err := broker.Register(consequentialRiskBrokerDraft(t, "req_http_send"))
			if err != nil {
				t.Fatal(err)
			}
			server := NewServer(0, nil, nil, "test")
			server.SetConsequentialRiskBroker(broker)
			t.Setenv(localPresenceEnv, "wire-fixture-presence")

			detailReq := httptest.NewRequest(http.MethodGet,
				"/local/computer-use/risk-intents/"+intent.IntentID, nil)
			detailReq.RemoteAddr = "127.0.0.1:54321"
			detailReq.Header.Set(localPresenceHeader, "wire-fixture-presence")
			detailRec := httptest.NewRecorder()
			server.Handler().ServeHTTP(detailRec, detailReq)
			if detailRec.Code != http.StatusOK || detailRec.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("risk detail status=%d cache=%q body=%s",
					detailRec.Code, detailRec.Header().Get("Cache-Control"), detailRec.Body.Bytes())
			}
			assertSemanticEqual(t, detailFixture, parseJSONMap(t, detailRec.Body.Bytes()))
			decodedIntent, err := tools.DecodeConsequentialRiskIntentV1(
				detailRec.Body.Bytes(), consequentialRiskBrokerFixtureNow)
			if err != nil || decodedIntent.IntentID != intent.IntentID ||
				decodedIntent.Target.TargetDigest != intent.Target.TargetDigest {
				t.Fatalf("consumer risk detail decode = %+v, %v", decodedIntent, err)
			}

			requestFixture := loadWireFixture(t, decision.requestFixture)
			requestPayload, err := json.Marshal(requestFixture)
			if err != nil {
				t.Fatal(err)
			}
			decisionReq := httptest.NewRequest(http.MethodPost,
				"/local/computer-use/risk-intents/"+intent.IntentID+"/decision",
				bytes.NewReader(requestPayload))
			decisionReq.RemoteAddr = "127.0.0.1:54321"
			decisionReq.Header.Set(localPresenceHeader, "wire-fixture-presence")
			decisionRec := httptest.NewRecorder()
			server.Handler().ServeHTTP(decisionRec, decisionReq)
			if decisionRec.Code != http.StatusOK || decisionRec.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("risk decision status=%d cache=%q body=%s",
					decisionRec.Code, decisionRec.Header().Get("Cache-Control"), decisionRec.Body.Bytes())
			}
			responseFixture := loadWireFixture(t, decision.responseFixture)
			assertSemanticEqual(t, responseFixture, parseJSONMap(t, decisionRec.Body.Bytes()))
			var response ConsequentialRiskDecisionResponseV1
			if err := decodeStrictComputerUseAppPolicyJSON(decisionRec.Body.Bytes(), &response); err != nil {
				t.Fatalf("consumer risk decision decode failed: %v", err)
			}
			if response.SchemaVersion != 1 || response.IntentID != intent.IntentID ||
				response.Decision != decision.wantDecision {
				t.Fatalf("consumer lost risk decision fields: %+v", response)
			}
		})
	}
}

func TestWireFixture_HTTPCoordinateConsequentialRiskDetail(t *testing.T) {
	fixture := loadWireFixture(t, "computer_use.risk_intent.detail.coordinate.response.json")
	broker, _ := newConsequentialRiskBrokerFixture(t, sequentialConsequentialRiskRandom(1))
	intent, _, err := broker.Register(consequentialRiskCoordinateBrokerDraft(
		t,
		"req_http_coordinate",
		consequentialRiskBrokerFixtureNow.Add(30*time.Second),
	))
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(0, nil, nil, "test")
	server.SetConsequentialRiskBroker(broker)
	t.Setenv(localPresenceEnv, "wire-fixture-presence")

	req := httptest.NewRequest(
		http.MethodGet,
		"/local/computer-use/risk-intents/"+intent.IntentID,
		nil,
	)
	req.RemoteAddr = "127.0.0.1:54321"
	req.Header.Set(localPresenceHeader, "wire-fixture-presence")
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("coordinate risk detail status=%d cache=%q body=%s",
			rec.Code, rec.Header().Get("Cache-Control"), rec.Body.Bytes())
	}
	assertSemanticEqual(t, fixture, parseJSONMap(t, rec.Body.Bytes()))
	decoded, err := tools.DecodeConsequentialRiskIntentV1(
		rec.Body.Bytes(), consequentialRiskBrokerFixtureNow)
	if err != nil {
		t.Fatalf("consumer strict decode failed: %v", err)
	}
	authority := decoded.Target.CoordinateAuthority
	if decoded.Target.ActionKind != "click" ||
		decoded.Target.ExecutionPath != "synthetic_coordinate" ||
		authority == nil || authority.ElementPath != "window[0]/AXButton[0]" ||
		authority.FrameExpiresAt != decoded.ExpiresAt ||
		authority.QuartzPoint != (tools.ConsequentialRiskQuartzPointV1{X: 100.5, Y: 200.5}) {
		t.Fatalf("consumer lost coordinate authority: %+v", decoded.Target)
	}
}

func TestWireFixture_HTTPStatus(t *testing.T) {
	fixture := loadWireFixture(t, "http_get.status.response.json")

	srv := NewServer(0, &Client{}, nil, "0.1.8")
	memSvc := memory.NewService(memory.Config{Provider: "disabled"}, nil)
	if err := memSvc.Start(context.Background()); err != nil {
		t.Fatalf("memory service start: %v", err)
	}
	srv.memSvc = memSvc

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /status = %d", rec.Code)
	}
	produced := parseJSONMap(t, rec.Body.Bytes())

	// uptime is wall-clock-dependent; assert numeric then normalize.
	if _, ok := produced["uptime"].(float64); !ok {
		t.Fatalf("uptime missing or not numeric: %#v", produced["uptime"])
	}
	produced["uptime"] = fixture["uptime"]
	assertSemanticEqual(t, fixture, produced)

	// Consumer-shaped decode (mirrors the Desktop status decoder: optional
	// capabilities array + has() gating, memory block with explicit-null
	// reason).
	var status struct {
		IsConnected  bool      `json:"is_connected"`
		ActiveAgent  string    `json:"active_agent"`
		Version      string    `json:"version"`
		Capabilities *[]string `json:"capabilities"`
		Memory       *struct {
			Provider string  `json:"provider"`
			Reason   *string `json:"reason"`
		} `json:"memory"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil {
		t.Fatalf("consumer decode failed: %v", err)
	}
	if status.Capabilities == nil {
		t.Fatal("capabilities missing — Desktop feature gating would see nil")
	}
	has := func(tok string) bool {
		for _, c := range *status.Capabilities {
			if c == tok {
				return true
			}
		}
		return false
	}
	if !has(CapToolUseIDEvents) {
		t.Fatalf("capabilities lost %q: %v", CapToolUseIDEvents, *status.Capabilities)
	}
	if !has(CapDeliverableEventV1) {
		t.Fatalf("capabilities lost %q: %v", CapDeliverableEventV1, *status.Capabilities)
	}
	if !has(CapConfigReloadStateV1) {
		t.Fatalf("capabilities lost %q: %v", CapConfigReloadStateV1, *status.Capabilities)
	}
	if !has(CapAgentDefaultCWDV1) {
		t.Fatalf("capabilities lost %q: %v", CapAgentDefaultCWDV1, *status.Capabilities)
	}
	if !has(CapRemoteSessionTimelineV1) {
		t.Fatalf("capabilities lost %q: %v", CapRemoteSessionTimelineV1, *status.Capabilities)
	}
	if !has(CapScheduleSessionFilterV1) {
		t.Fatalf("capabilities lost %q: %v", CapScheduleSessionFilterV1, *status.Capabilities)
	}
	if !has(CapWebSearchUsageV1) {
		t.Fatalf("capabilities lost %q: %v", CapWebSearchUsageV1, *status.Capabilities)
	}
	if !has(CapComputerUseTopologyV1) {
		t.Fatalf("capabilities lost %q: %v", CapComputerUseTopologyV1, *status.Capabilities)
	}
	if !has(CapComputerUseControlV1) {
		t.Fatalf("capabilities lost %q: %v", CapComputerUseControlV1, *status.Capabilities)
	}
	if !has(CapComputerUsePreviewV1) {
		t.Fatalf("capabilities lost %q: %v", CapComputerUsePreviewV1, *status.Capabilities)
	}
	if !has(CapComputerUseAppPolicyV1) {
		t.Fatalf("capabilities lost %q: %v", CapComputerUseAppPolicyV1, *status.Capabilities)
	}
	if !has(CapComputerUsePhysicalInterferenceV1) {
		t.Fatalf("capabilities lost %q: %v", CapComputerUsePhysicalInterferenceV1, *status.Capabilities)
	}
	if !has(CapComputerUseRiskConfirmationV1) {
		t.Fatalf("capabilities lost %q: %v", CapComputerUseRiskConfirmationV1, *status.Capabilities)
	}
	if !has(CapSkillInstallRecommendationV1) {
		t.Fatalf("capabilities lost %q: %v", CapSkillInstallRecommendationV1, *status.Capabilities)
	}
	if status.Memory == nil || status.Memory.Provider != "disabled" || status.Memory.Reason != nil {
		t.Fatalf("memory block decode mismatch: %+v", status.Memory)
	}
}

func newConfigReloadWireFixtureServer(t *testing.T) *Server {
	t.Helper()
	shannonDir := t.TempDir()
	configPath := filepath.Join(shannonDir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("skills:\n  disabled:\n    - old\n"), 0600); err != nil {
		t.Fatal(err)
	}
	revision, err := config.FileRevision(shannonDir)
	if err != nil {
		t.Fatal(err)
	}
	srv := NewServer(0, nil, &ServerDeps{
		ShannonDir: shannonDir,
		Config: &config.Config{
			Skills:  config.SkillsConfig{Disabled: []string{"old"}},
			Sources: map[string]config.ConfigSource{"skills.disabled": {File: "config.yaml", Level: "global"}},
		},
		ConfigRevision: revision,
	}, "test")
	if err := os.WriteFile(configPath, []byte("skills:\n  disabled: []\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return srv
}

func TestWireFixture_HTTPConfigReloadState(t *testing.T) {
	fixture := loadWireFixture(t, "http_get.config.reload_required.response.json")
	srv := newConfigReloadWireFixtureServer(t)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/config", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /config = %d, body=%s", rec.Code, rec.Body.Bytes())
	}
	assertSemanticEqual(t, fixture, parseJSONMap(t, rec.Body.Bytes()))

	var response struct {
		Global         map[string]any `json:"global"`
		Effective      map[string]any `json:"effective"`
		Sources        []string       `json:"sources"`
		ReloadRequired bool           `json:"reload_required"`
		ReloadReason   string         `json:"reload_reason"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("consumer decode failed: %v", err)
	}
	if !response.ReloadRequired || !strings.Contains(response.ReloadReason, "POST /config/reload") {
		t.Fatalf("consumer lost config reload state: %+v", response)
	}
}

func TestWireFixture_HTTPConfigStatusReloadState(t *testing.T) {
	fixture := loadWireFixture(t, "http_get.config_status.reload_required.response.json")
	srv := newConfigReloadWireFixtureServer(t)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/config/status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /config/status = %d, body=%s", rec.Code, rec.Body.Bytes())
	}
	assertSemanticEqual(t, fixture, parseJSONMap(t, rec.Body.Bytes()))

	var response struct {
		ReloadRequired bool   `json:"reload_required"`
		ReloadReason   string `json:"reload_reason"`
		Koe            *struct {
			AudioProcessing string `json:"audio_processing"`
		} `json:"koe"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("consumer decode failed: %v", err)
	}
	if !response.ReloadRequired || response.ReloadReason == "" || response.Koe == nil {
		t.Fatalf("consumer lost config status fields: %+v", response)
	}
}

func TestWireFixture_HTTPSkillDeleteErrors(t *testing.T) {
	type consumerError struct {
		Code  string `json:"code"`
		Error string `json:"error"`
	}
	tests := []struct {
		name       string
		fixture    string
		skill      string
		wantStatus int
		setup      func(*testing.T, string, string)
	}{
		{
			name:       "builtin",
			fixture:    "http_delete.skill.builtin.response.json",
			skill:      "kocoro",
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "invalid agent manifest",
			fixture:    "http_delete.skill.invalid_agent_manifest.response.json",
			skill:      "demo",
			wantStatus: http.StatusConflict,
			setup: func(t *testing.T, shannonDir, agentsDir string) {
				daemonTestWriteFile(t, filepath.Join(shannonDir, "skills", "demo", "SKILL.md"), "---\nname: demo\ndescription: fixture\n---\n")
				daemonTestWriteFile(t, filepath.Join(agentsDir, "broken", "_attached.yaml"), "not: a-list\n")
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			shannonDir := t.TempDir()
			agentsDir := filepath.Join(shannonDir, "agents")
			if test.setup != nil {
				test.setup(t, shannonDir, agentsDir)
			}
			srv := NewServer(0, nil, &ServerDeps{
				Config:     &config.Config{},
				ShannonDir: shannonDir,
				AgentsDir:  agentsDir,
			}, "test")
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/skills/"+test.skill+"?confirm=true", nil))
			if rec.Code != test.wantStatus {
				t.Fatalf("DELETE /skills/%s = %d, body=%s", test.skill, rec.Code, rec.Body.Bytes())
			}
			assertSemanticEqual(t, loadWireFixture(t, test.fixture), parseJSONMap(t, rec.Body.Bytes()))
			var decoded consumerError
			if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
				t.Fatal(err)
			}
			if decoded.Code == "" || decoded.Error == "" || decoded.Code == decoded.Error {
				t.Fatalf("consumer lost machine code or human message: %+v", decoded)
			}
		})
	}
}

func TestWireFixture_SkillRecommendationV1(t *testing.T) {
	fixture := loadWireFixture(t, "sse_event.skill.recommendation.v1.json")
	dir := t.TempDir()
	deps := &ServerDeps{Config: &config.Config{}, ShannonDir: dir, CatalogProvider: skills.NewEmbeddedCatalogProvider(dir)}
	s := NewServer(0, nil, deps, "test")
	auth := NewAuthManager(AuthManagerConfig{ShannonDir: dir})
	auth.setState(AuthStateSignedIn, &client.AuthUser{ID: "opaque-account"}, "")
	s.auth = auth
	httpServer := httptest.NewServer(http.HandlerFunc(s.handleEvents))
	defer httpServer.Close()
	request, err := http.NewRequest(http.MethodGet, httpServer.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	deviceID := "12345678-1234-1234-1234-123456789abc"
	request.Header.Set(desktopDeviceHeader, deviceID)
	request.Header.Set(skillRecommendationHeader, CapSkillInstallRecommendationV1)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	deadline := time.Now().Add(2 * time.Second)
	for !s.hasSkillRecommendationSink("opaque-account", deviceID) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	run := &skillRecommendationRunContext{accountID: "opaque-account", deviceID: deviceID, agentName: "researcher", sessionID: "session_demo", turnID: "turn_demo", store: s.skillRecommendations, emit: s.emitSkillRecommendation, discovered: map[string]bool{}}
	runContext := withSkillRecommendationRun(context.Background(), run)
	discovery, err := (&discoverInstallableSkillsTool{shannonDir: dir}).Run(runContext, `{"intent_tags":["presentation.create"]}`)
	if err != nil || discovery.IsError {
		t.Fatalf("catalog discovery=%+v err=%v", discovery, err)
	}
	offered := make(chan agent.ToolResult, 1)
	go func() {
		result, _ := (&offerSkillInstallationTool{shannonDir: dir}).Run(runContext, `{"catalog_ids":["official:pptx"],"reason":"Create the requested presentation"}`)
		offered <- result
	}()
	scanner := bufio.NewScanner(response.Body)
	var payload []byte
	for scanner.Scan() {
		if line := scanner.Bytes(); bytes.HasPrefix(line, []byte("data: ")) {
			payload = append([]byte(nil), line[len("data: "):]...)
			break
		}
	}
	if len(payload) == 0 {
		t.Fatal("real /events producer emitted no recommendation payload")
	}
	if result := <-offered; result.IsError || !result.StopAgentLoop {
		t.Fatalf("real offer producer result=%+v", result)
	}
	produced := parseJSONMap(t, payload)
	for _, field := range []string{"recommendation_id", "continuation_token", "expires_at"} {
		if produced[field] == nil || produced[field] == "" {
			t.Fatalf("dynamic field %s missing from producer: %v", field, produced)
		}
		produced[field] = fixture[field]
	}
	assertSemanticEqual(t, fixture, produced)
	var consumer struct {
		SchemaVersion     int    `json:"schema_version"`
		RecommendationID  string `json:"recommendation_id"`
		SessionID         string `json:"session_id"`
		TurnID            string `json:"turn_id"`
		CatalogRevision   string `json:"catalog_revision"`
		State             string `json:"state"`
		ContinuationToken string `json:"continuation_token"`
		Items             []struct {
			CatalogID         string `json:"catalog_id"`
			Slug              string `json:"slug"`
			CapabilitySummary string `json:"capability_summary"`
		} `json:"items"`
	}
	if err := json.Unmarshal(payload, &consumer); err != nil {
		t.Fatal(err)
	}
	if consumer.SchemaVersion != 1 || consumer.SessionID != "session_demo" || consumer.TurnID != "turn_demo" || consumer.ContinuationToken == "" || len(consumer.Items) != 1 || consumer.Items[0].CatalogID != "official:pptx" {
		t.Fatalf("consumer decode mismatch: %+v", consumer)
	}
}

func TestWireFixture_SkillRecommendationContinuationAndDismiss(t *testing.T) {
	continueRequestFixture := loadWireFixture(t, "http_post.skill_recommendation_continue.request.json")
	acceptedFixture := loadWireFixture(t, "http_post.skill_recommendation_continue.accepted.response.json")
	completedFixture := loadWireFixture(t, "http_post.skill_recommendation_continue.completed.response.json")
	dismissRequestFixture := loadWireFixture(t, "http_post.skill_recommendation_dismiss.request.json")
	dismissFixture := loadWireFixture(t, "http_post.skill_recommendation_dismiss.response.json")
	if len(dismissRequestFixture) != 0 {
		t.Fatalf("dismiss request must remain body-empty: %v", dismissRequestFixture)
	}

	dir := t.TempDir()
	deps := &ServerDeps{Config: &config.Config{}, ShannonDir: dir, CatalogProvider: skills.NewEmbeddedCatalogProvider(dir)}
	s := NewServer(0, nil, deps, "test")
	auth := NewAuthManager(AuthManagerConfig{ShannonDir: dir})
	auth.setState(AuthStateSignedIn, &client.AuthUser{ID: "opaque-account"}, "")
	s.auth = auth
	deviceID := "12345678-1234-1234-1234-123456789abc"
	if err := os.MkdirAll(filepath.Join(dir, "skills", "pptx"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "skills", "pptx", "SKILL.md"), []byte("---\nname: pptx\ndescription: fixture\n---\n"), 0600); err != nil {
		t.Fatal(err)
	}
	v, _, err := s.skillRecommendations.offer("opaque-account", deviceID, "", continueRequestFixture["session_id"].(string), "turn_demo", "sha256:fixture", []skillRecommendationItemWireV1{{CatalogID: "official:pptx", Slug: "pptx", DisplayName: "Presentation"}})
	if err != nil {
		t.Fatal(err)
	}
	accepted, run, err := s.skillRecommendations.beginContinuation("opaque-account", deviceID, v.SessionID, v.RecommendationID, v.ContinuationToken)
	if err != nil || !run {
		t.Fatalf("prime accepted state run=%v err=%v", run, err)
	}

	callContinue := func() *httptest.ResponseRecorder {
		body := map[string]any{
			"session_id":         continueRequestFixture["session_id"],
			"continuation_token": v.ContinuationToken,
		}
		encoded, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, "/skill-recommendations/"+v.RecommendationID+"/continue", bytes.NewReader(encoded))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(desktopDeviceHeader, deviceID)
		req.Header.Set(skillRecommendationHeader, CapSkillInstallRecommendationV1)
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, req)
		return rr
	}

	acceptedResponse := callContinue()
	if acceptedResponse.Code != http.StatusAccepted {
		t.Fatalf("accepted retry status=%d body=%s", acceptedResponse.Code, acceptedResponse.Body.String())
	}
	assertSemanticEqual(t, acceptedFixture, parseJSONMap(t, acceptedResponse.Body.Bytes()))
	var acceptedConsumer struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(acceptedResponse.Body.Bytes(), &acceptedConsumer); err != nil || acceptedConsumer.Status != "accepted" {
		t.Fatalf("accepted consumer decode=%+v err=%v", acceptedConsumer, err)
	}

	if err := s.skillRecommendations.finishContinuation(accepted, "completed"); err != nil {
		t.Fatal(err)
	}
	completedResponse := callContinue()
	if completedResponse.Code != http.StatusOK {
		t.Fatalf("completed replay status=%d body=%s", completedResponse.Code, completedResponse.Body.String())
	}
	assertSemanticEqual(t, completedFixture, parseJSONMap(t, completedResponse.Body.Bytes()))

	dismissed, _, err := s.skillRecommendations.offer("opaque-account", deviceID, "", "dismiss_session", "dismiss_turn", "sha256:fixture", []skillRecommendationItemWireV1{{CatalogID: "official:pptx", Slug: "pptx"}})
	if err != nil {
		t.Fatal(err)
	}
	dismissBody, _ := json.Marshal(dismissRequestFixture)
	dismissRequest := httptest.NewRequest(http.MethodPost, "/skill-recommendations/"+dismissed.RecommendationID+"/dismiss", bytes.NewReader(dismissBody))
	dismissRequest.Header.Set("Content-Type", "application/json")
	dismissRequest.Header.Set(desktopDeviceHeader, deviceID)
	dismissRequest.Header.Set(skillRecommendationHeader, CapSkillInstallRecommendationV1)
	dismissResponse := httptest.NewRecorder()
	s.Handler().ServeHTTP(dismissResponse, dismissRequest)
	if dismissResponse.Code != http.StatusOK {
		t.Fatalf("dismiss status=%d body=%s", dismissResponse.Code, dismissResponse.Body.String())
	}
	assertSemanticEqual(t, dismissFixture, parseJSONMap(t, dismissResponse.Body.Bytes()))
}

func TestWireFixture_HTTPDefaultSessionDetail(t *testing.T) {
	fixture := loadWireFixture(t, "http_get.session.response.json")
	dir := t.TempDir()
	deps := &ServerDeps{ShannonDir: dir, SessionCache: NewSessionCache(dir)}
	defer deps.SessionCache.CloseAll()
	mgr := deps.SessionCache.GetOrCreate("")
	sess := mgr.NewSessionWithID(fixture["id"].(string))
	sess.Title = fixture["title"].(string)
	sess.CWD = fixture["cwd"].(string)
	sess.Source = "desktop"
	sess.CreatedAt = time.Date(2026, 8, 5, 0, 0, 0, 0, time.UTC)
	sess.Messages = []client.Message{
		{Role: "user", Content: client.NewTextContent("ARCHIVE_ONLY_OLD_TASK")},
		{Role: "assistant", Content: client.NewTextContent("ARCHIVE_ONLY_OLD_REPLY")},
		{Role: "user", Content: client.NewTextContent("live tail question")},
	}
	firstTime := time.Date(2026, 8, 5, 0, 0, 1, 0, time.UTC)
	secondTime := time.Date(2026, 8, 5, 0, 0, 2, 0, time.UTC)
	tailTime := time.Date(2026, 8, 5, 0, 0, 3, 0, time.UTC)
	sess.MessageMeta = []session.MessageMeta{
		{Source: "desktop", Timestamp: &firstTime},
		{Source: "desktop", Timestamp: &secondTime},
		{Source: "desktop", Timestamp: &tailTime},
	}
	sess.CompactionCheckpoint = &session.CompactionCheckpoint{
		SchemaVersion:       session.CompactionCheckpointSchemaVersion,
		ArchiveThroughIndex: 2,
		Messages: []client.Message{
			{Role: "user", Content: client.NewTextContent("stable original primer")},
			{Role: "user", Content: client.NewTextContent("Previous context summary: stable state")},
			{Role: "assistant", Content: client.NewTextContent("compacted recent reply")},
		},
	}
	if err := mgr.Save(); err != nil {
		t.Fatalf("save fixture session: %v", err)
	}

	srv := NewServer(0, &Client{}, deps, "test")
	rec := httptest.NewRecorder()
	path := "/sessions/" + sess.ID
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d body=%s", path, rec.Code, rec.Body.String())
	}
	produced := parseJSONMap(t, rec.Body.Bytes())
	normalizeRFC3339(t, produced, fixture, "updated_at")
	assertSemanticEqual(t, fixture, produced)

	// Consumer-shaped decode pins the additive checkpoint while keeping the
	// default detail's top-level messages as the lossless archive.
	var detail struct {
		ID       string `json:"id"`
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
		MessageMeta []struct {
			Source    string `json:"source"`
			Timestamp string `json:"timestamp"`
		} `json:"message_meta"`
		CompactionCheckpoint *struct {
			SchemaVersion       int `json:"schema_version"`
			ArchiveThroughIndex int `json:"archive_through_index"`
			Messages            []struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		} `json:"compaction_checkpoint"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("consumer decode failed: %v", err)
	}
	cp := detail.CompactionCheckpoint
	if detail.ID != sess.ID || len(detail.Messages) != 3 || len(detail.MessageMeta) != 3 ||
		cp == nil || cp.SchemaVersion != 1 || cp.ArchiveThroughIndex != 2 || len(cp.Messages) != 3 {
		t.Fatalf("consumer decode lost default detail fields: %+v", detail)
	}
	if string(detail.Messages[0].Content) != `"ARCHIVE_ONLY_OLD_TASK"` ||
		string(cp.Messages[1].Content) != `"Previous context summary: stable state"` {
		t.Fatalf("archive/live checkpoint semantics drifted: archive=%s checkpoint=%s",
			detail.Messages[0].Content, cp.Messages[1].Content)
	}
}

func TestWireFixture_HTTPRemoteSessionTimeline(t *testing.T) {
	fixture := loadWireFixture(t, "http_get.session.remote_timeline.response.json")
	dir := t.TempDir()
	deps := &ServerDeps{ShannonDir: dir, SessionCache: NewSessionCache(dir)}
	mgr := deps.SessionCache.GetOrCreate("")
	sess := mgr.NewSession()
	sess.Title = fixture["title"].(string)
	sess.CWD = fixture["cwd"].(string)
	sess.CreatedAt = time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC)
	sess.Messages = []client.Message{
		{Role: "user", Content: client.NewTextContent("older message")},
		{Role: "assistant", Content: client.NewBlockContent([]client.ContentBlock{
			client.NewToolUseBlock("tool-1", "bash", json.RawMessage(`{"command":"pwd"}`)),
		})},
		{Role: "user", Content: client.NewBlockContent([]client.ContentBlock{
			client.NewToolResultBlock("tool-1", "/tmp/project", false),
		})},
	}
	firstMetaTime := time.Date(2026, 7, 15, 0, 0, 9, 0, time.UTC)
	toolTime := time.Date(2026, 7, 15, 0, 0, 10, 0, time.UTC)
	resultTime := time.Date(2026, 7, 15, 0, 0, 11, 0, time.UTC)
	sess.MessageMeta = []session.MessageMeta{
		{Source: "kocoro", Timestamp: &firstMetaTime},
		{Source: "kocoro", Timestamp: &toolTime},
		{Source: "kocoro", Timestamp: &resultTime},
	}
	if err := mgr.Save(); err != nil {
		t.Fatalf("save fixture session: %v", err)
	}

	srv := NewServer(0, &Client{}, deps, "test")
	rec := httptest.NewRecorder()
	path := "/sessions/" + sess.ID + "?view=remote_timeline&limit=2"
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d body=%s", path, rec.Code, rec.Body.String())
	}
	produced := parseJSONMap(t, rec.Body.Bytes())
	produced["id"] = fixture["id"]
	produced["created_at"] = fixture["created_at"]
	produced["updated_at"] = fixture["updated_at"]
	assertSemanticEqual(t, fixture, produced)

	// Consumer-shaped decode mirrors RemoteDaemonSessionDetail plus its paging
	// additions. This catches producer field renames independently of the fixture.
	var detail struct {
		PageVersion int `json:"page_version"`
		Messages    []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
		MessageMeta []struct {
			Source    string `json:"source"`
			Timestamp string `json:"timestamp"`
		} `json:"message_meta"`
		StartIndex          int    `json:"start_index"`
		TotalMessages       int    `json:"total_messages"`
		HasMore             bool   `json:"has_more"`
		NextCursor          string `json:"next_cursor"`
		OmittedContentCount int    `json:"omitted_content_count"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("consumer decode failed: %v", err)
	}
	if detail.PageVersion != 1 || len(detail.Messages) != 2 || len(detail.MessageMeta) != 2 ||
		detail.StartIndex != 1 || detail.TotalMessages != 3 || !detail.HasMore || detail.NextCursor != "1" {
		t.Fatalf("consumer decode lost timeline fields: %+v", detail)
	}
}

func TestWireFixture_HTTPAgents(t *testing.T) {
	listFixture := loadWireFixture(t, "http_get.agents.response.json")
	detailFixture := loadWireFixture(t, "http_get.agent_detail.response.json")

	agentsDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(agentsDir, "demo-agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	prompt := detailFixture["prompt"].(string)
	if err := os.WriteFile(filepath.Join(agentsDir, "demo-agent", "AGENT.md"), []byte(prompt), 0o644); err != nil {
		t.Fatal(err)
	}

	deps := &ServerDeps{AgentsDir: agentsDir, ShannonDir: t.TempDir()}
	srv := NewServer(0, nil, deps, "test")
	handler := srv.Handler()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/agents", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /agents = %d, body %s", rec.Code, rec.Body.String())
	}
	assertSemanticEqual(t, listFixture, parseJSONMap(t, rec.Body.Bytes()))

	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/agents/demo-agent", nil))
	if rec2.Code != http.StatusOK {
		t.Fatalf("GET /agents/demo-agent = %d, body %s", rec2.Code, rec2.Body.String())
	}
	assertSemanticEqual(t, detailFixture, parseJSONMap(t, rec2.Body.Bytes()))

	// Consumer-shaped decode pinning the historical field-name divergence:
	// list rows say `override`, the detail object says `overridden`. Both are
	// part of the live contract; neither side may "fix" one unilaterally.
	var list struct {
		Agents []struct {
			Name        string `json:"name"`
			DisplayName string `json:"display_name"`
			Builtin     bool   `json:"builtin"`
			Override    bool   `json:"override"`
		} `json:"agents"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("consumer decode (list) failed: %v", err)
	}
	if len(list.Agents) != 1 || list.Agents[0].Name != "demo-agent" {
		t.Fatalf("consumer decode (list) lost fields: %+v", list)
	}
	var detail struct {
		Name        string  `json:"name"`
		DisplayName string  `json:"display_name"`
		Prompt      string  `json:"prompt"`
		Memory      *string `json:"memory"`
		Builtin     bool    `json:"builtin"`
		Overridden  bool    `json:"overridden"`
	}
	if err := json.Unmarshal(rec2.Body.Bytes(), &detail); err != nil {
		t.Fatalf("consumer decode (detail) failed: %v", err)
	}
	if detail.Name != "demo-agent" || detail.Prompt != prompt || detail.Memory != nil {
		t.Fatalf("consumer decode (detail) lost fields: %+v", detail)
	}
}

// TestWireFixture_HTTPAgentDetailWithProfile pins the shape of GET
// /agents/{name} when the agent has a populated PROFILE.yaml. Covers all four
// new fields (category, description, guide_prompts, examples), the nested
// {code, label} category shape, and the ExampleTurn omitempty rules (user
// turns omit `markdown`/`tool_runs`, assistant turns omit `text`).
func TestWireFixture_HTTPAgentDetailWithProfile(t *testing.T) {
	fixture := loadWireFixture(t, "http_get.agent_detail.with_profile.response.json")

	agentsDir := t.TempDir()
	agentDir := filepath.Join(agentsDir, "profile-demo")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	prompt := fixture["prompt"].(string)
	if err := os.WriteFile(filepath.Join(agentDir, "AGENT.md"), []byte(prompt), 0o644); err != nil {
		t.Fatal(err)
	}
	// PROFILE.yaml must produce the exact JSON shape in the fixture. Any
	// drift here is a wire-contract change that breaks the Desktop side too —
	// the fixture is the single source of truth.
	profileYAML := `category: coding
description:
  en: A demo agent used by wire-fixture tests.
  zh-Hans: 用于线路 fixture 测试的演示智能体。
  ja: ワイヤフィクスチャテスト用のデモエージェント。
guide_prompts:
  - title:
      en: Find auth code
      zh-Hans: 找认证代码
      ja: 認証コードを探す
    prompt:
      en: Where is the authentication logic?
      zh-Hans: 认证逻辑在哪里？
      ja: 認証ロジックはどこ？
examples:
  - title:
      en: Sample dialog
      zh-Hans: 示例对话
      ja: サンプル対話
    turns:
      - role: user
        text:
          en: Hi.
          zh-Hans: 你好。
          ja: こんにちは。
      - role: assistant
        markdown:
          en: Hello! Let me look around.
          zh-Hans: 你好！让我看一下。
          ja: こんにちは！見てみます。
        tool_runs:
          - tool: grep
            summary:
              en: Searched src/ for entry points
              zh-Hans: 在 src/ 搜索入口点
              ja: src/ でエントリーポイントを検索
`
	if err := os.WriteFile(filepath.Join(agentDir, "PROFILE.yaml"), []byte(profileYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	deps := &ServerDeps{AgentsDir: agentsDir, ShannonDir: t.TempDir()}
	srv := NewServer(0, nil, deps, "test")
	handler := srv.Handler()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/agents/profile-demo", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /agents/profile-demo = %d, body %s", rec.Code, rec.Body.String())
	}
	assertSemanticEqual(t, fixture, parseJSONMap(t, rec.Body.Bytes()))

	// Consumer-shaped decode: pin the field path Desktop will hit. category
	// is a nested object with {code, label}; description / guide_prompts /
	// examples decode into client-shaped structs without losing locale keys.
	var detail struct {
		Name     string `json:"name"`
		Category *struct {
			Code  string            `json:"code"`
			Label map[string]string `json:"label"`
		} `json:"category"`
		Description  map[string]string `json:"description"`
		GuidePrompts []struct {
			Title  map[string]string `json:"title"`
			Prompt map[string]string `json:"prompt"`
		} `json:"guide_prompts"`
		Examples []struct {
			Title map[string]string `json:"title"`
			Turns []struct {
				Role     string            `json:"role"`
				Text     map[string]string `json:"text"`
				Markdown map[string]string `json:"markdown"`
				ToolRuns []struct {
					Tool    string            `json:"tool"`
					Summary map[string]string `json:"summary"`
				} `json:"tool_runs"`
			} `json:"turns"`
		} `json:"examples"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("consumer decode failed: %v", err)
	}
	if detail.Category == nil || detail.Category.Code != "coding" {
		t.Fatalf("category lost: %+v", detail.Category)
	}
	if detail.Category.Label["ja"] != "コーディング" {
		t.Errorf("category.label.ja=%q", detail.Category.Label["ja"])
	}
	if detail.Description["zh-Hans"] == "" {
		t.Errorf("description.zh-Hans empty")
	}
	if len(detail.GuidePrompts) != 1 || detail.GuidePrompts[0].Title["en"] != "Find auth code" {
		t.Errorf("guide_prompts decode: %+v", detail.GuidePrompts)
	}
	if len(detail.Examples) != 1 {
		t.Fatalf("examples len=%d", len(detail.Examples))
	}
	turns := detail.Examples[0].Turns
	if len(turns) != 2 {
		t.Fatalf("turns len=%d", len(turns))
	}
	if turns[0].Role != "user" || turns[0].Text["en"] != "Hi." {
		t.Errorf("turn 0: %+v", turns[0])
	}
	if turns[1].Role != "assistant" || turns[1].Markdown["en"] == "" {
		t.Errorf("turn 1 markdown: %+v", turns[1])
	}
	if len(turns[1].ToolRuns) != 1 || turns[1].ToolRuns[0].Tool != "grep" {
		t.Errorf("turn 1 tool_runs: %+v", turns[1].ToolRuns)
	}
}

// TestWireFixture_HTTPAgentsWithProfile pins the shape of GET /agents (list)
// when an agent ships a PROFILE.yaml with a description. The profile-less list
// fixture covers the omitted-description case; this one pins the localized
// `description` map that the front-side agent-resolver decodes off the list.
func TestWireFixture_HTTPAgentsWithProfile(t *testing.T) {
	fixture := loadWireFixture(t, "http_get.agents.with_profile.response.json")

	agentsDir := t.TempDir()
	agentDir := filepath.Join(agentsDir, "profile-demo")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "AGENT.md"), []byte("You are a demo agent.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Only `description` flows from PROFILE.yaml into the list row (avatar stays
	// ""); the JSON shape must match the committed fixture byte-for-field.
	profileYAML := `description:
  en: A demo agent used by wire-fixture tests.
  zh-Hans: 用于线路 fixture 测试的演示智能体。
  ja: ワイヤフィクスチャテスト用のデモエージェント。
`
	if err := os.WriteFile(filepath.Join(agentDir, "PROFILE.yaml"), []byte(profileYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	deps := &ServerDeps{AgentsDir: agentsDir, ShannonDir: t.TempDir()}
	srv := NewServer(0, nil, deps, "test")
	handler := srv.Handler()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/agents", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /agents = %d, body %s", rec.Code, rec.Body.String())
	}
	assertSemanticEqual(t, fixture, parseJSONMap(t, rec.Body.Bytes()))

	// Consumer-shaped decode: the front-side resolver reads `description` as a
	// locale→text map off each list row.
	var list struct {
		Agents []struct {
			Name        string            `json:"name"`
			Description map[string]string `json:"description"`
		} `json:"agents"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("consumer decode failed: %v", err)
	}
	if len(list.Agents) != 1 || list.Agents[0].Name != "profile-demo" {
		t.Fatalf("consumer decode lost fields: %+v", list)
	}
	if list.Agents[0].Description["en"] != "A demo agent used by wire-fixture tests." {
		t.Errorf("description.en=%q", list.Agents[0].Description["en"])
	}
	if list.Agents[0].Description["zh-Hans"] == "" {
		t.Errorf("description.zh-Hans empty")
	}
}

// TestWireFixture_HTTPAgentDetailWithAvatar pins the shape of GET
// /agents/{name} when the agent has a PROFILE.yaml containing only avatar and
// category (minimal profile). Verifies that avatar is propagated through
// LoadAgent → ToAPI() → HTTP response and matches the committed fixture.
func TestWireFixture_HTTPAgentDetailWithAvatar(t *testing.T) {
	fixture := loadWireFixture(t, "http_get.agent_detail.with_avatar.response.json")

	agentsDir := t.TempDir()
	agentDir := filepath.Join(agentsDir, "avatar-demo")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	prompt := fixture["prompt"].(string)
	if err := os.WriteFile(filepath.Join(agentDir, "AGENT.md"), []byte(prompt), 0o644); err != nil {
		t.Fatal(err)
	}
	profileYAML := `category: coding
avatar: https://cdn.example.com/a.png
description:
  en: Demo
`
	if err := os.WriteFile(filepath.Join(agentDir, "PROFILE.yaml"), []byte(profileYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	deps := &ServerDeps{AgentsDir: agentsDir, ShannonDir: t.TempDir()}
	srv := NewServer(0, nil, deps, "test")
	handler := srv.Handler()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/agents/avatar-demo", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /agents/avatar-demo = %d, body %s", rec.Code, rec.Body.String())
	}
	produced := parseJSONMap(t, rec.Body.Bytes())
	assertSemanticEqual(t, fixture, produced)

	// Consumer-shaped decode: pin that avatar reaches Desktop as a string.
	var detail struct {
		Name   string `json:"name"`
		Avatar string `json:"avatar"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("consumer decode failed: %v", err)
	}
	if detail.Avatar != "https://cdn.example.com/a.png" {
		t.Fatalf("avatar=%q, want cdn url", detail.Avatar)
	}
}

// --- Quick-panel surfaces (POST /local/screenshot/window + foreground_hint) --

// TestWireFixture_ScreenshotWindowRequest decodes the request fixture through
// the real screenshotWindowRequest struct and asserts every field survives
// round-trip unmarshal. The fixture represents the POST body Desktop sends.
func TestWireFixture_ScreenshotWindowRequest(t *testing.T) {
	fixture := loadWireFixture(t, "local_screenshot_window_request.json")

	raw, err := json.Marshal(fixture)
	if err != nil {
		t.Fatalf("re-marshal fixture: %v", err)
	}
	var req screenshotWindowRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		t.Fatalf("consumer decode failed: %v", err)
	}
	if req.PID != 1234 {
		t.Fatalf("pid=%d, want 1234", req.PID)
	}
	if req.AppName != "WeChat" {
		t.Fatalf("app_name=%q, want WeChat", req.AppName)
	}
	// window_title is present with empty value — omitempty means it is
	// omitted on re-encode, which is acceptable (empty string == absent).

	// Consumer-side assertion: Desktop sends pid+app_name together; either is
	// sufficient for the handler, but the fixture has both.
	if req.PID <= 0 && req.AppName == "" {
		t.Fatal("fixture must supply at least pid or app_name")
	}
}

// TestWireFixture_ScreenshotWindowDenied drives POST /local/screenshot/window
// through the real handler with a mock ax_server returning
// screen_recording_denied, and asserts the HTTP 403 body matches the fixture.
func TestWireFixture_ScreenshotWindowDenied(t *testing.T) {
	fixture := loadWireFixture(t, "local_screenshot_window_denied.json")

	// Install a seam override that simulates ax_server denying Screen Recording.
	orig := captureWindowVia
	captureWindowVia = func(_ context.Context, _ map[string]any) (json.RawMessage, error) {
		return json.Marshal(captureWindowResult{OK: false, Code: "screen_recording_denied"})
	}
	defer func() { captureWindowVia = orig }()

	srv := NewServer(0, nil, nil, "test")
	body := strings.NewReader(`{"pid":1234,"app_name":"WeChat"}`)
	req := httptest.NewRequest(http.MethodPost, "/local/screenshot/window", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d, want 403", rec.Code)
	}
	produced := parseJSONMap(t, rec.Body.Bytes())
	assertSemanticEqual(t, fixture, produced)

	// Consumer-shaped decode: Desktop keys localisation on `code`.
	var errResp struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("consumer decode failed: %v", err)
	}
	if errResp.Code != "screen_recording_denied" || errResp.Error == "" {
		t.Fatalf("consumer decode lost fields: %+v", errResp)
	}
}

// TestWireFixture_ScreenshotWindowSuccess drives POST /local/screenshot/window
// through the real handler with a mock ax_server returning a successful capture,
// and asserts the HTTP 200 body matches the fixture. Also decodes the body into
// consumer-shaped struct to anchor all three key names (image_base64/width/height).
func TestWireFixture_ScreenshotWindowSuccess(t *testing.T) {
	fixture := loadWireFixture(t, "local_screenshot_window_success.json")

	orig := captureWindowVia
	captureWindowVia = func(_ context.Context, _ map[string]any) (json.RawMessage, error) {
		return json.Marshal(captureWindowResult{OK: true, ImageBase64: "AAAA", Width: 100, Height: 50})
	}
	defer func() { captureWindowVia = orig }()

	srv := NewServer(0, nil, nil, "test")
	body := strings.NewReader(`{"pid":1234,"app_name":"WeChat"}`)
	req := httptest.NewRequest(http.MethodPost, "/local/screenshot/window", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	produced := parseJSONMap(t, rec.Body.Bytes())
	assertSemanticEqual(t, fixture, produced)

	// Consumer-shaped decode: Desktop CaptureWindowResult keys on image_base64/width/height.
	var result struct {
		ImageBase64 string `json:"image_base64"`
		Width       int    `json:"width"`
		Height      int    `json:"height"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("consumer decode failed: %v", err)
	}
	if result.ImageBase64 != "AAAA" {
		t.Fatalf("image_base64=%q, want AAAA", result.ImageBase64)
	}
	if result.Width != 100 {
		t.Fatalf("width=%d, want 100", result.Width)
	}
	if result.Height != 50 {
		t.Fatalf("height=%d, want 50", result.Height)
	}
}

// TestWireFixture_MessageForegroundHintRequest decodes the request fixture
// through the real RunAgentRequest struct and asserts the foreground_hint
// sub-object round-trips correctly. The fixture represents the POST /message
// body Desktop sends from the quick panel.
func TestWireFixture_MessageForegroundHintRequest(t *testing.T) {
	fixture := loadWireFixture(t, "message_foreground_hint_request.json")

	raw, err := json.Marshal(fixture)
	if err != nil {
		t.Fatalf("re-marshal fixture: %v", err)
	}
	var req RunAgentRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		t.Fatalf("consumer decode failed: %v", err)
	}
	if req.Text != "summarize what I'm looking at" {
		t.Fatalf("text=%q", req.Text)
	}
	if req.Source != "kocoro" {
		t.Fatalf("source=%q, want kocoro", req.Source)
	}
	if !req.NewSession {
		t.Fatal("new_session must be true in fixture")
	}
	if req.ForegroundHint == nil {
		t.Fatal("foreground_hint missing after decode")
	}
	h := req.ForegroundHint
	if h.PID != 1234 {
		t.Fatalf("foreground_hint.pid=%d, want 1234", h.PID)
	}
	if h.AppName != "WeChat" {
		t.Fatalf("foreground_hint.app_name=%q, want WeChat", h.AppName)
	}
	if h.BundleID != "com.tencent.xinWeChat" {
		t.Fatalf("foreground_hint.bundle_id=%q, want com.tencent.xinWeChat", h.BundleID)
	}

	// Re-encode through the production struct and compare semantically: this
	// catches any json tag rename on the producer side (e.g. pid→window_pid).
	reEncoded, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("re-encode failed: %v", err)
	}
	produced := parseJSONMap(t, reEncoded)
	// RunAgentRequest has json:"-" fields that won't appear; compare only the
	// wire-visible fields from the fixture. We check sub-map equality manually.
	fh, _ := fixture["foreground_hint"].(map[string]any)
	ph, _ := produced["foreground_hint"].(map[string]any)
	if fh == nil || ph == nil {
		t.Fatalf("foreground_hint missing in fixture=%v or produced=%v", fh, ph)
	}
	if !reflect.DeepEqual(fh, ph) {
		fj, _ := json.MarshalIndent(fh, "", "  ")
		pj, _ := json.MarshalIndent(ph, "", "  ")
		t.Fatalf("foreground_hint drifted\n--- fixture ---\n%s\n--- produced ---\n%s", fj, pj)
	}
}

// TestWireFixture_MessageIdempotencyRequest pins the request body used for a
// crash-safe idempotent handoff. It crosses the real RunAgentRequest JSON
// tags and validation seam; Desktop separately emits the same vendored bytes
// through its production request builder.
func TestWireFixture_MessageIdempotencyRequest(t *testing.T) {
	fixture := loadWireFixture(t, "message_idempotency_request.json")

	raw, err := json.Marshal(fixture)
	if err != nil {
		t.Fatalf("re-marshal fixture: %v", err)
	}
	var req RunAgentRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		t.Fatalf("request decode failed: %v", err)
	}
	if err := req.Validate(); err != nil {
		t.Fatalf("request validation failed: %v", err)
	}
	if req.SessionID != "12345678" {
		t.Fatalf("session_id=%q", req.SessionID)
	}
	if req.IdempotencyKey != "job-12345678" {
		t.Fatalf("idempotency_key=%q", req.IdempotencyKey)
	}
	if !req.NewSession || req.Source != "desktop" || req.Channel != "shanclaw" {
		t.Fatalf("routing fields drifted: %+v", req)
	}

	reEncoded, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("re-encode failed: %v", err)
	}
	assertSemanticEqual(t, fixture, parseJSONMap(t, reEncoded))
}

// TestWireFixture_MessageKoeExecutionFastRequest pins the source=koe fast
// request: the semantic execution_mode/requested_execution_mode claim plus
// client-minted lineage ids. The wire deliberately carries NO provider, model,
// or profile fields — the daemon re-decides admission and resolves the profile.
func TestWireFixture_MessageKoeExecutionFastRequest(t *testing.T) {
	fixture := loadWireFixture(t, "message_koe_execution_fast_request.json")

	raw, err := json.Marshal(fixture)
	if err != nil {
		t.Fatalf("re-marshal fixture: %v", err)
	}
	var req RunAgentRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		t.Fatalf("request decode failed: %v", err)
	}
	if err := req.Validate(); err != nil {
		t.Fatalf("request validation failed: %v", err)
	}
	if req.Source != "koe" || req.ExecutionMode != executionprofile.ModeFast {
		t.Fatalf("source=%q mode=%q, want koe/fast", req.Source, req.ExecutionMode)
	}
	if req.RequestedExecutionMode == nil || *req.RequestedExecutionMode != "fast" {
		t.Fatalf("requested_execution_mode = %v, want fast", req.RequestedExecutionMode)
	}
	if req.LogicalTaskID != "burst-4f2a:t01" || req.ExecutionRunID != "burst-4f2a:t01.r01" {
		t.Fatalf("lineage ids = %q / %q", req.LogicalTaskID, req.ExecutionRunID)
	}
	// json:"-" injection surfaces must stay zero even if a hostile client
	// added them; the fixture omits them, so this pins the decode default.
	if !req.ExecutionRun.IsZero() {
		t.Fatalf("execution_run decoded from wire: %+v", req.ExecutionRun)
	}

	// Producer side: the koe-visible execution fields survive re-encoding
	// through the production struct with the same tags and values.
	produced := parseJSONMap(t, []byte(mustJSON(req)))
	for _, key := range []string{"execution_mode", "requested_execution_mode", "logical_task_id", "execution_run_id"} {
		if produced[key] != fixture[key] {
			t.Fatalf("field %q drifted: fixture=%v produced=%v", key, fixture[key], produced[key])
		}
	}
}

// TestWireFixture_MessageKoeExecutionFullRequest pins the full-mode follow-up:
// full_reason (closed vocabulary), parent lineage, and the untrusted
// inherited_execution_mode claim (admission clears it — only validation
// against the execution ledger may restore the Full floor).
func TestWireFixture_MessageKoeExecutionFullRequest(t *testing.T) {
	fixture := loadWireFixture(t, "message_koe_execution_full_request.json")

	raw, err := json.Marshal(fixture)
	if err != nil {
		t.Fatalf("re-marshal fixture: %v", err)
	}
	var req RunAgentRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		t.Fatalf("request decode failed: %v", err)
	}
	if err := req.Validate(); err != nil {
		t.Fatalf("request validation failed: %v", err)
	}
	if req.ExecutionMode != executionprofile.ModeFull ||
		req.FullReason != executionprofile.FullReasonSecurityPermissions {
		t.Fatalf("mode=%q full_reason=%q", req.ExecutionMode, req.FullReason)
	}
	if req.InheritedMode != executionprofile.ModeFull {
		t.Fatalf("inherited_execution_mode = %q, want full", req.InheritedMode)
	}
	if req.ParentRunID != "burst-4f2a:t02.r01" {
		t.Fatalf("parent_run_id = %q", req.ParentRunID)
	}

	produced := parseJSONMap(t, []byte(mustJSON(req)))
	for _, key := range []string{"execution_mode", "full_reason", "inherited_execution_mode", "logical_task_id", "execution_run_id", "parent_run_id"} {
		if produced[key] != fixture[key] {
			t.Fatalf("field %q drifted: fixture=%v produced=%v", key, fixture[key], produced[key])
		}
	}
}

// TestWireFixture_DoneWithExecutionRun pins the `event: done` payload carrying
// the validated execution run (lineage ids + the resolved kfp1 profile).
// handleMessageSSE marshals *RunAgentResult directly, so serializing the
// producer type IS the production path; Koe's ledger is the consumer.
func TestWireFixture_DoneWithExecutionRun(t *testing.T) {
	fixture := loadWireFixture(t, "sse_event.done.with_execution_run.json")

	result := &RunAgentResult{
		Reply:     fixture["reply"].(string),
		SessionID: fixture["session_id"].(string),
		Agent:     fixture["agent"].(string),
		Usage: RunAgentUsage{
			InputTokens: 8120, OutputTokens: 240, TotalTokens: 8360, CostUSD: 0.021, WebSearchCalls: 1,
		},
		ExecutionRun: &executionprofile.Run{
			LogicalTaskID: "burst-4f2a:t01",
			RunID:         "burst-4f2a:t01.r01",
			Profile:       fastProfileForDaemonTest(),
		},
	}
	if err := result.ExecutionRun.ValidatePersisted(); err != nil {
		t.Fatalf("fixture run must satisfy the persisted contract: %v", err)
	}
	raw := []byte(mustJSON(result))
	produced := parseJSONMap(t, raw)
	assertSemanticEqual(t, fixture, produced)

	// Consumer shape: Koe decodes execution_run into executionprofile.Run for
	// ledger recording; pin the key fields it routes on.
	var done struct {
		Reply        string                `json:"reply"`
		Usage        RunAgentUsage         `json:"usage"`
		ExecutionRun *executionprofile.Run `json:"execution_run"`
	}
	if err := json.Unmarshal(raw, &done); err != nil {
		t.Fatalf("consumer decode failed: %v", err)
	}
	if done.Usage.WebSearchCalls != 1 ||
		done.ExecutionRun == nil ||
		done.ExecutionRun.RunID != "burst-4f2a:t01.r01" ||
		!done.ExecutionRun.Profile.IsFast() ||
		done.ExecutionRun.Profile.ResolutionReason != "cloud_profile_resolved" {
		t.Fatalf("consumer decode lost execution_run fields: %+v", done.ExecutionRun)
	}
}
