//go:build !kocoro_integration

package daemon

import "github.com/Kocoro-lab/ShanClaw/internal/guicontrol"

// The production build deliberately contains no environment-driven fixture
// behavior. Server.Start calls this seam unconditionally so the tagged build
// can exercise the exact startup ordering without adding a production route.
func startComputerUseIntegrationFixture(
	_ *guicontrol.Coordinator,
) (func(), error) {
	return func() {}, nil
}
