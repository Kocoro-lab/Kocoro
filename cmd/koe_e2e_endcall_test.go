//go:build darwin && cgo

package cmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestKoeProductionPersonaDismissesOnFirstUtteranceE2E exercises the complete
// standalone production path: macOS speech synthesis -> Opus/WebRTC -> native
// Realtime understanding -> production persona/tool schema -> end_call ->
// handler-local terminal -> process return. ASR backstop is explicitly disabled.
func TestKoeProductionPersonaDismissesOnFirstUtteranceE2E(t *testing.T) {
	if os.Getenv("KOE_E2E") != "1" {
		t.Skip("production dismiss E2E: set KOE_E2E=1 (mints via the running daemon)")
	}
	t.Setenv("KOE_SAY_VOICE", "Tingting")
	t.Setenv("KOE_ASR_DISMISS_BACKSTOP", "0")
	t.Setenv("KOE_DISMISS_EARCON", "0")
	t.Setenv("KOE_TASK_LEDGER", "1")

	daemonURL := strings.TrimSpace(os.Getenv("KOE_DAEMON_URL"))
	if daemonURL == "" {
		daemonURL = "http://127.0.0.1:7533"
	}
	model := strings.TrimSpace(os.Getenv("KOE_E2E_MODEL"))
	if model == "" {
		model = "gpt-realtime-2.1"
	}
	outputPath := filepath.Join(t.TempDir(), "dismiss.wav")
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	done := make(chan error, 1)
	started := time.Now()
	go func() {
		done <- runKoeCall(ctx, koeConfig{
			daemonURL: daemonURL,
			model:     model,
			voice:     "marin",
			language:  "zh",
			sayText:   "退出吧",
			audioOut:  outputPath,
		})
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("production dismiss run: %v", err)
		}
		t.Logf("VERDICT: production persona ended on the first 退出吧 in %s with ASR control disabled", time.Since(started).Round(time.Millisecond))
	case <-ctx.Done():
		t.Fatal("production persona did not end the call on the first 退出吧")
	}
}
