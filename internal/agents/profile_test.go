package agents

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeYAML writes a PROFILE.yaml at <dir>/PROFILE.yaml with the given body.
func writeYAML(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "PROFILE.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadAgentProfile_Missing(t *testing.T) {
	// No PROFILE.yaml present → (nil, nil).
	dir := t.TempDir()
	p, err := LoadAgentProfile(dir)
	if err != nil {
		t.Fatalf("expected nil error for missing file, got %v", err)
	}
	if p != nil {
		t.Errorf("expected nil profile for missing file, got %+v", p)
	}
}

func TestLoadAgentProfile_Full(t *testing.T) {
	dir := t.TempDir()
	writeYAML(t, dir, `
category: coding
description:
  en: An agent.
  zh-Hans: 一个智能体。
  ja: エージェント。
guide_prompts:
  - title:
      en: Find auth code
      zh-Hans: 找认证代码
    prompt:
      en: Where is the authentication logic?
      zh-Hans: 认证逻辑在哪里？
examples:
  - title:
      en: Trace a flaky test
    turns:
      - role: user
        text:
          en: Why does this fail?
      - role: assistant
        markdown:
          en: Let me investigate.
        tool_runs:
          - tool: grep
            summary:
              en: Searched src/
`)
	p, err := LoadAgentProfile(dir)
	if err != nil {
		t.Fatalf("LoadAgentProfile: %v", err)
	}
	if p == nil {
		t.Fatal("expected non-nil profile")
	}
	if p.Category != "coding" {
		t.Errorf("Category=%q, want coding", p.Category)
	}
	if p.Description["en"] != "An agent." {
		t.Errorf("Description.en=%q", p.Description["en"])
	}
	if len(p.GuidePrompts) != 1 {
		t.Fatalf("GuidePrompts len=%d, want 1", len(p.GuidePrompts))
	}
	if p.GuidePrompts[0].Title["en"] != "Find auth code" {
		t.Errorf("GuidePrompts[0].Title.en=%q", p.GuidePrompts[0].Title["en"])
	}
	if len(p.Examples) != 1 {
		t.Fatalf("Examples len=%d, want 1", len(p.Examples))
	}
	ex := p.Examples[0]
	if len(ex.Turns) != 2 {
		t.Fatalf("Turns len=%d, want 2", len(ex.Turns))
	}
	if ex.Turns[0].Role != "user" || ex.Turns[0].Text["en"] != "Why does this fail?" {
		t.Errorf("Turn 0: %+v", ex.Turns[0])
	}
	if ex.Turns[1].Role != "assistant" || ex.Turns[1].Markdown["en"] != "Let me investigate." {
		t.Errorf("Turn 1: %+v", ex.Turns[1])
	}
	if len(ex.Turns[1].ToolRuns) != 1 || ex.Turns[1].ToolRuns[0].Tool != "grep" {
		t.Errorf("Turn 1 tool_runs: %+v", ex.Turns[1].ToolRuns)
	}
}

func TestLoadAgentProfile_MinimalCategoryOnly(t *testing.T) {
	// Only `category` set; description / guide_prompts / examples absent.
	dir := t.TempDir()
	writeYAML(t, dir, "category: writing\n")
	p, err := LoadAgentProfile(dir)
	if err != nil {
		t.Fatalf("LoadAgentProfile: %v", err)
	}
	if p == nil || p.Category != "writing" {
		t.Fatalf("expected category=writing, got %+v", p)
	}
	if p.Description != nil {
		t.Errorf("Description should be nil, got %v", p.Description)
	}
	if p.GuidePrompts != nil {
		t.Errorf("GuidePrompts should be nil, got %v", p.GuidePrompts)
	}
	if p.Examples != nil {
		t.Errorf("Examples should be nil, got %v", p.Examples)
	}
}

func TestLoadAgentProfile_InvalidYAML(t *testing.T) {
	dir := t.TempDir()
	writeYAML(t, dir, "category: coding\n  bad: indent\n")
	_, err := LoadAgentProfile(dir)
	if err == nil {
		t.Fatal("expected parse error, got nil")
	}
	if !strings.Contains(err.Error(), "parse PROFILE.yaml") {
		t.Errorf("error should mention 'parse PROFILE.yaml', got: %v", err)
	}
}

func requireProfileValidateError(t *testing.T, p *AgentProfile, want string) {
	t.Helper()
	err := p.Validate()
	if err == nil {
		t.Fatalf("expected error containing %q, got nil", want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("error should contain %q, got: %v", want, err)
	}
}

func TestProfile_Validate_DescriptionIfPresentRequiresText(t *testing.T) {
	p := &AgentProfile{
		Description: LocalizedString{"en": " \t"},
	}
	requireProfileValidateError(t, p, "description missing text")
}

func TestProfile_Validate_GuidePromptsRequireUIFields(t *testing.T) {
	cases := []struct {
		name string
		p    *AgentProfile
		want string
	}{
		{
			name: "missing title",
			p: &AgentProfile{
				GuidePrompts: []GuidePrompt{{
					Prompt: LocalizedString{"en": "Where is auth?"},
				}},
			},
			want: "guide_prompts[0]: missing title",
		},
		{
			name: "blank prompt",
			p: &AgentProfile{
				GuidePrompts: []GuidePrompt{{
					Title:  LocalizedString{"en": "Find auth"},
					Prompt: LocalizedString{"en": "   "},
				}},
			},
			want: "guide_prompts[0]: missing prompt",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			requireProfileValidateError(t, tc.p, tc.want)
		})
	}
}

func TestProfile_Validate_BadRole(t *testing.T) {
	p := &AgentProfile{
		Examples: []AgentExample{{
			Turns: []ExampleTurn{{Role: "system", Text: LocalizedString{"en": "ignore"}}},
		}},
	}
	err := p.Validate()
	if err == nil {
		t.Fatal("expected error for invalid role, got nil")
	}
	if !strings.Contains(err.Error(), "invalid role") {
		t.Errorf("error should mention 'invalid role', got: %v", err)
	}
}

func TestProfile_Validate_ExampleRequiresTurns(t *testing.T) {
	p := &AgentProfile{
		Examples: []AgentExample{{}},
	}
	requireProfileValidateError(t, p, "examples[0]: missing turns")
}

func TestProfile_Validate_ExampleTitleIfPresentRequiresText(t *testing.T) {
	p := &AgentProfile{
		Examples: []AgentExample{{
			Title: LocalizedString{"en": ""},
			Turns: []ExampleTurn{{Role: "user", Text: LocalizedString{"en": "hi"}}},
		}},
	}
	requireProfileValidateError(t, p, "examples[0]: title missing text")
}

func TestProfile_Validate_UserMissingText(t *testing.T) {
	p := &AgentProfile{
		Examples: []AgentExample{{
			Turns: []ExampleTurn{{Role: "user"}}, // no text
		}},
	}
	err := p.Validate()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "user turn missing text") {
		t.Errorf("error should mention missing text, got: %v", err)
	}
}

func TestProfile_Validate_UserBlankText(t *testing.T) {
	p := &AgentProfile{
		Examples: []AgentExample{{
			Turns: []ExampleTurn{{Role: "user", Text: LocalizedString{"en": "  "}}},
		}},
	}
	requireProfileValidateError(t, p, "user turn missing text")
}

func TestProfile_Validate_AssistantMissingMarkdown(t *testing.T) {
	p := &AgentProfile{
		Examples: []AgentExample{{
			Turns: []ExampleTurn{{Role: "assistant"}}, // no markdown
		}},
	}
	err := p.Validate()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "assistant turn missing markdown") {
		t.Errorf("error should mention missing markdown, got: %v", err)
	}
}

func TestProfile_Validate_RoleFieldsAreMutuallyExclusive(t *testing.T) {
	cases := []struct {
		name string
		turn ExampleTurn
		want string
	}{
		{
			name: "user with markdown",
			turn: ExampleTurn{
				Role:     "user",
				Text:     LocalizedString{"en": "hi"},
				Markdown: LocalizedString{"en": "assistant-only"},
			},
			want: "user turn cannot include markdown",
		},
		{
			name: "user with tool runs",
			turn: ExampleTurn{
				Role:     "user",
				Text:     LocalizedString{"en": "hi"},
				ToolRuns: []ToolRunSummary{{Tool: "grep", Summary: LocalizedString{"en": "searched"}}},
			},
			want: "user turn cannot include tool_runs",
		},
		{
			name: "assistant with text",
			turn: ExampleTurn{
				Role:     "assistant",
				Text:     LocalizedString{"en": "user-only"},
				Markdown: LocalizedString{"en": "hi back"},
			},
			want: "assistant turn cannot include text",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &AgentProfile{
				Examples: []AgentExample{{
					Turns: []ExampleTurn{tc.turn},
				}},
			}
			requireProfileValidateError(t, p, tc.want)
		})
	}
}

func TestProfile_Validate_ToolRunsRequireUIFields(t *testing.T) {
	cases := []struct {
		name string
		run  ToolRunSummary
		want string
	}{
		{
			name: "missing tool",
			run:  ToolRunSummary{Summary: LocalizedString{"en": "searched"}},
			want: "tool_runs[0]: missing tool",
		},
		{
			name: "blank summary",
			run:  ToolRunSummary{Tool: "grep", Summary: LocalizedString{"en": " "}},
			want: "tool_runs[0]: missing summary",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &AgentProfile{
				Examples: []AgentExample{{
					Turns: []ExampleTurn{{
						Role:     "assistant",
						Markdown: LocalizedString{"en": "hi"},
						ToolRuns: []ToolRunSummary{tc.run},
					}},
				}},
			}
			requireProfileValidateError(t, p, tc.want)
		})
	}
}

func TestProfile_Validate_OK(t *testing.T) {
	p := &AgentProfile{
		Description: LocalizedString{"en": "Useful profile."},
		GuidePrompts: []GuidePrompt{{
			Title:  LocalizedString{"en": "Start"},
			Prompt: LocalizedString{"en": "Help me start."},
		}},
		Examples: []AgentExample{{
			Title: LocalizedString{"en": "Example"},
			Turns: []ExampleTurn{
				{Role: "user", Text: LocalizedString{"en": "hi"}},
				{
					Role:     "assistant",
					Markdown: LocalizedString{"en": "hi back"},
					ToolRuns: []ToolRunSummary{{
						Tool:    "grep",
						Summary: LocalizedString{"en": "searched"},
					}},
				},
			},
		}},
	}
	if err := p.Validate(); err != nil {
		t.Errorf("expected nil error for valid profile, got %v", err)
	}
}

// TestLoadAgent_RejectsUnknownCategoryCode covers the integration with
// LoadAgent: a typo in the category code should fail-loud at agent load,
// not produce a silently-dropped category field in the API.
func TestLoadAgent_RejectsUnknownCategoryCode(t *testing.T) {
	root := t.TempDir()
	agentDir := filepath.Join(root, "demo")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "AGENT.md"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeYAML(t, agentDir, "category: not-a-real-category\n")

	_, err := LoadAgent(root, "demo")
	if err == nil {
		t.Fatal("expected error for unknown category code, got nil")
	}
	if !strings.Contains(err.Error(), "unknown category") {
		t.Errorf("error should mention unknown category, got: %v", err)
	}
}

// TestLoadAgent_AllBuiltinsHaveValidProfile sweeps every name in BuiltinNames
// and asserts each one's bundled PROFILE.yaml parses + validates. This is
// the YAML-syntax safety net: a malformed builtin (unquoted CJK with bare
// `: `, bad indentation, invalid role, missing required text) was caught by
// the daemon at runtime returning 500 from /agents/{name}, NOT by the test
// suite — see the 2026-06-15 reviewer-PROFILE incident. Any new builtin
// added to BuiltinNames automatically picks up this check.
func TestLoadAgent_AllBuiltinsHaveValidProfile(t *testing.T) {
	root := t.TempDir()
	if err := EnsureBuiltins(root, "test-version"); err != nil {
		t.Fatal(err)
	}
	for _, name := range BuiltinNames {
		t.Run(name, func(t *testing.T) {
			a, err := LoadAgent(root, name)
			if err != nil {
				t.Fatalf("LoadAgent %s: %v", name, err)
			}
			if a.Profile == nil {
				t.Fatalf("expected %s to have a profile", name)
			}
			// Category must resolve through the registry. ResolveCategory is
			// called from LoadAgent above, so a typo'd code would have failed
			// the LoadAgent — re-checking here makes the dependency explicit.
			if a.Profile.Category == "" {
				t.Errorf("%s: empty category code", name)
			} else if _, err := ResolveCategory(a.Profile.Category); err != nil {
				t.Errorf("%s: category %q does not resolve: %v", name, a.Profile.Category, err)
			}
			// Every shipped builtin should have at least one guide prompt
			// and one example — they exist precisely to introduce new
			// users to what the agent does.
			if len(a.Profile.GuidePrompts) == 0 {
				t.Errorf("%s: missing guide_prompts", name)
			}
			if len(a.Profile.Examples) == 0 {
				t.Errorf("%s: missing examples", name)
			}
		})
	}
}

func TestLoadAgentProfile_WithAvatar(t *testing.T) {
	dir := t.TempDir()
	writeYAML(t, dir, `
category: coding
avatar: https://cdn.example.com/a.png
description:
  en: An agent.
`)
	p, err := LoadAgentProfile(dir)
	if err != nil {
		t.Fatalf("LoadAgentProfile: %v", err)
	}
	if p == nil {
		t.Fatal("expected non-nil profile")
	}
	if p.Avatar != "https://cdn.example.com/a.png" {
		t.Errorf("Avatar=%q, want the cdn url", p.Avatar)
	}
}

func TestValidateAvatarURL(t *testing.T) {
	ok := []string{
		"",                                // empty = no avatar, allowed
		"https://x/y.png",                 // minimal https with host
		"https://cdn.example.com/a.png",   // typical CDN url
	}
	for _, u := range ok {
		if err := ValidateAvatarURL(u); err != nil {
			t.Errorf("ValidateAvatarURL(%q) = %v, want nil", u, err)
		}
	}
	bad := []string{
		"http://x/y.png",      // wrong scheme
		"javascript:alert(1)", // script scheme
		"data:image/png;base64,AAAA",
		"https://",  // empty host
		"https:///path",
		"ftp://x/y",
	}
	for _, u := range bad {
		if err := ValidateAvatarURL(u); err == nil {
			t.Errorf("ValidateAvatarURL(%q) = nil, want error", u)
		}
	}
}
