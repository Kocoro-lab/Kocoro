package daemon

import (
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/session"
)

// A recovered turn runs with no human present, regardless of the session's
// original source. It must classify as unattended so the unattended
// auto-approval deny-list (computer_use, screenshot) applies.
func TestInterruptedRecoveryHandler_IsUnattended(t *testing.T) {
	if !isUnattendedRun("desktop", &interruptedRecoveryHandler{}) {
		t.Fatal("recovered turn of a desktop-source session classified as attended — unattended deny-list would not apply")
	}
}

// Candidates older than the staleness cutoff must not be resumed; they are
// abandoned (marker cleared) instead.
func TestInterruptedRecovery_StaleCandidateFiltering(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name             string
		stateUpdatedAt   time.Time
		sessionUpdatedAt time.Time
		maxAge           time.Duration
		want             bool
	}{
		{"fresh checkpoint resumes", now.Add(-30 * time.Minute), now, 4 * time.Hour, false},
		{"old checkpoint stays stale after a recent attempt save", now.Add(-97 * 24 * time.Hour), now, 4 * time.Hour, true},
		{"just past cutoff is stale", now.Add(-5 * time.Hour), now.Add(-5 * time.Hour), 4 * time.Hour, true},
		{"legacy session timestamp is the fallback", time.Time{}, now.Add(-30 * time.Minute), 4 * time.Hour, false},
		{"zero timestamps are stale", time.Time{}, time.Time{}, 4 * time.Hour, true},
	}
	for _, tc := range cases {
		got := isStaleInterruptedTurn(interruptedTurnCandidate{
			SessionID: "s",
			State:     session.InterruptedTurn{UpdatedAt: tc.stateUpdatedAt},
			UpdatedAt: tc.sessionUpdatedAt,
		}, tc.maxAge, now)
		if got != tc.want {
			t.Errorf("%s: isStaleInterruptedTurn = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// The staleness window comes from agent.interrupted_resume_max_age_hours;
// zero/unset and negative values fall back to the default.
func TestInterruptedResumeMaxAge_Default(t *testing.T) {
	if got := interruptedResumeMaxAge(nil); got != 4*time.Hour {
		t.Fatalf("default max age = %v, want 4h", got)
	}
}

// The resume request must lock the SAME route key concurrent inbound traffic
// for this session would compute; otherwise two agent loops can run over one
// session file (recovery holds session:<id> while a Slack thread message
// holds default:slack:...).
func TestInterruptedResumeRequest_LocksOriginalRouteKey(t *testing.T) {
	imCandidate := interruptedTurnCandidate{
		SessionID: "2026-07-23-abc",
		State: session.InterruptedTurn{
			Source:   "slack",
			Channel:  "C123",
			ThreadID: "171234.5678",
			RouteKey: "default:slack:171234.5678",
		},
	}
	req := buildInterruptedResumeRequest(imCandidate, 3, 4*time.Hour)
	if got := ComputeRouteKey(req); got != "default:slack:171234.5678" {
		t.Fatalf("IM-origin resume locks %q, want the original route key %q", got, "default:slack:171234.5678")
	}

	desktopCandidate := interruptedTurnCandidate{
		SessionID: "2026-07-23-def",
		State:     session.InterruptedTurn{Source: "desktop"},
	}
	req = buildInterruptedResumeRequest(desktopCandidate, 3, 4*time.Hour)
	if got := ComputeRouteKey(req); got != "session:2026-07-23-def" {
		t.Fatalf("desktop-origin resume locks %q, want session:2026-07-23-def", got)
	}
}
