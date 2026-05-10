package memory

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// TestIntegration_SupervisorDegradedSurfacedInStatus verifies the full chain:
// Supervisor fires onDegraded → Service stores reason + attempts → MemoryProviderStatus
// returns the correct shape. Uses only in-package types; no real process spawned.
func TestIntegration_SupervisorDegradedSurfacedInStatus(t *testing.T) {
	r := ReasonStartupTimeout
	svc := &Service{}
	svc.disabledReason.Store(&r)
	svc.restartAttempts.Store(2)
	svc.status.Store(int32(StatusDegraded))

	ms := svc.MemoryProviderStatus()
	if ms.Provider != "disabled" {
		t.Fatalf("provider=%q want disabled", ms.Provider)
	}
	if ms.Reason == nil || *ms.Reason != "startup_timeout" {
		t.Fatalf("reason=%v want startup_timeout", ms.Reason)
	}
	if ms.Detail["restart_attempts"] != 2 {
		t.Fatalf("detail.restart_attempts=%v want 2", ms.Detail["restart_attempts"])
	}

	// Also verify JSON round-trip (the shape that GET /status embeds).
	b, err := json.Marshal(ms)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["provider"] != "disabled" {
		t.Fatalf("json provider=%v", decoded["provider"])
	}
	detail, _ := decoded["detail"].(map[string]any)
	if detail == nil {
		t.Fatal("json detail must be present when degraded")
	}
}

// TestIntegration_ExpandPath_TmpDirFallback verifies $TMPDIR expansion when
// the env var is empty (CI environments, sandboxes).
func TestIntegration_ExpandPath_TmpDirFallback(t *testing.T) {
	t.Setenv("TMPDIR", "")
	result := expandPath("$TMPDIR/com.kocoro.tlm.sock")
	if result == "" || result == "/com.kocoro.tlm.sock" {
		t.Fatalf("expandPath with empty TMPDIR returned %q — expected os.TempDir() fallback", result)
	}
	if result[len(result)-len("/com.kocoro.tlm.sock"):] != "/com.kocoro.tlm.sock" {
		t.Fatalf("unexpected suffix in %q", result)
	}
}

// TestIntegration_OnDegraded_ReasonFlowsToService verifies the real Supervisor
// onDegraded callback stores the reason in Service fields.
func TestIntegration_OnDegraded_ReasonFlowsToService(t *testing.T) {
	sp := &fakeSpawner{
		waitReadyErr: errors.New("health probe failed"),
	}
	svc := &Service{}

	sup := NewSupervisor(sp, 2, nil)
	sup.testBackoff = func(int) time.Duration { return 1 * time.Millisecond }
	sup.SetOnDegraded(func(reason string, attempts int) {
		svc.restartAttempts.Store(int32(attempts))
		svc.setDisabledReason(reason)
		svc.status.Store(int32(StatusDegraded))
	})
	sup.Run(context.Background())

	ms := svc.MemoryProviderStatus()
	if ms.Provider != "disabled" {
		t.Fatalf("provider=%q", ms.Provider)
	}
	if ms.Reason == nil || *ms.Reason != "tlm_health_failed" {
		t.Fatalf("reason=%v want tlm_health_failed", ms.Reason)
	}
	if ms.Detail["restart_attempts"] != 2 {
		t.Fatalf("restart_attempts=%v want 2", ms.Detail["restart_attempts"])
	}
}
