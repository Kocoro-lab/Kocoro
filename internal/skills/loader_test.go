package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func createSkillDir(t *testing.T, base, name, content string) {
	t.Helper()
	dir := filepath.Join(base, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadSkills_BasicParsing(t *testing.T) {
	tmp := t.TempDir()
	createSkillDir(t, tmp, "pdf", "---\nname: pdf\ndescription: Extract text from PDFs\nlicense: MIT\n---\n\n# PDF Processing\n\nUse pypdf to extract text.\n")

	skills, err := LoadSkills(SkillSource{Dir: tmp, Source: "global"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(skills) != 1 {
		t.Fatalf("expected 1 skill, got %d", len(skills))
	}
	s := skills[0]
	if s.Name != "pdf" {
		t.Errorf("name = %q, want pdf", s.Name)
	}
	if s.Description != "Extract text from PDFs" {
		t.Errorf("description = %q", s.Description)
	}
	if s.License != "MIT" {
		t.Errorf("license = %q", s.License)
	}
	if !strings.Contains(s.Prompt, "# PDF Processing") {
		t.Errorf("prompt missing body")
	}
	if s.Source != "global" {
		t.Errorf("source = %q", s.Source)
	}
	if s.Dir != filepath.Join(tmp, "pdf") {
		t.Errorf("dir = %q", s.Dir)
	}
}

func TestLoadSkills_PriorityDedup(t *testing.T) {
	agentDir := t.TempDir()
	globalDir := t.TempDir()
	createSkillDir(t, agentDir, "pdf", "---\nname: pdf\ndescription: Agent PDF\n---\nAgent version.")
	createSkillDir(t, globalDir, "pdf", "---\nname: pdf\ndescription: Global PDF\n---\nGlobal version.")
	createSkillDir(t, globalDir, "xlsx", "---\nname: xlsx\ndescription: Spreadsheet\n---\nXLSX.")

	skills, err := LoadSkills(
		SkillSource{Dir: agentDir, Source: "agent:mybot"},
		SkillSource{Dir: globalDir, Source: "global"},
	)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if len(skills) != 2 {
		t.Fatalf("expected 2, got %d", len(skills))
	}
	var pdf *Skill
	for _, s := range skills {
		if s.Name == "pdf" {
			pdf = s
		}
	}
	if pdf == nil {
		t.Fatal("pdf not found")
	}
	if pdf.Source != "agent:mybot" {
		t.Errorf("pdf source = %q", pdf.Source)
	}
	if !strings.Contains(pdf.Prompt, "Agent version") {
		t.Error("agent pdf should win")
	}
}

func TestLoadSkills_NameMismatch(t *testing.T) {
	tmp := t.TempDir()
	createSkillDir(t, tmp, "pdf", "---\nname: wrong-name\ndescription: Mismatch\n---\nBody.")
	_, err := LoadSkills(SkillSource{Dir: tmp, Source: "global"})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestLoadSkills_LegacyYAML(t *testing.T) {
	tmp := t.TempDir()
	os.WriteFile(filepath.Join(tmp, "old.yaml"), []byte("name: old"), 0o644)
	skills, err := LoadSkills(SkillSource{Dir: tmp, Source: "global"})
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if len(skills) != 0 {
		t.Errorf("expected 0, got %d", len(skills))
	}
}

func TestLoadSkills_EmptyDir(t *testing.T) {
	tmp := t.TempDir()
	skills, err := LoadSkills(SkillSource{Dir: tmp, Source: "global"})
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if len(skills) != 0 {
		t.Errorf("expected 0, got %d", len(skills))
	}
}

func TestLoadSkills_NonexistentDir(t *testing.T) {
	skills, err := LoadSkills(SkillSource{Dir: "/nonexistent", Source: "global"})
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if skills != nil {
		t.Errorf("expected nil")
	}
}

func TestLoadSkills_Integration(t *testing.T) {
	agentDir := t.TempDir()
	globalDir := t.TempDir()

	// Agent skill shadows global
	createSkillDir(t, agentDir, "pdf", "---\nname: pdf\ndescription: Agent PDF\n---\n# Agent PDF Guide")
	// Global skills
	createSkillDir(t, globalDir, "pdf", "---\nname: pdf\ndescription: Global PDF\n---\n# Global PDF Guide")
	createSkillDir(t, globalDir, "xlsx", "---\nname: xlsx\ndescription: Spreadsheet processing\n---\n# XLSX Guide")

	loaded, err := LoadSkills(
		SkillSource{Dir: agentDir, Source: "agent:test"},
		SkillSource{Dir: globalDir, Source: "global"},
	)
	if err != nil {
		t.Fatalf("load error: %v", err)
	}
	if len(loaded) != 2 {
		t.Fatalf("expected 2 skills (deduped), got %d", len(loaded))
	}

	var pdf, xlsx *Skill
	for _, s := range loaded {
		switch s.Name {
		case "pdf":
			pdf = s
		case "xlsx":
			xlsx = s
		}
	}

	// Agent pdf shadows global
	if pdf == nil {
		t.Fatal("pdf not found")
	}
	if pdf.Source != "agent:test" {
		t.Errorf("pdf source = %q, want agent:test", pdf.Source)
	}
	if !strings.Contains(pdf.Prompt, "Agent PDF Guide") {
		t.Error("agent pdf should shadow global")
	}

	// Global xlsx loaded
	if xlsx == nil {
		t.Fatal("xlsx not found")
	}
	if xlsx.Source != "global" {
		t.Errorf("xlsx source = %q, want global", xlsx.Source)
	}

	// Sorted order
	if loaded[0].Name != "pdf" || loaded[1].Name != "xlsx" {
		t.Errorf("expected [pdf, xlsx], got [%s, %s]", loaded[0].Name, loaded[1].Name)
	}
}

func TestLoadSkills_Sorted(t *testing.T) {
	tmp := t.TempDir()
	createSkillDir(t, tmp, "zebra", "---\nname: zebra\ndescription: Z\n---\nZ")
	createSkillDir(t, tmp, "alpha", "---\nname: alpha\ndescription: A\n---\nA")
	skills, err := LoadSkills(SkillSource{Dir: tmp, Source: "global"})
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if len(skills) != 2 {
		t.Fatalf("expected 2, got %d", len(skills))
	}
	if skills[0].Name != "alpha" {
		t.Errorf("expected alpha first, got %s", skills[0].Name)
	}
}

func TestLoadSkills_InstallProvenance(t *testing.T) {
	globalDir := t.TempDir()
	bundledDir := t.TempDir()

	createSkillDir(t, globalDir, "local-skill", "---\nname: local-skill\ndescription: Local\n---\nlocal")
	createSkillDir(t, globalDir, "market-skill", "---\nname: market-skill\ndescription: Market\n---\nmarket")
	createSkillDir(t, bundledDir, "bundled-skill", "---\nname: bundled-skill\ndescription: Bundled\n---\nbundled")

	if err := writeMarketplaceProvenance(filepath.Join(globalDir, "market-skill"), "market-skill"); err != nil {
		t.Fatalf("write provenance: %v", err)
	}

	loaded, err := LoadSkills(
		SkillSource{Dir: globalDir, Source: SourceGlobal},
		SkillSource{Dir: bundledDir, Source: SourceBundled},
	)
	if err != nil {
		t.Fatalf("load error: %v", err)
	}

	got := make(map[string]*Skill, len(loaded))
	for _, skill := range loaded {
		got[skill.Name] = skill
	}

	if got["local-skill"].InstallSource != InstallSourceLocal {
		t.Errorf("local-skill install source = %q, want %q", got["local-skill"].InstallSource, InstallSourceLocal)
	}
	if got["local-skill"].MarketplaceSlug != "" {
		t.Errorf("local-skill marketplace slug = %q, want empty", got["local-skill"].MarketplaceSlug)
	}

	if got["market-skill"].InstallSource != InstallSourceMarketplace {
		t.Errorf("market-skill install source = %q, want %q", got["market-skill"].InstallSource, InstallSourceMarketplace)
	}
	if got["market-skill"].MarketplaceSlug != "market-skill" {
		t.Errorf("market-skill marketplace slug = %q, want market-skill", got["market-skill"].MarketplaceSlug)
	}

	if got["bundled-skill"].InstallSource != InstallSourceBundled {
		t.Errorf("bundled-skill install source = %q, want %q", got["bundled-skill"].InstallSource, InstallSourceBundled)
	}
}

func TestWriteGlobalSkillClearsMarketplaceProvenance(t *testing.T) {
	shannonDir := t.TempDir()
	skillDir := filepath.Join(shannonDir, "skills", "ontology")

	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := writeMarketplaceProvenance(skillDir, "ontology"); err != nil {
		t.Fatalf("write provenance: %v", err)
	}

	err := WriteGlobalSkill(shannonDir, &Skill{
		Name:        "ontology",
		Description: "Local replacement",
		Prompt:      "# local body",
	})
	if err != nil {
		t.Fatalf("WriteGlobalSkill: %v", err)
	}

	if _, err := os.Stat(filepath.Join(skillDir, marketplaceProvenanceFile)); !os.IsNotExist(err) {
		t.Fatalf("provenance marker should be removed, stat err = %v", err)
	}

	loaded, err := LoadSkills(SkillSource{Dir: filepath.Join(shannonDir, "skills"), Source: SourceGlobal})
	if err != nil {
		t.Fatalf("LoadSkills: %v", err)
	}
	if len(loaded) != 1 {
		t.Fatalf("expected 1 skill, got %d", len(loaded))
	}
	if loaded[0].InstallSource != InstallSourceLocal {
		t.Errorf("install source = %q, want %q", loaded[0].InstallSource, InstallSourceLocal)
	}
	if loaded[0].MarketplaceSlug != "" {
		t.Errorf("marketplace slug = %q, want empty", loaded[0].MarketplaceSlug)
	}
}
