package session

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
)

// Manager provides session lifecycle operations. It is safe for concurrent use
// across multiple route entries that share the same sessions directory.
type Manager struct {
	mu              sync.Mutex
	store           *Store
	current         *Session
	onCloseFns      []func()            // manager-wide cleanup callbacks invoked on Close
	sessionCloseFns map[string][]func() // per-session cleanup invoked on session switch/Close; append semantics
	runtime         map[string]*sessionRuntime
}

type sessionRuntime struct {
	workingSet *agent.WorkingSet
}

func NewManager(sessionsDir string) *Manager {
	return &Manager{
		store:   NewStore(sessionsDir),
		runtime: make(map[string]*sessionRuntime),
	}
}

func (m *Manager) NewSession() *Session {
	m.mu.Lock()
	prevID := ""
	if m.current != nil {
		prevID = m.current.ID
	}
	id := generateID()
	// CWD is intentionally left empty — callers (daemon runner, TUI,
	// one-shot CLI) are responsible for setting it explicitly based on
	// their own context. Historically this was populated via os.Getwd()
	// which leaked the daemon startup directory into every new session.
	m.current = &Session{
		SchemaVersion: 1,
		ID:            id,
		CreatedAt:     time.Now(),
		Title:         "New session",
	}
	m.ensureRuntimeLocked(id)
	sess := m.current
	callbacks := m.takeSessionCloseLocked(prevID)
	m.mu.Unlock()
	runCallbacks(callbacks)
	return sess
}

func (m *Manager) Current() *Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.current
}

func (m *Manager) Resume(id string) (*Session, error) {
	m.mu.Lock()
	prevID := ""
	if m.current != nil {
		prevID = m.current.ID
	}
	sess, err := m.store.Load(id)
	if err != nil {
		m.mu.Unlock()
		return nil, err
	}
	m.current = sess
	m.ensureRuntimeLocked(sess.ID)
	callbacks := []func(){}
	if prevID != "" && prevID != sess.ID {
		callbacks = m.takeSessionCloseLocked(prevID)
	}
	m.mu.Unlock()
	runCallbacks(callbacks)
	return sess, nil
}

// Load 从磁盘读取指定 session，不修改 m.current。
func (m *Manager) Load(id string) (*Session, error) {
	return m.store.Load(id)
}

func (m *Manager) Save() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.current == nil {
		return nil
	}
	return m.store.Save(m.current)
}

// PatchTitle updates the title of the given session and persists it.
// If the target is the active session, the in-memory title is also updated.
// Disk is written first so a failed write won't leave memory inconsistent.
func (m *Manager) PatchTitle(id, title string) error {
	err := m.store.PatchTitle(id, title)
	if err != nil {
		return err
	}
	m.mu.Lock()
	if m.current != nil && m.current.ID == id {
		m.current.Title = title
	}
	m.mu.Unlock()
	return nil
}

// PatchSummaryCache 从磁盘重新读取最新 session，仅更新摘要缓存字段后写回。
func (m *Manager) PatchSummaryCache(id, summary, cacheKey string) error {
	return m.store.PatchSummaryCache(id, summary, cacheKey)
}

// AddUsage merges a usage delta into the session's cumulative UsageSummary.
// If the target session is currently loaded it updates the in-memory copy too.
// Caller is expected to follow up with Save() — this keeps persistence batching
// up to the orchestration layer (daemon runner, CLI main) rather than doing a
// write per LLM call.
//
// Provenance marker: when AddUsage initializes a fresh Usage field (nil →
// non-nil) the session is marked SchemaVersion 2, because every value in
// that new Usage will be written with split LLM/tool semantics. Sessions
// that already had a Usage field from an earlier build are left at their
// current SchemaVersion so we don't advertise a clean split we can't
// guarantee — those totals may be mixed from intermediate builds.
func (m *Manager) AddUsage(sessionID string, delta UsageSummary) {
	if sessionID == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.current != nil && m.current.ID == sessionID {
		freshUsage := m.current.Usage == nil
		if freshUsage {
			m.current.Usage = &UsageSummary{}
			if m.current.SchemaVersion < 2 {
				m.current.SchemaVersion = 2
			}
		}
		m.current.Usage.Add(delta)
	}
}

func (m *Manager) List() ([]SessionSummary, error) {
	return m.store.List()
}

func (m *Manager) Delete(id string) error {
	m.mu.Lock()
	delete(m.runtime, id)
	m.mu.Unlock()
	return m.store.Delete(id)
}

func (m *Manager) Search(query string, limit int) ([]SearchResult, error) {
	return m.store.Search(query, limit)
}

// OnClose registers a function to be called when the manager is closed.
// Used for manager-wide cleanup that is not tied to a specific session.
func (m *Manager) OnClose(fn func()) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onCloseFns = append(m.onCloseFns, fn)
}

// OnSessionClose registers cleanup for a specific session ID. Multiple
// callbacks per session are appended and all fire (in registration order)
// when the manager switches away from that session or closes. Each caller
// is responsible for one resource — this is a safety-critical lifecycle
// hook, so composing cleanups by append (not replace) avoids silent leaks
// when two subsystems register for the same session.
func (m *Manager) OnSessionClose(sessionID string, fn func()) {
	if sessionID == "" || fn == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sessionCloseFns == nil {
		m.sessionCloseFns = make(map[string][]func())
	}
	m.sessionCloseFns[sessionID] = append(m.sessionCloseFns[sessionID], fn)
}

// WorkingSet returns the in-memory deferred-tool working set for a session.
// The working set is session-scoped runtime state and is never persisted.
func (m *Manager) WorkingSet(sessionID string) *agent.WorkingSet {
	if sessionID == "" {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ensureRuntimeLocked(sessionID).workingSet
}

// CurrentWorkingSet returns the working set for the current session.
func (m *Manager) CurrentWorkingSet() *agent.WorkingSet {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.current == nil {
		return nil
	}
	return m.ensureRuntimeLocked(m.current.ID).workingSet
}

func (m *Manager) Close() error {
	m.mu.Lock()
	fns := append([]func(){}, m.onCloseFns...)
	m.onCloseFns = nil
	for _, sessFns := range m.sessionCloseFns {
		for _, fn := range sessFns {
			if fn != nil {
				fns = append(fns, fn)
			}
		}
	}
	m.sessionCloseFns = nil
	m.runtime = make(map[string]*sessionRuntime)
	m.mu.Unlock()
	runCallbacks(fns)
	return m.store.Close()
}

func (m *Manager) takeSessionCloseLocked(sessionID string) []func() {
	if sessionID == "" || m.sessionCloseFns == nil {
		return nil
	}
	fns, ok := m.sessionCloseFns[sessionID]
	if !ok || len(fns) == 0 {
		return nil
	}
	delete(m.sessionCloseFns, sessionID)
	return fns
}

func runCallbacks(fns []func()) {
	for _, fn := range fns {
		fn()
	}
}

func (m *Manager) ensureRuntimeLocked(sessionID string) *sessionRuntime {
	if sessionID == "" {
		panic("session runtime requires non-empty session ID")
	}
	if m.runtime == nil {
		m.runtime = make(map[string]*sessionRuntime)
	}
	rt, ok := m.runtime[sessionID]
	if !ok || rt == nil {
		rt = &sessionRuntime{workingSet: agent.NewWorkingSet()}
		m.runtime[sessionID] = rt
	}
	if rt.workingSet == nil {
		rt.workingSet = agent.NewWorkingSet()
	}
	return rt
}

func (m *Manager) RebuildIndex() error {
	return m.store.RebuildIndex()
}

// TruncateMessages 将指定 session 的消息截断为前 index 条，同步截断 MessageMeta。
// 用于"编辑历史消息后重新发送"场景，截断点之后的所有消息将被丢弃并持久化。
func (m *Manager) TruncateMessages(id string, index int) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	sess, err := m.store.Load(id)
	if err != nil {
		return err
	}
	if index < 0 || index > len(sess.Messages) {
		return fmt.Errorf("message_index %d out of range [0, %d]", index, len(sess.Messages))
	}
	sess.Messages = sess.Messages[:index]
	if len(sess.MessageMeta) > index {
		sess.MessageMeta = sess.MessageMeta[:index]
	}
	// 若当前内存中缓存的 session 与截断目标一致，同步更新内存状态
	if m.current != nil && m.current.ID == id {
		m.current = sess
	}
	return m.store.Save(sess)
}

// ResumeLatest loads the most recently updated session from disk.
// Returns (nil, nil) if no sessions exist.
func (m *Manager) ResumeLatest() (*Session, error) {
	// Fast path: use index to find the latest session by updated_at.
	// Only trust a non-empty result — if index says "empty", fall through
	// to brute-force in case index is stale or partially migrated.
	if m.store.index != nil {
		id, err := m.store.index.LatestUpdatedID()
		if err == nil && id != "" {
			if sess, resumeErr := m.Resume(id); resumeErr == nil {
				return sess, nil
			}
			// JSON file missing/corrupt — fall through to brute-force
		}
		// On error, empty result, or failed resume — fall through to JSON scan
	}

	summaries, err := m.store.List()
	if err != nil {
		return nil, err
	}
	if len(summaries) == 0 {
		return nil, nil
	}

	// Find the session with the most recent UpdatedAt.
	// List() only has CreatedAt, so we load each to check UpdatedAt.
	// For typical daemon use (1 session per agent), this is just 1 load.
	var bestID string
	var bestTime time.Time
	for _, s := range summaries {
		sess, err := m.store.Load(s.ID)
		if err != nil {
			continue
		}
		if sess.UpdatedAt.After(bestTime) {
			bestTime = sess.UpdatedAt
			bestID = sess.ID
		}
	}
	if bestID == "" {
		return nil, nil
	}
	return m.Resume(bestID)
}

func generateID() string {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		// Fallback to timestamp-only ID if entropy fails
		return time.Now().Format("2006-01-02-150405")
	}
	return fmt.Sprintf("%s-%s", time.Now().Format("2006-01-02"), hex.EncodeToString(b))
}
