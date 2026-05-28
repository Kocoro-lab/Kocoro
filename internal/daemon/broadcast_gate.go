package daemon

import "github.com/Kocoro-lab/ShanClaw/internal/schedule"

// shouldBroadcast resolves the broadcast intent for a schedule.
// Order of precedence:
//   1. Explicit Broadcast override (true/false)
//   2. Smart default by CreatedFromSource:
//      - Cloud-distributed source (slack/line/feishu/lark/wecom/telegram/webhook) → broadcast
//      - Other / unknown (webview/tui/cli/one-shot/...) → silent
//      - Empty CreatedFromSource (pre-feature schedules) → silent (safe)
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
