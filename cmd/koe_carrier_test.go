//go:build darwin && cgo

package cmd

import (
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/koe"
)

func TestResolveCarrierProfile_MacLeavesAECUntouched(t *testing.T) {
	cfg := defaultKoeConfig() // aec == ""
	prof, err := resolveCarrierProfile(&cfg, "", "", "", "")
	if err != nil {
		t.Fatalf("mac should resolve, got err: %v", err)
	}
	if prof.Carrier != koe.CarrierMac {
		t.Errorf("carrier = %q, want mac", prof.Carrier)
	}
	// Mac byte-equivalence red line: no new flag may nudge the runtime aec.
	if cfg.aec != "" {
		t.Errorf("mac must not mutate cfg.aec, got %q", cfg.aec)
	}
}

func TestResolveCarrierProfile_ReachyLitePresetsVPIO(t *testing.T) {
	cfg := defaultKoeConfig()
	cfg.micDevice = "ReachyMic-UID"
	cfg.speakerDevice = "ReachySpk-UID"
	// aec left "" — the preset must select vpio so the device UIDs actually bind.
	prof, err := resolveCarrierProfile(&cfg, koe.CarrierReachyLite, "", "", "")
	if err != nil {
		t.Fatalf("reachy_lite should resolve with UIDs, got err: %v", err)
	}
	if cfg.aec != "vpio" {
		t.Errorf("reachy_lite must preset cfg.aec=vpio, got %q", cfg.aec)
	}
	if prof.AudioBackend != "vpio" {
		t.Errorf("profile AudioBackend = %q, want vpio", prof.AudioBackend)
	}
}

func TestResolveCarrierProfile_ExplicitAECWinsOverPreset(t *testing.T) {
	cfg := defaultKoeConfig()
	cfg.micDevice = "ReachyMic-UID"
	cfg.speakerDevice = "ReachySpk-UID"
	cfg.aec = "gate" // explicit power-user choice
	prof, err := resolveCarrierProfile(&cfg, koe.CarrierReachyLite, "", "", "")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if cfg.aec != "gate" {
		t.Errorf("explicit --aec gate must survive the reachy_lite preset, got %q", cfg.aec)
	}
	if prof.AudioBackend != "gate" {
		t.Errorf("profile AudioBackend = %q, want gate", prof.AudioBackend)
	}
}

func TestResolveCarrierProfile_UnknownCarrierFailsLoud(t *testing.T) {
	cfg := defaultKoeConfig()
	if _, err := resolveCarrierProfile(&cfg, "reachy_wireless", "", "", ""); err == nil {
		t.Fatal("unknown --carrier must fail loud")
	}
}
