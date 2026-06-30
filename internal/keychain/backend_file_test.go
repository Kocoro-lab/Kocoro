package keychain

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// These tests construct the file backend directly against t.TempDir() so they
// run on every CI platform (macOS/Linux), independent of the //go:build linux
// tag on NewOSStoreAt.

func newTestFileBackend(t *testing.T) (Backend, string) {
	t.Helper()
	dir := t.TempDir()
	be, err := newFileBackend(dir)
	if err != nil {
		t.Fatalf("newFileBackend: %v", err)
	}
	return be, dir
}

func TestFileBackend_RoundTrip(t *testing.T) {
	be, _ := newTestFileBackend(t)
	// Two services, multiple accounts — exercises the nested-map namespacing.
	if err := be.Write(ServiceDaemonAPIKey, "user-1", "sk_one"); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := be.Write(ServiceDaemonAPIKey, "user-2", "sk_two"); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := be.Write(ServiceDaemonState, AccountCurrentUser, "user-2"); err != nil {
		t.Fatalf("Write: %v", err)
	}
	for _, tc := range []struct{ svc, acc, want string }{
		{ServiceDaemonAPIKey, "user-1", "sk_one"},
		{ServiceDaemonAPIKey, "user-2", "sk_two"},
		{ServiceDaemonState, AccountCurrentUser, "user-2"},
	} {
		got, err := be.Read(tc.svc, tc.acc)
		if err != nil || got != tc.want {
			t.Fatalf("Read(%s,%s)=%q err=%v, want %q", tc.svc, tc.acc, got, err, tc.want)
		}
	}
	// Overwrite is in place.
	if err := be.Write(ServiceDaemonAPIKey, "user-1", "sk_one_v2"); err != nil {
		t.Fatalf("Write overwrite: %v", err)
	}
	if got, _ := be.Read(ServiceDaemonAPIKey, "user-1"); got != "sk_one_v2" {
		t.Fatalf("after overwrite got %q", got)
	}
}

func TestFileBackend_ReadAbsent_ErrNotFound(t *testing.T) {
	be, _ := newTestFileBackend(t)
	// Absent service and absent account both → ErrNotFound from the backend.
	if _, err := be.Read(ServiceDaemonAPIKey, "nobody"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("absent account: want ErrNotFound, got %v", err)
	}
	_ = be.Write(ServiceDaemonAPIKey, "user-1", "sk")
	if _, err := be.Read(ServiceDaemonState, "nobody"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("absent service: want ErrNotFound, got %v", err)
	}
	// Through the Store wrapper, ErrNotFound is mapped to ("", nil).
	s := NewStore(be, nil)
	if v, err := s.Read(ServiceDaemonAPIKey, "nobody"); err != nil || v != "" {
		t.Fatalf("Store.Read absent: got %q err=%v, want empty/no-error", v, err)
	}
}

func TestFileBackend_DeleteIdempotent(t *testing.T) {
	be, _ := newTestFileBackend(t)
	// Delete absent → ErrNotFound from backend; idempotent (nil) via Store.
	if err := be.Delete(ServiceDaemonAPIKey, "nobody"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete absent: want ErrNotFound, got %v", err)
	}
	s := NewStore(be, nil)
	if err := s.Delete(ServiceDaemonAPIKey, "nobody"); err != nil {
		t.Fatalf("Store.Delete absent should be idempotent: %v", err)
	}
	_ = be.Write(ServiceDaemonAPIKey, "user-1", "sk")
	if err := be.Delete(ServiceDaemonAPIKey, "user-1"); err != nil {
		t.Fatalf("Delete present: %v", err)
	}
	if _, err := be.Read(ServiceDaemonAPIKey, "user-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after delete: want ErrNotFound, got %v", err)
	}
}

func TestFileBackend_FileMode0600(t *testing.T) {
	be, dir := newTestFileBackend(t)
	if err := be.Write(ServiceDaemonAPIKey, "user-1", "sk"); err != nil {
		t.Fatalf("Write: %v", err)
	}
	info, err := os.Stat(filepath.Join(dir, "credentials.json"))
	if err != nil {
		t.Fatalf("stat credentials.json: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("credentials.json mode = %o, want 0600", perm)
	}
}

func TestFileBackend_NoLeftoverTempFiles(t *testing.T) {
	be, dir := newTestFileBackend(t)
	for i := 0; i < 5; i++ {
		if err := be.Write(ServiceDaemonAPIKey, "user-1", "sk"); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		name := e.Name()
		if len(name) >= 4 && name[len(name)-4:] == ".tmp" {
			t.Fatalf("leftover temp file after atomic write: %s", name)
		}
	}
}

func TestFileBackend_CorruptFileSurfacesError(t *testing.T) {
	be, dir := newTestFileBackend(t)
	// A present-but-unparseable file must surface a read error (NOT be silently
	// treated as an empty store) — otherwise, after the yaml key was migrated
	// away, a corrupt store would look like "no credential" and the daemon
	// would start silently unauthenticated. macOS/Windows backends surface
	// read errors; the file backend must match.
	if err := os.WriteFile(filepath.Join(dir, "credentials.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatalf("seed corrupt file: %v", err)
	}
	if _, err := be.Read(ServiceDaemonAPIKey, "user-1"); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("corrupt file Read: want a surfaced parse error, got %v", err)
	}
	// Write also loads first, so it refuses to clobber a corrupt file — the
	// user recovers from the .pre-migrate .bak rather than losing other entries.
	if err := be.Write(ServiceDaemonAPIKey, "user-1", "sk"); err == nil {
		t.Fatalf("Write over corrupt file: want error, got nil")
	}

	// A genuinely empty file (e.g. a 0-byte artifact) is still a fresh store.
	if err := os.WriteFile(filepath.Join(dir, "credentials.json"), []byte(""), 0o600); err != nil {
		t.Fatalf("seed empty file: %v", err)
	}
	if _, err := be.Read(ServiceDaemonAPIKey, "user-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("empty file Read: want ErrNotFound, got %v", err)
	}
}

func TestFileBackend_CrossInstanceSharesState(t *testing.T) {
	// Two backends over the same dir (simulates two `shan` processes). Writes
	// from one are visible to the other; fslock serializes them.
	dir := t.TempDir()
	a, err := newFileBackend(dir)
	if err != nil {
		t.Fatalf("newFileBackend a: %v", err)
	}
	b, err := newFileBackend(dir)
	if err != nil {
		t.Fatalf("newFileBackend b: %v", err)
	}
	if err := a.Write(ServiceDaemonAPIKey, "user-1", "sk_shared"); err != nil {
		t.Fatalf("a.Write: %v", err)
	}
	if got, err := b.Read(ServiceDaemonAPIKey, "user-1"); err != nil || got != "sk_shared" {
		t.Fatalf("b.Read sees a's write: got %q err=%v", got, err)
	}
}

// TestFileBackend_StoreFlow runs the full Store API (SetAPIKey / RenameLegacy /
// DeleteAPIKey / ClearActiveUser) against the file backend, mirroring the
// MemBackend-based suite to confirm the durable backend behaves identically.
func TestFileBackend_StoreFlow(t *testing.T) {
	be, _ := newTestFileBackend(t)
	s := NewStore(be, nil)

	// Simulate the yaml→store migration: legacy entry + current_user_id=legacy.
	if err := s.Write(ServiceDaemonAPIKey, AccountLegacy, "sk_legacy"); err != nil {
		t.Fatalf("write legacy: %v", err)
	}
	if err := s.Write(ServiceDaemonState, AccountCurrentUser, AccountLegacy); err != nil {
		t.Fatalf("write current_user_id: %v", err)
	}
	if err := s.RenameLegacy("real-user"); err != nil {
		t.Fatalf("RenameLegacy: %v", err)
	}
	u, k, err := s.GetActiveUserAndKey()
	if err != nil || u != "real-user" || k != "sk_legacy" {
		t.Fatalf("post-rename: user=%q key=%q err=%v", u, k, err)
	}
	// Legacy entry gone.
	if v, _ := s.Read(ServiceDaemonAPIKey, AccountLegacy); v != "" {
		t.Fatalf("legacy entry should be cleared, got %q", v)
	}

	if err := s.DeleteAPIKey(); err != nil {
		t.Fatalf("DeleteAPIKey: %v", err)
	}
	if k, _ := s.GetAPIKey(); k != "" {
		t.Fatalf("key should be gone, got %q", k)
	}
	if u, _ := s.CurrentUserID(); u != "real-user" {
		t.Fatalf("current user preserved after DeleteAPIKey, got %q", u)
	}
	if err := s.ClearActiveUser(); err != nil {
		t.Fatalf("ClearActiveUser: %v", err)
	}
	if u, _ := s.CurrentUserID(); u != "" {
		t.Fatalf("current user should be cleared, got %q", u)
	}
}
