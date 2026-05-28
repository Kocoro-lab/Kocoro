package agents

import "testing"

func langStrPtr(s string) *string { return &s }

// TestAgentLanguageRoundTrip locks the three-state per-agent language field
// across WriteAgentConfig → LoadAgent. The "" case is the critical one: an
// explicit empty string means "force mirror even if the global agent.language
// is locked", and it must survive as a non-nil *string == "", NOT collapse to
// nil ("inherit global"). Collapsing would silently invert the semantics.
func TestAgentLanguageRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		in   *string
		want *string // nil → expect Language nil; non-nil → expect *Language == *want
	}{
		{"locked", langStrPtr("日本語"), langStrPtr("日本語")},
		{"force_mirror_empty", langStrPtr(""), langStrPtr("")},
		{"inherit_nil", nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir, name := setupAgent(t, "lang-"+tc.name)
			cfg := &AgentConfigAPI{Agent: &AgentModelConfig{Language: tc.in}}
			if err := WriteAgentConfig(dir, name, cfg); err != nil {
				t.Fatalf("write: %v", err)
			}
			loaded, err := LoadAgent(dir, name)
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			var got *string
			if loaded.Config != nil && loaded.Config.Agent != nil {
				got = loaded.Config.Agent.Language
			}
			switch {
			case tc.want == nil && got != nil:
				t.Errorf("expected Language nil (inherit global), got %q", *got)
			case tc.want != nil && got == nil:
				t.Errorf("expected Language %q, got nil — three-state semantics inverted", *tc.want)
			case tc.want != nil && got != nil && *got != *tc.want:
				t.Errorf("Language round-trip: got %q, want %q", *got, *tc.want)
			}
		})
	}
}
