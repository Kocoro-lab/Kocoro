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
