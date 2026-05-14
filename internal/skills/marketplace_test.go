package skills

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRegistryIndexParse(t *testing.T) {
	raw := `{
		"version": 1,
		"updated_at": "2026-04-06T12:00:00Z",
		"skills": [
			{
				"slug": "self-improving-agent",
				"name": "self-improving-agent",
				"description": "Captures learnings",
				"author": "pskoett",
				"license": "MIT-0",
				"repo": "https://github.com/peterskoett/self-improving-agent",
				"repo_path": "",
				"ref": "main",
				"homepage": "https://clawhub.ai/pskoett/self-improving-agent",
				"downloads": 354000,
				"stars": 3000,
				"version": "3.0.13",
				"security": {
					"virustotal": "benign",
					"openclaw": "benign",
					"scanned_at": "2026-04-01T00:00:00Z"
				},
				"tags": ["productivity", "meta"]
			}
		]
	}`

	var idx RegistryIndex
	if err := json.Unmarshal([]byte(raw), &idx); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if idx.Version != 1 {
		t.Errorf("Version = %d, want 1", idx.Version)
	}
	if len(idx.Skills) != 1 {
		t.Fatalf("len(Skills) = %d, want 1", len(idx.Skills))
	}
	s := idx.Skills[0]
	if s.Slug != "self-improving-agent" {
		t.Errorf("Slug = %q", s.Slug)
	}
	if s.Downloads != 354000 {
		t.Errorf("Downloads = %d, want 354000", s.Downloads)
	}
	if s.Security.VirusTotal != "benign" {
		t.Errorf("Security.VirusTotal = %q, want benign", s.Security.VirusTotal)
	}
	if s.Ref != "main" {
		t.Errorf("Ref = %q, want main", s.Ref)
	}
}

func TestMarketplaceClientFetchAndCache(t *testing.T) {
	var hits int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"version":1,"updated_at":"2026-04-06T00:00:00Z","skills":[{"slug":"demo","name":"demo","description":"d","author":"a","repo":"https://x/y"}]}`))
	}))
	defer ts.Close()

	client := NewMarketplaceClient(ts.URL, 1*time.Hour)
	idx, err := client.Load(context.Background())
	if err != nil {
		t.Fatalf("first Load: %v", err)
	}
	if len(idx.Skills) != 1 || idx.Skills[0].Slug != "demo" {
		t.Fatalf("unexpected index: %+v", idx)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("expected 1 hit, got %d", got)
	}

	// Second call within TTL should not hit the server.
	if _, err := client.Load(context.Background()); err != nil {
		t.Fatalf("second Load: %v", err)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("expected still 1 hit after cached load, got %d", got)
	}
}

func TestMarketplaceClientStaleOnError(t *testing.T) {
	var fail int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.LoadInt32(&fail) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Write([]byte(`{"version":1,"skills":[{"slug":"demo","name":"demo","description":"d","author":"a","repo":"r"}]}`))
	}))
	defer ts.Close()

	// Zero TTL so every call tries to refetch.
	client := NewMarketplaceClient(ts.URL, 0)
	if _, err := client.Load(context.Background()); err != nil {
		t.Fatalf("priming Load: %v", err)
	}

	atomic.StoreInt32(&fail, 1)
	idx, err := client.Load(context.Background())
	if err != nil {
		t.Fatalf("stale Load should succeed, got: %v", err)
	}
	if len(idx.Skills) != 1 {
		t.Errorf("expected 1 skill from stale cache, got %d", len(idx.Skills))
	}
	if !client.IsStale() {
		t.Errorf("expected IsStale() true after serving stale")
	}
}

// TestMarketplaceClientStaleCooldown verifies that once we fall into
// stale mode during a registry outage, subsequent Loads within the
// cooldown window do NOT re-hit the upstream. Otherwise heavy UI
// traffic during an outage would turn into a retry storm.
func TestMarketplaceClientStaleCooldown(t *testing.T) {
	var hits int32
	var fail int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		if atomic.LoadInt32(&fail) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Write([]byte(`{"version":1,"skills":[{"slug":"demo","name":"demo","description":"d","author":"a","repo":"r"}]}`))
	}))
	defer ts.Close()

	// Zero TTL so every Load would refetch without the cooldown guard.
	// Generous cooldown so it definitely covers the test window.
	client := NewMarketplaceClient(ts.URL, 0)
	client.staleCooldown = 10 * time.Second

	// Prime the cache.
	if _, err := client.Load(context.Background()); err != nil {
		t.Fatalf("priming Load: %v", err)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("priming hits = %d, want 1", got)
	}

	// Enter outage; first Load after the outage triggers one fetch
	// attempt, then serves stale.
	atomic.StoreInt32(&fail, 1)
	if _, err := client.Load(context.Background()); err != nil {
		t.Fatalf("first stale Load: %v", err)
	}
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Errorf("after first stale Load hits = %d, want 2", got)
	}
	if !client.IsStale() {
		t.Errorf("expected IsStale() true after first stale Load")
	}

	// Three more Loads during cooldown: must NOT retry.
	for i := 0; i < 3; i++ {
		if _, err := client.Load(context.Background()); err != nil {
			t.Fatalf("cooldown Load %d: %v", i, err)
		}
	}
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Errorf("hits after cooldown Loads = %d, want 2 (no retries during cooldown)", got)
	}
}

func TestMarketplaceClientNoCacheNoServer(t *testing.T) {
	// Unreachable URL, no prior cache → must return error.
	client := NewMarketplaceClient("http://127.0.0.1:1/no-such", 1*time.Hour)
	_, err := client.Load(context.Background())
	if err == nil {
		t.Fatal("expected error with no cache and unreachable URL")
	}
}

func TestFilterSortPaginate(t *testing.T) {
	entries := []MarketplaceEntry{
		{Slug: "alpha", Name: "alpha", Description: "The first thing", Author: "alice", Downloads: 10, Stars: 5},
		{Slug: "bravo", Name: "bravo", Description: "Second thing", Author: "bob", Downloads: 100, Stars: 20},
		{Slug: "charlie", Name: "charlie", Description: "Third thing", Author: "alice", Downloads: 50, Stars: 15},
		{Slug: "delta", Name: "delta", Description: "Malicious", Author: "mallory", Downloads: 999,
			Security: SecurityScan{VirusTotal: "malicious"}},
	}

	// Default sort = downloads desc, malicious excluded.
	out, total := FilterSortPaginate(entries, "", "downloads", 1, 10)
	if total != 3 {
		t.Errorf("total = %d, want 3 (malicious excluded)", total)
	}
	if len(out) != 3 {
		t.Fatalf("len(out) = %d, want 3", len(out))
	}
	if out[0].Slug != "bravo" || out[1].Slug != "charlie" || out[2].Slug != "alpha" {
		t.Errorf("downloads sort order wrong: %v %v %v", out[0].Slug, out[1].Slug, out[2].Slug)
	}

	// Sort by name asc.
	out, _ = FilterSortPaginate(entries, "", "name", 1, 10)
	if out[0].Slug != "alpha" || out[2].Slug != "charlie" {
		t.Errorf("name sort order wrong: %v", sluggs(out))
	}

	// Search: matches name, description, author (case-insensitive).
	out, total = FilterSortPaginate(entries, "ALICE", "downloads", 1, 10)
	if total != 2 {
		t.Errorf("alice search total = %d, want 2", total)
	}
	out, total = FilterSortPaginate(entries, "third", "downloads", 1, 10)
	if total != 1 || out[0].Slug != "charlie" {
		t.Errorf("third search wrong: total=%d, %v", total, sluggs(out))
	}

	// Pagination: page 2 of size 2, downloads desc.
	out, total = FilterSortPaginate(entries, "", "downloads", 2, 2)
	if total != 3 {
		t.Errorf("page2 total = %d, want 3", total)
	}
	if len(out) != 1 || out[0].Slug != "alpha" {
		t.Errorf("page2 contents: %v", sluggs(out))
	}

	// Out-of-range page → empty slice, total still correct.
	out, total = FilterSortPaginate(entries, "", "downloads", 99, 10)
	if total != 3 {
		t.Errorf("OOR total = %d, want 3", total)
	}
	if len(out) != 0 {
		t.Errorf("OOR expected empty slice, got %v", sluggs(out))
	}
}

func sluggs(es []MarketplaceEntry) []string {
	out := make([]string, len(es))
	for i, e := range es {
		out[i] = e.Slug
	}
	return out
}

func TestSlugLockSerializesSameSlug(t *testing.T) {
	locks := NewSlugLocks()
	var order []string
	var mu sync.Mutex

	done := make(chan struct{}, 2)
	start := make(chan struct{})

	go func() {
		<-start
		unlock := locks.Lock("alpha")
		time.Sleep(20 * time.Millisecond)
		mu.Lock()
		order = append(order, "A")
		mu.Unlock()
		unlock()
		done <- struct{}{}
	}()
	go func() {
		<-start
		time.Sleep(5 * time.Millisecond) // start after A has the lock
		unlock := locks.Lock("alpha")
		mu.Lock()
		order = append(order, "B")
		mu.Unlock()
		unlock()
		done <- struct{}{}
	}()

	close(start)
	<-done
	<-done
	if len(order) != 2 || order[0] != "A" || order[1] != "B" {
		t.Errorf("expected serialized order [A, B], got %v", order)
	}
}

func TestSlugLockDifferentSlugsConcurrent(t *testing.T) {
	locks := NewSlugLocks()

	unlockA := locks.Lock("alpha")
	defer unlockA()

	// Locking a different slug must not block.
	ch := make(chan struct{})
	go func() {
		unlock := locks.Lock("bravo")
		unlock()
		close(ch)
	}()
	select {
	case <-ch:
		// good
	case <-time.After(200 * time.Millisecond):
		t.Fatal("locking a different slug should not block")
	}
}

func TestStageCleanPayloadExcludesGit(t *testing.T) {
	src := t.TempDir()
	mustWrite(t, filepath.Join(src, "SKILL.md"), "---\nname: demo\ndescription: d\n---\nbody")
	mustWriteExec(t, filepath.Join(src, "scripts/run.sh"), "#!/bin/sh\necho hi")
	mustWrite(t, filepath.Join(src, ".git/config"), "[core]")
	mustWrite(t, filepath.Join(src, ".github/workflows/ci.yml"), "name: ci")
	mustWrite(t, filepath.Join(src, ".gitignore"), "node_modules")

	dst := filepath.Join(t.TempDir(), "stage")
	if err := stageCleanPayload(src, dst); err != nil {
		t.Fatalf("stageCleanPayload: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dst, "SKILL.md")); err != nil {
		t.Errorf("SKILL.md missing: %v", err)
	}
	// Scripts directory must survive AND keep its executable bit. Community
	// skills like self-improving-agent ship scripts/activator.sh that break
	// silently if we strip mode during the copy.
	scriptInfo, err := os.Stat(filepath.Join(dst, "scripts/run.sh"))
	if err != nil {
		t.Errorf("scripts/run.sh missing: %v", err)
	} else if scriptInfo.Mode().Perm()&0100 == 0 {
		t.Errorf("scripts/run.sh lost its executable bit: mode = %v", scriptInfo.Mode().Perm())
	}
	for _, excluded := range []string{".git", ".github", ".gitignore"} {
		if _, err := os.Stat(filepath.Join(dst, excluded)); !os.IsNotExist(err) {
			t.Errorf("%s should have been excluded, got err = %v", excluded, err)
		}
	}
}

func TestStageCleanPayloadRejectsSymlinks(t *testing.T) {
	src := t.TempDir()
	mustWrite(t, filepath.Join(src, "SKILL.md"), "---\nname: demo\ndescription: d\n---\n")
	// Create a symlink pointing outside the src tree.
	if err := os.Symlink("/etc/passwd", filepath.Join(src, "evil")); err != nil {
		t.Skipf("symlink not supported on this filesystem: %v", err)
	}

	dst := filepath.Join(t.TempDir(), "stage")
	err := stageCleanPayload(src, dst)
	if err == nil {
		t.Fatal("expected error on symlink, got nil")
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Errorf("error should mention symlink, got: %v", err)
	}
	// Stage directory must not contain a half-copied payload.
	if _, statErr := os.Stat(filepath.Join(dst, "SKILL.md")); statErr == nil {
		t.Error("stage dir should be cleaned up on symlink rejection")
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func mustWriteExec(t *testing.T, path, content string) {
	t.Helper()
	mustWrite(t, path, content)
	if err := os.Chmod(path, 0755); err != nil {
		t.Fatalf("chmod: %v", err)
	}
}

// zipFileSpec describes one entry in a test zip fixture.
type zipFileSpec struct {
	name string      // path inside the zip
	body string      // file contents
	mode os.FileMode // file mode (0 means default 0644)
	link string      // non-empty → emit as a symlink to this target
}

// makeZipFixture builds an in-memory zip archive from the given entries.
func makeZipFixture(t *testing.T, specs []zipFileSpec) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, sp := range specs {
		mode := sp.mode
		if mode == 0 {
			mode = 0644
		}
		if sp.link != "" {
			mode |= os.ModeSymlink
		}
		hdr := &zip.FileHeader{
			Name:   sp.name,
			Method: zip.Deflate,
		}
		hdr.SetMode(mode)
		w, err := zw.CreateHeader(hdr)
		if err != nil {
			t.Fatalf("zip CreateHeader %q: %v", sp.name, err)
		}
		body := sp.body
		if sp.link != "" {
			body = sp.link
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatalf("zip Write %q: %v", sp.name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip Close: %v", err)
	}
	return buf.Bytes()
}

func TestExtractZipToSkillSuccess(t *testing.T) {
	zipBytes := makeZipFixture(t, []zipFileSpec{
		{name: "SKILL.md", body: "---\nname: demo\ndescription: d\n---\nbody"},
		{name: "scripts/run.sh", body: "#!/bin/sh\necho hi", mode: 0755},
		{name: "references/schema.md", body: "schema content"},
	})

	destDir := filepath.Join(t.TempDir(), "stage")
	if err := extractZipToSkill(bytes.NewReader(zipBytes), destDir); err != nil {
		t.Fatalf("extractZipToSkill: %v", err)
	}

	// Core files present.
	if _, err := os.Stat(filepath.Join(destDir, "SKILL.md")); err != nil {
		t.Errorf("SKILL.md missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(destDir, "references", "schema.md")); err != nil {
		t.Errorf("references/schema.md missing: %v", err)
	}
	// Executable bit preserved on scripts.
	info, err := os.Stat(filepath.Join(destDir, "scripts", "run.sh"))
	if err != nil {
		t.Errorf("scripts/run.sh missing: %v", err)
	} else if info.Mode().Perm()&0100 == 0 {
		t.Errorf("scripts/run.sh lost executable bit: %v", info.Mode().Perm())
	}
}

func TestExtractZipToSkillExcludesGitMetadata(t *testing.T) {
	zipBytes := makeZipFixture(t, []zipFileSpec{
		{name: "SKILL.md", body: "---\nname: demo\ndescription: d\n---\n"},
		{name: ".git/config", body: "[core]"},
		{name: ".github/workflows/ci.yml", body: "name: ci"},
		{name: ".gitignore", body: "node_modules"},
		{name: ".gitattributes", body: "* text"},
	})

	destDir := filepath.Join(t.TempDir(), "stage")
	if err := extractZipToSkill(bytes.NewReader(zipBytes), destDir); err != nil {
		t.Fatalf("extractZipToSkill: %v", err)
	}
	for _, excluded := range []string{".git", ".github", ".gitignore", ".gitattributes"} {
		if _, err := os.Stat(filepath.Join(destDir, excluded)); !os.IsNotExist(err) {
			t.Errorf("%s should have been excluded, got err = %v", excluded, err)
		}
	}
}

func TestExtractZipToSkillRejectsSymlink(t *testing.T) {
	zipBytes := makeZipFixture(t, []zipFileSpec{
		{name: "SKILL.md", body: "---\nname: demo\ndescription: d\n---\n"},
		{name: "evil", link: "/etc/passwd"},
	})

	destDir := filepath.Join(t.TempDir(), "stage")
	err := extractZipToSkill(bytes.NewReader(zipBytes), destDir)
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Errorf("expected symlink rejection error, got: %v", err)
	}
	// Stage dir must be cleaned up on failure.
	if _, statErr := os.Stat(filepath.Join(destDir, "SKILL.md")); statErr == nil {
		t.Error("stage dir should be cleaned up after symlink rejection")
	}
}

func TestExtractZipToSkillRejectsZipSlip(t *testing.T) {
	// Classic zip-slip: entry name escapes the destination via ../
	zipBytes := makeZipFixture(t, []zipFileSpec{
		{name: "SKILL.md", body: "---\nname: demo\ndescription: d\n---\n"},
		{name: "../outside.txt", body: "malicious"},
	})

	destDir := filepath.Join(t.TempDir(), "stage")
	err := extractZipToSkill(bytes.NewReader(zipBytes), destDir)
	if err == nil || !strings.Contains(err.Error(), "escapes") {
		t.Errorf("expected zip-slip rejection error, got: %v", err)
	}
}

// TestExtractZipToSkillZipBombActualBytes verifies the zip-bomb guard
// counts ACTUAL decompressed bytes, not the attacker-controlled
// UncompressedSize64 field. Regression test for the pre-fix bypass.
func TestExtractZipToSkillZipBombActualBytes(t *testing.T) {
	// Temporarily shrink the uncompressed cap to 512 bytes so a small
	// fixture can trip the guard without allocating hundreds of MB.
	original := maxZipUncompressedBytes
	maxZipUncompressedBytes = 512
	defer func() { maxZipUncompressedBytes = original }()

	// ~1060 actual bytes across two entries, well over the 512 cap.
	zipBytes := makeZipFixture(t, []zipFileSpec{
		{name: "SKILL.md", body: "---\nname: demo\ndescription: d\n---\n"},
		{name: "payload.txt", body: strings.Repeat("x", 1024)},
	})

	destDir := filepath.Join(t.TempDir(), "stage")
	err := extractZipToSkill(bytes.NewReader(zipBytes), destDir)
	if err == nil {
		t.Fatal("expected zip-bomb guard to fire, got nil")
	}
	if !strings.Contains(err.Error(), "uncompressed size exceeds") {
		t.Errorf("error should mention uncompressed size cap, got: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(destDir, "SKILL.md")); statErr == nil {
		t.Error("stage dir should be cleaned up after zip-bomb rejection")
	}
}

func TestExtractZipToSkillRejectsSizeCap(t *testing.T) {
	// Build a tiny zip but feed it through a reader capped tinier than the
	// compressed size. Simulates a server returning more bytes than the cap.
	zipBytes := makeZipFixture(t, []zipFileSpec{
		{name: "SKILL.md", body: strings.Repeat("x", 1024)},
	})

	destDir := filepath.Join(t.TempDir(), "stage")
	// Feed exactly 10 bytes so the zip reader can't even parse the header.
	err := extractZipToSkill(io.LimitReader(bytes.NewReader(zipBytes), 10), destDir)
	if err == nil {
		t.Errorf("expected error from truncated zip, got nil")
	}
}

// makeFixtureRepo creates a minimal git repository on disk that can be
// cloned via file:// URLs. Uses the runGit helper from api.go.
func makeFixtureRepo(t *testing.T, skillContent string) string {
	t.Helper()
	dir := t.TempDir()
	if err := runGit(dir, "init", "-q", "-b", "main"); err != nil {
		t.Fatalf("git init: %v", err)
	}
	mustWrite(t, filepath.Join(dir, "SKILL.md"), skillContent)
	mustWrite(t, filepath.Join(dir, "README.md"), "# demo")
	if err := runGit(dir, "config", "user.email", "test@example.com"); err != nil {
		t.Fatalf("git config email: %v", err)
	}
	if err := runGit(dir, "config", "user.name", "Test"); err != nil {
		t.Fatalf("git config name: %v", err)
	}
	if err := runGit(dir, "config", "commit.gpgsign", "false"); err != nil {
		t.Fatalf("git config gpgsign: %v", err)
	}
	if err := runGit(dir, "add", "."); err != nil {
		t.Fatalf("git add: %v", err)
	}
	if err := runGit(dir, "commit", "-q", "-m", "init"); err != nil {
		t.Fatalf("git commit: %v", err)
	}
	return dir
}

func TestInstallFromMarketplaceSuccess(t *testing.T) {
	repo := makeFixtureRepo(t, "---\nname: demo\ndescription: d\n---\nbody")
	shannonDir := t.TempDir()

	entry := MarketplaceEntry{
		Slug: "demo",
		Name: "demo",
		Repo: "file://" + repo,
		Ref:  "main",
	}
	locks := NewSlugLocks()

	if err := InstallFromMarketplace(context.Background(), shannonDir, entry, locks); err != nil {
		t.Fatalf("InstallFromMarketplace: %v", err)
	}

	installed := filepath.Join(shannonDir, "skills", "demo", "SKILL.md")
	if _, err := os.Stat(installed); err != nil {
		t.Errorf("installed SKILL.md missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(shannonDir, "skills", "demo", ".git")); !os.IsNotExist(err) {
		t.Error(".git should have been excluded from installed skill")
	}
}

// TestInstallFromMarketplaceNameDiffersFromSlug confirms that a skill whose
// frontmatter `name` differs from the marketplace slug still installs.
// Regression target: ClawHub's xiaohongshu-mcp-skills package ships with
// `name: xiaohongshu`. Previous strict `canonical == dirName` rejection was
// over-tight and not required by the openclaw/clawhub spec.
func TestInstallFromMarketplaceNameDiffersFromSlug(t *testing.T) {
	repo := makeFixtureRepo(t, "---\nname: different\ndescription: d\n---\n")
	shannonDir := t.TempDir()

	entry := MarketplaceEntry{Slug: "demo", Name: "demo", Repo: "file://" + repo, Ref: "main"}
	if err := InstallFromMarketplace(context.Background(), shannonDir, entry, NewSlugLocks()); err != nil {
		t.Fatalf("install should succeed despite name/slug mismatch, got: %v", err)
	}
	if _, err := os.Stat(filepath.Join(shannonDir, "skills", "demo", "SKILL.md")); err != nil {
		t.Errorf("SKILL.md should exist under the slug-named directory: %v", err)
	}
	// Loaded skill: Name from frontmatter, Slug from directory name.
	list, err := LoadSkills(SkillSource{Dir: filepath.Join(shannonDir, "skills"), Source: SourceGlobal})
	if err != nil {
		t.Fatalf("LoadSkills: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 skill, got %d", len(list))
	}
	if list[0].Name != "different" {
		t.Errorf("Name should be %q (from frontmatter), got %q", "different", list[0].Name)
	}
	if list[0].Slug != "demo" {
		t.Errorf("Slug should be %q (from directory), got %q", "demo", list[0].Slug)
	}
}

func TestInstallFromMarketplaceBlocksMalicious(t *testing.T) {
	shannonDir := t.TempDir()
	entry := MarketplaceEntry{
		Slug:     "demo",
		Name:     "demo",
		Repo:     "file:///does/not/matter",
		Security: SecurityScan{VirusTotal: "malicious"},
	}
	err := InstallFromMarketplace(context.Background(), shannonDir, entry, NewSlugLocks())
	if !errors.Is(err, ErrMaliciousSkill) {
		t.Errorf("expected ErrMaliciousSkill, got: %v", err)
	}
}

func TestInstallFromMarketplaceAlreadyInstalled(t *testing.T) {
	repo := makeFixtureRepo(t, "---\nname: demo\ndescription: d\n---\n")
	shannonDir := t.TempDir()
	entry := MarketplaceEntry{Slug: "demo", Name: "demo", Repo: "file://" + repo, Ref: "main"}
	locks := NewSlugLocks()
	if err := InstallFromMarketplace(context.Background(), shannonDir, entry, locks); err != nil {
		t.Fatalf("first install: %v", err)
	}
	err := InstallFromMarketplace(context.Background(), shannonDir, entry, locks)
	if !errors.Is(err, ErrSkillAlreadyInstalled) {
		t.Errorf("expected ErrSkillAlreadyInstalled, got: %v", err)
	}
}

// makeFixtureRepoSubdir creates a fixture repo where SKILL.md lives under
// skills/<slug>/ rather than at the repo root, plus an unrelated sibling
// directory that must NOT end up in the installed skill. Used to exercise
// the sparse-checkout branch of InstallFromMarketplace.
func makeFixtureRepoSubdir(t *testing.T, slug, skillContent string) string {
	t.Helper()
	dir := t.TempDir()
	if err := runGit(dir, "init", "-q", "-b", "main"); err != nil {
		t.Fatalf("git init: %v", err)
	}
	mustWrite(t, filepath.Join(dir, "skills", slug, "SKILL.md"), skillContent)
	mustWriteExec(t, filepath.Join(dir, "skills", slug, "scripts", "hello.sh"), "#!/bin/sh\necho hi")
	// Sibling skill that must not bleed into the install.
	mustWrite(t, filepath.Join(dir, "skills", "other", "SKILL.md"), "---\nname: other\ndescription: x\n---\n")
	// Unrelated top-level file.
	mustWrite(t, filepath.Join(dir, "README.md"), "# monorepo")

	for _, args := range [][]string{
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "Test"},
		{"config", "commit.gpgsign", "false"},
		{"add", "."},
		{"commit", "-q", "-m", "init"},
	} {
		if err := runGit(dir, args...); err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
	}
	return dir
}

func TestInstallFromMarketplaceSubdirectory(t *testing.T) {
	repo := makeFixtureRepoSubdir(t, "demo", "---\nname: demo\ndescription: d\n---\nbody")
	shannonDir := t.TempDir()

	entry := MarketplaceEntry{
		Slug:     "demo",
		Name:     "demo",
		Repo:     "file://" + repo,
		RepoPath: "skills/demo",
		Ref:      "main",
	}

	if err := InstallFromMarketplace(context.Background(), shannonDir, entry, NewSlugLocks()); err != nil {
		t.Fatalf("InstallFromMarketplace: %v", err)
	}

	installedRoot := filepath.Join(shannonDir, "skills", "demo")

	// SKILL.md lands at the top of the installed dir, not nested under skills/demo.
	if _, err := os.Stat(filepath.Join(installedRoot, "SKILL.md")); err != nil {
		t.Errorf("installed SKILL.md missing: %v", err)
	}

	// Helper script survives and keeps its executable bit.
	scriptInfo, err := os.Stat(filepath.Join(installedRoot, "scripts", "hello.sh"))
	if err != nil {
		t.Errorf("scripts/hello.sh missing: %v", err)
	} else if scriptInfo.Mode().Perm()&0100 == 0 {
		t.Errorf("scripts/hello.sh lost its executable bit: mode = %v", scriptInfo.Mode().Perm())
	}

	// Unrelated siblings and top-level files must NOT be copied in.
	mustNotExist := []string{
		filepath.Join(installedRoot, "README.md"),
		filepath.Join(installedRoot, "skills"), // no nested skills/ dir
		filepath.Join(installedRoot, "other"),
		filepath.Join(installedRoot, ".git"),
	}
	for _, p := range mustNotExist {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s should not exist in subdirectory install, got err = %v", p, err)
		}
	}
}

func TestInstallFromMarketplaceZipSuccess(t *testing.T) {
	zipBytes := makeZipFixture(t, []zipFileSpec{
		{name: "SKILL.md", body: "---\nname: ziptest\ndescription: From a zip\n---\nbody"},
		{name: "scripts/run.sh", body: "#!/bin/sh\necho hi", mode: 0755},
		{name: "references/README.md", body: "ref"},
	})
	// Serve the zip over HTTP.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/zip")
		w.Write(zipBytes)
	}))
	defer ts.Close()

	shannonDir := t.TempDir()
	entry := MarketplaceEntry{
		Slug:        "ziptest",
		Name:        "ziptest",
		DownloadURL: ts.URL,
	}

	if err := InstallFromMarketplace(context.Background(), shannonDir, entry, NewSlugLocks()); err != nil {
		t.Fatalf("InstallFromMarketplace: %v", err)
	}

	// Verify the installed tree.
	installed := filepath.Join(shannonDir, "skills", "ziptest")
	if _, err := os.Stat(filepath.Join(installed, "SKILL.md")); err != nil {
		t.Errorf("SKILL.md missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(installed, "references", "README.md")); err != nil {
		t.Errorf("references/README.md missing: %v", err)
	}
	scriptInfo, err := os.Stat(filepath.Join(installed, "scripts", "run.sh"))
	if err != nil {
		t.Errorf("scripts/run.sh missing: %v", err)
	} else if scriptInfo.Mode().Perm()&0100 == 0 {
		t.Errorf("scripts/run.sh lost executable bit: %v", scriptInfo.Mode().Perm())
	}
}

// Regression test for the "Skill package is malformed" install failure seen
// on ClawHub skills that ship with nested YAML metadata (e.g. a `clawdbot`
// object containing emoji, required bins, etc.). The old `map[string]string`
// typing caused yaml.Unmarshal to reject any non-string value, which surfaced
// as ErrInvalidSkillPayload → HTTP 422 → Desktop toast. With the widened
// `map[string]any` typing the install should succeed and the parsed metadata
// should preserve the nested shape verbatim.
func TestInstallFromMarketplaceZipNestedMetadata(t *testing.T) {
	skillMD := `---
name: docker-essentials
description: Essential Docker commands and workflows for container management, image operations, and debugging.
homepage: https://docs.docker.com/
metadata:
  clawdbot:
    emoji: "🐳"
    requires:
      bins:
        - docker
---
# Docker Essentials
Essential Docker commands for container and image management.
`
	zipBytes := makeZipFixture(t, []zipFileSpec{
		{name: "SKILL.md", body: skillMD},
	})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/zip")
		w.Write(zipBytes)
	}))
	defer ts.Close()

	shannonDir := t.TempDir()
	entry := MarketplaceEntry{
		Slug:        "docker-essentials",
		Name:        "docker-essentials",
		DownloadURL: ts.URL,
	}

	if err := InstallFromMarketplace(context.Background(), shannonDir, entry, NewSlugLocks()); err != nil {
		t.Fatalf("InstallFromMarketplace with nested metadata should succeed, got: %v", err)
	}

	installed := filepath.Join(shannonDir, "skills", "docker-essentials")
	if _, err := os.Stat(filepath.Join(installed, "SKILL.md")); err != nil {
		t.Errorf("SKILL.md missing after install: %v", err)
	}

	// Reload through the normal loader path and verify the nested metadata
	// survived the parse → model → re-read roundtrip. This is the exact
	// path `GET /skills` and `GET /skills/{name}` use.
	sources := []SkillSource{{Dir: filepath.Join(shannonDir, "skills"), Source: SourceGlobal}}
	list, err := LoadSkills(sources...)
	if err != nil {
		t.Fatalf("LoadSkills after install: %v", err)
	}
	var loaded *Skill
	for _, s := range list {
		if s.Name == "docker-essentials" {
			loaded = s
			break
		}
	}
	if loaded == nil {
		t.Fatal("docker-essentials not found after LoadSkills")
	}
	clawdbot, ok := loaded.Metadata["clawdbot"].(map[string]any)
	if !ok {
		t.Fatalf("expected clawdbot to be map[string]any, got %T: %#v", loaded.Metadata["clawdbot"], loaded.Metadata["clawdbot"])
	}
	if clawdbot["emoji"] != "🐳" {
		t.Errorf("emoji roundtrip failed: got %v", clawdbot["emoji"])
	}
	requires, ok := clawdbot["requires"].(map[string]any)
	if !ok {
		t.Fatalf("expected requires to be map[string]any, got %T", clawdbot["requires"])
	}
	bins, ok := requires["bins"].([]any)
	if !ok || len(bins) != 1 || bins[0] != "docker" {
		t.Errorf("bins roundtrip failed: got %#v", requires["bins"])
	}
}

// Regression test: ClawHub's `ivangdavila/docker` ships with `name: Docker`
// (display label) and `slug: docker` (directory identity). Install must
// succeed and the loaded skill must be addressable by its slug; the
// frontmatter display name is preserved as skill.Name.
func TestInstallFromMarketplaceZipSlugOverridesDisplayName(t *testing.T) {
	skillMD := `---
name: Docker
slug: docker
description: Docker containers, images, Compose stacks, networking, and the commands that keep real environments stable.
metadata:
  clawdbot:
    emoji: "🐳"
    requires:
      bins:
        - docker
---
# Docker
Commands and workflows.
`
	zipBytes := makeZipFixture(t, []zipFileSpec{
		{name: "SKILL.md", body: skillMD},
	})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/zip")
		w.Write(zipBytes)
	}))
	defer ts.Close()

	shannonDir := t.TempDir()
	entry := MarketplaceEntry{
		Slug:        "docker",
		Name:        "docker",
		DownloadURL: ts.URL,
	}

	if err := InstallFromMarketplace(context.Background(), shannonDir, entry, NewSlugLocks()); err != nil {
		t.Fatalf("InstallFromMarketplace with display-name/slug split should succeed, got: %v", err)
	}

	// The installed skill should be addressable by its slug on disk; the
	// frontmatter display name "Docker" is preserved as skill.Name.
	sources := []SkillSource{{Dir: filepath.Join(shannonDir, "skills"), Source: SourceGlobal}}
	list, err := LoadSkills(sources...)
	if err != nil {
		t.Fatalf("LoadSkills after install: %v", err)
	}
	var loaded *Skill
	for _, s := range list {
		if s.Slug == "docker" {
			loaded = s
			break
		}
	}
	if loaded == nil {
		t.Fatal("loaded skill should be addressable by slug \"docker\"")
	}
	if loaded.Name != "Docker" {
		t.Errorf("Name should preserve frontmatter display label %q, got %q", "Docker", loaded.Name)
	}
	if loaded.Slug != "docker" {
		t.Errorf("Slug should be %q (from directory), got %q", "docker", loaded.Slug)
	}
}

// TestInstallFromMarketplaceZipNameDiffersFromSlug: zip-transport counterpart
// to TestInstallFromMarketplaceNameDiffersFromSlug — frontmatter name differs
// from marketplace slug, install still succeeds.
func TestInstallFromMarketplaceZipNameDiffersFromSlug(t *testing.T) {
	zipBytes := makeZipFixture(t, []zipFileSpec{
		{name: "SKILL.md", body: "---\nname: actual\ndescription: d\n---\n"},
	})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(zipBytes)
	}))
	defer ts.Close()

	shannonDir := t.TempDir()
	entry := MarketplaceEntry{Slug: "expected", Name: "expected", DownloadURL: ts.URL}

	if err := InstallFromMarketplace(context.Background(), shannonDir, entry, NewSlugLocks()); err != nil {
		t.Fatalf("install should succeed despite name/slug mismatch, got: %v", err)
	}
	if _, err := os.Stat(filepath.Join(shannonDir, "skills", "expected", "SKILL.md")); err != nil {
		t.Errorf("SKILL.md should be installed under slug directory: %v", err)
	}
}

func TestInstallFromMarketplaceZipMaliciousBlocked(t *testing.T) {
	// Malicious gate runs before any network call, so no server needed.
	shannonDir := t.TempDir()
	entry := MarketplaceEntry{
		Slug:        "evil",
		Name:        "evil",
		DownloadURL: "http://127.0.0.1:1/never-called",
		Security:    SecurityScan{OpenClaw: "malicious"},
	}
	err := InstallFromMarketplace(context.Background(), shannonDir, entry, NewSlugLocks())
	if !errors.Is(err, ErrMaliciousSkill) {
		t.Errorf("expected ErrMaliciousSkill, got: %v", err)
	}
}

func TestInstallFromMarketplaceZipUpstreamFailure(t *testing.T) {
	// Server returns 500 — should surface as ErrMarketplaceUpstreamFailure.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()

	shannonDir := t.TempDir()
	entry := MarketplaceEntry{Slug: "demo", Name: "demo", DownloadURL: ts.URL}

	err := InstallFromMarketplace(context.Background(), shannonDir, entry, NewSlugLocks())
	if !errors.Is(err, ErrMarketplaceUpstreamFailure) {
		t.Errorf("expected ErrMarketplaceUpstreamFailure, got: %v", err)
	}
}

func TestInstallFromMarketplaceNoTransport(t *testing.T) {
	// Neither Repo nor DownloadURL set → must be rejected before any
	// filesystem work.
	shannonDir := t.TempDir()
	entry := MarketplaceEntry{Slug: "demo", Name: "demo"}

	err := InstallFromMarketplace(context.Background(), shannonDir, entry, NewSlugLocks())
	if err == nil || !errors.Is(err, ErrInvalidSkillPayload) {
		t.Errorf("expected ErrInvalidSkillPayload, got: %v", err)
	}
}

// TestInstallFromMarketplaceConcurrentSameSlug drives two goroutines at the
// same slug. With the per-slug lock in place, exactly one must succeed and
// the other must see ErrSkillAlreadyInstalled — no filesystem corruption,
// no ENOTEMPTY from a half-finished rename.
func TestInstallFromMarketplaceConcurrentSameSlug(t *testing.T) {
	repo := makeFixtureRepo(t, "---\nname: demo\ndescription: d\n---\n")
	shannonDir := t.TempDir()
	entry := MarketplaceEntry{Slug: "demo", Name: "demo", Repo: "file://" + repo, Ref: "main"}
	locks := NewSlugLocks()

	const N = 5
	results := make(chan error, N)
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- InstallFromMarketplace(context.Background(), shannonDir, entry, locks)
		}()
	}
	wg.Wait()
	close(results)

	var successes, alreadyInstalled, other int
	for err := range results {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, ErrSkillAlreadyInstalled):
			alreadyInstalled++
		default:
			other++
			t.Errorf("unexpected concurrent install error: %v", err)
		}
	}
	if successes != 1 {
		t.Errorf("expected exactly 1 successful install, got %d", successes)
	}
	if alreadyInstalled != N-1 {
		t.Errorf("expected %d already-installed results, got %d", N-1, alreadyInstalled)
	}

	// Final state must be a single clean install.
	if _, err := os.Stat(filepath.Join(shannonDir, "skills", "demo", "SKILL.md")); err != nil {
		t.Errorf("installed SKILL.md missing after concurrent installs: %v", err)
	}
}

func TestMarketplaceEntryIsMalicious(t *testing.T) {
	cases := []struct {
		name string
		e    MarketplaceEntry
		want bool
	}{
		{"clean", MarketplaceEntry{}, false},
		{"benign", MarketplaceEntry{Security: SecurityScan{VirusTotal: "benign", OpenClaw: "benign"}}, false},
		{"vt-malicious", MarketplaceEntry{Security: SecurityScan{VirusTotal: "malicious"}}, true},
		{"oc-malicious", MarketplaceEntry{Security: SecurityScan{OpenClaw: "malicious"}}, true},
		{"suspicious-only", MarketplaceEntry{Security: SecurityScan{VirusTotal: "suspicious"}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.e.IsMalicious(); got != tc.want {
				t.Errorf("IsMalicious = %v, want %v", got, tc.want)
			}
		})
	}
}

// --- InstallFromZipData tests ---

func makeSkillZip(t *testing.T, name, description, prompt string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	f, err := zw.Create("SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(f, "---\nname: %s\ndescription: %s\n---\n\n%s\n", name, description, prompt)
	zw.Close()
	return buf.Bytes()
}

func TestInstallFromZipData_Success(t *testing.T) {
	dir := t.TempDir()
	locks := NewSlugLocks()
	zipBytes := makeSkillZip(t, "my-skill", "does things", "Do the thing.")

	skill, err := InstallFromZipData(dir, bytes.NewReader(zipBytes), false, locks)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if skill.Name != "my-skill" {
		t.Errorf("Name = %q, want my-skill", skill.Name)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "skills", "my-skill", "SKILL.md")); statErr != nil {
		t.Errorf("SKILL.md not installed: %v", statErr)
	}
}

func TestInstallFromZipData_ConflictWithoutForce(t *testing.T) {
	dir := t.TempDir()
	locks := NewSlugLocks()
	zipBytes := makeSkillZip(t, "my-skill", "does things", "Do the thing.")

	if _, err := InstallFromZipData(dir, bytes.NewReader(zipBytes), false, locks); err != nil {
		t.Fatal(err)
	}
	_, err := InstallFromZipData(dir, bytes.NewReader(zipBytes), false, locks)
	var conflict *SkillConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("want SkillConflictError, got %v", err)
	}
	if conflict.ExistingName != "my-skill" {
		t.Errorf("ExistingName = %q", conflict.ExistingName)
	}
}

func TestInstallFromZipData_ForceOverwrites(t *testing.T) {
	dir := t.TempDir()
	locks := NewSlugLocks()
	zip1 := makeSkillZip(t, "my-skill", "v1 desc", "v1 prompt")
	zip2 := makeSkillZip(t, "my-skill", "v2 desc", "v2 prompt")

	if _, err := InstallFromZipData(dir, bytes.NewReader(zip1), false, locks); err != nil {
		t.Fatal(err)
	}
	skill, err := InstallFromZipData(dir, bytes.NewReader(zip2), true, locks)
	if err != nil {
		t.Fatalf("force install failed: %v", err)
	}
	if skill.Description != "v2 desc" {
		t.Errorf("Description = %q, want v2 desc", skill.Description)
	}
}

func TestInstallFromZipData_BuiltinBlocked(t *testing.T) {
	dir := t.TempDir()
	locks := NewSlugLocks()

	// Builtins live in skills/<slug>/ (synced by EnsureBuiltinSkills on every
	// daemon startup). Create one there to mirror the real on-disk state.
	globalSkillDir := filepath.Join(dir, "skills", "kocoro")
	if err := os.MkdirAll(globalSkillDir, 0700); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(globalSkillDir, "SKILL.md"),
		[]byte("---\nname: kocoro\ndescription: builtin\n---\n\nBuiltin.\n"), 0600)

	zipBytes := makeSkillZip(t, "kocoro", "replacement", "Replace the builtin.")
	// Without force — must be rejected as builtin, not 409 conflict.
	_, err := InstallFromZipData(dir, bytes.NewReader(zipBytes), false, locks)
	if !errors.Is(err, ErrSkillIsBuiltin) {
		t.Fatalf("want ErrSkillIsBuiltin, got %v", err)
	}
	// Even with force=true the builtin guard must hold: EnsureBuiltinSkills
	// would wipe the override on the next restart, so allowing it would be
	// a silent revert.
	_, err = InstallFromZipData(dir, bytes.NewReader(zipBytes), true, locks)
	if !errors.Is(err, ErrSkillIsBuiltin) {
		t.Fatalf("force=true: want ErrSkillIsBuiltin, got %v", err)
	}
}

// TestInstallFromZipData_BundledNotBuiltin_Allowed locks in that a bundled
// skill that is NOT in the builtinSkills list (e.g. kocoro-login) can be
// overridden via upload. The previous directory-stat check rejected these
// as "builtin" whenever the bundled-skills/ cache had been populated.
func TestInstallFromZipData_BundledNotBuiltin_Allowed(t *testing.T) {
	dir := t.TempDir()
	locks := NewSlugLocks()

	// Simulate a stale bundled-skills cache populated by a prior agent run.
	bundledDir := filepath.Join(dir, "bundled-skills", "kocoro-login")
	if err := os.MkdirAll(bundledDir, 0700); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(bundledDir, "SKILL.md"),
		[]byte("---\nname: kocoro-login\ndescription: bundled\n---\n\nBundled.\n"), 0600)

	zipBytes := makeSkillZip(t, "kocoro-login", "user override", "Override.")
	skill, err := InstallFromZipData(dir, bytes.NewReader(zipBytes), false, locks)
	if err != nil {
		t.Fatalf("expected upload to succeed, got %v", err)
	}
	if skill.Name != "kocoro-login" {
		t.Errorf("Name = %q", skill.Name)
	}
}

func TestInstallFromZipData_InvalidZip_NoSKILLMD(t *testing.T) {
	dir := t.TempDir()
	locks := NewSlugLocks()

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	f, _ := zw.Create("README.md")
	f.Write([]byte("no skill here"))
	zw.Close()

	_, err := InstallFromZipData(dir, bytes.NewReader(buf.Bytes()), false, locks)
	if !errors.Is(err, ErrInvalidSkillPayload) {
		t.Fatalf("want ErrInvalidSkillPayload, got %v", err)
	}
}

func TestInstallFromZipData_SingleSubdirUnwrap(t *testing.T) {
	dir := t.TempDir()
	locks := NewSlugLocks()

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	f, _ := zw.Create("my-skill-main/SKILL.md")
	fmt.Fprintf(f, "---\nname: my-skill\ndescription: github zip\n---\n\nPrompt.\n")
	zw.Close()

	skill, err := InstallFromZipData(dir, bytes.NewReader(buf.Bytes()), false, locks)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if skill.Name != "my-skill" {
		t.Errorf("Name = %q, want my-skill", skill.Name)
	}
}

func TestInstallFromZipData_MacOSFinderZip(t *testing.T) {
	dir := t.TempDir()
	locks := NewSlugLocks()

	// Simulate a macOS Finder-compressed ZIP: real skill dir + __MACOSX metadata dir
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	f, _ := zw.Create("my-skill/SKILL.md")
	fmt.Fprintf(f, "---\nname: my-skill\ndescription: finder zip\n---\n\nPrompt.\n")
	// macOS metadata file
	zw.Create("__MACOSX/my-skill/.DS_Store")
	zw.Close()

	skill, err := InstallFromZipData(dir, bytes.NewReader(buf.Bytes()), false, locks)
	if err != nil {
		t.Fatalf("unexpected error with __MACOSX dir: %v", err)
	}
	if skill.Name != "my-skill" {
		t.Errorf("Name = %q, want my-skill", skill.Name)
	}
}

// TestInstallFromZipData_PrefersFrontmatterSlug locks in that a skill with a
// display-only `name` (e.g. CJK) plus an ASCII `slug` installs at the slug
// path and isn't rejected by ValidateSkillName on the display name.
func TestInstallFromZipData_PrefersFrontmatterSlug(t *testing.T) {
	dir := t.TempDir()
	locks := NewSlugLocks()

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	f, _ := zw.Create("SKILL.md")
	fmt.Fprintf(f, "---\nname: 小红书\nslug: xiaohongshu\ndescription: cjk display name\n---\n\nPrompt.\n")
	zw.Close()

	skill, err := InstallFromZipData(dir, bytes.NewReader(buf.Bytes()), false, locks)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if skill.Slug != "xiaohongshu" {
		t.Errorf("Slug = %q, want xiaohongshu", skill.Slug)
	}
	if skill.Name != "小红书" {
		t.Errorf("Name = %q, want 小红书", skill.Name)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "skills", "xiaohongshu", "SKILL.md")); statErr != nil {
		t.Errorf("SKILL.md not installed at slug path: %v", statErr)
	}
}

// TestInstallFromZipData_ForceOverwriteNoBackupLingers locks in that the
// crash-safe overwrite pattern (rename old → bak, rename new → dest, cleanup
// bak) leaves no .bak.* directories behind on the happy path.
func TestInstallFromZipData_ForceOverwriteNoBackupLingers(t *testing.T) {
	dir := t.TempDir()
	locks := NewSlugLocks()
	zip1 := makeSkillZip(t, "my-skill", "v1 desc", "v1 prompt")
	zip2 := makeSkillZip(t, "my-skill", "v2 desc", "v2 prompt")

	if _, err := InstallFromZipData(dir, bytes.NewReader(zip1), false, locks); err != nil {
		t.Fatal(err)
	}
	if _, err := InstallFromZipData(dir, bytes.NewReader(zip2), true, locks); err != nil {
		t.Fatal(err)
	}

	skillsDir := filepath.Join(dir, "skills")
	entries, err := os.ReadDir(skillsDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".bak.") {
			t.Errorf("leftover backup directory after force overwrite: %s", e.Name())
		}
	}

	// New content must actually be on disk (replaced, not merged).
	data, err := os.ReadFile(filepath.Join(skillsDir, "my-skill", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "v2 desc") {
		t.Errorf("SKILL.md still contains v1 content after force overwrite: %q", data)
	}
	if strings.Contains(string(data), "v1 prompt") {
		t.Errorf("SKILL.md still contains v1 prompt after force overwrite")
	}
}

// TestInstallFromZipData_MalformedFrontmatter_ReportsParseError surfaces YAML
// parse failures instead of masking them as "missing name field".
func TestInstallFromZipData_MalformedFrontmatter_ReportsParseError(t *testing.T) {
	dir := t.TempDir()
	locks := NewSlugLocks()

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	f, _ := zw.Create("SKILL.md")
	// Malformed YAML: unclosed quoted string in name.
	f.Write([]byte("---\nname: \"unterminated\ndescription: bad\n---\n\nPrompt.\n"))
	zw.Close()

	_, err := InstallFromZipData(dir, bytes.NewReader(buf.Bytes()), false, locks)
	if !errors.Is(err, ErrInvalidSkillPayload) {
		t.Fatalf("want ErrInvalidSkillPayload, got %v", err)
	}
	if !strings.Contains(err.Error(), "frontmatter") && !strings.Contains(err.Error(), "yaml") {
		t.Errorf("error %q should mention frontmatter/yaml parse failure", err.Error())
	}
	if strings.Contains(err.Error(), "missing name field") {
		t.Errorf("error should not be misleading 'missing name field' for YAML parse failure: %q", err.Error())
	}
}

// TestInstallFromZipData_ConflictPromptTruncation caps the prompt fields in
// the 409 conflict response so a near-50 MB skill can't produce a huge JSON
// response body.
func TestInstallFromZipData_ConflictPromptTruncation(t *testing.T) {
	dir := t.TempDir()
	locks := NewSlugLocks()

	// 100 KB prompt, well above the 8 KB preview cap.
	bigPrompt := strings.Repeat("a", 100*1024)
	zip1 := makeSkillZip(t, "my-skill", "v1 desc", bigPrompt)
	zip2 := makeSkillZip(t, "my-skill", "v2 desc", bigPrompt)

	if _, err := InstallFromZipData(dir, bytes.NewReader(zip1), false, locks); err != nil {
		t.Fatal(err)
	}
	_, err := InstallFromZipData(dir, bytes.NewReader(zip2), false, locks)
	var conflict *SkillConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("want SkillConflictError, got %v", err)
	}
	if len(conflict.ExistingPrompt) > ConflictPromptPreviewBytes {
		t.Errorf("ExistingPrompt len = %d, want <= %d", len(conflict.ExistingPrompt), ConflictPromptPreviewBytes)
	}
	if len(conflict.NewPrompt) > ConflictPromptPreviewBytes {
		t.Errorf("NewPrompt len = %d, want <= %d", len(conflict.NewPrompt), ConflictPromptPreviewBytes)
	}
	// Truncated prompts must still convey content (not be empty).
	if conflict.ExistingPrompt == "" || conflict.NewPrompt == "" {
		t.Errorf("truncated prompts should not be empty")
	}
}
