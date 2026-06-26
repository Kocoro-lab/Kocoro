package agents

import (
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/skills"
)

func mkDisabledTestSkills() []*skills.Skill {
	return []*skills.Skill{
		{Name: "pdf-reader", Slug: "pdf-reader"},
		{Name: "Security Review", Slug: "security-review"},
		{Name: "file-tools", Slug: "file-tools"},
	}
}

func TestFilterDisabledSkills_RemovesByNameOrSlug(t *testing.T) {
	// One by Name, one by Slug.
	out := FilterDisabledSkills(mkDisabledTestSkills(), []string{"pdf-reader", "security-review"})
	if len(out) != 1 || out[0].Name != "file-tools" {
		t.Fatalf("got %+v, want only file-tools", out)
	}
}

func TestFilterDisabledSkills_EmptyPassThrough(t *testing.T) {
	for _, d := range [][]string{nil, {}} {
		out := FilterDisabledSkills(mkDisabledTestSkills(), d)
		if len(out) != 3 {
			t.Fatalf("disabled=%v: got %d, want 3 (empty = full set)", d, len(out))
		}
	}
}

func TestFilterDisabledSkills_NoMutate(t *testing.T) {
	in := mkDisabledTestSkills()
	snap := make([]*skills.Skill, len(in))
	copy(snap, in)
	_ = FilterDisabledSkills(in, []string{"pdf-reader"})
	if len(in) != len(snap) {
		t.Fatalf("input length changed: got %d, want %d", len(in), len(snap))
	}
	for i := range snap {
		if in[i] != snap[i] {
			t.Fatalf("input element %d identity changed", i)
		}
	}
}
