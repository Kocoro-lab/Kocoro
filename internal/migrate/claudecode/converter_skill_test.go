package claudecode

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConvertSkill_Flat(t *testing.T) {
	src := filepath.Join("testdata", "claude_home_basic", "skills", "aws-claude.md")
	staging := t.TempDir()
	scanned := ScannedSkill{Name: "aws-claude", SrcAbsPath: src, Layout: "flat"}
	if err := ConvertSkill(scanned, staging); err != nil {
		t.Fatalf("ConvertSkill: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(staging, "SKILL.md"))
	if err != nil {
		t.Fatalf("staged SKILL.md missing: %v", err)
	}
	if !strings.Contains(string(data), "aws-claude") {
		t.Errorf("staged SKILL.md content lost frontmatter: %s", data)
	}
}

func TestConvertSkill_DirWithScripts(t *testing.T) {
	home := t.TempDir()
	skill := filepath.Join(home, "skills", "ds")
	if err := os.MkdirAll(filepath.Join(skill, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skill, "SKILL.md"), []byte("---\nname: ds\ndescription: x\n---\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skill, "scripts", "helper.sh"), []byte("#!/bin/sh\necho hi\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	staging := t.TempDir()
	// For dir layout, SrcAbsPath is the skill DIRECTORY (per scanner_skills).
	scanned := ScannedSkill{Name: "ds", SrcAbsPath: skill, Layout: "dir"}
	if err := ConvertSkill(scanned, staging); err != nil {
		t.Fatalf("ConvertSkill: %v", err)
	}
	if _, err := os.Stat(filepath.Join(staging, "SKILL.md")); err != nil {
		t.Errorf("SKILL.md missing in staging: %v", err)
	}
	if _, err := os.Stat(filepath.Join(staging, "scripts", "helper.sh")); err != nil {
		t.Errorf("scripts/helper.sh missing in staging: %v", err)
	}
}

func TestConvertSkill_SymlinkInsideDir_NotCopied(t *testing.T) {
	home := t.TempDir()
	skill := filepath.Join(home, "skills", "with-symlink")
	if err := os.MkdirAll(filepath.Join(skill, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skill, "SKILL.md"), []byte("---\nname: with-symlink\ndescription: x\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("CONFIDENTIAL"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(skill, "scripts", "leak")); err != nil {
		t.Skipf("symlink unsupported here: %v", err)
	}

	staging := t.TempDir()
	scanned := ScannedSkill{Name: "with-symlink", SrcAbsPath: skill, Layout: "dir"}
	if err := ConvertSkill(scanned, staging); err != nil {
		t.Fatalf("ConvertSkill: %v", err)
	}
	if _, err := os.Stat(filepath.Join(staging, "scripts", "leak")); err == nil {
		t.Error("symlink leaked into staging — privacy invariant violated")
	}
}
