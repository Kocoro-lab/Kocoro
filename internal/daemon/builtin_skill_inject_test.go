package daemon

import (
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/skills"
)

// namedSkillSet returns the set of Skill.Name values for order-independent
// membership assertions.
func namedSkillSet(list []*skills.Skill) map[string]bool {
	out := make(map[string]bool, len(list))
	for _, s := range list {
		out[s.Name] = true
	}
	return out
}

// TestInjectBuiltinSkills_AddsBuiltinsToNamedAgent reproduces the gap: a named
// agent whose _attached.yaml omits the builtins must still end up with kocoro
// and kocoro-generative-ui — the capability the default agent gets for free via
// LoadGlobalSkills. The agent's own attached skills are preserved.
func TestInjectBuiltinSkills_AddsBuiltinsToNamedAgent(t *testing.T) {
	dir := t.TempDir()
	attached := []*skills.Skill{{Name: "file-tools", Slug: "file-tools", Description: "x"}}

	out := injectBuiltinSkills(attached, dir)

	got := namedSkillSet(out)
	for _, want := range []string{"kocoro", "kocoro-generative-ui", "file-tools"} {
		if !got[want] {
			t.Errorf("missing skill %q after injection; got %v", want, got)
		}
	}
}

// TestInjectBuiltinSkills_NoDuplicateWhenAlreadyAttached asserts an agent that
// already attached a builtin (matched by Name) doesn't get a duplicate entry.
func TestInjectBuiltinSkills_NoDuplicateWhenAlreadyAttached(t *testing.T) {
	dir := t.TempDir()
	attached := []*skills.Skill{
		{Name: "kocoro-generative-ui", Slug: "kocoro-generative-ui", Description: "preexisting"},
	}

	out := injectBuiltinSkills(attached, dir)

	count := 0
	for _, s := range out {
		if s.Name == "kocoro-generative-ui" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("kocoro-generative-ui appears %d times, want 1", count)
	}
}

// TestInjectBuiltinSkills_DoesNotMutateInput guards against append() writing
// into the caller's slice. agentOverride.Skills may be shared across concurrent
// sessions for the same agent (see filterSkillsForSource), so the injector must
// work on a copy. The input is given spare capacity so a careless in-place
// append would be observable.
func TestInjectBuiltinSkills_DoesNotMutateInput(t *testing.T) {
	dir := t.TempDir()
	in := make([]*skills.Skill, 1, 8)
	in[0] = &skills.Skill{Name: "file-tools", Slug: "file-tools"}
	snapshot := make([]*skills.Skill, len(in))
	copy(snapshot, in)

	_ = injectBuiltinSkills(in, dir)

	if len(in) != len(snapshot) {
		t.Fatalf("input slice length changed: got %d, want %d", len(in), len(snapshot))
	}
	for i := range snapshot {
		if in[i] != snapshot[i] {
			t.Fatalf("input element %d identity changed", i)
		}
	}
}
