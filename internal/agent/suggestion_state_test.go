package agent

import (
	"testing"
	"time"
)

func TestSuggestionState_SetAndGet(t *testing.T) {
	s := NewSuggestionState()
	s.Set("sess1", "fix the bug", time.Now())

	got, ok := s.Get("sess1")
	if !ok {
		t.Fatal("expected suggestion for sess1")
	}
	if got.Text != "fix the bug" {
		t.Errorf("Text = %q, want %q", got.Text, "fix the bug")
	}
	if got.AcceptedAt != nil {
		t.Error("AcceptedAt should be nil initially")
	}
}

func TestSuggestionState_Clear(t *testing.T) {
	s := NewSuggestionState()
	s.Set("sess1", "fix the bug", time.Now())
	s.Clear("sess1")

	if _, ok := s.Get("sess1"); ok {
		t.Error("expected sess1 to be cleared")
	}
}

func TestSuggestionState_MarkAccepted(t *testing.T) {
	s := NewSuggestionState()
	s.Set("sess1", "fix the bug", time.Now())
	if !s.MarkAccepted("sess1") {
		t.Fatal("MarkAccepted returned false on existing session")
	}
	got, _ := s.Get("sess1")
	if got.AcceptedAt == nil {
		t.Error("AcceptedAt should be set after MarkAccepted")
	}
}

func TestSuggestionState_MarkAccepted_AbsentSession(t *testing.T) {
	s := NewSuggestionState()
	if s.MarkAccepted("no-such") {
		t.Error("MarkAccepted should return false for unknown session")
	}
}

func TestSuggestionState_GetReturnsCopy(t *testing.T) {
	// Get must return a copy so callers can't mutate internal state via the pointer.
	s := NewSuggestionState()
	s.Set("sess1", "original", time.Now())
	got, _ := s.Get("sess1")
	got.Text = "mutated"

	again, _ := s.Get("sess1")
	if again.Text != "original" {
		t.Errorf("Get returned a live reference: external mutation visible (%q)", again.Text)
	}
}

func TestSuggestionState_ConcurrentSafe(t *testing.T) {
	// Smoke test for the sync.RWMutex — must not race under -race detector.
	s := NewSuggestionState()
	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			s.Set("sess1", "x", time.Now())
		}
		done <- struct{}{}
	}()
	go func() {
		for i := 0; i < 1000; i++ {
			_, _ = s.Get("sess1")
		}
		done <- struct{}{}
	}()
	<-done
	<-done
}

func TestSuggestionState_SetIfFresh_DropsStaleAfterClear(t *testing.T) {
	// GPT review P0 finding #2: a goroutine started before Clear must NOT
	// resurrect a suggestion via a late Set. SetIfFresh checks the
	// generation captured before the work started.
	s := NewSuggestionState()

	// Goroutine captures gen=0
	observedGen := s.CurrentGen("sess1")

	// Concurrent turn-start clears + bumps gen
	s.Clear("sess1")

	// Goroutine completes its gateway call and tries to Set with the
	// pre-Clear generation — must be dropped.
	if ok := s.SetIfFresh("sess1", observedGen, "stale text", time.Now()); ok {
		t.Error("SetIfFresh should reject when generation was bumped by Clear")
	}
	if _, ok := s.Get("sess1"); ok {
		t.Error("entry must not exist after stale SetIfFresh was dropped")
	}
}

func TestSuggestionState_SetIfFresh_StoresWhenGenMatches(t *testing.T) {
	s := NewSuggestionState()
	gen := s.CurrentGen("sess1")

	if ok := s.SetIfFresh("sess1", gen, "fresh", time.Now()); !ok {
		t.Fatal("SetIfFresh should succeed when generation matches")
	}
	got, ok := s.Get("sess1")
	if !ok || got.Text != "fresh" {
		t.Errorf("expected entry Text='fresh', got %+v ok=%v", got, ok)
	}
}

func TestSuggestionState_CurrentGen_BumpsOnEveryClear(t *testing.T) {
	s := NewSuggestionState()
	if g := s.CurrentGen("sess1"); g != 0 {
		t.Errorf("initial gen = %d, want 0", g)
	}
	s.Clear("sess1")
	if g := s.CurrentGen("sess1"); g != 1 {
		t.Errorf("after first Clear gen = %d, want 1", g)
	}
	s.Clear("sess1")
	if g := s.CurrentGen("sess1"); g != 2 {
		t.Errorf("after second Clear gen = %d, want 2", g)
	}
	// Set via SetIfFresh with current gen still works
	if ok := s.SetIfFresh("sess1", 2, "x", time.Now()); !ok {
		t.Error("SetIfFresh with current gen=2 must succeed")
	}
	// Clear after Set bumps to 3
	s.Clear("sess1")
	if g := s.CurrentGen("sess1"); g != 3 {
		t.Errorf("after Clear-after-Set gen = %d, want 3", g)
	}
}

func TestSuggestionState_CurrentGen_PerSessionIndependent(t *testing.T) {
	s := NewSuggestionState()
	s.Clear("sess1")
	s.Clear("sess1")
	// sess1 gen=2, sess2 still 0
	if g := s.CurrentGen("sess1"); g != 2 {
		t.Errorf("sess1 gen = %d, want 2", g)
	}
	if g := s.CurrentGen("sess2"); g != 0 {
		t.Errorf("sess2 gen = %d, want 0 (untouched by sess1 Clears)", g)
	}
}
