package daemon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/agents"
	"github.com/Kocoro-lab/ShanClaw/internal/config"
	"github.com/Kocoro-lab/ShanClaw/internal/fslock"
	"github.com/Kocoro-lab/ShanClaw/internal/skills"
)

const (
	CapSkillInstallRecommendationV1 = "skill_install_recommendation_v1"
	skillRecommendationHeader       = "X-Kocoro-Consumer-Capabilities"
	desktopDeviceHeader             = "X-Kocoro-Desktop-Device-ID"
	// skillRecommendationTTL bounds how long an offered card stays actionable,
	// and (via pruneLocked) how long a terminal record is retained for
	// idempotent re-entry. Workload: a user who leaves Desktop open overnight
	// and installs the next morning must still be able to continue the same
	// card. Symptom when it binds: continue/dismiss returns "no longer active"
	// and the run has to be re-asked. Override: not user-tunable by design —
	// the token is a bearer capability and a longer window widens its replay
	// surface; raise this const if the overnight workload proves too tight.
	skillRecommendationTTL = 24 * time.Hour
)

// skillRecommendationTerminalStates are the states a record can never leave.
// Once past its TTL such a record is dead weight: the card cannot be shown,
// continued, or dismissed again, and the install receipt it carries has already
// been applied to disk.
var skillRecommendationTerminalStates = map[string]bool{
	"completed": true, "dismissed": true, "expired": true, "superseded": true,
}

func skillRecommendationsEnabled(cfg *config.Config) bool {
	return cfg == nil || cfg.Daemon.SkillRecommendationsEnabled == nil || *cfg.Daemon.SkillRecommendationsEnabled
}
func (s *Server) skillRecommendationsEnabled() bool {
	if s == nil || s.deps == nil {
		return false
	}
	if s.skillRecommendationsOff.Load() {
		return false
	}
	cfg, _, _ := s.deps.Snapshot()
	return skillRecommendationsEnabled(cfg)
}

type skillRecommendationV1 struct {
	SchemaVersion    int                             `json:"schema_version"`
	RecommendationID string                          `json:"recommendation_id"`
	SessionID        string                          `json:"session_id"`
	TurnID           string                          `json:"turn_id"`
	CatalogRevision  string                          `json:"catalog_revision"`
	State            string                          `json:"state"`
	Items            []skillRecommendationItemWireV1 `json:"items"`
	ReasonCode       string                          `json:"reason_code"`
	ExpiresAt        time.Time                       `json:"expires_at"`
	// ContinuationToken is an idempotent bearer capability for this exact,
	// account+device-bound card. It must cross the directed SSE wire or Desktop
	// cannot continue after installation; it is never put on EventBus/audit.
	ContinuationToken   string                             `json:"continuation_token"`
	OwnerAccountID      string                             `json:"-"`
	OwnerDeviceID       string                             `json:"-"`
	OwnerAgentName      string                             `json:"-"`
	Consumed            bool                               `json:"-"`
	ContinuationRunning bool                               `json:"-"`
	Generation          uint64                             `json:"-"`
	InstallEntries      []skills.CatalogEntry              `json:"-"`
	InstallReceipt      *skillRecommendationInstallReceipt `json:"-"`
}

type skillRecommendationInstallReceipt struct {
	InstalledAt time.Time                      `json:"installed_at"`
	AgentName   string                         `json:"agent_name,omitempty"`
	Items       []skills.CatalogInstallReceipt `json:"items"`
}
type skillRecommendationItemWireV1 struct {
	CatalogID         string `json:"catalog_id"`
	Slug              string `json:"slug"`
	Source            string `json:"source"`
	DisplayName       string `json:"display_name"`
	CapabilitySummary string `json:"capability_summary"`
}
type skillRecommendationStore struct {
	mu      sync.Mutex
	byID    map[string]*skillRecommendationV1
	byTurn  map[string]string
	path    string
	loadErr error
	cancels map[string]skillRecommendationContinuationCancel
}

type skillRecommendationContinuationCancel struct {
	generation uint64
	cancel     context.CancelFunc
}

func newSkillRecommendationStore(shannonDir string) *skillRecommendationStore {
	path := ""
	if shannonDir != "" {
		path = filepath.Join(shannonDir, "skill-recommendations.json")
	}
	s := &skillRecommendationStore{byID: map[string]*skillRecommendationV1{}, byTurn: map[string]string{}, cancels: map[string]skillRecommendationContinuationCancel{}, path: path}
	if shannonDir != "" {
		s.loadErr = s.load()
	}
	return s
}
func randomRecommendationID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return ""
	}
	return hex.EncodeToString(b)
}
func (s *skillRecommendationStore) offer(account, device, agentName, session, turn, catalogRevision string, items []skillRecommendationItemWireV1, installEntries ...[]skills.CatalogEntry) (skillRecommendationV1, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loadErr != nil {
		return skillRecommendationV1{}, false, fmt.Errorf("recommendation store unavailable: %w", s.loadErr)
	}
	key := account + "\x00" + device + "\x00" + session + "\x00" + turn
	if id := s.byTurn[key]; id != "" {
		return cloneSkillRecommendation(s.byID[id]), false, nil
	}
	id := randomRecommendationID()
	if id == "" {
		return skillRecommendationV1{}, false, fmt.Errorf("generate recommendation ID")
	}
	continuationToken := randomRecommendationID()
	if continuationToken == "" {
		return skillRecommendationV1{}, false, fmt.Errorf("generate continuation token")
	}
	before, beforeTurns := s.snapshotLocked()
	var entries []skills.CatalogEntry
	if len(installEntries) > 0 {
		entries = cloneCatalogEntriesForRecommendation(installEntries[0])
	}
	v := &skillRecommendationV1{SchemaVersion: 1, RecommendationID: id, SessionID: session, TurnID: turn, CatalogRevision: catalogRevision, State: "offered", Items: items, ReasonCode: "task_capability_missing", ExpiresAt: time.Now().Add(skillRecommendationTTL), OwnerAccountID: account, OwnerDeviceID: device, OwnerAgentName: agentName, ContinuationToken: continuationToken, Generation: 1, InstallEntries: entries}
	for _, existing := range s.byID {
		if existing.OwnerAccountID == account && existing.OwnerDeviceID == device && existing.SessionID == session && existing.State == "offered" {
			existing.State = "superseded"
			existing.Generation++
		}
	}
	s.byID[id] = v
	s.byTurn[key] = id
	if err := s.saveLocked(); err != nil {
		s.restoreLocked(before, beforeTurns)
		return skillRecommendationV1{}, false, fmt.Errorf("persist recommendation offer: %w", err)
	}
	return cloneSkillRecommendation(v), true, nil
}
func (s *skillRecommendationStore) dismiss(account, device, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loadErr != nil {
		return fmt.Errorf("recommendation store unavailable: %w", s.loadErr)
	}
	v := s.byID[id]
	if v == nil || v.OwnerAccountID != account || v.OwnerDeviceID != device {
		return fmt.Errorf("recommendation not found")
	}
	if v.State == "offered" {
		previous := v.State
		previousGeneration := v.Generation
		v.State = "dismissed"
		v.Generation++
		if err := s.saveLocked(); err != nil {
			v.State = previous
			v.Generation = previousGeneration
			return fmt.Errorf("persist recommendation dismissal: %w", err)
		}
	}
	return nil
}
func (s *skillRecommendationStore) invalidateAccount(account string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loadErr != nil {
		return fmt.Errorf("recommendation store unavailable: %w", s.loadErr)
	}
	before, beforeTurns := s.snapshotLocked()
	var cancelIDs []string
	for _, v := range s.byID {
		if v.OwnerAccountID == account && v.State != "completed" && v.State != "dismissed" && v.State != "expired" {
			v.State = "expired"
			v.ContinuationRunning = false
			v.Generation++
		}
		if v.OwnerAccountID == account && s.cancels[v.RecommendationID].cancel != nil {
			cancelIDs = append(cancelIDs, v.RecommendationID)
		}
	}
	if err := s.saveLocked(); err != nil {
		s.restoreLocked(before, beforeTurns)
		return fmt.Errorf("persist account invalidation: %w", err)
	}
	for _, id := range cancelIDs {
		owner := s.cancels[id]
		owner.cancel()
		delete(s.cancels, id)
	}
	return nil
}
func (s *skillRecommendationStore) invalidateAll() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loadErr != nil {
		return fmt.Errorf("recommendation store unavailable: %w", s.loadErr)
	}
	before, beforeTurns := s.snapshotLocked()
	var cancelIDs []string
	for _, v := range s.byID {
		if v.State != "completed" && v.State != "dismissed" && v.State != "expired" {
			v.State = "expired"
			v.ContinuationRunning = false
			v.Generation++
		}
		if s.cancels[v.RecommendationID].cancel != nil {
			cancelIDs = append(cancelIDs, v.RecommendationID)
		}
	}
	if err := s.saveLocked(); err != nil {
		s.restoreLocked(before, beforeTurns)
		return fmt.Errorf("persist recommendation kill switch: %w", err)
	}
	for _, id := range cancelIDs {
		owner := s.cancels[id]
		owner.cancel()
		delete(s.cancels, id)
	}
	return nil
}

// failClosedAll is the emergency in-process backstop for an immediate kill
// switch when durable invalidation itself cannot be written. The reloaded
// config remains disabled on disk, so restart still rejects replay/continue;
// this method ensures the current process also cancels every owner and cannot
// serve uncertain recommendation state.
func (s *skillRecommendationStore) failClosedAll(cause error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, v := range s.byID {
		if v.State != "completed" && v.State != "dismissed" && v.State != "expired" {
			v.State = "expired"
			v.ContinuationRunning = false
			v.Generation++
		}
	}
	for id, owner := range s.cancels {
		if owner.cancel != nil {
			owner.cancel()
		}
		delete(s.cancels, id)
	}
	s.loadErr = fmt.Errorf("recommendation protocol disabled after persistence failure: %w", cause)
}

// failClosedAccount is the principal-change counterpart to failClosedAll.
// Durable invalidation failure must never let work for the old account keep
// running after AuthManager has switched identity. The uncertain store is
// disabled process-wide until restart/recovery.
func (s *skillRecommendationStore) failClosedAccount(account string, cause error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, v := range s.byID {
		if v.OwnerAccountID == account && v.State != "completed" && v.State != "dismissed" && v.State != "expired" {
			v.State = "expired"
			v.ContinuationRunning = false
			v.Generation++
		}
		if v.OwnerAccountID == account {
			if owner, ok := s.cancels[v.RecommendationID]; ok {
				if owner.cancel != nil {
					owner.cancel()
				}
				delete(s.cancels, v.RecommendationID)
			}
		}
	}
	s.loadErr = fmt.Errorf("recommendation principal invalidation persistence failure: %w", cause)
}
func (s *skillRecommendationStore) registerContinuation(id string, generation uint64, cancel context.CancelFunc) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := s.byID[id]
	if s.loadErr != nil || v == nil || v.State != "accepted" || !v.ContinuationRunning || v.Generation != generation {
		return false
	}
	s.cancels[id] = skillRecommendationContinuationCancel{generation: generation, cancel: cancel}
	return true
}

func (s *skillRecommendationStore) unregisterContinuation(id string, generation uint64) {
	s.mu.Lock()
	if owner, ok := s.cancels[id]; ok && owner.generation == generation {
		delete(s.cancels, id)
	}
	s.mu.Unlock()
}

// beginContinuation atomically claims the single permitted continuation. A
// repeated request observes its durable terminal/accepted state and never
// starts a second agent run.
func (s *skillRecommendationStore) beginContinuation(account, device, session, id, token string) (skillRecommendationV1, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loadErr != nil {
		return skillRecommendationV1{}, false, fmt.Errorf("recommendation store unavailable: %w", s.loadErr)
	}
	v := s.byID[id]
	if v == nil {
		return skillRecommendationV1{}, false, fmt.Errorf("recommendation not found")
	}
	if v.OwnerAccountID != account || v.OwnerDeviceID != device || v.SessionID != session {
		return skillRecommendationV1{}, false, fmt.Errorf("recommendation owner mismatch")
	}
	if v.ExpiresAt.Before(time.Now()) {
		previous := v.State
		previousGeneration := v.Generation
		v.State = "expired"
		v.Generation++
		if err := s.saveLocked(); err != nil {
			v.State = previous
			v.Generation = previousGeneration
			return skillRecommendationV1{}, false, fmt.Errorf("persist recommendation expiry: %w", err)
		}
		return skillRecommendationV1{}, false, fmt.Errorf("recommendation expired")
	}
	if v.ContinuationToken != token {
		return skillRecommendationV1{}, false, fmt.Errorf("invalid continuation token")
	}
	if v.ContinuationRunning || v.State == "completed" {
		return cloneSkillRecommendation(v), false, nil
	}
	if v.State != "offered" && v.State != "accepted" && v.State != "installation_failed" {
		return skillRecommendationV1{}, false, fmt.Errorf("recommendation is %s", v.State)
	}
	previous := *v
	v.Consumed = true
	v.State = "accepted"
	v.ContinuationRunning = true
	v.Generation++
	if err := s.saveLocked(); err != nil {
		*v = previous
		return skillRecommendationV1{}, false, fmt.Errorf("persist continuation acceptance: %w", err)
	}
	return cloneSkillRecommendation(v), true, nil
}

// recordInstallReceipt durably binds the exact catalog artifacts and Agent
// scope to the already-claimed continuation before the model is resumed. It
// intentionally reuses the continuation generation instead of introducing a
// second installation state machine.
func (s *skillRecommendationStore) recordInstallReceipt(v skillRecommendationV1, receipt skillRecommendationInstallReceipt) (skillRecommendationV1, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loadErr != nil {
		return skillRecommendationV1{}, fmt.Errorf("recommendation store unavailable: %w", s.loadErr)
	}
	current := s.byID[v.RecommendationID]
	if current == nil || current.State != "accepted" || !current.ContinuationRunning || current.Generation != v.Generation {
		return skillRecommendationV1{}, fmt.Errorf("recommendation was invalidated during installation")
	}
	current.InstallReceipt = &receipt
	if err := s.saveLocked(); err != nil {
		current.InstallReceipt = nil
		current.ContinuationRunning = false
		current.State = "expired"
		current.Generation++
		if owner := s.cancels[v.RecommendationID]; owner.cancel != nil {
			owner.cancel()
			delete(s.cancels, v.RecommendationID)
		}
		s.loadErr = fmt.Errorf("persist installation receipt: %w", err)
		return skillRecommendationV1{}, s.loadErr
	}
	return cloneSkillRecommendation(current), nil
}
func (s *skillRecommendationStore) finishContinuation(v skillRecommendationV1, state string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loadErr != nil {
		return fmt.Errorf("recommendation store unavailable: %w", s.loadErr)
	}
	if current := s.byID[v.RecommendationID]; current != nil {
		// An account switch, dismissal, expiry, or a prior terminal completion
		// wins over an old continuation goroutine. Only the active accepted
		// generation is permitted to write its terminal state.
		if current.State != "accepted" || !current.ContinuationRunning || current.Generation != v.Generation {
			return nil
		}
		current.ContinuationRunning = false
		current.State = state
		current.Generation++
		if err := s.saveLocked(); err != nil {
			// The durable state is now uncertain. Keep this process fail-closed:
			// invalidate the in-memory generation, cancel any owner, and reject all
			// future store operations until restart/recovery rather than leaving a
			// permanent accepted+running record that only returns 202.
			current.ContinuationRunning = false
			current.State = "expired"
			current.Generation++
			if owner := s.cancels[v.RecommendationID]; owner.cancel != nil {
				owner.cancel()
				delete(s.cancels, v.RecommendationID)
			}
			s.loadErr = fmt.Errorf("persist continuation state: %w", err)
			return fmt.Errorf("persist continuation state: %w", err)
		}
	}
	return nil
}
func (s *skillRecommendationStore) offeredFor(account, device string) []skillRecommendationV1 {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []skillRecommendationV1{}
	if s.loadErr != nil {
		return out
	}
	for _, v := range s.byID {
		if v.OwnerAccountID == account && v.OwnerDeviceID == device && v.State == "offered" && time.Now().Before(v.ExpiresAt) {
			out = append(out, cloneSkillRecommendation(v))
		}
	}
	return out
}
func (s *skillRecommendationStore) isOffered(id, account, device string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loadErr != nil {
		return false
	}
	v := s.byID[id]
	return v != nil && v.OwnerAccountID == account && v.OwnerDeviceID == device && v.State == "offered" && time.Now().Before(v.ExpiresAt)
}

func cloneSkillRecommendation(v *skillRecommendationV1) skillRecommendationV1 {
	if v == nil {
		return skillRecommendationV1{}
	}
	copy := *v
	copy.Items = append([]skillRecommendationItemWireV1(nil), v.Items...)
	copy.InstallEntries = cloneCatalogEntriesForRecommendation(v.InstallEntries)
	if v.InstallReceipt != nil {
		receipt := *v.InstallReceipt
		receipt.Items = append([]skills.CatalogInstallReceipt(nil), v.InstallReceipt.Items...)
		copy.InstallReceipt = &receipt
	}
	return copy
}

func cloneCatalogEntriesForRecommendation(entries []skills.CatalogEntry) []skills.CatalogEntry {
	out := append([]skills.CatalogEntry(nil), entries...)
	for i := range out {
		out[i].Recommendation.IntentTags = append([]string(nil), entries[i].Recommendation.IntentTags...)
		out[i].Recommendation.Surfaces = append([]string(nil), entries[i].Recommendation.Surfaces...)
	}
	return out
}

func (s *skillRecommendationStore) snapshotLocked() (map[string]skillRecommendationV1, map[string]string) {
	values := make(map[string]skillRecommendationV1, len(s.byID))
	for id, v := range s.byID {
		values[id] = cloneSkillRecommendation(v)
	}
	turns := make(map[string]string, len(s.byTurn))
	for key, id := range s.byTurn {
		turns[key] = id
	}
	return values, turns
}
func (s *skillRecommendationStore) restoreLocked(values map[string]skillRecommendationV1, turns map[string]string) {
	s.byID = make(map[string]*skillRecommendationV1, len(values))
	for id, v := range values {
		copy := cloneSkillRecommendation(&v)
		s.byID[id] = &copy
	}
	s.byTurn = turns
}

type skillRecommendationDiskV1 struct {
	Recommendation      skillRecommendationV1              `json:"recommendation"`
	OwnerAccountID      string                             `json:"owner_account_id"`
	OwnerDeviceID       string                             `json:"owner_device_id"`
	OwnerAgentName      string                             `json:"owner_agent_name,omitempty"`
	Consumed            bool                               `json:"consumed"`
	ContinuationRunning bool                               `json:"continuation_running"`
	Generation          uint64                             `json:"generation"`
	InstallEntries      []skills.CatalogEntry              `json:"install_entries,omitempty"`
	InstallReceipt      *skillRecommendationInstallReceipt `json:"install_receipt,omitempty"`
}

func (s *skillRecommendationStore) load() error {
	b, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var values []skillRecommendationDiskV1
	if err = json.Unmarshal(b, &values); err != nil {
		return err
	}
	for _, disk := range values {
		v := &disk.Recommendation
		v.OwnerAccountID, v.OwnerDeviceID, v.OwnerAgentName = disk.OwnerAccountID, disk.OwnerDeviceID, disk.OwnerAgentName
		v.Consumed = disk.Consumed
		v.InstallEntries = cloneCatalogEntriesForRecommendation(disk.InstallEntries)
		v.InstallReceipt = disk.InstallReceipt
		v.Generation = disk.Generation
		if v.Generation == 0 {
			v.Generation = 1
		}
		// A daemon crash cannot be allowed to leave a recommendation permanently
		// claimed. The next idempotent continue request may safely resume it.
		v.ContinuationRunning = false
		if v.ExpiresAt.Before(time.Now()) && v.State != "completed" && v.State != "dismissed" && v.State != "expired" && v.State != "superseded" {
			v.State = "expired"
			v.Generation++
		}
		s.byID[v.RecommendationID] = v
		s.byTurn[v.OwnerAccountID+"\x00"+v.OwnerDeviceID+"\x00"+v.SessionID+"\x00"+v.TurnID] = v.RecommendationID
	}
	s.pruneLocked()
	return nil
}

// pruneLocked drops terminal records whose TTL has elapsed. Without it the store
// is a pure accumulator: nothing else deletes from byID/byTurn, turn keys embed
// a per-run random turnID so they never collide across runs, and saveLocked
// re-serializes the entire map — including each record's InstallEntries — on
// every offer/dismiss/continue/expire. Both the file and the per-mutation cost
// would grow without bound.
//
// Callers hold s.mu (load() runs in the constructor, before the store is
// shared).
func (s *skillRecommendationStore) pruneLocked() {
	now := time.Now()
	for id, v := range s.byID {
		if skillRecommendationTerminalStates[v.State] && v.ExpiresAt.Before(now) {
			delete(s.byID, id)
		}
	}
	for key, id := range s.byTurn {
		if _, ok := s.byID[id]; !ok {
			delete(s.byTurn, key)
		}
	}
}
func (s *skillRecommendationStore) saveLocked() error {
	if s.path == "" {
		return nil
	}
	s.pruneLocked()
	values := make([]skillRecommendationDiskV1, 0, len(s.byID))
	for _, v := range s.byID {
		values = append(values, skillRecommendationDiskV1{Recommendation: *v, OwnerAccountID: v.OwnerAccountID, OwnerDeviceID: v.OwnerDeviceID, OwnerAgentName: v.OwnerAgentName, Consumed: v.Consumed, ContinuationRunning: v.ContinuationRunning, Generation: v.Generation, InstallEntries: cloneCatalogEntriesForRecommendation(v.InstallEntries), InstallReceipt: v.InstallReceipt})
	}
	b, err := json.Marshal(values)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(s.path), 0700); err != nil {
		return err
	}
	lock, err := os.OpenFile(s.path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err = fslock.Lock(lock.Fd()); err != nil {
		return err
	}
	defer fslock.Unlock(lock.Fd())
	tmp := s.path + ".tmp"
	if err = os.WriteFile(tmp, b, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

type skillRecommendationContextKey struct{}
type skillRecommendationRunContext struct {
	accountID, deviceID, agentName, sessionID, turnID string
	store                                             *skillRecommendationStore
	emit                                              func(skillRecommendationV1) bool
	enabled                                           func() bool
	discovered                                        map[string]bool
	visibleSkills                                     map[string]bool
	catalogRevision                                   string
	sideEffects                                       bool
	mu                                                sync.Mutex
}

func (r *skillRecommendationRunContext) markSideEffect() {
	r.mu.Lock()
	r.sideEffects = true
	r.mu.Unlock()
}
func (r *skillRecommendationRunContext) hasSideEffects() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sideEffects
}

// skillRecommendationEffectHandler owns the single pre-side-effect invariant:
// offers are rejected once an actual non-read-only tool has started. It is a
// handler wrapper rather than a model instruction, so it remains true across
// prompts and retries.
type skillRecommendationEffectHandler struct {
	agent.EventHandler
	registry *agent.ToolRegistry
	run      *skillRecommendationRunContext
}

func (h *skillRecommendationEffectHandler) OnToolCall(name, args, toolUseID string) {
	if name != "discover_installable_skills" && name != "offer_skill_installation" {
		if tool, ok := h.registry.Get(name); !ok {
			h.run.markSideEffect() // unknown execution is never assumed harmless.
		} else if readOnly, ok := tool.(interface{ IsReadOnlyCall(string) bool }); !ok || !readOnly.IsReadOnlyCall(args) {
			h.run.markSideEffect()
		}
	}
	h.EventHandler.OnToolCall(name, args, toolUseID)
}
func (h *skillRecommendationEffectHandler) SetSessionID(id string) {
	if v, ok := h.EventHandler.(interface{ SetSessionID(string) }); ok {
		v.SetSessionID(id)
	}
}
func (h *skillRecommendationEffectHandler) OnRunStatus(code, detail string) {
	if v, ok := h.EventHandler.(agent.RunStatusHandler); ok {
		v.OnRunStatus(code, detail)
	}
}
func (h *skillRecommendationEffectHandler) OnInjectedCommitted(id, text string) {
	if v, ok := h.EventHandler.(agent.InjectCommitHandler); ok {
		v.OnInjectedCommitted(id, text)
	}
}
func (h *skillRecommendationEffectHandler) OnIntermediateAnswer(text, id string) {
	if v, ok := h.EventHandler.(agent.IntermediateAnswerHandler); ok {
		v.OnIntermediateAnswer(text, id)
	}
}
func (h *skillRecommendationEffectHandler) Usage() agent.AccumulatedUsage {
	if v, ok := h.EventHandler.(agent.UsageProvider); ok {
		return v.Usage()
	}
	return agent.AccumulatedUsage{}
}

func withSkillRecommendationRun(ctx context.Context, v *skillRecommendationRunContext) context.Context {
	return context.WithValue(ctx, skillRecommendationContextKey{}, v)
}

type discoverInstallableSkillsTool struct {
	shannonDir string
	catalog    skills.CatalogProvider
}

func (t *discoverInstallableSkillsTool) Info() agent.ToolInfo {
	return agent.ToolInfo{Name: "discover_installable_skills", Description: "Find up to three installable official capabilities for the current Desktop task. Provide concise task_summary and/or stable intent_tags; never guess skill names.", Parameters: map[string]any{"type": "object", "properties": map[string]any{"task_summary": map[string]any{"type": "string"}, "intent_tags": map[string]any{"type": "array", "items": map[string]any{"type": "string"}}}}}
}
func (*discoverInstallableSkillsTool) RequiresApproval() bool     { return false }
func (*discoverInstallableSkillsTool) IsReadOnlyCall(string) bool { return true }
func (*discoverInstallableSkillsTool) AuditSummaries(_ string, result string) (string, string) {
	var candidates []struct {
		CatalogID string `json:"catalog_id"`
	}
	_ = json.Unmarshal([]byte(result), &candidates)
	ids := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.CatalogID != "" {
			ids = append(ids, candidate.CatalogID)
		}
	}
	input, _ := json.Marshal(map[string]string{"reason_code": "task_capability_missing"})
	output, _ := json.Marshal(map[string]any{"catalog_ids": ids})
	return string(input), string(output)
}
func (t *discoverInstallableSkillsTool) Run(ctx context.Context, args string) (agent.ToolResult, error) {
	var in struct {
		TaskSummary string   `json:"task_summary"`
		IntentTags  []string `json:"intent_tags"`
	}
	if json.Unmarshal([]byte(args), &in) != nil || (len(in.IntentTags) == 0 && strings.TrimSpace(in.TaskSummary) == "") {
		return agent.ValidationError("task_summary or intent_tags is required"), nil
	}
	if rc, _ := ctx.Value(skillRecommendationContextKey{}).(*skillRecommendationRunContext); rc != nil && rc.enabled != nil && !rc.enabled() {
		return agent.BusinessError("skill recommendations are disabled"), nil
	}
	var visible map[string]bool
	if rc, _ := ctx.Value(skillRecommendationContextKey{}).(*skillRecommendationRunContext); rc != nil {
		visible = rc.visibleSkills
	}
	entries, revision, err := skills.RecommendationCatalogFrom(ctx, t.catalog, t.shannonDir, "desktop", visible)
	if err != nil {
		return agent.BusinessError(err.Error()), nil
	}
	queryTerms := recommendationQueryTerms(in.TaskSummary)
	type rankedCandidate struct {
		entry skills.CatalogEntry
		score int
	}
	ranked := make([]rankedCandidate, 0, len(entries))
	for _, e := range entries {
		score := recommendationQueryScore(e, queryTerms)
		for _, tag := range in.IntentTags {
			if containsFold(e.Recommendation.IntentTags, tag) {
				score += 100
			}
		}
		if score > 0 {
			ranked = append(ranked, rankedCandidate{entry: e, score: score})
		}
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].score != ranked[j].score {
			return ranked[i].score > ranked[j].score
		}
		return ranked[i].entry.ID < ranked[j].entry.ID
	})
	out := make([]map[string]any, 0, min(3, len(ranked)))
	for _, candidate := range ranked {
		e := candidate.entry
		out = append(out, map[string]any{"catalog_id": e.ID, "display_name": e.DisplayName, "description": e.Description, "intent_tags": e.Recommendation.IntentTags})
		if len(out) == 3 {
			break
		}
	}
	// Bind offer inputs to this run's discovery result. Catalog IDs from arbitrary
	// model text are rejected by offer even if they happen to exist globally.
	if rc, _ := ctx.Value(skillRecommendationContextKey{}).(*skillRecommendationRunContext); rc != nil {
		rc.mu.Lock()
		rc.discovered = map[string]bool{}
		for _, candidate := range out {
			rc.discovered[candidate["catalog_id"].(string)] = true
		}
		rc.catalogRevision = revision
		rc.mu.Unlock()
	}
	b, _ := json.Marshal(out)
	return agent.ToolResult{Content: string(b)}, nil
}
func recommendationQueryTerms(value string) []string {
	fields := strings.Fields(strings.ToLower(value))
	out := make([]string, 0, len(fields))
	for _, field := range fields {
		field = strings.Trim(field, " ,.;:!?()[]{}\"'")
		if len([]rune(field)) >= 3 {
			out = append(out, field)
		}
	}
	return out
}
func recommendationQueryScore(entry skills.CatalogEntry, terms []string) int {
	haystack := strings.ToLower(entry.DisplayName + " " + entry.Description + " " + strings.Join(entry.Recommendation.IntentTags, " "))
	score := 0
	for _, term := range terms {
		if strings.Contains(haystack, term) {
			score++
		}
	}
	return score
}

// offerCardReadyResult is the model-facing tool_result for a delivered card.
// It is a const rather than an inline literal because AuditSummaries classifies
// the offer state by matching it: two copies of the prose let the audit silently
// record "not_offered" the moment one of them was reworded.
const offerCardReadyResult = "A skill installation card is ready. Stop here and wait for the user's choice."

// User-addressed counterparts to this tool's terminal FAILURE results. loop.go
// promotes a terminal result to the run's final answer, so these — not the
// model-facing Content — are what the user reads.
//
// These are persisted prose, not structured i18n keys, so they ship in English
// and clients cannot localize them; that is why the success path emits nothing
// here and lets the already-localized installation card be the only user-facing
// result. Giving terminal failures a structured reason code (the
// EventApprovalNotice {code, message} shape) is the follow-up that would let
// clients localize these too.
const (
	offerDisabledUserMessage      = "Skill recommendations are turned off, so I can't offer to install the skill this task needs."
	offerInactiveUserMessage      = "This skill installation offer is no longer active."
	offerUndeliverableUserMessage = "I couldn't show the skill installation card — the Desktop connection dropped. Nothing was installed; please try again."
	offerStoreErrorUserMessage    = "I couldn't prepare the skill installation card. Nothing was installed; please try again."
)

// terminalOffer pairs a model-facing tool_result with the sentence the user
// actually sees. Every StopAgentLoop return from this tool must go through it —
// otherwise a raw "[business error] ..." string ends the chat as the user's
// final bubble and is persisted into the session transcript.
func terminalOffer(result agent.ToolResult, userMessage string) agent.ToolResult {
	result.StopAgentLoop = true
	result.TerminalUserMessage = userMessage
	return result
}

type offerSkillInstallationTool struct {
	shannonDir string
	catalog    skills.CatalogProvider
}

func (*offerSkillInstallationTool) Info() agent.ToolInfo {
	return agent.ToolInfo{Name: "offer_skill_installation", Description: "Offer only catalog IDs returned by discover_installable_skills. This only shows a Desktop card; it never installs a skill.", Parameters: map[string]any{"type": "object", "properties": map[string]any{"catalog_ids": map[string]any{"type": "array", "items": map[string]any{"type": "string"}}, "reason": map[string]any{"type": "string"}}}, Required: []string{"catalog_ids", "reason"}}
}
func (*offerSkillInstallationTool) RequiresApproval() bool     { return false }
func (*offerSkillInstallationTool) IsReadOnlyCall(string) bool { return false }
func (*offerSkillInstallationTool) StopsAgentLoop() bool       { return true }
func (t *offerSkillInstallationTool) AuditSummaries(args, result string) (string, string) {
	var inputArgs struct {
		CatalogIDs []string `json:"catalog_ids"`
	}
	_ = json.Unmarshal([]byte(args), &inputArgs)
	known := map[string]bool{}
	provider := t.catalog
	if provider == nil {
		provider = skills.NewEmbeddedCatalogProvider(t.shannonDir)
	}
	auditCtx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if entries, _, err := provider.Catalog(auditCtx); err == nil {
		for _, entry := range entries {
			known[entry.ID] = true
		}
	}
	seen := map[string]bool{}
	ids := make([]string, 0, len(inputArgs.CatalogIDs))
	for _, id := range inputArgs.CatalogIDs {
		if known[id] && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	state := "not_offered"
	if strings.Contains(result, offerCardReadyResult) {
		state = "offered"
	}
	input, _ := json.Marshal(map[string]any{"catalog_ids": ids, "reason_code": "task_capability_missing"})
	output, _ := json.Marshal(map[string]string{"state": state})
	return string(input), string(output)
}
func (t *offerSkillInstallationTool) Run(ctx context.Context, args string) (agent.ToolResult, error) {
	rc, _ := ctx.Value(skillRecommendationContextKey{}).(*skillRecommendationRunContext)
	if rc == nil {
		return agent.BusinessError("skill recommendation is unavailable for this consumer"), nil
	}
	if rc.enabled != nil && !rc.enabled() {
		return terminalOffer(agent.BusinessError("skill recommendations are disabled"), offerDisabledUserMessage), nil
	}
	var in struct {
		CatalogIDs []string `json:"catalog_ids"`
		Reason     string   `json:"reason"`
	}
	if json.Unmarshal([]byte(args), &in) != nil || len(in.CatalogIDs) == 0 || sanitizeRecommendationReason(in.Reason) == "" {
		return agent.ValidationError("catalog_ids and reason are required"), nil
	}
	if rc.hasSideEffects() {
		return agent.BusinessError("a skill installation offer must be made before material side effects"), nil
	}
	available, revision, err := skills.RecommendationCatalogFrom(ctx, t.catalog, t.shannonDir, "desktop", rc.visibleSkills)
	if err != nil {
		return agent.BusinessError(err.Error()), nil
	}
	byID := map[string]skills.CatalogEntry{}
	for _, e := range available {
		byID[e.ID] = e
	}
	seen := map[string]bool{}
	items := []skillRecommendationItemWireV1{}
	installEntries := []skills.CatalogEntry{}
	rc.mu.Lock()
	discovered := rc.discovered
	discoveredRevision := rc.catalogRevision
	rc.mu.Unlock()
	for _, id := range in.CatalogIDs {
		if seen[id] {
			continue
		}
		seen[id] = true
		if e, ok := byID[id]; ok && discovered[id] {
			items = append(items, skillRecommendationItemWireV1{e.ID, e.Slug, e.Source, e.DisplayName, e.Description})
			installEntries = append(installEntries, e)
		}
	}
	if len(items) == 0 {
		return agent.ToolResult{Content: "No eligible uninstalled catalog entries remain."}, nil
	}
	if discoveredRevision != revision {
		return agent.BusinessError("catalog changed; rediscover installable skills before offering"), nil
	}
	v, created, err := rc.store.offer(rc.accountID, rc.deviceID, rc.agentName, rc.sessionID, rc.turnID, revision, items, installEntries)
	if err != nil {
		// The raw error is logged, not returned: it is a persistence detail
		// (path, errno, wrapped %w chain) and this string reaches the user.
		log.Printf("daemon: skill recommendation offer failed to persist: %v", err)
		return terminalOffer(agent.BusinessError("persist recommendation offer"), offerStoreErrorUserMessage), nil
	}
	if v.State != "offered" {
		return terminalOffer(agent.BusinessError("the recommendation for this turn is no longer active"), offerInactiveUserMessage), nil
	}
	if created {
		// emit is nil when no /events sink was live at admission time. The tool
		// stays registered regardless (registration keys on stable request
		// attributes so the tools array does not flap across turns), so an
		// undeliverable card fails here rather than by vanishing from the schema.
		if rc.emit == nil || !rc.emit(v) {
			if err := rc.store.expire(v.RecommendationID); err != nil {
				log.Printf("daemon: skill recommendation expire after failed delivery: %v", err)
				return terminalOffer(agent.BusinessError("expire undelivered recommendation"), offerStoreErrorUserMessage), nil
			}
			return terminalOffer(agent.BusinessError("Desktop recommendation stream disconnected; do not continue this task"), offerUndeliverableUserMessage), nil
		}
	}
	// Success is silent: Desktop has already rendered the localized card, so an
	// extra English sentence would be both redundant and untranslatable.
	return agent.ToolResult{Content: offerCardReadyResult, StopAgentLoop: true, TerminalUserSuppressed: true}, nil
}
func containsFold(a []string, b string) bool {
	for _, v := range a {
		if strings.EqualFold(v, b) {
			return true
		}
	}
	return false
}

// The model-supplied reason is not an authority and is deliberately not used
// to choose or render a catalog item. Still validate it as untrusted input so
// a future UI extension cannot accidentally surface control characters.
func sanitizeRecommendationReason(value string) string {
	var b strings.Builder
	for _, r := range value {
		if r >= 0x20 && r != 0x7f {
			b.WriteRune(r)
		}
		if b.Len() >= 280 {
			break
		}
	}
	return strings.TrimSpace(b.String())
}

func skillRecommendationAdmission(r *http.Request, source string) (string, map[string]bool) {
	// Kocoro Desktop currently uses both source="kocoro" (Quick Panel) and
	// source="desktop" (full chat). Capability+device admission, not the
	// spelling alone, identifies the consumer; cloud-distributed sources remain
	// excluded even if they forge the headers.
	if !isSkillRecommendationDesktopSource(source) {
		return "", nil
	}
	device := strings.TrimSpace(r.Header.Get(desktopDeviceHeader))
	if !validOpaqueUUID(device) {
		return "", nil
	}
	caps := map[string]bool{}
	for _, value := range strings.Split(r.Header.Get(skillRecommendationHeader), ",") {
		caps[strings.TrimSpace(value)] = true
	}
	if !caps[CapSkillInstallRecommendationV1] {
		return "", nil
	}
	return device, caps
}

func isSkillRecommendationDesktopSource(source string) bool {
	return source == "desktop" || source == "kocoro"
}
func validOpaqueUUID(v string) bool {
	if len(v) != 36 {
		return false
	}
	for i, c := range v {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
			continue
		}
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

func skillRecommendationSinkKey(accountID, deviceID string) string {
	return accountID + "\x00" + deviceID
}

type skillRecommendationSink struct {
	id    string
	emit  func(skillRecommendationV1) bool
	close func()
}

func (s *Server) registerSkillRecommendationSink(accountID, deviceID string, emit func(skillRecommendationV1) bool, close func()) func() {
	key := skillRecommendationSinkKey(accountID, deviceID)
	sinkID := randomRecommendationID()
	s.skillRecommendationSinksMu.Lock()
	if old, ok := s.skillRecommendationSinks[key]; ok && old.close != nil {
		old.close()
	}
	s.skillRecommendationSinks[key] = skillRecommendationSink{id: sinkID, emit: emit, close: close}
	s.skillRecommendationSinksMu.Unlock()
	return func() {
		s.skillRecommendationSinksMu.Lock()
		if current, ok := s.skillRecommendationSinks[key]; ok && current.id == sinkID {
			delete(s.skillRecommendationSinks, key)
		}
		s.skillRecommendationSinksMu.Unlock()
	}
}
func (s *Server) hasSkillRecommendationSink(accountID, deviceID string) bool {
	s.skillRecommendationSinksMu.RLock()
	defer s.skillRecommendationSinksMu.RUnlock()
	_, ok := s.skillRecommendationSinks[skillRecommendationSinkKey(accountID, deviceID)]
	return ok
}

// skillRecommendationEmitter captures the exact SSE connection generation
// that admitted a /message request. A replacement connection for the same
// account+device is not interchangeable: the old turn must fail delivery
// rather than silently moving its card to a newer subscription.
func (s *Server) skillRecommendationEmitter(accountID, deviceID string) (func(skillRecommendationV1) bool, bool) {
	key := skillRecommendationSinkKey(accountID, deviceID)
	s.skillRecommendationSinksMu.RLock()
	sink, ok := s.skillRecommendationSinks[key]
	s.skillRecommendationSinksMu.RUnlock()
	if !ok || sink.emit == nil {
		return nil, false
	}
	return func(v skillRecommendationV1) bool {
		return s.emitSkillRecommendationGeneration(key, sink.id, v)
	}, true
}

func (s *Server) emitSkillRecommendationGeneration(key, generation string, v skillRecommendationV1) bool {
	if key != skillRecommendationSinkKey(v.OwnerAccountID, v.OwnerDeviceID) {
		return false
	}
	s.skillRecommendationSinksMu.RLock()
	sink, ok := s.skillRecommendationSinks[key]
	s.skillRecommendationSinksMu.RUnlock()
	if !ok || sink.id != generation || sink.emit == nil || v.State != "offered" || !time.Now().Before(v.ExpiresAt) {
		return false
	}
	return sink.emit(v)
}

func (s *Server) closeSkillRecommendationSinks() {
	s.skillRecommendationSinksMu.Lock()
	sinks := make([]skillRecommendationSink, 0, len(s.skillRecommendationSinks))
	for key, sink := range s.skillRecommendationSinks {
		sinks = append(sinks, sink)
		delete(s.skillRecommendationSinks, key)
	}
	s.skillRecommendationSinksMu.Unlock()
	for _, sink := range sinks {
		if sink.close != nil {
			sink.close()
		}
	}
}
func (s *skillRecommendationStore) expire(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loadErr != nil {
		return fmt.Errorf("recommendation store unavailable: %w", s.loadErr)
	}
	if v := s.byID[id]; v != nil && v.State == "offered" {
		previous := v.State
		previousGeneration := v.Generation
		v.State = "expired"
		v.Generation++
		if err := s.saveLocked(); err != nil {
			v.State = previous
			v.Generation = previousGeneration
			return err
		}
	}
	return nil
}
func (s *Server) emitSkillRecommendation(v skillRecommendationV1) bool {
	key := skillRecommendationSinkKey(v.OwnerAccountID, v.OwnerDeviceID)
	s.skillRecommendationSinksMu.RLock()
	sink, ok := s.skillRecommendationSinks[key]
	s.skillRecommendationSinksMu.RUnlock()
	if ok {
		return s.emitSkillRecommendationGeneration(key, sink.id, v)
	}
	return false
}

func (s *Server) installAndEnableRecommendation(ctx context.Context, v skillRecommendationV1) (skillRecommendationInstallReceipt, error) {
	if len(v.InstallEntries) == 0 || len(v.InstallEntries) != len(v.Items) {
		return skillRecommendationInstallReceipt{}, fmt.Errorf("recommendation has no complete installation snapshot")
	}
	entries := cloneCatalogEntriesForRecommendation(v.InstallEntries)
	itemByID := make(map[string]string, len(v.Items))
	for _, item := range v.Items {
		itemByID[item.CatalogID] = item.Slug
	}
	for _, entry := range entries {
		if itemByID[entry.ID] != entry.Slug {
			return skillRecommendationInstallReceipt{}, fmt.Errorf("recommendation installation snapshot mismatch")
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Slug < entries[j].Slug })
	if s.slugLocks == nil {
		s.slugLocks = skills.NewSlugLocks()
	}
	unlocks := make([]func(), 0, len(entries))
	for _, entry := range entries {
		unlocks = append(unlocks, s.slugLocks.Lock(entry.Slug))
	}
	defer func() {
		for i := len(unlocks) - 1; i >= 0; i-- {
			unlocks[i]()
		}
	}()

	receipts := make([]skills.CatalogInstallReceipt, 0, len(entries))
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return skillRecommendationInstallReceipt{}, err
		}
		receipt, err := skills.InstallCatalogEntry(ctx, s.deps.ShannonDir, entry, s.catalog)
		if err != nil {
			return skillRecommendationInstallReceipt{}, err
		}
		receipts = append(receipts, receipt)
	}
	if err := ctx.Err(); err != nil {
		return skillRecommendationInstallReceipt{}, err
	}

	slugs := make([]string, 0, len(entries))
	for _, entry := range entries {
		slugs = append(slugs, entry.Slug)
	}
	if v.OwnerAgentName != "" {
		s.recommendationAgentMu.Lock()
		defer s.recommendationAgentMu.Unlock()
		userAgentDir := filepath.Join(s.deps.AgentsDir, v.OwnerAgentName)
		if _, err := os.Stat(filepath.Join(userAgentDir, "AGENT.md")); os.IsNotExist(err) && agents.IsBuiltinAgent(v.OwnerAgentName) {
			if err := agents.MaterializeBuiltin(s.deps.AgentsDir, v.OwnerAgentName); err != nil {
				return skillRecommendationInstallReceipt{}, fmt.Errorf("materialize agent: %w", err)
			}
		}
		for _, slug := range slugs {
			if err := agents.AttachSkill(s.deps.AgentsDir, v.OwnerAgentName, slug); err != nil {
				return skillRecommendationInstallReceipt{}, fmt.Errorf("attach skill %q to agent %q: %w", slug, v.OwnerAgentName, err)
			}
		}
		loaded, err := agents.LoadAgent(s.deps.AgentsDir, v.OwnerAgentName)
		if err != nil {
			return skillRecommendationInstallReceipt{}, fmt.Errorf("reload agent after skill attachment: %w", err)
		}
		visible := map[string]bool{}
		for _, skill := range loaded.Skills {
			visible[skill.Slug] = true
		}
		for _, slug := range slugs {
			if !visible[slug] {
				return skillRecommendationInstallReceipt{}, fmt.Errorf("installed skill %q is not visible to agent %q", slug, v.OwnerAgentName)
			}
		}
	} else {
		enableTargets := append([]string(nil), slugs...)
		installed, err := skills.LoadSkills(skills.SkillSource{Dir: filepath.Join(s.deps.ShannonDir, "skills"), Source: skills.SourceGlobal})
		if err != nil {
			return skillRecommendationInstallReceipt{}, fmt.Errorf("reload installed skills: %w", err)
		}
		wanted := make(map[string]bool, len(slugs))
		for _, slug := range slugs {
			wanted[slug] = true
		}
		for _, installedSkill := range installed {
			if wanted[installedSkill.Slug] && installedSkill.Name != installedSkill.Slug {
				enableTargets = append(enableTargets, installedSkill.Name)
			}
		}
		if err := config.RemoveGlobalDisabledSkills(s.deps.ShannonDir, enableTargets); err != nil {
			return skillRecommendationInstallReceipt{}, fmt.Errorf("enable installed skill for default agent: %w", err)
		}
		rm := make(map[string]bool, len(enableTargets))
		for _, target := range enableTargets {
			rm[target] = true
		}
		s.deps.WriteLock()
		if s.deps.Config != nil {
			filtered := s.deps.Config.Skills.Disabled[:0]
			for _, disabled := range s.deps.Config.Skills.Disabled {
				if !rm[disabled] {
					filtered = append(filtered, disabled)
				}
			}
			s.deps.Config.Skills.Disabled = filtered
		}
		s.deps.WriteUnlock()
	}
	return skillRecommendationInstallReceipt{InstalledAt: time.Now().UTC(), AgentName: v.OwnerAgentName, Items: receipts}, nil
}

func (s *Server) handleSkillRecommendationContinue(w http.ResponseWriter, r *http.Request) {
	if !s.skillRecommendationsEnabled() || s.deps == nil || s.auth == nil || s.skillRecommendations == nil {
		writeError(w, http.StatusNotFound, "skill recommendations unavailable")
		return
	}
	device, caps := skillRecommendationAdmission(r, "desktop")
	account, ok := s.auth.VerifiedAccountID()
	if device == "" || !caps[CapSkillInstallRecommendationV1] || !ok {
		writeError(w, http.StatusForbidden, "skill recommendations unavailable for this consumer")
		return
	}
	var body struct {
		SessionID         string `json:"session_id"`
		ContinuationToken string `json:"continuation_token"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if body.SessionID == "" || body.ContinuationToken == "" {
		writeError(w, http.StatusBadRequest, "session_id and continuation_token are required")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusNotAcceptable, "continuation requires an SSE-capable Desktop connection")
		return
	}
	v, shouldRun, err := s.skillRecommendations.beginContinuation(account, device, body.SessionID, r.PathValue("id"), body.ContinuationToken)
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	if v.State == "completed" {
		writeJSON(w, http.StatusOK, map[string]string{"status": "completed"})
		return
	}
	if !shouldRun {
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "accepted"})
		return
	}
	continuationCtx, cancel := context.WithCancel(r.Context())
	if !s.skillRecommendations.registerContinuation(v.RecommendationID, v.Generation, cancel) {
		cancel()
		writeError(w, http.StatusConflict, "recommendation was invalidated")
		return
	}
	defer func() { s.skillRecommendations.unregisterContinuation(v.RecommendationID, v.Generation); cancel() }()

	// The single Desktop action owns install + Agent enablement + continuation.
	// Installation uses the immutable entries captured by offer; it never
	// re-reads a same-slug entry from the current catalog.
	receipt, installErr := s.installAndEnableRecommendation(continuationCtx, v)
	if installErr != nil {
		if persistErr := s.skillRecommendations.finishContinuation(v, "installation_failed"); persistErr != nil {
			writeError(w, http.StatusInternalServerError, persistErr.Error())
			return
		}
		writeError(w, http.StatusConflict, installErr.Error())
		return
	}
	v, err = s.skillRecommendations.recordInstallReceipt(v, receipt)
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	parts := make([]string, 0, len(v.Items))
	for _, item := range v.Items {
		parts = append(parts, item.DisplayName)
	}
	followup := "User enabled " + strings.Join(parts, ", ") + ". Continue only from the previously waiting capability-dependent step; do not repeat completed work or the original request."
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher.Flush()
	// Continuation retains ordinary attended Desktop approval semantics. It is
	// not an implicit grant for whatever consequential work follows install.
	broker := NewApprovalBroker(newSSEApprovalSendFn(w, flusher))
	broker.onRequest = s.approvalBroker.onRequest
	broker.onCleanup = s.approvalBroker.onCleanup
	broker.onAutoApprove = s.approvalBroker.onAutoApprove
	broker.onRegister = func(requestID string) { s.pendingBrokers.Store(requestID, broker) }
	broker.onDeregister = func(requestID string) { s.pendingBrokers.Delete(requestID) }
	defer broker.CancelAll()
	cfg, _, _ := s.deps.Snapshot()
	autoApprove := cfg != nil && cfg.Daemon.AutoApprove
	if v.OwnerAgentName != "" {
		if named, err := agents.LoadAgent(s.deps.AgentsDir, v.OwnerAgentName); err == nil && named.Config != nil && named.Config.AutoApprove != nil {
			autoApprove = *named.Config.AutoApprove
		}
	}
	handler := &sseEventHandler{w: w, flusher: flusher, broker: broker, ctx: r.Context(), autoApprove: autoApprove, deps: s.deps, agent: v.OwnerAgentName, source: "desktop"}
	result, err := RunAgent(continuationCtx, s.deps, skillRecommendationContinuationRequest(v, followup), handler)
	if err == nil && continuationCtx.Err() != nil {
		// A transport disconnect is not a completed continuation even if the
		// agent loop returned a partial/empty result while unwinding cancellation.
		// The stable idempotency key prevents a later retry from replaying any
		// consequential work that may already have crossed its commit boundary.
		err = continuationCtx.Err()
	}
	if err != nil {
		if persistErr := s.skillRecommendations.finishContinuation(v, "installation_failed"); persistErr != nil {
			fmt.Fprintf(w, "event: error\ndata: %s\n\n", mustJSON(map[string]string{"error": persistErr.Error()}))
			flusher.Flush()
			return
		}
		fmt.Fprintf(w, "event: error\ndata: %s\n\n", mustJSON(map[string]string{"error": err.Error()}))
		flusher.Flush()
		return
	}
	if err := s.skillRecommendations.finishContinuation(v, "completed"); err != nil {
		fmt.Fprintf(w, "event: error\ndata: %s\n\n", mustJSON(map[string]string{"error": err.Error()}))
		flusher.Flush()
		return
	}
	fmt.Fprintf(w, "event: done\ndata: %s\n\n", mustJSON(result))
	flusher.Flush()
}
func skillRecommendationContinuationRequest(v skillRecommendationV1, followup string) RunAgentRequest {
	// followup is daemon-authored ("User enabled ..."), not something the user
	// typed — SystemInjected keeps it out of displayed history, share exports and
	// the session index. Without it the user sees a bubble they never wrote.
	return RunAgentRequest{Text: followup, SessionID: v.SessionID, Agent: v.OwnerAgentName, Source: "desktop", SystemInjected: true, IdempotencyKey: "skillrec:" + v.RecommendationID}
}
func (s *Server) handleSkillRecommendationDismiss(w http.ResponseWriter, r *http.Request) {
	if !s.skillRecommendationsEnabled() || s.auth == nil || s.skillRecommendations == nil {
		writeError(w, http.StatusNotFound, "skill recommendations unavailable")
		return
	}
	device, caps := skillRecommendationAdmission(r, "desktop")
	account, ok := s.auth.VerifiedAccountID()
	if device == "" || !caps[CapSkillInstallRecommendationV1] || !ok {
		writeError(w, http.StatusForbidden, "skill recommendations unavailable for this consumer")
		return
	}
	if err := s.skillRecommendations.dismiss(account, device, r.PathValue("id")); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "dismissed"})
}
