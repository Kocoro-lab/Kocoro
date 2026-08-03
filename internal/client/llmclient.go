package client

import (
	"context"

	"github.com/Kocoro-lab/ShanClaw/internal/executionprofile"
)

// LLMClient is the common interface for LLM completion backends.
// Satisfied by *GatewayClient and *OllamaClient.
type LLMClient interface {
	Complete(ctx context.Context, req CompletionRequest) (*CompletionResponse, error)
	CompleteStream(ctx context.Context, req CompletionRequest, onDelta func(StreamDelta)) (*CompletionResponse, error)
}

// ComputerExecutionProfileRequest pins computer use to the exact route that
// produced the tool_search decision. AllowModelFallback must remain false: a
// tool-selection continuation cannot silently switch provider or model.
type ComputerExecutionProfileRequest struct {
	SchemaVersion        int    `json:"schema_version"`
	ModelTier            string `json:"model_tier,omitempty"`
	SpecificModel        string `json:"specific_model,omitempty"`
	Capability           string `json:"capability"`
	RequiredToolContract string `json:"required_tool_contract"`
	AllowModelFallback   bool   `json:"allow_model_fallback"`
}

// ComputerExecutionProfileResolver is optional so local backends and test
// clients can keep implementing only LLMClient. GatewayClient implements it.
type ComputerExecutionProfileResolver interface {
	ResolveComputerExecutionProfile(context.Context, ComputerExecutionProfileRequest) (executionprofile.Profile, error)
}
