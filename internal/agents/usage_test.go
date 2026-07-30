package agents

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestAgentsAttachingSkill(t *testing.T) {
	agentsDir := t.TempDir()

	// Agent "coder" attaches self-improving-agent + other-skill.
	if err := SetAttachedSkills(agentsDir, "coder", []string{"self-improving-agent", "other-skill"}); err != nil {
		t.Fatalf("SetAttachedSkills coder: %v", err)
	}
	// Agent "researcher" attaches self-improving-agent only.
	if err := SetAttachedSkills(agentsDir, "researcher", []string{"self-improving-agent"}); err != nil {
		t.Fatalf("SetAttachedSkills researcher: %v", err)
	}
	// Agent "writer" attaches nothing — no manifest file at all.
	// (Create the directory so the walk sees it.)
	if err := os.MkdirAll(filepath.Join(agentsDir, "writer"), 0700); err != nil {
		t.Fatalf("mkdir writer: %v", err)
	}

	got, err := AgentsAttachingSkill(agentsDir, "self-improving-agent")
	if err != nil {
		t.Fatalf("AgentsAttachingSkill: %v", err)
	}
	want := []string{"coder", "researcher"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}

	// Skill not attached anywhere → empty slice, not nil, for clean JSON marshalling.
	got, err = AgentsAttachingSkill(agentsDir, "unused-skill")
	if err != nil {
		t.Fatalf("AgentsAttachingSkill unused: %v", err)
	}
	if got == nil || len(got) != 0 {
		t.Errorf("expected empty slice for unused skill, got %v", got)
	}
}

func TestDetachSkillAliasesFromAllAgents(t *testing.T) {
	agentsDir := t.TempDir()
	if err := SetAttachedSkills(agentsDir, "analyst", []string{"Docker", "other"}); err != nil {
		t.Fatalf("SetAttachedSkills analyst: %v", err)
	}
	if err := SetAttachedSkills(agentsDir, "operator", []string{"docker"}); err != nil {
		t.Fatalf("SetAttachedSkills operator: %v", err)
	}
	if err := SetAttachedSkills(agentsDir, "writer", []string{"other"}); err != nil {
		t.Fatalf("SetAttachedSkills writer: %v", err)
	}

	got, err := DetachSkillAliasesFromAllAgents(agentsDir, "docker", "Docker")
	if err != nil {
		t.Fatalf("DetachSkillAliasesFromAllAgents: %v", err)
	}
	want := []string{"analyst", "operator"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("detached agents = %v, want %v", got, want)
	}

	analyst, err := ReadAttachedSkills(agentsDir, "analyst")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(analyst, []string{"other"}) {
		t.Errorf("analyst skills = %v, want [other]", analyst)
	}
	if _, err := os.Stat(filepath.Join(agentsDir, "operator", "_attached.yaml")); !os.IsNotExist(err) {
		t.Errorf("operator manifest should be removed, stat error = %v", err)
	}
	writer, err := ReadAttachedSkills(agentsDir, "writer")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(writer, []string{"other"}) {
		t.Errorf("writer skills changed: %v", writer)
	}
}

func TestDetachSkillAliasesFromAllAgentsRejectsCorruptManifestBeforeWriting(t *testing.T) {
	agentsDir := t.TempDir()
	if err := SetAttachedSkills(agentsDir, "analyst", []string{"docker"}); err != nil {
		t.Fatalf("SetAttachedSkills analyst: %v", err)
	}
	corruptDir := filepath.Join(agentsDir, "broken")
	if err := os.MkdirAll(corruptDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(corruptDir, "_attached.yaml"), []byte("not: a-list\n"), 0600); err != nil {
		t.Fatal(err)
	}

	if _, err := DetachSkillAliasesFromAllAgents(agentsDir, "docker"); err == nil {
		t.Fatal("expected corrupt manifest error")
	}
	got, err := ReadAttachedSkills(agentsDir, "analyst")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{"docker"}) {
		t.Errorf("analyst was modified before validation completed: %v", got)
	}
}

func TestPruneDanglingSkillAttachments(t *testing.T) {
	shannonDir := t.TempDir()
	agentsDir := filepath.Join(shannonDir, "agents")
	writeUsageTestSkill(t, shannonDir, "docker", "Docker", true)
	writeUsageTestSkill(t, shannonDir, "temporarily-broken", "Temporary Broken", false)

	if err := SetAttachedSkills(agentsDir, "analyst", []string{
		"Docker",
		"missing-one",
		"Temporary Broken",
	}); err != nil {
		t.Fatalf("SetAttachedSkills analyst: %v", err)
	}
	if err := SetAttachedSkills(agentsDir, "operator", []string{"docker", "missing-two"}); err != nil {
		t.Fatalf("SetAttachedSkills operator: %v", err)
	}

	got, err := PruneDanglingSkillAttachments(agentsDir, shannonDir)
	if err != nil {
		t.Fatalf("PruneDanglingSkillAttachments: %v", err)
	}
	want := []SkillAttachmentCleanup{
		{Agent: "analyst", Skills: []string{"missing-one"}},
		{Agent: "operator", Skills: []string{"missing-two"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("cleanup = %#v, want %#v", got, want)
	}

	analyst, err := ReadAttachedSkills(agentsDir, "analyst")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(analyst, []string{"Docker", "Temporary Broken"}) {
		t.Errorf("analyst skills = %v", analyst)
	}
	operator, err := ReadAttachedSkills(agentsDir, "operator")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(operator, []string{"docker"}) {
		t.Errorf("operator skills = %v", operator)
	}
}

func TestPruneDanglingSkillAttachmentsRefusesUnparseableIdentity(t *testing.T) {
	shannonDir := t.TempDir()
	agentsDir := filepath.Join(shannonDir, "agents")
	writeUsageTestSkill(t, shannonDir, "broken", "", false)
	if err := SetAttachedSkills(agentsDir, "analyst", []string{"Legacy Broken", "missing"}); err != nil {
		t.Fatal(err)
	}

	if _, err := PruneDanglingSkillAttachments(agentsDir, shannonDir); err == nil {
		t.Fatal("expected unsafe alias resolution to stop pruning")
	}
	got, err := ReadAttachedSkills(agentsDir, "analyst")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{"Legacy Broken", "missing"}) {
		t.Fatalf("manifest changed after conservative prune failure: %v", got)
	}
}

func writeUsageTestSkill(t *testing.T, shannonDir, slug, name string, valid bool) {
	t.Helper()
	dir := filepath.Join(shannonDir, "skills", slug)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	content := "not valid skill frontmatter\n"
	if valid {
		content = "---\nname: " + name + "\ndescription: test\n---\nbody\n"
	} else if name != "" {
		content = "---\nname: " + name + "\n---\nbody\n"
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}
