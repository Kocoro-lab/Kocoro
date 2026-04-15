package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

// TimePtr returns a pointer to t, for use in MessageMeta literals.
func TimePtr(t time.Time) *time.Time { return &t }

// MessageMeta holds per-message metadata not sent to the LLM gateway.
// Indexed parallel to Session.Messages.
type MessageMeta struct {
	Source         string     `json:"source,omitempty"`          // "local", "slack", "line", "shanclaw", "webhook", "scheduler"
	MessageID      string     `json:"message_id,omitempty"`      // stable ID for dedup (e.g. "msg-<uuid>")
	Timestamp      *time.Time `json:"timestamp,omitempty"`       // when this message was sent/received; nil = legacy (pre-timestamp)
	SystemInjected bool       `json:"system_injected,omitempty"` // true for guardrail/nudge messages injected by the agent loop
}

type Session struct {
	SchemaVersion   int              `json:"schema_version,omitempty"`
	ID              string           `json:"id"`
	CreatedAt       time.Time        `json:"created_at"`
	UpdatedAt       time.Time        `json:"updated_at"`
	Title           string           `json:"title"`
	CWD             string           `json:"cwd"`
	Messages        []client.Message `json:"messages"`
	RemoteTasks     []string         `json:"remote_tasks,omitempty"`
	MessageMeta     []MessageMeta    `json:"message_meta,omitempty"`
	Source          string           `json:"source,omitempty"`            // "slack", "line", "shanclaw", "webhook"
	Channel         string           `json:"channel,omitempty"`           // source channel/group identifier
	SummaryCache    string           `json:"summary_cache,omitempty"`     // cached summary Markdown
	SummaryCacheKey string           `json:"summary_cache_key,omitempty"` // invalidation key for cached summary
	Usage           *UsageSummary    `json:"usage,omitempty"`             // cumulative LLM + tool cost/token totals
	// InProgress is true between a mid-turn checkpoint save and the final
	// post-turn save. If a session is loaded with this set, the previous
	// run crashed or was killed mid-turn — the transcript is partial but
	// recoverable; tool results already executed are preserved.
	InProgress bool `json:"in_progress,omitempty"`
}

// UsageSummary captures cumulative LLM and gateway-tool costs across a session.
// LLM fields come from agent.TurnUsage (input/output tokens, cache tokens, cost).
// Tool fields come from gateway tools that report usage (e.g. x_search→xAI Grok,
// web_search→SerpAPI). Fields are additive across turns; zero-valued fields are
// omitted from JSON for smaller session files.
type UsageSummary struct {
	LLMCalls            int     `json:"llm_calls,omitempty"`
	InputTokens         int     `json:"input_tokens,omitempty"`
	OutputTokens        int     `json:"output_tokens,omitempty"`
	TotalTokens         int     `json:"total_tokens,omitempty"`
	CostUSD             float64 `json:"cost_usd,omitempty"`
	CacheReadTokens     int     `json:"cache_read_tokens,omitempty"`
	CacheCreationTokens int     `json:"cache_creation_tokens,omitempty"`
	Model               string  `json:"model,omitempty"` // last-seen model
	// Gateway tool costs (populated once Shannon Cloud returns usage per tool call).
	ToolCalls   int     `json:"tool_calls,omitempty"`
	ToolCostUSD float64 `json:"tool_cost_usd,omitempty"`
}

// UsageFromTurn converts LLM-only numeric values into a UsageSummary.
// Left in place for callers that only have LLM data; new code should prefer
// UsageFromAccumulated which carries both LLM and gateway-tool costs.
func UsageFromTurn(llmCalls, inputTokens, outputTokens, totalTokens int, costUSD float64, cacheRead, cacheCreation int, model string) UsageSummary {
	return UsageSummary{
		LLMCalls:            llmCalls,
		InputTokens:         inputTokens,
		OutputTokens:        outputTokens,
		TotalTokens:         totalTokens,
		CostUSD:             costUSD,
		CacheReadTokens:     cacheRead,
		CacheCreationTokens: cacheCreation,
		Model:               model,
	}
}

// UsageFromAccumulated builds a UsageSummary carrying both LLM and gateway
// tool costs as separate fields so totals stay unambiguous when a run
// touched billed tools (x_search, web_search).
func UsageFromAccumulated(
	llmCalls, inputTokens, outputTokens, totalTokens int, costUSD float64,
	cacheRead, cacheCreation int, model string,
	toolCalls int, toolCostUSD float64,
) UsageSummary {
	return UsageSummary{
		LLMCalls:            llmCalls,
		InputTokens:         inputTokens,
		OutputTokens:        outputTokens,
		TotalTokens:         totalTokens,
		CostUSD:             costUSD,
		CacheReadTokens:     cacheRead,
		CacheCreationTokens: cacheCreation,
		Model:               model,
		ToolCalls:           toolCalls,
		ToolCostUSD:         toolCostUSD,
	}
}

// Add accumulates another UsageSummary into u.
func (u *UsageSummary) Add(o UsageSummary) {
	u.LLMCalls += o.LLMCalls
	u.InputTokens += o.InputTokens
	u.OutputTokens += o.OutputTokens
	u.TotalTokens += o.TotalTokens
	u.CostUSD += o.CostUSD
	u.CacheReadTokens += o.CacheReadTokens
	u.CacheCreationTokens += o.CacheCreationTokens
	u.ToolCalls += o.ToolCalls
	u.ToolCostUSD += o.ToolCostUSD
	if o.Model != "" {
		u.Model = o.Model
	}
}

// SourceAt returns the source for message at index i, or "unknown" if not available.
func (s *Session) SourceAt(i int) string {
	if i >= 0 && i < len(s.MessageMeta) && s.MessageMeta[i].Source != "" {
		return s.MessageMeta[i].Source
	}
	return "unknown"
}

// HistoryForLoop returns the message history to feed into a fresh agent
// loop Run(), with loop-internal guardrail/nudge messages filtered out.
//
// Injected messages (MessageMeta.SystemInjected == true) are transient
// single-turn corrections — e.g. the hallucination guardrail "STOP. You
// wrote out tool calls as text…". Resurrecting them in a future run's
// context is both (a) confusing to the model, since the correction no
// longer applies, and (b) a security leak: tools that read the live
// conversation snapshot (schedule_create, session_search helpers, etc.)
// would otherwise persist them as if they were real user input.
//
// When the meta slice is missing or shorter than Messages (legacy sessions
// predating the flag), unannotated messages are returned unchanged.
func (s *Session) HistoryForLoop() []client.Message {
	return FilterInjected(s.Messages, s.MessageMeta)
}

// FilterInjected returns msgs with any positions flagged SystemInjected in
// the parallel meta slice removed. If meta is empty or shorter than msgs,
// unannotated positions are kept. Used by call sites that already have
// sliced views of session history (e.g. TUI: everything-except-last).
//
// The return value aliases the input slice on the fast path (nothing
// flagged) but is capped to its current length, so a caller that later
// appends to the result cannot silently mutate the input's backing array
// past its visible length.
func FilterInjected(msgs []client.Message, meta []MessageMeta) []client.Message {
	if len(meta) == 0 {
		// Cap capacity so an append on the result allocates fresh storage
		// instead of extending into the caller's backing array.
		return msgs[:len(msgs):len(msgs)]
	}
	// Fast path: nothing flagged → alias the original slice (with capped
	// capacity, as above).
	anyInjected := false
	for i := 0; i < len(msgs) && i < len(meta); i++ {
		if meta[i].SystemInjected {
			anyInjected = true
			break
		}
	}
	if !anyInjected {
		return msgs[:len(msgs):len(msgs)]
	}
	out := make([]client.Message, 0, len(msgs))
	for i, msg := range msgs {
		if i < len(meta) && meta[i].SystemInjected {
			continue
		}
		out = append(out, msg)
	}
	return out
}

type SessionSummary struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	CreatedAt time.Time `json:"created_at"`
	MsgCount  int       `json:"msg_count"`
}

type Store struct {
	dir   string
	index *Index // nil = index unavailable (graceful degradation)
}

func NewStore(dir string) *Store {
	os.MkdirAll(dir, 0700)
	s := &Store{dir: dir}
	idx, err := OpenIndex(dir)
	if err == nil {
		s.index = idx
		// First-launch migration OR tokenizer-version migration: if the index
		// is empty (fresh install), or OpenIndex detected a version mismatch
		// and dropped the stale FTS tables, re-seed from the JSON files.
		empty, _ := idx.IsEmpty()
		if empty || idx.NeedsRebuild() {
			idx.Rebuild(s) // best-effort
		}
	}
	return s
}

func (s *Store) Save(sess *Session) error {
	sess.UpdatedAt = time.Now()
	if sess.CreatedAt.IsZero() {
		sess.CreatedAt = sess.UpdatedAt
	}
	if sess.SchemaVersion == 0 {
		sess.SchemaVersion = 1
	}

	data, err := json.MarshalIndent(sess, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal session: %w", err)
	}

	path := filepath.Join(s.dir, sess.ID+".json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		return err
	}

	if s.index != nil {
		s.index.UpsertSession(sess) // best-effort, don't fail save on index error
	}
	return nil
}

// PatchTitle re-reads the session from disk, updates only the title, and writes it back.
// UpdatedAt is not touched, so session sort order is preserved.
func (s *Store) PatchTitle(id, title string) error {
	sess, err := s.Load(id)
	if err != nil {
		return err
	}
	sess.Title = title

	data, err := json.MarshalIndent(sess, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal session: %w", err)
	}

	path := filepath.Join(s.dir, sess.ID+".json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		return err
	}

	if s.index != nil {
		s.index.UpsertSession(sess)
	}
	return nil
}

// PatchSummaryCache 从磁盘重新读取 session 的最新版本，仅更新摘要缓存字段后写回。
// 避免覆盖在初次 Load 和写入之间被 agent loop 追加的新消息。
// 不更新 UpdatedAt，不影响 session 排序。
func (s *Store) PatchSummaryCache(id, summary, cacheKey string) error {
	sess, err := s.Load(id)
	if err != nil {
		return err
	}
	sess.SummaryCache = summary
	sess.SummaryCacheKey = cacheKey

	data, err := json.MarshalIndent(sess, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal session: %w", err)
	}

	path := filepath.Join(s.dir, sess.ID+".json")
	return os.WriteFile(path, data, 0600)
}

func (s *Store) Load(id string) (*Session, error) {
	path := filepath.Join(s.dir, id+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read session: %w", err)
	}

	var sess Session
	if err := json.Unmarshal(data, &sess); err != nil {
		return nil, fmt.Errorf("parse session: %w", err)
	}
	if sess.SchemaVersion == 0 {
		sess.SchemaVersion = 1
	}
	return &sess, nil
}

func (s *Store) List() ([]SessionSummary, error) {
	if s.index != nil {
		if summaries, err := s.index.ListSessions(); err == nil {
			return summaries, nil
		}
		// Fall through to JSON scan on index error
	}

	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}

	var summaries []SessionSummary
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".json")
		sess, err := s.Load(id)
		if err != nil {
			continue
		}
		summaries = append(summaries, SessionSummary{
			ID:        sess.ID,
			Title:     sess.Title,
			CreatedAt: sess.CreatedAt,
			MsgCount:  len(sess.Messages),
		})
	}

	sort.Slice(summaries, func(i, j int) bool {
		return summaries[i].CreatedAt.After(summaries[j].CreatedAt)
	})
	return summaries, nil
}

func (s *Store) Delete(id string) error {
	path := filepath.Join(s.dir, id+".json")
	if err := os.Remove(path); err != nil {
		return err
	}

	if s.index != nil {
		s.index.DeleteSession(id) // best-effort
	}
	return nil
}

func (s *Store) Search(query string, limit int) ([]SearchResult, error) {
	if s.index == nil {
		return nil, fmt.Errorf("search index not available")
	}
	return s.index.Search(query, limit)
}

func (s *Store) Close() error {
	if s.index != nil {
		return s.index.Close()
	}
	return nil
}

func (s *Store) RebuildIndex() error {
	if s.index == nil {
		return fmt.Errorf("search index not available")
	}
	return s.index.Rebuild(s)
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
