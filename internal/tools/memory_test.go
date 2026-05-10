package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/memory"
)

type fakeFallback struct {
	snippet      string
	hits         []any
	gotQuery     string
	gotLimit     int
	snippetQuery string
}

func (f *fakeFallback) SessionKeyword(_ context.Context, q string, limit int) ([]any, error) {
	f.gotQuery = q
	f.gotLimit = limit
	return f.hits, nil
}
func (f *fakeFallback) MemoryFileSnippet(_ context.Context, q string) (string, error) {
	f.snippetQuery = q
	return f.snippet, nil
}

func TestMemoryTool_FallbackWhenNoService(t *testing.T) {
	tool := &MemoryTool{Fallback: &fakeFallback{snippet: "memory.md note"}}
	res, err := tool.Run(context.Background(), `{"anchor_mentions":["x"],"relation_constraints":["created"]}`)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if res.IsError {
		t.Fatalf("res.IsError=true: %s", res.Content)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(res.Content), &body); err != nil {
		t.Fatalf("decode tool result: %v\n%s", err, res.Content)
	}
	if body["source"] != "fallback" {
		t.Fatalf("source=%v want fallback", body["source"])
	}
	if body["evidence_quality"] != "text_search" {
		t.Fatalf("evidence_quality=%v", body["evidence_quality"])
	}
}

func TestMemoryTool_RejectsEmptyAnchorMentions(t *testing.T) {
	tool := &MemoryTool{Fallback: &fakeFallback{}}
	res, _ := tool.Run(context.Background(), `{"anchor_mentions":[]}`)
	if !res.IsError || !strings.Contains(res.Content, "anchor_mentions") {
		t.Fatalf("res=%+v", res)
	}
}

func TestMemoryTool_RejectsMissingAnchorMentions(t *testing.T) {
	tool := &MemoryTool{Fallback: &fakeFallback{}}
	res, _ := tool.Run(context.Background(), `{}`)
	if !res.IsError {
		t.Fatalf("expected error: %s", res.Content)
	}
}

func TestMemoryTool_RejectsMalformedJSON(t *testing.T) {
	tool := &MemoryTool{Fallback: &fakeFallback{}}
	res, _ := tool.Run(context.Background(), `{not json`)
	if !res.IsError {
		t.Fatalf("expected error: %s", res.Content)
	}
}

func TestMemoryTool_RejectsBroadStructuredDirectRelation(t *testing.T) {
	tool := &MemoryTool{
		Service:  &stubQuerier{status: memory.StatusReady, env: &memory.ResponseEnvelope{}, class: memory.ClassOK},
		Fallback: &fakeFallback{},
	}
	res, _ := tool.Run(context.Background(), `{"mode":"direct_relation","anchor_mentions":["Nexus"]}`)
	if !res.IsError || !strings.Contains(res.Content, "direct_relation requires") {
		t.Fatalf("expected direct_relation relation guard, got %+v", res)
	}
}

func TestMemoryTool_RejectsNonConcreteRelationConstraints(t *testing.T) {
	tool := &MemoryTool{
		Service:  &stubQuerier{status: memory.StatusReady, env: &memory.ResponseEnvelope{}, class: memory.ClassOK},
		Fallback: &fakeFallback{},
	}
	res, _ := tool.Run(context.Background(), `{"mode":"direct_relation","anchor_mentions":["Alice Nakamura","Jordan Sato"],"relation_constraints":["related_to"]}`)
	if !res.IsError || !strings.Contains(res.Content, "requires concrete relation_constraints") {
		t.Fatalf("expected broad relation guard, got %+v", res)
	}
}

func TestMemoryTool_RejectsInvalidTypedNeighborhood(t *testing.T) {
	tool := &MemoryTool{
		Service:  &stubQuerier{status: memory.StatusReady, env: &memory.ResponseEnvelope{}, class: memory.ClassOK},
		Fallback: &fakeFallback{},
	}
	res, _ := tool.Run(context.Background(), `{"mode":"typed_neighborhood","anchor_mentions":["Alice Nakamura"],"relation_constraints":["created"]}`)
	if !res.IsError || !strings.Contains(res.Content, "typed_neighborhood requires candidate_type") {
		t.Fatalf("expected typed_neighborhood candidate_type guard, got %+v", res)
	}
}

type stubQuerier struct {
	status memory.ServiceStatus
	env    *memory.ResponseEnvelope
	class  memory.ErrorClass
	err    error
	callN  int
	seq    []memory.ErrorClass // optional: deliver i-th class on i-th call
}

func (s *stubQuerier) Status() memory.ServiceStatus { return s.status }
func (s *stubQuerier) Query(_ context.Context, _ memory.QueryIntent) (*memory.ResponseEnvelope, memory.ErrorClass, error) {
	s.callN++
	if len(s.seq) > 0 {
		c := s.seq[0]
		s.seq = s.seq[1:]
		return s.env, c, nil
	}
	return s.env, s.class, s.err
}

func TestMemoryTool_ClassOK(t *testing.T) {
	env := &memory.ResponseEnvelope{
		Reason:        "ok",
		BundleVersion: "0.4.0",
		Candidates: []memory.QueryCandidate{
			{Value: "v", Score: 0.9, Evidence: "observed", SupportingEventIDs: []string{"e1"}},
		},
	}
	tool := &MemoryTool{
		Service:  &stubQuerier{status: memory.StatusReady, env: env, class: memory.ClassOK},
		Fallback: &fakeFallback{},
	}
	res, err := tool.Run(context.Background(), `{"anchor_mentions":["x"],"relation_constraints":["created"]}`)
	if err != nil || res.IsError {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	var body map[string]any
	json.Unmarshal([]byte(res.Content), &body)
	if body["source"] != "memory_sidecar" {
		t.Fatalf("source=%v", body["source"])
	}
	if body["evidence_quality"] != "structured" {
		t.Fatalf("evidence_quality=%v", body["evidence_quality"])
	}
	if body["bundle_version"] != "0.4.0" {
		t.Fatalf("bundle_version=%v", body["bundle_version"])
	}
	cands, _ := body["candidates"].([]any)
	if len(cands) != 1 {
		t.Fatalf("candidates=%+v", cands)
	}
}

func TestMemoryTool_ClassDegraded(t *testing.T) {
	env := &memory.ResponseEnvelope{
		Reason:        "degraded",
		BundleVersion: "0.4.0",
		Candidates:    []memory.QueryCandidate{{Value: "v", Evidence: "observed"}},
	}
	tool := &MemoryTool{
		Service:  &stubQuerier{status: memory.StatusReady, env: env, class: memory.ClassOK},
		Fallback: &fakeFallback{},
	}
	res, _ := tool.Run(context.Background(), `{"anchor_mentions":["x"],"relation_constraints":["created"]}`)
	var body map[string]any
	json.Unmarshal([]byte(res.Content), &body)
	if body["evidence_quality"] != "structured_degraded" {
		t.Fatalf("evidence_quality=%v", body["evidence_quality"])
	}
	warnings, _ := body["warnings"].([]any)
	if len(warnings) == 0 {
		t.Fatal("expected degraded warning")
	}
	w0, _ := warnings[0].(map[string]any)
	if msg, _ := w0["message"].(string); !strings.Contains(msg, "degraded") {
		t.Fatalf("first warning should be the degraded notice; got %+v", w0)
	}
}

func TestMemoryTool_ClassRetryable_OneRetryThenFallback(t *testing.T) {
	sq := &stubQuerier{status: memory.StatusReady, seq: []memory.ErrorClass{memory.ClassRetryable, memory.ClassRetryable}}
	tool := &MemoryTool{Service: sq, Fallback: &fakeFallback{}}
	res, _ := tool.Run(context.Background(), `{"anchor_mentions":["x"],"relation_constraints":["created"]}`)
	var body map[string]any
	json.Unmarshal([]byte(res.Content), &body)
	if body["source"] != "fallback_after_retry" {
		t.Fatalf("source=%v", body["source"])
	}
	if body["fallback_reason"] != "retryable_failed" {
		t.Fatalf("fallback_reason=%v", body["fallback_reason"])
	}
	if sq.callN != 2 {
		t.Fatalf("expected 2 calls, got %d", sq.callN)
	}
}

func TestMemoryTool_ClassPermanent_SurfacesDiagnostics(t *testing.T) {
	env := &memory.ResponseEnvelope{
		Reason: "error",
		Error: &memory.ErrorObject{
			Code:    "validation_error",
			Message: "bad mode",
			Details: map[string]any{"sub_code": "schema_validation"},
		},
	}
	tool := &MemoryTool{
		Service:  &stubQuerier{status: memory.StatusReady, env: env, class: memory.ClassPermanent},
		Fallback: &fakeFallback{},
	}
	res, _ := tool.Run(context.Background(), `{"anchor_mentions":["x"],"relation_constraints":["created"]}`)
	if !res.IsError {
		t.Fatal("permanent should be IsError")
	}
	var body map[string]any
	json.Unmarshal([]byte(res.Content), &body)
	warnings, _ := body["warnings"].([]any)
	if len(warnings) == 0 {
		t.Fatal("expected warnings with sub_code")
	}
	w0, _ := warnings[0].(map[string]any)
	if w0["sub_code"] != "schema_validation" {
		t.Fatalf("w0=%+v", w0)
	}
}

func TestMemoryTool_ClassUnavailable_FallsBack(t *testing.T) {
	tool := &MemoryTool{
		Service:  &stubQuerier{status: memory.StatusReady, class: memory.ClassUnavailable},
		Fallback: &fakeFallback{},
	}
	res, _ := tool.Run(context.Background(), `{"anchor_mentions":["x"],"relation_constraints":["created"]}`)
	var body map[string]any
	json.Unmarshal([]byte(res.Content), &body)
	if body["source"] != "fallback" {
		t.Fatalf("source=%v", body["source"])
	}
	if body["fallback_reason"] != "service_unavailable" {
		t.Fatalf("fallback_reason=%v", body["fallback_reason"])
	}
}

func TestMemoryTool_Info(t *testing.T) {
	tool := &MemoryTool{Fallback: &fakeFallback{}}
	info := tool.Info()
	if info.Name != "memory_recall" {
		t.Fatalf("name=%q want memory_recall", info.Name)
	}
	if !tool.IsReadOnlyCall("") {
		t.Fatal("memory_recall must be read-only")
	}
	if tool.RequiresApproval() {
		t.Fatal("memory_recall must not require approval")
	}
	// Required field declared.
	found := false
	for _, r := range info.Required {
		if r == "anchor_mentions" {
			found = true
		}
	}
	if !found {
		t.Fatal("anchor_mentions must be in Required")
	}
	if !strings.Contains(info.Description, "direct_relation") || !strings.Contains(info.Description, "path_query") || !strings.Contains(info.Description, "typed_neighborhood") {
		t.Fatal("description should enumerate all three modes")
	}
	if !strings.Contains(info.Description, "^-1") {
		t.Fatal("description should document inverse-hop syntax for path_query")
	}
	if !strings.Contains(info.Description, "candidate_type") {
		t.Fatal("description should document candidate_type for typed_neighborhood")
	}
	if !strings.Contains(info.Description, "past records") {
		t.Fatal("description should mandate user-facing 'past records' wording")
	}
	if !strings.Contains(info.Description, "do not surface internal labels") && !strings.Contains(info.Description, "do not surface raw event IDs") {
		t.Fatal("description should forbid surfacing internal labels in user-facing output")
	}
	// Trim invariants: ensure removed prose stays gone.
	for _, removed := range []string{
		"Explicit memory requests",
		"prior assistant summaries, session titles, or search snippets",
		"keep generated outlines, timelines, and role splits",
		"Stop after a few focused raw-context lookups",
		"Do not use typed_neighborhood as a generic profile",
	} {
		if strings.Contains(info.Description, removed) {
			t.Errorf("description still contains trimmed prose: %q", removed)
		}
	}
}

// TestMemoryTool_MemoryBlock_Direct confirms the direct_relation path:
// the structured memory_block reaches the tool result with via_relations
// populated. Legacy candidates stay alongside as fallback.
func TestMemoryTool_MemoryBlock_Direct(t *testing.T) {
	env := &memory.ResponseEnvelope{
		Reason:        "ok",
		BundleVersion: "0.6.0",
		Candidates:    []memory.QueryCandidate{{Value: "Nexus", Score: 1.37, Evidence: "observed", SupportingEventIDs: []string{"ev_a"}}},
		MemoryBlock: &memory.MemoryBlock{
			Groups: []memory.MemoryCandidateGroup{{
				Value:              "Nexus",
				Score:              1.37,
				Evidence:           "observed",
				SupportCount:       2,
				SupportingEventIDs: []string{"ev_a", "ev_b"},
				EntityIDs:          []string{"ent_1"},
				Scopes:             []string{"project:ShanClaw"},
				ViaRelations:       []string{"created"},
				ViaAnchorEntityIDs: []string{"ent_anchor"},
			}},
			Notes: []string{},
		},
	}
	tool := &MemoryTool{
		Service:  &stubQuerier{status: memory.StatusReady, env: env, class: memory.ClassOK},
		Fallback: &fakeFallback{},
	}
	res, err := tool.Run(context.Background(), `{"mode":"direct_relation","anchor_mentions":["Alice Nakamura"],"relation_constraints":["created"]}`)
	if err != nil || res.IsError {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(res.Content), &body); err != nil {
		t.Fatalf("decode body: %v\n%s", err, res.Content)
	}
	mb, ok := body["memory_block"].(map[string]any)
	if !ok {
		t.Fatalf("memory_block missing or wrong type: %#v", body["memory_block"])
	}
	groups, _ := mb["groups"].([]any)
	if len(groups) != 1 {
		t.Fatalf("groups=%+v", groups)
	}
	g0, _ := groups[0].(map[string]any)
	via, _ := g0["via_relations"].([]any)
	if len(via) != 1 || via[0] != "created" {
		t.Fatalf("group[0].via_relations=%+v", via)
	}
	cands, _ := body["candidates"].([]any)
	if len(cands) != 1 {
		t.Fatalf("legacy candidates dropped: %+v", cands)
	}
}

// TestMemoryTool_MemoryBlock_Path confirms the path_query path: each hop
// of observed_path round-trips through shapeResult and is visible to the
// LLM with from_label / relation / direction / to_label /
// supporting_event_ids intact.
func TestMemoryTool_MemoryBlock_Path(t *testing.T) {
	env := &memory.ResponseEnvelope{
		Reason:        "ok",
		BundleVersion: "0.6.0",
		Candidates:    []memory.QueryCandidate{},
		MemoryBlock: &memory.MemoryBlock{
			Groups: []memory.MemoryCandidateGroup{{
				Value:     "Nexus",
				Score:     0.5,
				Evidence:  "observed",
				EntityIDs: []string{"ent_000000000001"},
				ObservedPath: []memory.HopRecord{
					{FromEntityID: "ent_000000000020", FromLabel: "Jordan Sato", Relation: "studied_under", Direction: "inverse", ToEntityID: "ent_000000000010", ToLabel: "Alice Nakamura", SupportingEventIDs: []string{"ev_h1", "ev_h2"}},
					{FromEntityID: "ent_000000000010", FromLabel: "Alice Nakamura", Relation: "created", Direction: "forward", ToEntityID: "ent_000000000001", ToLabel: "Nexus", SupportingEventIDs: []string{"ev_d1"}},
				},
			}},
			Notes: []string{"Observed path matched 1 candidates; TL fallback skipped."},
		},
	}
	tool := &MemoryTool{
		Service:  &stubQuerier{status: memory.StatusReady, env: env, class: memory.ClassOK},
		Fallback: &fakeFallback{},
	}
	res, _ := tool.Run(context.Background(), `{"mode":"path_query","anchor_mentions":["Jordan Sato"]}`)
	if res.IsError {
		t.Fatalf("res.IsError: %s", res.Content)
	}
	var body map[string]any
	json.Unmarshal([]byte(res.Content), &body)
	mb, _ := body["memory_block"].(map[string]any)
	groups, _ := mb["groups"].([]any)
	g0, _ := groups[0].(map[string]any)
	hops, _ := g0["observed_path"].([]any)
	if len(hops) != 2 {
		t.Fatalf("expected 2 hops, got %d: %+v", len(hops), hops)
	}
	h0, _ := hops[0].(map[string]any)
	if h0["from_label"] != "Jordan Sato" || h0["direction"] != "inverse" || h0["relation"] != "studied_under" {
		t.Fatalf("hop[0]=%+v", h0)
	}
	h1, _ := hops[1].(map[string]any)
	if h1["to_label"] != "Nexus" || h1["direction"] != "forward" {
		t.Fatalf("hop[1]=%+v", h1)
	}
	ev, _ := h0["supporting_event_ids"].([]any)
	if len(ev) != 2 {
		t.Fatalf("hop[0].supporting_event_ids=%+v", ev)
	}
}

// TestMemoryTool_MemoryBlock_NilOlderSidecar confirms a nil MemoryBlock
// (older sidecar that doesn't emit the field) reaches the tool result as
// JSON null — the LLM's fallback rule keys on this exact shape.
func TestMemoryTool_MemoryBlock_NilOlderSidecar(t *testing.T) {
	env := &memory.ResponseEnvelope{
		Reason:        "ok",
		BundleVersion: "0.4.0",
		Candidates:    []memory.QueryCandidate{{Value: "v", Score: 0.5, Evidence: "observed"}},
		MemoryBlock:   nil,
	}
	tool := &MemoryTool{
		Service:  &stubQuerier{status: memory.StatusReady, env: env, class: memory.ClassOK},
		Fallback: &fakeFallback{},
	}
	res, _ := tool.Run(context.Background(), `{"anchor_mentions":["x"],"relation_constraints":["created"]}`)
	if res.IsError {
		t.Fatalf("res.IsError: %s", res.Content)
	}
	if !strings.Contains(res.Content, `"memory_block":null`) {
		t.Fatalf("expected explicit memory_block:null in tool result, got: %s", res.Content)
	}
	var body map[string]any
	json.Unmarshal([]byte(res.Content), &body)
	if body["memory_block"] != nil {
		t.Fatalf("decoded memory_block should be JSON null (nil interface), got: %#v", body["memory_block"])
	}
	cands, _ := body["candidates"].([]any)
	if len(cands) != 1 {
		t.Fatalf("legacy candidates required for older-sidecar fallback: %+v", cands)
	}
}

func TestMemoryTool_MemoryBlock_WeakResultDoesNotBecomeCompleteAnswer(t *testing.T) {
	env := &memory.ResponseEnvelope{
		Reason:        "ok",
		BundleVersion: "0.6.0",
		MemoryBlock: &memory.MemoryBlock{
			Groups: []memory.MemoryCandidateGroup{{
				Value:        "contextual hint",
				Score:        0.5,
				Evidence:     "observed",
				SupportCount: 1,
			}},
		},
	}
	tool := &MemoryTool{
		Service:  &stubQuerier{status: memory.StatusReady, env: env, class: memory.ClassOK},
		Fallback: &fakeFallback{},
	}
	res, _ := tool.Run(context.Background(), `{"mode":"path_query","anchor_mentions":["x"],"relation_constraints":["works_on","created"]}`)
	if res.IsError {
		t.Fatalf("res.IsError: %s", res.Content)
	}
	var body map[string]any
	json.Unmarshal([]byte(res.Content), &body)
	if body["memory_recall_authority"] != nil {
		t.Fatalf("memory_recall_authority should not be present in OSS sidecar output, got %v", body["memory_recall_authority"])
	}
	if body["memory_recall_directive"] != nil {
		t.Fatalf("memory_recall_directive should not be present in OSS sidecar output, got %v", body["memory_recall_directive"])
	}
}

// TestMemoryTool_StructuredNoData_NoFallback locks the rule from the
// design (Section 3): when the sidecar returns 200 OK with reason=no_data
// and a structured no_data_reason, the tool surfaces the structured shape
// to the LLM and does NOT silently invoke the keyword fallback.
func TestMemoryTool_StructuredNoData_NoFallback(t *testing.T) {
	reason := "no_matches"
	env := &memory.ResponseEnvelope{
		Reason:        "no_data",
		BundleVersion: "0.6.0",
		Candidates:    []memory.QueryCandidate{},
		MemoryBlock: &memory.MemoryBlock{
			Groups:       []memory.MemoryCandidateGroup{},
			NoDataReason: &reason,
			Notes:        []string{},
		},
	}
	fb := &fakeFallback{snippet: "should-not-be-used", hits: []any{map[string]any{"id": "should-not-be-used"}}}
	tool := &MemoryTool{
		Service:  &stubQuerier{status: memory.StatusReady, env: env, class: memory.ClassOK},
		Fallback: fb,
	}
	res, err := tool.Run(context.Background(), `{"anchor_mentions":["Alice Nakamura"],"relation_constraints":["created"]}`)
	if err != nil || res.IsError {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if fb.gotQuery != "" || fb.snippetQuery != "" {
		t.Fatalf("fallback was invoked despite structured no-data: gotQuery=%q snippetQuery=%q", fb.gotQuery, fb.snippetQuery)
	}
	var body map[string]any
	json.Unmarshal([]byte(res.Content), &body)
	if body["source"] != "memory_sidecar" {
		t.Fatalf("source=%v want memory_sidecar (no fallback)", body["source"])
	}
	mb, ok := body["memory_block"].(map[string]any)
	if !ok {
		t.Fatalf("memory_block missing: %#v", body["memory_block"])
	}
	if mb["no_data_reason"] != "no_matches" {
		t.Fatalf("no_data_reason=%v want no_matches", mb["no_data_reason"])
	}
	if body["evidence_quality"] != "structured" {
		t.Fatalf("evidence_quality=%v want structured (binary, no third state for no-data)", body["evidence_quality"])
	}
	if body["memory_recall_directive"] != nil {
		t.Fatalf("memory_recall_directive should not be present in OSS sidecar output, got %v", body["memory_recall_directive"])
	}
}

// TestMemoryTool_FallbackInvokesProvider locks the contract that fallback()
// actually delegates to FallbackQuery.SessionKeyword + MemoryFileSnippet
// (regression: earlier the fallback returned an empty envelope and the
// provider plumbing was dead code).
func TestMemoryTool_FallbackInvokesProvider(t *testing.T) {
	fb := &fakeFallback{
		snippet: "MEMORY.md hit line",
		hits:    []any{map[string]any{"id": "sess1", "snippet": "from session_search"}},
	}
	tool := &MemoryTool{Fallback: fb}
	res, _ := tool.Run(context.Background(), `{"anchor_mentions":["foo","bar"],"result_limit":7}`)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	if fb.gotQuery != "foo bar" {
		t.Fatalf("session keyword query = %q want %q", fb.gotQuery, "foo bar")
	}
	if fb.gotLimit != 7 {
		t.Fatalf("session keyword limit = %d want 7", fb.gotLimit)
	}
	if fb.snippetQuery != "foo bar" {
		t.Fatalf("memory snippet query = %q want %q", fb.snippetQuery, "foo bar")
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(res.Content), &body); err != nil {
		t.Fatal(err)
	}
	cands, _ := body["candidates"].([]any)
	if len(cands) != 2 {
		t.Fatalf("expected 2 fallback candidates (1 session hit + 1 MEMORY.md snippet), got %d: %+v", len(cands), cands)
	}
}
