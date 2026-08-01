package agent

import "github.com/Kocoro-lab/ShanClaw/internal/client"

// ExecutionConfig is the narrow, provider-neutral AgentLoop configuration
// needed to reconstruct an interrupted run. It deliberately excludes prompts,
// tools, credentials, provider response state, and execution-profile routing.
//
// Scalar fields intentionally do not use omitempty: when the containing
// checkpoint field is present, zero values are part of the authoritative
// snapshot rather than an instruction to rediscover current configuration.
type ExecutionConfig struct {
	SpecificModel         string                 `json:"specific_model"`
	ModelTier             string                 `json:"model_tier"`
	Thinking              *client.ThinkingConfig `json:"thinking,omitempty"`
	ReasoningEffort       string                 `json:"reasoning_effort"`
	EffortTier            string                 `json:"effort_tier"`
	ServiceTier           string                 `json:"service_tier"`
	ResponseLanguage      string                 `json:"response_language"`
	Temperature           float64                `json:"temperature"`
	MaxTokens             int                    `json:"max_tokens"`
	ContextWindow         int                    `json:"context_window"`
	ContextWindowExplicit bool                   `json:"context_window_explicit"`
	MaxIterations         int                    `json:"max_iterations"`
}

// CloneExecutionConfig returns an ownership-independent copy. A nil snapshot
// represents a legacy checkpoint and remains nil.
func CloneExecutionConfig(config *ExecutionConfig) *ExecutionConfig {
	if config == nil {
		return nil
	}
	cloned := *config
	if config.Thinking != nil {
		thinking := *config.Thinking
		cloned.Thinking = &thinking
	}
	return &cloned
}

// Thinking returns an ownership-independent copy of the configured thinking
// selector.
func (a *AgentLoop) Thinking() *client.ThinkingConfig {
	if a == nil || a.thinking == nil {
		return nil
	}
	thinking := *a.thinking
	return &thinking
}

// ReasoningEffort returns the configured provider-native reasoning effort.
func (a *AgentLoop) ReasoningEffort() string {
	if a == nil {
		return ""
	}
	return a.reasoningEffort
}

// Temperature returns the configured sampling temperature.
func (a *AgentLoop) Temperature() float64 {
	if a == nil {
		return 0
	}
	return a.temperature
}

// MaxTokens returns the raw configured output-token limit. Zero remains zero;
// model-specific fallback is resolved later by effectiveMaxTokens.
func (a *AgentLoop) MaxTokens() int {
	if a == nil {
		return 0
	}
	return a.maxTokens
}

// ContextWindow returns both the active token window and whether user config
// explicitly locked it against model auto-detection.
func (a *AgentLoop) ContextWindow() (tokens int, explicit bool) {
	if a == nil {
		return 0, false
	}
	return a.contextWindow, a.contextWindowExplicit
}

// MaxIterations returns the configured agent-loop iteration limit.
func (a *AgentLoop) MaxIterations() int {
	if a == nil {
		return 0
	}
	return a.maxIter
}

// ExecutionConfig returns an ownership-independent snapshot of the resolved
// Agent configuration. Callers must capture it before applying a transient
// execution profile such as Koe Fast.
func (a *AgentLoop) ExecutionConfig() ExecutionConfig {
	if a == nil {
		return ExecutionConfig{}
	}
	contextWindow, contextWindowExplicit := a.ContextWindow()
	return ExecutionConfig{
		SpecificModel:         a.SpecificModel(),
		ModelTier:             a.ModelTier(),
		Thinking:              a.Thinking(),
		ReasoningEffort:       a.ReasoningEffort(),
		EffortTier:            a.EffortTier(),
		ServiceTier:           a.ServiceTier(),
		ResponseLanguage:      a.ResponseLanguage(),
		Temperature:           a.Temperature(),
		MaxTokens:             a.MaxTokens(),
		ContextWindow:         contextWindow,
		ContextWindowExplicit: contextWindowExplicit,
		MaxIterations:         a.MaxIterations(),
	}
}

// ApplyExecutionConfig restores every snapshotted field exactly, including
// empty strings, zero values, nil thinking, and the context-window lock bit.
func (a *AgentLoop) ApplyExecutionConfig(config ExecutionConfig) {
	if a == nil {
		return
	}
	a.SetSpecificModel(config.SpecificModel)
	a.SetModelTier(config.ModelTier)
	var thinking *client.ThinkingConfig
	if config.Thinking != nil {
		cloned := *config.Thinking
		thinking = &cloned
	}
	a.SetThinking(thinking)
	a.SetReasoningEffort(config.ReasoningEffort)
	a.SetEffortTier(config.EffortTier)
	a.SetServiceTier(config.ServiceTier)
	a.SetResponseLanguage(config.ResponseLanguage)
	a.SetTemperature(config.Temperature)
	a.SetMaxTokens(config.MaxTokens)
	a.SetContextWindow(config.ContextWindow)
	if config.ContextWindowExplicit {
		a.SetContextWindowExplicit(config.ContextWindow)
	}
	a.SetMaxIterations(config.MaxIterations)
}
