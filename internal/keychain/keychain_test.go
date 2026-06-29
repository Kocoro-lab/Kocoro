package keychain

import (
	"errors"
	"testing"
)

// TestSupportedMatchesBuildTag enforces the invariant that the runtime
// Supported() predicate (hand-maintained as darwin || windows in keychain.go)
// stays in lockstep with the build tags selecting the real NewOSStore
// (backend_keyring.go) vs the ErrUnsupportedPlatform stub (backend_other.go).
// Without this, adding a platform to one and forgetting the other would only
// surface at runtime as a spurious ErrUnsupportedPlatform. This is build-tag
// agnostic, so it runs on every CI job (Linux + macOS) — no Windows runner
// needed. NewOSStore only constructs the backend; it never touches the real
// credential store, so there is no GUI prompt to hang CI.
func TestSupportedMatchesBuildTag(t *testing.T) {
	store, err := NewOSStore(nil)
	if Supported() != (err == nil) {
		t.Fatalf("Supported()=%v but NewOSStore err=%v — predicate and build tags out of sync", Supported(), err)
	}
	if Supported() {
		if store == nil {
			t.Fatal("supported platform: NewOSStore returned nil store with nil error")
		}
	} else {
		if !errors.Is(err, ErrUnsupportedPlatform) {
			t.Fatalf("unsupported platform: expected ErrUnsupportedPlatform, got %v", err)
		}
	}
}

func newTestStore() (*Store, *MemBackend) {
	be := NewMemBackend()
	return NewStore(be, nil), be
}

func TestStore_GetAPIKey_Empty(t *testing.T) {
	s, _ := newTestStore()
	if k, err := s.GetAPIKey(); err != nil || k != "" {
		t.Fatalf("expected empty without active user, got key=%q err=%v", k, err)
	}
	if u, err := s.CurrentUserID(); err != nil || u != "" {
		t.Fatalf("expected empty active user, got %q err=%v", u, err)
	}
}

func TestStore_SetAPIKey_RoundTrip(t *testing.T) {
	s, _ := newTestStore()
	if err := s.SetAPIKey("user-1", "sk_test"); err != nil {
		t.Fatalf("SetAPIKey: %v", err)
	}
	u, k, err := s.GetActiveUserAndKey()
	if err != nil {
		t.Fatalf("GetActiveUserAndKey: %v", err)
	}
	if u != "user-1" || k != "sk_test" {
		t.Fatalf("got user=%q key=%q", u, k)
	}
}

func TestStore_SetAPIKey_Validation(t *testing.T) {
	s, _ := newTestStore()
	if err := s.SetAPIKey("", "key"); err == nil {
		t.Fatal("empty userID should error")
	}
	if err := s.SetAPIKey("u", ""); err == nil {
		t.Fatal("empty key should error")
	}
}

func TestStore_DeleteAPIKey_PreservesUser(t *testing.T) {
	s, _ := newTestStore()
	_ = s.SetAPIKey("user-1", "sk_test")
	if err := s.DeleteAPIKey(); err != nil {
		t.Fatalf("DeleteAPIKey: %v", err)
	}
	u, err := s.CurrentUserID()
	if err != nil {
		t.Fatalf("CurrentUserID: %v", err)
	}
	if u != "user-1" {
		t.Fatalf("expected current_user_id preserved, got %q", u)
	}
	if k, _ := s.GetAPIKey(); k != "" {
		t.Fatalf("expected api_key gone, got %q", k)
	}
}

func TestStore_ClearActiveUser(t *testing.T) {
	s, _ := newTestStore()
	_ = s.SetAPIKey("user-1", "sk_test")
	if err := s.ClearActiveUser(); err != nil {
		t.Fatalf("ClearActiveUser: %v", err)
	}
	if u, _ := s.CurrentUserID(); u != "" {
		t.Fatalf("expected empty active user, got %q", u)
	}
	// Key entry per-user remains, but GetAPIKey returns "" because no active user.
	if k, _ := s.GetAPIKey(); k != "" {
		t.Fatalf("expected empty GetAPIKey, got %q", k)
	}
}

func TestStore_DeleteIdempotent(t *testing.T) {
	s, _ := newTestStore()
	// Delete non-existent: should not error.
	if err := s.DeleteAPIKey(); err != nil {
		t.Fatalf("DeleteAPIKey on empty: %v", err)
	}
	if err := s.ClearActiveUser(); err != nil {
		t.Fatalf("ClearActiveUser on empty: %v", err)
	}
	if err := s.Delete(ServiceDaemonAPIKey, "nonexistent"); err != nil {
		t.Fatalf("Delete unknown account: %v", err)
	}
}

func TestStore_RenameLegacy(t *testing.T) {
	s, be := newTestStore()
	// Simulate migration: legacy entry exists + current_user_id points at "legacy".
	_ = s.Write(ServiceDaemonAPIKey, AccountLegacy, "sk_legacy_value")
	_ = s.Write(ServiceDaemonState, AccountCurrentUser, AccountLegacy)

	if err := s.RenameLegacy("real-user-uuid"); err != nil {
		t.Fatalf("RenameLegacy: %v", err)
	}
	u, k, err := s.GetActiveUserAndKey()
	if err != nil {
		t.Fatalf("GetActiveUserAndKey: %v", err)
	}
	if u != "real-user-uuid" || k != "sk_legacy_value" {
		t.Fatalf("post-rename got user=%q key=%q", u, k)
	}
	// Legacy entry should be gone.
	snap := be.Snapshot()
	if _, ok := snap[memKey(ServiceDaemonAPIKey, AccountLegacy)]; ok {
		t.Fatal("legacy entry should be deleted after RenameLegacy")
	}
}

func TestStore_RenameLegacy_NoLegacyEntry(t *testing.T) {
	s, _ := newTestStore()
	// No legacy entry — RenameLegacy is a no-op success.
	if err := s.RenameLegacy("real-user-uuid"); err != nil {
		t.Fatalf("RenameLegacy on empty: %v", err)
	}
	if u, _ := s.CurrentUserID(); u != "" {
		t.Fatalf("RenameLegacy must not create state when no legacy entry, got user=%q", u)
	}
}

func TestStore_RenameLegacy_ValidatesUserID(t *testing.T) {
	s, _ := newTestStore()
	if err := s.RenameLegacy(""); err == nil {
		t.Fatal("empty realUserID should error")
	}
}

func TestStore_NilStore(t *testing.T) {
	var s *Store
	if _, err := s.GetAPIKey(); err == nil {
		t.Fatal("nil Store GetAPIKey should error")
	}
	if err := s.SetAPIKey("u", "k"); err == nil {
		t.Fatal("nil Store SetAPIKey should error")
	}
}

func TestStore_NilBackend(t *testing.T) {
	s := NewStore(nil, nil)
	if _, err := s.Read("s", "a"); err == nil {
		t.Fatal("nil backend Read should error")
	}
}

func TestStore_GetAPIKey_ActiveUserNoKey(t *testing.T) {
	s, _ := newTestStore()
	// current_user_id set but no key for that user (corrupt state).
	_ = s.Write(ServiceDaemonState, AccountCurrentUser, "orphan-user")
	k, err := s.GetAPIKey()
	if err != nil {
		t.Fatalf("GetAPIKey should not error on orphan, got %v", err)
	}
	if k != "" {
		t.Fatalf("expected empty key for orphan user, got %q", k)
	}
}
