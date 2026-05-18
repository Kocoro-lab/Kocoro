package daemon

import "testing"

func TestValidateSessionID(t *testing.T) {
	valid := []string{
		"2026-03-30-0154aef79640",      // production Kocoro format
		"2026-05-15-ca10391dad3a",
		"kocoro-cachetest-1778857984",  // legacy test session
		"abc123",
	}
	for _, id := range valid {
		if err := ValidateSessionID(id); err != nil {
			t.Errorf("ValidateSessionID(%q) returned %v, want nil", id, err)
		}
	}

	attacks := []string{
		"../../../../etc/passwd",
		"../foo",
		"./bar",
		"/abs/path",
		"a/b",
		`a\b`,
		".",
		"..",
		"foo/../bar",
		"",
	}
	for _, id := range attacks {
		if err := ValidateSessionID(id); err == nil {
			t.Errorf("ValidateSessionID(%q) returned nil, want error", id)
		}
	}
}
