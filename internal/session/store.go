package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
	ctxwin "github.com/Kocoro-lab/ShanClaw/internal/context"
)

// TimePtr returns a pointer to t, for use in MessageMeta literals.
func TimePtr(t time.Time) *time.Time { return &t }

// MessageMeta holds per-message metadata not sent to the LLM gateway.
// Indexed parallel to Session.Messages.
type MessageMeta struct {
	Source         string     `json:"source,omitempty"`          // "local", "slack", "line", "kocoro", "webhook", "scheduler" (legacy "shanclaw" still appears in older sessions)
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
	Source          string           `json:"source,omitempty"`            // "slack", "line", "kocoro", "webhook" (legacy "shanclaw" still appears in older sessions)
	Channel         string           `json:"channel,omitempty"`           // source channel/group identifier
	ScheduleID      string           `json:"schedule_id,omitempty"`       // owning schedule for scheduler-created sessions; retained after schedule deletion
	RouteKey        string           `json:"route_key,omitempty"`         // persisted daemon route binding for routed conversations
	SummaryCache    string           `json:"summary_cache,omitempty"`     // cached summary Markdown
	SummaryCacheKey string           `json:"summary_cache_key,omitempty"` // invalidation key for cached summary
	Usage           *UsageSummary    `json:"usage,omitempty"`             // cumulative LLM + tool cost/token totals
	// ToolResultReplacements stores query-time tool_result replacement text
	// keyed by tool_use_id. It is not model-visible by itself; agent loops
	// apply it to a request-local message copy before LLM calls.
	ToolResultReplacements map[string]string `json:"tool_result_replacements,omitempty"`
	// ToolResultSeen stores tool_use_ids that have already passed through
	// query-time budgeting, even if they were not replaced. This freezes their
	// fate across turns and prevents old history from drifting later.
	ToolResultSeen map[string]bool `json:"tool_result_seen,omitempty"`
	// InProgress is true between a mid-turn checkpoint save and the final
	// post-turn save. If a session is loaded with this set, the previous
	// run crashed or was killed mid-turn — the transcript is partial but
	// recoverable; tool results already executed are preserved.
	InProgress bool `json:"in_progress,omitempty"`
	// Pinned sticks the session to the top of the list regardless of
	// recency. Set/cleared via PATCH /sessions/{id} {"pinned": bool}.
	Pinned bool `json:"pinned,omitempty"`
	// Favorite marks the session as starred for filter views. Independent
	// of Pinned. Set/cleared via PATCH /sessions/{id} {"favorite": bool}.
	Favorite bool `json:"favorite,omitempty"`
	// TitleAuto reports whether Title was machine-derived (first-line
	// placeholder or LLM upgrade) and may still be overwritten by a later
	// upgrade. A user rename via PATCH /sessions/{id} sets it false, locking it.
	TitleAuto bool `json:"title_auto,omitempty"`
	// TitleTurns is the assistant-turn count at which the current auto Title
	// was generated. A later upgrade triggered at a LOWER turn count is a
	// reordered straggler and is skipped.
	TitleTurns int `json:"title_turns,omitempty"`
	// PublishedShares is the daemon-side source-of-truth for upload_ids
	// returned by POST /sessions/{id}/share. The UI is the primary keeper of
	// these IDs (it sends them back on DELETE /sessions/{id}/share), but UI
	// state can drift — most visibly when a named agent's session ends up
	// retract-broken because the UI dropped its upload_id. Storing the list
	// here gives the UI a fallback to read via GET /sessions/{id}/shares,
	// and gives us an audit trail that survives UI restarts.
	//
	// Append-only on share; on successful retract the matching UploadID is
	// filtered out. Writes go through Store.PatchPublishedShares so they do
	// NOT bump UpdatedAt (share/retract is metadata, not activity, and a
	// bump would re-sort the session to the top of the recency list).
	PublishedShares []PublishedShareEntry `json:"published_shares,omitempty"`
}

// PublishedShareEntry is one entry in Session.PublishedShares — a successful
// share-page upload that has not yet been retracted. URL is the public CDN
// link; UploadID is the UUID needed to retract via DELETE /api/v1/uploads/{id}.
type PublishedShareEntry struct {
	UploadID  string    `json:"upload_id"`
	URL       string    `json:"url"`
	Filename  string    `json:"filename,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// LastSeenModel returns the model that served the most recent LLM call on
// this session, or "" when the session has no prior usage. Used by
// AgentLoop callers to seed the soft context window when the daemon (or
// any other caller) builds a fresh loop per request and the auto-detect
// from a prior turn would otherwise be lost.
func (s *Session) LastSeenModel() string {
	if s == nil || s.Usage == nil {
		return ""
	}
	return s.Usage.Model
}

// UsageSummary captures cumulative LLM and gateway-tool costs across a session.
// LLM fields come from agent.TurnUsage (input/output tokens, cache tokens, cost).
// Tool fields come from gateway tools that report usage (e.g. x_search→xAI Grok,
// web_search→SerpAPI). Fields are additive across turns; zero-valued fields are
// omitted from JSON for smaller session files.
type UsageSummary struct {
	LLMCalls              int     `json:"llm_calls,omitempty"`
	InputTokens           int     `json:"input_tokens,omitempty"`
	OutputTokens          int     `json:"output_tokens,omitempty"`
	TotalTokens           int     `json:"total_tokens,omitempty"`
	CostUSD               float64 `json:"cost_usd,omitempty"`
	CacheReadTokens       int     `json:"cache_read_tokens,omitempty"`
	CacheCreationTokens   int     `json:"cache_creation_tokens,omitempty"`
	CacheCreation5mTokens int     `json:"cache_creation_5m_tokens,omitempty"`
	CacheCreation1hTokens int     `json:"cache_creation_1h_tokens,omitempty"`
	Model                 string  `json:"model,omitempty"` // last-seen model
	// Gateway tool costs (populated once Shannon Cloud returns usage per tool call).
	ToolCalls   int     `json:"tool_calls,omitempty"`
	ToolCostUSD float64 `json:"tool_cost_usd,omitempty"`
}

// UsageFromTurn converts LLM-only numeric values into a UsageSummary.
// Left in place for callers that only have LLM data; new code should prefer
// UsageFromAccumulated which carries both LLM and gateway-tool costs.
func UsageFromTurn(llmCalls, inputTokens, outputTokens, totalTokens int, costUSD float64, cacheRead, cacheCreation, cacheCreation5m, cacheCreation1h int, model string) UsageSummary {
	return UsageSummary{
		LLMCalls:              llmCalls,
		InputTokens:           inputTokens,
		OutputTokens:          outputTokens,
		TotalTokens:           totalTokens,
		CostUSD:               costUSD,
		CacheReadTokens:       cacheRead,
		CacheCreationTokens:   cacheCreation,
		CacheCreation5mTokens: cacheCreation5m,
		CacheCreation1hTokens: cacheCreation1h,
		Model:                 model,
	}
}

// UsageFromAccumulated builds a UsageSummary carrying both LLM and gateway
// tool costs as separate fields so totals stay unambiguous when a run
// touched billed tools (x_search, web_search).
func UsageFromAccumulated(
	llmCalls, inputTokens, outputTokens, totalTokens int, costUSD float64,
	cacheRead, cacheCreation, cacheCreation5m, cacheCreation1h int, model string,
	toolCalls int, toolCostUSD float64,
) UsageSummary {
	return UsageSummary{
		LLMCalls:              llmCalls,
		InputTokens:           inputTokens,
		OutputTokens:          outputTokens,
		TotalTokens:           totalTokens,
		CostUSD:               costUSD,
		CacheReadTokens:       cacheRead,
		CacheCreationTokens:   cacheCreation,
		CacheCreation5mTokens: cacheCreation5m,
		CacheCreation1hTokens: cacheCreation1h,
		Model:                 model,
		ToolCalls:             toolCalls,
		ToolCostUSD:           toolCostUSD,
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
	u.CacheCreation5mTokens += o.CacheCreation5mTokens
	u.CacheCreation1hTokens += o.CacheCreation1hTokens
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
	ID    string `json:"id"`
	Title string `json:"title"`
	// CWD is the immutable working directory captured for the session. It is
	// always emitted (empty for legacy/unlinked sessions) so Desktop can derive
	// project groups without issuing one GET /sessions/{id} request per row.
	CWD       string    `json:"cwd"`
	CreatedAt time.Time `json:"created_at"`
	// UpdatedAt is the timestamp of the most recent activity in the session
	// (mirrors Session.UpdatedAt). Drives list ordering in GET /sessions so
	// the most recently used session surfaces first, regardless of creation
	// date. Title / pinned / favorite edits intentionally do NOT bump this
	// (see Store.PatchTitle / Store.PatchFlags, which write JSON directly
	// and bypass Save / nextUpdatedAt) so metadata changes preserve order.
	UpdatedAt time.Time `json:"updated_at"`
	MsgCount  int       `json:"msg_count"`
	// Source identifies the originating IM / surface for this session.
	// Canonical values are the `Channel*` constants in
	// `internal/daemon/types.go` (slack/line/teams/wechat/wecom/web/feishu/
	// lark/discord/telegram/schedule/system/webhook) plus "kocoro" (set by
	// POST /messages when the inbound request omits a source — i.e. the
	// Desktop / TUI path). Empty for legacy sessions written before the
	// column existed. Frontends use this to pick a channel icon / filter
	// the sidebar.
	Source string `json:"source,omitempty"`
	// ScheduleID identifies the exact scheduled task that created this
	// session. It remains on the session after the schedule configuration is
	// deleted, so deleting a schedule never destroys or rewrites history.
	ScheduleID string `json:"schedule_id,omitempty"`
	// InProgress reports whether the daemon currently owns an in-flight
	// agent run for this session (mirrors SessionCache.ActiveSessionIDs).
	// Populated at HTTP-list time by the daemon — Store.List itself leaves
	// this false because store has no view into runtime state.
	InProgress bool `json:"in_progress,omitempty"`
	// AwaitingApproval reports whether the agent loop is currently blocked
	// waiting for the user to approve a tool call. Populated at HTTP-list
	// time from ApprovalTracker; Store.List leaves it false.
	AwaitingApproval bool `json:"awaiting_approval,omitempty"`
	// Kind classifies the session by origin (interactive / im / schedule),
	// derived from Source via the daemon's exclusion rule (see
	// internal/daemon.kindOf). Populated at HTTP-list time by the daemon —
	// Store.List leaves it empty because the IM-platform set lives in the
	// daemon package and importing it here would create a cycle. Clients
	// (Desktop session grouping) read this directly instead of re-deriving.
	Kind string `json:"kind,omitempty"`
	// Pinned mirrors Session.Pinned: sticky-to-top regardless of recency.
	Pinned bool `json:"pinned,omitempty"`
	// Favorite mirrors Session.Favorite: starred for filter views.
	Favorite bool `json:"favorite,omitempty"`
	// Agent identifies the agent scope this session belongs to: empty string
	// for the default agent, otherwise the agent slug. Populated at HTTP-list
	// time by the daemon (Store.List has no view of which scope it serves) so
	// cross-agent views (GET /sessions?scope=all) can attribute each session.
	// Always emitted (even when empty) so clients can rely on its presence.
	Agent string `json:"agent"`
}

type Store struct {
	dir   string
	index *Index // nil = index unavailable (graceful degradation)

	// clockMu guards lastUpdated so concurrent Save calls produce strictly
	// monotonic UpdatedAt values. Without this, two Saves that observe the
	// same time.Now() reading would land on identical timestamps and the
	// index ORDER BY updated_at DESC would return them in non-deterministic
	// order — caught by TestSmoke_EndToEnd on low-resolution clocks
	// (GitHub Actions Linux runners).
	clockMu     sync.Mutex
	lastUpdated time.Time
}

// safeSessionPath joins id onto s.dir while refusing inputs that could escape
// the sessions directory. Defense in depth — handler-edge validation is the
// primary block, but Store.Load/Delete are also reachable from internal code
// paths where the id may have been derived rather than validated.
func (s *Store) safeSessionPath(id string) (string, error) {
	if id == "" {
		return "", fmt.Errorf("session id is empty")
	}
	// "." and ".." survive filepath.Base unchanged and contain no slash, so they
	// would otherwise slip past the next check and resolve to ".json" / "..json"
	// inside s.dir — bypassing the helper's contract even if not directly exploitable.
	if id == "." || id == ".." {
		return "", fmt.Errorf("invalid session id: %s", id)
	}
	if id != filepath.Base(id) || strings.ContainsAny(id, `/\`) {
		return "", fmt.Errorf("invalid session id: %s", id)
	}
	return filepath.Join(s.dir, id+".json"), nil
}

func NewStore(dir string) *Store {
	os.MkdirAll(dir, 0700)
	sweepStaleTempFiles(dir)
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

// nextUpdatedAt returns time.Now(), bumped by 1ns if the underlying clock has
// not advanced past the previous Save. This makes the per-Store sequence of
// UpdatedAt values strictly monotonic, which the SQLite index relies on for
// stable ORDER BY updated_at DESC (the SQL has no tie-breaker).
func (s *Store) nextUpdatedAt() time.Time {
	s.clockMu.Lock()
	defer s.clockMu.Unlock()
	now := time.Now()
	if !now.After(s.lastUpdated) {
		now = s.lastUpdated.Add(time.Nanosecond)
	}
	s.lastUpdated = now
	return now
}

// sweepStaleTempFiles removes orphaned atomic-write temp files left in dir by a
// crash between os.CreateTemp and os.Rename in writeFileAtomic. Those temps are
// dot-prefixed and .tmp-suffixed (".<id>.json-<rand>.tmp"), so the .json
// session loader already ignores them — this just stops them accumulating
// across crashes. Best-effort; called once per Store at construction.
func sweepStaleTempFiles(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".tmp") {
			continue
		}
		os.Remove(filepath.Join(dir, name))
	}
}

// writeFileAtomic writes data to a unique temp file in the same directory and
// atomically renames it over path. The daemon can hold two Manager instances
// over the same sessions dir (a user rename via the shared sc.managers manager
// vs the route's own manager running a turn), so concurrent writes to one
// <id>.json are real. A plain truncate-in-place os.WriteFile could interleave
// two writers' bytes into a torn file that fails to parse on the next Load;
// temp+rename makes every reader observe a complete file (old or new). The
// residual race collapses to a benign last-writer-wins lost update — never
// corruption. Mirrors the schedule package's atomic-write pattern.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+"-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	cleanup := func() {
		tmp.Close()
		os.Remove(tmpPath)
	}
	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return err
	}
	return nil
}

func (s *Store) Save(sess *Session) error {
	sess.UpdatedAt = s.nextUpdatedAt()
	if sess.CreatedAt.IsZero() {
		sess.CreatedAt = sess.UpdatedAt
	}
	if sess.SchemaVersion == 0 {
		sess.SchemaVersion = 1
	}
	backfillLegacyScheduleID(sess)

	data, err := json.MarshalIndent(sess, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal session: %w", err)
	}

	path := filepath.Join(s.dir, sess.ID+".json")
	if err := writeFileAtomic(path, data, 0600); err != nil {
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
	sess.TitleAuto = false // user rename locks the title against auto-upgrade

	data, err := json.MarshalIndent(sess, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal session: %w", err)
	}

	path := filepath.Join(s.dir, sess.ID+".json")
	if err := writeFileAtomic(path, data, 0600); err != nil {
		return err
	}

	if s.index != nil {
		s.index.UpsertSession(sess)
	}
	return nil
}

// PatchAutoTitle overwrites a machine-derived title with a freshly generated
// one. Guards: (1) a user-locked title (TitleAuto == false) is never touched;
// (2) a title already generated at a STRICTLY higher turn count is kept — an
// equal-or-lower turn re-trigger still overwrites, but turn counts are
// monotonic per session so in practice only a richer later turn wins.
// Returns true if written.
func (s *Store) PatchAutoTitle(id, title string, atTurns int) (bool, error) {
	sess, err := s.Load(id)
	if err != nil {
		return false, err
	}
	if !sess.TitleAuto || sess.TitleTurns > atTurns {
		return false, nil
	}
	sess.Title = title
	sess.TitleTurns = atTurns
	data, err := json.MarshalIndent(sess, "", "  ")
	if err != nil {
		return false, fmt.Errorf("marshal session: %w", err)
	}
	if err := writeFileAtomic(filepath.Join(s.dir, sess.ID+".json"), data, 0600); err != nil {
		return false, err
	}
	if s.index != nil {
		s.index.UpsertSession(sess)
	}
	return true, nil
}

// PatchFlags re-reads the session from disk, applies any non-nil flag, and
// writes it back. UpdatedAt is not touched so list ordering is preserved
// (pinned/favorite is metadata, not activity). Either flag may be nil to
// leave that field unchanged; passing both nil is a no-op.
func (s *Store) PatchFlags(id string, pinned, favorite *bool) error {
	if pinned == nil && favorite == nil {
		return nil
	}
	sess, err := s.Load(id)
	if err != nil {
		return err
	}
	if pinned != nil {
		sess.Pinned = *pinned
	}
	if favorite != nil {
		sess.Favorite = *favorite
	}

	data, err := json.MarshalIndent(sess, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal session: %w", err)
	}

	path := filepath.Join(s.dir, sess.ID+".json")
	if err := writeFileAtomic(path, data, 0600); err != nil {
		return err
	}

	if s.index != nil {
		// Narrow UPDATE on pinned/favorite columns only — does not rebuild
		// the messages/FTS index, so toggle cost stays O(1) regardless of
		// session length. When the index row is missing (manual session
		// import, prior best-effort UpsertSession failure during Save,
		// partial index corruption), fall back to a full UpsertSession so
		// the session reappears in GET /sessions with current flags. Best-
		// effort: any other index error is dropped, matching PatchTitle.
		if err := s.index.UpdateSessionFlags(sess.ID, pinned, favorite); errors.Is(err, os.ErrNotExist) {
			s.index.UpsertSession(sess)
		}
	}
	return nil
}

// PatchPublishedShares re-reads the session from disk, applies mutate to the
// current PublishedShares slice, and writes it back. UpdatedAt is NOT touched
// — share/retract is metadata, not user activity, so a bump would re-sort the
// session to the top of the recency list unnecessarily. The mutator pattern
// (vs. an Append + Remove pair) lets a future caller batch share+retract or
// dedup in-place without round-tripping through two reads.
//
// Concurrency note: this method is NOT safe to call concurrently with
// Store.Save on the same session — both do a full Marshal+WriteFile and the
// last writer wins. Callers MUST serialise via Manager.PatchPublishedShares
// (which holds Manager.mu around the whole RMW). Direct Store callers are
// only acceptable in tests where no concurrent Save is in flight.
func (s *Store) PatchPublishedShares(id string, mutate func([]PublishedShareEntry) []PublishedShareEntry) error {
	sess, err := s.Load(id)
	if err != nil {
		return err
	}
	sess.PublishedShares = mutate(sess.PublishedShares)

	data, err := json.MarshalIndent(sess, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal session: %w", err)
	}

	path := filepath.Join(s.dir, sess.ID+".json")
	return writeFileAtomic(path, data, 0600)
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
	return writeFileAtomic(path, data, 0600)
}

func (s *Store) Load(id string) (*Session, error) {
	path, err := s.safeSessionPath(id)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		// Return the not-exist error unwrapped so callers using
		// os.IsNotExist(err) still detect it — os.IsNotExist does NOT
		// traverse fmt.Errorf("%w") chains, so wrapping a missing-file
		// error here made handleGetSession et al. fall through to 500
		// instead of 404 (e.g. loading a named-agent session via the
		// default-dir manager when ?agent= was omitted).
		if errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		return nil, fmt.Errorf("read session: %w", err)
	}

	var sess Session
	if err := json.Unmarshal(data, &sess); err != nil {
		return nil, fmt.Errorf("parse session: %w", err)
	}
	if sess.SchemaVersion == 0 {
		sess.SchemaVersion = 1
	}
	// Sessions created before schedule_id was introduced already persisted a
	// stable channel in the form "schedule-<id>". Recover that association at
	// load time so the new filter includes legacy history without rewriting
	// every JSON file eagerly. A later normal Save self-heals the file.
	backfillLegacyScheduleID(&sess)
	// Load-time self-heal for malformed thinking blocks persisted by earlier
	// daemon versions. See internal/context/thinking_sanitize.go for the
	// wire-shape that motivates this. The next Save() persists the cleaned
	// shape, so disk copies repair themselves on first read after upgrade.
	sess.Messages = ctxwin.DropMalformedThinking(sess.Messages)
	return &sess, nil
}

func backfillLegacyScheduleID(sess *Session) {
	if sess == nil || sess.ScheduleID != "" || !strings.EqualFold(strings.TrimSpace(sess.Source), "schedule") {
		return
	}
	if id, ok := strings.CutPrefix(strings.TrimSpace(sess.Channel), "schedule-"); ok && id != "" {
		sess.ScheduleID = id
	}
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
			ID:         sess.ID,
			Title:      sess.Title,
			CWD:        sess.CWD,
			CreatedAt:  sess.CreatedAt,
			UpdatedAt:  sess.UpdatedAt,
			MsgCount:   len(sess.Messages),
			Source:     sess.Source,
			ScheduleID: sess.ScheduleID,
			Pinned:     sess.Pinned,
			Favorite:   sess.Favorite,
		})
	}

	sort.Slice(summaries, func(i, j int) bool {
		if summaries[i].Pinned != summaries[j].Pinned {
			return summaries[i].Pinned
		}
		return summaries[i].UpdatedAt.After(summaries[j].UpdatedAt)
	})
	return summaries, nil
}

func (s *Store) Delete(id string) error {
	path, err := s.safeSessionPath(id)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil {
		return err
	}

	if s.index != nil {
		s.index.DeleteSession(id) // best-effort
	}
	return nil
}

func (s *Store) LatestByRouteKey(routeKey string) (*Session, error) {
	if strings.TrimSpace(routeKey) == "" {
		return nil, nil
	}
	if s.index != nil {
		id, err := s.index.LatestUpdatedIDByRouteKey(routeKey)
		if err == nil {
			// Index is authoritative for negative results. Skipping the
			// brute-force scan here matters during v0.1.1 → v0.1.2 upgrade:
			// every pre-upgrade session has empty route_key, so each first
			// inbound on a previously-unbound thread would otherwise walk
			// the whole sessions dir + JSON-decode every file before
			// returning nil.
			if id == "" {
				return nil, nil
			}
			if sess, loadErr := s.Load(id); loadErr == nil {
				return sess, nil
			}
			// JSON file missing/corrupt — fall through to brute-force.
		}
	}

	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}

	var best *Session
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".json")
		sess, err := s.Load(id)
		if err != nil || sess.RouteKey != routeKey {
			continue
		}
		if best == nil || sess.UpdatedAt.After(best.UpdatedAt) {
			best = sess
		}
	}
	return best, nil
}

func (s *Store) Search(query string, limit int) ([]SearchResult, error) {
	if s.index == nil {
		return nil, fmt.Errorf("search index not available")
	}
	return s.index.Search(query, limit)
}

// SearchSessions runs a session-grouped content search (see Index.SearchSessions).
func (s *Store) SearchSessions(query string) ([]SessionHit, error) {
	if s.index == nil {
		return nil, fmt.Errorf("search index not available")
	}
	return s.index.SearchSessions(query)
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
