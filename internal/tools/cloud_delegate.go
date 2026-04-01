package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

// OnWorkflowStartedFunc is called when cloud_delegate gets a workflow_id from Cloud.
// Used by daemon mode to send progress with workflow_id for streaming card replies.
type OnWorkflowStartedFunc func(workflowID string)

type contextKeyWorkflowStarted struct{}

// WithOnWorkflowStarted returns a context carrying a per-request workflow started callback.
// This is safe for concurrent use (each message gets its own context).
func WithOnWorkflowStarted(ctx context.Context, fn OnWorkflowStartedFunc) context.Context {
	return context.WithValue(ctx, contextKeyWorkflowStarted{}, fn)
}

type CloudDelegateTool struct {
	gw          *client.GatewayClient
	apiKey      string
	timeout     time.Duration
	handler     agent.EventHandler
	agentName   string
	agentPrompt string
}

type cloudDelegateArgs struct {
	Task         string `json:"task"`
	Context      string `json:"context,omitempty"`
	WorkflowType string `json:"workflow_type,omitempty"`
	Terminal     *bool  `json:"terminal,omitempty"`
}

func NewCloudDelegateTool(gw *client.GatewayClient, apiKey string, timeout time.Duration, handler agent.EventHandler, agentName, agentPrompt string) *CloudDelegateTool {
	return &CloudDelegateTool{
		gw:          gw,
		apiKey:      apiKey,
		timeout:     timeout,
		handler:     handler,
		agentName:   agentName,
		agentPrompt: agentPrompt,
	}
}

// SetHandler updates the event handler. Used when the handler isn't available
// at registration time (e.g., TUI creates handler per-run).
func (t *CloudDelegateTool) SetHandler(h agent.EventHandler) {
	t.handler = h
}

// SetAgentContext updates the agent identity forwarded to Shannon Cloud.
// Used in daemon mode where the agent isn't known at registration time.
func (t *CloudDelegateTool) SetAgentContext(name, prompt string) {
	t.agentName = name
	t.agentPrompt = prompt
}

func (t *CloudDelegateTool) Info() agent.ToolInfo {
	return agent.ToolInfo{
		Name: "cloud_delegate",
		Description: "Delegate a complex task to Shannon Cloud for multi-agent processing. " +
			"Use for research, analysis, or tasks that benefit from multiple specialized agents " +
			"working together. The task runs remotely and streams progress back. " +
			"Returns the final result when complete.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"task": map[string]any{
					"type":        "string",
					"description": "The task to delegate. Be specific and detailed about what you need.",
				},
				"context": map[string]any{
					"type":        "string",
					"description": "Optional context to include with the task (max 8000 chars). Can include relevant code snippets, data, or background information.",
				},
				"workflow_type": map[string]any{
					"type":        "string",
					"enum":        []string{"research", "swarm", "auto"},
					"description": "Workflow type: 'research' for deep multi-source research with web search and synthesis. 'swarm' for complex tasks requiring a lead agent coordinating dynamic sub-agents (researcher, coder, analyst) with a shared workspace — best for open-ended analysis, multi-step problem solving, or tasks combining research + computation + writing. 'auto' (default) routes to a fixed DAG plan — good for structured tasks with clear subtask dependencies.",
				},
				"terminal": map[string]any{
					"type":        "boolean",
					"description": "If true, return the cloud result directly to the user (bypasses further processing). If false, feed the result back into your context so you can continue working with it (e.g., write files, run tests, apply changes). Defaults to true for 'research' workflow, false otherwise.",
				},
			},
		},
		Required: []string{"task"},
	}
}

func (t *CloudDelegateTool) Run(ctx context.Context, argsJSON string) (agent.ToolResult, error) {
	var args cloudDelegateArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return agent.ToolResult{Content: fmt.Sprintf("invalid arguments: %v", err), IsError: true}, nil
	}

	if args.Task == "" {
		return agent.ToolResult{Content: "task is required", IsError: true}, nil
	}

	// Cap context length
	if len(args.Context) > 8000 {
		args.Context = args.Context[:8000]
	}

	// Build context map based on workflow_type
	taskContext := make(map[string]any)
	if args.Context != "" {
		taskContext["user_context"] = args.Context
	}
	switch args.WorkflowType {
	case "research":
		taskContext["force_research"] = true
	case "swarm":
		taskContext["force_swarm"] = true
	case "auto", "":
		// no flag — let the system decide
	}

	if t.agentName != "" {
		taskContext["agent_name"] = t.agentName
		if t.agentPrompt != "" {
			taskContext["agent_instructions"] = t.agentPrompt
		}
	}

	taskReq := client.TaskRequest{
		Query:   args.Task,
		Context: taskContext,
	}

	if t.gw == nil {
		return agent.ToolResult{Content: "cloud delegation not available: gateway not configured", IsError: true}, nil
	}

	// Apply timeout
	timeoutCtx, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()

	resp, err := t.gw.SubmitTaskStream(timeoutCtx, taskReq)
	if err != nil {
		return agent.ToolResult{Content: fmt.Sprintf("failed to submit task: %v", err), IsError: true}, nil
	}

	// Notify daemon of workflow_id so Cloud can start streaming card replies
	if resp.WorkflowID != "" {
		if fn, ok := ctx.Value(contextKeyWorkflowStarted{}).(OnWorkflowStartedFunc); ok && fn != nil {
			fn(resp.WorkflowID)
		}
	}

	// Resolve stream URL
	streamURL := resp.StreamURL
	if streamURL == "" {
		streamURL = t.gw.StreamURL(resp.WorkflowID)
	}
	streamURL = t.gw.ResolveURL(streamURL)

	var finalResult string
	var workflowErr error
	var cloudUsage agent.TurnUsage

	// Enable cloud streaming on handlers that support it (e.g., TUI)
	type cloudStreamToggle interface {
		SetCloudStreaming(bool)
	}
	if cs, ok := t.handler.(cloudStreamToggle); ok {
		cs.SetCloudStreaming(true)
		defer cs.SetCloudStreaming(false)
	}

	err = client.StreamSSE(timeoutCtx, streamURL, t.apiKey, func(ev client.SSEEvent) {
		var event struct {
			Message  string                 `json:"message"`
			AgentID  string                 `json:"agent_id"`
			Delta    string                 `json:"delta"`
			Response string                 `json:"response"`
			Type     string                 `json:"type"`
			Payload  map[string]interface{} `json:"payload"`
		}
		json.Unmarshal([]byte(ev.Data), &event)

		switch ev.Event {
		// --- Streaming deltas ---
		case "thread.message.delta", "LLM_PARTIAL":
			// Only stream deltas from synthesis / final_output / swarm-lead / single-agent (empty) to user
			if t.handler != nil && (event.AgentID == "final_output" || event.AgentID == "swarm-lead" || event.AgentID == "synthesis" || event.AgentID == "") {
				delta := event.Delta
				if delta == "" {
					delta = event.Message
				}
				if delta != "" {
					t.handler.OnStreamDelta(delta)
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
			t.accumulateUsage(ev.Data, &cloudUsage)

		// --- HITL: research plan review ---
		case "RESEARCH_PLAN_READY":
			// Surface the plan to the user, then auto-approve
			if t.handler != nil && event.Message != "" {
				t.handler.OnCloudPlan("research_plan", event.Message, true)
			}
			// Auto-approve so the workflow continues (matches Desktop's autoApprove: "on" default)
			go t.gw.ApproveReviewPlan(timeoutCtx, resp.WorkflowID)

		case "RESEARCH_PLAN_UPDATED":
			// Updated plan from feedback — surface to user
			if t.handler != nil && event.Message != "" {
				t.handler.OnCloudPlan("research_plan_updated", event.Message, false)
			}

		case "RESEARCH_PLAN_APPROVED":
			// Plan approved, execution starting
			if t.handler != nil {
				t.handler.OnCloudPlan("approved", "", false)
			}

		case "APPROVAL_REQUESTED":
			// General approval request — auto-approve
			if t.handler != nil && event.Message != "" {
				t.handler.OnStreamDelta("\n[Approval requested: " + event.Message + " — auto-approving]\n")
			}
			go t.gw.ApproveReviewPlan(timeoutCtx, resp.WorkflowID)

		// --- Status events — only surface user-facing milestones ---
		case "AGENT_STARTED":
			if t.handler != nil {
				t.handler.OnCloudAgent(event.AgentID, "started", statusMsg(event.AgentID, event.Message, "Agent working..."))
			}
		case "AGENT_COMPLETED":
			if t.handler != nil {
				t.handler.OnCloudAgent(event.AgentID, "completed", statusMsg(event.AgentID, event.Message, "Agent completed"))
			}
		case "AGENT_THINKING":
			if len(event.Message) <= 100 && t.handler != nil {
				t.handler.OnCloudAgent("", "thinking", statusMsg("", event.Message, "Thinking..."))
			}
		case "TOOL_INVOKED", "TOOL_STARTED":
			if t.handler != nil {
				t.handler.OnCloudAgent("", "tool", statusMsg("", event.Message, "Calling tool..."))
			}

		case "DATA_PROCESSING":
			if msg := event.Message; msg != "" && len(msg) <= 150 && t.handler != nil {
				t.handler.OnCloudAgent("synthesis", "processing", msg)
			}

		// --- Internal plumbing — silently ignore ---
		case "WORKFLOW_STARTED", "TOOL_OBSERVATION", "TOOL_COMPLETED",
			"DELEGATION", "PROGRESS", "STATUS_UPDATE", "WAITING":
			// Drop — these are too verbose for the desktop UI
		case "APPROVAL_DECISION":
			// no-op

		// --- Swarm-specific events ---
		case "LEAD_DECISION":
			if msg := event.Message; msg != "" && len(msg) <= 150 && t.handler != nil {
				t.handler.OnCloudAgent("", "thinking", msg)
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
					if t.handler != nil {
						t.handler.OnCloudProgress(completed, len(tasks))
					}
				}
			}
		case "HITL_RESPONSE":
			if event.Message != "" && t.handler != nil {
				t.handler.OnCloudAgent("", "thinking", "Lead responding to your input")
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

	// Report accumulated cloud usage
	if t.handler != nil && cloudUsage.LLMCalls > 0 {
		t.handler.OnUsage(cloudUsage)
	}

	// Handle timeout
	if err != nil && timeoutCtx.Err() == context.DeadlineExceeded {
		if finalResult != "" {
			return agent.ToolResult{Content: fmt.Sprintf("[cloud_delegate timed out after %s, returning partial result]\n\n%s", t.timeout, finalResult)}, nil
		}
		return agent.ToolResult{Content: fmt.Sprintf("cloud task timed out after %s with no result", t.timeout), IsError: true}, nil
	}

	if err != nil {
		return agent.ToolResult{Content: fmt.Sprintf("stream error: %v", err), IsError: true}, nil
	}

	if workflowErr != nil {
		return agent.ToolResult{Content: workflowErr.Error(), IsError: true}, nil
	}

	if finalResult == "" {
		return agent.ToolResult{Content: "workflow completed but returned no response", IsError: true}, nil
	}

	// API fallback: SSE events may be truncated (cloud caps at 10K runes).
	// Always attempt to fetch the full result from the REST API.
	// Only mark as CloudResult (bypass LLM summarization) when we have
	// a confirmed full result — either from SSE or from the API fallback.
	fullResultConfirmed := true
	taskID := resp.TaskID
	if taskID == "" {
		taskID = resp.WorkflowID
	}
	if taskID != "" && t.gw != nil {
		apiCtx, apiCancel := context.WithTimeout(ctx, 10*time.Second)
		defer apiCancel()
		if task, apiErr := t.gw.GetTask(apiCtx, taskID); apiErr == nil && task.Result != "" {
			if len(task.Result) > len(finalResult) {
				// API returned a longer result — SSE was truncated
				finalResult = task.Result
			}
			// API succeeded: we have the canonical full result
		} else {
			// API fallback failed — SSE result may be truncated
			fullResultConfirmed = false
		}
	}

	// Determine terminal mode: explicit arg takes precedence,
	// otherwise research defaults to terminal, swarm/auto default to non-terminal.
	terminal := args.WorkflowType == "research"
	if args.Terminal != nil {
		terminal = *args.Terminal
	}

	return agent.ToolResult{Content: finalResult, CloudResult: fullResultConfirmed && terminal}, nil
}

func (t *CloudDelegateTool) RequiresApproval() bool { return true }

func (t *CloudDelegateTool) IsReadOnlyCall(string) bool { return false }

// accumulateUsage extracts usage metadata from LLM_OUTPUT events and adds it to the running total.
func (t *CloudDelegateTool) accumulateUsage(data string, usage *agent.TurnUsage) {
	// Shannon Cloud sends usage info in "metadata" field of LLM_OUTPUT events
	var meta struct {
		Metadata *struct {
			InputTokens         int     `json:"input_tokens"`
			OutputTokens        int     `json:"output_tokens"`
			TokensUsed          int     `json:"tokens_used"`
			CostUSD             float64 `json:"cost_usd"`
			CacheReadTokens     int     `json:"cache_read_tokens"`
			CacheCreationTokens int     `json:"cache_creation_tokens"`
			ModelUsed           string  `json:"model_used"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal([]byte(data), &meta); err != nil || meta.Metadata == nil {
		return
	}
	usage.InputTokens += meta.Metadata.InputTokens
	usage.OutputTokens += meta.Metadata.OutputTokens
	usage.TotalTokens += meta.Metadata.InputTokens + meta.Metadata.OutputTokens
	usage.CostUSD += meta.Metadata.CostUSD
	usage.CacheReadTokens += meta.Metadata.CacheReadTokens
	usage.CacheCreationTokens += meta.Metadata.CacheCreationTokens
	usage.LLMCalls++
	if meta.Metadata.ModelUsed != "" {
		usage.Model = meta.Metadata.ModelUsed
	}
}

// statusMsg returns message if non-empty, otherwise fallback.
// Prepends agentID label if present.
func statusMsg(agentID, message, fallback string) string {
	msg := message
	if msg == "" {
		msg = fallback
	}
	if agentID != "" && agentID != "orchestrator" && agentID != "streaming" {
		return "[" + agentID + "] " + msg
	}
	return msg
}

// Ensure CloudDelegateTool implements SafeChecker to always require approval.
var _ agent.SafeChecker = (*CloudDelegateTool)(nil)

func (t *CloudDelegateTool) IsSafeArgs(_ string) bool { return false }
