package koe

import "testing"

func TestOpusRoundTrip(t *testing.T) {
	a, err := NewAudioIO()
	if err != nil {
		t.Fatalf("NewAudioIO: %v", err)
	}
	// A 20 ms 48 kHz mono frame of a low-amplitude tone.
	frame := make([]int16, 960)
	for i := range frame {
		frame[i] = int16(1000 * ((i % 48) - 24))
	}
	enc, err := a.EncodeFrame(frame)
	if err != nil {
		t.Fatalf("EncodeFrame: %v", err)
	}
	if len(enc) == 0 {
		t.Fatal("encoded frame is empty")
	}
	dec, err := a.DecodeFrame(enc)
	if err != nil {
		t.Fatalf("DecodeFrame: %v", err)
	}
	if len(dec) != 960 {
		t.Errorf("decoded %d samples, want 960", len(dec))
	}
}

func TestHalfDuplexGateDropsWhileSpeaking(t *testing.T) {
	a, _ := NewAudioIO()
	a.SetSpeaking(true)
	if !a.dropCapture() {
		t.Error("while speaking, capture must be dropped")
	}
	a.SetSpeaking(false)
	if a.dropCapture() {
		t.Error("when not speaking, capture must flow")
	}
}
