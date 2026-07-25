package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/cloudflow"
)

type CloudDelegateTool struct {
	gw          *client.GatewayClient
	apiKey      string
	timeout     time.Duration
	idleTimeout time.Duration
	handler     agent.EventHandler
	agentName   string
	agentPrompt string
}

type cloudDelegateArgs struct {
	Task         string `json:"task"`
	Description  string `json:"description,omitempty"`
	Context      string `json:"context,omitempty"`
	WorkflowType string `json:"workflow_type,omitempty"`
	Terminal     *bool  `json:"terminal,omitempty"`
}

func NewCloudDelegateTool(gw *client.GatewayClient, apiKey string, timeout, idleTimeout time.Duration, handler agent.EventHandler, agentName, agentPrompt string) *CloudDelegateTool {
	return &CloudDelegateTool{
		gw:          gw,
		apiKey:      apiKey,
		timeout:     timeout,
		idleTimeout: idleTimeout,
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
		Description: "Delegate to Shannon Cloud. Remote, 5-15 min, expensive.\n\n" +
			"Use cloud_delegate ONLY when the task contains 3+ sub-investigations that\n" +
			"each require a DIFFERENT source and a DIFFERENT query strategy, and only\n" +
			"need to converge at the end (intermediate state sharing between agents is fine).\n\n" +
			"Key distinction — do not confuse these:\n" +
			"  - OUTPUT cardinality (return N items in a list)        → NOT parallelism\n" +
			"  - INVESTIGATION cardinality (run N different queries\n" +
			"    on N different sources with N different strategies)  → may warrant cloud\n\n" +
			"A single platform returning a long list is ONE investigation, regardless\n" +
			"of list length. Use local tools.\n\n" +
			"Do NOT use cloud_delegate when:\n" +
			"  - One query on one platform can return the list (even if list is long)\n" +
			"  - Task iterates on a single topic/entity with follow-up queries\n" +
			"  - The task names one domain, one source, or one entity\n" +
			"  - The user asks to \"find N X's\" on a specific platform\n\n" +
			"Local routing by task shape:\n" +
			"  - List/enumerate/find on any single platform   → x_search\n" +
			"  - Iterative research on one topic              → x_search + web_fetch\n" +
			"  - Fetch a specific URL                         → web_fetch or http\n" +
			"  - Save output                                  → file_write" +
			agent.DescriptionGuidance,
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"task": map[string]any{
					"type":        "string",
					"description": "The task to delegate. Be specific and detailed about what you need.",
				},
				"description": agent.DescriptionFieldSpec,
				"context": map[string]any{
					"type":        "string",
					"description": "Optional context to include with the task (max 8000 chars). Can include relevant code snippets, data, or background information.",
				},
				"workflow_type": map[string]any{
					"type": "string",
					"enum": []string{"research", "auto"},
					"description": "Execution mode. Assumes the gate in the top-level description has already passed — this parameter does NOT expand eligibility for calling cloud_delegate. " +
						"'auto' (default): system picks a DAG based on task shape. " +
						"'research': fixed research DAG, ~5 min.",
				},
				"terminal": map[string]any{
					"type":        "boolean",
					"description": "If true, return the cloud result directly to the user (bypasses further processing). If false, feed the result back into your context so you can continue working with it (e.g., write files, run tests, apply changes). Defaults to true for 'research' workflow, false otherwise.",
				},
			},
		},
		Required: []string{"task", "description"},
	}
}

func (t *CloudDelegateTool) Run(ctx context.Context, argsJSON string) (agent.ToolResult, error) {
	var args cloudDelegateArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return agent.ValidationError(fmt.Sprintf("invalid arguments: %v", err)), nil
	}
	// Explicit required-field checks so the [validation error] prefix is kept
	// and each field names itself for the loop detector's early stop.
	if strings.TrimSpace(args.Task) == "" {
		return agent.ValidationError("task is required: describe the work to delegate to the cloud."), nil
	}
	if strings.TrimSpace(args.Description) == "" {
		return agent.ValidationError("cloud_delegate: missing required `description` parameter"), nil
	}

	// Cap context length
	if len(args.Context) > 8000 {
		args.Context = args.Context[:8000]
	}

	extras := map[string]any{}
	if t.agentName != "" {
		extras["agent_name"] = t.agentName
		if t.agentPrompt != "" {
			extras["agent_instructions"] = t.agentPrompt
		}
	}

	// ctx already carries any OnWorkflowStartedFunc set by cmd/daemon.go via
	// cloudflow.WithOnWorkflowStarted — pass it through directly so cloudflow.Run
	// finds it under the correct context key without an extra wrapper.
	res, err := cloudflow.Run(ctx, cloudflow.Request{
		Gateway:      t.gw,
		APIKey:       t.apiKey,
		Query:        args.Task,
		WorkflowType: args.WorkflowType,
		UserContext:  args.Context,
		ExtraContext: extras,
		Timeout:      t.timeout,
		IdleTimeout:  t.idleTimeout,
	}, t.handler)

	if err != nil {
		if errors.Is(err, cloudflow.ErrGatewayNotConfigured) {
			return agent.ToolResult{Content: "cloud delegation not available: gateway not configured", IsError: true}, nil
		}
		return agent.ToolResult{Content: fmt.Sprintf("cloud workflow error: %v", err), IsError: true}, nil
	}

	terminal := args.WorkflowType == "research"
	if args.Terminal != nil {
		terminal = *args.Terminal
	}
	return agent.ToolResult{Content: res.FinalText, CloudResult: res.FullResultConfirmed && terminal}, nil
}

func (t *CloudDelegateTool) RequiresApproval() bool { return true }

func (t *CloudDelegateTool) IsReadOnlyCall(string) bool { return false }

// Ensure CloudDelegateTool implements SafeChecker to always require approval.
var _ agent.SafeChecker = (*CloudDelegateTool)(nil)

func (t *CloudDelegateTool) IsSafeArgs(_ string) bool { return false }
