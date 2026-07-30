package guicontrol

import "strings"

var computerUseControllerSurfaceBundleIDs = []string{
	"run.shannon.shanclaw",
	"run.shannon.shanclaw.ax-server",
	"run.shannon.shanclaw.dev",
	"run.shannon.shanclaw.dev.ax-server",
}

var computerUseControllerSurfaceBundleIDSet = func() map[string]struct{} {
	set := make(map[string]struct{}, len(computerUseControllerSurfaceBundleIDs))
	for _, bundleID := range computerUseControllerSurfaceBundleIDs {
		set[bundleID] = struct{}{}
	}
	return set
}()

// IsComputerUseControllerSurfaceBundleID identifies only Kocoro-owned UI and
// helper processes that may temporarily become frontmost while the user
// supervises an existing Computer Use task.
func IsComputerUseControllerSurfaceBundleID(bundleID string) bool {
	normalized := strings.ToLower(strings.TrimSpace(bundleID))
	_, ok := computerUseControllerSurfaceBundleIDSet[normalized]
	return ok
}

// ComputerUseControllerSurfaceBundleIDs returns a copy so app-policy and
// target-retention logic share one exact controller identity list.
func ComputerUseControllerSurfaceBundleIDs() []string {
	result := make([]string, len(computerUseControllerSurfaceBundleIDs))
	copy(result, computerUseControllerSurfaceBundleIDs)
	return result
}
