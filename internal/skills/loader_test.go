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

// Regression: ClawHub skills commonly declare `allowed-tools` as a YAML list.
// A `string`-typed field made yaml.Unmarshal fail ("cannot unmarshal !!seq into
// string"), which errored loadSkillMD → 422 on marketplace install AND fail-open
// skip from GET /skills. stringOrList must accept both the list and scalar forms.
func TestLoadSkills_AllowedTools_ListAndScalar(t *testing.T) {
	want := []string{"read_file", "write_to_file", "run_command"}

	t.Run("list form", func(t *testing.T) {
		tmp := t.TempDir()
		createSkillDir(t, tmp, "listy", "---\nname: listy\ndescription: uses list-form tools\nallowed-tools:\n  - read_file\n  - write_to_file\n  - run_command\n---\n\nBody.\n")
		skills, err := LoadSkills(SkillSource{Dir: tmp, Source: "global"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(skills) != 1 {
			t.Fatalf("list-form skill was skipped: expected 1 skill, got %d", len(skills))
		}
		if strings.Join(skills[0].AllowedTools, " ") != strings.Join(want, " ") {
			t.Errorf("allowed tools = %v, want %v", skills[0].AllowedTools, want)
		}
	})

	t.Run("scalar form", func(t *testing.T) {
		tmp := t.TempDir()
		createSkillDir(t, tmp, "scalary", "---\nname: scalary\ndescription: uses scalar-form tools\nallowed-tools: read_file write_to_file run_command\n---\n\nBody.\n")
		skills, err := LoadSkills(SkillSource{Dir: tmp, Source: "global"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(skills) != 1 {
			t.Fatalf("expected 1 skill, got %d", len(skills))
		}
		if strings.Join(skills[0].AllowedTools, " ") != strings.Join(want, " ") {
			t.Errorf("allowed tools = %v, want %v", skills[0].AllowedTools, want)
		}
	})
}

// The list form must survive a WriteGlobalSkill → LoadSkills round-trip, and the
// on-disk form stays the spec-compliant scalar string (stringOrList.MarshalYAML).
func TestWriteGlobalSkill_AllowedTools_RoundTrip(t *testing.T) {
	tmp := t.TempDir()
	want := []string{"read_file", "write_to_file"}
	if err := WriteGlobalSkill(tmp, &Skill{
		Name:         "rt",
		Slug:         "rt",
		Description:  "round-trip",
		Prompt:       "Body.",
		AllowedTools: want,
	}); err != nil {
		t.Fatalf("WriteGlobalSkill: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(tmp, "skills", "rt", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "allowed-tools: read_file write_to_file") {
		t.Errorf("on-disk allowed-tools not scalar form:\n%s", raw)
	}
	skills, err := LoadSkills(SkillSource{Dir: filepath.Join(tmp, "skills"), Source: "global"})
	if err != nil {
		t.Fatalf("LoadSkills: %v", err)
	}
	if len(skills) != 1 || strings.Join(skills[0].AllowedTools, " ") != strings.Join(want, " ") {
		t.Fatalf("round-trip lost allowed tools: %+v", skills)
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

// TestLoadSkills_NameDifferentFromDir_Loads confirms the name/slug decoupling:
// when frontmatter.name differs from the directory basename, the skill still
// loads. This matches the openclaw/clawhub contract (slug is always derived
// from the directory, name is a free-form label from frontmatter). Regression
// target: ClawHub's xiaohongshu-mcp-skills package which ships with
// `name: xiaohongshu` but installs under slug `xiaohongshu-mcp-skills`.
func TestLoadSkills_NameDifferentFromDir_Loads(t *testing.T) {
	tmp := t.TempDir()
	createSkillDir(t, tmp, "xiaohongshu-mcp-skills", "---\nname: xiaohongshu\ndescription: Mismatch is fine\n---\nBody.")
	loaded, err := LoadSkills(SkillSource{Dir: tmp, Source: "global"})
	if err != nil {
		t.Fatalf("LoadSkills should not error: %v", err)
	}
	if len(loaded) != 1 {
		t.Fatalf("expected the skill to load, got %d skills", len(loaded))
	}
	if loaded[0].Name != "xiaohongshu" {
		t.Errorf("Name should come from frontmatter, got %q", loaded[0].Name)
	}
	if loaded[0].Slug != "xiaohongshu-mcp-skills" {
		t.Errorf("Slug should come from directory name, got %q", loaded[0].Slug)
	}
}

// TestLoadSkills_FailOpenWithGoodAndBad guards the central fail-open contract:
// when one skill in a source is malformed (e.g. broken YAML frontmatter), the
// loader must skip it and still return every other valid skill in the same
// source. Regression target: a real user environment where one skill with a
// nested-map metadata block silently broke ALL global skill loading.
func TestLoadSkills_FailOpenWithGoodAndBad(t *testing.T) {
	tmp := t.TempDir()

	// Valid skill — must survive.
	createSkillDir(t, tmp, "pdf", "---\nname: pdf\ndescription: Read PDFs\n---\n# PDF body")

	// Broken skill — frontmatter parses fine but the description field is a
	// map instead of a string, mirroring the real-world failure observed in
	// the user's environment ("cannot unmarshal !!map into string"). This
	// MUST be skipped, not fatal.
	createSkillDir(t, tmp, "broken", "---\nname: broken\ndescription:\n  nested: oops\n---\n# body")

	// Another valid skill loaded after the broken one in alphabetical order
	// (sorted by directory name): "pdf" comes before "broken"? No — "broken"
	// sorts before "pdf". So this confirms the loader recovers and continues
	// past a mid-stream failure rather than aborting.
	createSkillDir(t, tmp, "zebra", "---\nname: zebra\ndescription: Stripes\n---\n# zebra body")

	skills, err := LoadSkills(SkillSource{Dir: tmp, Source: "global"})
	if err != nil {
		t.Fatalf("LoadSkills must not error when only some skills are bad: %v", err)
	}
	if len(skills) != 2 {
		t.Fatalf("expected 2 valid skills (pdf, zebra) — broken should be skipped — got %d", len(skills))
	}
	got := map[string]bool{}
	for _, s := range skills {
		got[s.Name] = true
	}
	if !got["pdf"] || !got["zebra"] {
		t.Errorf("expected both pdf and zebra to load, got %v", got)
	}
	if got["broken"] {
		t.Error("broken skill should have been skipped, but it was loaded")
	}
}

// TestLoadSkills_BrokenSkillDoesNotShadowLowerSource locks in the part of the
// fail-open fix that intentionally does NOT mark a broken skill as "seen".
// If a broken global skill shadowed a working bundled skill of the same name,
// users would be silently downgraded; instead, the bundled version must take
// over when the higher-priority source is malformed.
func TestLoadSkills_BrokenSkillDoesNotShadowLowerSource(t *testing.T) {
	highPrio := t.TempDir()
	lowPrio := t.TempDir()

	// Higher-priority source has a broken `pdf` skill (missing required
	// description field, so the loader rejects it).
	createSkillDir(t, highPrio, "pdf", "---\nname: pdf\n---\n# broken: no description")

	// Lower-priority source has a valid `pdf` skill.
	createSkillDir(t, lowPrio, "pdf", "---\nname: pdf\ndescription: Real PDF\n---\n# Real PDF")

	skills, err := LoadSkills(
		SkillSource{Dir: highPrio, Source: "global"},
		SkillSource{Dir: lowPrio, Source: "bundled"},
	)
	if err != nil {
		t.Fatalf("LoadSkills returned error: %v", err)
	}
	if len(skills) != 1 {
		t.Fatalf("expected the bundled pdf to take over, got %d skills", len(skills))
	}
	if skills[0].Name != "pdf" {
		t.Errorf("expected name=pdf, got %q", skills[0].Name)
	}
	if skills[0].Description != "Real PDF" {
		t.Errorf("expected the bundled (working) skill to win, got description=%q", skills[0].Description)
	}
	if skills[0].Source != "bundled" {
		t.Errorf("expected source=bundled (broken global was skipped), got %q", skills[0].Source)
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

func TestLoadSkills_StickyInstructions_OptIn(t *testing.T) {
	tmp := t.TempDir()
	body := "---\nname: policy\ndescription: Policy skill\nsticky-instructions: true\n---\n\n# Policy\n\nRoute all platform operations through http://localhost:7533 — never edit ~/.shannon files directly.\n\nMore detail in later sections."
	createSkillDir(t, tmp, "policy", body)

	loaded, err := LoadSkills(SkillSource{Dir: tmp, Source: "global"})
	if err != nil {
		t.Fatalf("LoadSkills: %v", err)
	}
	if len(loaded) != 1 {
		t.Fatalf("expected 1 skill, got %d", len(loaded))
	}
	s := loaded[0]
	if !s.StickyInstructions {
		t.Errorf("StickyInstructions = false, want true")
	}
	if s.StickySnippet == "" {
		t.Error("StickySnippet is empty, expected first-paragraph extraction")
	}
	if strings.HasPrefix(s.StickySnippet, "#") {
		t.Errorf("snippet should not start with an ATX heading: %q", s.StickySnippet)
	}
	if !strings.Contains(s.StickySnippet, "Route all platform operations") {
		t.Errorf("snippet missing expected body content: %q", s.StickySnippet)
	}
	if strings.Contains(s.StickySnippet, "\n") {
		t.Errorf("snippet should be single-line (newlines collapsed), got %q", s.StickySnippet)
	}
}

func TestLoadSkills_StickyInstructions_DefaultFalse(t *testing.T) {
	tmp := t.TempDir()
	createSkillDir(t, tmp, "plain", "---\nname: plain\ndescription: Plain skill\n---\n# Heading\n\nBody text.")

	loaded, err := LoadSkills(SkillSource{Dir: tmp, Source: "global"})
	if err != nil {
		t.Fatalf("LoadSkills: %v", err)
	}
	if len(loaded) != 1 {
		t.Fatalf("expected 1, got %d", len(loaded))
	}
	if loaded[0].StickyInstructions {
		t.Error("StickyInstructions should default to false when frontmatter omits it")
	}
}

func TestLoadSkills_Hidden_OptIn(t *testing.T) {
	tmp := t.TempDir()
	createSkillDir(t, tmp, "kocoro", "---\nname: kocoro\ndescription: Policy skill\nhidden: true\n---\n# Body")

	loaded, err := LoadSkills(SkillSource{Dir: tmp, Source: "global"})
	if err != nil {
		t.Fatalf("LoadSkills: %v", err)
	}
	if len(loaded) != 1 {
		t.Fatalf("expected 1 skill, got %d", len(loaded))
	}
	if !loaded[0].Hidden {
		t.Error("Hidden = false, want true when frontmatter sets hidden: true")
	}
	if !loaded[0].ToMeta().Hidden {
		t.Error("SkillMeta.Hidden = false, want true — ToMeta must propagate the flag")
	}
}

func TestLoadSkills_Hidden_DefaultFalse(t *testing.T) {
	tmp := t.TempDir()
	createSkillDir(t, tmp, "plain", "---\nname: plain\ndescription: Plain skill\n---\n# Body")

	loaded, err := LoadSkills(SkillSource{Dir: tmp, Source: "global"})
	if err != nil {
		t.Fatalf("LoadSkills: %v", err)
	}
	if len(loaded) != 1 {
		t.Fatalf("expected 1, got %d", len(loaded))
	}
	if loaded[0].Hidden {
		t.Error("Hidden should default to false when frontmatter omits it")
	}
	if loaded[0].ToMeta().Hidden {
		t.Error("SkillMeta.Hidden should default to false")
	}
}

func TestLoadSkills_StickySnippet_TruncatedTo400(t *testing.T) {
	tmp := t.TempDir()
	// Build a long first paragraph (>400 chars) to exercise the cap.
	long := strings.Repeat("abcdefghij ", 60) // 660 chars
	body := "---\nname: long\ndescription: Long skill\nsticky-instructions: true\n---\n\n" + long
	createSkillDir(t, tmp, "long", body)

	loaded, err := LoadSkills(SkillSource{Dir: tmp, Source: "global"})
	if err != nil {
		t.Fatalf("LoadSkills: %v", err)
	}
	if len(loaded) != 1 {
		t.Fatalf("expected 1, got %d", len(loaded))
	}
	runes := []rune(loaded[0].StickySnippet)
	if len(runes) > stickySnippetMaxChars {
		t.Errorf("snippet len=%d, want <= %d", len(runes), stickySnippetMaxChars)
	}
	if len(runes) < 10 {
		t.Errorf("snippet too short: %q", loaded[0].StickySnippet)
	}
	if !strings.HasSuffix(loaded[0].StickySnippet, "...") {
		t.Errorf("truncated snippet should end with '...', got %q", loaded[0].StickySnippet)
	}
}

func TestLoadSkills_StickySnippet_FallsBackToDescription(t *testing.T) {
	tmp := t.TempDir()
	// Frontmatter-only SKILL.md — body is empty, so snippet should fall back.
	createSkillDir(t, tmp, "empty", "---\nname: empty\ndescription: Fallback description text\nsticky-instructions: true\n---\n")

	loaded, err := LoadSkills(SkillSource{Dir: tmp, Source: "global"})
	if err != nil {
		t.Fatalf("LoadSkills: %v", err)
	}
	if len(loaded) != 1 {
		t.Fatalf("expected 1 skill, got %d", len(loaded))
	}
	if loaded[0].StickySnippet != "Fallback description text" {
		t.Errorf("expected description fallback, got %q", loaded[0].StickySnippet)
	}
}

func TestExtractStickySnippet_SkipsHeadings(t *testing.T) {
	body := "# Title\n\n## Sub\n\nThe actual first guidance paragraph."
	got := extractStickySnippet(body)
	if got != "The actual first guidance paragraph." {
		t.Errorf("extractStickySnippet = %q", got)
	}
}

func TestExtractStickySnippet_CollapsesNewlines(t *testing.T) {
	body := "Line one\nline two\nline three."
	got := extractStickySnippet(body)
	if got != "Line one line two line three." {
		t.Errorf("extractStickySnippet = %q", got)
	}
}

// TestExtractStickySnippet_PrefersImperativeParagraph locks in the core
// reviewer-flagged bug: when a SKILL.md body has a bland intro paragraph
// followed by the actual policy ("ALL platform operations MUST go through
// ..."), the extractor must prefer the policy paragraph, not the intro.
func TestExtractStickySnippet_PrefersImperativeParagraph(t *testing.T) {
	body := "You help users manage the platform.\n\nALL platform operations go through the daemon HTTP API. Never edit config files directly.\n\nMore detail later."
	got := extractStickySnippet(body)
	if !strings.Contains(got, "ALL platform operations") {
		t.Errorf("expected imperative paragraph, got %q", got)
	}
	if strings.Contains(got, "You help users") {
		t.Errorf("should NOT select the bland intro paragraph, got %q", got)
	}
}

func TestExtractStickySnippet_ImperativeMarkers(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string // substring that must be in the returned snippet
	}{
		{"MUST", "Intro paragraph.\n\nYou MUST do X before Y.", "MUST do X"},
		{"NEVER caps", "Intro.\n\nNEVER run destructive commands.", "NEVER run"},
		{"DO NOT", "Intro.\n\nDO NOT bypass validation.", "DO NOT bypass"},
		{"DON'T", "Intro.\n\nDON'T touch prod.", "DON'T touch"},
		{"Never sentence-start", "Intro.\n\nNever commit without review.", "Never commit"},
		{"Use the ...", "Intro.\n\nUse the http tool for every operation.", "Use the http tool"},
		{"ZH 必须", "中性描述。\n\n必须通过 API 操作，不要直接改文件。", "必须通过"},
		{"ZH 绝不", "介绍段落。\n\n绝不要直接写 ~/.shannon 文件。", "绝不要"},
		{"JA 必ず", "汎用説明。\n\n必ずAPIを使用してください。", "必ずAPI"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractStickySnippet(tt.body)
			if !strings.Contains(got, tt.want) {
				t.Errorf("extractStickySnippet missed imperative paragraph, got %q (want substring %q)", got, tt.want)
			}
		})
	}
}

func TestLoadSkills_StickySnippet_ExplicitOverride(t *testing.T) {
	tmp := t.TempDir()
	// Body has a clear imperative paragraph that SHOULD win via heuristic,
	// but the frontmatter explicitly pins a different snippet — override wins.
	body := "---\nname: over\ndescription: Override test\nsticky-instructions: true\nsticky-snippet: \"Explicit: use http tool only.\"\n---\n\nIntro.\n\nMUST not appear in snippet because override is set."
	createSkillDir(t, tmp, "over", body)

	loaded, err := LoadSkills(SkillSource{Dir: tmp, Source: "global"})
	if err != nil {
		t.Fatalf("LoadSkills: %v", err)
	}
	if len(loaded) != 1 {
		t.Fatalf("expected 1, got %d", len(loaded))
	}
	if loaded[0].StickySnippet != "Explicit: use http tool only." {
		t.Errorf("expected explicit override, got %q", loaded[0].StickySnippet)
	}
}

// TestLoadSkills_Kocoro_NotSticky asserts kocoro is intentionally NOT sticky.
// Sticky reminders re-inject a snippet at the start of every turn; for a
// task-driven skill like kocoro that body competes with the use_skill tool
// result for model attention, prompting the model to re-call use_skill to
// reconcile and tripping ConsecutiveDuplicate on turn 3. The opt-in was
// removed; this test pins the decision so future edits don't silently
// re-enable it.
func TestLoadSkills_Kocoro_NotSticky(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	bundledDir := filepath.Join(wd, "bundled", "skills")
	loaded, err := LoadSkills(SkillSource{Dir: bundledDir, Source: "bundled"})
	if err != nil {
		t.Fatalf("LoadSkills bundled: %v", err)
	}
	var kocoro *Skill
	for _, s := range loaded {
		if s.Name == "kocoro" {
			kocoro = s
			break
		}
	}
	if kocoro == nil {
		t.Fatalf("kocoro skill not found in bundled; loaded=%d", len(loaded))
	}
	if kocoro.StickyInstructions {
		t.Fatal("kocoro must NOT be sticky — re-injection competes with use_skill body and triggers ConsecutiveDuplicate loop")
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

// TestWriteGlobalSkill_RoundTripsSticky guards the reviewer-flagged
// persistence gap: WriteGlobalSkill must preserve sticky-instructions and
// the author-pinned sticky-snippet across a write→load cycle. Otherwise
// global skills silently lose sticky config on save.
func TestWriteGlobalSkill_RoundTripsSticky(t *testing.T) {
	shannonDir := t.TempDir()

	// Case A: sticky-instructions=true + author-pinned sticky-snippet override.
	t.Run("explicit snippet preserved", func(t *testing.T) {
		err := WriteGlobalSkill(shannonDir, &Skill{
			Name:                  "policy-a",
			Description:           "A",
			Prompt:                "# Policy\n\nBland intro here.\n\nALL ops go through http://localhost.",
			StickyInstructions:    true,
			StickySnippetOverride: "Use the http tool for every platform op.",
		})
		if err != nil {
			t.Fatalf("WriteGlobalSkill: %v", err)
		}
		loaded, err := LoadSkills(SkillSource{Dir: filepath.Join(shannonDir, "skills"), Source: SourceGlobal})
		if err != nil {
			t.Fatalf("LoadSkills: %v", err)
		}
		var s *Skill
		for _, x := range loaded {
			if x.Name == "policy-a" {
				s = x
				break
			}
		}
		if s == nil {
			t.Fatal("policy-a not reloaded")
		}
		if !s.StickyInstructions {
			t.Error("StickyInstructions dropped on round-trip")
		}
		if s.StickySnippetOverride != "Use the http tool for every platform op." {
			t.Errorf("StickySnippetOverride lost: %q", s.StickySnippetOverride)
		}
		if s.StickySnippet != "Use the http tool for every platform op." {
			t.Errorf("resolved StickySnippet != override: %q", s.StickySnippet)
		}
	})

	// Case B: sticky-instructions=true but NO explicit override. Save must
	// NOT freeze the heuristic result into the file; on reload, the
	// heuristic must run again and still pick the imperative paragraph.
	t.Run("heuristic snippet not frozen", func(t *testing.T) {
		err := WriteGlobalSkill(shannonDir, &Skill{
			Name:                  "policy-b",
			Description:           "B",
			Prompt:                "# Policy\n\nBland intro here.\n\nNEVER edit config.yaml directly.",
			StickyInstructions:    true,
			StickySnippet:         "bland intro here.", // would-be frozen value
			StickySnippetOverride: "",                  // NOT explicit — should be dropped
		})
		if err != nil {
			t.Fatalf("WriteGlobalSkill: %v", err)
		}
		raw, err := os.ReadFile(filepath.Join(shannonDir, "skills", "policy-b", "SKILL.md"))
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if strings.Contains(string(raw), "sticky-snippet:") {
			t.Errorf("WriteGlobalSkill froze heuristic snippet into SKILL.md:\n%s", raw)
		}
		if !strings.Contains(string(raw), "sticky-instructions: true") {
			t.Errorf("WriteGlobalSkill dropped sticky-instructions flag:\n%s", raw)
		}

		loaded, _ := LoadSkills(SkillSource{Dir: filepath.Join(shannonDir, "skills"), Source: SourceGlobal})
		var s *Skill
		for _, x := range loaded {
			if x.Name == "policy-b" {
				s = x
				break
			}
		}
		if s == nil {
			t.Fatal("policy-b not reloaded")
		}
		// Heuristic should run on reload and pick the NEVER paragraph.
		if !strings.Contains(s.StickySnippet, "NEVER edit") {
			t.Errorf("heuristic re-extraction failed, got %q", s.StickySnippet)
		}
	})

	// Case C: sticky-instructions=false + no override → neither field
	// appears in the written frontmatter (no noisy `false` line).
	t.Run("disabled sticky omitted", func(t *testing.T) {
		err := WriteGlobalSkill(shannonDir, &Skill{
			Name:        "plain",
			Description: "Plain",
			Prompt:      "# plain\n\nhello",
		})
		if err != nil {
			t.Fatalf("WriteGlobalSkill: %v", err)
		}
		raw, err := os.ReadFile(filepath.Join(shannonDir, "skills", "plain", "SKILL.md"))
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if strings.Contains(string(raw), "sticky-instructions:") {
			t.Errorf("plain skill gained noisy sticky-instructions: line:\n%s", raw)
		}
		if strings.Contains(string(raw), "sticky-snippet:") {
			t.Errorf("plain skill gained noisy sticky-snippet: line:\n%s", raw)
		}
	})

	// Case D: hidden=true must survive the write→load cycle. Without this,
	// PUT /skills/{slug} on a hidden skill would silently strip hidden:true
	// from disk, making the skill reappear in the list on next reload.
	t.Run("hidden flag preserved", func(t *testing.T) {
		err := WriteGlobalSkill(shannonDir, &Skill{
			Name:        "policy-hidden",
			Description: "Hidden policy",
			Prompt:      "# Policy\n\nbody",
			Hidden:      true,
		})
		if err != nil {
			t.Fatalf("WriteGlobalSkill: %v", err)
		}
		raw, err := os.ReadFile(filepath.Join(shannonDir, "skills", "policy-hidden", "SKILL.md"))
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if !strings.Contains(string(raw), "hidden: true") {
			t.Errorf("WriteGlobalSkill dropped hidden flag:\n%s", raw)
		}
		loaded, _ := LoadSkills(SkillSource{Dir: filepath.Join(shannonDir, "skills"), Source: SourceGlobal})
		var s *Skill
		for _, x := range loaded {
			if x.Name == "policy-hidden" {
				s = x
				break
			}
		}
		if s == nil {
			t.Fatal("policy-hidden not reloaded")
		}
		if !s.Hidden {
			t.Error("Hidden flag lost on round-trip")
		}
	})

	// Case E: hidden omitted or false → no noisy `hidden: false` line in
	// the written frontmatter, consistent with Case C's treatment of
	// sticky-instructions.
	t.Run("hidden false omitted", func(t *testing.T) {
		err := WriteGlobalSkill(shannonDir, &Skill{
			Name:        "policy-visible",
			Description: "Visible policy",
			Prompt:      "# Policy\n\nbody",
		})
		if err != nil {
			t.Fatalf("WriteGlobalSkill: %v", err)
		}
		raw, err := os.ReadFile(filepath.Join(shannonDir, "skills", "policy-visible", "SKILL.md"))
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if strings.Contains(string(raw), "hidden:") {
			t.Errorf("visible skill gained noisy hidden: line:\n%s", raw)
		}
	})
}

// TestBundledGenerativeUISkill_RetainsToolAllowlist is a manifest guard for the
// kocoro-generative-ui builtin. That skill is visualization-only — it emits
// html-artifact fences that render in Desktop's WKWebView sandbox and must NOT
// be able to fetch live data or run arbitrary tools. The restriction is enforced
// at execution time (loop.go) from the skill's `allowed-tools` frontmatter, and
// an EMPTY AllowedTools clears that filter entirely, silently granting the skill
// every registered tool (bash, http, browser, web search…). Commit 39fee8f
// dropped this line once as an accidental side effect of an unrelated feature;
// this test walks the real embed → extract → parse path so any future drop (or
// unreviewed widening) fails loudly here instead of in production.
func TestBundledGenerativeUISkill_RetainsToolAllowlist(t *testing.T) {
	src, err := BundledSkillSource(t.TempDir())
	if err != nil {
		t.Fatalf("BundledSkillSource: %v", err)
	}
	loaded, err := LoadSkills(src)
	if err != nil {
		t.Fatalf("LoadSkills: %v", err)
	}
	var gui *Skill
	for _, s := range loaded {
		if s.Slug == "kocoro-generative-ui" {
			gui = s
			break
		}
	}
	if gui == nil {
		t.Fatal("kocoro-generative-ui skill not found in bundled set")
	}
	const want = "file_read file_write publish_to_web think"
	if got := strings.Join(gui.AllowedTools, " "); got != want {
		t.Errorf("kocoro-generative-ui allowed-tools = %q, want %q\n"+
			"(a visualization-only skill must stay tool-restricted; an empty allowlist "+
			"clears the loop.go execution filter and grants every registered tool)", got, want)
	}
}
