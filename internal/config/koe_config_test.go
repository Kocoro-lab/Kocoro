package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoad_KoeSection round-trips a koe section through config.yaml, proving the
// mapstructure/yaml tags resolve — this is what makes PATCH /config persistence
// and Desktop's settings panel work against cfg.Koe.
func TestLoad_KoeSection(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	shannonDir := filepath.Join(home, ".shannon")
	if err := os.MkdirAll(shannonDir, 0700); err != nil {
		t.Fatalf("mkdir shannon dir: %v", err)
	}
	yaml := "koe:\n" +
		"  enabled: true\n" +
		"  model: gpt-realtime-mini-2025-12-15\n" +
		"  voice: marin\n" +
		"  agent: finance\n" +
		"  language: ja\n"
	if err := os.WriteFile(filepath.Join(shannonDir, "config.yaml"), []byte(yaml), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	if !cfg.Koe.Enabled {
		t.Error("koe.enabled should be true after Load")
	}
	if cfg.Koe.Model != "gpt-realtime-mini-2025-12-15" {
		t.Errorf("koe.model = %q", cfg.Koe.Model)
	}
	if cfg.Koe.Voice != "marin" || cfg.Koe.Agent != "finance" || cfg.Koe.Language != "ja" {
		t.Errorf("koe section not loaded as expected: %+v", cfg.Koe)
	}
}

// TestLoad_KoeSectionAbsent confirms an omitted koe section is the zero value
// (disabled) rather than a load error — koe is opt-in.
func TestLoad_KoeSectionAbsent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	shannonDir := filepath.Join(home, ".shannon")
	if err := os.MkdirAll(shannonDir, 0700); err != nil {
		t.Fatalf("mkdir shannon dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(shannonDir, "config.yaml"), []byte("model_tier: medium\n"), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	if cfg.Koe.Enabled {
		t.Error("koe.enabled should default to false when the section is absent")
	}
}
