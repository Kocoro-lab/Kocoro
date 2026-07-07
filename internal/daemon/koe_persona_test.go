package daemon

import (
	"context"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/config"
)

// TestBuildKoePersonaCustomSource verifies the "custom" persona source returns the
// user's text verbatim (trimmed) and never reaches the distill path — so no gateway
// is required.
func TestBuildKoePersonaCustomSource(t *testing.T) {
	s := &Server{deps: &ServerDeps{Config: &config.Config{}}}
	s.deps.Config.Koe.PersonaSource = "custom"
	s.deps.Config.Koe.CustomPersona = "  The user is Alice, a product designer who prefers English.  "

	got, err := s.buildKoePersona(context.Background())
	if err != nil {
		t.Fatalf("buildKoePersona: %v", err)
	}
	want := "The user is Alice, a product designer who prefers English."
	if got != want {
		t.Fatalf("custom persona = %q, want %q (verbatim, trimmed, no distill)", got, want)
	}
}

// TestBuildKoePersonaCustomEmptyFallsBack verifies an empty custom persona yields ""
// so Koe uses its base persona only, rather than an empty-but-selected custom source.
func TestBuildKoePersonaCustomEmptyFallsBack(t *testing.T) {
	s := &Server{deps: &ServerDeps{Config: &config.Config{}}}
	s.deps.Config.Koe.PersonaSource = "custom"
	s.deps.Config.Koe.CustomPersona = "   "

	got, err := s.buildKoePersona(context.Background())
	if err != nil {
		t.Fatalf("buildKoePersona: %v", err)
	}
	if got != "" {
		t.Fatalf("empty custom persona = %q, want \"\" (base persona only)", got)
	}
}
