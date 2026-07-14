package audiobridge

import (
	"bytes"
	"encoding/binary"
	"io"
	"testing"
)

// Canonical 16-byte header wire layout (little-endian), per koe-audio-carrier-spec
// §3: magic(1) format(1) channels(2) sample_rate(4) n_samples(4) seq(4). This test
// pins the byte layout so the Go koe side and the Python carrier side stay in sync.
func TestHeaderMarshalCanonicalBytes(t *testing.T) {
	h := Header{
		Magic:      MagicMic,
		Format:     FormatF32LE,
		Channels:   2,
		SampleRate: 16000,
		NSamples:   320, // 20 ms @ 16k
		Seq:        1,
	}
	got := h.MarshalBinary()
	if len(got) != HeaderSize {
		t.Fatalf("header len = %d, want %d", len(got), HeaderSize)
	}
	want := make([]byte, HeaderSize)
	want[0] = MagicMic
	want[1] = FormatF32LE
	binary.LittleEndian.PutUint16(want[2:], 2)
	binary.LittleEndian.PutUint32(want[4:], 16000)
	binary.LittleEndian.PutUint32(want[8:], 320)
	binary.LittleEndian.PutUint32(want[12:], 1)
	if !bytes.Equal(got, want) {
		t.Errorf("header bytes = % x, want % x", got, want)
	}
}

func TestHeaderRoundtrip(t *testing.T) {
	h := Header{Magic: MagicSpk, Format: FormatS16LE, Channels: 1, SampleRate: 48000, NSamples: 960, Seq: 42}
	dec, err := UnmarshalHeader(h.MarshalBinary())
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if dec != h {
		t.Errorf("roundtrip = %+v, want %+v", dec, h)
	}
}

func TestUnmarshalShortBufferFailsLoud(t *testing.T) {
	if _, err := UnmarshalHeader(make([]byte, HeaderSize-1)); err == nil {
		t.Fatal("short header must fail, got nil err")
	}
}

func TestPayloadLen(t *testing.T) {
	// F32LE 2ch 320 samples = 320*2*4 = 2560 bytes; S16LE 1ch 960 = 960*1*2 = 1920.
	if n := (Header{Format: FormatF32LE, Channels: 2, NSamples: 320}).PayloadLen(); n != 2560 {
		t.Errorf("F32/2ch/320 PayloadLen = %d, want 2560", n)
	}
	if n := (Header{Format: FormatS16LE, Channels: 1, NSamples: 960}).PayloadLen(); n != 1920 {
		t.Errorf("S16/1ch/960 PayloadLen = %d, want 1920", n)
	}
}

func TestWriteReadFrameRoundtrip(t *testing.T) {
	h := Header{Magic: MagicMic, Format: FormatS16LE, Channels: 1, SampleRate: 48000, NSamples: 4, Seq: 7}
	payload := []byte{1, 0, 2, 0, 3, 0, 4, 0} // 4 samples S16LE
	var buf bytes.Buffer
	if err := WriteFrame(&buf, h, payload); err != nil {
		t.Fatalf("write: %v", err)
	}
	if buf.Len() != HeaderSize+len(payload) {
		t.Fatalf("framed len = %d, want %d", buf.Len(), HeaderSize+len(payload))
	}
	gotH, gotP, err := ReadFrame(&buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if gotH != h {
		t.Errorf("header = %+v, want %+v", gotH, h)
	}
	if !bytes.Equal(gotP, payload) {
		t.Errorf("payload = % x, want % x", gotP, payload)
	}
}

// A frame whose header claims a payload longer than the writer supplies must be
// rejected, not silently truncated (fail-loud, like the daemon UDS discipline).
func TestWriteFramePayloadLenMismatchFailsLoud(t *testing.T) {
	h := Header{Magic: MagicMic, Format: FormatS16LE, Channels: 1, NSamples: 4} // wants 8 bytes
	if err := WriteFrame(&bytes.Buffer{}, h, []byte{1, 2}); err == nil {
		t.Fatal("payload length mismatch must fail, got nil err")
	}
}

func TestReadFrameTruncatedPayloadFailsLoud(t *testing.T) {
	h := Header{Magic: MagicMic, Format: FormatS16LE, Channels: 1, NSamples: 4} // wants 8 bytes
	buf := bytes.NewBuffer(h.MarshalBinary())
	buf.Write([]byte{1, 0, 2, 0}) // only 4 of 8 payload bytes
	if _, _, err := ReadFrame(buf); err == nil || err == io.EOF {
		t.Fatalf("truncated payload must fail loud, got %v", err)
	}
}
