package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/memory"
)

type fakeMemoryPreflightQuerier struct {
	status  memory.ServiceStatus
	intents []memory.QueryIntent
	results []memory.QueryResult
}

func (q *fakeMemoryPreflightQuerier) Status() memory.ServiceStatus {
	return q.status
}

func (q *fakeMemoryPreflightQuerier) QueryBatch(ctx context.Context, intents []memory.QueryIntent) []memory.QueryResult {
	q.intents = append([]memory.QueryIntent(nil), intents...)
	return q.results
}

type fakeMemoryHelperLLM struct {
	responses []*client.CompletionResponse
	errors    []error
	requests  []client.CompletionRequest
}

func (m *fakeMemoryHelperLLM) Complete(ctx context.Context, req client.CompletionRequest) (*client.CompletionResponse, error) {
	m.requests = append(m.requests, req)
	if len(m.errors) > 0 {
		err := m.errors[0]
		m.errors = m.errors[1:]
		if err != nil {
			return nil, err
		}
	}
	if len(m.responses) == 0 {
		return &client.CompletionResponse{}, nil
	}
	resp := m.responses[0]
	m.responses = m.responses[1:]
	return resp, nil
}

func (m *fakeMemoryHelperLLM) CompleteStream(ctx context.Context, req client.CompletionRequest, onDelta func(client.StreamDelta)) (*client.CompletionResponse, error) {
	return m.Complete(ctx, req)
}

func helperToolResponse(t *testing.T, out helperMemoryOutput, usage client.Usage) *client.CompletionResponse {
	t.Helper()
	args, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal helper output: %v", err)
	}
	return &client.CompletionResponse{
		ToolCalls: []client.FunctionCall{{
			Name:      memoryHelperToolName,
			Arguments: args,
		}},
		Usage: usage,
	}
}

func TestDetectMemoryIntents_DeterministicRelationship(t *testing.T) {
	tests := []struct {
		query  string
		anchor string
	}{
		{query: "示例联系人与我的关系", anchor: "示例联系人"},
		{query: "我和示例联系人是什么关系？", anchor: "示例联系人"},
		{query: "who is Example Contact to me?", anchor: "Example Contact"},
		{query: "my relationship with Example Contact", anchor: "Example Contact"},
		{query: "how do I know Example Contact?", anchor: "Example Contact"},
		{query: "Example Contactと私の関係", anchor: "Example Contact"},
		{query: "私とExample Contactはどんな関係？", anchor: "Example Contact"},
		{query: "Example Contactは私にとって誰？", anchor: "Example Contact"},
	}

	for _, tt := range tests {
		t.Run(tt.query, func(t *testing.T) {
			intents, usage := DetectMemoryIntents(context.Background(), nil, tt.query)
			if usage.TotalTokens != 0 {
				t.Fatalf("usage=%+v; deterministic tier should not call helper", usage)
			}
			if len(intents) != 1 {
				t.Fatalf("len(intents)=%d want 1", len(intents))
			}
			intent := intents[0]
			if intent.Mode != memory.ModeDirectRelation {
				t.Fatalf("mode=%q want %q", intent.Mode, memory.ModeDirectRelation)
			}
			if got := strings.Join(intent.AnchorMentions, ","); got != tt.anchor {
				t.Fatalf("anchor=%q want %q", got, tt.anchor)
			}
			if len(intent.RelationConstraints) != 0 {
				t.Fatalf("relation_constraints=%v want empty", intent.RelationConstraints)
			}
			if intent.EvidenceBudget != 5 || intent.ResultLimit != 10 {
				t.Fatalf("budgets=(%d,%d) want (5,10)", intent.EvidenceBudget, intent.ResultLimit)
			}
		})
	}
}

func TestDetectMemoryIntents_SkipsPronounAnchor(t *testing.T) {
	for _, query := range []string{"我和我是什么关系？", "私と私はどんな関係？"} {
		intents, _ := DetectMemoryIntents(context.Background(), nil, query)
		if len(intents) != 0 {
			t.Fatalf("%q intents=%v want none", query, intents)
		}
	}
}

func TestDetectMemoryIntents_HelperToolCallHappyPath(t *testing.T) {
	resp := helperToolResponse(t, helperMemoryOutput{
		ShouldRecall: true,
		GateReason:   "entity relation question",
		Intents: []memory.QueryIntent{{
			Mode:                memory.ModeDirectRelation,
			AnchorMentions:      []string{"示例联系人"},
			RelationConstraints: []string{"related_to", "employed_at"},
			EvidenceBudget:      5,
			ResultLimit:         10,
		}},
	}, client.Usage{InputTokens: 11, OutputTokens: 7, TotalTokens: 18})
	llm := &fakeMemoryHelperLLM{responses: []*client.CompletionResponse{resp}}

	intents, usage := DetectMemoryIntents(context.Background(), llm, "示例联系人是谁？")
	if len(llm.requests) != 1 {
		t.Fatalf("helper calls=%d want 1", len(llm.requests))
	}
	req := llm.requests[0]
	if req.ModelTier != "small" || req.CacheSource != "helper" || req.Temperature != 0 {
		t.Fatalf("helper request tier/cache/temp=(%q,%q,%v)", req.ModelTier, req.CacheSource, req.Temperature)
	}
	if len(req.Tools) != 1 || req.Tools[0].Function.Name != memoryHelperToolName {
		t.Fatalf("helper request missing forced tool: %+v", req.Tools)
	}
	choice, ok := req.ToolChoice.(map[string]any)
	if !ok || choice["type"] != "tool" || choice["name"] != memoryHelperToolName {
		t.Fatalf("helper request must force compile_memory_intents tool; tool_choice=%+v", req.ToolChoice)
	}
	if usage.TotalTokens != 18 {
		t.Fatalf("usage=%+v want helper usage", usage)
	}
	if len(intents) != 1 || strings.Join(intents[0].AnchorMentions, ",") != "示例联系人" {
		t.Fatalf("intents=%v", intents)
	}
	if len(intents[0].RelationConstraints) != 1 || intents[0].RelationConstraints[0] != "employed_at" {
		t.Fatalf("relation_constraints=%v want [employed_at]", intents[0].RelationConstraints)
	}
}

func TestDetectMemoryIntents_HelperPromptCarriesCatalogInSystemMessage(t *testing.T) {
	resp := helperToolResponse(t, helperMemoryOutput{ShouldRecall: false}, client.Usage{})
	llm := &fakeMemoryHelperLLM{responses: []*client.CompletionResponse{resp}}

	_, _ = DetectMemoryIntents(context.Background(), llm, "示例联系人是谁？")
	if len(llm.requests) != 1 {
		t.Fatalf("helper calls=%d want 1", len(llm.requests))
	}
	req := llm.requests[0]
	if len(req.Messages) != 2 || req.Messages[0].Role != "system" || req.Messages[1].Role != "user" {
		t.Fatalf("helper messages=%+v", req.Messages)
	}
	system := req.Messages[0].Content.Text()
	user := req.Messages[1].Content.Text()

	// Catalog must live in the cacheable system message so identical helper
	// calls share a prompt-cache hit. If this regresses (catalog goes back
	// into the per-call user message), helper cache CER drops to ~0.
	if !strings.Contains(system, "people_and_social:") || !strings.Contains(system, "technical_and_project:") {
		t.Fatalf("system prompt missing relation catalog: %q", system)
	}
	if strings.Contains(user, "people_and_social:") || strings.Contains(user, "technical_and_project:") {
		t.Fatalf("relation catalog leaked into per-call user message: %q", user)
	}
	if !strings.Contains(system, "私") || !strings.Contains(system, "わたし") {
		t.Fatalf("system prompt missing Japanese pronoun blocklist: %q", system)
	}
	for _, privateDetail := range []string{"Person currently employed", "RESCAL", "inverse direction"} {
		if strings.Contains(system, privateDetail) {
			t.Fatalf("system prompt leaked private ontology %q in %q", privateDetail, system)
		}
	}
}

func TestDetectMemoryIntents_HelperToolCallFailureModes(t *testing.T) {
	tests := []struct {
		name      string
		resp      *client.CompletionResponse
		wantClass string
	}{
		{
			name:      "no tool call",
			resp:      &client.CompletionResponse{OutputText: "I am ignoring the tool"},
			wantClass: "no_tool_call",
		},
		{
			name: "wrong tool name",
			resp: &client.CompletionResponse{ToolCalls: []client.FunctionCall{{
				Name:      "some_other_tool",
				Arguments: []byte(`{"should_recall":false,"intents":[]}`),
			}}},
			wantClass: "wrong_tool",
		},
		{
			name: "invalid arguments JSON",
			resp: &client.CompletionResponse{ToolCalls: []client.FunctionCall{{
				Name:      memoryHelperToolName,
				Arguments: []byte(`{"should_recall":true,"intents":`),
			}}},
			wantClass: "invalid_tool_args",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			llm := &fakeMemoryHelperLLM{responses: []*client.CompletionResponse{tt.resp}}
			trace := &agent.MemoryPreflightTrace{}
			intents, _ := DetectMemoryIntents(context.Background(), llm, "tell me about Example Contact from memory", MemoryIntentOptions{ForceHelper: true, Trace: trace})
			if len(intents) != 0 {
				t.Fatalf("intents=%v want none", intents)
			}
			if trace.Outcome != "helper_error" {
				t.Fatalf("outcome=%q want helper_error", trace.Outcome)
			}
			if trace.ErrorClass != tt.wantClass {
				t.Fatalf("error_class=%q want %q", trace.ErrorClass, tt.wantClass)
			}
		})
	}
}

func TestDetectMemoryIntents_ForceHelperBypassesCheapGate(t *testing.T) {
	resp := helperToolResponse(t, helperMemoryOutput{
		ShouldRecall: false,
		GateReason:   "not memory",
	}, client.Usage{})
	llm := &fakeMemoryHelperLLM{responses: []*client.CompletionResponse{resp}}

	_, _ = DetectMemoryIntents(context.Background(), llm, "hello", MemoryIntentOptions{ForceHelper: true})
	if len(llm.requests) != 1 {
		t.Fatalf("helper calls=%d want 1", len(llm.requests))
	}
}

func TestDetectMemoryIntents_HelperDropsJapanesePronounAnchor(t *testing.T) {
	resp := helperToolResponse(t, helperMemoryOutput{
		ShouldRecall: true,
		Intents: []memory.QueryIntent{{
			Mode:                memory.ModeDirectRelation,
			AnchorMentions:      []string{"私"},
			RelationConstraints: []string{"employed_at"},
			EvidenceBudget:      5,
			ResultLimit:         10,
		}},
	}, client.Usage{})
	llm := &fakeMemoryHelperLLM{responses: []*client.CompletionResponse{resp}}

	intents, _ := DetectMemoryIntents(context.Background(), llm, "私は誰？")
	if len(intents) != 0 {
		t.Fatalf("intents=%v want none", intents)
	}
}

func TestDetectMemoryIntents_SkipsTaskLikePrompt(t *testing.T) {
	llm := &fakeMemoryHelperLLM{}
	intents, _ := DetectMemoryIntents(context.Background(), llm, "fix the failing test in memory_preflight.go")
	if len(intents) != 0 {
		t.Fatalf("intents=%v want none", intents)
	}
	if len(llm.requests) != 0 {
		t.Fatalf("helper calls=%d want 0", len(llm.requests))
	}
}

func TestDetectMemoryIntents_TypedNilLLMSkipsHelper(t *testing.T) {
	var gw *client.GatewayClient
	intents, _ := DetectMemoryIntents(context.Background(), gw, "示例联系人是谁？")
	if len(intents) != 0 {
		t.Fatalf("intents=%v want none", intents)
	}
}

func TestDetectMemoryIntents_LongQueryTruncatesInsteadOfRefusing(t *testing.T) {
	// Build a query well over memoryHelperMaxInputRunes that still mentions
	// a named entity, so the cheap gate accepts the truncated head.
	long := strings.Repeat("a", memoryHelperMaxInputRunes+200) + " Example Contact employed_at"
	resp := helperToolResponse(t, helperMemoryOutput{
		ShouldRecall: true,
		Intents: []memory.QueryIntent{{
			Mode:                memory.ModeDirectRelation,
			AnchorMentions:      []string{"Example Contact"},
			RelationConstraints: []string{"employed_at"},
			EvidenceBudget:      5,
			ResultLimit:         10,
		}},
	}, client.Usage{})
	llm := &fakeMemoryHelperLLM{responses: []*client.CompletionResponse{resp}}
	intents, _ := DetectMemoryIntents(context.Background(), llm, long, MemoryIntentOptions{ForceHelper: true})
	if len(llm.requests) != 1 {
		t.Fatalf("helper calls=%d want 1 (long query should be truncated, not refused)", len(llm.requests))
	}
	sent := llm.requests[0].Messages[1].Content.Text()
	// User message is JSON-encoded query string; the encoded length must
	// reflect the truncated head, not the full input.
	if len([]rune(sent)) > memoryHelperMaxInputRunes+8 { // +8 covers the two quote chars and any small escape overhead
		t.Fatalf("user message not truncated: %d runes", len([]rune(sent)))
	}
	if len(intents) != 1 {
		t.Fatalf("intents=%v want one", intents)
	}
}

func TestDetectMemoryIntents_HelperErrorClass(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		resp       *client.CompletionResponse
		wantClass  string
		wantStatus int
	}{
		{name: "timeout", err: context.DeadlineExceeded, wantClass: "timeout"},
		{name: "canceled", err: context.Canceled, wantClass: "canceled"},
		{name: "auth", err: &client.APIError{StatusCode: 401, Body: "do not log this"}, wantClass: "auth", wantStatus: 401},
		{name: "rate limited", err: &client.APIError{StatusCode: 429}, wantClass: "rate_limited", wantStatus: 429},
		{name: "provider server", err: &client.APIError{StatusCode: 503}, wantClass: "provider_server", wantStatus: 503},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			llm := &fakeMemoryHelperLLM{errors: []error{tt.err}, responses: []*client.CompletionResponse{tt.resp}}
			trace := &agent.MemoryPreflightTrace{}
			intents, _ := DetectMemoryIntents(context.Background(), llm, "hello", MemoryIntentOptions{ForceHelper: true, Trace: trace})
			if len(intents) != 0 {
				t.Fatalf("intents=%v want none", intents)
			}
			if trace.Outcome != "helper_error" {
				t.Fatalf("outcome=%q want helper_error", trace.Outcome)
			}
			if trace.ErrorClass != tt.wantClass {
				t.Fatalf("error_class=%q want %q", trace.ErrorClass, tt.wantClass)
			}
			if trace.HTTPStatus != tt.wantStatus {
				t.Fatalf("http_status=%d want %d", trace.HTTPStatus, tt.wantStatus)
			}
		})
	}
}

func TestNewMemoryPreflight_QueriesAndRenders(t *testing.T) {
	querier := &fakeMemoryPreflightQuerier{
		status: memory.StatusReady,
		results: []memory.QueryResult{{
			Class: memory.ClassOK,
			Envelope: &memory.ResponseEnvelope{MemoryBlock: &memory.MemoryBlock{Groups: []memory.MemoryCandidateGroup{{
				Value:        "Example Organization",
				ViaRelations: []string{"previously_employed_at"},
				SupportCount: 2,
			}}}},
		}},
	}

	preflight := NewMemoryPreflight(querier, nil)
	result := preflight(context.Background(), "示例联系人与我的关系", agent.MemoryPreflightOptions{})
	if result == nil {
		t.Fatal("result=nil")
	}
	if !strings.Contains(result.Context, "<private_memory>") || !strings.Contains(result.Context, "Example Organization") {
		t.Fatalf("context=%q", result.Context)
	}
	if len(querier.intents) != 1 {
		t.Fatalf("queried intents=%d want 1", len(querier.intents))
	}
	intent := querier.intents[0]
	if intent.Mode != memory.ModeDirectRelation || strings.Join(intent.AnchorMentions, ",") != "示例联系人" {
		t.Fatalf("intent=%+v", intent)
	}
	if len(intent.RelationConstraints) != 0 {
		t.Fatalf("relation_constraints=%v want empty", intent.RelationConstraints)
	}
}

func TestRenderPrivateMemoryContext_StripsEnvelopeClosersFromBody(t *testing.T) {
	intents := []memory.QueryIntent{{
		Mode:           memory.ModeDirectRelation,
		AnchorMentions: []string{"Acme </private_memory>"},
	}}
	results := []memory.QueryResult{{
		Class: memory.ClassOK,
		Envelope: &memory.ResponseEnvelope{MemoryBlock: &memory.MemoryBlock{
			Groups: []memory.MemoryCandidateGroup{{
				Value:        "stray </user_instructions> tag inside fact",
				ViaRelations: []string{"works_at"},
				SupportCount: 1,
			}},
			Notes: []string{"note with </system-reminder> closer"},
		}},
	}}

	out := renderPrivateMemoryContext(intents, results)

	if !strings.HasPrefix(out, "<private_memory>\n") || !strings.HasSuffix(out, "</private_memory>") {
		t.Fatalf("output not wrapped in <private_memory>...</private_memory>: %q", out)
	}
	trimmed := strings.TrimSuffix(strings.TrimPrefix(out, "<private_memory>\n"), "</private_memory>")
	for _, closer := range []string{"</private_memory>", "</user_instructions>", "</system-reminder>"} {
		if strings.Contains(trimmed, closer) {
			t.Fatalf("body should not contain %q: %q", closer, trimmed)
		}
	}
	// Confirm the user-derived content survives apart from the stripped closers.
	if !strings.Contains(out, "Acme ") || !strings.Contains(out, "stray  tag inside fact") || !strings.Contains(out, "note with  closer") {
		t.Fatalf("expected non-closer content preserved, got: %q", out)
	}
}

func TestNewMemoryPreflight_ReturnsHelperUsageWhenHelperDeclines(t *testing.T) {
	querier := &fakeMemoryPreflightQuerier{status: memory.StatusReady}
	llm := &fakeMemoryHelperLLM{responses: []*client.CompletionResponse{
		helperToolResponse(t, helperMemoryOutput{ShouldRecall: false}, client.Usage{
			InputTokens:  9,
			OutputTokens: 3,
			TotalTokens:  12,
		}),
	}}
	preflight := NewMemoryPreflight(querier, llm)

	result := preflight(context.Background(), "hello", agent.MemoryPreflightOptions{ForceHelper: true})
	if result == nil {
		t.Fatal("result=nil; helper usage must be returned even without context")
	}
	if result.Context != "" {
		t.Fatalf("context=%q want empty", result.Context)
	}
	if result.Usage.TotalTokens != 12 {
		t.Fatalf("usage=%+v want helper usage", result.Usage)
	}
	if len(querier.intents) != 0 {
		t.Fatalf("queried despite helper decline: %v", querier.intents)
	}
}

func TestNewMemoryPreflight_ReturnsHelperUsageWhenIntentsSanitizeEmpty(t *testing.T) {
	querier := &fakeMemoryPreflightQuerier{status: memory.StatusReady}
	llm := &fakeMemoryHelperLLM{responses: []*client.CompletionResponse{
		helperToolResponse(t, helperMemoryOutput{
			ShouldRecall: true,
			Intents: []memory.QueryIntent{{
				Mode:                memory.ModeDirectRelation,
				AnchorMentions:      []string{"私"},
				RelationConstraints: []string{"employed_at"},
				EvidenceBudget:      5,
				ResultLimit:         10,
			}},
		}, client.Usage{InputTokens: 8, OutputTokens: 2, TotalTokens: 10}),
	}}
	preflight := NewMemoryPreflight(querier, llm)

	result := preflight(context.Background(), "私は誰？", agent.MemoryPreflightOptions{ForceHelper: true})
	if result == nil {
		t.Fatal("result=nil; sanitized-empty helper usage must still be returned")
	}
	if result.Context != "" {
		t.Fatalf("context=%q want empty", result.Context)
	}
	if result.Usage.TotalTokens != 10 {
		t.Fatalf("usage=%+v want helper usage", result.Usage)
	}
	if len(querier.intents) != 0 {
		t.Fatalf("queried despite empty sanitized intents: %v", querier.intents)
	}
}

func TestNewMemoryPreflight_FailSilentWhenUnavailable(t *testing.T) {
	querier := &fakeMemoryPreflightQuerier{status: memory.StatusUnavailable}
	preflight := NewMemoryPreflight(querier, nil)
	if got := preflight(context.Background(), "示例联系人与我的关系", agent.MemoryPreflightOptions{}); got != nil {
		t.Fatalf("result=%+v want nil", got)
	}
	if len(querier.intents) != 0 {
		t.Fatalf("queried while unavailable: %v", querier.intents)
	}
}

func TestMemoryHelperToolSchemaIsByteStable(t *testing.T) {
	// Helper system prompt and tool schema must be byte-stable across calls so
	// they participate in the small-tier prompt cache. Regenerate both, marshal
	// to JSON, and compare to a recorded baseline.
	if got := memoryHelperSystemPrompt; got != buildMemoryHelperSystemPrompt() {
		t.Fatalf("system prompt not stable across rebuilds")
	}
	a, err := json.Marshal(memoryHelperTool)
	if err != nil {
		t.Fatalf("marshal helper tool: %v", err)
	}
	b, err := json.Marshal(buildMemoryHelperTool())
	if err != nil {
		t.Fatalf("marshal helper tool (rebuilt): %v", err)
	}
	if string(a) != string(b) {
		t.Fatalf("helper tool schema drifted across rebuilds:\n  a=%s\n  b=%s", a, b)
	}
}
