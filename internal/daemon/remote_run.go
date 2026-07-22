package daemon

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/agents"
)

type remoteRunState struct {
	cancel context.CancelFunc
	broker *ApprovalBroker
}

const (
	localPresenceEnv       = "KOCORO_LOCAL_PRESENCE_TOKEN"
	localPresenceHeader    = "X-Kocoro-Local-Presence"
	remoteRunOutboxLimit   = 500
	remoteRunOutboxTTL     = 10 * time.Minute
	maxRemoteRunEventBytes = maxRemoteResponseBodyBytes
)

type remoteRunEventRecord struct {
	seq   int64
	event RemoteRunEvent
}

func (s *Server) handleRemotePairingCode(w http.ResponseWriter, r *http.Request) {
	if !localPresenceAuthorized(r) {
		writeError(w, http.StatusForbidden, "local presence confirmation required")
		return
	}
	if s.client == nil {
		writeError(w, http.StatusServiceUnavailable, "cloud websocket unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()
	resp, err := s.client.RequestPairingCode(ctx)
	if err != nil {
		status, message := remotePairingError(err)
		writeError(w, status, message)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func localPresenceAuthorized(r *http.Request) bool {
	expected := strings.TrimSpace(os.Getenv(localPresenceEnv))
	actual := strings.TrimSpace(r.Header.Get(localPresenceHeader))
	if expected == "" || actual == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(actual), []byte(expected)) == 1
}

func (s *Server) handleRemotePairings(w http.ResponseWriter, r *http.Request) {
	if !localPresenceAuthorized(r) {
		writeError(w, http.StatusForbidden, "local presence confirmation required")
		return
	}
	if s.client == nil {
		writeError(w, http.StatusServiceUnavailable, "cloud websocket unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()
	resp, err := s.client.RequestRemotePairings(ctx)
	if err != nil {
		status, message := remotePairingsError(err)
		writeError(w, status, message)
		return
	}
	if resp.Controllers == nil {
		resp.Controllers = []RemotePairingController{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *Server) handleRemoteRevoke(w http.ResponseWriter, r *http.Request) {
	if !localPresenceAuthorized(r) {
		writeError(w, http.StatusForbidden, "local presence confirmation required")
		return
	}
	if s.client == nil {
		writeError(w, http.StatusServiceUnavailable, "cloud websocket unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()
	resp, err := s.client.RequestRemoteHostRevoke(ctx)
	if err != nil {
		status, message := remoteRevokeError(err)
		writeError(w, status, message)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func remotePairingError(err error) (int, string) {
	if errors.Is(err, context.DeadlineExceeded) {
		return http.StatusBadGateway, "cloud did not respond to the remote pairing request; make sure the configured Cloud endpoint is running the remote-control backend"
	}
	if errors.Is(err, context.Canceled) {
		return http.StatusBadGateway, "remote pairing request was cancelled before Cloud responded"
	}
	return http.StatusBadGateway, err.Error()
}

func remotePairingsError(err error) (int, string) {
	if errors.Is(err, context.DeadlineExceeded) {
		return http.StatusBadGateway, "cloud did not respond to the remote pairings request; make sure the configured Cloud endpoint is running the remote-control backend"
	}
	if errors.Is(err, context.Canceled) {
		return http.StatusBadGateway, "remote pairings request was cancelled before Cloud responded"
	}
	return http.StatusBadGateway, err.Error()
}

func remoteRevokeError(err error) (int, string) {
	if errors.Is(err, context.DeadlineExceeded) {
		return http.StatusBadGateway, "cloud did not respond to the remote revoke request; make sure the configured Cloud endpoint is running the remote-control backend"
	}
	if errors.Is(err, context.Canceled) {
		return http.StatusBadGateway, "remote revoke request was cancelled before Cloud responded"
	}
	return http.StatusBadGateway, err.Error()
}

func (s *Server) HandleRemoteRunRequest(parent context.Context, req RemoteRunRequest) {
	if s == nil || s.client == nil {
		return
	}
	runID := strings.TrimSpace(req.RunID)
	if runID == "" {
		return
	}
	if s.deps == nil {
		s.sendRemoteRunEvent(RemoteRunEvent{RunID: runID, Type: "error", Payload: rawJSON(map[string]string{"error": "agent dependencies unavailable"})})
		return
	}
	text := strings.TrimSpace(req.Text)
	if text == "" && len(req.Content) == 0 && len(req.Files) == 0 {
		s.sendRemoteRunEvent(RemoteRunEvent{RunID: runID, Type: "error", Payload: rawJSON(map[string]string{"error": "text or attachments are required"})})
		return
	}
	if text == "" {
		text = "Please review the attached file."
	}
	inlineImages := 0
	for _, block := range req.Content {
		if block.Type == "image" {
			inlineImages++
		}
	}
	log.Printf("daemon: remote run request run_id=%s content_blocks=%d inline_images=%d files=%d", runID, len(req.Content), inlineImages, len(req.Files))
	runReq := RunAgentRequest{
		Text:            text,
		Content:         req.Content,
		SessionID:       req.SessionID,
		Agent:           req.Agent,
		NewSession:      req.NewSession,
		ClientMessageID: req.ClientMessageID,
		Source:          "ios_remote",
		Files:           req.Files,
	}
	runReq.EnsureRouteKey()
	switch s.injectIntoActiveRun(parent, runReq) {
	case InjectOK:
		_ = s.sendRemoteRunEvent(RemoteRunEvent{
			RunID:     runID,
			Type:      "injected",
			SessionID: req.SessionID,
			Payload: rawJSON(map[string]string{
				"route":      runReq.RouteKey,
				"session_id": req.SessionID,
			}),
		})
		return
	case InjectQueueFull:
		s.sendRemoteRunEvent(RemoteRunEvent{RunID: runID, Type: "error", Payload: rawJSON(map[string]string{"error": "active run injection queue is full"})})
		return
	case InjectBusy:
		s.sendRemoteRunEvent(RemoteRunEvent{RunID: runID, Type: "error", Payload: rawJSON(map[string]string{"error": "active run is not ready for injection"})})
		return
	case InjectCWDConflict:
		s.sendRemoteRunEvent(RemoteRunEvent{RunID: runID, Type: "error", Payload: rawJSON(map[string]string{"error": "cannot change working directory while a run is active"})})
		return
	case InjectRetracted:
		_ = s.sendRemoteRunEvent(RemoteRunEvent{RunID: runID, Type: "cancelled"})
		return
	case InjectNoActiveRun:
	}
	ctx, cancel := context.WithCancel(parent)
	broker := NewApprovalBroker(func(areq ApprovalRequest) error {
		return s.sendRemoteRunEvent(RemoteRunEvent{
			RunID:     runID,
			Type:      "approval_requested",
			SessionID: areq.SessionID,
			RequestID: areq.RequestID,
			Payload:   rawJSON(areq),
		})
	})
	broker.SetOnCleanup(func(requestID string) {
		_ = s.sendRemoteRunEvent(RemoteRunEvent{
			RunID:     runID,
			Type:      "approval_resolved",
			RequestID: requestID,
			Payload: rawJSON(map[string]string{
				"request_id":  requestID,
				"decision":    string(DecisionDeny),
				"resolved_by": "daemon_cleanup",
			}),
		})
	})
	state := &remoteRunState{cancel: cancel, broker: broker}
	if _, loaded := s.remoteRuns.LoadOrStore(runID, state); loaded {
		cancel()
		s.sendRemoteRunEvent(RemoteRunEvent{RunID: runID, Type: "error", Payload: rawJSON(map[string]string{"error": "run already exists"})})
		return
	}
	releaseSlot := s.acquireRemoteRunSlot()
	if releaseSlot == nil {
		s.remoteRuns.Delete(runID)
		broker.CancelAll()
		cancel()
		s.sendRemoteRunEvent(RemoteRunEvent{RunID: runID, Type: "error", Payload: rawJSON(map[string]string{"error": "too many remote runs are already active"})})
		return
	}
	go func() {
		defer releaseSlot()
		defer s.remoteRuns.Delete(runID)
		defer broker.CancelAll()
		defer cancel()

		cfg, _, _ := s.deps.Snapshot()
		autoApprove := cfg != nil && cfg.Daemon.AutoApprove
		if req.Agent != "" {
			if a, err := agents.LoadAgent(s.deps.AgentsDir, req.Agent); err == nil && a.Config != nil && a.Config.AutoApprove != nil {
				autoApprove = *a.Config.AutoApprove
			}
		}

		_ = s.sendRemoteRunEvent(RemoteRunEvent{RunID: runID, Type: "run_started"})
		handler := &remoteRunEventHandler{
			server:      s,
			runID:       runID,
			broker:      broker,
			ctx:         ctx,
			agent:       req.Agent,
			source:      "ios_remote",
			autoApprove: autoApprove,
		}
		result, err := RunAgent(ctx, s.deps, runReq, handler)
		if ctx.Err() != nil {
			_ = s.sendRemoteRunEvent(RemoteRunEvent{RunID: runID, Type: "cancelled"})
			return
		}
		if err != nil {
			_ = s.sendRemoteRunEvent(RemoteRunEvent{RunID: runID, Type: "error", Payload: rawJSON(map[string]string{"error": FriendlyAgentError(err)})})
			return
		}
		_ = s.sendRemoteRunEvent(RemoteRunEvent{
			RunID:     runID,
			Type:      "done",
			SessionID: result.SessionID,
			Payload:   rawJSON(result),
		})
	}()
}

func (s *Server) acquireRemoteRunSlot() func() {
	if s == nil || s.remoteRunSlots == nil {
		return func() {}
	}
	select {
	case s.remoteRunSlots <- struct{}{}:
		return func() { <-s.remoteRunSlots }
	default:
		return nil
	}
}

func (s *Server) HandleRemoteRunCancel(req RemoteRunCancel) {
	if v, ok := s.remoteRuns.Load(req.RunID); ok {
		v.(*remoteRunState).cancel()
		return
	}
	_ = s.sendRemoteRunEvent(RemoteRunEvent{RunID: req.RunID, Type: "error", Payload: rawJSON(map[string]string{"error": "run not found"})})
}

func (s *Server) HandleRemoteApprovalResponse(resp RemoteApprovalResponse) {
	v, ok := s.remoteRuns.Load(resp.RunID)
	if !ok {
		return
	}
	state := v.(*remoteRunState)
	resolvedBy := resp.ResolvedBy
	if resolvedBy == "" {
		resolvedBy = "ios"
	}
	state.broker.Resolve(resp.RequestID, resp.Decision, func() {
		_ = s.sendRemoteRunEvent(RemoteRunEvent{
			RunID:     resp.RunID,
			Type:      "approval_resolved",
			RequestID: resp.RequestID,
			Payload: rawJSON(map[string]string{
				"request_id":  resp.RequestID,
				"decision":    string(resp.Decision),
				"resolved_by": resolvedBy,
			}),
		})
	})
}

func (s *Server) sendRemoteRunEvent(evt RemoteRunEvent) error {
	if s == nil {
		return nil
	}
	evt = s.prepareRemoteRunEvent(evt)
	if remoteRunEventTerminal(evt.Type) {
		s.scheduleRemoteRunOutboxCleanup(evt.RunID)
	}
	if s.client == nil {
		return nil
	}
	if err := s.client.SendRemoteRunEvent(evt); err != nil {
		log.Printf("daemon: remote_run_event failed run=%s type=%s: %v", evt.RunID, evt.Type, err)
		return err
	}
	return nil
}

func (s *Server) prepareRemoteRunEvent(evt RemoteRunEvent) RemoteRunEvent {
	if s == nil || strings.TrimSpace(evt.RunID) == "" {
		return evt
	}
	if evt.Seq == 0 {
		counter, _ := s.remoteRunSeqs.LoadOrStore(evt.RunID, &atomic.Int64{})
		evt.Seq = counter.(*atomic.Int64).Add(1)
	}
	evt = constrainRemoteRunEvent(evt)
	s.rememberRemoteRunEvent(evt)
	return evt
}

func (s *Server) rememberRemoteRunEvent(evt RemoteRunEvent) {
	if evt.RunID == "" || evt.Seq <= 0 {
		return
	}
	s.remoteRunOutboxMu.Lock()
	defer s.remoteRunOutboxMu.Unlock()
	var records []remoteRunEventRecord
	if raw, ok := s.remoteRunOutbox.Load(evt.RunID); ok {
		records = append(records, raw.([]remoteRunEventRecord)...)
	}
	records = append(records, remoteRunEventRecord{seq: evt.Seq, event: evt})
	if len(records) > remoteRunOutboxLimit {
		records = records[len(records)-remoteRunOutboxLimit:]
	}
	s.remoteRunOutbox.Store(evt.RunID, records)
}

func (s *Server) ReplayRemoteRunEvents() {
	if s == nil || s.client == nil {
		return
	}
	events := make([]RemoteRunEvent, 0)
	s.remoteRunOutboxMu.Lock()
	s.remoteRunOutbox.Range(func(_, value any) bool {
		for _, record := range value.([]remoteRunEventRecord) {
			events = append(events, record.event)
		}
		return true
	})
	s.remoteRunOutboxMu.Unlock()
	sort.Slice(events, func(i, j int) bool {
		if events[i].RunID == events[j].RunID {
			return events[i].Seq < events[j].Seq
		}
		return events[i].RunID < events[j].RunID
	})
	for _, evt := range events {
		if err := s.client.SendRemoteRunEvent(evt); err != nil {
			log.Printf("daemon: remote_run_event replay failed run=%s seq=%d type=%s: %v", evt.RunID, evt.Seq, evt.Type, err)
			return
		}
	}
}

func (s *Server) scheduleRemoteRunOutboxCleanup(runID string) {
	if s == nil || runID == "" {
		return
	}
	time.AfterFunc(remoteRunOutboxTTL, func() {
		s.remoteRunOutboxMu.Lock()
		s.remoteRunOutbox.Delete(runID)
		s.remoteRunOutboxMu.Unlock()
		s.remoteRunSeqs.Delete(runID)
	})
}

func remoteRunEventTerminal(typ string) bool {
	switch typ {
	case "done", "error", "cancelled", "injected":
		// "injected" is terminal for the submitting run_id: the actual follow-up
		// output is emitted under the active absorbing run_id.
		return true
	default:
		return false
	}
}

func constrainRemoteRunEvent(evt RemoteRunEvent) RemoteRunEvent {
	if remoteRunEventSize(evt) <= maxRemoteRunEventBytes {
		return evt
	}
	if evt.Type == "done" {
		evt = omitRemoteRunDoneReply(evt)
		if remoteRunEventSize(evt) <= maxRemoteRunEventBytes {
			return evt
		}
	}
	log.Printf("daemon: remote_run_event payload trimmed run=%s seq=%d type=%s bytes>%d", evt.RunID, evt.Seq, evt.Type, maxRemoteRunEventBytes)
	evt.Payload = rawJSON(map[string]any{
		"payload_omitted": true,
		"reason":          "remote run event exceeded size limit",
	})
	if remoteRunEventSize(evt) > maxRemoteRunEventBytes {
		evt.Payload = nil
	}
	return evt
}

func remoteRunEventSize(evt RemoteRunEvent) int {
	data, err := json.Marshal(evt)
	if err != nil {
		return maxRemoteRunEventBytes + 1
	}
	return len(data)
}

func omitRemoteRunDoneReply(evt RemoteRunEvent) RemoteRunEvent {
	if len(evt.Payload) == 0 {
		return evt
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(evt.Payload, &payload); err != nil {
		return evt
	}
	replyBytes := 0
	if raw, ok := payload["reply"]; ok {
		replyBytes = len(raw)
		delete(payload, "reply")
		payload["reply_omitted"] = json.RawMessage(`true`)
		payload["reply_json_bytes"] = rawJSON(replyBytes)
	}
	evt.Payload = rawJSON(payload)
	if replyBytes > 0 {
		log.Printf("daemon: remote_run_event done reply omitted run=%s seq=%d reply_json_bytes=%d", evt.RunID, evt.Seq, replyBytes)
	}
	return evt
}

type remoteRunEventHandler struct {
	server      *Server
	runID       string
	broker      *ApprovalBroker
	ctx         context.Context
	agent       string
	source      string
	sessionID   string
	usage       agent.UsageAccumulator
	autoApprove bool
}

// IsUnattendedRun reports whether this remote run has explicitly disabled its
// approval round-trip. Source classification independently covers schedules
// and non-interactive IM channels.
func (h *remoteRunEventHandler) IsUnattendedRun() bool { return h.autoApprove }

func (h *remoteRunEventHandler) SetSessionID(id string) {
	h.sessionID = id
	_ = h.server.sendRemoteRunEvent(RemoteRunEvent{
		RunID:     h.runID,
		Type:      "session_started",
		SessionID: id,
		Payload:   rawJSON(map[string]string{"session_id": id}),
	})
}

func (h *remoteRunEventHandler) Usage() agent.AccumulatedUsage { return h.usage.Snapshot() }

func (h *remoteRunEventHandler) OnToolCall(name string, args string, toolUseID string) {
	_ = h.server.sendRemoteRunEvent(RemoteRunEvent{
		RunID:     h.runID,
		Type:      "tool",
		SessionID: h.sessionID,
		Payload: rawJSON(map[string]any{
			"tool":        name,
			"tool_use_id": toolUseID,
			"status":      "running",
			"args":        redactAndTruncate(args, 200),
		}),
	})
}

func (h *remoteRunEventHandler) OnToolResult(name string, args string, toolUseID string, result agent.ToolResult, elapsed time.Duration) {
	_ = h.server.sendRemoteRunEvent(RemoteRunEvent{
		RunID:     h.runID,
		Type:      "tool",
		SessionID: h.sessionID,
		Payload: rawJSON(map[string]any{
			"tool":        name,
			"tool_use_id": toolUseID,
			"status":      "completed",
			"elapsed":     elapsed.Seconds(),
			"is_error":    result.IsError,
			"preview":     redactAndTruncate(toolResultPreview(result), 200),
		}),
	})
}

func (h *remoteRunEventHandler) OnText(text string) {}
func (h *remoteRunEventHandler) OnPreamble(text string) {
	if text == "" {
		return
	}
	_ = h.server.sendRemoteRunEvent(RemoteRunEvent{
		RunID:     h.runID,
		Type:      EventAssistantText,
		SessionID: h.sessionID,
		Payload:   rawJSON(map[string]string{"text": text}),
	})
}

func (h *remoteRunEventHandler) OnStreamDelta(delta string) {
	_ = h.server.sendRemoteRunEvent(RemoteRunEvent{
		RunID:     h.runID,
		Type:      "delta",
		SessionID: h.sessionID,
		Payload:   rawJSON(map[string]string{"text": delta}),
	})
}

func (h *remoteRunEventHandler) OnUsage(usage agent.TurnUsage) {
	h.usage.Add(usage)
	_ = h.server.sendRemoteRunEvent(RemoteRunEvent{
		RunID:     h.runID,
		Type:      "usage",
		SessionID: h.sessionID,
		Payload: rawJSON(map[string]any{
			"input_tokens":  usage.InputTokens,
			"output_tokens": usage.OutputTokens,
			"total_tokens":  usage.TotalTokens,
			"cost_usd":      usage.CostUSD,
			"llm_calls":     usage.LLMCalls,
			"model":         usage.Model,
		}),
	})
}

func (h *remoteRunEventHandler) OnCloudAgent(agentID, status, message string) {
	_ = h.server.sendRemoteRunEvent(RemoteRunEvent{RunID: h.runID, Type: EventCloudAgent, SessionID: h.sessionID, Payload: rawJSON(map[string]any{"agent_id": agentID, "status": status, "message": message})})
}

func (h *remoteRunEventHandler) OnCloudProgress(completed, total int) {
	_ = h.server.sendRemoteRunEvent(RemoteRunEvent{RunID: h.runID, Type: EventCloudProgress, SessionID: h.sessionID, Payload: rawJSON(map[string]int{"completed": completed, "total": total})})
}

func (h *remoteRunEventHandler) OnCloudPlan(planType, content string, needsReview bool) {
	_ = h.server.sendRemoteRunEvent(RemoteRunEvent{RunID: h.runID, Type: EventCloudPlan, SessionID: h.sessionID, Payload: rawJSON(map[string]any{"type": planType, "content": content, "needs_review": needsReview})})
}

func (h *remoteRunEventHandler) OnApprovalNeeded(tool string, args string) bool {
	if h.autoApprove {
		if !agent.DisallowsUnattendedAutoApproval(tool) {
			log.Printf("daemon: remote run auto-approving %s (auto_approve=true)", tool)
			// Auto-approval bypasses the broker, so no approval_requested event
			// reaches the controller (the phone). Emit an explicit approval_auto
			// notice so the unattended tool execution is observable on-device and
			// in the run's replay buffer — otherwise a remote-initiated bash/http
			// call runs with zero controller-visible telemetry, only a local log.
			if h.server != nil {
				_ = h.server.sendRemoteRunEvent(RemoteRunEvent{
					RunID:     h.runID,
					Type:      "approval_auto",
					SessionID: h.sessionID,
					Payload: rawJSON(map[string]string{
						"tool":   tool,
						"reason": "auto_approve",
					}),
				})
			}
			return true
		}
		log.Printf("daemon: remote run %s requires per-call approval (auto_approve=true); prompting via broker", tool)
	}
	if h.broker == nil {
		return false
	}
	var tracker *ApprovalTracker
	if h.server != nil && h.server.deps != nil {
		tracker = h.server.deps.ApprovalTracker
	}
	if tracker != nil {
		tracker.Mark(h.sessionID)
		defer tracker.Clear(h.sessionID)
	}
	decision := h.broker.Request(h.ctx, ApprovalRequestMeta{
		SessionID: h.sessionID,
		Source:    h.source,
		Agent:     h.agent,
	}, tool, args)
	if decision == DecisionAlwaysAllow && h.server != nil && h.server.deps != nil {
		HandleAlwaysAllowDecision(h.server.deps, h.broker, h.agent, tool, args)
	}
	return decision == DecisionAllow || decision == DecisionAlwaysAllow
}

func rawJSON(v any) json.RawMessage {
	data, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return data
}
