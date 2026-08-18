//go:build darwin && cgo

package koe

import "testing"

// TestToolDefs_NeverExposeSetWorkPlan is a tripwire, not a mechanism:
// ToolDefs() is a hardcoded literal with no registry connection, so nothing
// leaks in by accident today. This test exists so a future refactor that
// derives Realtime tools from the daemon registry fails loudly instead of
// silently handing the voice front-brain a progress tool it must not have —
// detailed progress belongs in daemon session history and Desktop; voice
// stays coarse.
func TestToolDefs_NeverExposeSetWorkPlan(t *testing.T) {
	for _, def := range ToolDefs() {
		if def.Name == "set_work_plan" {
			t.Fatal("set_work_plan must never be exposed to the Koe Realtime tool set")
		}
	}
}
