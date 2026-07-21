package agents

import "testing"

func TestValidateCommandName(t *testing.T) {
	// Valid names
	for _, name := range []string{"review", "deploy", "my-cmd"} {
		if err := ValidateCommandName(name); err != nil {
			t.Errorf("expected valid: %q, got %v", name, err)
		}
	}
	// Built-in collision
	for _, name := range []string{"help", "quit", "copy", "search"} {
		if err := ValidateCommandName(name); err == nil {
			t.Errorf("expected error for built-in %q", name)
		}
	}
	// Invalid charset
	if err := ValidateCommandName("UPPER"); err == nil {
		t.Error("expected error for uppercase")
	}
}

func TestValidateToolsFilter(t *testing.T) {
	// nil is ok
	if err := ValidateToolsFilter(nil); err != nil {
		t.Errorf("nil should be valid: %v", err)
	}
	// allow only is ok
	if err := ValidateToolsFilter(&AgentToolsFilter{Allow: []string{"bash"}}); err != nil {
		t.Errorf("allow-only should be valid: %v", err)
	}
	// both set is error
	if err := ValidateToolsFilter(&AgentToolsFilter{Allow: []string{"a"}, Deny: []string{"b"}}); err == nil {
		t.Error("expected error when both allow and deny set")
	}
}

func TestValidateAgentModelConfig(t *testing.T) {
	ptr := func(s string) *string { return &s }

	// nil config / nil model are fine.
	if err := ValidateAgentModelConfig(nil); err != nil {
		t.Errorf("nil config should be valid: %v", err)
	}
	if err := ValidateAgentModelConfig(&AgentModelConfig{}); err != nil {
		t.Errorf("nil model should be valid: %v", err)
	}

	// A specific model id is fine; model_tier holding a tier is fine.
	if err := ValidateAgentModelConfig(&AgentModelConfig{Model: ptr("claude-opus-4-8")}); err != nil {
		t.Errorf("specific model id should be valid: %v", err)
	}
	if err := ValidateAgentModelConfig(&AgentModelConfig{ModelTier: ptr("large")}); err != nil {
		t.Errorf("tier in model_tier should be valid: %v", err)
	}

	// A tier keyword in agent.model is the bug we guard against — including
	// cased / whitespace-padded copy-paste variants.
	for _, tier := range []string{"small", "medium", "large", "Large", " large", "MEDIUM", "Small "} {
		if err := ValidateAgentModelConfig(&AgentModelConfig{Model: ptr(tier)}); err == nil {
			t.Errorf("expected error for tier %q in agent.model", tier)
		}
	}

	// nil EffortTier is fine (inherit); "" is fine (explicit inherit); each
	// valid tier is fine.
	if err := ValidateAgentModelConfig(&AgentModelConfig{EffortTier: nil}); err != nil {
		t.Errorf("nil effort_tier should be valid: %v", err)
	}
	for _, tier := range []string{"", "low", "high", "xhigh", "max"} {
		if err := ValidateAgentModelConfig(&AgentModelConfig{EffortTier: ptr(tier)}); err != nil {
			t.Errorf("effort_tier %q should be valid: %v", tier, err)
		}
	}

	// An out-of-enum effort_tier is the bug we guard against — a bad value
	// would otherwise reach the LLM provider and fail with an obscure remote
	// 400.
	for _, tier := range []string{"medium", "Low", " low", "LOW", "extreme"} {
		if err := ValidateAgentModelConfig(&AgentModelConfig{EffortTier: ptr(tier)}); err == nil {
			t.Errorf("expected error for invalid effort_tier %q", tier)
		}
	}
}

func TestIsValidEffortTier(t *testing.T) {
	for _, tier := range []string{"", "low", "high", "xhigh", "max"} {
		if !IsValidEffortTier(tier) {
			t.Errorf("expected %q to be valid", tier)
		}
	}
	for _, tier := range []string{"medium", "Low", " low", "LOW", "extreme"} {
		if IsValidEffortTier(tier) {
			t.Errorf("expected %q to be invalid", tier)
		}
	}
}
