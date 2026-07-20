package update

import "testing"

func TestAutoUpdateSkipsNonReleaseBuildsBeforeTouchingCache(t *testing.T) {
	t.Parallel()

	for _, version := range []string{
		"dev",
		"0.3.5-79-g1a466fb2",
		"0.3.5-79-g1a466fb2-dirty",
		"0.4.0-rc.1",
		"0.3.5+local",
	} {
		version := version
		t.Run(version, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			if got := AutoUpdate(version, dir); got != "" {
				t.Fatalf("AutoUpdate(%q) = %q, want empty", version, got)
			}
			if !NewUpdateCache(dir + "/update-check.json").ShouldCheck() {
				t.Fatalf("AutoUpdate(%q) touched the update cache", version)
			}
		})
	}
}
