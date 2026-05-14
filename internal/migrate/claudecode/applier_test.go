package claudecode

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// --- helpers ---

func buildHappyPlan(t *testing.T) (*Plan, string) {
	t.Helper()
	src := SourcePaths{
		ClaudeHome:       filepath.Join("testdata", "claude_home_basic"),
		ClaudeUserConfig: filepath.Join("testdata", "claude_user_config_basic.json"),
	}
	scan, _ := Scan(src)
	target := t.TempDir()
	p, err := BuildPlan(scan, src, target, "/Users/wayland", time.Now())
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	return p, target
}

// --- Phase A only (test hook stops before commit) ---

func TestApply_PhaseA_StagesNoTargetWrites(t *testing.T) {
	p, target := buildHappyPlan(t)
	a := NewApplier(target)
	a.stopAfterStaging = true

	result, err := a.Apply(p)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if result.Result != "staged_only" {
		t.Errorf("Result = %q, want staged_only", result.Result)
	}
	// Staging entries (suffixed .staging-* / .migrate.tmp) are allowed under
	// target. What's NOT allowed is anything at the planned final destination
	// — those land in Phase B only. Check each planned action's actual target.
	for _, act := range p.PlannedActions {
		if act.Category == "mcp_servers" {
			continue // config.yaml may pre-exist; merge is Phase B
		}
		if _, err := os.Stat(act.DstAbs); err == nil {
			t.Errorf("Phase A wrote to final target for %s/%s: %s", act.Category, act.Name, act.DstAbs)
		}
	}
}

// --- Happy path ---

func TestApply_HappyPath(t *testing.T) {
	p, target := buildHappyPlan(t)
	res, err := NewApplier(target).Apply(p)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if res.Result != "applied" {
		t.Errorf("Result = %q, want applied", res.Result)
	}

	// Sanity: counts match plan, NOT raw scan.
	planned := categoryPlanCounts(p)
	for cat, want := range planned {
		got := getCompletedInt(res.Imported[cat].Completed)
		if got != want {
			t.Errorf("%s.completed = %d, want planned %d", cat, got, want)
		}
	}

	// Sanity: target tree populated for non-MCP categories.
	for _, a := range p.PlannedActions {
		if a.Category == "mcp_servers" || a.Category == "global_rules" {
			continue
		}
		sentinel := dstSentinel(a.Category)
		if _, err := os.Stat(filepath.Join(a.DstAbs, sentinel)); err != nil {
			t.Errorf("%s/%s: target sentinel missing: %v", a.Category, a.Name, err)
		}
	}
	// Global rules sentinel is the file itself.
	for _, a := range p.PlannedActions {
		if a.Category == "global_rules" {
			if _, err := os.Stat(a.DstAbs); err != nil {
				t.Errorf("global_rules target missing: %v", err)
			}
		}
	}
}

// --- Within-category partial: fail on skill #2 of N ---

func TestApply_PhaseB_WithinCategoryPartial(t *testing.T) {
	p, target := buildHappyPlan(t)
	a := NewApplier(target)
	a.testFailOnItem = func(act PlannedAction, idxWithinCategory int) error {
		if act.Category == "skills" && idxWithinCategory == 1 {
			return fmt.Errorf("simulated_rename_fail")
		}
		return nil
	}
	res, err := a.Apply(p)
	if err != nil {
		t.Fatalf("Apply (partial should still return a result): %v", err)
	}
	if res.Result != "partial_applied" {
		t.Errorf("Result = %q, want partial_applied", res.Result)
	}
	if res.Failure == nil || res.Failure.Category != "skills" {
		t.Errorf("Failure should be recorded for skills, got %+v", res.Failure)
	}

	plannedSkills := categoryPlanCounts(p)["skills"]
	gotSkills := getCompletedInt(res.Imported["skills"].Completed)
	if gotSkills != 1 {
		t.Errorf("skills.completed = %d, want 1 (only the first skill committed before failure)", gotSkills)
	}
	if getPlannedInt(res.Imported["skills"].Planned) != plannedSkills {
		t.Errorf("skills.planned = %v, want %d", res.Imported["skills"].Planned, plannedSkills)
	}

	// Categories after skills must report completed=0/false.
	for _, cat := range []string{"agents", "commands", "global_rules", "mcp_servers"} {
		switch v := res.Imported[cat].Completed.(type) {
		case int:
			if v != 0 {
				t.Errorf("%s.completed = %d, want 0 after skills failure", cat, v)
			}
		case bool:
			if v {
				t.Errorf("%s.completed = true, want false after skills failure", cat)
			}
		}
	}
}

// --- Mutex: second Apply while first is in flight returns migration_in_progress ---

func TestApply_MutexBlocksConcurrent(t *testing.T) {
	p1, target1 := buildHappyPlan(t)
	p2, target2 := buildHappyPlan(t)

	a1 := NewApplier(target1)
	a1.testFailOnItem = func(_ PlannedAction, _ int) error {
		time.Sleep(200 * time.Millisecond)
		return nil
	}
	done := make(chan struct{})
	go func() {
		_, _ = a1.Apply(p1)
		close(done)
	}()
	time.Sleep(50 * time.Millisecond)

	_, err := NewApplier(target2).Apply(p2)
	if err == nil || !strings.Contains(err.Error(), "migration_in_progress") {
		t.Errorf("expected migration_in_progress, got %v", err)
	}
	<-done
}

// --- Plan-staleness: source mutation between BuildPlan and Apply → 409 ---

func TestApply_RejectsStaleSource(t *testing.T) {
	// Build plan against a private claude_home so we can safely mutate the source.
	home := t.TempDir()
	skillsDir := filepath.Join(home, "skills")
	if err := os.MkdirAll(skillsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(skillsDir, "mutating.md")
	if err := os.WriteFile(src, []byte("---\nname: mutating\ndescription: x\n---\nbody v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	srcPaths := SourcePaths{ClaudeHome: home, ClaudeUserConfig: filepath.Join(t.TempDir(), "none.json")}
	scan, _ := Scan(srcPaths)
	target := t.TempDir()
	p, _ := BuildPlan(scan, srcPaths, target, "/Users/wayland", time.Now())

	// Mutate the source file after plan, before apply.
	if err := os.WriteFile(src, []byte("MUTATED CONTENT\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := NewApplier(target).Apply(p)
	if err == nil || !strings.Contains(err.Error(), "plan_stale") {
		t.Errorf("expected plan_stale error after source mutation, got %v", err)
	}
	// Target must be untouched.
	if _, err := os.Stat(filepath.Join(target, "skills", "mutating")); err == nil {
		t.Error("stale apply must not write to target")
	}
}

// --- Partial apply must not leave staging behind ---

func TestApply_PhaseB_PartialCleansLeftoverStaging(t *testing.T) {
	p, target := buildHappyPlan(t)
	a := NewApplier(target)
	// Fail on the second skill so #1 commits, #2 fails, and all subsequent
	// staged actions (agents/commands/rules/mcp) are abandoned.
	a.testFailOnItem = func(act PlannedAction, idx int) error {
		if act.Category == "skills" && idx == 1 {
			return fmt.Errorf("simulated_fail")
		}
		return nil
	}
	res, err := a.Apply(p)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if res.Result != "partial_applied" {
		t.Fatalf("Result = %q, want partial_applied", res.Result)
	}

	// Scan target for any *.staging-* or *.migrate.tmp leftovers.
	leftover := []string{}
	err = filepath.Walk(target, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		name := filepath.Base(path)
		if strings.Contains(name, ".staging-") || strings.HasSuffix(name, ".migrate.tmp") {
			leftover = append(leftover, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(leftover) > 0 {
		t.Errorf("partial apply left staging behind:\n  %s", strings.Join(leftover, "\n  "))
	}
}

// --- MCP target conflict added after preview → plan_stale ---

func TestApply_RejectsMCPTargetConflictAddedAfterPreview(t *testing.T) {
	p, target := buildHappyPlan(t)
	// Pick any planned MCP server name.
	var mcpName string
	for _, a := range p.PlannedActions {
		if a.Category == "mcp_servers" {
			mcpName = a.Name
			break
		}
	}
	if mcpName == "" {
		t.Skip("fixture has no planned MCP servers")
	}
	// Simulate the user editing config.yaml between preview and apply.
	if err := writeFile(t, filepath.Join(target, "config.yaml"),
		fmt.Sprintf("mcp_servers:\n  %s:\n    command: user-added\n", mcpName)); err != nil {
		t.Fatal(err)
	}
	_, err := NewApplier(target).Apply(p)
	if err == nil || !strings.Contains(err.Error(), "plan_stale") {
		t.Errorf("expected plan_stale for MCP target_conflict_added, got %v", err)
	}
	if err != nil && !strings.Contains(err.Error(), "mcp_servers/"+mcpName) {
		t.Errorf("error should name the conflicting server %q: %v", mcpName, err)
	}
}

// --- Applied manifest write failure must surface in response ---

func TestApply_ManifestWriteFailure_Surfaced(t *testing.T) {
	p, target := buildHappyPlan(t)
	a := NewApplier(target)
	a.testAppliedManifestError = fmt.Errorf("disk full")

	res, err := a.Apply(p)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	// Items DID land — Result should still be applied.
	if res.Result != "applied" {
		t.Errorf("Result = %q, want applied (items renamed)", res.Result)
	}
	if res.ManifestID != "" {
		t.Errorf("ManifestID should be cleared when audit record missing, got %q", res.ManifestID)
	}
	if res.Failure == nil || res.Failure.Reason != "manifest_write_failed" {
		t.Errorf("expected Failure.Reason=manifest_write_failed, got %+v", res.Failure)
	}
}

// --- helpers ---

func categoryPlanCounts(p *Plan) map[string]int {
	c := map[string]int{}
	for _, a := range p.PlannedActions {
		c[a.Category]++
	}
	return c
}

func getCompletedInt(v any) int {
	if n, ok := v.(int); ok {
		return n
	}
	if b, ok := v.(bool); ok && b {
		return 1
	}
	return 0
}

func getPlannedInt(v any) int { return getCompletedInt(v) }
