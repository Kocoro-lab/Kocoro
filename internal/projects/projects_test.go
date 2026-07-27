package projects

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCreateLoadListUpdateDelete(t *testing.T) {
	dir := t.TempDir()

	// Create
	p, err := CreateProject(dir, &ProjectCreateRequest{
		Name:         "武汉之旅",
		Description:  "3 天亲子游",
		Instructions: "带老人，爱吃辣",
	})
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if p.ID == "" || p.Name != "武汉之旅" {
		t.Fatalf("unexpected project: %+v", p)
	}
	if p.Instructions != "带老人，爱吃辣" {
		t.Fatalf("instructions not persisted: %q", p.Instructions)
	}

	// on-disk layout
	if _, err := os.Stat(filepath.Join(dir, p.ID, metaFile)); err != nil {
		t.Fatalf("project.yaml missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, p.ID, instructionsFile)); err != nil {
		t.Fatalf("instructions.md missing: %v", err)
	}

	// Load
	got, err := LoadProject(dir, p.ID)
	if err != nil {
		t.Fatalf("LoadProject: %v", err)
	}
	if got.Name != "武汉之旅" || got.Description != "3 天亲子游" {
		t.Fatalf("load mismatch: %+v", got)
	}

	// A second project + List order (most-recent first)
	p2, err := CreateProject(dir, &ProjectCreateRequest{Name: "西安之旅"})
	if err != nil {
		t.Fatalf("CreateProject 2: %v", err)
	}
	list, err := ListProjects(dir)
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("expected 2 projects, got %d", len(list))
	}
	if list[0].ID != p2.ID {
		t.Fatalf("expected most-recent (%s) first, got %s", p2.ID, list[0].ID)
	}

	// Update: change name + set memory + clear instructions
	empty := ""
	newName := "武汉三日游"
	mem := "已订汉口住宿"
	updated, err := UpdateProject(dir, p.ID, &ProjectUpdateRequest{
		Name:         &newName,
		Instructions: &empty, // clear
		Memory:       &mem,
	})
	if err != nil {
		t.Fatalf("UpdateProject: %v", err)
	}
	if updated.Name != newName {
		t.Fatalf("name not updated: %q", updated.Name)
	}
	if updated.Instructions != "" {
		t.Fatalf("instructions not cleared: %q", updated.Instructions)
	}
	if updated.Memory != mem {
		t.Fatalf("memory not set: %q", updated.Memory)
	}
	if _, err := os.Stat(filepath.Join(dir, p.ID, instructionsFile)); !os.IsNotExist(err) {
		t.Fatalf("instructions.md should be removed after clear")
	}
	// MEMORY.md lives where MemoryDir points, so injection + write-back agree.
	if _, err := os.Stat(filepath.Join(MemoryDir(dir, p.ID), "MEMORY.md")); err != nil {
		t.Fatalf("MEMORY.md missing at MemoryDir: %v", err)
	}

	// Delete
	if err := DeleteProjectDir(dir, p.ID); err != nil {
		t.Fatalf("DeleteProjectDir: %v", err)
	}
	if _, err := LoadProject(dir, p.ID); err == nil {
		t.Fatalf("expected load to fail after delete")
	}
}

func TestValidateProjectID(t *testing.T) {
	valid := []string{"proj-abc123", "a", "my_project-1"}
	for _, v := range valid {
		if err := ValidateProjectID(v); err != nil {
			t.Errorf("expected %q valid: %v", v, err)
		}
	}
	invalid := []string{"", "-bad", "UPPER", "has space", "../escape", "a/b"}
	for _, v := range invalid {
		if err := ValidateProjectID(v); err == nil {
			t.Errorf("expected %q invalid", v)
		}
	}
}

func TestValidateProjectName(t *testing.T) {
	if err := ValidateProjectName("  "); err == nil {
		t.Error("blank name should be invalid")
	}
	if err := ValidateProjectName("正常名字"); err != nil {
		t.Errorf("valid name rejected: %v", err)
	}
	if err := ValidateProjectName("bad\x00name"); err == nil {
		t.Error("control char should be invalid")
	}
}

func TestListProjectsMissingDir(t *testing.T) {
	list, err := ListProjects(filepath.Join(t.TempDir(), "does-not-exist"))
	if err != nil {
		t.Fatalf("missing dir should not error: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("expected empty, got %d", len(list))
	}
}
