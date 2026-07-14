// Package audiobridge is the wire codec for the Koe ↔ Reachy audio-carrier UDS
// link (Wireless / W-中, direction A). It frames raw PCM between the Go Koe side
// and the Python audio carrier (which bridges the daemon's WebRTC audio). Pure Go,
// no cgo, no device coupling — the koe linux audio backend and the carrier both
// speak this format. Contract: ~/Desktop/koe-audio-carrier-spec.md §3/§4.
package audiobridge

import (
	"encoding/binary"
	"fmt"
	"io"
)

// HeaderSize is the fixed little-endian frame header width in bytes (§3).
const HeaderSize = 16

// Frame magic bytes — which leg / kind a frame carries (§3).
const (
	MagicMic     uint8 = 0xA0 // daemon mic → koe (downlink)
	MagicSpk     uint8 = 0xA1 // koe → daemon speaker (uplink)
	MagicControl uint8 = 0xC0 // control frame (§4)
)

// Sample formats (§3).
const (
	FormatF32LE uint8 = 0
	FormatS16LE uint8 = 1
)

// Header is the 16-byte little-endian PCM frame header (§3).
type Header struct {
	Magic      uint8
	Format     uint8
	Channels   uint16
	SampleRate uint32
	NSamples   uint32 // per-channel sample count in this frame's payload
	Seq        uint32 // monotonic, for drop detection (no retransmit)
}

// bytesPerSample returns the size of one sample for the header's format.
func (h Header) bytesPerSample() int {
	if h.Format == FormatS16LE {
		return 2
	}
	return 4 // FormatF32LE (and any unknown format defaults to the F32 width)
}

// PayloadLen is the exact payload byte count this header describes.
func (h Header) PayloadLen() int {
	return int(h.NSamples) * int(h.Channels) * h.bytesPerSample()
}

// MarshalBinary encodes the header into HeaderSize little-endian bytes.
func (h Header) MarshalBinary() []byte {
	b := make([]byte, HeaderSize)
	b[0] = h.Magic
	b[1] = h.Format
	binary.LittleEndian.PutUint16(b[2:], h.Channels)
	binary.LittleEndian.PutUint32(b[4:], h.SampleRate)
	binary.LittleEndian.PutUint32(b[8:], h.NSamples)
	binary.LittleEndian.PutUint32(b[12:], h.Seq)
	return b
}

// UnmarshalHeader decodes a HeaderSize-byte header. Fails loud on a short buffer.
func UnmarshalHeader(b []byte) (Header, error) {
	if len(b) < HeaderSize {
		return Header{}, fmt.Errorf("audiobridge: header needs %d bytes, got %d", HeaderSize, len(b))
	}
	return Header{
		Magic:      b[0],
		Format:     b[1],
		Channels:   binary.LittleEndian.Uint16(b[2:]),
		SampleRate: binary.LittleEndian.Uint32(b[4:]),
		NSamples:   binary.LittleEndian.Uint32(b[8:]),
		Seq:        binary.LittleEndian.Uint32(b[12:]),
	}, nil
}

// WriteFrame writes header + payload as one length-consistent frame. It refuses a
// payload whose length disagrees with the header (fail-loud, not silent truncation).
func WriteFrame(w io.Writer, h Header, payload []byte) error {
	if len(payload) != h.PayloadLen() {
		return fmt.Errorf("audiobridge: payload %d bytes, header describes %d", len(payload), h.PayloadLen())
	}
	if _, err := w.Write(h.MarshalBinary()); err != nil {
		return err
	}
	_, err := w.Write(payload)
	return err
}

// ReadFrame reads one framed header + payload. A truncated payload fails loud
// (io.ErrUnexpectedEOF), never a short read passed off as a complete frame.
func ReadFrame(r io.Reader) (Header, []byte, error) {
	var hb [HeaderSize]byte
	if _, err := io.ReadFull(r, hb[:]); err != nil {
		return Header{}, nil, err // io.EOF on a clean frame boundary
	}
	h, err := UnmarshalHeader(hb[:])
	if err != nil {
		return Header{}, nil, err
	}
	payload := make([]byte, h.PayloadLen())
	if _, err := io.ReadFull(r, payload); err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return Header{}, nil, fmt.Errorf("audiobridge: read payload: %w", err)
	}
	return h, payload, nil
}
