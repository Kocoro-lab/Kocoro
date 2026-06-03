package agents

import (
	"fmt"
	"strings"
)

// BuiltinCommands is the set of slash command names reserved by the TUI.
// Agent commands and skills must not use these names.
var BuiltinCommands = map[string]bool{
	"quit": true, "exit": true, "help": true, "clear": true,
	"sessions": true, "session": true, "model": true, "config": true,
	"setup": true, "update": true, "copy": true, "research": true,
	"swarm": true, "search": true, "dag": true,
}

// ValidateCommandName checks that a command/skill name is valid and doesn't
// collide with built-in slash commands.
func ValidateCommandName(name string) error {
	if err := ValidateAgentName(name); err != nil {
		return fmt.Errorf("invalid command name: %w", err)
	}
	if BuiltinCommands[name] {
		return fmt.Errorf("command name %q conflicts with built-in slash command", name)
	}
	return nil
}

// ValidateToolsFilter checks that allow and deny are not both set.
func ValidateToolsFilter(f *AgentToolsFilter) error {
	if f == nil {
		return nil
	}
	if len(f.Allow) > 0 && len(f.Deny) > 0 {
		return fmt.Errorf("tools.allow and tools.deny are mutually exclusive")
	}
	return nil
}

// modelTierKeywords are the routing-tier names. They belong in model_tier, NOT
// model — agent.model is a specific model id forwarded to the Gateway as
// specific_model. A tier word placed there is sent verbatim and rejected at
// LLM-call time with an opaque "model_id_unknown" 400 (a stuck named agent).
// See the precedence chain in references/agents.md.
var modelTierKeywords = map[string]bool{"small": true, "medium": true, "large": true}

// IsModelTierKeyword reports whether s is a routing-tier name (small/medium/
// large). It is the single source of truth for "this is a tier, not a model id"
// across every config write boundary. Matching is case- and whitespace-
// insensitive: no real model id is ever one of these words, so normalizing
// catches copy-paste/typo variants (`Large`, ` large`) that would otherwise hit
// the very model_id_unknown failure this guard exists to prevent.
func IsModelTierKeyword(s string) bool {
	return modelTierKeywords[strings.ToLower(strings.TrimSpace(s))]
}

// ValidateAgentModelConfig rejects a tier keyword wedged into agent.model. Both
// model and model_tier are free-form strings on the wire, so without this guard
// `{"model": "large"}` persists silently and only fails far downstream.
func ValidateAgentModelConfig(c *AgentModelConfig) error {
	if c == nil || c.Model == nil {
		return nil
	}
	if IsModelTierKeyword(*c.Model) {
		return fmt.Errorf("agent.model expects a specific model id (e.g. \"claude-opus-4-8\"), not the tier %q; use agent.model_tier for tiers", *c.Model)
	}
	return nil
}
