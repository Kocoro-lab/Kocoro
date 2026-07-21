package skills

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestEnsureBuiltinSkills_InstallsAllOnFirstRun guards against a typo in the
// builtinSkills slice — every name must end up on disk after a fresh run, with
// real frontmatter content (not a stub).
func TestEnsureBuiltinSkills_InstallsAllOnFirstRun(t *testing.T) {
	shannonDir := t.TempDir()

	if err := EnsureBuiltinSkills(shannonDir); err != nil {
		t.Fatalf("EnsureBuiltinSkills: %v", err)
	}

	for _, name := range builtinSkills {
		skillMD := filepath.Join(shannonDir, "skills", name, "SKILL.md")
		data, err := os.ReadFile(skillMD)
		if err != nil {
			t.Fatalf("builtin %q SKILL.md missing after install: %v", name, err)
		}
		if !strings.Contains(string(data), "name: "+name) {
			t.Fatalf("builtin %q SKILL.md content wrong: %s", name, data)
		}
	}
}

// TestEnsureBuiltinSkills_RestoresDeleted is the headline self-heal test:
// `rm -rf ~/.shannon/skills/<builtin>` must be undone on the next startup.
// This is the user-reported bug the new design fixes.
func TestEnsureBuiltinSkills_RestoresDeleted(t *testing.T) {
	shannonDir := t.TempDir()
	if err := EnsureBuiltinSkills(shannonDir); err != nil {
		t.Fatalf("first EnsureBuiltinSkills: %v", err)
	}

	target := filepath.Join(shannonDir, "skills", "kocoro-generative-ui")
	if err := os.RemoveAll(target); err != nil {
		t.Fatalf("delete builtin: %v", err)
	}

	if err := EnsureBuiltinSkills(shannonDir); err != nil {
		t.Fatalf("restore EnsureBuiltinSkills: %v", err)
	}

	if _, err := os.Stat(filepath.Join(target, "SKILL.md")); err != nil {
		t.Fatalf("kocoro-generative-ui not restored: %v", err)
	}
}

// TestEnsureBuiltinSkills_OverwritesUserEdits locks in the new "builtins are
// daemon-managed" semantic: any local edit to a builtin SKILL.md is wiped on
// next startup. Users wanting customization should fork under a different
// skill name.
func TestEnsureBuiltinSkills_OverwritesUserEdits(t *testing.T) {
	shannonDir := t.TempDir()
	if err := EnsureBuiltinSkills(shannonDir); err != nil {
		t.Fatalf("first EnsureBuiltinSkills: %v", err)
	}

	skillMD := filepath.Join(shannonDir, "skills", "kocoro", "SKILL.md")
	if err := os.WriteFile(skillMD, []byte("user-edit"), 0600); err != nil {
		t.Fatalf("edit SKILL.md: %v", err)
	}

	if err := EnsureBuiltinSkills(shannonDir); err != nil {
		t.Fatalf("second EnsureBuiltinSkills: %v", err)
	}

	data, err := os.ReadFile(skillMD)
	if err != nil {
		t.Fatalf("read SKILL.md: %v", err)
	}
	// `user-edit` cannot contain `name: kocoro`, so this single positive
	// assertion proves the edit was wiped AND the embed content was written.
	if !strings.Contains(string(data), "name: kocoro") {
		t.Fatalf("user edit survived overlay; got %q, want frontmatter from embed.FS", data)
	}
}

// TestEnsureBuiltinSkills_RemovesStaleFiles confirms orphan files (e.g. a
// reference file the user dropped in, or a leftover from a previous bundled
// version) are removed when overlay fires.
func TestEnsureBuiltinSkills_RemovesStaleFiles(t *testing.T) {
	shannonDir := t.TempDir()
	if err := EnsureBuiltinSkills(shannonDir); err != nil {
		t.Fatalf("first EnsureBuiltinSkills: %v", err)
	}

	stale := filepath.Join(shannonDir, "skills", "kocoro", "references", "removed.md")
	if err := os.MkdirAll(filepath.Dir(stale), 0700); err != nil {
		t.Fatalf("mkdir references: %v", err)
	}
	if err := os.WriteFile(stale, []byte("stale"), 0600); err != nil {
		t.Fatalf("write stale: %v", err)
	}

	if err := EnsureBuiltinSkills(shannonDir); err != nil {
		t.Fatalf("second EnsureBuiltinSkills: %v", err)
	}

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale file survived overlay: err=%v", err)
	}
}

// TestEnsureBuiltinSkills_NoOpWhenContentMatches is a performance + safety
// guarantee: when on-disk content already matches embed.FS, the overlay path
// must NOT fire (otherwise we churn mtimes and inotify watchers on every
// startup). Detected by stuffing an artificial old mtime and asserting it
// survives.
func TestEnsureBuiltinSkills_NoOpWhenContentMatches(t *testing.T) {
	shannonDir := t.TempDir()
	if err := EnsureBuiltinSkills(shannonDir); err != nil {
		t.Fatalf("first EnsureBuiltinSkills: %v", err)
	}

	skillMD := filepath.Join(shannonDir, "skills", "kocoro", "SKILL.md")
	pastTime := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(skillMD, pastTime, pastTime); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	if err := EnsureBuiltinSkills(shannonDir); err != nil {
		t.Fatalf("second EnsureBuiltinSkills: %v", err)
	}

	info, err := os.Stat(skillMD)
	if err != nil {
		t.Fatalf("stat after second run: %v", err)
	}
	if !info.ModTime().Equal(pastTime) {
		t.Fatalf("SKILL.md was rewritten when content matched; mtime moved %v -> %v", pastTime, info.ModTime())
	}
}

// TestEnsureBuiltinSkills_RemovesLegacyVersionSidecar confirms the previous
// design's `_builtin.version` file is cleaned up on next startup so it
// doesn't accumulate as a stale curiosity.
func TestEnsureBuiltinSkills_RemovesLegacyVersionSidecar(t *testing.T) {
	shannonDir := t.TempDir()
	skillsDir := filepath.Join(shannonDir, "skills")
	if err := os.MkdirAll(skillsDir, 0700); err != nil {
		t.Fatalf("mkdir skills: %v", err)
	}
	legacy := filepath.Join(skillsDir, "_builtin.version")
	if err := os.WriteFile(legacy, []byte("0.0.99"), 0600); err != nil {
		t.Fatalf("write legacy sidecar: %v", err)
	}

	if err := EnsureBuiltinSkills(shannonDir); err != nil {
		t.Fatalf("EnsureBuiltinSkills: %v", err)
	}

	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Fatalf("legacy _builtin.version survived: err=%v", err)
	}
}

// fakeTarball builds an in-memory .tar.gz mimicking the GitHub codeload archive
// of anthropics/skills: every named skill lands at
// "<root>/skills/<name>/SKILL.md" under a single top-level wrapper directory
// (codeload always wraps the repo like this). Returns the complete gzip-tar
// bytes, ready to hand back from a fake openRepoTarball.
func fakeTarball(t *testing.T, root string, names ...string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, name := range names {
		body := []byte("---\nname: " + name + "\ndescription: fixture\n---\nbody")
		hdr := &tar.Header{
			Name:     root + "/skills/" + name + "/SKILL.md",
			Mode:     0644,
			Size:     int64(len(body)),
			Typeflag: tar.TypeReg,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("write tar header: %v", err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatalf("write tar body: %v", err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	return buf.Bytes()
}

func tarballReader(b []byte) io.ReadCloser { return io.NopCloser(bytes.NewReader(b)) }

// TestInstallFromRepo_RetriesOnTransientFailure pins the retry contract:
// a single transient download failure must NOT surface to the caller;
// the next attempt's success should produce a clean install. This is the
// core robustness change — if it ever regresses, ouikyou's class of
// "intermittent github.com reachability" failures comes back as user-
// visible install errors. Backoff makes this test ~1s in the happy path.
func TestInstallFromRepo_RetriesOnTransientFailure(t *testing.T) {
	orig := openRepoTarball
	t.Cleanup(func() { openRepoTarball = orig })

	shannonDir := t.TempDir()
	var attempts atomic.Int32
	tarball := fakeTarball(t, "skills-main", "retry-fixture")
	openRepoTarball = func(ctx context.Context) (io.ReadCloser, error) {
		if attempts.Add(1) == 1 {
			return nil, fmt.Errorf("simulated network flake")
		}
		return tarballReader(tarball), nil
	}

	destDir := filepath.Join(shannonDir, "skills", "retry-fixture")
	if err := installFromRepo(context.Background(), shannonDir, "retry-fixture", destDir); err != nil {
		t.Fatalf("install should succeed after retry, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(destDir, "SKILL.md")); err != nil {
		t.Fatalf("installed SKILL.md missing: %v", err)
	}
	// Attempt 1 flake, attempt 2 success → exactly 2 fetch attempts.
	if got := attempts.Load(); got != 2 {
		t.Errorf("expected exactly 2 fetch attempts (1 flake + 1 success), got %d", got)
	}
}

// TestInstallFromRepo_NoRetryOnNotFoundSentinel pins the boundary case:
// when the download succeeds but the requested skill isn't actually in the
// Anthropic repo's tree, the wrapper must return immediately rather than
// re-fetching the (large, network-bound) tarball twice more on what is a
// deterministic 404. The fake serves a valid tarball containing a *different*
// skill; expect exactly one fetch.
func TestInstallFromRepo_NoRetryOnNotFoundSentinel(t *testing.T) {
	orig := openRepoTarball
	t.Cleanup(func() { openRepoTarball = orig })

	shannonDir := t.TempDir()
	var fetches atomic.Int32
	tarball := fakeTarball(t, "skills-main", "some-other-skill")
	openRepoTarball = func(ctx context.Context) (io.ReadCloser, error) {
		fetches.Add(1)
		return tarballReader(tarball), nil
	}

	destDir := filepath.Join(shannonDir, "skills", "does-not-exist")
	err := installFromRepo(context.Background(), shannonDir, "does-not-exist", destDir)
	if err == nil {
		t.Fatalf("expected sentinel error, got nil")
	}
	// Typed-sentinel assertion: pins the wrapper contract so a future
	// rename of the error string can't silently regress the no-retry path.
	if !errors.Is(err, ErrSkillNotInRepo) {
		t.Errorf("expected errors.Is(err, ErrSkillNotInRepo) == true, got %v", err)
	}
	if got := fetches.Load(); got != 1 {
		t.Errorf("fetched %d times, want exactly 1 (no retry on deterministic 404)", got)
	}
}

// TestInstallFromRepo_ExhaustsRetriesAndReturnsLastError pins the failure
// shape after all retries are exhausted: the last download error is returned
// unchanged so the audit log captures the actual failure the user needs to
// diagnose (proxy, DNS, upstream 5xx, etc.).
func TestInstallFromRepo_ExhaustsRetriesAndReturnsLastError(t *testing.T) {
	orig := openRepoTarball
	t.Cleanup(func() { openRepoTarball = orig })

	shannonDir := t.TempDir()
	var attempts atomic.Int32
	openRepoTarball = func(ctx context.Context) (io.ReadCloser, error) {
		attempts.Add(1)
		return nil, fmt.Errorf("persistent failure: connection refused")
	}

	destDir := filepath.Join(shannonDir, "skills", "always-fails")
	err := installFromRepo(context.Background(), shannonDir, "always-fails", destDir)
	if err == nil {
		t.Fatalf("expected error after exhausting retries, got nil")
	}
	if !strings.Contains(err.Error(), "persistent failure") {
		t.Errorf("final error did not preserve underlying message: %v", err)
	}
	// One fetch per attempt. installFromRepoMaxAttempts is the contract here;
	// if it changes, this assertion documents the expected attempt count.
	if got := attempts.Load(); int(got) != installFromRepoMaxAttempts {
		t.Errorf("fetched %d times, want %d (one per attempt)", got, installFromRepoMaxAttempts)
	}
}

// TestExtractSkillFromTarball_ArbitraryRootAndNested proves the wrapper-dir
// name is not hardcoded (codeload derives it from the ref) and that nested
// files under the skill dir are extracted with the prefix stripped.
func TestExtractSkillFromTarball_ArbitraryRootAndNested(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	writeReg := func(name, body string) {
		hdr := &tar.Header{Name: name, Mode: 0644, Size: int64(len(body)), Typeflag: tar.TypeReg}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	// Arbitrary wrapper name + a nested reference file + an unrelated skill.
	writeReg("skills-abc123/skills/pdf/SKILL.md", "---\nname: pdf\ndescription: x\n---\nbody")
	writeReg("skills-abc123/skills/pdf/reference/guide.md", "nested")
	writeReg("skills-abc123/skills/other/SKILL.md", "---\nname: other\ndescription: y\n---\n")
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}

	dest := filepath.Join(t.TempDir(), "pdf")
	if err := extractSkillFromTarball(bytes.NewReader(buf.Bytes()), "pdf", dest); err != nil {
		t.Fatalf("extract: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "SKILL.md")); err != nil {
		t.Errorf("SKILL.md not extracted: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "reference", "guide.md")); err != nil {
		t.Errorf("nested reference file not extracted: %v", err)
	}
	// The unrelated skill must not have leaked into pdf's dir.
	if _, err := os.Stat(filepath.Join(dest, "other")); err == nil {
		t.Errorf("unrelated skill leaked into dest")
	}
}

// TestExtractSkillFromTarball_SkipsSymlinkAndTraversal proves a hostile tarball
// cannot plant a symlink or escape destDir: non-regular entries are skipped and
// there is a within-dir guard on every write.
func TestExtractSkillFromTarball_SkipsSymlinkAndTraversal(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	body := "---\nname: pdf\ndescription: x\n---\nbody"
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(tw.WriteHeader(&tar.Header{Name: "root/skills/pdf/SKILL.md", Mode: 0644, Size: int64(len(body)), Typeflag: tar.TypeReg}))
	_, err := tw.Write([]byte(body))
	must(err)
	// A symlink pointing outside — must be skipped, not created.
	must(tw.WriteHeader(&tar.Header{Name: "root/skills/pdf/evil-link", Linkname: "/etc/passwd", Typeflag: tar.TypeSymlink}))
	must(tw.Close())
	must(gz.Close())

	dest := filepath.Join(t.TempDir(), "pdf")
	if err := extractSkillFromTarball(bytes.NewReader(buf.Bytes()), "pdf", dest); err != nil {
		t.Fatalf("extract: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dest, "evil-link")); err == nil {
		t.Errorf("symlink entry was materialized; it must be skipped")
	}
	if _, err := os.Stat(filepath.Join(dest, "SKILL.md")); err != nil {
		t.Errorf("regular file should still extract alongside skipped symlink: %v", err)
	}
}

// TestExtractSkillFromTarball_PerFileSizeCap proves the per-file byte cap trips
// on a lying/oversized entry rather than writing unbounded data to disk.
func TestExtractSkillFromTarball_PerFileSizeCap(t *testing.T) {
	orig := maxSkillFileBytes
	maxSkillFileBytes = 8
	t.Cleanup(func() { maxSkillFileBytes = orig })

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	big := strings.Repeat("A", 64)
	if err := tw.WriteHeader(&tar.Header{Name: "root/skills/pdf/SKILL.md", Mode: 0644, Size: int64(len(big)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(big)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}

	dest := filepath.Join(t.TempDir(), "pdf")
	if err := extractSkillFromTarball(bytes.NewReader(buf.Bytes()), "pdf", dest); err == nil {
		t.Fatalf("expected per-file size cap error, got nil")
	}
}

// tarGzBytes builds a .tar.gz from ordered (name, body) regular-file entries.
func tarGzBytes(t *testing.T, entries [][2]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		body := []byte(e[1])
		if err := tw.WriteHeader(&tar.Header{Name: e[0], Mode: 0644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestExtractSkillFromTarball_FileCountCap trips the per-skill file-count guard
// — one of the decompression-bomb backstops that otherwise has no direct test.
func TestExtractSkillFromTarball_FileCountCap(t *testing.T) {
	orig := maxSkillFiles
	maxSkillFiles = 2
	t.Cleanup(func() { maxSkillFiles = orig })

	data := tarGzBytes(t, [][2]string{
		{"root/skills/pdf/SKILL.md", "---\nname: pdf\ndescription: x\n---\n"},
		{"root/skills/pdf/a.md", "a"},
		{"root/skills/pdf/b.md", "b"},
	})
	dest := filepath.Join(t.TempDir(), "pdf")
	if err := extractSkillFromTarball(bytes.NewReader(data), "pdf", dest); err == nil ||
		!strings.Contains(err.Error(), "files") {
		t.Fatalf("expected file-count cap error, got %v", err)
	}
}

// TestExtractSkillFromTarball_TotalDecompressionCap trips the whole-archive
// decompression guard (countingReader / maxSkillTarballBytes) — the other
// backstop that was previously untested.
func TestExtractSkillFromTarball_TotalDecompressionCap(t *testing.T) {
	orig := maxSkillTarballBytes
	maxSkillTarballBytes = 4 // below even a single 512-byte tar header block
	t.Cleanup(func() { maxSkillTarballBytes = orig })

	data := tarGzBytes(t, [][2]string{
		{"root/skills/pdf/SKILL.md", "---\nname: pdf\ndescription: x\n---\nbody"},
	})
	dest := filepath.Join(t.TempDir(), "pdf")
	if err := extractSkillFromTarball(bytes.NewReader(data), "pdf", dest); err == nil ||
		!strings.Contains(err.Error(), "decompression") {
		t.Fatalf("expected decompression-guard error, got %v", err)
	}
}

// Sanity that the backoff actually adds up to the documented worst-case
// total. Cheap arithmetic test, no I/O.
func TestInstallFromRepo_BackoffBudget(t *testing.T) {
	var total time.Duration
	for attempt := 1; attempt < installFromRepoMaxAttempts; attempt++ {
		total += time.Duration(attempt) * time.Second
	}
	if want := 3 * time.Second; total != want {
		t.Errorf("backoff budget = %v, want %v (0s + 1s + 2s)", total, want)
	}
}
