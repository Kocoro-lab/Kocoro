package claudecode

import (
	"os"
	"path/filepath"
	"testing"
)

func TestComputeSkillFingerprint_Flat(t *testing.T) {
	home := t.TempDir()
	skillsDir := filepath.Join(home, "skills")
	if err := os.MkdirAll(skillsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	flat := filepath.Join(skillsDir, "flat.md")
	if err := os.WriteFile(flat, []byte("---\nname: flat\ndescription: x\n---\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, _, err := scanSkills(home)
	if err != nil || len(got) != 1 {
		t.Fatalf("scan: %v %+v", err, got)
	}
	fp, err := ComputeSkillFingerprint(got[0])
	if err != nil {
		t.Fatalf("ComputeSkillFingerprint: %v", err)
	}
	if fp.Kind != "file" {
		t.Errorf("Kind = %q, want file", fp.Kind)
	}
	if fp.Hash != got[0].ContentHash {
		t.Errorf("Hash %q != scan ContentHash %q", fp.Hash, got[0].ContentHash)
	}
}

func TestComputeSkillFingerprint_Dir_DetectsScriptEdit(t *testing.T) {
	home := t.TempDir()
	skill := filepath.Join(home, "skills", "ds")
	if err := os.MkdirAll(filepath.Join(skill, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skill, "SKILL.md"), []byte("---\nname: ds\ndescription: x\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	scriptPath := filepath.Join(skill, "scripts", "go.sh")
	if err := os.WriteFile(scriptPath, []byte("#!/bin/sh\necho 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	scanned, _, _ := scanSkills(home)
	if len(scanned) != 1 || scanned[0].Layout != "dir" {
		t.Fatalf("setup: got %+v", scanned)
	}
	fp1, err := ComputeSkillFingerprint(scanned[0])
	if err != nil {
		t.Fatal(err)
	}
	if fp1.Kind != "skill_tree" {
		t.Errorf("Kind = %q, want skill_tree", fp1.Kind)
	}

	// Mutate the script. ComputeSkillFingerprint MUST produce a different hash.
	if err := os.WriteFile(scriptPath, []byte("#!/bin/sh\necho 2 longer\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	scanned2, _, _ := scanSkills(home)
	fp2, err := ComputeSkillFingerprint(scanned2[0])
	if err != nil {
		t.Fatal(err)
	}
	if fp1.Hash == fp2.Hash {
		t.Errorf("tree hash unchanged after script edit — applier TOCTOU re-check would miss")
	}
}

func TestValidateSourceFingerprint_File_MatchesAndMismatches(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.md")
	if err := os.WriteFile(p, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	fp, err := ComputeFileFingerprint(p)
	if err != nil {
		t.Fatal(err)
	}
	ok, err := ValidateSourceFingerprint(p, fp)
	if err != nil || !ok {
		t.Errorf("expected match: ok=%v err=%v", ok, err)
	}
	// Mutate.
	if err := os.WriteFile(p, []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	ok, err = ValidateSourceFingerprint(p, fp)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if ok {
		t.Error("expected mismatch after mutation")
	}
}

func TestValidateSourceFingerprint_FileRejectsSymlinkSwap(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.md")
	other := filepath.Join(dir, "other.md")
	if err := os.WriteFile(p, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(other, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	fp, err := ComputeFileFingerprint(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, p); err != nil {
		t.Skipf("symlink unsupported here: %v", err)
	}

	ok, err := ValidateSourceFingerprint(p, fp)
	if err == nil || ok {
		t.Fatalf("expected symlink swap rejection, ok=%v err=%v", ok, err)
	}
	if _, err := ComputeFileFingerprint(p); err == nil {
		t.Fatal("ComputeFileFingerprint should reject symlink paths")
	}
}

func TestValidateSourceFingerprint_SkillTree_RoundTrip(t *testing.T) {
	home := t.TempDir()
	skill := filepath.Join(home, "skills", "tree")
	if err := os.MkdirAll(filepath.Join(skill, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skill, "SKILL.md"), []byte("---\nname: tree\ndescription: x\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	scriptPath := filepath.Join(skill, "scripts", "x.sh")
	if err := os.WriteFile(scriptPath, []byte("# orig\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	scanned, _, _ := scanSkills(home)
	fp, err := ComputeSkillFingerprint(scanned[0])
	if err != nil {
		t.Fatal(err)
	}
	// Validate against the skill DIR (the path the planner will record for dir layout).
	ok, err := ValidateSourceFingerprint(scanned[0].SrcAbsPath, fp)
	if err != nil || !ok {
		t.Fatalf("expected initial match: ok=%v err=%v", ok, err)
	}
	// Edit a script — validate must now return false.
	if err := os.WriteFile(scriptPath, []byte("# changed and longer\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ok, err = ValidateSourceFingerprint(scanned[0].SrcAbsPath, fp)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if ok {
		t.Error("expected mismatch after script edit (the applier-side TOCTOU defense)")
	}
}
