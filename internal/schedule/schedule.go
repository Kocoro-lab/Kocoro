package schedule

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agents"
	"github.com/adhocore/gronx"
)

type Schedule struct {
	ID         string    `json:"id"`
	Agent      string    `json:"agent"`
	Cron       string    `json:"cron"`
	Prompt     string    `json:"prompt"`
	Enabled    bool      `json:"enabled"`
	SyncStatus string    `json:"sync_status"`
	CreatedAt  time.Time `json:"created_at"`

	// Stateful controls whether scheduled runs preserve LLM context across
	// triggers. nil = legacy schedule (treated as stateful for backward
	// compatibility); *false = each run gets an empty history snapshot
	// (default for new schedules); *true = explicit opt-in to share history
	// across runs. The session file is appended to on every run regardless —
	// only the LLM's view (runner.historySnapshotForRequest) is affected.
	Stateful *bool `json:"stateful,omitempty"`

	// Broadcast is a three-state opt-in/out for IM channel push:
	//   nil   → smart default (see internal/daemon/broadcast_gate.shouldBroadcast)
	//   true  → always broadcast (regardless of CreatedFromSource)
	//   false → never broadcast (regardless of CreatedFromSource)
	Broadcast *bool `json:"broadcast,omitempty"`

	// CreatedFromSource snapshots req.Source at creation time. Used by the
	// daemon's shouldBroadcast helper as the smart-default signal. Examples:
	// "slack", "feishu", "webview", "tui", "cli", "one-shot".
	// Empty string means "pre-feature" (the field didn't exist when the
	// schedule was saved) — treated as unknown and falls through to silent.
	CreatedFromSource string `json:"created_from_source,omitempty"`

	// LastRunAt is the wall-clock time of the most recent scheduler-triggered
	// run (succeeded or failed). nil = never run. Stamped by
	// Manager.MarkLastRun from the scheduler's runWithLifecycle.
	LastRunAt *time.Time `json:"last_run_at,omitempty"`

	// LastRunSessionID is the session that received the most recent run's
	// transcript. Resolves through the standard session store (agent-scoped
	// or global default) — see schedule.SummarizeLastRun. Empty = never run.
	LastRunSessionID string `json:"last_run_session_id,omitempty"`

	// LastRunMessageStartIndex / LastRunMessageEndIndex pin down the precise
	// slice of sess.Messages this run wrote. Required because the named-agent
	// route key is `agent:<name>` (SessionCache.agentRouteKey in router.go) —
	// every schedule + every interactive chat with the same agent shares one
	// session, so without an index range, schedule_show would return the
	// session's tail (which could be the user's last chat reply, not the
	// schedule's output). When the schedule has never run, both default to 0;
	// combined with the empty LastRunSessionID this unambiguously signals
	// never-run.
	LastRunMessageStartIndex int `json:"last_run_message_start_index,omitempty"`
	LastRunMessageEndIndex   int `json:"last_run_message_end_index,omitempty"`
}

// IsStateless reports whether this schedule should run with an empty LLM
// history snapshot. Legacy schedules (Stateful == nil) preserve their
// pre-feature stateful behaviour.
func (s *Schedule) IsStateless() bool {
	return s.Stateful != nil && !*s.Stateful
}

type UpdateOpts struct {
	Cron     *string
	Prompt   *string
	Enabled  *bool
	Stateful *bool // nil = no change; non-nil = overwrite (including flip to/from legacy nil)
}

type Manager struct {
	indexPath string
}

func NewManager(indexPath string) *Manager {
	return &Manager{indexPath: indexPath}
}

func validateCron(expr string) error {
	g := gronx.New()
	if !g.IsValid(expr) {
		return fmt.Errorf("invalid cron expression: %q", expr)
	}
	return nil
}

func validateAgent(name string) error {
	if name == "" {
		return nil
	}
	return agents.ValidateAgentName(name)
}

func validatePrompt(prompt string) error {
	if strings.TrimSpace(prompt) == "" {
		return fmt.Errorf("prompt cannot be empty")
	}
	if strings.ContainsRune(prompt, 0) {
		return fmt.Errorf("prompt contains null bytes")
	}
	return nil
}

func (m *Manager) load() ([]Schedule, error) {
	f, err := os.Open(m.indexPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH); err != nil {
		return nil, fmt.Errorf("flock shared: %w", err)
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	var schedules []Schedule
	if err := json.NewDecoder(f).Decode(&schedules); err != nil {
		return nil, err
	}
	return schedules, nil
}

func (m *Manager) save(schedules []Schedule) error {
	dir := filepath.Dir(m.indexPath)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".schedules-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpPath := tmp.Name()
	if err := syscall.Flock(int(tmp.Fd()), syscall.LOCK_EX); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("flock exclusive: %w", err)
	}
	data, err := json.MarshalIndent(schedules, "", "  ")
	if err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return err
	}
	tmp.Close()
	if err := os.Rename(tmpPath, m.indexPath); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("atomic rename: %w", err)
	}
	return nil
}

func (m *Manager) lockedModify(fn func([]Schedule) ([]Schedule, error)) error {
	dir := filepath.Dir(m.indexPath)
	os.MkdirAll(dir, 0700)
	lockPath := m.indexPath + ".lock"
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return fmt.Errorf("open lock file: %w", err)
	}
	defer lockFile.Close()
	// Do NOT os.Remove the lock file — concurrent goroutines may flock
	// on different inodes if the file is deleted and recreated between them.
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("flock: %w", err)
	}
	defer syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
	var schedules []Schedule
	if data, err := os.ReadFile(m.indexPath); err == nil {
		json.Unmarshal(data, &schedules)
	}
	schedules, err = fn(schedules)
	if err != nil {
		return err
	}
	return m.save(schedules)
}

// CreateOpts carries the optional, non-validated fields a caller can attach
// to a new schedule. Required fields (agent/cron/prompt/stateful) stay on
// the function signature so misuse is a compile error. Pointer/string-typed
// because nil/"" are both legal "not specified" markers downstream.
type CreateOpts struct {
	// Broadcast is the IM-channel-push opt-in/out. nil = smart default;
	// *true = always broadcast; *false = never broadcast. See the schedule
	// struct comment for the full semantics.
	Broadcast *bool
	// CreatedFromSource snapshots the originating req.Source at creation
	// time (e.g. "slack", "feishu", "webview", "tui"). Drives the smart
	// default in internal/daemon/broadcast_gate.shouldBroadcast. Empty
	// string is acceptable and means "unknown / pre-feature caller".
	CreatedFromSource string
}

func (m *Manager) Create(agentName, cron, prompt string, stateful bool) (string, error) {
	return m.CreateWithOpts(agentName, cron, prompt, stateful, CreateOpts{})
}

// CreateWithOpts is the extended form of Create that accepts the optional
// broadcast + source fields. Kept as a separate method so the many existing
// callers of Create stay compilable without churn.
func (m *Manager) CreateWithOpts(agentName, cron, prompt string, stateful bool, opts CreateOpts) (string, error) {
	if err := validateAgent(agentName); err != nil {
		return "", err
	}
	if err := validateCron(cron); err != nil {
		return "", err
	}
	if err := validatePrompt(prompt); err != nil {
		return "", err
	}
	id := generateScheduleID()
	statefulCopy := stateful
	s := Schedule{
		ID: id, Agent: agentName, Cron: cron, Prompt: prompt,
		Enabled: true, SyncStatus: "ok", CreatedAt: time.Now(),
		Stateful:          &statefulCopy, // always explicit on Create — never leave nil for new rows
		CreatedFromSource: opts.CreatedFromSource,
	}
	if opts.Broadcast != nil {
		bCopy := *opts.Broadcast
		s.Broadcast = &bCopy
	}
	err := m.lockedModify(func(schedules []Schedule) ([]Schedule, error) {
		return append(schedules, s), nil
	})
	if err != nil {
		return "", err
	}
	return id, nil
}

func (m *Manager) List() ([]Schedule, error) {
	return m.load()
}

func (m *Manager) Get(id string) (*Schedule, error) {
	schedules, err := m.load()
	if err != nil {
		return nil, err
	}
	for _, s := range schedules {
		if s.ID == id {
			return &s, nil
		}
	}
	return nil, fmt.Errorf("schedule %q not found", id)
}

func (m *Manager) Update(id string, opts *UpdateOpts) error {
	if opts.Cron == nil && opts.Prompt == nil && opts.Enabled == nil && opts.Stateful == nil {
		return fmt.Errorf("no fields to update: provide at least one of cron, prompt, enabled, or stateful")
	}
	if opts.Cron != nil {
		if err := validateCron(*opts.Cron); err != nil {
			return err
		}
	}
	if opts.Prompt != nil {
		if err := validatePrompt(*opts.Prompt); err != nil {
			return err
		}
	}
	return m.lockedModify(func(schedules []Schedule) ([]Schedule, error) {
		for i, s := range schedules {
			if s.ID == id {
				if opts.Cron != nil {
					schedules[i].Cron = *opts.Cron
				}
				if opts.Prompt != nil && *opts.Prompt != s.Prompt {
					schedules[i].Prompt = *opts.Prompt
					// Prompt change means the task goal changed, so the
					// previously captured "why" is stale. Delete the sidecar
					// BEFORE save() and while still holding the index lock —
					// otherwise a scheduler tick could land in the window
					// where the new prompt is already written but the old
					// sidecar still exists, mixing old context into the new
					// task.
					//
					// Note: if save() fails afterwards, the sidecar is gone
					// but the schedule is not updated. That's an acceptable
					// degradation — losing context is better than running
					// with an inconsistent (new prompt, old context) state.
					m.RemoveContext(id)
				}
				if opts.Enabled != nil {
					schedules[i].Enabled = *opts.Enabled
				}
				if opts.Stateful != nil {
					v := *opts.Stateful
					schedules[i].Stateful = &v
				}
				return schedules, nil
			}
		}
		return nil, fmt.Errorf("schedule %q not found", id)
	})
}

// MarkLastRun records that the schedule fired and which session captured
// the transcript, plus the message index range this run wrote. Called by
// the scheduler at end-of-lifecycle. Idempotent overwrite — only the most
// recent run is tracked, by design: list endpoints stay light (just
// pointers), and a separate show endpoint resolves the pointer to the
// actual transcript slice on demand.
//
// The index range pins down precisely which slice of sess.Messages came
// from this run, so SummarizeLastRun can return the correct turns even
// when the session is shared across multiple schedules + interactive chat
// (the named-agent route key is `agent:<name>`, which makes sharing the
// common case).
//
// No-op cases:
//   - id not found: schedule may have been deleted between dispatch and
//     completion; we should not crash the scheduler.
//   - sessionID empty: the run failed before session resolution; there's
//     nothing for SummarizeLastRun to fetch.
func (m *Manager) MarkLastRun(id, sessionID string, when time.Time, startIdx, endIdx int) error {
	if sessionID == "" {
		return nil
	}
	return m.lockedModify(func(schedules []Schedule) ([]Schedule, error) {
		for i := range schedules {
			if schedules[i].ID == id {
				w := when
				schedules[i].LastRunAt = &w
				schedules[i].LastRunSessionID = sessionID
				schedules[i].LastRunMessageStartIndex = startIdx
				schedules[i].LastRunMessageEndIndex = endIdx
				return schedules, nil
			}
		}
		return schedules, nil // unknown id — silent
	})
}

func (m *Manager) Remove(id string) error {
	err := m.lockedModify(func(schedules []Schedule) ([]Schedule, error) {
		filtered := make([]Schedule, 0, len(schedules))
		found := false
		for _, s := range schedules {
			if s.ID == id {
				found = true
				continue
			}
			filtered = append(filtered, s)
		}
		if !found {
			return nil, fmt.Errorf("schedule %q not found", id)
		}
		return filtered, nil
	})
	if err == nil {
		// Clean up the associated context sidecar.
		m.RemoveContext(id)
	}
	return err
}

func (m *Manager) SetSyncStatus(id, status string) error {
	log.Printf("schedule: SetSyncStatus is deprecated (no-op)")
	return nil
}

func (m *Manager) Sync() (int, error) {
	log.Printf("schedule: Sync is deprecated (no-op)")
	return 0, nil
}

// ContextMessage is the compact representation of a conversation message used
// for schedule context sidecars: just the role and plain text.
type ContextMessage struct {
	Role    string `json:"role"`    // "user" or "assistant"
	Content string `json:"content"` // plain text content
}

// contextDir returns the directory that stores per-schedule context sidecars.
func (m *Manager) contextDir() string {
	return filepath.Join(filepath.Dir(m.indexPath), "schedule_context")
}

// SaveContext writes the conversation context to the schedule's sidecar file.
// It uses temp+rename to ensure the write is atomic — otherwise a crash or
// concurrent read could see a half-written JSON file, and a subsequent
// LoadContext parse failure would cause runSchedule to silently execute with
// no context.
func (m *Manager) SaveContext(id string, messages []ContextMessage) error {
	if len(messages) == 0 {
		return nil
	}
	dir := m.contextDir()
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create context dir: %w", err)
	}
	data, err := json.MarshalIndent(messages, "", "  ")
	if err != nil {
		return err
	}
	finalPath := filepath.Join(dir, id+".json")
	tmp, err := os.CreateTemp(dir, "."+id+"-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, finalPath); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("atomic rename: %w", err)
	}
	return nil
}

// LoadContext loads the conversation context for a schedule. Returns
// (nil, nil) when the sidecar file does not exist.
func (m *Manager) LoadContext(id string) ([]ContextMessage, error) {
	data, err := os.ReadFile(filepath.Join(m.contextDir(), id+".json"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var msgs []ContextMessage
	if err := json.Unmarshal(data, &msgs); err != nil {
		return nil, err
	}
	return msgs, nil
}

// RemoveContext deletes the conversation context sidecar for a schedule.
func (m *Manager) RemoveContext(id string) {
	os.Remove(filepath.Join(m.contextDir(), id+".json"))
}

// HasContext reports whether the schedule has an associated context sidecar.
func (m *Manager) HasContext(id string) bool {
	_, err := os.Stat(filepath.Join(m.contextDir(), id+".json"))
	return err == nil
}

func generateScheduleID() string {
	b := make([]byte, 4)
	rand.Read(b)
	return hex.EncodeToString(b)
}
