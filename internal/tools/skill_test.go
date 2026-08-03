package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/skills"
)

func TestUseSkill_HappyPath(t *testing.T) {
	s := &skills.Skill{
		Name: "pdf", Description: "Extract text from PDFs",
		Prompt: "# PDF Guide\n\nUse pypdf to extract text.", Dir: t.TempDir(),
	}
	skillList := []*skills.Skill{s}
	tool := newUseSkillTool(&skillList)

	args, _ := json.Marshal(map[string]string{"skill_name": "pdf"})
	result, err := tool.Run(context.Background(), string(args))
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if result.IsError {
		t.Error("should not be error")
	}
	if !strings.Contains(result.Content, "# PDF Guide") {
		t.Errorf("missing body: %s", result.Content)
	}
}

func TestUseSkill_WithArgs(t *testing.T) {
	s := &skills.Skill{Name: "pdf", Prompt: "# PDF Guide", Dir: t.TempDir()}
	skillList := []*skills.Skill{s}
	tool := newUseSkillTool(&skillList)

	args, _ := json.Marshal(map[string]string{"skill_name": "pdf", "args": "merge two PDFs"})
	result, err := tool.Run(context.Background(), string(args))
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if !strings.Contains(result.Content, "## User Context") {
		t.Error("missing User Context")
	}
	if !strings.Contains(result.Content, "merge two PDFs") {
		t.Error("missing args")
	}
}

func TestUseSkill_UnknownSkill(t *testing.T) {
	s := &skills.Skill{Name: "pdf", Prompt: "body"}
	skillList := []*skills.Skill{s}
	tool := newUseSkillTool(&skillList)

	args, _ := json.Marshal(map[string]string{"skill_name": "nonexistent"})
	result, err := tool.Run(context.Background(), string(args))
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if !result.IsError {
		t.Fatal("should be error")
	}
	if !strings.Contains(result.Content, "pdf") {
		t.Errorf("should list available: %s", result.Content)
	}
}

func TestUseSkill_NoSkills(t *testing.T) {
	var skillList []*skills.Skill
	tool := newUseSkillTool(&skillList)

	args, _ := json.Marshal(map[string]string{"skill_name": "anything"})
	result, err := tool.Run(context.Background(), string(args))
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if !result.IsError {
		t.Fatal("should be error")
	}
}

func TestUseSkill_RelativePathRewrite(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "scripts"), 0o755)
	os.WriteFile(filepath.Join(dir, "scripts", "extract.py"), []byte("print('hi')"), 0o644)

	s := &skills.Skill{Name: "pdf", Prompt: "Run scripts/extract.py to extract.", Dir: dir}
	skillList := []*skills.Skill{s}
	tool := newUseSkillTool(&skillList)

	args, _ := json.Marshal(map[string]string{"skill_name": "pdf"})
	result, err := tool.Run(context.Background(), string(args))
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	expected := filepath.Join(dir, "scripts/extract.py")
	if !strings.Contains(result.Content, expected) {
		t.Errorf("expected absolute path %s in: %s", expected, result.Content)
	}
}

func TestUseSkill_PromptNeverContainsSecretValues(t *testing.T) {
	// Regression test: skill prompts with $KEY references MUST be returned
	// verbatim — secret values must never be substituted into the content
	// that goes into the session transcript.
	s := &skills.Skill{
		Name:   "my-skill",
		Prompt: "Run: curl -H \"Authorization: $MY_API_KEY\" https://api.example.com",
		Dir:    t.TempDir(),
	}
	skillList := []*skills.Skill{s}
	tool := newUseSkillTool(&skillList)

	args, _ := json.Marshal(map[string]string{"skill_name": "my-skill"})
	result, err := tool.Run(context.Background(), string(args))
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if !strings.Contains(result.Content, "$MY_API_KEY") {
		t.Errorf("prompt must retain $MY_API_KEY literally, got: %s", result.Content)
	}
}

func TestUseSkill_RegistersActivatedSkill(t *testing.T) {
	s := &skills.Skill{Name: "my-skill", Slug: "my-skill", Prompt: "body", Dir: t.TempDir()}
	skillList := []*skills.Skill{s}
	tool := newUseSkillTool(&skillList)

	set := skills.NewActivatedSet()
	ctx := skills.WithActivatedSet(context.Background(), set)

	args, _ := json.Marshal(map[string]string{"skill_name": "my-skill"})
	if _, err := tool.Run(ctx, string(args)); err != nil {
		t.Fatalf("error: %v", err)
	}

	names := set.Names()
	if len(names) != 1 || names[0] != "my-skill" {
		t.Errorf("expected activated set to contain [my-skill], got %v", names)
	}
}

// TestUseSkill_RegistersSlugWhenNameDiffers ensures activation uses the
// on-disk Slug (the key SecretsStore is indexed by) rather than the
// frontmatter Name when the two differ. Regression target: xiaohongshu-
// mcp-skills where Name="xiaohongshu" but secrets live under
// Slug="xiaohongshu-mcp-skills".
func TestUseSkill_RegistersSlugWhenNameDiffers(t *testing.T) {
	s := &skills.Skill{Name: "xiaohongshu", Slug: "xiaohongshu-mcp-skills", Prompt: "body", Dir: t.TempDir()}
	skillList := []*skills.Skill{s}
	tool := newUseSkillTool(&skillList)

	set := skills.NewActivatedSet()
	ctx := skills.WithActivatedSet(context.Background(), set)

	// LLM activates by Name (what it sees in the "Available Skills" list).
	args, _ := json.Marshal(map[string]string{"skill_name": "xiaohongshu"})
	if _, err := tool.Run(ctx, string(args)); err != nil {
		t.Fatalf("error: %v", err)
	}

	names := set.Names()
	if len(names) != 1 || names[0] != "xiaohongshu-mcp-skills" {
		t.Errorf("expected activated set to contain the Slug [xiaohongshu-mcp-skills], got %v", names)
	}
}

// TestUseSkill_ActivationBySlug covers the fallback path: some callers may
// address the skill by its Slug (directory name) instead of frontmatter
// Name. Both must resolve to the same skill and register the Slug.
func TestUseSkill_ActivationBySlug(t *testing.T) {
	s := &skills.Skill{Name: "xiaohongshu", Slug: "xiaohongshu-mcp-skills", Prompt: "body", Dir: t.TempDir()}
	skillList := []*skills.Skill{s}
	tool := newUseSkillTool(&skillList)

	set := skills.NewActivatedSet()
	ctx := skills.WithActivatedSet(context.Background(), set)

	args, _ := json.Marshal(map[string]string{"skill_name": "xiaohongshu-mcp-skills"})
	if _, err := tool.Run(ctx, string(args)); err != nil {
		t.Fatalf("error: %v", err)
	}

	names := set.Names()
	if len(names) != 1 || names[0] != "xiaohongshu-mcp-skills" {
		t.Errorf("expected activated set to contain [xiaohongshu-mcp-skills], got %v", names)
	}
}

func TestUseSkill_NoActivatedSetInContext_NoPanic(t *testing.T) {
	// Tools called without an activated set (e.g. in non-daemon contexts)
	// must not crash — Add on nil set is a no-op.
	s := &skills.Skill{Name: "my-skill", Prompt: "body", Dir: t.TempDir()}
	skillList := []*skills.Skill{s}
	tool := newUseSkillTool(&skillList)

	args, _ := json.Marshal(map[string]string{"skill_name": "my-skill"})
	if _, err := tool.Run(context.Background(), string(args)); err != nil {
		t.Fatalf("error: %v", err)
	}
}

func TestUseSkill_StatesSkillDirectory(t *testing.T) {
	// A skill that ships code OUTSIDE the scripts/references/assets rewrite
	// convention (e.g. core/ imported as Python modules) previously gave the
	// model no path anchor at all — observed failure: a disk-wide `find /`
	// hunting for the bundled files. The absolute directory must always be
	// stated up front.
	dir := t.TempDir()
	s := &skills.Skill{Name: "gif", Prompt: "from core.gif_builder import GIFBuilder", Dir: dir}
	skillList := []*skills.Skill{s}
	tool := newUseSkillTool(&skillList)

	args, _ := json.Marshal(map[string]string{"skill_name": "gif"})
	result, err := tool.Run(context.Background(), string(args))
	if err != nil || result.IsError {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if !strings.Contains(result.Content, "Skill directory: "+dir) {
		t.Fatalf("skill directory not stated:\n%s", result.Content)
	}
}

func TestMaterialSideEffectClassifications(t *testing.T) {
	// These feed the daemon's offer-before-side-effects invariant: non-read-
	// only flags that exist for scheduling must not count as material side
	// effects (production false-positive, 2026-08-03).
	skillList := []*skills.Skill{}
	if (&AskUserQuestionTool{}).HasMaterialSideEffect(`{"questions":[]}`) {
		t.Fatal("ask_user_question misclassified as a material side effect")
	}
	if newUseSkillTool(&skillList).HasMaterialSideEffect(`{"skill_name":"x"}`) {
		t.Fatal("use_skill misclassified as a material side effect")
	}
	p := &ProcessTool{}
	if p.HasMaterialSideEffect(`{"action":"list","description":"d"}`) || p.HasMaterialSideEffect(`{"action":"ports","description":"d"}`) {
		t.Fatal("process observation actions misclassified as material side effects")
	}
	if !p.HasMaterialSideEffect(`{"action":"kill","pid":1,"description":"d"}`) || !p.HasMaterialSideEffect(`not-json`) {
		t.Fatal("process kill / unparseable args must count as material side effects")
	}
}
