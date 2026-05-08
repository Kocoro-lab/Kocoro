package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"
)

func holdFlock(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}

func TestPuller_VersionOutOfRange(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(Manifest{
			BundleTs:      "2026-04-19T03-14-00Z",
			BundleVersion: "0.7.0",
			Files:         []ManifestFile{},
		})
	}))
	defer srv.Close()
	root := t.TempDir()
	p := NewPuller(Config{Provider: "cloud", BundleRoot: root, Endpoint: srv.URL, APIKey: "k"}, nil, nil)
	err := p.tick(context.Background())
	if err == nil || !strings.Contains(err.Error(), "version") {
		t.Fatalf("err=%v", err)
	}
}

func TestPuller_VersionInRange(t *testing.T) {
	for _, v := range []string{"0.4.0", "0.4.5", "0.4.99", "0.5.0", "0.5.99", "0.6.0", "0.6.99"} {
		if !versionInRange(v) {
			t.Fatalf("%q should be in range", v)
		}
	}
	for _, v := range []string{"0.3.9", "0.7.0", "1.0.0", "garbage"} {
		if versionInRange(v) {
			t.Fatalf("%q should NOT be in range", v)
		}
	}
}

func TestPuller_NoopWhenSameTs(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "bundles", "2026-04-19T03-14-00Z"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "bundles", "2026-04-19T03-14-00Z"), filepath.Join(root, "current")); err != nil {
		t.Fatal(err)
	}
	if err := WriteFingerprint(root, "k"); err != nil {
		t.Fatal(err)
	}
	fetched := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/manifest") {
			_ = json.NewEncoder(w).Encode(Manifest{
				BundleTs:      "2026-04-19T03-14-00Z",
				BundleVersion: "0.4.0",
				Files:         []ManifestFile{},
			})
			return
		}
		fetched = true
		w.WriteHeader(404)
	}))
	defer srv.Close()
	p := NewPuller(Config{Provider: "cloud", BundleRoot: root, Endpoint: srv.URL, APIKey: "k"}, nil, nil)
	if err := p.tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fetched {
		t.Fatal("should not fetch files for same ts")
	}
}

func TestPuller_TenantSwitch(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "bundles", "old"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := WriteFingerprint(root, "old-key"); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(Manifest{
			BundleTs:      "2026-04-19T03-14-00Z",
			BundleVersion: "0.4.0",
			Files:         nil,
		})
	}))
	defer srv.Close()
	p := NewPuller(Config{Provider: "cloud", BundleRoot: root, Endpoint: srv.URL, APIKey: "new-key"}, nil, nil)
	_ = p.tick(context.Background())
	if _, err := os.Stat(filepath.Join(root, "bundles", "old")); !os.IsNotExist(err) {
		t.Fatal("old bundles should have been wiped on tenant switch")
	}
	fp, _ := ReadFingerprint(root)
	if fp != Fingerprint("new-key") {
		t.Fatalf("fp=%q", fp)
	}
}

func TestPuller_EscapesManifestPathInDownload(t *testing.T) {
	root := t.TempDir()
	if err := WriteFingerprint(root, "k"); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte("hello"))
	sum := hex.EncodeToString(digest[:])

	var gotPath string
	var gotRawQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/manifest") {
			_ = json.NewEncoder(w).Encode(Manifest{
				BundleTs:      "2026-04-19T06-00-00Z",
				BundleVersion: "0.4.0",
				Files: []ManifestFile{
					{Path: "dir/a b?x#c", Size: 5, Sha256: sum},
				},
			})
			return
		}
		gotPath = r.URL.EscapedPath()
		gotRawQuery = r.URL.RawQuery
		_, _ = w.Write([]byte("hello"))
	}))
	defer srv.Close()

	p := NewPuller(Config{Provider: "cloud", BundleRoot: root, Endpoint: srv.URL, APIKey: "k"}, nil, nil)
	if err := p.tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if gotPath == "" {
		t.Fatal("file download request not observed")
	}
	if !strings.Contains(gotPath, "a%20b%3Fx%23c") {
		t.Fatalf("expected escaped segment in file request path, got %q", gotPath)
	}
	if gotRawQuery != "" {
		t.Fatalf("expected empty raw query, got %q", gotRawQuery)
	}
}

func TestPuller_FlockContentionSkips(t *testing.T) {
	root := t.TempDir()
	WriteFingerprint(root, "k")
	// Acquire the lock externally and never release it during the test.
	lockPath := filepath.Join(root, "bundle.lock")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := holdFlock(f); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("manifest must not be fetched while lock contended")
	}))
	defer srv.Close()
	p := NewPuller(Config{Provider: "cloud", BundleRoot: root, Endpoint: srv.URL, APIKey: "k"}, nil, nil)
	if err := p.tick(context.Background()); err != nil {
		t.Fatalf("contention should be silent: %v", err)
	}
}

func TestPuller_RejectsUnsafePaths(t *testing.T) {
	cases := []string{"/etc/passwd", "../escape", "x/../../y", "with\x00null", ""}
	for _, bad := range cases {
		root := t.TempDir()
		WriteFingerprint(root, "k")
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/manifest") {
				_ = json.NewEncoder(w).Encode(Manifest{
					BundleTs:      "2026-04-19T04-00-00Z",
					BundleVersion: "0.4.0",
					Files:         []ManifestFile{{Path: bad, Size: 1, Sha256: "deadbeef"}},
				})
				return
			}
			t.Fatalf("file fetch must NOT be issued for unsafe path %q", bad)
		}))
		p := NewPuller(Config{Provider: "cloud", BundleRoot: root, Endpoint: srv.URL, APIKey: "k"}, nil, nil)
		err := p.tick(context.Background())
		srv.Close()
		if err == nil || !strings.Contains(err.Error(), "unsafe") {
			t.Fatalf("path=%q expected unsafe error, got %v", bad, err)
		}
	}
}

func TestPuller_HashMismatchAborts(t *testing.T) {
	root := t.TempDir()
	WriteFingerprint(root, "k")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/manifest") {
			_ = json.NewEncoder(w).Encode(Manifest{
				BundleTs:      "2026-04-19T04-00-00Z",
				BundleVersion: "0.4.0",
				Files: []ManifestFile{{
					Path: "data.bin", Size: 4,
					Sha256: "deadbeef00000000000000000000000000000000000000000000000000000000",
				}},
			})
			return
		}
		_, _ = w.Write([]byte("xxxx"))
	}))
	defer srv.Close()
	p := NewPuller(Config{Provider: "cloud", BundleRoot: root, Endpoint: srv.URL, APIKey: "k"}, nil, nil)
	err := p.tick(context.Background())
	if err == nil {
		t.Fatal("expected sha mismatch error")
	}
	if _, e := os.Stat(filepath.Join(root, "bundles", "2026-04-19T04-00-00Z")); !os.IsNotExist(e) {
		t.Fatal("bundle should not be installed on hash mismatch")
	}
	// Staging should be cleaned up.
	if _, e := os.Stat(filepath.Join(root, "staging", "2026-04-19T04-00-00Z")); !os.IsNotExist(e) {
		t.Fatal("staging dir should be removed on abort")
	}
}

func TestPuller_AuditOnUnsafePath(t *testing.T) {
	root := t.TempDir()
	WriteFingerprint(root, "k")
	captured := []string{}
	a := AuditFunc(func(ev string, _ map[string]any) { captured = append(captured, ev) })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/manifest") {
			_ = json.NewEncoder(w).Encode(Manifest{
				BundleTs:      "2026-04-19T04-00-00Z",
				BundleVersion: "0.4.0",
				Files:         []ManifestFile{{Path: "../escape", Size: 1, Sha256: "x"}},
			})
		}
	}))
	defer srv.Close()
	p := NewPuller(Config{Provider: "cloud", BundleRoot: root, Endpoint: srv.URL, APIKey: "k"}, nil, a)
	_ = p.tick(context.Background())
	found := false
	for _, e := range captured {
		if e == "memory_bundle_unsafe_path" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected memory_bundle_unsafe_path audit, got %v", captured)
	}
}

func TestPuller_AtomicInstallAndRetention(t *testing.T) {
	root := t.TempDir()
	// Pre-install 4 old bundles + current symlink.
	bundles := []string{
		"2026-04-15T00-00-00Z",
		"2026-04-16T00-00-00Z",
		"2026-04-17T00-00-00Z",
		"2026-04-18T00-00-00Z",
	}
	for _, ts := range bundles {
		if err := os.MkdirAll(filepath.Join(root, "bundles", ts), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(root, "bundles", "2026-04-18T00-00-00Z"), filepath.Join(root, "current")); err != nil {
		t.Fatal(err)
	}
	WriteFingerprint(root, "k")

	const dataSha = "3a6eb0790f39ac87c94f3856b2dd2c5d110e6811602261a9a923d3bb23adc8b7"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/manifest") {
			_ = json.NewEncoder(w).Encode(Manifest{
				BundleTs:      "2026-04-19T00-00-00Z",
				BundleVersion: "0.4.0",
				Files:         []ManifestFile{{Path: "data", Size: 4, Sha256: dataSha}},
			})
			return
		}
		_, _ = w.Write([]byte("data"))
	}))
	defer srv.Close()
	p := NewPuller(Config{Provider: "cloud", BundleRoot: root, Endpoint: srv.URL, APIKey: "k"}, nil, nil)
	if err := p.tick(context.Background()); err != nil {
		t.Fatal(err)
	}

	target, err := os.Readlink(filepath.Join(root, "current"))
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(target) != "2026-04-19T00-00-00Z" {
		t.Fatalf("current=%q", target)
	}
	if _, err := os.Stat(filepath.Join(root, "bundles", "2026-04-19T00-00-00Z", "data")); err != nil {
		t.Fatal("installed bundle missing data file")
	}
	// Retention: newest 3 by ts (keeps 04-19, 04-18, 04-17). Plus current symlink target
	// is preserved if it falls outside newest-3 (here it does NOT — 04-19 is current).
	// 04-15 and 04-16 should be removed.
	entries, _ := os.ReadDir(filepath.Join(root, "bundles"))
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	want := []string{"2026-04-17T00-00-00Z", "2026-04-18T00-00-00Z", "2026-04-19T00-00-00Z"}
	if len(names) != 3 {
		t.Fatalf("kept %v want %v", names, want)
	}
	for i, n := range names {
		if n != want[i] {
			t.Fatalf("kept[%d]=%s want %s", i, n, want[i])
		}
	}
}

func TestPuller_RetentionPreservesCurrent(t *testing.T) {
	// Edge case: current symlink targets a bundle that falls OUTSIDE the
	// newest-3 by ts. Retention must still keep it (defensive).
	root := t.TempDir()
	for _, ts := range []string{"a", "b", "c", "d", "e"} {
		os.MkdirAll(filepath.Join(root, "bundles", ts), 0o755)
	}
	// current → "a" (oldest by lexicographic sort)
	os.Symlink(filepath.Join(root, "bundles", "a"), filepath.Join(root, "current"))
	WriteFingerprint(root, "k")
	// Manifest reports same ts as current ("a") → tick is noop and won't trigger
	// install, but we want to test retention in isolation. Call retain directly.
	p := NewPuller(Config{Provider: "cloud", BundleRoot: root, Endpoint: "http://x", APIKey: "k"}, nil, nil)
	p.retain(3)
	if _, err := os.Stat(filepath.Join(root, "bundles", "a")); err != nil {
		t.Fatal("retain wiped current symlink target")
	}
}
