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
	_, err := ParseCarrierProfile(CarrierInputs{Carrier: "reachy_pro"})
	if err == nil {
		t.Fatal("unknown --carrier must fail loud, got nil err")
	}
	if !strings.Contains(err.Error(), "carrier") {
		t.Errorf("error should mention carrier, got: %v", err)
	}
}

func TestParseCarrierProfile_ReachyWirelessDefaultCaps(t *testing.T) {
	// reachy_wireless (Wireless SKU, §07/§21): a body that hears/sees/moves but
	// has NO screen — the caps superset drops has_screen vs reachy_lite.
	p, err := ParseCarrierProfile(CarrierInputs{Carrier: CarrierReachyWireless})
	if err != nil {
		t.Fatalf("reachy_wireless should parse, got err: %v", err)
	}
	want := []string{CapFullDuplex, CapHasBody, CapHasCamera, CapHasFace} // no has_screen, sorted
	if !slices.Equal(p.Caps, want) {
		t.Errorf("reachy_wireless default caps = %v, want %v (superset minus has_screen)", p.Caps, want)
	}
}

func TestParseCarrierProfile_ReachyWirelessHasNoScreen(t *testing.T) {
	p, err := ParseCarrierProfile(CarrierInputs{Carrier: CarrierReachyWireless})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if p.HasCap(CapHasScreen) {
		t.Error("reachy_wireless HasCap(has_screen) = true, want false (no display on the wireless body)")
	}
	for _, cap := range []string{CapFullDuplex, CapHasBody, CapHasCamera, CapHasFace} {
		if !p.HasCap(cap) {
			t.Errorf("reachy_wireless HasCap(%s) = false, want true", cap)
		}
	}
}

func TestParseCarrierProfile_ReachyWirelessNoDeviceUIDsRequired(t *testing.T) {
	// Unlike reachy_lite (Mac CoreAudio UID binding), reachy_wireless audio lives
	// on the CM4/ALSA side, so Koe binds no CoreAudio device UID — the lite
	// missing-UID fail-loud must NOT apply. The ALSA-side "bind card 0, fail loud"
	// rule is the audio layer's (U1 un-darwin), not the carrier profile's.
	p, err := ParseCarrierProfile(CarrierInputs{Carrier: CarrierReachyWireless})
	if err != nil {
		t.Fatalf("reachy_wireless must parse without mic/speaker UIDs, got err: %v", err)
	}
	if p.MicUID != "" || p.SpeakerUID != "" {
		t.Errorf("reachy_wireless should carry no CoreAudio UIDs, got mic=%q spk=%q", p.MicUID, p.SpeakerUID)
	}
}

func TestParseCarrierProfile_ReachyWirelessDaemonURL(t *testing.T) {
	p, err := ParseCarrierProfile(CarrierInputs{Carrier: CarrierReachyWireless})
	if err != nil {
		t.Fatal(err)
	}
	if p.ReachyDaemonURL != reachyWirelessDefaultDaemonURL {
		t.Errorf("wireless daemon URL = %q, want %q", p.ReachyDaemonURL, reachyWirelessDefaultDaemonURL)
	}

	p, err = ParseCarrierProfile(CarrierInputs{
		Carrier:         CarrierReachyWireless,
		ReachyDaemonURL: "http://reachy-mini.local:8000",
	})
	if err != nil {
		t.Fatal(err)
	}
	if p.ReachyDaemonURL != "http://reachy-mini.local:8000" {
		t.Errorf("explicit wireless daemon URL lost: %q", p.ReachyDaemonURL)
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

func TestParseCarrierProfile_OpenModeDefaultsToTrigger(t *testing.T) {
	for _, carrier := range []string{CarrierMac, CarrierReachyWireless} {
		p, err := ParseCarrierProfile(CarrierInputs{Carrier: carrier})
		if err != nil {
			t.Fatalf("%s: unexpected err: %v", carrier, err)
		}
		if p.OpenMode != OpenModeTrigger {
			t.Errorf("%s: open mode = %q, want %q", carrier, p.OpenMode, OpenModeTrigger)
		}
	}
}

func TestParseCarrierProfile_OpenModeStandbyIsWirelessOnly(t *testing.T) {
	p, err := ParseCarrierProfile(CarrierInputs{Carrier: CarrierReachyWireless, OpenMode: OpenModeStandby})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if p.OpenMode != OpenModeStandby {
		t.Errorf("wireless open mode = %q, want %q", p.OpenMode, OpenModeStandby)
	}

	// mac/lite must never enter resident listening, however the env is set.
	for _, carrier := range []string{CarrierMac, CarrierReachyLite} {
		in := CarrierInputs{Carrier: carrier, OpenMode: OpenModeStandby}
		if carrier == CarrierReachyLite {
			in.MicUID, in.SpeakerUID = "mic", "spk"
		}
		p, err := ParseCarrierProfile(in)
		if err != nil {
			t.Fatalf("%s: unexpected err: %v", carrier, err)
		}
		if p.OpenMode != OpenModeTrigger {
			t.Errorf("%s: open mode = %q, want %q", carrier, p.OpenMode, OpenModeTrigger)
		}
	}
}

func TestParseCarrierProfile_UnknownOpenModeFailsLoud(t *testing.T) {
	_, err := ParseCarrierProfile(CarrierInputs{Carrier: CarrierReachyWireless, OpenMode: "always_on"})
	if err == nil || !strings.Contains(err.Error(), "KOE_OPEN_MODE") {
		t.Fatalf("err = %v, want a KOE_OPEN_MODE validation error", err)
	}
}
