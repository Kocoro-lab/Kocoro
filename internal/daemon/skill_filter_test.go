package daemon

import (
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/skills"
)

// makeTestSkills returns a fresh slice of three minimal *skills.Skill values
// covering the desktop-only target and two unaffected siblings. Slug == Name
// is fine — filter logic keys on Name and never inspects Slug.
func makeTestSkills() []*skills.Skill {
	return []*skills.Skill{
		{Name: "kocoro-generative-ui", Slug: "kocoro-generative-ui", Description: "html-artifact widgets"},
		{Name: "kocoro", Slug: "kocoro", Description: "kocoro policy skill"},
		{Name: "file-tools", Slug: "file-tools", Description: "file helpers"},
	}
}

// TestFilterSkillsForSource_CloudSuppresses asserts that for every entry in
// cloudSourceSet, the desktop-only skill is removed and the other two pass
// through in their original order.
func TestFilterSkillsForSource_CloudSuppresses(t *testing.T) {
	for src := range cloudSourceSet {
		t.Run(src, func(t *testing.T) {
			in := makeTestSkills()
			out := filterSkillsForSource(in, src)
			if len(out) != 2 {
				t.Fatalf("source %q: got %d skills, want 2", src, len(out))
			}
			if out[0].Name != "kocoro" || out[1].Name != "file-tools" {
				t.Fatalf("source %q: order/identity wrong: got [%s, %s]", src, out[0].Name, out[1].Name)
			}
			for _, s := range out {
				if s.Name == "kocoro-generative-ui" {
					t.Fatalf("source %q: kocoro-generative-ui leaked through filter", src)
				}
			}
		})
	}
}

// TestFilterSkillsForSource_NonCloudPassThrough asserts non-cloud sources
// receive the full list unchanged (length + element identity). Empty string
// covers the TUI/Desktop path; the other entries cover known non-cloud
// sources that reach daemon (web/kocoro/cron/schedule/ws).
func TestFilterSkillsForSource_NonCloudPassThrough(t *testing.T) {
	for _, src := range []string{"", "kocoro", "web", "cron", "schedule", "ws"} {
		t.Run(src, func(t *testing.T) {
			in := makeTestSkills()
			out := filterSkillsForSource(in, src)
			if len(out) != len(in) {
				t.Fatalf("source %q: len=%d, want %d", src, len(out), len(in))
			}
			for i := range in {
				if out[i] != in[i] {
					t.Fatalf("source %q: element %d identity changed", src, i)
				}
			}
		})
	}
}

// TestFilterSkillsForSource_DoesNotMutateInput captures input pointers and
// length before the call, then verifies they are unchanged. Guards against a
// future "optimization" that filters in-place — which would leak the filtered
// view to a subsequent SetSkills call on a different source (cross-session
// pollution since runner.go shares loadedSkills across the if/else branches).
func TestFilterSkillsForSource_DoesNotMutateInput(t *testing.T) {
	in := makeTestSkills()
	snapshot := make([]*skills.Skill, len(in))
	copy(snapshot, in)
	_ = filterSkillsForSource(in, "feishu")
	if len(in) != len(snapshot) {
		t.Fatalf("input slice length changed: got %d, want %d", len(in), len(snapshot))
	}
	for i := range snapshot {
		if in[i] != snapshot[i] {
			t.Fatalf("input slice element %d identity changed", i)
		}
	}
}

// TestFilterSkillsForSource_NoDesktopOnlySkills_PassThrough exercises the
// fast path: cloud source but the input contains no desktop-only skills.
// Must still return a logically-equal list; identity preservation is not
// required (impl may copy or return as-is — test asserts contents only).
func TestFilterSkillsForSource_NoDesktopOnlySkills_PassThrough(t *testing.T) {
	in := []*skills.Skill{
		{Name: "kocoro", Slug: "kocoro"},
		{Name: "file-tools", Slug: "file-tools"},
	}
	out := filterSkillsForSource(in, "feishu")
	if len(out) != 2 {
		t.Fatalf("len=%d, want 2", len(out))
	}
	if out[0].Name != "kocoro" || out[1].Name != "file-tools" {
		t.Fatalf("contents wrong: [%s, %s]", out[0].Name, out[1].Name)
	}
}

// TestFilterSkillsForSource_NormalizesSourceCasing asserts the filter honors
// the same lower+trim normalization isCloudSource applies, so a channel
// handler that ships "Slack" or " feishu " still triggers suppression. Locks
// in the contract — without this test someone "optimizing" isCloudSource into
// a direct map lookup would silently regress mixed-case / padded inputs.
func TestFilterSkillsForSource_NormalizesSourceCasing(t *testing.T) {
	for _, src := range []string{"Slack", "FEISHU", " feishu ", "\tlark\n", "WeCom"} {
		t.Run(src, func(t *testing.T) {
			in := makeTestSkills()
			out := filterSkillsForSource(in, src)
			if len(out) != 2 {
				t.Fatalf("source %q: got %d skills, want 2 (suppression should ignore case/whitespace)", src, len(out))
			}
			for _, s := range out {
				if s.Name == "kocoro-generative-ui" {
					t.Fatalf("source %q: kocoro-generative-ui leaked — normalization regressed", src)
				}
			}
		})
	}
}

// TestDesktopOnlySkillsCoverage_Drift asserts that for every desktop-only
// skill, every cloudSourceSet entry suppresses it. Mirrors the drift test in
// session_cwd_test.go: cheap insurance against someone adding a new cloud
// channel without revisiting the suppression list, or adding a new desktop-
// only skill that someone forgets to wire through every channel.
func TestDesktopOnlySkillsCoverage_Drift(t *testing.T) {
	for skillName := range desktopOnlySkills {
		for src := range cloudSourceSet {
			in := []*skills.Skill{{Name: skillName, Slug: skillName}}
			out := filterSkillsForSource(in, src)
			if len(out) != 0 {
				t.Errorf("desktopOnly skill %q not suppressed on cloud source %q (got %d skills)", skillName, src, len(out))
			}
		}
	}
}

// makeDenylistTestSkills mirrors makeTestSkills but includes a Name!=Slug entry
// so the denylist's match-Name-or-Slug contract is exercised.
func makeDenylistTestSkills() []*skills.Skill {
	return []*skills.Skill{
		{Name: "pdf-reader", Slug: "pdf-reader", Description: "read pdfs"},
		{Name: "Security Review", Slug: "security-review", Description: "audit code"},
		{Name: "file-tools", Slug: "file-tools", Description: "file helpers"},
	}
}

// TestApplyDefaultAgentSkillDenylist_DefaultRemovesNamedSurvives is the
// load-bearing test for the default-agent skill denylist: a disabled skill must
// vanish from the DEFAULT agent's set while the SAME denylist leaves a NAMED
// agent untouched. Named agents select skills via their _attached.yaml allowlist
// (opposite semantics) — applying the default-agent denylist to them would
// silently strip skills the user explicitly attached.
func TestApplyDefaultAgentSkillDenylist_DefaultRemovesNamedSurvives(t *testing.T) {
	disabled := []string{"file-tools"}

	// Default agent (isDefaultAgent == true): file-tools must be gone.
	outDefault := applyDefaultAgentSkillDenylist(makeTestSkills(), disabled, true)
	if len(outDefault) != 2 {
		t.Fatalf("default agent: got %d skills, want 2", len(outDefault))
	}
	for _, s := range outDefault {
		if s.Name == "file-tools" {
			t.Fatalf("default agent: disabled skill file-tools leaked through")
		}
	}

	// Named agent (isDefaultAgent == false): the same denylist must NOT apply.
	inNamed := makeTestSkills()
	outNamed := applyDefaultAgentSkillDenylist(inNamed, disabled, false)
	if len(outNamed) != len(inNamed) {
		t.Fatalf("named agent: denylist narrowed skills (got %d, want %d)", len(outNamed), len(inNamed))
	}
	survived := false
	for _, s := range outNamed {
		if s.Name == "file-tools" {
			survived = true
		}
	}
	if !survived {
		t.Fatalf("named agent: file-tools wrongly removed by default-agent denylist")
	}
}

// TestApplyDefaultAgentSkillDenylist_EmptyListPassThrough locks back-compat: a
// config with no skills.disabled field (nil/empty) leaves the default agent's
// full set intact — existing installs keep loading every skill.
func TestApplyDefaultAgentSkillDenylist_EmptyListPassThrough(t *testing.T) {
	for _, disabled := range [][]string{nil, {}} {
		out := applyDefaultAgentSkillDenylist(makeTestSkills(), disabled, true)
		if len(out) != 3 {
			t.Fatalf("disabled=%v: got %d skills, want 3 (empty denylist = full set)", disabled, len(out))
		}
	}
}

// TestApplyDefaultAgentSkillDenylist_MatchesNameOrSlug verifies a skill is
// dropped whether the user disabled it by Name or by Slug — handleListSkills
// surfaces both identifiers and Desktop may persist either.
func TestApplyDefaultAgentSkillDenylist_MatchesNameOrSlug(t *testing.T) {
	// Disable by Slug "security-review" — the entry whose Name is "Security Review".
	out := applyDefaultAgentSkillDenylist(makeDenylistTestSkills(), []string{"security-review"}, true)
	for _, s := range out {
		if s.Slug == "security-review" {
			t.Fatalf("skill disabled by slug leaked through (Name=%q)", s.Name)
		}
	}
	if len(out) != 2 {
		t.Fatalf("got %d skills, want 2", len(out))
	}
}

// TestApplyDefaultAgentSkillDenylist_DoesNotMutateInput guards against an
// in-place filter that would leak the narrowed view (loadedSkills is shared
// across concurrent sessions — same rationale as filterSkillsForSource).
func TestApplyDefaultAgentSkillDenylist_DoesNotMutateInput(t *testing.T) {
	in := makeTestSkills()
	snapshot := make([]*skills.Skill, len(in))
	copy(snapshot, in)
	_ = applyDefaultAgentSkillDenylist(in, []string{"file-tools"}, true)
	if len(in) != len(snapshot) {
		t.Fatalf("input length changed: got %d, want %d", len(in), len(snapshot))
	}
	for i := range snapshot {
		if in[i] != snapshot[i] {
			t.Fatalf("input element %d identity changed", i)
		}
	}
}
