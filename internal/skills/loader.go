package skills

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Kocoro-lab/ShanClaw/internal/skills/bundled"
	"github.com/adrg/frontmatter"
	"gopkg.in/yaml.v3"
)

type SkillSource struct {
	Dir    string
	Source string
}

const (
	SourceGlobal  = "global"
	SourceBundled = "bundled"
)

func BundledSkillSource(shannonDir string) (SkillSource, error) {
	dir, err := bundled.ExtractBundledSkills(shannonDir)
	if err != nil {
		return SkillSource{}, err
	}
	return SkillSource{Dir: dir, Source: SourceBundled}, nil
}

// stringOrList unmarshals a YAML value that may be EITHER a scalar string
// ("Read Write Bash") OR a sequence (["Read", "Write", "Bash"]). ClawHub skill
// authors commonly declare `allowed-tools` as a list, while the Agent Skills
// spec and Anthropic's bundled skills use the scalar form. Typing the field as
// a plain string rejects the list form outright: yaml.Unmarshal fails with
// "cannot unmarshal !!seq into string", which makes loadSkillMD error, which
// (a) 422s the marketplace install and (b) fail-open skips the skill from GET
// /skills — so a perfectly valid ClawHub skill silently never installs. Accept
// both forms so that can't happen.
type stringOrList []string

func (s *stringOrList) UnmarshalYAML(value *yaml.Node) error {
	switch value.Kind {
	case yaml.ScalarNode:
		var str string
		if err := value.Decode(&str); err != nil {
			return err
		}
		*s = strings.Fields(str)
	case yaml.SequenceNode:
		var list []string
		if err := value.Decode(&list); err != nil {
			return err
		}
		*s = list
	default:
		return fmt.Errorf("allowed-tools: expected a string or a list of strings")
	}
	return nil
}

// MarshalYAML re-serializes as a single space-separated scalar, so re-saving a
// skill (WriteGlobalSkill) keeps the spec-compliant string form on disk and the
// output is byte-identical to the previous `strings.Join(..., " ")` behavior.
func (s stringOrList) MarshalYAML() (interface{}, error) {
	return strings.Join([]string(s), " "), nil
}

type skillFrontmatter struct {
	Name string `yaml:"name"`
	// Slug is not part of the Agent Skills spec nor the openclaw/clawhub
	// spec (slug is derived from the directory basename). We accept the
	// YAML key so authors who historically set it don't blow up
	// unmarshal, but we intentionally ignore its value — Skill.Slug is
	// always the on-disk directory name.
	Slug          string         `yaml:"slug,omitempty"`
	Description   string         `yaml:"description"`
	License       string         `yaml:"license"`
	Compatibility string         `yaml:"compatibility"`
	// Metadata is intentionally `map[string]any` so nested YAML values
	// (ClawHub skills embed a structured `clawdbot` object with emoji,
	// required bins, etc.) round-trip through loadSkillMD without blowing
	// up unmarshal. A flat `map[string]string` would reject any non-string
	// value and surface as ErrInvalidSkillPayload / HTTP 422 "malformed"
	// — see the regression test in marketplace_test.go.
	Metadata     map[string]any `yaml:"metadata,omitempty"`
	// AllowedTools accepts both the scalar-string and YAML-list forms — see
	// stringOrList. On disk it is always re-serialized as the scalar form.
	AllowedTools stringOrList `yaml:"allowed-tools,omitempty"`
	// StickyInstructions opts the skill into post-activation / post-drift
	// <system-reminder> reinjection. Opt-in only. See Skill.StickyInstructions.
	// omitempty so skills that never set it don't gain a noisy
	// `sticky-instructions: false` line on re-save.
	StickyInstructions bool `yaml:"sticky-instructions,omitempty"`
	// Hidden, when true, excludes the skill from the default GET /skills
	// listing so frontends don't show it to users. Display-only flag — the
	// skill still loads, still participates in discovery, still invokable
	// via use_skill. Intended for policy/internal skills like kocoro that
	// the LLM must keep using but users should not see.
	Hidden bool `yaml:"hidden,omitempty"`
	// StickySnippet, when set, overrides the auto-extracted snippet so
	// authors can pin the precise guidance to re-inject. Essential for
	// skills where the first paragraph is boilerplate ("You help users ...")
	// but the actual policy sits further down. Falls back to the imperative-
	// paragraph heuristic and then to the first non-heading paragraph.
	StickySnippet string `yaml:"sticky-snippet,omitempty"`
}


func LoadSkills(sources ...SkillSource) ([]*Skill, error) {
	seen := make(map[string]bool)
	var result []*Skill

	for _, src := range sources {
		if _, err := os.Stat(src.Dir); os.IsNotExist(err) {
			continue
		}
		warnLegacyYAML(src.Dir)

		entries, err := os.ReadDir(src.Dir)
		if err != nil {
			continue
		}
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			if e.IsDir() {
				names = append(names, e.Name())
			}
		}
		sort.Strings(names)

		for _, name := range names {
			if seen[name] {
				continue
			}
			skillDir := filepath.Join(src.Dir, name)
			skillFile := filepath.Join(skillDir, "SKILL.md")
			if _, err := os.Stat(skillFile); os.IsNotExist(err) {
				continue
			}
			s, err := loadSkillMD(skillFile, name, src.Source)
			if err != nil {
				// Fail open per skill: a malformed SKILL.md must not block
				// every other skill in the same (or any other) source. Log a
				// warning that names the file path so the user can find and
				// fix it, then move on without marking `seen[name]` — that
				// way a valid lower-priority version of the same skill name
				// (e.g. bundled vs broken global) can still take over.
				log.Printf("WARNING: skipping skill %q (%s): %v", name, skillFile, err)
				continue
			}
			s.Dir = skillDir
			s.InstallSource, s.MarketplaceSlug = installProvenanceForSkill(src.Source, skillDir)
			seen[name] = true
			result = append(result, s)
		}
	}
	return result, nil
}

func loadSkillMD(path, dirName, source string) (*Skill, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var fm skillFrontmatter
	body, err := frontmatter.Parse(bytes.NewReader(data), &fm, frontmatter.NewFormat("---", "---", yaml.Unmarshal))
	if err != nil {
		return nil, fmt.Errorf("parse frontmatter: %w", err)
	}
	if fm.Name == "" {
		return nil, fmt.Errorf("skill name is required in frontmatter")
	}
	// Slug is the on-disk / URL-safe identifier, always derived from the
	// directory name (per openclaw/clawhub docs/skill-format.md). We don't
	// require frontmatter.name to equal dirName: some ClawHub authors ship
	// skills where `name` is a display label (e.g. `name: xiaohongshu`) and
	// the marketplace slug is different (e.g. `xiaohongshu-mcp-skills`).
	// Both names are valid and must coexist so the skill still installs.
	if err := ValidateSkillName(dirName); err != nil {
		return nil, fmt.Errorf("directory name %q is not a valid slug: %w", dirName, err)
	}
	// Bound frontmatter.name so it can't smuggle unusual characters into
	// prompts shown to the LLM (skill catalog, use_skill results, sticky
	// reinjection snippets). Formatting-sensitive: reject control chars
	// and any newline. Length cap prevents oversized catalog entries.
	if err := validateFrontmatterName(fm.Name); err != nil {
		return nil, err
	}
	// If an author set `slug:` in frontmatter and it disagrees with the
	// directory, warn so the mismatch surfaces during development. We
	// still trust the directory name — it's the URL/marketplace truth.
	if fm.Slug != "" && fm.Slug != dirName {
		log.Printf("WARNING: skill %s/SKILL.md has slug: %q which does not match directory %q; using the directory as the authoritative slug",
			path, fm.Slug, dirName)
	}
	if fm.Description == "" {
		return nil, fmt.Errorf("skill description is required")
	}
	// stringOrList already normalized both the scalar and list YAML forms into
	// a []string during unmarshal.
	allowedTools := []string(fm.AllowedTools)
	prompt := strings.TrimSpace(string(body))
	override := strings.TrimSpace(fm.StickySnippet)
	snippet := override
	if snippet == "" {
		snippet = extractStickySnippet(prompt)
	}
	if snippet == "" {
		snippet = fm.Description
	}
	snippet = truncateStickySnippet(snippet, stickySnippetMaxChars)
	return &Skill{
		Name:                  fm.Name,
		Slug:                  dirName,
		Description:           fm.Description,
		Prompt:                prompt,
		License:               fm.License,
		Compatibility:         fm.Compatibility,
		Metadata:              fm.Metadata,
		AllowedTools:          allowedTools,
		StickyInstructions:    fm.StickyInstructions,
		StickySnippet:         snippet,
		StickySnippetOverride: override,
		Hidden:                fm.Hidden,
		Source:                source,
	}, nil
}

// stickySnippetMaxChars caps the per-activation / per-drift reinjection size.
// 400 chars is the budget called out in the task plan — adds to the turn after
// use_skill and after every skill-filter denial, so keep it small.
const stickySnippetMaxChars = 400

// imperativeMarkers identify paragraphs with actionable policy language.
// Matched case-sensitively for EN (caps are a strong imperative signal —
// "MUST use" vs "must use") and as substring for CJK.
var imperativeMarkers = []string{
	// EN — capitalized imperatives (strong signal)
	"MUST", "ALWAYS", "NEVER", "DO NOT", "DON'T",
	"REQUIRED", "ONLY", "ALL ",
	// EN — sentence-start imperatives (moderate signal, case-insensitive check)
	"Never ", "Always ", "Must ", "Use the ", "Do not ",
	// ZH
	"必须", "绝不", "仅限", "总是", "不要", "只能",
	// JA
	"必ず", "決して", "絶対", "禁止", "常に", "使用してください",
}

// extractStickySnippet returns a single paragraph from the SKILL.md body
// most likely to be actionable guidance. Selection order:
//   1. First paragraph containing any imperativeMarker ("MUST", "NEVER",
//      "必须", "必ず", …) — these are pre-filtered actionable policy.
//   2. First non-heading paragraph — title/boilerplate is skipped.
// Newlines within the paragraph are collapsed to single spaces so the
// snippet renders cleanly inside a single-line <system-reminder>.
// Returns "" when no suitable paragraph is found (caller falls back to
// Description). Authors can override with the `sticky-snippet:` frontmatter
// field when neither heuristic picks the right paragraph.
func extractStickySnippet(body string) string {
	if body == "" {
		return ""
	}
	paragraphs := strings.Split(body, "\n\n")

	// Pass 1: prefer paragraphs with imperative/policy markers. Ignore
	// headings but don't require them to be absent — a paragraph can start
	// with "**MUST:** ..." and that's still policy.
	for _, p := range paragraphs {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if strings.HasPrefix(p, "#") {
			continue
		}
		if hasImperativeMarker(p) {
			return strings.Join(strings.Fields(p), " ")
		}
	}

	// Pass 2: fall back to first non-heading paragraph.
	for _, p := range paragraphs {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if strings.HasPrefix(p, "#") {
			continue
		}
		return strings.Join(strings.Fields(p), " ")
	}
	return ""
}

// hasImperativeMarker reports whether p contains any imperative/policy
// marker. EN caps markers require exact case; CJK markers use substring.
func hasImperativeMarker(p string) bool {
	for _, m := range imperativeMarkers {
		if strings.Contains(p, m) {
			return true
		}
	}
	return false
}

// truncateStickySnippet rune-safe truncates to max chars, appending an
// ellipsis when shortened so the model can tell the reminder is abbreviated.
func truncateStickySnippet(s string, max int) string {
	if max <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	if max <= 3 {
		return string(runes[:max])
	}
	return string(runes[:max-3]) + "..."
}

func warnLegacyYAML(dir string) {
	matches, _ := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if len(matches) > 0 {
		log.Printf("WARNING: Found legacy skills/*.yaml files in %s — migrate to SKILL.md format", dir)
	}
	matches, _ = filepath.Glob(filepath.Join(dir, "*.yml"))
	if len(matches) > 0 {
		log.Printf("WARNING: Found legacy skills/*.yml files in %s — migrate to SKILL.md format", dir)
	}
}
