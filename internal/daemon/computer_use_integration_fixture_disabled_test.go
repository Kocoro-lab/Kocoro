//go:build !kocoro_integration

package daemon

import (
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/guicontrol"
)

func TestComputerUseIntegrationFixtureDefaultBuildIsNoOp(t *testing.T) {
	t.Setenv("KOCORO_INTEGRATION_COMPUTER_USE_SCENARIO", "active_idle")
	coordinator := guicontrol.NewCoordinator(guicontrol.CoordinatorOptions{
		InstanceID: "cui_default_build",
	})

	cleanup, err := startComputerUseIntegrationFixture(coordinator)
	if err != nil {
		t.Fatalf("startComputerUseIntegrationFixture: %v", err)
	}
	t.Cleanup(cleanup)
	if snapshot := coordinator.Snapshot(); snapshot.Active != nil {
		t.Fatalf("default build seeded integration lease: %#v", snapshot.Active)
	}
}
