package koe

import "testing"

func fixtureAgents() []AgentSummary {
	return []AgentSummary{
		{Slug: "finance", DisplayName: "金融分析 agent", Description: map[string]string{"en": "stock and market analysis"}},
		{Slug: "default", DisplayName: "Kocoro"},
		{Slug: "legal", DisplayName: "法务 agent", Description: map[string]string{"en": "contracts"}},
	}
}

func TestResolveExactSlug(t *testing.T) {
	r := NewAgentResolver(fixtureAgents(), NoopSemanticMatcher{})
	got := r.Resolve("finance")
	if got.Status != ResolveResolved || got.Slug != "finance" {
		t.Errorf("Resolve(finance) = %+v, want Resolved/finance", got)
	}
}

func TestResolveDisplayNameSubstring(t *testing.T) {
	r := NewAgentResolver(fixtureAgents(), NoopSemanticMatcher{})
	got := r.Resolve("金融") // ⊂ "金融分析 agent"
	if got.Status != ResolveResolved || got.Slug != "finance" {
		t.Errorf("Resolve(金融) = %+v, want Resolved/finance", got)
	}
}

func TestResolveAmbiguous(t *testing.T) {
	agents := append(fixtureAgents(), AgentSummary{Slug: "finance-jp", DisplayName: "金融日本 agent"})
	r := NewAgentResolver(agents, NoopSemanticMatcher{})
	got := r.Resolve("金融") // now ⊂ two display names
	if got.Status != ResolveAmbiguous {
		t.Fatalf("Resolve(金融) status = %v, want Ambiguous", got.Status)
	}
	if len(got.Candidates) != 2 {
		t.Errorf("candidates = %v, want 2", got.Candidates)
	}
}

func TestResolveNotFound(t *testing.T) {
	r := NewAgentResolver(fixtureAgents(), NoopSemanticMatcher{})
	got := r.Resolve("nonexistent xyz")
	if got.Status != ResolveNotFound {
		t.Errorf("Resolve(nonexistent) = %v, want NotFound", got.Status)
	}
}

func TestResolveSemanticHook(t *testing.T) {
	// A stub matcher that maps any reference to "legal" proves the ③ seam fires
	// only after ① and ② miss.
	r := NewAgentResolver(fixtureAgents(), funcSemanticMatcher(func(ref string, a []AgentSummary) string { return "legal" }))
	got := r.Resolve("the lawyer one")
	if got.Status != ResolveResolved || got.Slug != "legal" {
		t.Errorf("semantic Resolve = %+v, want Resolved/legal", got)
	}
}

type funcSemanticMatcher func(ref string, agents []AgentSummary) string

func (f funcSemanticMatcher) Match(ref string, agents []AgentSummary) string { return f(ref, agents) }
