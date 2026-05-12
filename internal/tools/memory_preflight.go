package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"reflect"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/memory"
	"github.com/Kocoro-lab/ShanClaw/internal/prompt"
)

const (
	memoryHelperTimeout       = 5 * time.Second
	memoryQueryTimeout        = 2 * time.Second
	memoryHelperMaxInputRunes = 500
	memoryHelperToolName      = "compile_memory_intents"

	// privateMemoryBodyByteCap bounds the size of the body inside the
	// <private_memory> envelope. With result_limit=10 per intent × up to
	// 3 intents = 30 groups of free-form text, an unbounded body could
	// blow out the cacheable user-message payload. 8 KiB is comfortably
	// larger than any realistic recall result set while keeping the
	// injected context modest. Exceeding the cap truncates at the last
	// newline before the limit and appends a marker line.
	privateMemoryBodyByteCap = 8 * 1024
)

// MemoryPreflightQuerier is satisfied by memory.Service and memory.AttachedQuerier.
type MemoryPreflightQuerier interface {
	Status() memory.ServiceStatus
	QueryBatch(ctx context.Context, intents []memory.QueryIntent) []memory.QueryResult
}

// helperMemoryOutput is the schema bound to the forced tool_use call. Providers
// enforce this at the tool_use boundary, so the lenient JSON fallback ladder is
// intentionally gone.
type helperMemoryOutput struct {
	ShouldRecall bool                 `json:"should_recall"`
	GateReason   string               `json:"gate_reason,omitempty"`
	Intents      []memory.QueryIntent `json:"intents,omitempty"`
}

type MemoryIntentOptions struct {
	ForceHelper bool
	Trace       *agent.MemoryPreflightTrace
}

type exactMemoryPattern struct {
	re          *regexp.Regexp
	anchorGroup int
}

var exactRelationshipPatterns = []exactMemoryPattern{
	{re: regexp.MustCompile(`^\s*(.+?)\s*(?:与|和|跟)\s*我\s*(?:的)?\s*关系\s*[？?。.!！]*\s*$`), anchorGroup: 1},
	{re: regexp.MustCompile(`^\s*我\s*(?:与|和|跟)\s*(.+?)\s*(?:是\s*)?(?:什么|什麼|啥|怎样|怎樣)?\s*关系\s*[？?。.!！]*\s*$`), anchorGroup: 1},
	{re: regexp.MustCompile(`^\s*我\s*(?:认识|認識|见过|見過)\s*(.+?)\s*(?:吗|嗎)?\s*[？?。.!！]*\s*$`), anchorGroup: 1},
	{re: regexp.MustCompile(`(?i)^\s*who\s+is\s+(.+?)\s+to\s+me\s*[?.!]*\s*$`), anchorGroup: 1},
	{re: regexp.MustCompile(`(?i)^\s*my\s+relationship\s+with\s+(.+?)\s*[?.!]*\s*$`), anchorGroup: 1},
	{re: regexp.MustCompile(`(?i)^\s*how\s+do\s+i\s+know\s+(.+?)\s*[?.!]*\s*$`), anchorGroup: 1},
	{re: regexp.MustCompile(`(?i)^\s*what\s+is\s+my\s+(?:connection|relationship)\s+(?:to|with)\s+(.+?)\s*[?.!]*\s*$`), anchorGroup: 1},
	{re: regexp.MustCompile(`(?i)^\s*(?:do|did)\s+i\s+(?:know|meet)\s+(.+?)\s*[?.!]*\s*$`), anchorGroup: 1},
	{re: regexp.MustCompile(`(?i)^\s*where\s+do\s+i\s+know\s+(.+?)\s+from\s*[?.!]*\s*$`), anchorGroup: 1},
	{re: regexp.MustCompile(`^\s*(.+?)\s*と\s*(?:私|わたし|僕|俺|自分)\s*(?:の)?\s*関係\s*[？?。.!！]*\s*$`), anchorGroup: 1},
	{re: regexp.MustCompile(`^\s*(?:私|わたし|僕|俺|自分)\s*と\s*(.+?)\s*(?:は|って)?\s*(?:どんな|どういう)?\s*関係\s*[？?。.!！]*\s*$`), anchorGroup: 1},
	{re: regexp.MustCompile(`^\s*(.+?)\s*は\s*(?:私|わたし|僕|俺|自分)\s*にとって\s*(?:誰|何)\s*[？?。.!！]*\s*$`), anchorGroup: 1},
	{re: regexp.MustCompile(`^\s*(.+?)\s*を\s*(?:どう|どこで)?\s*知って(?:いる|る)?\s*(?:の|か)?\s*[？?。.!！]*\s*$`), anchorGroup: 1},
	{re: regexp.MustCompile(`^\s*(.+?)\s*に\s*会ったこと(?:が)?ある\s*(?:の|か)?\s*[？?。.!！]*\s*$`), anchorGroup: 1},
}

var latinEntityPattern = regexp.MustCompile(`(?:^|[\s"'(（【])([A-Z][\p{L}\p{N}_&.+-]*(?:\s+[A-Z0-9][\p{L}\p{N}_&.+-]*){0,4})`)

type compactMemoryRelationGroup struct {
	Name      string
	Relations []string
}

// compactMemoryRelationCatalog is the public-safe summary of canonical
// relation ids passed to the helper model. It must never contain private
// ontology details (descriptions, weights, inverse mappings, evidence rules).
var compactMemoryRelationCatalog = []compactMemoryRelationGroup{
	{
		Name: "people_and_social",
		Relations: []string{
			"employed_at", "previously_employed_at", "works_on", "affiliated_with",
			"studied_under", "studied_at", "collaborates_with", "follows_person",
			"followed_by_person", "commented_on", "knows_about", "has_handle_on",
			"has_email",
		},
	},
	{
		Name: "ownership_and_company",
		Relations: []string{
			"created", "created_by", "maintained_by", "develops",
			"developed_by_org", "owns", "owned_by", "acquired", "acquired_by",
			"subsidiary_of", "parent_of", "founded", "founded_by", "invested_in",
			"received_investment_from", "customer_of", "has_customer",
			"competes_with", "banking_relationship",
		},
	},
	{
		Name: "technical_and_project",
		Relations: []string{
			"uses", "used_by", "depends_on", "implemented_in", "runs_on",
			"integrates_with", "supports", "powered_by", "loaded_via",
			"has_component", "part_of", "has_property", "has_path", "stored_at",
			"monitors", "targets", "enables", "enabled_by", "generates",
			"generated_from", "implements", "implemented_by", "excludes",
			"deleted_from",
		},
	},
	{
		Name: "content_and_metadata",
		Relations: []string{
			"published_on", "released", "latest_release_tag", "forked_from",
			"inspired_by", "succeeds", "preceded_by", "describes", "described_in",
			"category", "has_alias", "has_url", "located_in", "scheduled_for",
			"ranked_on", "listed_on", "features_project",
		},
	},
	{
		Name:      "generic_fallback",
		Relations: []string{"related_to", "other"},
	},
}

var knownMemoryRelations = buildKnownMemoryRelations()

// memoryHelperSystemPrompt is byte-stable across helper calls so it caches
// against the small-tier model's prompt-cache prefix. The relation catalog,
// rules, and pronoun blocklist all live here — the per-call user message is
// just the JSON-encoded query.
var memoryHelperSystemPrompt = buildMemoryHelperSystemPrompt()

func buildMemoryHelperSystemPrompt() string {
	var sb strings.Builder
	sb.WriteString("You compile private episodic memory preflight intents. ")
	sb.WriteString("Call the compile_memory_intents tool exactly once for every user message. Do not reply with prose. Do not answer the user.\n\n")
	sb.WriteString("Set should_recall=false when the message is generic knowledge, public biographies, coding tasks, file/build/test chores, platform config chores, web/current facts, greetings, small talk, or anything that does not ask about the user's private episodic memory. In that case intents must be empty.\n\n")
	sb.WriteString("Set should_recall=true only when the message asks about people, projects, companies, tools, content, or facts the user would only know from their own past records.\n\n")
	sb.WriteString("Anchor rules:\n")
	sb.WriteString("- anchor_mentions must name a concrete entity surface form (person, org, project, tool, file, URL). Never use first-person pronouns. Forbidden anchors: \"I\", \"me\", \"my\", \"mine\", \"myself\", \"user\", \"the user\", \"我\", \"我的\", \"自己\", \"本人\", \"私\", \"わたし\", \"僕\", \"俺\", \"自分\".\n")
	sb.WriteString("- If the question has no concrete anchor, set should_recall=false.\n\n")
	sb.WriteString("Mode rules:\n")
	sb.WriteString("- direct_relation: one-hop or broad relationship questions about a named entity. Use empty relation_constraints for broad \"who is X to me\" / \"X与我的关系\" questions; use a single canonical relation when the user clearly asks one specific fact. Use target_slot=head to look up the inverse subject without renaming the relation.\n")
	sb.WriteString("- path_query: explicit 2-4 hop chain. relation_constraints must have 2-4 canonical relations. Inverse hops use relation^-1.\n")
	sb.WriteString("- typed_neighborhood: user asks for typed targets (\"which people / which company / 哪些项目\"). Requires candidate_type plus exactly one canonical relation_constraint.\n\n")
	sb.WriteString("Relation vocabulary:\n")
	sb.WriteString("- Use canonical snake_case ids only. If none fit cleanly, leave relation_constraints empty. Never invent ids.\n")
	sb.WriteString("- Do not use the generic ids \"related_to\" or \"other\" unless the user explicitly asks a generic association.\n\n")
	sb.WriteString("Scope/time:\n")
	sb.WriteString("- Leave scope_filter empty unless the user explicitly names a durable project or workspace.\n")
	sb.WriteString("- Leave time_window null unless the user names a concrete time anchor.\n\n")
	sb.WriteString("Budgets:\n")
	sb.WriteString("- Emit at most 3 intents.\n")
	sb.WriteString("- evidence_budget=5 and result_limit=10 unless the user explicitly asks for more.\n\n")
	sb.WriteString("Entity types: Person, Company, Organization, Project, Tool, Language, Concept, Document, Article, BlogPost, File, Country, Location, Region, Product, Agent, Application, Format, Website, Platform, ContactInfo, Tag.\n\n")
	sb.WriteString("Canonical relation ids by group:\n")
	sb.WriteString(memoryRelationCatalog())
	return sb.String()
}

// memoryHelperTool is the forced tool the helper must call. Providers enforce
// the schema at the tool_use boundary, so we don't need a lenient parser.
var memoryHelperTool = buildMemoryHelperTool()

func buildMemoryHelperTool() client.Tool {
	intentSchema := map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"mode": map[string]any{
				"type": "string",
				"enum": []string{
					string(memory.ModeDirectRelation),
					string(memory.ModePathQuery),
					string(memory.ModeTypedNeighborhood),
				},
				"description": "Query mode. direct_relation = one-hop or broad relationship. path_query = 2-4 hop chain. typed_neighborhood = list typed targets via a relation.",
			},
			"anchor_mentions": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"minItems":    1,
				"maxItems":    4,
				"description": "Concrete entity surfaces. Never first-person pronouns.",
			},
			"relation_constraints": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"maxItems":    4,
				"description": "Canonical snake_case relation ids from the system catalog. Empty for broad relationship questions. Inverse hops use relation^-1 (path_query only).",
			},
			"candidate_type": map[string]any{
				"type":        "string",
				"description": "Required for typed_neighborhood; entity-type name to filter targets.",
			},
			"scope_filter": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"maxItems":    4,
				"description": "Durable project or workspace names. Leave empty unless the user explicitly named one.",
			},
			"target_slot": map[string]any{
				"type":        "string",
				"enum":        []string{"head", "tail"},
				"description": "Which side of the relation is the unknown. Use head for inverse subject lookup; otherwise omit.",
			},
			"time_window": map[string]any{
				"type":        "string",
				"description": "ISO-8601 interval or named window. Omit unless the user named a concrete time anchor.",
			},
			"evidence_budget": map[string]any{
				"type":        "integer",
				"minimum":     1,
				"maximum":     50,
				"description": "How many evidence events the sidecar should consider. Default 5.",
			},
			"result_limit": map[string]any{
				"type":        "integer",
				"minimum":     1,
				"maximum":     100,
				"description": "How many candidate groups to return. Default 10.",
			},
		},
		"required": []string{"mode", "anchor_mentions"},
	}
	params := map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"should_recall": map[string]any{
				"type":        "boolean",
				"description": "True only when the user is asking about their own private episodic memory.",
			},
			"gate_reason": map[string]any{
				"type":        "string",
				"description": "One short phrase (under 40 chars) describing the gate decision. Do not include user content.",
			},
			"intents": map[string]any{
				"type":     "array",
				"items":    intentSchema,
				"maxItems": 3,
			},
		},
		"required": []string{"should_recall", "intents"},
	}
	return client.Tool{
		Type: "function",
		Function: client.FunctionDef{
			Name:        memoryHelperToolName,
			Description: "Compile private-memory preflight intents for one user message. Call exactly once. should_recall=false with empty intents is the correct response whenever the user is not asking about their own past records.",
			Parameters:  params,
		},
	}
}

var memoryHelperToolChoice = map[string]any{"type": "tool", "name": memoryHelperToolName}

// NewMemoryPreflight builds the default fail-silent implicit memory preflight.
func NewMemoryPreflight(q MemoryPreflightQuerier, llm client.LLMClient) agent.MemoryPreflightFunc {
	return func(ctx context.Context, query string, opts agent.MemoryPreflightOptions) *agent.MemoryPreflightResult {
		trace := opts.Trace
		if trace != nil {
			trace.Attempted = true
			trace.ForceHelper = opts.ForceHelper
		}
		if q == nil {
			setMemoryPreflightOutcome(trace, "querier_unavailable")
			return nil
		}
		if q.Status() != memory.StatusReady {
			setMemoryPreflightOutcome(trace, "memory_unavailable")
			return nil
		}
		intents, usage := DetectMemoryIntents(ctx, llm, query, MemoryIntentOptions{ForceHelper: opts.ForceHelper, Trace: trace})
		if len(intents) == 0 {
			if trace != nil && trace.Outcome == "" {
				trace.Outcome = "no_intents"
			}
			if memoryUsageNonZero(usage) {
				return &agent.MemoryPreflightResult{Usage: usage}
			}
			return nil
		}
		if trace != nil {
			trace.IntentsCount = len(intents)
			trace.Queried = true
		}
		queryCtx, cancel := context.WithTimeout(ctx, memoryQueryTimeout)
		defer cancel()
		results := q.QueryBatch(queryCtx, intents)
		if queryCtx.Err() != nil {
			setMemoryPreflightOutcome(trace, "query_timeout")
			return &agent.MemoryPreflightResult{Usage: usage}
		}
		if trace != nil {
			trace.ResultsCount = len(results)
		}
		if len(results) == 0 {
			setMemoryPreflightOutcome(trace, "no_results")
			return &agent.MemoryPreflightResult{Usage: usage}
		}
		rendered := renderPrivateMemoryContext(intents, results)
		if rendered == "" {
			setMemoryPreflightOutcome(trace, "no_context")
			return &agent.MemoryPreflightResult{Usage: usage}
		}
		if trace != nil {
			trace.ContextReturned = true
		}
		setMemoryPreflightOutcome(trace, "context_returned")
		return &agent.MemoryPreflightResult{Context: rendered, Usage: usage}
	}
}

func memoryUsageNonZero(u client.Usage) bool {
	return u.InputTokens != 0 ||
		u.OutputTokens != 0 ||
		u.TotalTokens != 0 ||
		u.CostUSD != 0 ||
		u.CacheReadTokens != 0 ||
		u.CacheCreationTokens != 0 ||
		u.CacheCreation5mTokens != 0 ||
		u.CacheCreation1hTokens != 0
}

// DetectMemoryIntents compiles a user message into QueryIntent values.
// Deterministic relationship patterns bypass the helper; otherwise the helper
// model is invoked as a forced tool_use call (schema-enforced).
func DetectMemoryIntents(ctx context.Context, llm client.LLMClient, query string, opts ...MemoryIntentOptions) ([]memory.QueryIntent, client.Usage) {
	var opt MemoryIntentOptions
	if len(opts) > 0 {
		opt = opts[0]
	}
	trace := opt.Trace
	forceHelper := opt.ForceHelper
	if trace != nil {
		trace.ForceHelper = forceHelper
	}
	if intents := detectExactMemoryIntents(query); len(intents) > 0 {
		if trace != nil {
			trace.IntentSource = "exact"
			trace.IntentsCount = len(intents)
			trace.Outcome = "intents_exact"
		}
		return intents, client.Usage{}
	}
	if isNilLLM(llm) {
		setMemoryPreflightOutcome(trace, "helper_unavailable")
		return nil, client.Usage{}
	}
	helperInput := truncateRunes(query, memoryHelperMaxInputRunes)
	if !canRunMemoryHelper(helperInput) {
		setMemoryPreflightOutcome(trace, "query_not_eligible")
		return nil, client.Usage{}
	}
	if looksLikeTaskText(helperInput) {
		setMemoryPreflightOutcome(trace, "task_text")
		return nil, client.Usage{}
	}
	if !forceHelper && !looksMemoryRelevant(helperInput) {
		setMemoryPreflightOutcome(trace, "gate_declined")
		return nil, client.Usage{}
	}
	if trace != nil {
		trace.HelperUsed = true
		trace.IntentSource = "helper"
	}
	out, usage, err := callMemoryHelper(ctx, llm, helperInput)
	if err != nil {
		setMemoryPreflightHelperError(trace, err)
		return nil, usage
	}
	if !out.ShouldRecall {
		setMemoryPreflightOutcome(trace, "helper_declined")
		return nil, usage
	}
	if len(out.Intents) > 3 {
		out.Intents = out.Intents[:3]
	}
	intents := sanitizeMemoryIntents(out.Intents)
	if trace != nil {
		trace.IntentsCount = len(intents)
	}
	if len(intents) == 0 {
		setMemoryPreflightOutcome(trace, "helper_empty_after_sanitize")
	} else {
		setMemoryPreflightOutcome(trace, "intents_helper")
	}
	return intents, usage
}

func callMemoryHelper(ctx context.Context, llm client.LLMClient, query string) (helperMemoryOutput, client.Usage, error) {
	helperCtx, cancel := context.WithTimeout(ctx, memoryHelperTimeout)
	defer cancel()
	queryEnc, err := json.Marshal(query)
	if err != nil {
		queryEnc = []byte(`""`)
	}
	resp, err := llm.Complete(helperCtx, client.CompletionRequest{
		Messages: []client.Message{
			{Role: "system", Content: client.NewTextContent(memoryHelperSystemPrompt)},
			{Role: "user", Content: client.NewTextContent(string(queryEnc))},
		},
		Tools:       []client.Tool{memoryHelperTool},
		ToolChoice:  memoryHelperToolChoice,
		ModelTier:   "small",
		Temperature: 0,
		MaxTokens:   900,
		CacheSource: "helper",
	})
	if err != nil {
		return helperMemoryOutput{}, client.Usage{}, &helperCallError{kind: classifyMemoryHelperError(helperCtx, resp, err), httpStatus: extractAPIStatus(err), inner: err}
	}
	if resp == nil {
		return helperMemoryOutput{}, client.Usage{}, &helperCallError{kind: "nil_response"}
	}
	calls := resp.AllToolCalls()
	if len(calls) == 0 {
		return helperMemoryOutput{}, resp.Usage, &helperCallError{kind: "no_tool_call"}
	}
	if calls[0].Name != "" && calls[0].Name != memoryHelperToolName {
		return helperMemoryOutput{}, resp.Usage, &helperCallError{kind: "wrong_tool"}
	}
	var out helperMemoryOutput
	if err := json.Unmarshal(calls[0].Arguments, &out); err != nil {
		return helperMemoryOutput{}, resp.Usage, &helperCallError{kind: "invalid_tool_args", inner: err}
	}
	return out, resp.Usage, nil
}

// helperCallError carries a stable kind tag for trace classification without
// leaking error message content (which may contain raw provider response bodies).
type helperCallError struct {
	kind       string
	httpStatus int
	inner      error
}

func (e *helperCallError) Error() string {
	if e == nil {
		return ""
	}
	if e.inner != nil {
		return fmt.Sprintf("memory helper %s: %v", e.kind, e.inner)
	}
	return fmt.Sprintf("memory helper %s", e.kind)
}

func (e *helperCallError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.inner
}

func setMemoryPreflightOutcome(trace *agent.MemoryPreflightTrace, outcome string) {
	if trace != nil {
		trace.Outcome = outcome
	}
}

func setMemoryPreflightHelperError(trace *agent.MemoryPreflightTrace, err error) {
	if trace == nil {
		return
	}
	trace.Outcome = "helper_error"
	var he *helperCallError
	if errors.As(err, &he) {
		trace.ErrorClass = he.kind
		trace.HTTPStatus = he.httpStatus
		return
	}
	// Generic fallback: shouldn't happen since callMemoryHelper always wraps.
	trace.ErrorClass = "unknown"
}

func classifyMemoryHelperError(ctx context.Context, resp *client.CompletionResponse, err error) string {
	if ctx != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "timeout"
		}
		if errors.Is(ctx.Err(), context.Canceled) {
			return "canceled"
		}
	}
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return "timeout"
		}
		if errors.Is(err, context.Canceled) {
			return "canceled"
		}
		var apiErr *client.APIError
		if errors.As(err, &apiErr) {
			switch apiErr.StatusCode {
			case 400:
				return "bad_request"
			case 401, 403:
				return "auth"
			case 408, 504:
				return "timeout"
			case 429:
				return "rate_limited"
			case 500, 502, 503, 529:
				return "provider_server"
			default:
				return "http_status"
			}
		}
		var netErr net.Error
		if errors.As(err, &netErr) {
			if netErr.Timeout() {
				return "timeout"
			}
			return "transport"
		}
		return "transport"
	}
	if resp == nil {
		return "nil_response"
	}
	return "unknown"
}

func extractAPIStatus(err error) int {
	var apiErr *client.APIError
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode
	}
	return 0
}

func detectExactMemoryIntents(query string) []memory.QueryIntent {
	q := strings.TrimSpace(query)
	if q == "" {
		return nil
	}
	for _, p := range exactRelationshipPatterns {
		if m := p.re.FindStringSubmatch(q); len(m) > p.anchorGroup {
			if anchor := cleanMemoryAnchor(m[p.anchorGroup]); anchor != "" {
				return []memory.QueryIntent{defaultDirectMemoryIntent(anchor)}
			}
		}
	}
	return nil
}

func defaultDirectMemoryIntent(anchor string) memory.QueryIntent {
	return memory.QueryIntent{
		Mode:           memory.ModeDirectRelation,
		AnchorMentions: []string{anchor},
		EvidenceBudget: 5,
		ResultLimit:    10,
	}
}

func cleanMemoryAnchor(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Trim(s, " \t\r\n\"'“”‘’`.,，。?？!！:：;；()（）[]【】")
	s = strings.TrimPrefix(s, "the ")
	s = strings.TrimPrefix(s, "The ")
	for _, prefix := range []string{"关于", "和", "与", "跟", "to ", "with "} {
		s = strings.TrimSpace(strings.TrimPrefix(s, prefix))
	}
	if s == "" || isPronounAnchor(s) || looksLikeTaskText(s) {
		return ""
	}
	return s
}

func isPronounAnchor(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "i", "me", "my", "mine", "myself", "user", "the user", "我", "我的", "自己", "本人", "私", "わたし", "僕", "俺":
		return true
	default:
		return false
	}
}

// looksLikeTaskText is a hard skip for the helper: any prompt that names
// build/edit/debug verbs or carries code-shaped surface is not a memory query.
func looksLikeTaskText(s string) bool {
	lower := strings.ToLower(s)
	if strings.Contains(lower, "http://") || strings.Contains(lower, "https://") || strings.Contains(lower, "\n") || strings.Contains(lower, "```") {
		return true
	}
	for _, marker := range []string{
		" fix ", " implement ", " debug ", " code ", " file ", " error ", " stack trace ",
		"修复", "修改", "实现", "代码", "文件", "錯誤", "错误", "测试", "構建", "构建",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// canRunMemoryHelper checks the input is non-empty and within the helper's
// size budget. Callers should truncate to memoryHelperMaxInputRunes BEFORE
// this check (see truncateRunes); the bound here is the floor on what we
// actually send to the model.
func canRunMemoryHelper(query string) bool {
	q := strings.TrimSpace(query)
	return q != "" && len([]rune(q)) <= memoryHelperMaxInputRunes
}

func truncateRunes(s string, max int) string {
	if max <= 0 {
		return s
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max])
}

// looksMemoryRelevant is the cheap gate for non-forced calls. With tool_use
// caching, the helper round-trip is cheap, so this is mostly a filter to skip
// helper on obvious non-memory prompts (the helper itself returns should_recall=false
// on most ambiguous cases, so over-firing has a small cost).
//
// True when the query carries any memory/relation/typed-target cue OR an
// entity-like surface form, AND is not dominated by public-current-fact cues.
func looksMemoryRelevant(query string) bool {
	lower := strings.ToLower(query)
	hasMemoryCue := containsAny(lower, privateMemoryCues)
	hasRelationCue := containsAny(lower, relationQuestionCues)
	hasTypedTarget := containsAny(lower, typedTargetCues)
	hasEntity := hasEntityishSurface(query)
	if !(hasMemoryCue || hasRelationCue || hasTypedTarget || hasEntity) {
		return false
	}
	if containsAny(lower, publicCurrentFactCues) && !hasMemoryCue {
		return false
	}
	return true
}

var privateMemoryCues = []string{
	"remember", "recall", "last time", "previously", "before", "we discussed", "we decided", "did we", "have we", "my ", " me ", " i ",
	"记得", "記得", "回忆", "回憶", "上次", "之前", "以前", "我们聊", "我們聊", "我们说", "我們說", "我", "我的",
	"覚えて", "思い出", "前回", "以前", "前に", "話した", "決めた", "私", "わたし", "僕", "俺", "自分",
}

var relationQuestionCues = []string{
	"relationship", "connection", "know", "met", "meet", "worked", "work with", "colleague", "coworker", "classmate", "advisor", "mentor", "friend",
	"created", "built", "authored", "founded", "owns", "owned", "depends", "requires", "uses", "implemented", "runs on", "integrates", "supports", "released", "published", "forked", "inspired", "customer", "competitor", "email", "handle", "url", "path", "scheduled", "monitors", "what does", "who created", "who owns", "who built",
	"关系", "關係", "认识", "認識", "见过", "見過", "合作", "同事", "同学", "同學", "导师", "導師", "朋友", "是谁", "是誰", "工作", "任职", "任職", "创建", "創建", "作者", "拥有", "擁有", "依赖", "依賴", "使用", "实现", "實現", "运行", "運行", "集成", "支持", "发布", "發布", "项目", "項目",
	"関係", "知", "会った", "仕事", "同僚", "友達", "先生", "メンター", "作った", "作者", "所有", "依存", "使", "実装", "動", "統合", "対応", "発表", "公開", "プロジェクト",
}

var typedTargetCues = []string{
	"which people", "which person", "which company", "which companies", "which project", "which projects", "which tool", "which tools", "which language", "which languages", "what projects", "what tools", "who are",
	"哪些人", "哪个公司", "哪些公司", "哪些项目", "哪些項目", "哪些工具", "什么项目", "什麼項目",
	"どの人", "どの会社", "どのプロジェクト", "どのツール", "どの言語", "誰が",
}

var publicCurrentFactCues = []string{
	"latest", "current", "today's", "news", "stock price", "weather", "president", "ceo of", "exchange rate", "schedule for",
	"最新", "新闻", "新聞", "股价", "股價", "天气", "天氣", "总统", "總統", "汇率", "匯率",
	"最新", "ニュース", "株価", "天気", "大統領", "為替",
}

func isNilLLM(llm client.LLMClient) bool {
	if llm == nil {
		return true
	}
	v := reflect.ValueOf(llm)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return v.IsNil()
	default:
		return false
	}
}

func containsAny(s string, markers []string) bool {
	for _, marker := range markers {
		if strings.Contains(s, marker) {
			return true
		}
	}
	return false
}

func hasEntityishSurface(s string) bool {
	if latinEntityPattern.FindStringSubmatch(s) != nil {
		return true
	}
	if strings.ContainsAny(s, "\"'“”‘’`「」『』") {
		return true
	}
	hasCJKOrKana := false
	for _, r := range s {
		if unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana) {
			hasCJKOrKana = true
			break
		}
	}
	return hasCJKOrKana && len([]rune(s)) <= 80
}

func memoryRelationCatalog() string {
	var sb strings.Builder
	for _, group := range compactMemoryRelationCatalog {
		fmt.Fprintf(&sb, "- %s: %s\n", group.Name, strings.Join(group.Relations, ", "))
	}
	return sb.String()
}

func buildKnownMemoryRelations() map[string]bool {
	known := make(map[string]bool)
	for _, group := range compactMemoryRelationCatalog {
		for _, rel := range group.Relations {
			known[rel] = true
		}
	}
	return known
}

func sanitizeMemoryIntents(intents []memory.QueryIntent) []memory.QueryIntent {
	out := make([]memory.QueryIntent, 0, len(intents))
	seen := make(map[string]bool, len(intents))
	for _, intent := range intents {
		intent.AnchorMentions = cleanAnchorList(intent.AnchorMentions)
		if len(intent.AnchorMentions) == 0 {
			continue
		}
		if intent.Mode == "" {
			intent.Mode = memory.ModeDirectRelation
		}
		if intent.EvidenceBudget <= 0 || intent.EvidenceBudget > 50 {
			intent.EvidenceBudget = 5
		}
		if intent.ResultLimit <= 0 || intent.ResultLimit > 100 {
			intent.ResultLimit = 10
		}
		intent.RelationConstraints = cleanRelationConstraints(intent.RelationConstraints)
		switch intent.Mode {
		case memory.ModeDirectRelation:
		case memory.ModePathQuery:
			if len(intent.RelationConstraints) < 2 || len(intent.RelationConstraints) > 4 {
				continue
			}
			intent.TargetSlot = "tail"
		case memory.ModeTypedNeighborhood:
			if intent.CandidateType == nil || strings.TrimSpace(*intent.CandidateType) == "" || len(intent.RelationConstraints) != 1 {
				continue
			}
			intent.TargetSlot = "tail"
		default:
			continue
		}
		keyBytes, _ := json.Marshal(intent)
		key := string(keyBytes)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, intent)
		if len(out) >= 3 {
			break
		}
	}
	return out
}

func cleanAnchorList(in []string) []string {
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, raw := range in {
		anchor := cleanMemoryAnchor(raw)
		if anchor == "" {
			continue
		}
		key := strings.ToLower(anchor)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, anchor)
	}
	return out
}

func cleanRelationConstraints(in []string) []string {
	out := make([]string, 0, len(in))
	for _, raw := range in {
		rel := strings.TrimSpace(raw)
		if rel == "" || isBroadMemoryRelation(rel) || !isSnakeRelation(rel) || !isKnownMemoryRelation(rel) {
			continue
		}
		out = append(out, rel)
	}
	return out
}

func isKnownMemoryRelation(rel string) bool {
	base := strings.TrimPrefix(strings.TrimSuffix(rel, "^-1"), "^")
	return knownMemoryRelations[base]
}

func isSnakeRelation(rel string) bool {
	rel = strings.TrimPrefix(rel, "^")
	rel = strings.TrimSuffix(rel, "^-1")
	if rel == "" {
		return false
	}
	for _, r := range rel {
		if r == '_' || unicode.IsLower(r) || unicode.IsDigit(r) {
			continue
		}
		return false
	}
	return true
}

func renderPrivateMemoryContext(intents []memory.QueryIntent, results []memory.QueryResult) string {
	// Body is built without the <private_memory> envelope, then sanitized,
	// then wrapped. All non-literal fields below are user-derived (helper
	// LLM output or memory-store content) — running the body through
	// prompt.SanitizeUserBlock strips any stray `</private_memory>` /
	// `</user_instructions>` / `</system-reminder>` closers so the envelope
	// we add cannot be terminated early. Same defense pattern as
	// instructions.md (issue #125).
	var body strings.Builder
	for i, result := range results {
		if result.Err != nil || result.Class != memory.ClassOK || result.Envelope == nil {
			continue
		}
		env := result.Envelope
		if env.MemoryBlock == nil || len(env.MemoryBlock.Groups) == 0 {
			continue
		}
		intent := memory.QueryIntent{}
		if i < len(intents) {
			intent = intents[i]
		}
		body.WriteString("\n")
		fmt.Fprintf(&body, "Query: mode=%s anchors=%s", intent.Mode, strings.Join(intent.AnchorMentions, ", "))
		if len(intent.RelationConstraints) > 0 {
			fmt.Fprintf(&body, " relations=%s", strings.Join(intent.RelationConstraints, " -> "))
		}
		body.WriteString("\n")
		for _, g := range env.MemoryBlock.Groups {
			fmt.Fprintf(&body, "- %s", g.Value)
			if len(g.ViaRelations) > 0 {
				fmt.Fprintf(&body, " via %s", strings.Join(g.ViaRelations, ", "))
			}
			if len(g.ObservedPath) > 0 {
				body.WriteString(" via ")
				body.WriteString(renderObservedPath(g.ObservedPath))
			}
			if g.SupportCount > 0 {
				fmt.Fprintf(&body, " (support=%d)", g.SupportCount)
			}
			body.WriteString("\n")
		}
		for _, note := range env.MemoryBlock.Notes {
			note = strings.TrimSpace(note)
			if note != "" {
				fmt.Fprintf(&body, "Note: %s\n", note)
			}
		}
	}
	if body.Len() == 0 {
		return ""
	}
	bodyStr := truncatePrivateMemoryBody(body.String(), privateMemoryBodyByteCap)
	var out strings.Builder
	out.WriteString("<private_memory>\n")
	out.WriteString("Past private records matched the user's message. Use only when directly relevant; prefer these personal facts over training knowledge. Do not mention raw provenance unless asked. Phrase findings per the system prompt's `## Private Memory > Internal vocabulary` rule.\n")
	out.WriteString(prompt.SanitizeUserBlock(bodyStr))
	out.WriteString("</private_memory>")
	return out.String()
}

// truncatePrivateMemoryBody enforces a byte cap on the body inside the
// <private_memory> envelope. Truncates at the last newline before the cap
// so the visual break is clean; falls back to a UTF-8 rune boundary if no
// newline exists within the limit. Returns the input unchanged if under the
// cap. Appends a single-line marker so downstream readers (and the main
// model) can see that truncation happened.
func truncatePrivateMemoryBody(body string, cap int) string {
	if len(body) <= cap {
		return body
	}
	cut := cap
	if idx := strings.LastIndexByte(body[:cut], '\n'); idx >= 0 {
		cut = idx
	} else {
		// No newline in the leading window — back up to a rune boundary
		// so we don't slice mid-multibyte. (Realistically unreachable;
		// every group/note line ends with '\n'.)
		for cut > 0 && !utf8.RuneStart(body[cut]) {
			cut--
		}
	}
	return body[:cut] + fmt.Sprintf("\n…(truncated: private memory exceeded %d-byte cap)\n", cap)
}

func renderObservedPath(path []memory.HopRecord) string {
	parts := make([]string, 0, len(path))
	for _, h := range path {
		arrow := "->"
		if h.Direction == "inverse" {
			arrow = "<-"
		}
		parts = append(parts, fmt.Sprintf("%s -[%s]%s %s", h.FromLabel, h.Relation, arrow, h.ToLabel))
	}
	return strings.Join(parts, "; ")
}
