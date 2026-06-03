package desktop_rpc

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// ErrFrameTooLarge is returned by ReadFrame when the length prefix declares
// a body size exceeding MaxFrameBodyBytes. Callers must close the connection
// on this error — the remote is either misbehaving or speaking a different
// protocol, and resync is not possible.
var ErrFrameTooLarge = errors.New("desktop_rpc: frame body exceeds 4 MiB cap")

// ErrEmptyFrame is returned when the length prefix is zero. Empty frames have
// no legitimate use in v1 and indicate a desynced stream.
var ErrEmptyFrame = errors.New("desktop_rpc: zero-length frame")

// ReadFrame reads one length-prefixed JSON frame from r and decodes the
// outer envelope. The 4-byte big-endian uint32 length prefix is consumed
// first; the body is then read in full and json-decoded into a Frame.
//
// Returns:
//   - io.EOF if the prefix read hits clean EOF (peer closed sock between frames)
//   - io.ErrUnexpectedEOF if EOF arrives mid-frame (during prefix or body)
//   - ErrFrameTooLarge if the prefix exceeds MaxFrameBodyBytes
//   - ErrEmptyFrame if the prefix is zero
//   - JSON decode error if the body is not valid JSON
//
// Callers should close the underlying conn on any error except io.EOF.
func ReadFrame(r *bufio.Reader) (*Frame, error) {
	var lenBuf [4]byte
	if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
		// io.ErrUnexpectedEOF if partial; io.EOF if zero bytes — caller's choice.
		return nil, err
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
		// ReadFull turns EOF into ErrUnexpectedEOF after partial read — good for us.
		return nil, fmt.Errorf("read frame body: %w", err)
	}
	var f Frame
	if err := json.Unmarshal(body, &f); err != nil {
		return nil, fmt.Errorf("decode frame envelope: %w", err)
	}
	return &f, nil
}

// WriteFrame marshals f to JSON and writes it as a single length-prefixed
// frame. The body must fit in MaxFrameBodyBytes; oversized payloads are
// rejected without writing anything (caller should reject the source data
// earlier, e.g. by clamping list result sizes per spec §5.2 limit semantics).
//
// The caller is responsible for any external synchronization needed to keep
// frames atomic on a shared writer — WriteFrame issues a single Write per
// frame (header + body in one buffer) so concurrent calls cannot interleave
// at the byte level, but interleaving order of different frames on the same
// conn is the caller's concern.
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

// EncodeRequestFrame wraps payload into the standard frame envelope with
// Type = FrameDesktopRPCRequest. It's a convenience helper for the broker's
// send path so callers don't repeat the json.Marshal + Frame literal.
func EncodeRequestFrame(req *RPCRequest) (*Frame, error) {
	payload, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("encode RPCRequest payload: %w", err)
	}
	return &Frame{Type: FrameDesktopRPCRequest, Payload: payload}, nil
}

// EncodeResultFrame wraps payload into a result frame.
func EncodeResultFrame(res *RPCResult) (*Frame, error) {
	payload, err := json.Marshal(res)
	if err != nil {
		return nil, fmt.Errorf("encode RPCResult payload: %w", err)
	}
	return &Frame{Type: FrameDesktopRPCResult, Payload: payload}, nil
}

// EncodeEventFrame wraps payload into an event frame.
func EncodeEventFrame(evt *DesktopEvent) (*Frame, error) {
	payload, err := json.Marshal(evt)
	if err != nil {
		return nil, fmt.Errorf("encode DesktopEvent payload: %w", err)
	}
	return &Frame{Type: FrameDesktopEvent, Payload: payload}, nil
}
