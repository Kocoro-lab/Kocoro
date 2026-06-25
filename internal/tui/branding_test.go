package tui

import (
	"strings"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/config"
)

// TestFormatConfigDisplay_KocoroBranding guards the /config screen title is
// Kocoro-branded (rebrand from "Shannon CLI Configuration").
func TestFormatConfigDisplay_KocoroBranding(t *testing.T) {
	got := formatConfigDisplay(&config.Config{})
	if !strings.Contains(got, "Kocoro CLI Configuration") {
		t.Errorf("config display should be Kocoro-branded; got first line:\n%s",
			strings.SplitN(got, "\n", 2)[0])
	}
}
