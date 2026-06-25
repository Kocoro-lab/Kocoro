package koe

import "strings"

// SemanticMatcher is the ③ rung of the resolution ladder: best-effort
// description-based matching of a spoken reference to a slug. Returns "" for no
// match. Plan B ships NoopSemanticMatcher; C-full wires the realtime mini /
// daemon small tier behind this seam (kept minimal per YAGNI).
type SemanticMatcher interface {
	Match(ref string, agents []AgentSummary) string
}

// NoopSemanticMatcher never matches — the safe default until C provides a model.
type NoopSemanticMatcher struct{}

func (NoopSemanticMatcher) Match(string, []AgentSummary) string { return "" }

// ResolveStatus classifies a resolution outcome.
type ResolveStatus int

const (
	ResolveResolved  ResolveStatus = iota // Slug is set
	ResolveAmbiguous                      // Candidates has >1 slug; caller must ask
	ResolveNotFound                       // no rung matched; caller asks or uses default
)

// ResolveResult is the outcome of Resolve.
type ResolveResult struct {
	Status     ResolveStatus
	Slug       string   // set when Resolved
	Candidates []string // set when Ambiguous (slugs)
}

// AgentResolver canonicalizes a spoken agent reference to an on-disk slug.
type AgentResolver struct {
	agents []AgentSummary
	sem    SemanticMatcher
}

// NewAgentResolver builds a resolver over a snapshot of the agent registry.
func NewAgentResolver(agents []AgentSummary, sem SemanticMatcher) *AgentResolver {
	if sem == nil {
		sem = NoopSemanticMatcher{}
	}
	return &AgentResolver{agents: agents, sem: sem}
}

// Resolve runs the deterministic-first ladder. It never silently picks one of
// several substring hits — that returns Ambiguous so the caller can ask.
func (r *AgentResolver) Resolve(ref string) ResolveResult {
	norm := strings.ToLower(strings.TrimSpace(ref))
	if norm == "" {
		return ResolveResult{Status: ResolveNotFound}
	}

	// ① exact slug wins outright (a display name can never shadow a real slug).
	for _, a := range r.agents {
		if strings.ToLower(a.Slug) == norm {
			return ResolveResult{Status: ResolveResolved, Slug: a.Slug}
		}
	}

	// ② display-name substring (bidirectional: "金融" ⊂ "金融分析 agent", and a
	//    longer spoken phrase still matches a short display name).
	var hits []string
	for _, a := range r.agents {
		dn := strings.ToLower(strings.TrimSpace(a.DisplayName))
		if dn == "" {
			continue
		}
		if strings.Contains(dn, norm) || strings.Contains(norm, dn) {
			hits = append(hits, a.Slug)
		}
	}
	if len(hits) == 1 {
		return ResolveResult{Status: ResolveResolved, Slug: hits[0]}
	}
	if len(hits) > 1 {
		return ResolveResult{Status: ResolveAmbiguous, Candidates: hits}
	}

	// ③ semantic (pluggable). NoopSemanticMatcher returns "" → falls through.
	if slug := r.sem.Match(ref, r.agents); slug != "" {
		return ResolveResult{Status: ResolveResolved, Slug: slug}
	}

	// ④ nothing matched.
	return ResolveResult{Status: ResolveNotFound}
}
