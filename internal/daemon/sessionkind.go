package daemon

import "strings"

// Session kind classifies a session by its origin. It is derived from
// Session.Source, exposed on GET /sessions (SessionSummary.Kind) for client
// classification (Desktop session grouping), and used internally to filter
// cold-start and heartbeat session resolution.
const (
	SessionKindInteractive = "interactive"
	SessionKindIM          = "im"
	SessionKindSchedule    = "schedule"
)

// kindOf derives a session kind from its Source using an EXCLUSION rule rather
// than a whitelist: `schedule` and the IM platforms are closed sets, so
// everything else — including an empty source and "desktop"/"kocoro"/"tui"/
// "cli" — is interactive. This is load-bearing: on real data ~93% of sessions
// have an empty or "desktop" source, so a whitelist of "interactive" sources
// would misclassify the bulk of sessions and break interactive cold-start +
// heartbeat resolution. See
// docs/superpowers/specs/2026-06-01-named-agent-multi-session-design.md §6.3.
func kindOf(source string) string {
	norm := strings.ToLower(strings.TrimSpace(source))
	switch {
	case norm == ChannelSchedule:
		return SessionKindSchedule
	case IsMessagingPlatform(norm):
		return SessionKindIM
	default:
		return SessionKindInteractive
	}
}

// isInteractiveSource reports whether a session Source classifies as
// interactive (neither a schedule run nor an IM push). Used as the predicate
// for cold-start (resumeNamedAgentColdStart) and heartbeat resolution so they
// resolve onto the user's interactive chat and never onto a schedule/IM
// session.
func isInteractiveSource(source string) bool {
	return kindOf(source) == SessionKindInteractive
}
