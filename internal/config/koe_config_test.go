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
		"  mic_device: BuiltInMicrophoneDevice\n" +
		"  speaker_device: BuiltInSpeakerDevice\n" +
		"  audio_processing: clean_device\n" +
		"  agent: finance\n" +
		"  language: ja\n" +
		"  barge_in: true\n" +
		"  persona_source: custom\n" +
		"  custom_persona: The user is Alice.\n"
	if err := os.WriteFile(filepath.Join(shannonDir, "config.yaml"), []byte(yaml), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	if cfg.Koe.Enabled == nil || !*cfg.Koe.Enabled {
		t.Error("koe.enabled should be true after Load")
	}
	if cfg.Koe.Model != "gpt-realtime-mini-2025-12-15" {
		t.Errorf("koe.model = %q", cfg.Koe.Model)
	}
	if cfg.Koe.Voice != "marin" || cfg.Koe.Agent != "finance" || cfg.Koe.Language != "ja" {
		t.Errorf("koe section not loaded as expected: %+v", cfg.Koe)
	}
	if cfg.Koe.MicDevice != "BuiltInMicrophoneDevice" || cfg.Koe.SpeakerDevice != "BuiltInSpeakerDevice" {
		t.Fatalf("koe device fields not parsed: mic=%q speaker=%q", cfg.Koe.MicDevice, cfg.Koe.SpeakerDevice)
	}
	if cfg.Koe.AudioProcessing != "clean_device" {
		t.Fatalf("koe.audio_processing = %q, want clean_device", cfg.Koe.AudioProcessing)
	}
	if cfg.Koe.BargeIn == nil || !*cfg.Koe.BargeIn {
		t.Errorf("koe.barge_in = %v, want &true", cfg.Koe.BargeIn)
	}
	if cfg.Koe.PersonaSource != "custom" || cfg.Koe.CustomPersona != "The user is Alice." {
		t.Fatalf("koe persona fields not parsed: source=%q custom=%q", cfg.Koe.PersonaSource, cfg.Koe.CustomPersona)
	}
}

// TestLoad_KoeSectionAbsent confirms an omitted koe section loads as nil
// Enabled ("never set") rather than a load error — consumers apply the
// default-ON policy to nil.
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
	if cfg.Koe.Enabled != nil {
		t.Error("koe.enabled should be nil (never set) when the section is absent")
	}
}

// TestLoad_KoeBargeInExplicitFalse confirms barge_in: false survives load as &false,
// not nil — the *bool contract the field documents (so Desktop's RFC-7386 PATCH can
// persist barge-in OFF, exactly as Enabled does via TestLoad_KoeExplicitFalse).
func TestLoad_KoeBargeInExplicitFalse(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	shannonDir := filepath.Join(home, ".shannon")
	if err := os.MkdirAll(shannonDir, 0700); err != nil {
		t.Fatalf("mkdir shannon dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(shannonDir, "config.yaml"), []byte("koe:\n  barge_in: false\n"), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	if cfg.Koe.BargeIn == nil || *cfg.Koe.BargeIn {
		t.Errorf("koe.barge_in = %v, want explicit &false", cfg.Koe.BargeIn)
	}
}

// TestLoad_KoeExplicitFalse confirms an explicit opt-out survives load as
// &false — distinguishable from the nil "never set" state (the default-ON
// policy must not resurrect voice for users who turned it off).
func TestLoad_KoeExplicitFalse(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	shannonDir := filepath.Join(home, ".shannon")
	if err := os.MkdirAll(shannonDir, 0700); err != nil {
		t.Fatalf("mkdir shannon dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(shannonDir, "config.yaml"), []byte("koe:\n  enabled: false\n"), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	if cfg.Koe.Enabled == nil || *cfg.Koe.Enabled {
		t.Errorf("koe.enabled = %v, want explicit false", cfg.Koe.Enabled)
	}
}
