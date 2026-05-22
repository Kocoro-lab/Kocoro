package agent

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/prompt"
)

// TestSystemPromptAudit dumps a token-distribution table of the assembled
// system prompt so reviewers can size the cache prefix and spot wasteful
// sections. Run with:
//
//	go test ./internal/agent -run TestSystemPromptAudit -v
//
// Not a pass/fail test — it always succeeds. The test exists to keep the
// audit reproducible; rerun after any change to coreOperationalRules,
// contrastExamples*, or buildStaticSystem to see the new distribution.
//
// See docs/issues/cache-action-plan.md §1.5.
func TestSystemPromptAudit(t *testing.T) {
	if testing.Short() {
		t.Skip("audit dump skipped in -short mode")
	}

	// Realistic one-shot CLI baseline using the real production constants:
	// defaultPersona + coreOperationalRules + contrastExamplesCore is the
	// BasePrompt assembled in AgentLoop.Run (line ~999).
	basePrompt := defaultPersona + coreOperationalRules + contrastExamplesCore
	t.Logf("--- BasePrompt constants ---")
	dumpConst(t, "  defaultPersona", defaultPersona)
	dumpConst(t, "  coreOperationalRules", coreOperationalRules)
	dumpConst(t, "  contrastExamplesCore", contrastExamplesCore)
	dumpConst(t, "  cloudDelegationGuidance (conditional)", cloudDelegationGuidance)
	dumpConst(t, "  contrastExamplesCloud (conditional)", contrastExamplesCloud)

	// Mimic a typical post-categorical-defer one-shot CLI tool registry.
	// 17 tools after browser/computer/schedule/etc. are filtered out by
	// buildLocalActiveSchemas (see cache-action-plan §1.2).
	tools := []string{
		"bash", "clipboard", "cloud_delegate", "directory_list",
		"file_edit", "file_read", "file_write", "glob", "grep", "http",
		"memory_append", "notify", "session_search", "system_info", "think",
		"tool_search", "use_skill",
	}
	parts := prompt.BuildSystemPrompt(prompt.PromptOptions{
		BasePrompt:     basePrompt,
		LocalToolNames: tools,
		MemoryDir:      "/Users/test/.shannon/agents/sample",
		ModelID:        "medium",
		OutputFormat:   "markdown",
	})

	t.Logf("--- Assembled system prompt ---")
	t.Logf("  System total:    %d chars / ~%.0f tokens", len(parts.System), tokensFromChars(len(parts.System)))
	t.Logf("  StableContext:   %d chars / ~%.0f tokens", len(parts.StableContext), tokensFromChars(len(parts.StableContext)))
	t.Logf("  VolatileContext: %d chars / ~%.0f tokens", len(parts.VolatileContext), tokensFromChars(len(parts.VolatileContext)))

	// Section breakdown of System: split by top-level "## " headings.
	sections := splitBySection(parts.System)
	totalChars := 0
	for _, s := range sections {
		totalChars += s.chars
	}
	type row struct {
		name  string
		chars int
		toks  float64
		pct   float64
	}
	rows := make([]row, 0, len(sections))
	for _, s := range sections {
		rows = append(rows, row{
			name:  s.name,
			chars: s.chars,
			toks:  tokensFromChars(s.chars),
			pct:   100.0 * float64(s.chars) / float64(totalChars),
		})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].chars > rows[j].chars })
	t.Logf("--- System ## sections (sorted by size) ---")
	for _, r := range rows {
		t.Logf("  %5d chars  ~%5.0f tok  %5.1f%%   %s", r.chars, r.toks, r.pct, r.name)
	}

	// Cross-section redundancy detection: well-known overlapping concepts.
	// These are flagged for human review — not auto-cut, since the duplication
	// may be intentional emphasis. See cache-action-plan.md §1.5 follow-up notes.
	t.Logf("--- Redundancy probes ---")
	probes := []struct {
		concept string
		needles []string
	}{
		{"don't brute-force blocked approach", []string{"do not brute-force", "blocked approach"}},
		{"stop after N attempts", []string{"3 attempts", "3 different approaches", "3+ different"}},
		{"stop at sufficiency / never repeat", []string{"summarize and stop", "sufficiency", "never repeat"}},
		{"act directly on simple tasks", []string{"act directly", "single-action requests", "executed immediately"}},
		{"verification preference chain", []string{"verification preference", "minimum viable verification"}},
	}
	lower := strings.ToLower(parts.System)
	flagged := 0
	for _, p := range probes {
		hits := 0
		for _, n := range p.needles {
			if strings.Contains(lower, strings.ToLower(n)) {
				hits++
			}
		}
		if hits >= 2 {
			t.Logf("  potential duplicate (%d/%d phrases hit): %s", hits, len(p.needles), p.concept)
			flagged++
		}
	}
	t.Logf("  total redundancy candidates: %d", flagged)
}

func dumpConst(t *testing.T, label, content string) {
	t.Helper()
	t.Logf("%s: %d chars / ~%.0f tokens", label, len(content), tokensFromChars(len(content)))
}

func tokensFromChars(n int) float64 { return float64(n) / 3.5 }

type sectionRange struct {
	name  string
	chars int
}

// splitBySection partitions the assembled system prompt by top-level "## "
// headings. "### " sub-sections roll up into the parent. The pre-heading
// preamble (BasePrompt, before the first "## " in buildStaticSystem's output)
// shows as "(prelude)".
func splitBySection(s string) []sectionRange {
	type pos struct {
		name  string
		start int
	}
	var marks []pos
	idx := 0
	for {
		next := strings.Index(s[idx:], "\n## ")
		if next < 0 {
			break
		}
		next += idx + 1
		// The search pattern "\n## " already excludes "### " sub-headings
		// (third char differs: space vs '#'), so no extra guard is needed —
		// s[next:] is guaranteed to start with "## " here.
		eol := strings.Index(s[next:], "\n")
		if eol < 0 {
			eol = len(s) - next
		}
		head := s[next : next+eol]
		marks = append(marks, pos{name: strings.TrimPrefix(head, "## "), start: next})
		idx = next + 1
	}
	if len(marks) == 0 {
		return []sectionRange{{name: "(prelude)", chars: len(s)}}
	}
	out := []sectionRange{}
	if marks[0].start > 0 {
		out = append(out, sectionRange{name: "(prelude)", chars: marks[0].start})
	}
	for i, m := range marks {
		end := len(s)
		if i+1 < len(marks) {
			end = marks[i+1].start
		}
		out = append(out, sectionRange{name: m.name, chars: end - m.start})
	}
	for i := range out {
		if out[i].name == "" {
			out[i].name = fmt.Sprintf("(unnamed-%d)", i)
		}
	}
	return out
}
