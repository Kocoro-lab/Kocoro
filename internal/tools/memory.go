package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/memory"
)

// MemoryQuerier is the subset of *memory.Service the tool consumes.
// Pulled out as an interface so tests can substitute a stub. The real
// memory.Service satisfies it via Status() and Query(ctx, intent).
type MemoryQuerier interface {
	Status() memory.ServiceStatus
	Query(ctx context.Context, intent memory.QueryIntent) (*memory.ResponseEnvelope, memory.ErrorClass, error)
}

// FallbackQuery is the legacy memory recall path the tool falls back to when
// the structured memory service (Kocoro Cloud memory sidecar) is unavailable.
// Daemon supplies an adapter wired to session.Manager.Search + a MEMORY.md
// grep; CLI/TUI supplies the same against their local resources.
type FallbackQuery interface {
	SessionKeyword(ctx context.Context, query string, limit int) ([]any, error)
	MemoryFileSnippet(ctx context.Context, query string) (string, error)
}

// MemoryTool exposes memory_recall to the agent loop. Service may be nil
// when the daemon's memory.Service failed to start, when provider is
// disabled, or in CLI/TUI attach paths where AttachPolicy returned
// ready=false. Fallback must always be supplied.
type MemoryTool struct {
	Service  MemoryQuerier
	Fallback FallbackQuery
}

type memoryArgs struct {
	Mode                string   `json:"mode"`
	AnchorMentions      []string `json:"anchor_mentions"`
	RelationConstraints []string `json:"relation_constraints,omitempty"`
	CandidateType       *string  `json:"candidate_type,omitempty"`
	ScopeFilter         []string `json:"scope_filter,omitempty"`
	TargetSlot          string   `json:"target_slot,omitempty"`
	TimeWindow          *string  `json:"time_window,omitempty"`
	EvidenceBudget      int      `json:"evidence_budget,omitempty"`
	ResultLimit         int      `json:"result_limit,omitempty"`
}

func (t *MemoryTool) Info() agent.ToolInfo {
	return agent.ToolInfo{
		Name: "memory_recall",
		Description: "Read structured long-term memory results from the user's personal knowledge graph.\n" +
			"Modes:\n" +
			"- direct_relation: one-hop predicate (e.g. \"what did X create?\"). Read `groups[].via_relations`.\n" +
			"- path_query: multi-hop / possessive (e.g. \"what did X's collaborator create?\"). relation_constraints is the ordered path; inverse hops use `^-1`. Read `groups[].observed_path`.\n" +
			"- typed_neighborhood: typed target with exactly one relation. Requires candidate_type. Rank by score.\n\n" +
			"Evidence rules: ground each factual item in `supporting_event_ids` or `observed_path`, but in user-facing wording say \"past records\" / \"I found\"; do not surface raw event IDs, `memory_block`, `no_data_reason`, or scope labels unless the user asks for debug/provenance. If `no_data_reason` is set, say past records have no direct answer; do not invent from training data.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"mode": map[string]any{
					"type":        "string",
					"enum":        []string{"direct_relation", "path_query", "typed_neighborhood"},
					"description": "Use direct_relation for one-hop facts. Use path_query for multi-hop relationship questions. Use typed_neighborhood only when candidate_type and exactly one relation constraint are clear.",
				},
				"anchor_mentions": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": "string"},
					"description": "Entity names to start from. Use the clearest/fullest name when obvious (for example, use a person's full name rather than a nickname). For path_query, include only the starting entity mention, not relationship words like student/collaborator/project.",
				},
				"relation_constraints": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": "string"},
					"description": "Canonical relation names. For path_query, this is the ordered path; append ^-1 for inverse hops, e.g. collaborated_with^-1 then created for a two-hop possessive question. For typed_neighborhood, provide exactly one relation.",
				},
				"candidate_type": map[string]any{
					"type":        "string",
					"description": "Optional target entity type filter such as Person, Project, Company, Tool. Required for typed_neighborhood.",
				},
				"scope_filter": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": "string"},
					"description": "Optional scope filters when the user asks about a specific project/topic scope.",
				},
				"target_slot": map[string]any{
					"type":        "string",
					"enum":        []string{"head", "tail"},
					"description": "For direct_relation, return relation tails by default; use head for inverse lookup.",
				},
				"time_window": map[string]any{
					"type":        "string",
					"description": "Optional time window when the user asks about a date/time-bounded memory.",
				},
				"evidence_budget": map[string]any{
					"type":        "integer",
					"description": "Maximum supporting event ids to include per candidate or path hop.",
				},
				"result_limit": map[string]any{
					"type":        "integer",
					"description": "Maximum candidate groups to return.",
				},
			},
		},
		Required: []string{"anchor_mentions"},
	}
}

func (t *MemoryTool) RequiresApproval() bool     { return false }
func (t *MemoryTool) IsReadOnlyCall(string) bool { return true }

func (t *MemoryTool) Run(ctx context.Context, argsJSON string) (agent.ToolResult, error) {
	var a memoryArgs
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
		return agent.ToolResult{Content: fmt.Sprintf("invalid input: %v", err), IsError: true}, nil
	}
	if len(a.AnchorMentions) == 0 {
		return agent.ToolResult{Content: "anchor_mentions is required and must be non-empty", IsError: true}, nil
	}
	if a.Mode == "" {
		a.Mode = "direct_relation"
	}
	if a.ResultLimit <= 0 {
		a.ResultLimit = 10
	}
	if a.EvidenceBudget <= 0 {
		a.EvidenceBudget = 5
	}

	intent := memory.QueryIntent{
		Mode:                memory.QueryMode(a.Mode),
		AnchorMentions:      a.AnchorMentions,
		RelationConstraints: a.RelationConstraints,
		CandidateType:       a.CandidateType,
		ScopeFilter:         a.ScopeFilter,
		TargetSlot:          a.TargetSlot,
		TimeWindow:          a.TimeWindow,
		EvidenceBudget:      a.EvidenceBudget,
		ResultLimit:         a.ResultLimit,
	}
	return t.run(ctx, intent)
}

func validateMemoryArgs(a memoryArgs) string {
	for _, rel := range a.RelationConstraints {
		if isBroadMemoryRelation(rel) {
			return "memory_recall requires concrete relation_constraints. Broad relations like related_to are not valid for structured lookup; use a concrete relation/path, or use session_search for raw private-context lookup."
		}
	}
	switch memory.QueryMode(a.Mode) {
	case memory.ModeDirectRelation:
		if len(a.RelationConstraints) == 0 {
			return "direct_relation requires at least one relation_constraints value. Choose a concrete relation for the user's question, or use session_search for raw private-context lookup."
		}
	case memory.ModeTypedNeighborhood:
		if a.CandidateType == nil || strings.TrimSpace(*a.CandidateType) == "" {
			return "typed_neighborhood requires candidate_type. Ask the user to narrow the target type, or use direct_relation/path_query when a relation is clear."
		}
		if len(a.RelationConstraints) != 1 {
			return "typed_neighborhood requires exactly one relation_constraints value. Use direct_relation/path_query for relationship questions, or ask the user to narrow the memory lookup."
		}
	}
	return ""
}

func isBroadMemoryRelation(rel string) bool {
	rel = strings.TrimSpace(strings.ToLower(rel))
	rel = strings.TrimPrefix(rel, "^")
	rel = strings.TrimSuffix(rel, "^-1")
	switch rel {
	case "related_to", "relates_to", "associated_with", "about", "mentions":
		return true
	default:
		return false
	}
}

// run is the post-validation path. Implements the four class branches
// from spec §5.3: ClassOK shapes structured candidates (with a degraded
// warning prefix when env.Reason == "degraded"), ClassRetryable does one
// inline retry after 500ms before falling back, ClassPermanent surfaces
// the envelope error as warnings with IsError=true, ClassUnavailable and
// any transport error fall back via the legacy keyword path.
func (t *MemoryTool) run(ctx context.Context, intent memory.QueryIntent) (agent.ToolResult, error) {
	if t.Service == nil || t.Service.Status() != memory.StatusReady {
		return t.fallback(ctx, intent, "service_unavailable", "fallback")
	}
	if err := validateMemoryArgs(memoryArgs{
		Mode:                string(intent.Mode),
		RelationConstraints: intent.RelationConstraints,
		CandidateType:       intent.CandidateType,
	}); err != "" {
		return agent.ToolResult{Content: err, IsError: true}, nil
	}
	env, class, err := t.Service.Query(ctx, intent)
	if err != nil || class == memory.ClassUnavailable {
		return t.fallback(ctx, intent, "service_unavailable", "fallback")
	}
	if class == memory.ClassRetryable {
		// Spec §5.3: one inline retry after 500ms.
		select {
		case <-ctx.Done():
			return t.fallback(ctx, intent, "service_unavailable", "fallback")
		case <-time.After(500 * time.Millisecond):
		}
		env, class, err = t.Service.Query(ctx, intent)
		if err != nil || class == memory.ClassUnavailable || class == memory.ClassRetryable {
			return t.fallback(ctx, intent, "retryable_failed", "fallback_after_retry")
		}
	}
	if class == memory.ClassPermanent {
		return t.permanentResult(env), nil
	}
	return t.shapeResult(env), nil
}

func (t *MemoryTool) permanentResult(env *memory.ResponseEnvelope) agent.ToolResult {
	out := map[string]any{
		"source":           "memory_sidecar",
		"evidence_quality": "structured",
		"bundle_version":   env.BundleVersion,
		"candidates":       []any{},
		"warnings":         envelopeWarnings(env),
		"fallback_reason":  nil,
	}
	body, _ := json.Marshal(out)
	return agent.ToolResult{Content: string(body), IsError: true}
}

func (t *MemoryTool) shapeResult(env *memory.ResponseEnvelope) agent.ToolResult {
	quality := "structured"
	warnings := envelopeWarnings(env)
	if env.Reason == "degraded" {
		quality = "structured_degraded"
		warnings = append([]map[string]any{{
			"code":    "bundle_degraded",
			"message": "memory bundle degraded — results may be incomplete",
		}}, warnings...)
	}
	cands := make([]map[string]any, 0, len(env.Candidates))
	for _, c := range env.Candidates {
		m := map[string]any{
			"value":                c.Value,
			"score":                c.Score,
			"evidence":             c.Evidence,
			"supporting_event_ids": c.SupportingEventIDs,
		}
		if c.Scope != nil {
			m["scope"] = *c.Scope
		}
		if c.SupportCount != nil {
			m["support_count"] = *c.SupportCount
		}
		if c.DistinctSessionCount != nil {
			m["distinct_session_count"] = *c.DistinctSessionCount
		}
		cands = append(cands, m)
	}
	out := map[string]any{
		"source":           "memory_sidecar",
		"evidence_quality": quality,
		"bundle_version":   env.BundleVersion,
		"memory_block":     env.MemoryBlock,
		"candidates":       cands,
		"warnings":         warnings,
		"fallback_reason":  nil,
	}
	body, _ := json.Marshal(out)
	return agent.ToolResult{Content: string(body)}
}

func envelopeWarnings(env *memory.ResponseEnvelope) []map[string]any {
	out := []map[string]any{}
	for _, w := range env.Warnings {
		out = append(out, map[string]any{"code": w.Code, "message": w.Message})
	}
	if env.Error != nil {
		out = append(out, map[string]any{
			"code":     env.Error.Code,
			"message":  env.Error.Message,
			"sub_code": env.Error.SubCode(),
		})
	}
	return out
}

// fallback returns the JSON-shaped fallback envelope per spec §5.4. Delegates
// to FallbackQuery so when the structured sidecar is unavailable the agent
// still gets keyword session_search hits + MEMORY.md snippets shaped into
// the same envelope (with evidence_quality="text_search" so downstream
// reasoning can weight them lower).
func (t *MemoryTool) fallback(ctx context.Context, intent memory.QueryIntent, reason, source string) (agent.ToolResult, error) {
	candidates := []map[string]any{}
	warnings := []map[string]any{}
	if t.Fallback != nil {
		query := strings.TrimSpace(strings.Join(intent.AnchorMentions, " "))
		limit := intent.ResultLimit
		if limit <= 0 {
			limit = 10
		}
		if hits, err := t.Fallback.SessionKeyword(ctx, query, limit); err == nil {
			for _, h := range hits {
				b, mErr := json.Marshal(h)
				if mErr != nil {
					continue
				}
				candidates = append(candidates, map[string]any{
					"value":    string(b),
					"evidence": "text_search",
				})
			}
		} else {
			warnings = append(warnings, map[string]any{
				"code":    "fallback_session_search_failed",
				"message": err.Error(),
			})
		}
		if snippet, err := t.Fallback.MemoryFileSnippet(ctx, query); err == nil && snippet != "" {
			scope := "memory_md"
			candidates = append(candidates, map[string]any{
				"value":    snippet,
				"evidence": "text_search",
				"scope":    scope,
			})
		}
	}
	out := map[string]any{
		"source":           source,
		"evidence_quality": "text_search",
		"bundle_version":   nil,
		"candidates":       candidates,
		"warnings":         warnings,
		"fallback_reason":  reason,
	}
	body, _ := json.Marshal(out)
	return agent.ToolResult{Content: string(body)}, nil
}
