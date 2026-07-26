package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/audit"
	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/hooks"
	"github.com/Kocoro-lab/ShanClaw/internal/permissions"
	"github.com/Kocoro-lab/ShanClaw/internal/tools"
)

type openAIComputerTaskArgsV1 struct {
	Task        string   `json:"task"`
	Apps        []string `json:"apps,omitempty"`
	Description string   `json:"description,omitempty"`
}

type openAIComputerTaskRuntimeV1 interface {
	openAIComputerActionRuntimeV1
	ResolveTaskAppV1(
		context.Context,
		string,
	) (tools.OpenAIComputerTaskAppV1, error)
	LaunchAndFocusTaskAppsV1(
		context.Context,
		[]tools.OpenAIComputerTaskAppV1,
	) error
}

// openAIComputerTaskToolV1 is the only model-visible desktop-control surface.
// The parent model delegates one complete goal; a private OpenAI Responses loop
// owns screenshots, ordered action batches, and continuation state.
type openAIComputerTaskToolV1 struct {
	gateway         client.LLMClient
	profile         *client.ExecutionProfile
	resolveProfile  func(context.Context) (*client.ExecutionProfile, error)
	profileOnce     sync.Once
	resolvedProfile *client.ExecutionProfile
	profileErr      error
	childTools      *agent.ToolRegistry
	workflow        *daemonGUIWorkflow
	runtime         openAIComputerTaskRuntimeV1
	appPolicy       *ComputerUseAppPolicyStore
	handler         agent.EventHandler

	modelTier   string
	shannonDir  string
	maxIter     int
	maxTokens   int
	resultTrunc int
	argsTrunc   int
	permissions *permissions.PermissionsConfig
	auditor     *audit.AuditLogger
	hookRunner  *hooks.HookRunner
}

func (t *openAIComputerTaskToolV1) Info() agent.ToolInfo {
	return agent.ToolInfo{
		Name: "computer_use",
		Description: "Operate native macOS desktop apps to complete one full user goal. " +
			"Give the complete task once; the computer executor launches/focuses apps, " +
			"observes the current UI, performs the needed actions, and verifies the result internally. " +
			"Do not split clicks, typing, screenshots, or app switches into separate calls. " +
			"List app names in apps when known; the executor may switch apps itself.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"task": map[string]any{
					"type":        "string",
					"description": "The complete desktop task and desired end state.",
				},
				"apps": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": "string"},
					"description": "Optional installed macOS app names to launch before starting.",
				},
				"description": agent.DescriptionFieldSpec,
			},
		},
		Required: []string{"task", "description"},
	}
}

func (t *openAIComputerTaskToolV1) RequiresApproval() bool { return true }

func (t *openAIComputerTaskToolV1) Run(
	ctx context.Context,
	argsJSON string,
) (agent.ToolResult, error) {
	var args openAIComputerTaskArgsV1
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return agent.ValidationError(fmt.Sprintf("invalid arguments: %v", err)), nil
	}
	args.Task = strings.TrimSpace(args.Task)
	args.Description = strings.TrimSpace(args.Description)
	if args.Task == "" {
		return agent.ValidationError("task is required"), nil
	}
	if args.Description == "" {
		return agent.ValidationError("description is required"), nil
	}
	if t == nil || t.gateway == nil ||
		t.childTools == nil || t.workflow == nil || t.runtime == nil {
		return agent.BusinessError(
			"OpenAI native Computer Use is temporarily unavailable; no desktop action was attempted",
		), nil
	}
	profile := t.profile
	if profile == nil && t.resolveProfile != nil {
		t.profileOnce.Do(func() {
			t.resolvedProfile, t.profileErr = t.resolveProfile(ctx)
		})
		profile = t.resolvedProfile
		if t.profileErr != nil {
			return agent.BusinessError(
				"computer_use_error: backend_contract_unavailable\n" +
					"message: OpenAI native Computer Use is unavailable because its execution profile could not be resolved\n" +
					"recovery: do not retry computer_use in this turn; no desktop action was attempted\n" +
					"detail: " + t.profileErr.Error(),
			), nil
		}
	}
	if profile == nil ||
		!profile.IsTrustedResolution() ||
		profile.Provider() != client.OpenAIComputerProvider ||
		profile.APISurface() != client.APISurfaceOpenAIResponses ||
		profile.ExecutionMode() != client.ExecutionModeNativeComputer ||
		profile.ToolContract() != client.ToolContractOpenAIComputerV1 ||
		!profile.SupportsImageInput() ||
		!profile.SupportsToolResultImages() ||
		profile.SupportsFunctionTools() ||
		!profile.SupportsBatchedActions() {
		return agent.BusinessError(
			"OpenAI native Computer Use is temporarily unavailable; no desktop action was attempted",
		), nil
	}

	seen := make(map[string]struct{}, len(args.Apps))
	apps := make([]tools.OpenAIComputerTaskAppV1, 0, len(args.Apps))
	for _, requested := range args.Apps {
		requested = strings.TrimSpace(requested)
		if requested == "" {
			continue
		}
		key := strings.ToLower(requested)
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		identity, err := t.runtime.ResolveTaskAppV1(ctx, requested)
		if err != nil {
			return agent.BusinessError(err.Error()), nil
		}
		if t.appPolicy != nil &&
			t.appPolicy.DecisionFor(identity.BundleID).Decision ==
				ComputerUseAppPolicyBlocked {
			return agent.PermissionError(
				fmt.Sprintf("%s is blocked in Saved App Blocks", identity.App),
			), nil
		}
		apps = append(apps, identity)
	}
	if len(apps) > 0 {
		if _, err := t.workflow.ensureLease(
			ctx,
			agent.GUIActionDescriptor{
				Participates:   true,
				ActionKind:     "desktop_task",
				Effect:         agent.GUIActionMutation,
				TargetBundleID: apps[0].BundleID,
				TargetAppName:  apps[0].App,
				ExecutionPath:  "openai_native",
			},
		); err != nil {
			return guiCoordinatorToolError(err), nil
		}
	}
	if err := t.runtime.LaunchAndFocusTaskAppsV1(ctx, apps); err != nil {
		return agent.BusinessError(err.Error()), nil
	}

	invocationCtx := tools.ContextWithOpenAINativeComputerActionV1(
		agent.ContextWithToolInvocation(ctx, agent.ToolInvocation{
			ToolName:  "computer_use",
			ToolUseID: "computer-task/initial-observation",
		}),
	)
	var initial agent.ToolResult
	var runErr error
	var observationErr error
	for attempt := 0; attempt < 2; attempt++ {
		plan, err := t.runtime.PlanOpenAIComputerObservationV1(
			"Capture the initial desktop task state",
			true,
		)
		if err != nil {
			observationErr = err
			continue
		}
		initial, runErr = t.workflow.runTool(invocationCtx, plan.Tool, plan.Args)
		if runErr == nil && !initial.IsError && len(initial.Images) == 1 {
			observationErr = nil
			break
		}
		observationErr = runErr
	}
	if runErr != nil || initial.IsError || len(initial.Images) != 1 {
		detail := "the desktop observation backend returned an error"
		if observationErr != nil {
			detail = observationErr.Error()
		}
		return agent.BusinessError(
			"computer_use_error: initial_observation_unavailable\n" +
				"message: Computer Use could not capture the verified initial app window\n" +
				"recovery: retry computer_use once; no desktop action was attempted\n" +
				"detail: " + detail,
		), nil
	}

	runner, err := newDaemonOpenAIComputerBatchRunnerV1(
		t.workflow,
		t.runtime,
	)
	if err != nil {
		return agent.BusinessError(err.Error()), nil
	}
	child := agent.NewAgentLoop(
		t.gateway,
		t.childTools,
		t.modelTier,
		t.shannonDir,
		t.maxIter,
		t.resultTrunc,
		t.argsTrunc,
		t.permissions,
		t.auditor,
		t.hookRunner,
	)
	child.SetSkillDiscovery(false)
	child.SetBypassPermissions(true)
	child.SetSpecificModel(profile.Model())
	child.SetExecutionProfile(profile)
	child.SetOpenAIComputerBatchExecutor(runner)
	child.SetForceInitialToolUse(true)
	child.SetHandler(openAIComputerChildHandlerV1{parent: t.handler})
	child.SetStickyContext(
		"execution_role=private_openai_native_computer\n" +
			"Complete the user's entire desktop goal with the native computer tool. " +
			"The initial image is the current verified app window. " +
			"Use coordinates from the latest returned screenshot, continue across app switches, " +
			"and stop as soon as the requested end state is verified. " +
			"Do not ask the parent to perform clicks, typing, screenshots, or state management.",
	)
	if t.maxTokens > 0 {
		child.SetMaxTokens(t.maxTokens)
	}
	content := []client.ContentBlock{{
		Type: "image",
		Source: &client.ImageSource{
			Type:      "base64",
			MediaType: initial.Images[0].MediaType,
			Data:      initial.Images[0].Data,
		},
	}}
	reply, _, err := child.Run(ctx, args.Task, content, nil)
	if err != nil {
		return agent.BusinessError(
			"computer_use_error: executor_failed\n" +
				"message: the private OpenAI Computer Use executor could not complete the task\n" +
				"recovery: retry computer_use once; it will inspect the current app state before continuing, and the failure does not mean the target app is missing or blocked\n" +
				"detail: " + err.Error(),
		), nil
	}
	reply = strings.TrimSpace(reply)
	if reply == "" {
		reply = "Computer Use completed the desktop task."
	}
	return agent.ToolResult{Content: reply}, nil
}

// The child shares approvals and usage reporting with the parent transport,
// while its intermediate narration stays inside the single parent tool card.
type openAIComputerChildHandlerV1 struct {
	parent agent.EventHandler
}

func (h openAIComputerChildHandlerV1) OnToolCall(string, string, string) {}
func (h openAIComputerChildHandlerV1) OnToolResult(
	string, string, string, agent.ToolResult, time.Duration,
) {
}
func (h openAIComputerChildHandlerV1) OnText(string)                       {}
func (h openAIComputerChildHandlerV1) OnPreamble(string)                   {}
func (h openAIComputerChildHandlerV1) OnStreamDelta(string)                {}
func (h openAIComputerChildHandlerV1) OnCloudAgent(string, string, string) {}
func (h openAIComputerChildHandlerV1) OnCloudProgress(int, int)            {}
func (h openAIComputerChildHandlerV1) OnCloudPlan(string, string, bool)    {}
func (h openAIComputerChildHandlerV1) OnUsage(usage agent.TurnUsage) {
	if h.parent != nil {
		h.parent.OnUsage(usage)
	}
}
func (h openAIComputerChildHandlerV1) OnApprovalNeeded(
	tool string,
	args string,
) bool {
	return h.parent != nil && h.parent.OnApprovalNeeded(tool, args)
}

var _ agent.Tool = (*openAIComputerTaskToolV1)(nil)
