// Package reachy is Koe's client for the reachy motion bridge: it dials the
// bridge's Unix domain socket, does the system.hello handshake, calls motion RPCs,
// feeds the speech/face offset streams, and degrades silently when the bridge is
// unreachable (the conversation never blocks on motion). Contract:
// ~/Desktop/koe-motion-bridge-uds-spec.md (UDS RPC v1.0).
//
// The wire framing mirrors internal/daemon/desktop_rpc but is reimplemented here
// standalone: internal/koe must never import internal/daemon.
package reachy

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// MaxFrameBodyBytes caps a single frame body (mirrors desktop_rpc). A larger
// declared length means a desynced stream — close, don't resync.
const MaxFrameBodyBytes = 4 * 1024 * 1024

// ErrFrameTooLarge / ErrEmptyFrame signal an unrecoverable framing error; the
// caller must close the connection.
var (
	ErrFrameTooLarge = errors.New("reachy: frame body exceeds 4 MiB cap")
	ErrEmptyFrame    = errors.New("reachy: zero-length frame")
)

// Frame is the outer envelope: a type discriminator plus a raw JSON payload.
type Frame struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

// ReadFrame reads one length-prefixed JSON frame. Returns io.EOF on a clean
// close between frames, io.ErrUnexpectedEOF on a truncated frame, ErrEmptyFrame
// / ErrFrameTooLarge on a bad length prefix. Close the conn on any error but EOF.
func ReadFrame(r *bufio.Reader) (*Frame, error) {
	var lenBuf [4]byte
	if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
		return nil, err // io.EOF if clean between frames; io.ErrUnexpectedEOF if partial
	}
	bodyLen := binary.BigEndian.Uint32(lenBuf[:])
	if bodyLen == 0 {
		return nil, ErrEmptyFrame
	}
	if bodyLen > MaxFrameBodyBytes {
		return nil, fmt.Errorf("%w: got %d bytes", ErrFrameTooLarge, bodyLen)
	}
	body := make([]byte, bodyLen)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, fmt.Errorf("read frame body: %w", err)
	}
	var f Frame
	if err := json.Unmarshal(body, &f); err != nil {
		return nil, fmt.Errorf("decode frame envelope: %w", err)
	}
	return &f, nil
}

// WriteFrame marshals f and writes it as one length-prefixed frame (header + body
// in a single Write so concurrent writers can't interleave a frame's bytes).
func WriteFrame(w io.Writer, f *Frame) error {
	body, err := json.Marshal(f)
	if err != nil {
		return fmt.Errorf("encode frame envelope: %w", err)
	}
	if len(body) > MaxFrameBodyBytes {
		return fmt.Errorf("%w: would write %d bytes", ErrFrameTooLarge, len(body))
	}
	out := make([]byte, 4+len(body))
	binary.BigEndian.PutUint32(out[:4], uint32(len(body)))
	copy(out[4:], body)
	if _, err := w.Write(out); err != nil {
		return fmt.Errorf("write frame: %w", err)
	}
	return nil
}
