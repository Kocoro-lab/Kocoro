//go:build live

// test/e2e/sync_live_test.go
package e2e

import (
	"os"
	"testing"
)

// TestE2ESyncLive runs against a real Shannon Cloud sync endpoint. Currently
// always skipped — the endpoint does not yet exist (see Phase 2.0 spec
// "Cloud-side TODO"). When Cloud ships /api/v1/sessions/sync, replace the
// skip with assertions against a small synthetic dataset.
func TestE2ESyncLive(t *testing.T) {
	if os.Getenv("SHANNON_E2E_LIVE") != "1" {
		t.Skip("set SHANNON_E2E_LIVE=1 to enable")
	}
	t.Skip("cloud sync endpoint not yet live; replace this skip when /api/v1/sessions/sync exists")
}
