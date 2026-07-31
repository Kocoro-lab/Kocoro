package guicontrol

import "testing"

func TestComputerUseControllerSurfaceBundleIDsAreExact(t *testing.T) {
	for _, bundleID := range []string{
		"run.shannon.shanclaw",
		"run.shannon.shanclaw.dev",
		"run.shannon.shanclaw.ax-server",
		"run.shannon.shanclaw.dev.ax-server",
	} {
		if !IsComputerUseControllerSurfaceBundleID(bundleID) {
			t.Errorf("controller bundle %q was not recognized", bundleID)
		}
	}
	for _, bundleID := range []string{
		"",
		"run.shannon.shanclaw.preview",
		"com.example.run.shannon.shanclaw",
		"com.apple.systempreferences",
		"com.apple.terminal",
	} {
		if IsComputerUseControllerSurfaceBundleID(bundleID) {
			t.Errorf("ordinary or protected app %q was classified as controller", bundleID)
		}
	}
}
