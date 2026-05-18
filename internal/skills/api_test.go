package skills

import (
	"errors"
	"fmt"
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

// fakeGitSucceed populates dir with a minimal Anthropic-repo-style layout
// for the requested skill name. Returns a function suitable for assigning
// to the package-level runGit variable. The caller chooses which subcommand
// (clone / sparse-checkout) triggers the layout write.
func fakeGitSucceed(name string) func(dir string, args ...string) error {
	return func(dir string, args ...string) error {
		if len(args) > 0 && args[0] == "clone" {
			skillDir := filepath.Join(dir, "skills", name)
			if err := os.MkdirAll(skillDir, 0700); err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"),
				[]byte("---\nname: "+name+"\ndescription: fixture\n---\nbody"), 0644); err != nil {
				return err
			}
		}
		return nil
	}
}

// TestInstallFromRepo_RetriesOnTransientFailure pins the retry contract:
// a single transient `git clone` failure must NOT surface to the caller;
// the next attempt's success should produce a clean install. This is the
// core robustness change — if it ever regresses, ouikyou's class of
// "intermittent github.com reachability" failures comes back as user-
// visible install errors. Backoff makes this test ~1s in the happy path.
func TestInstallFromRepo_RetriesOnTransientFailure(t *testing.T) {
	origRunGit := runGit
	t.Cleanup(func() { runGit = origRunGit })

	shannonDir := t.TempDir()
	var attempts atomic.Int32
	succeed := fakeGitSucceed("retry-fixture")
	runGit = func(dir string, args ...string) error {
		n := attempts.Add(1)
		if len(args) > 0 && args[0] == "clone" && n == 1 {
			return fmt.Errorf("simulated network flake")
		}
		return succeed(dir, args...)
	}

	destDir := filepath.Join(shannonDir, "skills", "retry-fixture")
	if err := installFromRepo(shannonDir, "retry-fixture", destDir); err != nil {
		t.Fatalf("install should succeed after retry, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(destDir, "SKILL.md")); err != nil {
		t.Fatalf("installed SKILL.md missing: %v", err)
	}
	// Attempt 1: clone fail. Attempt 2: clone ok + sparse-checkout. Total 3 git calls.
	if got := attempts.Load(); got < 2 {
		t.Errorf("expected ≥2 git calls (proves retry happened), got %d", got)
	}
}

// TestInstallFromRepo_NoRetryOnNotFoundSentinel pins the boundary case:
// when `git clone` succeeds but the requested skill isn't actually in the
// Anthropic repo's tree, the wrapper must return immediately rather than
// re-running the (slow, network-bound) clone twice more on what is a
// deterministic 404. Mock clone always succeeds; we never lay down the
// requested skill subdir; expect exactly one clone call.
func TestInstallFromRepo_NoRetryOnNotFoundSentinel(t *testing.T) {
	origRunGit := runGit
	t.Cleanup(func() { runGit = origRunGit })

	shannonDir := t.TempDir()
	var cloneCalls atomic.Int32
	runGit = func(dir string, args ...string) error {
		if len(args) > 0 && args[0] == "clone" {
			cloneCalls.Add(1)
		}
		// Intentionally do not create skills/<name>/SKILL.md — that's what
		// triggers the "not found in Anthropic repo" sentinel.
		return nil
	}

	destDir := filepath.Join(shannonDir, "skills", "does-not-exist")
	err := installFromRepo(shannonDir, "does-not-exist", destDir)
	if err == nil {
		t.Fatalf("expected sentinel error, got nil")
	}
	// Typed-sentinel assertion: pins the wrapper contract so a future
	// rename of the error string can't silently regress the no-retry path.
	if !errors.Is(err, ErrSkillNotInRepo) {
		t.Errorf("expected errors.Is(err, ErrSkillNotInRepo) == true, got %v", err)
	}
	if got := cloneCalls.Load(); got != 1 {
		t.Errorf("clone called %d times, want exactly 1 (no retry on deterministic 404)", got)
	}
}

// TestInstallFromRepo_ExhaustsRetriesAndReturnsLastError pins the failure
// shape after all retries are exhausted: the last git error is returned
// unchanged so the audit log captures the actual stderr the user needs to
// diagnose (proxy, DNS, missing git binary, etc.).
func TestInstallFromRepo_ExhaustsRetriesAndReturnsLastError(t *testing.T) {
	origRunGit := runGit
	t.Cleanup(func() { runGit = origRunGit })

	shannonDir := t.TempDir()
	var attempts atomic.Int32
	runGit = func(dir string, args ...string) error {
		attempts.Add(1)
		return fmt.Errorf("persistent failure: exit status 128")
	}

	destDir := filepath.Join(shannonDir, "skills", "always-fails")
	err := installFromRepo(shannonDir, "always-fails", destDir)
	if err == nil {
		t.Fatalf("expected error after exhausting retries, got nil")
	}
	if !strings.Contains(err.Error(), "persistent failure") {
		t.Errorf("final error did not preserve underlying message: %v", err)
	}
	// Each attempt makes one clone call before the failure short-circuits
	// the rest of the attempt's git operations. installFromRepoMaxAttempts
	// is the contract here; if it changes, this assertion documents the
	// expected attempt count.
	if got := attempts.Load(); int(got) != installFromRepoMaxAttempts {
		t.Errorf("clone called %d times, want %d (one per attempt)", got, installFromRepoMaxAttempts)
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
