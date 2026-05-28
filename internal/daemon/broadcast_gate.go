package daemon

import (
	"strings"

	"github.com/Kocoro-lab/ShanClaw/internal/schedule"
)

// shouldBroadcast resolves the broadcast intent for a schedule.
// Order of precedence:
//  1. Explicit Broadcast override (true/false)
//  2. Smart default by CreatedFromSource:
//     - Cloud-distributed source (slack/line/feishu/lark/wecom/telegram/webhook) → broadcast
//     - Other / unknown (webview/tui/cli/one-shot/...) → silent
//     - Empty CreatedFromSource (pre-feature schedules) → silent (safe)
//
// Applies uniformly to default agent (Agent=="") and named agents — there
// is intentionally no per-agent-type branch.
//
// The cloud-distributed source set is the canonical `isCloudSource` helper
// from session_cwd.go — same enum, single source of truth (avoids drift
// against the parallel cloudSourceSet / IsMessagingPlatform lists called
// out in CLAUDE.md).
func shouldBroadcast(s *schedule.Schedule) bool {
	if s == nil {
		return false
	}
	if s.Broadcast != nil {
		return *s.Broadcast
	}
	return isCloudSource(s.CreatedFromSource)
}

// localScheduleSources enumerates the non-cloud origins the daemon attributes
// schedules to. "kocoro" is the HTTP API's default source (server.go sets it
// when a RunAgentRequest arrives without one); the rest map to the surfaces a
// schedule can be created from — Desktop UI (webview), TUI (tui), CLI (cli),
// one-shot CLI (one-shot). None of these is an IM source, so shouldBroadcast
// resolves them to silent.
var localScheduleSources = map[string]struct{}{
	"kocoro":   {},
	"webview":  {},
	"tui":      {},
	"cli":      {},
	"one-shot": {},
}

// isValidScheduleSource reports whether s is a recognized schedule origin tag.
// Empty is accepted — legacy schedules pre-date the field and the CLI leaves
// it unset on purpose (see cmd/schedule.go). Cloud sources flow through
// isCloudSource; everything else must be in localScheduleSources.
//
// POST /schedules takes created_from_source straight from the request body.
// The endpoint is localhost-only, but a buggy client could still POST a
// free-form value that shouldBroadcast would then mis-interpret. Closing the
// vocabulary at the API edge turns an unrecognized origin into an explicit
// 400 instead of a silently-wrong broadcast decision.
func isValidScheduleSource(s string) bool {
	if s == "" {
		return true
	}
	norm := strings.ToLower(strings.TrimSpace(s))
	if isCloudSource(norm) {
		return true
	}
	_, ok := localScheduleSources[norm]
	return ok
}
