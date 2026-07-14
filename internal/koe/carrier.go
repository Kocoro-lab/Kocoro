package koe

import (
	"fmt"
	"slices"
	"strings"
)

// Carrier identifiers — which physical body Koe drives for this process
// lifetime. The carrier and its capability bits (§18) are injected statically at
// startup and never flip mid-process; ParseCarrierProfile resolves them once.
const (
	CarrierMac            = "mac"
	CarrierReachyLite     = "reachy_lite"
	CarrierReachyWireless = "reachy_wireless"
)

// Capability bits — the closed set (desktop-koe-carrier-control-spec §2). Adding
// a token is a spec change (two-repo PR), not just a code change.
const (
	CapFullDuplex = "full_duplex"
	CapHasCamera  = "has_camera"
	CapHasBody    = "has_body"
	CapHasFace    = "has_face"
	CapHasScreen  = "has_screen"
)

// Audio backends reported in /carrier/status.audio.backend (mirror --aec values).
const (
	audioBackendVPIO = "vpio"
	audioBackendGate = "gate"
)

// reachyDefaultDaemonURL is the Pollen daemon read-plane (DOA/volume) address
// under reachy_lite when --reachy-daemon-url is not given (plan §16: port 7534).
const reachyDefaultDaemonURL = "http://127.0.0.1:7534"

var validCaps = map[string]struct{}{
	CapFullDuplex: {}, CapHasCamera: {}, CapHasBody: {}, CapHasFace: {}, CapHasScreen: {},
}

// carrierDefaultCaps is the caps superset each carrier declares when --caps is
// omitted (§07). reachy_lite (Lite shares the Mac screen) is the superset.
var carrierDefaultCaps = map[string][]string{
	CarrierMac:        {CapHasScreen},
	CarrierReachyLite: {CapFullDuplex, CapHasCamera, CapHasBody, CapHasFace, CapHasScreen},
	// reachy_wireless is the same body as reachy_lite MINUS the screen (§07/§21):
	// the Wireless SKU has no display, so has_screen is dropped from the superset.
	CarrierReachyWireless: {CapFullDuplex, CapHasCamera, CapHasBody, CapHasFace},
}

// CarrierInputs is the raw, already-resolved configuration passed to
// ParseCarrierProfile. The caller resolves --aec (flag/env) and the device UIDs
// first; this function only validates them and derives the carrier preset.
type CarrierInputs struct {
	Carrier         string // --carrier ("" → mac)
	CapsCSV         string // --caps ("" → carrier default superset)
	BridgeSocket    string // --bridge-socket ("" → motion disabled)
	ReachyDaemonURL string // --reachy-daemon-url ("" → reachy_lite default)
	AEC             string // resolved --aec/KOE_AEC ("" → carrier preset default)
	MicUID          string // --mic-device (CoreAudio input UID)
	SpeakerUID      string // --speaker-device (CoreAudio output UID)
	Model           string // realtime model
	Agent           string // bound back-brain agent slug
}

// CarrierProfile is Koe's resolved, immutable carrier identity for the process
// lifetime (§18 static injection). Build it once via ParseCarrierProfile; nothing
// mutates it afterward.
type CarrierProfile struct {
	Carrier         string
	Caps            []string // closed-set tokens, deduped + sorted
	BridgeSocket    string
	ReachyDaemonURL string // empty under mac
	AudioBackend    string // "vpio" | "gate" | explicit --aec
	MicUID          string
	SpeakerUID      string
	Model           string
	Agent           string
}

// ParseCarrierProfile validates the carrier/caps inputs fail-loud (unknown values
// are argv bugs, not user input — Desktop generates the argv) and derives the
// carrier preset. Explicit flags always win over preset defaults.
func ParseCarrierProfile(in CarrierInputs) (CarrierProfile, error) {
	carrier := in.Carrier
	if carrier == "" {
		carrier = CarrierMac
	}
	if carrier != CarrierMac && carrier != CarrierReachyLite && carrier != CarrierReachyWireless {
		return CarrierProfile{}, fmt.Errorf("invalid --carrier %q (want mac, reachy_lite, or reachy_wireless)", in.Carrier)
	}

	caps, err := resolveCaps(carrier, in.CapsCSV)
	if err != nil {
		return CarrierProfile{}, err
	}

	p := CarrierProfile{
		Carrier:      carrier,
		Caps:         caps,
		BridgeSocket: in.BridgeSocket,
		MicUID:       in.MicUID,
		SpeakerUID:   in.SpeakerUID,
		Model:        in.Model,
		Agent:        in.Agent,
	}

	// Audio backend: an explicit --aec always wins. Otherwise reachy_lite presets
	// vpio — the mic/speaker device UIDs only bind on the VPIO backend — and mac
	// keeps its current half-duplex gate default. The caller applies this to the
	// runtime aec only for reachy_lite; mac stays byte-identical.
	switch {
	case in.AEC != "":
		p.AudioBackend = in.AEC
	case carrier == CarrierReachyLite:
		p.AudioBackend = audioBackendVPIO
	default:
		p.AudioBackend = audioBackendGate
	}

	if carrier == CarrierReachyLite {
		p.ReachyDaemonURL = in.ReachyDaemonURL
		if p.ReachyDaemonURL == "" {
			p.ReachyDaemonURL = reachyDefaultDaemonURL
		}
		// M1 preset fail-loud (§02 lesson): reachy_lite must bind the explicit
		// Reachy device UIDs; silently falling back to the system default mic or
		// speaker is exactly the trap that made "voice ran" a false positive.
		if in.MicUID == "" || in.SpeakerUID == "" {
			return CarrierProfile{}, fmt.Errorf("--carrier reachy_lite requires --mic-device and --speaker-device (Reachy audio UIDs); refusing to fall back to the system default")
		}
	}

	return p, nil
}

func resolveCaps(carrier, csv string) ([]string, error) {
	if strings.TrimSpace(csv) == "" {
		out := slices.Clone(carrierDefaultCaps[carrier])
		slices.Sort(out)
		return out, nil
	}
	seen := map[string]struct{}{}
	var out []string
	for _, raw := range strings.Split(csv, ",") {
		tok := strings.ToLower(strings.TrimSpace(raw))
		if tok == "" {
			continue
		}
		if _, ok := validCaps[tok]; !ok {
			return nil, fmt.Errorf("invalid --caps token %q (want a subset of full_duplex,has_camera,has_body,has_face,has_screen)", tok)
		}
		if _, dup := seen[tok]; dup {
			continue
		}
		seen[tok] = struct{}{}
		out = append(out, tok)
	}
	slices.Sort(out)
	return out, nil
}

// HasCap reports whether the carrier declared a capability bit. Drives the
// tool-set, turn-gate policy, and persona segment (M2/M5).
func (p CarrierProfile) HasCap(bit string) bool {
	return slices.Contains(p.Caps, bit)
}
