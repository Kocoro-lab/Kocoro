package daemon

import (
	"github.com/Kocoro-lab/ShanClaw/internal/agents"
	"github.com/Kocoro-lab/ShanClaw/internal/skills"
)

// desktopOnlySkills enumerates builtin skills whose output only renders in
// Kocoro Desktop (e.g. kocoro-generative-ui emits html-artifact fences that
// only the WKWebView host can interpret). They are suppressed on cloud-
// distributed channels — without suppression the LLM would happily activate
// the skill and ship raw HTML/CSS/JS that surfaces as a fenced code block in
// Feishu / Lark / WeCom / Slack / LINE / Telegram / webhook clients.
//
// Keep aligned with cloudSourceSet in session_cwd.go: the drift test in
// skill_filter_test.go enforces that every entry here is suppressed across
// every cloudSourceSet entry. Adding a new desktop-only skill requires no
// channel-side work — adding a new cloud source likewise requires no skill-
// side work — the matrix product is checked automatically.
//
// Map keys are Skill.Name (the identifier the LLM sees in the listing and
// passes to use_skill). use_skill's runtime lookup (internal/tools/skill.go)
// resolves Name first then falls back to Slug, but because filterSkillsForSource
// drops the *whole* entry both identifiers vanish from every downstream
// consumer — the asymmetry is moot as long as we stay in "drop the entry"
// mode. If anyone later replaces this with a set-based identifier-contains
// check (e.g. retaining the skill but filtering its name from the listing
// only), they must match both Name and Slug to preserve the same guarantee.
var desktopOnlySkills = map[string]struct{}{
	"kocoro-generative-ui": {},
}

// filterSkillsForSource returns a new slice with desktop-only skills removed
// when the request source is a cloud-distributed channel. Non-cloud sources
// (empty / kocoro / web / cron / schedule / ws — i.e. TUI, Desktop, one-shot
// CLI, scheduler) receive the full list unchanged.
//
// The input slice is intentionally not mutated. runner.go reuses the same
// loadedSkills value across the agent-override and default-agent branches
// within a single Run() invocation, and the slice itself may be shared across
// concurrent sessions for the same agent. An in-place filter would leak the
// filtered view to subsequent calls with a different (or empty) source value.
func filterSkillsForSource(loaded []*skills.Skill, source string) []*skills.Skill {
	if !isCloudSource(source) {
		return loaded
	}
	out := make([]*skills.Skill, 0, len(loaded))
	for _, s := range loaded {
		if _, blocked := desktopOnlySkills[s.Name]; blocked {
			continue
		}
		out = append(out, s)
	}
	return out
}

// applyDefaultAgentSkillDenylist applies the default-agent skill denylist
// (config.skills.disabled), gated on isDefaultAgent so it never narrows a named
// agent's _attached.yaml allowlist (the opposite semantics). The filtering
// itself is shared with the one-shot CLI and TUI default-agent paths via
// agents.FilterDisabledSkills, so a disabled skill is hidden uniformly across
// all three (they share ~/.shannon/config.yaml). Callers pass
// `agentOverride == nil` for isDefaultAgent.
func applyDefaultAgentSkillDenylist(loaded []*skills.Skill, disabled []string, isDefaultAgent bool) []*skills.Skill {
	if !isDefaultAgent {
		return loaded
	}
	return agents.FilterDisabledSkills(loaded, disabled)
}
