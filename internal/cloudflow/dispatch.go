// Package cloudflow runs Shannon Cloud Gateway workflows (research, swarm,
// auto-routing) and bridges Gateway SSE events to a daemon-style EventHandler.
//
// This package was extracted from internal/tools/cloud_delegate.go so the same
// pipeline can be invoked both from the agent loop (as a tool) and from the
// daemon HTTP layer (as a slash-command target).
package cloudflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

// ErrGatewayNotConfigured is returned when Run is called without a Gateway.
// Callers should surface a user-readable message; the daemon HTTP path turns
// this into a 503-style SSE error event.
var ErrGatewayNotConfigured = errors.New("cloudflow: gateway not configured")

// Request describes a single cloud workflow run.
type Request struct {
	Gateway      *client.GatewayClient
	APIKey       string
	Query        string
	WorkflowType string         // "research", "swarm", "auto", or ""
	Strategy     string         // "quick" | "standard" | "deep" | "academic" — research only
	SessionID    string         // optional; passed to Gateway for correlation
	UserContext  string         // optional free-text context appended to the request

	// Timeout is the workflow deadline. Zero falls back to the package default (30 minutes).
	Timeout time.Duration

	// ExtraContext is merged into TaskRequest.Context after user_context and
	// the workflow-type flags are applied. ExtraContext keys take precedence:
	// they override user_context, force_research, and force_swarm if they
	// collide. Use this for caller-specific metadata (agent_name, etc.).
	ExtraContext map[string]any

	// IdleTimeout is the per-connection SSE liveness window passed to
	// StreamSSEWithOptions. 0 disables the idle watchdog.
	IdleTimeout time.Duration

	// MaxReconnects bounds SSE reconnect attempts on unexpected disconnect.
	// 0 lets Run apply its default budget.
	MaxReconnects int
}

// Result holds the final assistant message and accumulated cloud usage.
type Result struct {
	FinalText           string
	Usage               agent.TurnUsage
	WorkflowID          string
	TaskID              string
	FullResultConfirmed bool
}

// contextKeyOnWorkflowStarted is the unexported key used by WithOnWorkflowStarted.
type contextKeyOnWorkflowStarted struct{}

// OnWorkflowStartedFunc is invoked exactly once with the resolved workflow ID
// after Gateway accepts the submission. The daemon uses this to forward the
// workflow ID to its EventBus so other subscribers (Slack, LINE, webhook) can
// hand off subsequent stream events.
type OnWorkflowStartedFunc func(workflowID string)

// WithOnWorkflowStarted returns a child context that carries cb. Run calls cb
// after a successful SubmitTaskStream when present.
func WithOnWorkflowStarted(ctx context.Context, cb OnWorkflowStartedFunc) context.Context {
	return context.WithValue(ctx, contextKeyOnWorkflowStarted{}, cb)
}

// Run submits a Gateway task, streams its SSE events into handler via
// the OnCloudAgent / OnCloudProgress / OnCloudPlan / OnStreamDelta / OnUsage
// callbacks, and returns the final assistant text plus the workflow_id /
// task_id and a FullResultConfirmed flag (true when the API fallback
// returned a complete untruncated result, false when the SSE-only payload
// is the best we have).
//
// Callers that need to inject a workflow_started callback (so the daemon
// EventBus can hand a workflow ID to other subscribers) can place an
// OnWorkflowStartedFunc into ctx via WithOnWorkflowStarted.
func Run(ctx context.Context, req Request, handler agent.EventHandler) (Result, error) {
	if req.Gateway == nil {
		return Result{}, ErrGatewayNotConfigured
	}

	// Resolve the workflow deadline:
	//   (a) honor the caller's existing ctx deadline if set,
	//   (b) else use req.Timeout if non-zero,
	//   (c) else fall back to the 30-minute package default.
	// This caps runaway workflows that never emit WORKFLOW_COMPLETED while
	// preserving any user-configured cloud.timeout passed by the caller.
	timeoutCtx := ctx
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		d := req.Timeout
		if d == 0 {
			d = 30 * time.Minute
		}
		var cancel context.CancelFunc
		timeoutCtx, cancel = context.WithTimeout(ctx, d)
		defer cancel()
	}

	// Build context map for the task request.
	taskContext := make(map[string]any)
	if req.UserContext != "" {
		taskContext["user_context"] = req.UserContext
	}
	switch req.WorkflowType {
	case "research":
		taskContext["force_research"] = true
	case "swarm":
		taskContext["force_swarm"] = true
	case "auto", "":
		// no flag — let the system decide
	}
	// ExtraContext keys override the base flags above.
	for k, v := range req.ExtraContext {
		taskContext[k] = v
	}

	taskReq := client.TaskRequest{
		Query:            req.Query,
		SessionID:        req.SessionID,
		Context:          taskContext,
		ResearchStrategy: req.Strategy,
	}

	resp, err := req.Gateway.SubmitTaskStream(timeoutCtx, taskReq)
	if err != nil {
		return Result{}, fmt.Errorf("submit task: %w", err)
	}

	// Notify any registered workflow-started listener (e.g. daemon EventBus).
	if resp.WorkflowID != "" {
		if fn, ok := ctx.Value(contextKeyOnWorkflowStarted{}).(OnWorkflowStartedFunc); ok && fn != nil {
			fn(resp.WorkflowID)
		}
	}

	// Resolve stream URL: prefer the one returned by Gateway, fall back to
	// the canonical SSE URL derived from workflowID.
	streamURL := resp.StreamURL
	if streamURL == "" {
		streamURL = req.Gateway.StreamURL(resp.WorkflowID)
	}
	streamURL = req.Gateway.ResolveURL(streamURL)

	var finalResult string
	var workflowErr error
	var cloudUsage agent.TurnUsage

	// Enable cloud streaming on handlers that support it (e.g., TUI).
	type cloudStreamToggle interface {
		SetCloudStreaming(bool)
	}
	if cs, ok := handler.(cloudStreamToggle); ok {
		cs.SetCloudStreaming(true)
		defer cs.SetCloudStreaming(false)
	}

	reconnects := req.MaxReconnects
	if reconnects == 0 {
		// 0 here means "apply the default budget", NOT "disable" — there is
		// intentionally no caller-facing way to turn reconnect off from
		// Request (both call sites leave this 0). Note the client layer's
		// StreamSSEOptions.MaxReconnects DOES treat 0 as off, so the two
		// layers differ; don't assume 0 propagates as "off".
		reconnects = 5 // bounded resume budget; cloud ReplaySince is gap-free (seq>N)
	}
	err = client.StreamSSEWithOptions(timeoutCtx, streamURL, req.APIKey, client.StreamSSEOptions{
		IdleTimeout:   req.IdleTimeout,
		MaxReconnects: reconnects,
	}, func(ev client.SSEEvent) {
		var event struct {
			Message  string                 `json:"message"`
			AgentID  string                 `json:"agent_id"`
			Delta    string                 `json:"delta"`
			Response string                 `json:"response"`
			Type     string                 `json:"type"`
			Payload  map[string]interface{} `json:"payload"`
		}
		json.Unmarshal([]byte(ev.Data), &event) //nolint:errcheck

		switch ev.Event {
		// --- Streaming deltas ---
		case "thread.message.delta", "LLM_PARTIAL":
			// Only stream deltas from synthesis / final_output / swarm-lead / single-agent (empty) to user
			if handler != nil && (event.AgentID == "final_output" || event.AgentID == "swarm-lead" || event.AgentID == "synthesis" || event.AgentID == "") {
				delta := event.Delta
				if delta == "" {
					delta = event.Message
				}
				if delta != "" {
					handler.OnStreamDelta(delta)
				}
			}

		// --- Final result ---
		case "thread.message.completed", "LLM_OUTPUT":
			if event.AgentID == "title_generator" {
				break // skip title generation output
			}
			if event.Response != "" {
				finalResult = event.Response
			}
			// Accumulate usage from LLM_OUTPUT metadata
			accumulateUsage(ev.Data, &cloudUsage)

		// --- HITL: research plan review ---
		case "RESEARCH_PLAN_READY":
			// Surface the plan to the user, then auto-approve
			if handler != nil && event.Message != "" {
				handler.OnCloudPlan("research_plan", event.Message, true)
			}
			// Auto-approve so the workflow continues (matches Desktop's autoApprove: "on" default)
			go req.Gateway.ApproveReviewPlan(timeoutCtx, resp.WorkflowID) //nolint:errcheck

		case "RESEARCH_PLAN_UPDATED":
			// Updated plan from feedback — surface to user
			if handler != nil && event.Message != "" {
				handler.OnCloudPlan("research_plan_updated", event.Message, false)
			}

		case "RESEARCH_PLAN_APPROVED":
			// Plan approved, execution starting
			if handler != nil {
				handler.OnCloudPlan("approved", "", false)
			}

		case "APPROVAL_REQUESTED":
			// General approval request — auto-approve
			if handler != nil && event.Message != "" {
				handler.OnStreamDelta("\n[Approval requested: " + event.Message + " — auto-approving]\n")
			}
			go req.Gateway.ApproveReviewPlan(timeoutCtx, resp.WorkflowID) //nolint:errcheck

		// --- Status events — only surface user-facing milestones ---
		case "AGENT_STARTED":
			if handler != nil {
				handler.OnCloudAgent(event.AgentID, "started", event.Message)
			}
		case "AGENT_COMPLETED":
			if handler != nil {
				handler.OnCloudAgent(event.AgentID, "completed", event.Message)
			}
		case "AGENT_THINKING":
			// Cap on rune count, not bytes: cloud builds this as
			// "Thinking: " + truncateQuery(query, 200 runes), so a byte cap
			// would drop multi-byte (CJK) thinking lines well before cloud's
			// own limit — and JP/zh is the primary market. 200 runes matches
			// cloud's cap, so anything cloud forwards gets through.
			if utf8.RuneCountInString(event.Message) <= 200 && handler != nil {
				handler.OnCloudAgent(event.AgentID, "thinking", event.Message)
			}
		case "TOOL_INVOKED", "TOOL_STARTED":
			if handler != nil {
				handler.OnCloudAgent(event.AgentID, "tool", event.Message)
			}
		case "TOOL_OBSERVATION":
			// Tool result coming back (e.g. "Search: <summary>"). Cloud bounds
			// it as MsgToolCompleted = "<ToolName>: " + extractSummary(…, 80) —
			// ~80 chars plus a short tool-name prefix — small enough that no
			// extra length guard is needed here. Forwarded as a "tool" status
			// carrying the originating agent_id so it updates the SAME sub-agent
			// row that TOOL_INVOKED started — the liveness signal that the row is
			// progressing rather than stuck. Previously dropped as "too verbose".
			if handler != nil && event.Message != "" {
				handler.OnCloudAgent(event.AgentID, "tool", event.Message)
			}

		case "DATA_PROCESSING":
			// Use a semantic label for pre-planning / data prep. Was "synthesis",
			// which confusingly implies the final summarization step and also
			// collides with Shannon Cloud's real `synthesis` agent ID (filter above).
			// `preparing` reflects what DATA_PROCESSING actually is.
			if msg := event.Message; msg != "" && len(msg) <= 150 && handler != nil {
				handler.OnCloudAgent("preparing", "processing", msg)
			}

		// --- Internal plumbing — silently ignore ---
		case "WORKFLOW_STARTED", "TOOL_COMPLETED",
			"DELEGATION", "PROGRESS", "STATUS_UPDATE", "WAITING":
			// Drop. PROGRESS stays dropped on purpose: on the research/DAG path
			// its agent_id is a workflow-stage label (citation_agent,
			// research-refiner, domain_analysis), not a worker station nickname —
			// routing it to a sub-agent row would spawn confusing non-worker
			// pills. Worker liveness already comes from AGENT_THINKING /
			// TOOL_INVOKED / TOOL_OBSERVATION above. NOTE: swarm PROGRESS *does*
			// carry a real nickname (MsgAgentProgress(input.AgentID,…)) — revisit
			// this drop if force_swarm is ever enabled.
		case "APPROVAL_DECISION":
			// no-op

		// --- Swarm-specific events ---
		case "LEAD_DECISION":
			if msg := event.Message; msg != "" && len(msg) <= 150 && handler != nil {
				handler.OnCloudAgent("", "thinking", msg)
			}
		case "TASKLIST_UPDATED":
			if payload := event.Payload; payload != nil {
				if tasks, ok := payload["tasks"].([]interface{}); ok && len(tasks) > 0 {
					completed := 0
					for _, task := range tasks {
						if tm, ok := task.(map[string]interface{}); ok {
							if tm["status"] == "completed" {
								completed++
							}
						}
					}
					if handler != nil {
						handler.OnCloudProgress(completed, len(tasks))
					}
				}
			}
		case "HITL_RESPONSE":
			// Passthrough the raw cloud message + agent_id rather than baking an
			// English literal on the wire (Phase B principle); the consumer
			// formats/localizes. Swarm/HITL-only path.
			if event.Message != "" && handler != nil {
				handler.OnCloudAgent(event.AgentID, "thinking", event.Message)
			}

		case "WORKFLOW_COMPLETED":
			if finalResult == "" {
				finalResult = event.Message
			}

		case "WORKFLOW_FAILED", "error", "ERROR_OCCURRED":
			workflowErr = fmt.Errorf("workflow failed: %s", event.Message)

		case "workflow.cancelled":
			workflowErr = fmt.Errorf("workflow cancelled")
		}
	})

	// Report accumulated cloud usage.
	if handler != nil && cloudUsage.LLMCalls > 0 {
		handler.OnUsage(cloudUsage)
	}

	// Always-available REST recovery. Cloud's /tasks/{id} returns the full,
	// untruncated result even when the SSE stream dropped, timed out, or
	// errored before delivering it — and reports a terminal status. This is the
	// only path that can recover a fully-dropped stream, so it runs on EVERY
	// non-clean termination, not just to upgrade a truncated success.
	fullResultConfirmed := false
	taskID := resp.TaskID
	if taskID == "" {
		taskID = resp.WorkflowID
	}
	// recoverViaREST polls /tasks/{id} until the task settles or a bounded
	// deadline. A terminal COMPLETED/SUCCEEDED + non-empty result is recovered
	// success; a terminal FAILED/CANCELLED/TIMEOUT records workflowErr so a
	// failed task is never reported as success; a still-running task that
	// doesn't settle within the budget leaves finalResult unchanged. Cloud
	// status strings are protobuf-style ("TASK_STATUS_COMPLETED"); match
	// case-insensitive substrings (also robust to a normalized "completed").
	recoverViaREST := func(poll bool) {
		if taskID == "" {
			return
		}
		deadline := time.Now().Add(30 * time.Second)
		for {
			apiCtx, apiCancel := context.WithTimeout(ctx, 10*time.Second)
			task, apiErr := req.Gateway.GetTask(apiCtx, taskID)
			apiCancel()
			if apiErr == nil {
				st := strings.ToUpper(task.Status)
				switch {
				case strings.Contains(st, "FAIL") || strings.Contains(st, "CANCEL") || strings.Contains(st, "TIMEOUT"):
					if workflowErr == nil {
						workflowErr = fmt.Errorf("workflow ended in status %s", task.Status)
					}
					return
				case strings.Contains(st, "COMPLETED") || strings.Contains(st, "SUCCEED"):
					if task.Result != "" {
						if len(task.Result) > len(finalResult) {
							finalResult = task.Result
						}
						fullResultConfirmed = true
					}
					return // terminal — don't retry even if result is empty
				case task.Result != "":
					// No explicit terminal status but a result is present (older
					// gateway / mock without a status field) — accept it.
					if len(task.Result) > len(finalResult) {
						finalResult = task.Result
					}
					fullResultConfirmed = true
					return
				}
				// Non-terminal, no result yet → retry until deadline.
			}
			// poll=false (upgrade-only call) does a single GetTask attempt;
			// poll=true keeps polling until terminal or the deadline.
			if !poll || ctx.Err() != nil || !time.Now().Before(deadline) {
				return
			}
			select {
			case <-time.After(2 * time.Second):
			case <-ctx.Done():
				return
			}
		}
	}

	if err != nil && timeoutCtx.Err() == context.DeadlineExceeded {
		// The user's total deadline (cloud.timeout) elapsed — do a single REST
		// check (the task may have just completed), but do NOT poll for 30s,
		// which would blow past the very deadline that just fired.
		recoverViaREST(false)
		// A terminal REST failure (FAIL/CANCEL/TIMEOUT) must win over any partial
		// SSE chunk — never surface a failed task as a nil-error success.
		if workflowErr != nil {
			return Result{}, workflowErr
		}
		if finalResult != "" {
			prefix := ""
			if !fullResultConfirmed {
				prefix = "[cloudflow timed out, returning partial result]\n\n"
			}
			return Result{FinalText: prefix + finalResult, Usage: cloudUsage, WorkflowID: resp.WorkflowID, TaskID: taskID, FullResultConfirmed: fullResultConfirmed}, nil
		}
		return Result{}, fmt.Errorf("cloud task timed out with no result")
	}

	if err != nil {
		recoverViaREST(true)
		// A terminal REST failure must win over a partial SSE chunk — never
		// return a failed task's partial as a nil-error success.
		if workflowErr != nil {
			return Result{}, workflowErr
		}
		if finalResult != "" {
			return Result{FinalText: finalResult, Usage: cloudUsage, WorkflowID: resp.WorkflowID, TaskID: taskID, FullResultConfirmed: fullResultConfirmed}, nil
		}
		return Result{}, fmt.Errorf("stream error: %w", err)
	}

	if workflowErr != nil {
		recoverViaREST(true)
		// Only surface a result if REST authoritatively confirmed COMPLETED
		// (fullResultConfirmed). An SSE-reported failure plus a mere partial
		// chunk (finalResult set but NOT REST-confirmed) must return the error,
		// not the partial — otherwise a failed task leaks out as success.
		if fullResultConfirmed {
			return Result{FinalText: finalResult, Usage: cloudUsage, WorkflowID: resp.WorkflowID, TaskID: taskID, FullResultConfirmed: fullResultConfirmed}, nil
		}
		return Result{}, workflowErr
	}

	if finalResult == "" {
		recoverViaREST(true)
		if finalResult == "" {
			if workflowErr != nil {
				return Result{}, workflowErr
			}
			return Result{}, fmt.Errorf("workflow completed but returned no response")
		}
	} else {
		recoverViaREST(false) // upgrade-only: single GetTask attempt, don't poll
	}

	return Result{
		FinalText:           finalResult,
		Usage:               cloudUsage,
		WorkflowID:          resp.WorkflowID,
		TaskID:              taskID,
		FullResultConfirmed: fullResultConfirmed,
	}, nil
}

// accumulateUsage extracts usage metadata from LLM_OUTPUT events and adds it
// to the running total. Shannon Cloud sends usage info in "metadata" field.
func accumulateUsage(data string, usage *agent.TurnUsage) {
	var meta struct {
		Metadata *struct {
			InputTokens           int     `json:"input_tokens"`
			OutputTokens          int     `json:"output_tokens"`
			TokensUsed            int     `json:"tokens_used"`
			CostUSD               float64 `json:"cost_usd"`
			CacheReadTokens       int     `json:"cache_read_tokens"`
			CacheCreationTokens   int     `json:"cache_creation_tokens"`
			CacheCreation5mTokens int     `json:"cache_creation_5m_tokens"`
			CacheCreation1hTokens int     `json:"cache_creation_1h_tokens"`
			ModelUsed             string  `json:"model_used"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal([]byte(data), &meta); err != nil || meta.Metadata == nil {
		return
	}
	usage.Add(client.Usage{
		InputTokens:           meta.Metadata.InputTokens,
		OutputTokens:          meta.Metadata.OutputTokens,
		TotalTokens:           meta.Metadata.TokensUsed,
		CostUSD:               meta.Metadata.CostUSD,
		CacheReadTokens:       meta.Metadata.CacheReadTokens,
		CacheCreationTokens:   meta.Metadata.CacheCreationTokens,
		CacheCreation5mTokens: meta.Metadata.CacheCreation5mTokens,
		CacheCreation1hTokens: meta.Metadata.CacheCreation1hTokens,
	})
	if meta.Metadata.ModelUsed != "" {
		usage.Model = meta.Metadata.ModelUsed
	}
}
