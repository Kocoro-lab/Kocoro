package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/viper"
)

func TestLoadWithRevisionMatchesLoadedFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	viper.Reset()
	t.Cleanup(viper.Reset)

	shannonDir := filepath.Join(home, ".shannon")
	if err := os.MkdirAll(shannonDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(shannonDir, "config.yaml"),
		[]byte("provider: ollama\nagent:\n  temperature: 0.4\n"),
		0600,
	); err != nil {
		t.Fatal(err)
	}

	cfg, revision, err := LoadWithRevision()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != "ollama" || cfg.Agent.Temperature != 0.4 {
		t.Fatalf("loaded config = provider %q temperature %v", cfg.Provider, cfg.Agent.Temperature)
	}
	current, err := FileRevision(shannonDir)
	if err != nil {
		t.Fatal(err)
	}
	if revision != current {
		t.Fatalf("loaded revision = %q, disk revision = %q", revision, current)
	}
}
