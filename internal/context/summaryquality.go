package context

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

// maxExtractedIdentifiers bounds the identifier inventory the audit enforces.
//   - Workload: compaction of tool-heavy transcripts (file reads, browser
//     dumps) whose droppable middle contains far more identifier-shaped
//     tokens than a 2000-token summary could ever echo.
//   - Symptom when it binds: identifiers past the first 12 unique matches are
//     not enforced (they may still survive via the summary's own sections or
//     post-compaction file restoration).
//   - Override: raise together with GenerateSummary's MaxTokens — every
//     enforced identifier must fit in the summary verbatim.
const maxExtractedIdentifiers = 12

// opaqueIdentifierPattern matches token shapes a summarizer paraphrase would
// destroy and a later turn may need verbatim: long hex (commit hashes), URLs,
// unix paths, host:port pairs, and long bare numbers (tickets, PR/issue ids).
var opaqueIdentifierPattern = regexp.MustCompile(
	`([A-Fa-f0-9]{8,}|https?://\S+|/[\w.-]{2,}(?:/[\w.-]+)+|[A-Za-z0-9._-]+\.[A-Za-z0-9._/-]+:\d{1,5}|\b\d{6,}\b)`)

// summarySectionHeaders are the labeled sections summarizePrompt demands. The
// audit requires at least one to be present: the first two are expected on
// any compaction-scale summary, while the tail three are legitimately
// conditional ("omit if none"), so requiring all five would reject honest
// output. A summary with zero headers is the observed junk shape (2026-08-04
// e2e: a single "Continuing with Step 8..." line accepted unvalidated).
var summarySectionHeaders = []string{
	"## Current task & next steps",
	"## User corrections & decisions",
	"## Open files / important reads",
	"## Active skill policies",
	"## Loaded tool capabilities",
}

// autoPreservedHeader labels the mechanically-appended identifier section
// used when even the retry failed to echo every at-risk identifier.
const autoPreservedHeader = "## Exact identifiers (auto-preserved)"

func sanitizeExtractedIdentifier(v string) string {
	v = strings.TrimSpace(v)
	v = strings.TrimLeft(v, `("'`+"`"+`[{<`)
	v = strings.TrimRight(v, `)]"'`+"`"+`,;:.!?<>`)
	return v
}

var pureHexPattern = regexp.MustCompile(`^[A-Fa-f0-9]{8,}$`)

// extractOpaqueIdentifiers returns up to maxExtractedIdentifiers unique
// identifier-shaped tokens from text. Pure-hex tokens are upper-cased so the
// later containment check is case-insensitive for them (models frequently
// re-case hashes) while URLs and paths stay case-exact.
func extractOpaqueIdentifiers(text string) []string {
	matches := opaqueIdentifierPattern.FindAllString(text, -1)
	seen := make(map[string]bool, len(matches))
	out := make([]string, 0, maxExtractedIdentifiers)
	for _, m := range matches {
		v := sanitizeExtractedIdentifier(m)
		if len(v) < 4 {
			continue
		}
		if pureHexPattern.MatchString(v) {
			v = strings.ToUpper(v)
		}
		if seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
		if len(out) >= maxExtractedIdentifiers {
			break
		}
	}
	return out
}

// identifiersAtRisk collects identifiers from the part of the history that
// ShapeHistory may drop: everything between the protected prefix (system +
// first user) and the minKeepLast tail pairs it always retains. Identifiers
// that only appear in the protected prefix or the kept tail survive
// compaction verbatim and are not enforced against the summary.
func identifiersAtRisk(messages []client.Message) []string {
	start := 2
	end := len(messages) - minKeepLast*2
	if end <= start {
		return nil
	}
	return extractOpaqueIdentifiers(buildTranscript(messages[start:end]))
}

func summaryIncludesIdentifier(summary, id string) bool {
	if pureHexPattern.MatchString(id) {
		return strings.Contains(strings.ToUpper(summary), id)
	}
	return strings.Contains(summary, id)
}

func missingIdentifiers(summary string, identifiers []string) []string {
	var missing []string
	for _, id := range identifiers {
		if !summaryIncludesIdentifier(summary, id) {
			missing = append(missing, id)
		}
	}
	return missing
}

// auditSummary returns the quality-failure reasons for a candidate summary:
// "missing_sections" when none of the labeled section headers is present, and
// "missing_identifiers:<list>" when at-risk identifiers were paraphrased
// away. Empty result = acceptable.
func auditSummary(summary string, identifiers []string) []string {
	var reasons []string
	hasSection := false
	for _, h := range summarySectionHeaders {
		if strings.Contains(summary, h) {
			hasSection = true
			break
		}
	}
	if !hasSection {
		reasons = append(reasons, "missing_sections")
	}
	if missing := missingIdentifiers(summary, identifiers); len(missing) > 0 {
		reasons = append(reasons, "missing_identifiers:"+strings.Join(missing, ", "))
	}
	return reasons
}

// buildSummaryRetryMessage phrases the audit failure as an instruction for
// the one retry pass.
func buildSummaryRetryMessage(reasons, identifiers []string) string {
	var sb strings.Builder
	sb.WriteString("Your summary failed validation: ")
	sb.WriteString(strings.Join(reasons, "; "))
	sb.WriteString(".\nRewrite the <summary> block. It MUST use the labeled sections from the instructions")
	if len(identifiers) > 0 {
		sb.WriteString(" and MUST preserve these identifiers verbatim (copy the exact characters): ")
		sb.WriteString(strings.Join(identifiers, ", "))
	}
	sb.WriteString(".\nRespond with <summary>...</summary> only.")
	return sb.String()
}

// appendAutoPreservedIdentifiers mechanically guarantees identifier survival
// when the model would not: whatever the summary still misses is appended
// under an explicit section. Structure cannot be fixed mechanically, but
// identifier loss — the permanent, unrecoverable failure observed live — can.
func appendAutoPreservedIdentifiers(summary string, identifiers []string) string {
	missing := missingIdentifiers(summary, identifiers)
	if len(missing) == 0 {
		return summary
	}
	var sb strings.Builder
	sb.WriteString(strings.TrimRight(summary, "\n"))
	sb.WriteString("\n\n")
	sb.WriteString(autoPreservedHeader)
	sb.WriteString("\n")
	for _, id := range missing {
		fmt.Fprintf(&sb, "- %s\n", id)
	}
	return strings.TrimRight(sb.String(), "\n")
}

func addUsage(a, b client.Usage) client.Usage {
	a.InputTokens += b.InputTokens
	a.OutputTokens += b.OutputTokens
	a.TotalTokens += b.TotalTokens
	a.CostUSD += b.CostUSD
	a.CacheReadTokens += b.CacheReadTokens
	a.CacheCreationTokens += b.CacheCreationTokens
	a.CacheCreation5mTokens += b.CacheCreation5mTokens
	a.CacheCreation1hTokens += b.CacheCreation1hTokens
	return a
}
