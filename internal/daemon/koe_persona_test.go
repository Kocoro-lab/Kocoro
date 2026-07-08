package daemon

import (
	"context"
	"os"
	"path/filepath"
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

// TestBuildKoePersonaReadsFreshConfigAfterPatch is the regression for the stale-config
// bug: PATCH /config writes config.yaml but does not refresh s.deps.Config, so
// buildKoePersona must read the file to see a persona the user just saved from Desktop
// (otherwise it takes effect only after a daemon reload/restart).
func TestBuildKoePersonaReadsFreshConfigAfterPatch(t *testing.T) {
	dir := t.TempDir()
	yaml := "koe:\n  persona_source: custom\n  custom_persona: \"The user is Kanye.\"\n"
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(yaml), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	// s.deps.Config is STALE (no persona set) — the post-PATCH, pre-reload state.
	s := &Server{deps: &ServerDeps{ShannonDir: dir, Config: &config.Config{}}}

	got, err := s.buildKoePersona(context.Background())
	if err != nil {
		t.Fatalf("buildKoePersona: %v", err)
	}
	if got != "The user is Kanye." {
		t.Fatalf("persona = %q, want the fresh custom text read from config.yaml", got)
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
