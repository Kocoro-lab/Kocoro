package claudecode

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestScanSkills_BasicLayouts(t *testing.T) {
	src := filepath.Join("testdata", "claude_home_basic")
	got, warnings, err := scanSkills(src)
	if err != nil {
		t.Fatalf("scanSkills: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %+v", warnings)
	}
	names := map[string]string{}
	for _, s := range got {
		names[s.Name] = s.Layout
	}
	if names["aws-claude"] != "flat" {
		t.Errorf("aws-claude: want flat, got %q", names["aws-claude"])
	}
	if names["dir-style"] != "dir" {
		t.Errorf("dir-style: want dir, got %q", names["dir-style"])
	}
	if names["flat-one"] != "" {
		t.Errorf("empty dir 'flat-one' should not be scanned, got %q", names["flat-one"])
	}
}

func TestScanSkills_MissingDir(t *testing.T) {
	_, warnings, err := scanSkills(filepath.Join("testdata", "does-not-exist"))
	if err != nil {
		t.Fatalf("missing dir should not error, got %v", err)
	}
	if len(warnings) == 0 {
		t.Error("expected source_unavailable warning")
	}
}

func TestScanSkills_InvalidNameWarnsAndSkips(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "skills", "My-Skill"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "skills", "My-Skill", "SKILL.md"), []byte("---\nname: My-Skill\ndescription: x\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "skills", "my_skill.md"), []byte("---\nname: my_skill\ndescription: x\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "skills", "valid-skill.md"), []byte("---\nname: valid-skill\ndescription: x\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, warns, err := scanSkills(home)
	if err != nil {
		t.Fatalf("scanSkills: %v", err)
	}
	if len(got) != 1 || got[0].Name != "valid-skill" {
		t.Fatalf("scanned skills = %+v, want only valid-skill", got)
	}
	invalid := 0
	for _, w := range warns {
		if w.Kind == "invalid_name" {
			invalid++
		}
	}
	if invalid != 2 {
		t.Fatalf("invalid_name warnings = %d, want 2: %+v", invalid, warns)
	}
}

// TestScanSkills_DirHashIncludesScripts proves that any change to a file
// under a dir-layout skill (e.g. scripts/helper.sh) flips ContentHash and
// SizeBytes. Without this, the planner's TOCTOU re-check would miss mid-flight
// script edits during Phase B.
func TestScanSkills_DirHashIncludesScripts(t *testing.T) {
	home := t.TempDir()
	skill := filepath.Join(home, "skills", "with-scripts")
	if err := os.MkdirAll(filepath.Join(skill, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skill, "SKILL.md"),
		[]byte("---\nname: with-scripts\ndescription: x\n---\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	scriptPath := filepath.Join(skill, "scripts", "helper.sh")
	if err := os.WriteFile(scriptPath, []byte("#!/bin/sh\necho v1\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	got1, _, err := scanSkills(home)
	if err != nil || len(got1) != 1 {
		t.Fatalf("first scan failed: err=%v got=%+v", err, got1)
	}
	first := got1[0]
	if first.Layout != "dir" {
		t.Fatalf("layout = %q", first.Layout)
	}
	if first.SizeBytes < int64(len("body\n")+len("#!/bin/sh\necho v1\n")) {
		t.Errorf("SizeBytes too small to include scripts: %d", first.SizeBytes)
	}

	// Mutate the script — ContentHash and SizeBytes must change.
	if err := os.WriteFile(scriptPath, []byte("#!/bin/sh\necho v2 with more bytes\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	got2, _, err := scanSkills(home)
	if err != nil || len(got2) != 1 {
		t.Fatalf("second scan failed")
	}
	second := got2[0]
	if first.ContentHash == second.ContentHash {
		t.Errorf("ContentHash unchanged after script edit — TOCTOU re-check would miss the change")
	}
	if first.SizeBytes == second.SizeBytes {
		t.Errorf("SizeBytes unchanged after script grew")
	}
}

// TestScanSkills_SymlinkAtRoot_Rejected ensures a skill whose top-level entry
// is itself a symlink is skipped with a symlink_escape warning.
func TestScanSkills_SymlinkAtRoot_Rejected(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "secret.md")
	if err := os.WriteFile(outside, []byte("CONFIDENTIAL"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(home, "skills", "evil.md")); err != nil {
		t.Skipf("symlink unsupported here: %v", err)
	}

	got, warns, err := scanSkills(home)
	if err != nil {
		t.Fatalf("scanSkills: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("symlinked skill should be skipped, got %+v", got)
	}
	gotEscape := false
	for _, w := range warns {
		if w.Kind == "symlink_escape" {
			gotEscape = true
		}
	}
	if !gotEscape {
		t.Errorf("expected symlink_escape warning, got %+v", warns)
	}
}

// TestScanSkills_SymlinkInsideDir_Rejected ensures a skill DIRECTORY containing
// any symlink (e.g. scripts/leak → /etc/passwd) is rejected entirely. We do
// not silently skip the symlink while admitting the rest — the whole skill is
// invalidated so a partial copy cannot conceal an exfiltration attempt.
func TestScanSkills_SymlinkInsideDir_Rejected(t *testing.T) {
	home := t.TempDir()
	skill := filepath.Join(home, "skills", "evil-dir")
	if err := os.MkdirAll(filepath.Join(skill, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skill, "SKILL.md"),
		[]byte("---\nname: evil-dir\ndescription: x\n---\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("CONFIDENTIAL"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(skill, "scripts", "leak")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	got, warns, _ := scanSkills(home)
	if len(got) != 0 {
		t.Errorf("dir skill with symlink should be rejected, got %+v", got)
	}
	gotEscape := false
	for _, w := range warns {
		if w.Kind == "symlink_escape" {
			gotEscape = true
		}
	}
	if !gotEscape {
		t.Errorf("expected symlink_escape warning, got %+v", warns)
	}
}

// TestScanSkills_DirSizeOverLimit_Skipped verifies the 50 MB dir limit.
func TestScanSkills_DirSizeOverLimit_Skipped(t *testing.T) {
	home := t.TempDir()
	skill := filepath.Join(home, "skills", "huge-dir")
	if err := os.MkdirAll(skill, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skill, "SKILL.md"),
		[]byte("---\nname: huge-dir\ndescription: x\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Two 4 MB files don't trip MaxFileBytes (5 MB) but together with more
	// files we approach the 50 MB dir limit. Write 14 files of ~4 MB each
	// = ~56 MB > MaxSkillDirBytes.
	chunk := bytes.Repeat([]byte("x"), 4*1024*1024)
	for i := 0; i < 14; i++ {
		if err := os.WriteFile(filepath.Join(skill, "f"+string(rune('a'+i))+".bin"), chunk, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, warns, _ := scanSkills(home)
	if len(got) != 0 {
		t.Errorf("oversized dir should be skipped, got %+v", got)
	}
	gotSize := false
	for _, w := range warns {
		if w.Kind == "size_limit" {
			gotSize = true
		}
	}
	if !gotSize {
		t.Errorf("expected size_limit warning, got %+v", warns)
	}
}
