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
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/memory"
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
			InputTokens:  18432,
			OutputTokens: 956,
			TotalTokens:  19388,
			CostUSD:      0.0712,
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
			InputTokens  int     `json:"input_tokens"`
			OutputTokens int     `json:"output_tokens"`
			TotalTokens  int     `json:"total_tokens"`
			CostUSD      float64 `json:"cost_usd"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &done); err != nil {
		t.Fatalf("consumer decode failed: %v", err)
	}
	if done.Reply == "" || done.Usage.TotalTokens != 19388 {
		t.Fatalf("consumer decode lost fields: %+v", done)
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
	if status.Memory == nil || status.Memory.Provider != "disabled" || status.Memory.Reason != nil {
		t.Fatalf("memory block decode mismatch: %+v", status.Memory)
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
