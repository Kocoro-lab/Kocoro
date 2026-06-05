package daemon

import "testing"

// fireTitleAfterRun must be a safe no-op when gated out (nil deps, nil mgr,
// or a turn count outside the trigger set) — it must not panic or spawn work.
func TestFireTitleAfterRun_GatingIsNoOp(t *testing.T) {
	fireTitleAfterRun(nil, nil, "", "", "", nil, 2)                         // nil deps
	fireTitleAfterRun(&ServerDeps{}, nil, "s1", "slack", "Wayland", nil, 1) // nil mgr + nil GW
	fireTitleAfterRun(&ServerDeps{}, nil, "s1", "slack", "Wayland", nil, 2) // turn not in {1,3}
}
