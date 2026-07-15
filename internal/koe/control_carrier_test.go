//go:build darwin && cgo

package koe

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type carrierStatusWant struct {
	Carrier string   `json:"carrier"`
	Caps    []string `json:"caps"`
	Audio   struct {
		Backend    string `json:"backend"`
		MicUID     string `json:"mic_uid"`
		SpeakerUID string `json:"speaker_uid"`
		Bound      bool   `json:"bound"`
	} `json:"audio"`
	Bridge struct {
		State string `json:"state"`
	} `json:"bridge"`
	Model string `json:"model"`
	Agent string `json:"agent"`
}

func TestCarrierStatusEndpoint_ReachyLite(t *testing.T) {
	s := NewControlServer(nil, nil, nil)
	prof, err := ParseCarrierProfile(CarrierInputs{
		Carrier:    CarrierReachyLite,
		MicUID:     "ReachyMic-UID",
		SpeakerUID: "ReachySpk-UID",
		Model:      "gpt-realtime-2.1-mini",
		Agent:      "kocoro-robot",
	})
	if err != nil {
		t.Fatalf("build profile: %v", err)
	}
	s.SetCarrierProfile(prof, func() bool { return true })
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/carrier/status")
	if err != nil {
		t.Fatalf("GET /carrier/status: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var got carrierStatusWant
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Carrier != CarrierReachyLite {
		t.Errorf("carrier = %q, want reachy_lite", got.Carrier)
	}
	if len(got.Caps) != 5 {
		t.Errorf("caps = %v, want the 5-bit superset", got.Caps)
	}
	if got.Audio.Backend != "vpio" {
		t.Errorf("audio.backend = %q, want vpio", got.Audio.Backend)
	}
	if got.Audio.MicUID != "ReachyMic-UID" || got.Audio.SpeakerUID != "ReachySpk-UID" {
		t.Errorf("audio uids = %q / %q", got.Audio.MicUID, got.Audio.SpeakerUID)
	}
	if !got.Audio.Bound {
		t.Error("audio.bound = false, want true")
	}
	if got.Bridge.State != "disabled" {
		t.Errorf("bridge.state = %q, want disabled (M1)", got.Bridge.State)
	}
	if got.Model != "gpt-realtime-2.1-mini" {
		t.Errorf("model = %q", got.Model)
	}
	if got.Agent != "kocoro-robot" {
		t.Errorf("agent = %q", got.Agent)
	}
}

func TestCarrierStatusEndpoint_MacDefaultAndCapsArray(t *testing.T) {
	s := NewControlServer(nil, nil, nil)
	prof, err := ParseCarrierProfile(CarrierInputs{Carrier: CarrierMac})
	if err != nil {
		t.Fatalf("build profile: %v", err)
	}
	s.SetCarrierProfile(prof, func() bool { return prof.MicUID != "" && prof.SpeakerUID != "" })
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/carrier/status")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	s2 := string(body)
	// caps must serialize as a JSON array, never null (Desktop gates on membership).
	if !strings.Contains(s2, `"caps":["has_screen"]`) {
		t.Errorf("mac caps not rendered as array: %s", s2)
	}
	if !strings.Contains(s2, `"state":"disabled"`) {
		t.Errorf("bridge must be disabled in M1: %s", s2)
	}
	// mac with no explicit device UIDs is bound to the system default, not an
	// explicit device → bound=false.
	if !strings.Contains(s2, `"bound":false`) {
		t.Errorf("mac bound should be false: %s", s2)
	}
}

func TestBridgeStatusSSEEvent(t *testing.T) {
	s := NewControlServer(nil, nil, nil)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /events: %v", err)
	}
	defer resp.Body.Close()

	deadline := time.Now().Add(2 * time.Second)
	for s.subscriberCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	s.EmitBridgeStatus("disabled")

	br := bufio.NewReader(resp.Body)
	readDeadline := time.Now().Add(3 * time.Second)
	var line string
	for time.Now().Before(readDeadline) {
		l, err := br.ReadString('\n')
		if err != nil {
			break
		}
		if data, ok := strings.CutPrefix(l, "data: "); ok {
			line = strings.TrimSpace(data)
			break
		}
	}
	if line != `{"type":"bridge_status","state":"disabled"}` {
		t.Errorf("bridge_status SSE = %q, want disabled flat event", line)
	}
}

func TestBridgeStatusUpdatesCarrierSnapshot(t *testing.T) {
	s := NewControlServer(nil, nil, nil)
	s.EmitBridgeStatus("connected")
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/carrier/status")
	if err != nil {
		t.Fatalf("GET /carrier/status: %v", err)
	}
	defer resp.Body.Close()
	var got carrierStatusWant
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Bridge.State != "connected" {
		t.Fatalf("bridge.state = %q, want latest connected snapshot", got.Bridge.State)
	}
}
