package daemon

import "github.com/Kocoro-lab/ShanClaw/internal/schedule"

// shouldBroadcast resolves the broadcast intent for a schedule.
// Order of precedence:
//   1. Explicit Broadcast override (true/false)
//   2. Smart default by CreatedFromSource:
//      - IM source (slack/line/feishu/lark/wecom/telegram/webhook) → broadcast
//      - Other / unknown (webview/tui/cli/one-shot/...) → silent
//      - Empty CreatedFromSource (pre-feature schedules) → silent (safe)
//
// Applies uniformly to default agent (Agent=="") and named agents — there
// is intentionally no per-agent-type branch.
func shouldBroadcast(s *schedule.Schedule) bool {
	if s == nil {
		return false
	}
	if s.Broadcast != nil {
		return *s.Broadcast
	}
	return isIMSource(s.CreatedFromSource)
}

// isIMSource matches the cloud-distributed channel set used by
// runner.outputFormatForSource. Keep this list in sync if that set grows.
func isIMSource(source string) bool {
	switch source {
	case "slack", "line", "feishu", "lark", "wecom", "telegram", "webhook":
		return true
	default:
		return false
	}
}
