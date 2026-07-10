package koe

import (
	"slices"
	"strings"
	"testing"
)

// reachyUIDs is a minimal valid device pair so reachy_lite parses without the
// missing-UID fail-loud tripping in tests that aren't about that rule.
func reachyUIDs(in CarrierInputs) CarrierInputs {
	in.MicUID = "ReachyMic-UID"
	in.SpeakerUID = "ReachySpk-UID"
	return in
}

func TestParseCarrierProfile_MacDefaults(t *testing.T) {
	p, err := ParseCarrierProfile(CarrierInputs{})
	if err != nil {
		t.Fatalf("mac default should parse, got err: %v", err)
	}
	if p.Carrier != CarrierMac {
		t.Errorf("empty --carrier should default to mac, got %q", p.Carrier)
	}
	if want := []string{CapHasScreen}; !slices.Equal(p.Caps, want) {
		t.Errorf("mac default caps = %v, want %v", p.Caps, want)
	}
	// mac must not adopt the reachy daemon URL.
	if p.ReachyDaemonURL != "" {
		t.Errorf("mac ReachyDaemonURL = %q, want empty", p.ReachyDaemonURL)
	}
}

func TestParseCarrierProfile_ReachyLiteDefaultCaps(t *testing.T) {
	p, err := ParseCarrierProfile(reachyUIDs(CarrierInputs{Carrier: CarrierReachyLite}))
	if err != nil {
		t.Fatalf("reachy_lite should parse with UIDs, got err: %v", err)
	}
	want := []string{CapFullDuplex, CapHasBody, CapHasCamera, CapHasFace, CapHasScreen}
	if !slices.Equal(p.Caps, want) {
		t.Errorf("reachy_lite default caps = %v, want %v (full superset)", p.Caps, want)
	}
}

func TestParseCarrierProfile_ExplicitCapsOverrideAndSort(t *testing.T) {
	p, err := ParseCarrierProfile(reachyUIDs(CarrierInputs{
		Carrier: CarrierReachyLite,
		CapsCSV: "has_face, full_duplex ,has_face", // whitespace + duplicate
	}))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	want := []string{CapFullDuplex, CapHasFace} // deduped + sorted
	if !slices.Equal(p.Caps, want) {
		t.Errorf("explicit caps = %v, want %v", p.Caps, want)
	}
}

func TestParseCarrierProfile_UnknownCarrierFailsLoud(t *testing.T) {
	_, err := ParseCarrierProfile(CarrierInputs{Carrier: "reachy_wireless"})
	if err == nil {
		t.Fatal("unknown --carrier must fail loud, got nil err")
	}
	if !strings.Contains(err.Error(), "carrier") {
		t.Errorf("error should mention carrier, got: %v", err)
	}
}

func TestParseCarrierProfile_UnknownCapTokenFailsLoud(t *testing.T) {
	_, err := ParseCarrierProfile(reachyUIDs(CarrierInputs{
		Carrier: CarrierReachyLite,
		CapsCSV: "has_face,has_wings",
	}))
	if err == nil {
		t.Fatal("unknown caps token must fail loud, got nil err")
	}
	if !strings.Contains(err.Error(), "has_wings") {
		t.Errorf("error should name the bad token, got: %v", err)
	}
}

func TestParseCarrierProfile_ReachyLiteMissingMicUIDFailsLoud(t *testing.T) {
	_, err := ParseCarrierProfile(CarrierInputs{
		Carrier:    CarrierReachyLite,
		SpeakerUID: "ReachySpk-UID",
	})
	if err == nil {
		t.Fatal("reachy_lite without mic UID must fail loud (no silent system-default fallback)")
	}
}

func TestParseCarrierProfile_ReachyLiteMissingSpeakerUIDFailsLoud(t *testing.T) {
	_, err := ParseCarrierProfile(CarrierInputs{
		Carrier: CarrierReachyLite,
		MicUID:  "ReachyMic-UID",
	})
	if err == nil {
		t.Fatal("reachy_lite without speaker UID must fail loud")
	}
}

func TestParseCarrierProfile_ReachyLiteDaemonURLDefault(t *testing.T) {
	p, err := ParseCarrierProfile(reachyUIDs(CarrierInputs{Carrier: CarrierReachyLite}))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if p.ReachyDaemonURL != "http://127.0.0.1:7534" {
		t.Errorf("reachy_lite daemon URL default = %q, want http://127.0.0.1:7534", p.ReachyDaemonURL)
	}
}

func TestParseCarrierProfile_ReachyLiteExplicitDaemonURLWins(t *testing.T) {
	p, err := ParseCarrierProfile(reachyUIDs(CarrierInputs{
		Carrier:         CarrierReachyLite,
		ReachyDaemonURL: "http://127.0.0.1:9999",
	}))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if p.ReachyDaemonURL != "http://127.0.0.1:9999" {
		t.Errorf("explicit daemon URL should win, got %q", p.ReachyDaemonURL)
	}
}

func TestParseCarrierProfile_ReachyLiteAudioBackendDefaultsVPIO(t *testing.T) {
	// M1 preset: reachy_lite with no explicit --aec resolves to vpio so the
	// mic/speaker device UIDs actually bind (they are vpio-backend only).
	p, err := ParseCarrierProfile(reachyUIDs(CarrierInputs{Carrier: CarrierReachyLite}))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if p.AudioBackend != "vpio" {
		t.Errorf("reachy_lite default AudioBackend = %q, want vpio", p.AudioBackend)
	}
}

func TestParseCarrierProfile_ExplicitAECWins(t *testing.T) {
	p, err := ParseCarrierProfile(reachyUIDs(CarrierInputs{
		Carrier: CarrierReachyLite,
		AEC:     "gate",
	}))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if p.AudioBackend != "gate" {
		t.Errorf("explicit --aec gate should win over reachy_lite vpio preset, got %q", p.AudioBackend)
	}
}

func TestParseCarrierProfile_MacAudioBackendUntouched(t *testing.T) {
	// mac with no --aec keeps the current half-duplex default; the preset must
	// never nudge mac onto vpio.
	p, err := ParseCarrierProfile(CarrierInputs{Carrier: CarrierMac})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if p.AudioBackend == "vpio" {
		t.Errorf("mac AudioBackend must not default to vpio, got %q", p.AudioBackend)
	}
}

func TestCarrierProfile_HasCap(t *testing.T) {
	p, err := ParseCarrierProfile(reachyUIDs(CarrierInputs{
		Carrier: CarrierReachyLite,
		CapsCSV: "has_camera,has_body",
	}))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !p.HasCap(CapHasCamera) {
		t.Error("HasCap(has_camera) = false, want true")
	}
	if p.HasCap(CapFullDuplex) {
		t.Error("HasCap(full_duplex) = true, want false (not in explicit set)")
	}
}

func TestParseCarrierProfile_PassesModelAndAgentThrough(t *testing.T) {
	p, err := ParseCarrierProfile(CarrierInputs{
		Carrier: CarrierMac,
		Model:   "gpt-realtime-2.1-mini",
		Agent:   "kocoro-default",
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if p.Model != "gpt-realtime-2.1-mini" || p.Agent != "kocoro-default" {
		t.Errorf("model/agent not carried through: model=%q agent=%q", p.Model, p.Agent)
	}
}
