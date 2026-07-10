package reachy

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"testing"
)

func TestWriteThenReadFrameRoundtrips(t *testing.T) {
	in := &Frame{Type: FrameRPCRequest, Payload: json.RawMessage(`{"request_id":"r-1","method":"system.hello"}`)}
	var buf bytes.Buffer
	if err := WriteFrame(&buf, in); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	// 4-byte BE length prefix precedes the JSON body.
	body, _ := json.Marshal(in)
	if got := binary.BigEndian.Uint32(buf.Bytes()[:4]); int(got) != len(body) {
		t.Errorf("length prefix = %d, want %d", got, len(body))
	}
	out, err := ReadFrame(bufio.NewReader(&buf))
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if out.Type != in.Type || string(out.Payload) != string(in.Payload) {
		t.Errorf("roundtrip mismatch: got %+v", out)
	}
}

func TestReadFrameRejectsZeroLength(t *testing.T) {
	var buf bytes.Buffer
	_ = binary.Write(&buf, binary.BigEndian, uint32(0))
	if _, err := ReadFrame(bufio.NewReader(&buf)); !errors.Is(err, ErrEmptyFrame) {
		t.Errorf("want ErrEmptyFrame, got %v", err)
	}
}

func TestReadFrameRejectsOversizeLength(t *testing.T) {
	var buf bytes.Buffer
	_ = binary.Write(&buf, binary.BigEndian, uint32(MaxFrameBodyBytes+1))
	if _, err := ReadFrame(bufio.NewReader(&buf)); !errors.Is(err, ErrFrameTooLarge) {
		t.Errorf("want ErrFrameTooLarge, got %v", err)
	}
}

func TestWriteFrameRejectsOversizeBody(t *testing.T) {
	big := make([]byte, MaxFrameBodyBytes+1)
	for i := range big {
		big[i] = 'a'
	}
	f := &Frame{Type: "x", Payload: json.RawMessage(`"` + string(big) + `"`)}
	if err := WriteFrame(io.Discard, f); !errors.Is(err, ErrFrameTooLarge) {
		t.Errorf("want ErrFrameTooLarge, got %v", err)
	}
}

func TestReadFrameCleanEOFBetweenFrames(t *testing.T) {
	// No bytes at all = peer closed cleanly between frames.
	if _, err := ReadFrame(bufio.NewReader(bytes.NewReader(nil))); !errors.Is(err, io.EOF) {
		t.Errorf("want io.EOF, got %v", err)
	}
}
