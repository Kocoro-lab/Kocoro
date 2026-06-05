package tui

import (
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/session"
)

// stampRemotePlaceholderForTest mirrors the title-placeholder block inlined in
// runRemote (app.go ~L2367-2371) for the /research·/swarm·/dag entry points. It
// exists only so the contract can be unit-tested without spinning up a full TUI
// harness (same pattern as applyTUIAgentOverlayForTest). It must stay byte-equal
// to that production block for the test to be meaningful. The TitleAuto=true line
// is load-bearing: without it the async upgrade guard `!sess.TitleAuto` would
// permanently skip smart titles for remote-workflow sessions.
func stampRemotePlaceholderForTest(sess *session.Session, query string) {
	if sess.Title == "New session" {
		sess.Title = session.Title(query)
		sess.TitleAuto = true
	}
}

// TestTUI_RunRemotePlaceholderStampsTitleAuto verifies the runRemote placeholder
// flips TitleAuto on a fresh session so the async upgrade is not skipped.
func TestTUI_RunRemotePlaceholderStampsTitleAuto(t *testing.T) {
	sess := &session.Session{Title: "New session"}
	stampRemotePlaceholderForTest(sess, "research the latest on llm caching")

	if sess.Title != "research the latest on llm caching" {
		t.Errorf("placeholder Title = %q, want the query-derived title", sess.Title)
	}
	if !sess.TitleAuto {
		t.Error("placeholder did not set TitleAuto=true; async smart-title upgrade would be skipped")
	}
}

// TestTUI_RunRemotePlaceholderLeavesNonDefaultUntouched guards the `== "New
// session"` gate: a session whose title was already set (e.g. a prior /send)
// must not be re-stamped, so a user-locked title (TitleAuto already false) is
// not silently re-flagged for auto-upgrade.
func TestTUI_RunRemotePlaceholderLeavesNonDefaultUntouched(t *testing.T) {
	sess := &session.Session{Title: "My renamed session", TitleAuto: false}
	stampRemotePlaceholderForTest(sess, "research the latest on llm caching")

	if sess.Title != "My renamed session" {
		t.Errorf("non-default Title clobbered: got %q", sess.Title)
	}
	if sess.TitleAuto {
		t.Error("non-default session re-flagged TitleAuto=true; gate should have skipped it")
	}
}
