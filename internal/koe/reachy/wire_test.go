package reachy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// The fixtures in testdata/wire are byte-for-byte mirrors of the bridge repo's
// fixtures/wire (spec section 13): both ends decode the identical bytes. These
// tests decode each canonical frame into the Go structs and check the fields the
// client relies on, so a drift in either repo's shape fails here.

func loadFixture(t *testing.T, name string) Frame {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "wire", name+".json"))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	var f Frame
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatalf("decode envelope %s: %v", name, err)
	}
	return f
}

func TestFixtureHelloResultDecodes(t *testing.T) {
	f := loadFixture(t, "hello_result")
	if f.Type != FrameRPCResult {
		t.Fatalf("type = %s", f.Type)
	}
	var res RPCResult
	if err := json.Unmarshal(f.Payload, &res); err != nil {
		t.Fatal(err)
	}
	if !res.OK {
		t.Fatal("hello result should be ok")
	}
	var hr HelloResult
	if err := json.Unmarshal(res.Result, &hr); err != nil {
		t.Fatal(err)
	}
	if hr.Proto != "1.0" || hr.SdkVersion != "1.8.0" || len(hr.Moves) == 0 {
		t.Errorf("hello = %+v", hr)
	}
}

func TestFixturePlayMoveResultDecodes(t *testing.T) {
	f := loadFixture(t, "play_move_result")
	var res RPCResult
	if err := json.Unmarshal(f.Payload, &res); err != nil {
		t.Fatal(err)
	}
	var out struct {
		MoveID string `json:"move_id"`
		Queued int    `json:"queued"`
	}
	if err := json.Unmarshal(res.Result, &out); err != nil {
		t.Fatal(err)
	}
	if out.MoveID == "" || out.Queued != 1 {
		t.Errorf("play_move result = %+v", out)
	}
}

func TestFixtureErrorFramesDecode(t *testing.T) {
	for _, tc := range []struct {
		name string
		code string
	}{
		{"error_unknown_move", ErrCodeUnknownMove},
		{"error_version_mismatch", ErrCodeVersionMismatch},
	} {
		f := loadFixture(t, tc.name)
		var res RPCResult
		if err := json.Unmarshal(f.Payload, &res); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if res.OK || res.Error == nil {
			t.Fatalf("%s should carry an error", tc.name)
		}
		if res.Error.Code != tc.code {
			t.Errorf("%s code = %s, want %s", tc.name, res.Error.Code, tc.code)
		}
	}
}

func TestFixtureEventsDecode(t *testing.T) {
	for _, name := range []string{"event_move_started", "event_move_finished", "event_move_failed", "event_status"} {
		f := loadFixture(t, name)
		if f.Type != FrameEvent {
			t.Errorf("%s type = %s, want motion_event", name, f.Type)
		}
		var ev Event
		if err := json.Unmarshal(f.Payload, &ev); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if ev.Event == "" {
			t.Errorf("%s has empty event name", name)
		}
	}
}

func TestFixtureStreamAndRequestDecode(t *testing.T) {
	fs := loadFixture(t, "stream_speech_envelope")
	var s Stream
	if err := json.Unmarshal(fs.Payload, &s); err != nil {
		t.Fatal(err)
	}
	if s.Stream != StreamSpeechEnvelope {
		t.Errorf("stream = %s", s.Stream)
	}

	fr := loadFixture(t, "hello_request")
	var req RPCRequest
	if err := json.Unmarshal(fr.Payload, &req); err != nil {
		t.Fatal(err)
	}
	if req.Method != MethodHello {
		t.Errorf("request method = %s", req.Method)
	}
}
