package bundled

import (
	"os"
	"path/filepath"
	"testing"
)

// TestExtractBundledSkills_ExtractsAndIsIdempotent verifies a first extraction
// materializes the embedded tree with a content-hash sidecar, and a second call
// is a no-op fast path (no re-extraction) that returns the same directory.
func TestExtractBundledSkills_ExtractsAndIsIdempotent(t *testing.T) {
	shannonDir := t.TempDir()

	dir, err := ExtractBundledSkills(shannonDir)
	if err != nil {
		t.Fatalf("first extract: %v", err)
	}
	sidecar := filepath.Join(dir, hashFileName)
	hash1, err := os.ReadFile(sidecar)
	if err != nil {
		t.Fatalf("content-hash sidecar missing after extract: %v", err)
	}
	if len(hash1) == 0 {
		t.Fatal("content-hash sidecar is empty")
	}
	// At least one skill dir should exist (the tree is non-empty).
	entries, _ := os.ReadDir(dir)
	if len(entries) < 2 { // sidecar + ≥1 skill
		t.Fatalf("expected extracted skills, got %d entries", len(entries))
	}

	// Second call: fast path, sidecar unchanged, same dir.
	dir2, err := ExtractBundledSkills(shannonDir)
	if err != nil {
		t.Fatalf("second extract: %v", err)
	}
	if dir2 != dir {
		t.Errorf("dir changed across calls: %q vs %q", dir, dir2)
	}
	hash2, _ := os.ReadFile(sidecar)
	if string(hash1) != string(hash2) {
		t.Errorf("content hash changed across identical calls")
	}
}

// TestExtractBundledSkills_SelfHealsStaleSidecar proves the content-addressed
// design refreshes the tree when the sidecar does not match the embedded
// content — the exact upgrade case the old version-string sidecar missed.
func TestExtractBundledSkills_SelfHealsStaleSidecar(t *testing.T) {
	shannonDir := t.TempDir()
	dir, err := ExtractBundledSkills(shannonDir)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}

	// Simulate an upgrade from an older binary: stale sidecar + a leftover
	// orphan file that a newer embedded tree no longer ships.
	sidecar := filepath.Join(dir, hashFileName)
	if err := os.WriteFile(sidecar, []byte("stale-hash-from-old-build"), 0600); err != nil {
		t.Fatal(err)
	}
	orphan := filepath.Join(dir, "orphan-skill-from-old-version")
	if err := os.MkdirAll(orphan, 0700); err != nil {
		t.Fatal(err)
	}

	dir2, err := ExtractBundledSkills(shannonDir)
	if err != nil {
		t.Fatalf("re-extract: %v", err)
	}
	// Sidecar must be rewritten to the real embedded hash (no longer stale).
	got, _ := os.ReadFile(filepath.Join(dir2, hashFileName))
	if string(got) == "stale-hash-from-old-build" {
		t.Error("stale sidecar was not refreshed — bundled skills would stay stale across upgrades")
	}
	// The orphan directory from the old version must be gone (full replace).
	if _, err := os.Stat(orphan); err == nil {
		t.Error("orphan file from previous version survived re-extraction")
	}
}
