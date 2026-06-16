package agents

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// LocalizedString is an open map of BCP-47 short locale id → translated text.
// Keys are not validated (any string accepted); resolution-time fallback is
// performed by the client. Daemon ships at minimum "en" for every value, but
// this is convention, not schema.
type LocalizedString map[string]string

// AgentProfile is the user-facing presentation metadata loaded from
// <agentDir>/PROFILE.yaml. All fields optional; an absent PROFILE.yaml yields
// a nil profile.
//
// The profile is decoupled from config.yaml so runtime config (tools / MCP /
// heartbeat / etc.) stays free of multi-language presentation strings, and
// so callers that only need the runtime config don't pay the cost of
// unmarshalling examples / guide prompts.
type AgentProfile struct {
	// Category is just the code (slug); the daemon resolves the label via
	// CategoryRegistry at API-serialization time. Empty string means no
	// category — handled the same as the field being absent in the yaml.
	Category string `yaml:"category,omitempty"`

	// Avatar is the agent's avatar image URL (CDN). Non-localized. Empty
	// means no avatar — handled the same as the field being absent. Set via
	// PROFILE.yaml `avatar:` or the create/update agent API; synced to Cloud.
	Avatar string `yaml:"avatar,omitempty"`

	// Description: single localized blurb shown on the agent profile surface.
	Description LocalizedString `yaml:"description,omitempty"`

	// GuidePrompts: clickable starter cards shown in the chat empty state.
	// Order preserved.
	GuidePrompts []GuidePrompt `yaml:"guide_prompts,omitempty"`

	// Examples: scripted multi-turn dialogues shown on the agent profile
	// surface. Read-only display; the model never sees them.
	Examples []AgentExample `yaml:"examples,omitempty"`
}

// GuidePrompt is one clickable starter card.
type GuidePrompt struct {
	Title  LocalizedString `yaml:"title" json:"title"`
	Prompt LocalizedString `yaml:"prompt" json:"prompt"`
}

// AgentExample is one multi-turn dialogue used to demonstrate the agent.
type AgentExample struct {
	Title LocalizedString `yaml:"title,omitempty" json:"title,omitempty"`
	Turns []ExampleTurn   `yaml:"turns" json:"turns"`
}

// ExampleTurn is one turn in an example dialogue. `Text` is used for `user`
// turns, `Markdown` and `ToolRuns` for `assistant` turns. These are not
// enforced as a discriminated-union at the wire level so the schema stays
// flat for hand-authoring — they are validated by AgentProfile.Validate.
type ExampleTurn struct {
	Role     string           `yaml:"role" json:"role"`
	Text     LocalizedString  `yaml:"text,omitempty" json:"text,omitempty"`
	Markdown LocalizedString  `yaml:"markdown,omitempty" json:"markdown,omitempty"`
	ToolRuns []ToolRunSummary `yaml:"tool_runs,omitempty" json:"tool_runs,omitempty"`
}

// Role values for ExampleTurn.
const (
	ExampleRoleUser      = "user"
	ExampleRoleAssistant = "assistant"
)

// ToolRunSummary is one compact chip describing a tool call inside an example
// assistant turn. The chip is purely informational — there is no expanded
// output schema (yet). Tool names are free-form strings; the client decides
// how to render unknown tools.
type ToolRunSummary struct {
	Tool    string          `yaml:"tool" json:"tool"`
	Summary LocalizedString `yaml:"summary" json:"summary"`
}

// LoadAgentProfile reads <dir>/PROFILE.yaml. Returns (nil, nil) when the file
// is absent. Returns an error when the file exists but is malformed or
// semantically invalid.
func LoadAgentProfile(dir string) (*AgentProfile, error) {
	path := filepath.Join(dir, "PROFILE.yaml")
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var p AgentProfile
	if err := yaml.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("parse PROFILE.yaml: %w", err)
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return &p, nil
}

// Validate enforces the semantic invariants of a parsed profile that yaml
// unmarshal cannot enforce on its own. UI-visible fields must contain at least
// one non-empty localized value, and example turns behave like a flat
// discriminated union:
//   - user turns require text and reject markdown/tool_runs.
//   - assistant turns require markdown, may include tool_runs, and reject text.
//
// Category code validity is checked by the caller against CategoryRegistry()
// (LoadAgent does this) so a missing registry is reported once, not for each
// profile we parse.
func (p *AgentProfile) Validate() error {
	if p == nil {
		return nil
	}
	if len(p.Description) > 0 && !hasLocalizedText(p.Description) {
		return fmt.Errorf("description missing text")
	}
	for gi, prompt := range p.GuidePrompts {
		if !hasLocalizedText(prompt.Title) {
			return fmt.Errorf("guide_prompts[%d]: missing title", gi)
		}
		if !hasLocalizedText(prompt.Prompt) {
			return fmt.Errorf("guide_prompts[%d]: missing prompt", gi)
		}
	}
	for ei, ex := range p.Examples {
		if len(ex.Title) > 0 && !hasLocalizedText(ex.Title) {
			return fmt.Errorf("examples[%d]: title missing text", ei)
		}
		if len(ex.Turns) == 0 {
			return fmt.Errorf("examples[%d]: missing turns", ei)
		}
		for ti, turn := range ex.Turns {
			path := fmt.Sprintf("examples[%d].turns[%d]", ei, ti)
			switch turn.Role {
			case ExampleRoleUser:
				if !hasLocalizedText(turn.Text) {
					return fmt.Errorf("examples[%d].turns[%d]: user turn missing text", ei, ti)
				}
				if len(turn.Markdown) > 0 {
					return fmt.Errorf("%s: user turn cannot include markdown", path)
				}
				if len(turn.ToolRuns) > 0 {
					return fmt.Errorf("%s: user turn cannot include tool_runs", path)
				}
			case ExampleRoleAssistant:
				if !hasLocalizedText(turn.Markdown) {
					return fmt.Errorf("examples[%d].turns[%d]: assistant turn missing markdown", ei, ti)
				}
				if len(turn.Text) > 0 {
					return fmt.Errorf("%s: assistant turn cannot include text", path)
				}
				for ri, run := range turn.ToolRuns {
					if strings.TrimSpace(run.Tool) == "" {
						return fmt.Errorf("%s.tool_runs[%d]: missing tool", path, ri)
					}
					if !hasLocalizedText(run.Summary) {
						return fmt.Errorf("%s.tool_runs[%d]: missing summary", path, ri)
					}
				}
			default:
				return fmt.Errorf("examples[%d].turns[%d]: invalid role %q (must be %q or %q)",
					ei, ti, turn.Role, ExampleRoleUser, ExampleRoleAssistant)
			}
		}
	}
	return nil
}

func hasLocalizedText(v LocalizedString) bool {
	for _, text := range v {
		if strings.TrimSpace(text) != "" {
			return true
		}
	}
	return false
}
