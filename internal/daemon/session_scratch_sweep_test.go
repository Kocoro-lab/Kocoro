package daemon

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Interactive-session scratch dirs are kept across session switches (their
// artifacts must outlive OnSessionClose), so disk reclaim happens by age at
// daemon startup instead.
func TestSweepSessionScratchRemovesOnlyOldDirs(t *testing.T) {
	shannonDir := t.TempDir()
	root := filepath.Join(shannonDir, "tmp", "sessions")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}

	oldDir := filepath.Join(root, "session-old")
	newDir := filepath.Join(root, "session-new")
	looseFile := filepath.Join(root, "stray-file.txt")
	for _, d := range []string{oldDir, newDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(oldDir, "screenshot.png"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(looseFile, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	past := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(oldDir, past, past); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(looseFile, past, past); err != nil {
		t.Fatal(err)
	}

	removed, err := sweepSessionScratch(shannonDir, 24*time.Hour)
	if err != nil {
		t.Fatalf("sweep failed: %v", err)
	}
	if removed != 1 {
		t.Fatalf("expected 1 removal, got %d", removed)
	}
	if _, err := os.Stat(oldDir); !os.IsNotExist(err) {
		t.Fatal("old scratch dir should have been removed")
	}
	if _, err := os.Stat(newDir); err != nil {
		t.Fatal("fresh scratch dir must be kept")
	}
	// Only session DIRECTORIES are swept; unexpected regular files are left
	// alone rather than guessed at.
	if _, err := os.Stat(looseFile); err != nil {
		t.Fatal("non-directory entries must not be touched")
	}
}

func TestSweepSessionScratchMissingRootIsNoop(t *testing.T) {
	removed, err := sweepSessionScratch(t.TempDir(), 24*time.Hour)
	if err != nil {
		t.Fatalf("missing root must be a no-op, got %v", err)
	}
	if removed != 0 {
		t.Fatalf("expected 0 removals, got %d", removed)
	}
}

// sessionScratchDirPath computes and validates the per-session scratch path
// WITHOUT creating it — creation is lazy (first artifact injection) so
// sessions that never produce files leave no empty directories behind.
func TestSessionScratchDirPathDoesNotCreate(t *testing.T) {
	shannonDir := t.TempDir()
	dir, err := sessionScratchDirPath(shannonDir, "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	if dir == "" {
		t.Fatal("expected a path")
	}
	if _, statErr := os.Stat(dir); !os.IsNotExist(statErr) {
		t.Fatal("path-only helper must not create the directory")
	}
	// Traversal-shaped session IDs are rejected, mirroring the mkdir variant.
	if _, err := sessionScratchDirPath(shannonDir, "../escape"); err == nil {
		t.Fatal("expected traversal rejection")
	}
}

// The commit-invariant behind the age-based sweep: a NON-cloud run must NOT
// register scratch cleanup on OnSessionClose. That hook fires on every
// session SWITCH, and interactive surfaces revisit history — live 2026-07-30
// repro: a screenshot whose "Saved to:" path the model had just reported was
// deleted seconds later when the user switched sessions. This drives the real
// RunAgent registration seam: BypassRouting runs use an ephemeral session
// manager whose deferred Close() fires every registered OnSessionClose
// callback before RunAgent returns, so a pre-placed artifact still existing
// afterwards proves no cleanup was registered.
func TestRunAgent_NonCloudArtifactScratchSurvivesSessionClose(t *testing.T) {
	gw := &fakeGatewayBackend{reply: "done"}
	ts := httptest.NewServer(gw.handler())
	defer ts.Close()

	deps := runAgentContractTestDeps(t, ts.URL)
	defer deps.SessionCache.CloseAll()

	// Client-minted session id (the Desktop NewSession+SessionID path) so the
	// scratch path is known BEFORE the run; place an artifact there as if a
	// prior turn's filename injection had created it.
	const sessionID = "11111111-2222-3333-4444-555555555555"
	scratch, err := sessionScratchDirPath(deps.ShannonDir, sessionID)
	if err != nil || scratch == "" {
		t.Fatalf("sessionScratchDirPath: %q, %v", scratch, err)
	}
	if err := os.MkdirAll(scratch, 0o700); err != nil {
		t.Fatal(err)
	}
	artifact := filepath.Join(scratch, "screenshot-survives.png")
	if err := os.WriteFile(artifact, []byte("png"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := RunAgent(context.Background(), deps, RunAgentRequest{
		Text:          "hi",
		Source:        "desktop", // interactive non-cloud source
		NewSession:    true,
		SessionID:     sessionID,
		BypassRouting: true,
	}, nullEventHandler{}); err != nil {
		t.Fatalf("RunAgent: %v", err)
	}

	if _, err := os.Stat(artifact); err != nil {
		t.Fatalf("non-cloud artifact scratch was reclaimed on session close: %v", err)
	}
}

// Control for the survival test above (and the cloud half of the invariant):
// a cloud-source run registers cloudSessionTmpCleanup, so the same pre-placed
// artifact must be GONE after RunAgent returns. If OnSessionClose callbacks
// stopped firing on the ephemeral-manager path, this test fails instead of
// the survival test silently passing for the wrong reason.
func TestRunAgent_CloudArtifactScratchReclaimedOnSessionClose(t *testing.T) {
	gw := &fakeGatewayBackend{reply: "done"}
	ts := httptest.NewServer(gw.handler())
	defer ts.Close()

	deps := runAgentContractTestDeps(t, ts.URL)
	defer deps.SessionCache.CloseAll()

	const sessionID = "66666666-7777-8888-9999-aaaaaaaaaaaa"
	scratch, err := sessionScratchDirPath(deps.ShannonDir, sessionID)
	if err != nil || scratch == "" {
		t.Fatalf("sessionScratchDirPath: %q, %v", scratch, err)
	}
	if err := os.MkdirAll(scratch, 0o700); err != nil {
		t.Fatal(err)
	}
	artifact := filepath.Join(scratch, "screenshot-reclaimed.png")
	if err := os.WriteFile(artifact, []byte("png"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := RunAgent(context.Background(), deps, RunAgentRequest{
		Text:          "hi",
		Source:        "slack", // cloud source → scratch is the effective CWD, cleanup registered
		NewSession:    true,
		SessionID:     sessionID,
		BypassRouting: true,
	}, nullEventHandler{}); err != nil {
		t.Fatalf("RunAgent: %v", err)
	}

	if _, err := os.Stat(scratch); !os.IsNotExist(err) {
		t.Fatalf("cloud session scratch must be reclaimed on session close, stat err = %v", err)
	}
}
