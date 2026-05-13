package agent

import (
	"sync"
	"time"
)

// Suggestion is a single per-session suggestion record.
type Suggestion struct {
	// Text is the filtered suggestion text returned to Desktop.
	Text string
	// SuggestedAt is the time GenerateSuggestion returned successfully.
	SuggestedAt time.Time
	// AcceptedAt is non-nil if the user accepted via POST /suggestion/accept.
	AcceptedAt *time.Time
}

// SuggestionState holds the latest prompt suggestion per session.
// Cleared on new turn start (suggestion_handler.OnTurnStart) and on session
// close so a stale suggestion never resurfaces against a different conversation
// state.
//
// Concurrent-safe — all mutating methods take the write lock, all reads use the
// read lock. Get returns a copy of the Suggestion struct so callers cannot
// mutate internal state via the returned pointer.
//
// Generation tokens: Clear bumps a per-session counter. Callers that dispatch
// async writes (the suggestion goroutine in daemon.RunAgent) capture the
// generation BEFORE starting the gateway call, then pass it to SetIfFresh.
// A Clear fired between capture and Set drops the write — without this,
// a slow goroutine completing AFTER a new turn started would resurrect a
// stale suggestion that the user no longer sees in their UI.
type SuggestionState struct {
	mu    sync.RWMutex
	items map[string]*Suggestion // key: session id
	gens  map[string]int         // session id → generation; bumped by Clear
}

// NewSuggestionState returns an empty state container.
func NewSuggestionState() *SuggestionState {
	return &SuggestionState{
		items: make(map[string]*Suggestion),
		gens:  make(map[string]int),
	}
}

// Set stores a new suggestion for sessionID, overwriting any prior entry.
// Test-only: production paths always use SetIfFresh so the generation token
// check protects against stale-goroutine resurrect. Kept exported because
// unit tests in sibling packages (internal/daemon, test/e2e) need to seed
// state without a paired CurrentGen()/SetIfFresh() dance. New production
// callers MUST use SetIfFresh — reviewers should treat any new call to Set
// outside _test.go files as a bug.
func (s *SuggestionState) Set(sessionID, text string, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items[sessionID] = &Suggestion{
		Text:        text,
		SuggestedAt: at,
	}
}

// Get returns a snapshot of the current suggestion for sessionID, or
// (nil, false) if none. The returned *Suggestion is a fresh copy — mutating
// it does not affect the internal state.
func (s *SuggestionState) Get(sessionID string) (*Suggestion, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.items[sessionID]
	if !ok {
		return nil, false
	}
	cp := *v
	return &cp, true
}

// Clear removes any suggestion for sessionID and bumps the generation token.
// Called on new turn start. The generation bump means any in-flight goroutine
// that captured an earlier gen via CurrentGen will have its SetIfFresh call
// dropped — preventing stale-suggestion resurrect.
//
// Use Clear at turn-start boundaries (mid-session lifecycle). Use Forget when
// the session itself is gone — Forget removes the gens entry too so the map
// does not grow unboundedly across millions of long-running sessions.
func (s *SuggestionState) Clear(sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.items, sessionID)
	s.gens[sessionID]++
}

// Forget removes all state for sessionID — both the active suggestion and the
// generation counter. Should be called from SessionManager.OnSessionClose
// after the manager confirms no in-flight goroutines are still racing on this
// sessionID. After Forget, a brand-new suggestion goroutine starting on the
// same sessionID begins at gen=0 (default) — the same starting point a fresh
// session would use, so the race-protection guarantee still holds even if a
// sessionID is reused after Forget.
//
// Why this exists separately from Clear: Clear bumps gens but never deletes
// the entry, so gens[] grows by one int per ever-touched session over the
// daemon's lifetime. With millions of sessions over a year this is a slow
// memory leak; Forget is the proper drop hook tied to session lifetime.
func (s *SuggestionState) Forget(sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.items, sessionID)
	delete(s.gens, sessionID)
}

// CurrentGen returns the current generation token for sessionID. Capture
// this BEFORE starting an async write (e.g., before the forked suggestion
// gateway call), then pass it to SetIfFresh — if a Clear fired in between,
// SetIfFresh drops the write so a stale goroutine cannot revive a
// suggestion the user has already moved past.
func (s *SuggestionState) CurrentGen(sessionID string) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.gens[sessionID]
}

// SetIfFresh stores a new suggestion only if the per-session generation
// token matches observedGen. Returns true if stored, false if dropped as
// stale (Clear fired since observedGen was captured).
//
// Used by daemon.fireSuggestionAfterRun so a slow goroutine completing
// after a Clear (new turn / session close) cannot resurrect an entry
// the user has already moved past.
func (s *SuggestionState) SetIfFresh(sessionID string, observedGen int, text string, at time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.gens[sessionID] != observedGen {
		return false
	}
	s.items[sessionID] = &Suggestion{
		Text:        text,
		SuggestedAt: at,
	}
	return true
}

// MarkAccepted records that the user accepted the current suggestion for
// sessionID. Returns false if no current suggestion exists for the session.
func (s *SuggestionState) MarkAccepted(sessionID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.items[sessionID]
	if !ok {
		return false
	}
	now := time.Now()
	v.AcceptedAt = &now
	return true
}

