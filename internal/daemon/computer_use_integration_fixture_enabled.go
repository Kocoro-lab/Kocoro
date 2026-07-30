//go:build kocoro_integration

package daemon

import (
	"fmt"
	"os"
	"sync"

	"github.com/Kocoro-lab/ShanClaw/internal/guicontrol"
)

const (
	computerUseIntegrationScenarioEnv        = "KOCORO_INTEGRATION_COMPUTER_USE_SCENARIO"
	computerUseIntegrationScenarioActiveIdle = "active_idle"
)

// startComputerUseIntegrationFixture seeds an in-memory, action-free lease for
// signed Desktop integration tests. This file is absent from normal binaries.
// The fixed scenario has no arbitrary payload surface and never enters the
// agent, tool, Accessibility, CGEvent, model, or Cloud execution paths.
func startComputerUseIntegrationFixture(
	coordinator *guicontrol.Coordinator,
) (func(), error) {
	switch os.Getenv(computerUseIntegrationScenarioEnv) {
	case "":
		return func() {}, nil
	case computerUseIntegrationScenarioActiveIdle:
		// Continue below.
	default:
		return nil, fmt.Errorf("unsupported computer-use integration scenario")
	}
	if coordinator == nil {
		return nil, fmt.Errorf("computer-use integration coordinator is unavailable")
	}

	lease, err := coordinator.BeginWorkflow(guicontrol.WorkflowRequest{
		SessionID:        "integration/computer-use/active-idle",
		TurnID:           "integration/computer-use/active-idle",
		SourceKind:       "desktop",
		SourceLabel:      "Kocoro Integration",
		RequestedAppName: "Integration Fixture",
		PolicySnapshotID: "integration/computer-use/active-idle",
	})
	if err != nil {
		return nil, fmt.Errorf("seed active-idle computer-use integration lease: %w", err)
	}

	var cleanupOnce sync.Once
	cleanup := func() {
		cleanupOnce.Do(func() {
			snapshot := coordinator.Snapshot()
			if snapshot.Active == nil || snapshot.Active.LeaseID != lease.LeaseID {
				return
			}
			_ = coordinator.EndTurn(lease.TurnID, guicontrol.ComputerUseResultCancelled)
		})
	}
	return cleanup, nil
}
