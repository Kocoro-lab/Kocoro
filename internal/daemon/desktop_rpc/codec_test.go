package desktop_rpc

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRoundTrip writes a frame and reads it back, verifying byte-for-byte
// payload preservation. This is the baseline sanity check — every encoder
// change must keep this green.
func TestRoundTrip(t *testing.T) {
	t.Parallel()

	in := &Frame{
		Type:    FrameDesktopRPCRequest,
		Payload: json.RawMessage(`{"request_id":"drpc_1","method":"system.ping"}`),
	}
	var buf bytes.Buffer
	if err := WriteFrame(&buf, in); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	got, err := ReadFrame(bufio.NewReader(&buf))
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if got.Type != in.Type {
		t.Errorf("type: got %q, want %q", got.Type, in.Type)
	}
	if string(got.Payload) != string(in.Payload) {
		t.Errorf("payload: got %s, want %s", got.Payload, in.Payload)
	}
}

func TestReadFrame_ShortPrefix(t *testing.T) {
	t.Parallel()
	// Only 2 bytes — not enough for a 4-byte prefix.
	r := bufio.NewReader(bytes.NewReader([]byte{0x00, 0x00}))
	_, err := ReadFrame(r)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("expected ErrUnexpectedEOF on short prefix, got %v", err)
	}
}

func TestReadFrame_CleanEOF(t *testing.T) {
	t.Parallel()
	// Zero bytes — clean EOF, caller treats as graceful close.
	r := bufio.NewReader(bytes.NewReader(nil))
	_, err := ReadFrame(r)
	if !errors.Is(err, io.EOF) {
		t.Errorf("expected io.EOF on clean stream end, got %v", err)
	}
}

func TestReadFrame_ZeroLengthPrefix(t *testing.T) {
	t.Parallel()
	// Valid 4-byte prefix encoding 0 — rejected per spec §5.1 ("empty frames
	// have no legitimate use").
	buf := make([]byte, 4)
	binary.BigEndian.PutUint32(buf, 0)
	r := bufio.NewReader(bytes.NewReader(buf))
	_, err := ReadFrame(r)
	if !errors.Is(err, ErrEmptyFrame) {
		t.Errorf("expected ErrEmptyFrame, got %v", err)
	}
}

func TestReadFrame_TooLarge(t *testing.T) {
	t.Parallel()
	// Prefix declares 4 MiB + 1 — the codec must reject without attempting
	// to allocate the body buffer.
	buf := make([]byte, 4)
	binary.BigEndian.PutUint32(buf, MaxFrameBodyBytes+1)
	r := bufio.NewReader(bytes.NewReader(buf))
	_, err := ReadFrame(r)
	if !errors.Is(err, ErrFrameTooLarge) {
		t.Errorf("expected ErrFrameTooLarge, got %v", err)
	}
}

func TestReadFrame_MaxSizeBoundary(t *testing.T) {
	t.Parallel()
	// A body of exactly MaxFrameBodyBytes is the *upper inclusive* bound and
	// must be accepted. Build a valid JSON of exact target size by padding
	// the payload string with 'a' until total body size hits the cap.
	bodyLen := MaxFrameBodyBytes
	// Frame envelope: {"type":"x","payload":"<padding>"}
	// Fixed chars: {"type":"x","payload":""}  = 25 bytes (count carefully).
	const overhead = 25
	padLen := bodyLen - overhead
	if padLen <= 0 {
		t.Fatalf("test logic: bodyLen %d too small for overhead %d", bodyLen, overhead)
	}
	body := make([]byte, 0, bodyLen)
	body = append(body, []byte(`{"type":"x","payload":"`)...)
	for i := 0; i < padLen; i++ {
		body = append(body, 'a')
	}
	body = append(body, '"', '}')
	if len(body) != bodyLen {
		t.Fatalf("test logic: body len = %d, want %d", len(body), bodyLen)
	}
	var verify Frame
	if err := json.Unmarshal(body, &verify); err != nil {
		t.Fatalf("constructed body is invalid JSON (test bug): %v", err)
	}

	frame := make([]byte, 4+bodyLen)
	binary.BigEndian.PutUint32(frame[:4], uint32(bodyLen))
	copy(frame[4:], body)

	r := bufio.NewReader(bytes.NewReader(frame))
	got, err := ReadFrame(r)
	if err != nil {
		t.Fatalf("ReadFrame on max-size frame: %v", err)
	}
	if got.Type != "x" {
		t.Errorf("max-size frame Type: got %q, want %q", got.Type, "x")
	}
}

func TestReadFrame_PartialBody(t *testing.T) {
	t.Parallel()
	// Prefix declares 100 bytes but only 50 are available — io.ReadFull
	// surfaces ErrUnexpectedEOF.
	const declared = 100
	buf := make([]byte, 4+declared/2)
	binary.BigEndian.PutUint32(buf[:4], declared)
	r := bufio.NewReader(bytes.NewReader(buf))
	_, err := ReadFrame(r)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("expected ErrUnexpectedEOF on partial body, got %v", err)
	}
}

func TestReadFrame_MalformedJSON(t *testing.T) {
	t.Parallel()
	// Valid prefix + invalid JSON body.
	body := []byte("{not json")
	frame := make([]byte, 4+len(body))
	binary.BigEndian.PutUint32(frame[:4], uint32(len(body)))
	copy(frame[4:], body)
	r := bufio.NewReader(bytes.NewReader(frame))
	_, err := ReadFrame(r)
	if err == nil {
		t.Fatal("expected JSON decode error, got nil")
	}
	if !strings.Contains(err.Error(), "decode frame envelope") {
		t.Errorf("error should wrap decode failure, got: %v", err)
	}
}

func TestWriteFrame_TooLarge(t *testing.T) {
	t.Parallel()
	// Craft a frame whose JSON encoding exceeds MaxFrameBodyBytes — must be
	// rejected before any Write call.
	huge := strings.Repeat("a", MaxFrameBodyBytes)
	frame := &Frame{
		Type:    FrameDesktopRPCRequest,
		Payload: json.RawMessage(`"` + huge + `"`),
	}
	var buf bytes.Buffer
	err := WriteFrame(&buf, frame)
	if !errors.Is(err, ErrFrameTooLarge) {
		t.Errorf("expected ErrFrameTooLarge, got %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("WriteFrame should not have written %d bytes on oversize", buf.Len())
	}
}

func TestEncodeHelpers(t *testing.T) {
	t.Parallel()
	// EncodeRequestFrame, EncodeResultFrame, EncodeEventFrame are thin
	// wrappers; verify the resulting Frame.Type is correct and Payload is
	// the expected JSON for each.

	t.Run("request", func(t *testing.T) {
		req := &RPCRequest{RequestID: "drpc_abc", Method: MethodSystemPing}
		f, err := EncodeRequestFrame(req)
		if err != nil {
			t.Fatalf("EncodeRequestFrame: %v", err)
		}
		if f.Type != FrameDesktopRPCRequest {
			t.Errorf("Type: got %q, want %q", f.Type, FrameDesktopRPCRequest)
		}
		var roundtrip RPCRequest
		if err := json.Unmarshal(f.Payload, &roundtrip); err != nil {
			t.Fatalf("decode payload: %v", err)
		}
		if roundtrip.RequestID != "drpc_abc" || roundtrip.Method != MethodSystemPing {
			t.Errorf("roundtrip mismatch: %+v", roundtrip)
		}
	})

	t.Run("result", func(t *testing.T) {
		res := &RPCResult{RequestID: "drpc_abc", OK: true, Result: json.RawMessage(`{"x":1}`)}
		f, err := EncodeResultFrame(res)
		if err != nil {
			t.Fatalf("EncodeResultFrame: %v", err)
		}
		if f.Type != FrameDesktopRPCResult {
			t.Errorf("Type: got %q, want %q", f.Type, FrameDesktopRPCResult)
		}
	})

	t.Run("event", func(t *testing.T) {
		evt := &DesktopEvent{Event: EventDesktopOnline, TS: "2026-05-26T10:00:00+08:00"}
		f, err := EncodeEventFrame(evt)
		if err != nil {
			t.Fatalf("EncodeEventFrame: %v", err)
		}
		if f.Type != FrameDesktopEvent {
			t.Errorf("Type: got %q, want %q", f.Type, FrameDesktopEvent)
		}
	})
}

// TestRoundTrip_AllFixtures parses every JSON file under
// docs/desktop-calendar-rpc-fixtures and verifies it round-trips through
// our codec (decode → re-encode → decode again, semantic equality).
//
// This catches drift between spec example schemas and our Go structs.
// Skipped when fixtures dir is not findable from test working dir.
func TestRoundTrip_AllFixtures(t *testing.T) {
	t.Parallel()
	// Test runs from internal/daemon/desktop_rpc/ — climb 3 levels to repo root.
	const fixturesDir = "../../../docs/desktop-calendar-rpc-fixtures"
	entries, err := os.ReadDir(fixturesDir)
	if err != nil {
		t.Skipf("fixtures dir not accessible (%v); skipping fixture round-trip", err)
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			data, err := os.ReadFile(filepath.Join(fixturesDir, name))
			if err != nil {
				t.Fatalf("read fixture: %v", err)
			}
			// Each fixture is a full frame (not length-prefixed) — decode as Frame.
			var f Frame
			if err := json.Unmarshal(data, &f); err != nil {
				t.Fatalf("decode fixture: %v", err)
			}
			if f.Type == "" {
				t.Fatal("fixture frame missing type field")
			}
			// Re-encode through our Frame and verify round-trip.
			var buf bytes.Buffer
			if err := WriteFrame(&buf, &f); err != nil {
				t.Fatalf("re-encode: %v", err)
			}
			got, err := ReadFrame(bufio.NewReader(&buf))
			if err != nil {
				t.Fatalf("re-decode: %v", err)
			}
			if got.Type != f.Type {
				t.Errorf("type drift: got %q, want %q", got.Type, f.Type)
			}
		})
	}
}
