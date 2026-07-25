//go:build kocoro_integration

package daemon

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/guicontrol"
)

func TestComputerUseIntegrationFixtureActiveIdleRunsThroughServerStartupAndCleansUp(t *testing.T) {
	t.Setenv(computerUseIntegrationScenarioEnv, computerUseIntegrationScenarioActiveIdle)
	coordinator := guicontrol.NewCoordinator(guicontrol.CoordinatorOptions{
		InstanceID: "cui_integration_active_idle",
		LeaseTTL:   time.Minute,
	})

	server := NewServer(-1, nil, nil, "test")
	server.SetComputerUseCoordinator(coordinator)
	events := server.EventBus().Subscribe()
	defer server.EventBus().Unsubscribe(events)

	err := server.Start(context.Background())
	if err == nil {
		t.Fatal("Start succeeded despite invalid port")
	}
	if !strings.Contains(err.Error(), "listen") {
		t.Fatalf("Start error = %v, want listen failure", err)
	}

	active := requireComputerUseIntegrationEvent(t, events)
	if active.LeaseState != guicontrol.ComputerUseLeaseActive {
		t.Fatalf("seed lease_state = %q, want active", active.LeaseState)
	}
	if active.ActionPhase != guicontrol.ComputerUsePhaseIdle {
		t.Fatalf("seed action_phase = %q, want idle", active.ActionPhase)
	}
	if active.ActionID != "" || active.ToolUseID != "" || active.ActionKind != "" {
		t.Fatalf("seed unexpectedly began an action: %#v", active)
	}
	if active.Pointer != nil || active.ExecutionPath != nil || active.ConsequentialRisk != nil {
		t.Fatalf("seed unexpectedly carried GUI execution state: %#v", active)
	}
	if active.SourceKind != "desktop" || active.TargetAppName != "Integration Fixture" {
		t.Fatalf("seed presentation = %#v", active)
	}
	if active.TargetBundleID != "" {
		t.Fatalf("seed target_bundle_id = %q, want empty side-effect-free target", active.TargetBundleID)
	}

	terminal := requireComputerUseIntegrationEvent(t, events)
	if terminal.LeaseID != active.LeaseID {
		t.Fatalf("cleanup lease_id = %q, want %q", terminal.LeaseID, active.LeaseID)
	}
	if terminal.LeaseState != guicontrol.ComputerUseLeaseTerminal {
		t.Fatalf("cleanup lease_state = %q, want terminal", terminal.LeaseState)
	}
	if terminal.ActionResult == nil ||
		*terminal.ActionResult != guicontrol.ComputerUseResultCancelled {
		t.Fatalf("cleanup action_result = %#v, want cancelled", terminal.ActionResult)
	}
	if snapshot := coordinator.Snapshot(); snapshot.Active != nil {
		t.Fatalf("server exit retained integration lease: %#v", snapshot.Active)
	}
}

func TestComputerUseIntegrationFixtureUnknownScenarioFailsClosed(t *testing.T) {
	for _, scenario := range []string{"unknown", " active_idle ", "ACTIVE_IDLE"} {
		t.Run(scenario, func(t *testing.T) {
			t.Setenv(computerUseIntegrationScenarioEnv, scenario)
			coordinator := guicontrol.NewCoordinator(guicontrol.CoordinatorOptions{
				InstanceID: "cui_integration_unknown",
			})
			server := NewServer(0, nil, nil, "test")
			server.SetComputerUseCoordinator(coordinator)

			err := server.Start(context.Background())
			if err == nil ||
				!strings.Contains(err.Error(), "unsupported computer-use integration scenario") {
				t.Fatalf("Start error = %v, want unsupported-scenario failure", err)
			}
			if snapshot := coordinator.Snapshot(); snapshot.Active != nil {
				t.Fatalf("unknown scenario seeded lease: %#v", snapshot.Active)
			}
		})
	}
}

func TestComputerUseIntegrationFixtureUnsetIsNoOp(t *testing.T) {
	t.Setenv(computerUseIntegrationScenarioEnv, "")
	coordinator := guicontrol.NewCoordinator(guicontrol.CoordinatorOptions{
		InstanceID: "cui_integration_unset",
	})

	cleanup, err := startComputerUseIntegrationFixture(coordinator)
	if err != nil {
		t.Fatalf("startComputerUseIntegrationFixture: %v", err)
	}
	cleanup()
	if snapshot := coordinator.Snapshot(); snapshot.Active != nil {
		t.Fatalf("unset scenario seeded lease: %#v", snapshot.Active)
	}
}

func requireComputerUseIntegrationEvent(
	t *testing.T,
	events <-chan Event,
) guicontrol.ComputerUseActivityEvent {
	t.Helper()
	select {
	case event := <-events:
		if event.Type != EventComputerUseActivity {
			t.Fatalf("event type = %q, want %q", event.Type, EventComputerUseActivity)
		}
		var activity guicontrol.ComputerUseActivityEvent
		if err := json.Unmarshal(event.Payload, &activity); err != nil {
			t.Fatalf("decode computer-use event: %v", err)
		}
		return activity
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for computer-use activity event")
		return guicontrol.ComputerUseActivityEvent{}
	}
}
