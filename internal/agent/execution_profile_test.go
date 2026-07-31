package agent

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/executionprofile"
)

func fastProfileForAgentTest() executionprofile.Profile {
	return executionprofile.Profile{
		RequestedMode:       executionprofile.ModeFast,
		EffectiveMode:       executionprofile.ModeFast,
		SchemaVersion:       executionprofile.FastSchemaVersion,
		ProfileName:         executionprofile.FastProfileName,
		ProfileVersion:      executionprofile.FastProfileVersion,
		ProfileID:           "kfp1_agent-test",
		Provider:            "openai",
		Model:               "gpt-5.6-luna",
		APISurface:          "openai_responses",
		ToolContract:        executionprofile.FastToolContract,
		ReasoningEffort:     "medium",
		ServiceTier:         "fast",
		ParallelToolCalls:   true,
		ResponseCachePolicy: executionprofile.ResponseCacheOff,
		ResolutionReason:    "cloud_profile_resolved",
	}
}

func TestAgentLoopExecutionProfileWireAndFullConfigPreservation(t *testing.T) {
	t.Run("full preserves exact agent selectors", func(t *testing.T) {
		llm := &budgetCaptureLLMClient{responses: []*client.CompletionResponse{{
			OutputText: "done", FinishReason: "end_turn",
		}}}
		loop := NewAgentLoop(llm, NewToolRegistry(), "large", "", 3, 1000, 100, nil, nil, nil)
		loop.SetSpecificModel("gpt-5.6-terra")
		loop.SetReasoningEffort("low")
		loop.SetEffortTier("medium")
		loop.SetThinking(&client.ThinkingConfig{Type: "enabled", BudgetTokens: 4096})
		loop.SetKoeExecutionProfile(executionprofile.FullProfile(executionprofile.ModeFull, "requested_full"))
		if _, _, err := loop.Run(context.Background(), "work", nil, nil); err != nil {
			t.Fatal(err)
		}
		req := llm.requests[0]
		if req.ModelTier != "large" || req.SpecificModel != "gpt-5.6-terra" ||
			req.ReasoningEffort != "low" || req.EffortTier != "medium" {
			t.Fatalf("full request changed agent config: %+v", req)
		}
		if req.Thinking == nil || req.Thinking.Type != "enabled" || req.Thinking.BudgetTokens != 4096 {
			t.Fatalf("full request changed thinking config: %+v", req.Thinking)
		}
		if req.ExecutionProfileID != "" || req.ParallelToolCalls {
			t.Fatalf("full request leaked fast selectors: %+v", req)
		}
		if req.ResponseCachePolicy != "" {
			t.Fatalf("full response cache policy = %q, want omitted", req.ResponseCachePolicy)
		}
		if req.MaxTokens != 128_000 {
			t.Fatalf("full max_tokens = %d, want configured Terra cap 128000", req.MaxTokens)
		}
	})

	t.Run("fast sends only Cloud profile authority", func(t *testing.T) {
		llm := &budgetCaptureLLMClient{responses: []*client.CompletionResponse{{
			OutputText: "done", FinishReason: "end_turn",
		}}}
		loop := NewAgentLoop(llm, NewToolRegistry(), "large", "", 3, 1000, 100, nil, nil, nil)
		loop.SetSpecificModel("claude-sonnet-5")
		loop.SetReasoningEffort("high")
		loop.SetEffortTier("xhigh")
		loop.SetThinking(&client.ThinkingConfig{Type: "enabled", BudgetTokens: 4096})
		loop.SetKoeExecutionProfile(fastProfileForAgentTest())
		if loop.SpecificModel() != "claude-sonnet-5" || loop.EffortTier() != "xhigh" {
			t.Fatal("SetKoeExecutionProfile mutated user Agent configuration")
		}
		if _, _, err := loop.Run(context.Background(), "work", nil, nil); err != nil {
			t.Fatal(err)
		}
		req := llm.requests[0]
		if req.ModelTier != "" || req.SpecificModel != "" ||
			req.ReasoningEffort != "" || req.EffortTier != "" {
			t.Fatalf("fast request carried caller model/effort selectors: %+v", req)
		}
		if req.Thinking != nil {
			t.Fatalf("fast request carried caller thinking selector: %+v", req.Thinking)
		}
		if req.ExecutionProfileID != "kfp1_agent-test" || !req.ParallelToolCalls ||
			req.ResponseCachePolicy != executionprofile.ResponseCacheOff {
			t.Fatalf("fast profile wire = %+v", req)
		}
		if loop.toolRefSupported {
			t.Fatal("Fast harness must use the deterministic deferred-tool protocol")
		}
		if req.MaxTokens != 128_000 {
			t.Fatalf("fast max_tokens = %d, want Luna cap 128000", req.MaxTokens)
		}
	})
}

type executionEvidenceTool struct {
	runs atomic.Int32
}

func (t *executionEvidenceTool) Info() ToolInfo {
	return ToolInfo{
		Name:        "write_probe",
		Description: "write probe",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"value": map[string]any{"type": "string"},
			},
			"required": []string{"value"},
		},
	}
}

func (t *executionEvidenceTool) Run(context.Context, string) (ToolResult, error) {
	t.runs.Add(1)
	return ToolResult{Content: "persisted"}, nil
}

func (t *executionEvidenceTool) RequiresApproval() bool { return false }

func TestAgentLoopExecutionProfileContinuityAndSideEffectEvidence(t *testing.T) {
	llm := &budgetCaptureLLMClient{responses: []*client.CompletionResponse{
		{
			FinishReason: "tool_use",
			FunctionCall: &client.FunctionCall{
				ID: "call-write", Name: "write_probe", Arguments: json.RawMessage(`{"value":"x"}`),
			},
		},
		{OutputText: "done", FinishReason: "end_turn"},
	}}
	tool := &executionEvidenceTool{}
	reg := NewToolRegistry()
	reg.Register(tool)
	loop := NewAgentLoop(llm, reg, "medium", "", 4, 1000, 100, nil, nil, nil)
	loop.SetKoeExecutionProfile(fastProfileForAgentTest())
	var checkpointEvidence executionprofile.Evidence
	loop.SetCheckpointFunc(func(context.Context) error {
		checkpointEvidence = loop.ExecutionEvidence()
		return nil
	})
	if _, _, err := loop.Run(context.Background(), "write once", nil, nil); err != nil {
		t.Fatal(err)
	}
	if tool.runs.Load() != 1 {
		t.Fatalf("write tool executed %d times, want once", tool.runs.Load())
	}
	if len(llm.requests) != 2 {
		t.Fatalf("completion calls = %d, want 2", len(llm.requests))
	}
	for i, req := range llm.requests {
		if req.ExecutionProfileID != "kfp1_agent-test" || !req.ParallelToolCalls ||
			req.ResponseCachePolicy != executionprofile.ResponseCacheOff ||
			req.SpecificModel != "" || req.ReasoningEffort != "" || req.EffortTier != "" ||
			req.Thinking != nil {
			t.Fatalf("request %d profile drifted: %+v", i, req)
		}
	}
	if len(checkpointEvidence.ToolOutcomes) != 1 {
		t.Fatalf("checkpoint evidence = %+v", checkpointEvidence)
	}
	evidence := checkpointEvidence.ToolOutcomes[0]
	if !evidence.Validated || evidence.Outcome != "succeeded" || !evidence.SideEffect ||
		evidence.ToolCallID != "call-write" || evidence.ArgumentsDigest == "" ||
		evidence.ResultDigest == "" {
		t.Fatalf("tool evidence = %+v", evidence)
	}

	// Recovery receives the persisted tool_use/tool_result transcript plus the
	// provider-neutral evidence. Even if the provider emits the same action
	// again under a fresh call ID, the loop must fail that call closed without
	// dispatching the historical write a second time.
	resumeLLM := &budgetCaptureLLMClient{responses: []*client.CompletionResponse{
		{
			FinishReason: "tool_use",
			FunctionCall: &client.FunctionCall{
				ID: "call-write-replayed", Name: "write_probe",
				Arguments: json.RawMessage(`{"value":"x"}`),
			},
		},
		{OutputText: "resumed", FinishReason: "end_turn"},
	}}
	resume := NewAgentLoop(resumeLLM, reg, "medium", "", 4, 1000, 100, nil, nil, nil)
	resume.SetKoeExecutionProfile(fastProfileForAgentTest())
	resume.SetExecutionEvidence(checkpointEvidence)
	if _, _, err := resume.ResumeInterrupted(
		context.Background(), "continue from checkpoint", loop.RunMessages(),
	); err != nil {
		t.Fatal(err)
	}
	if tool.runs.Load() != 1 {
		t.Fatalf("recovery replayed side-effecting tool; executions=%d", tool.runs.Load())
	}
	resumeEvidence := resume.ExecutionEvidence()
	if len(resumeEvidence.ToolOutcomes) != 2 {
		t.Fatalf("recovery evidence = %+v, want persisted success plus replay block", resumeEvidence)
	}
	blocked := resumeEvidence.ToolOutcomes[1]
	if !blocked.Validated || blocked.Outcome != "failed" || blocked.SideEffect ||
		blocked.PermissionDecision != "replay_blocked" ||
		blocked.ArgumentsDigest != evidence.ArgumentsDigest {
		t.Fatalf("replay-block evidence = %+v", blocked)
	}

	// The replay key is argument-specific. A genuinely different write in a
	// resumed run must still be able to execute.
	distinctLLM := &budgetCaptureLLMClient{responses: []*client.CompletionResponse{
		{
			FinishReason: "tool_use",
			FunctionCall: &client.FunctionCall{
				ID: "call-write-distinct", Name: "write_probe",
				Arguments: json.RawMessage(`{"value":"y"}`),
			},
		},
		{OutputText: "distinct write done", FinishReason: "end_turn"},
	}}
	distinct := NewAgentLoop(distinctLLM, reg, "medium", "", 4, 1000, 100, nil, nil, nil)
	distinct.SetKoeExecutionProfile(fastProfileForAgentTest())
	distinct.SetExecutionEvidence(checkpointEvidence)
	if _, _, err := distinct.ResumeInterrupted(
		context.Background(), "continue with a different write", loop.RunMessages(),
	); err != nil {
		t.Fatal(err)
	}
	if tool.runs.Load() != 2 {
		t.Fatalf("distinct resumed side effect executions=%d, want 2 total", tool.runs.Load())
	}
	distinctEvidence := distinct.ExecutionEvidence()
	if len(distinctEvidence.ToolOutcomes) != 2 ||
		!distinctEvidence.ToolOutcomes[1].SideEffect ||
		distinctEvidence.ToolOutcomes[1].ArgumentsDigest == evidence.ArgumentsDigest {
		t.Fatalf("distinct side-effect evidence = %+v", distinctEvidence)
	}
}
