package daemon

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"image/jpeg"
	"image/png"
	"sync"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
)

const (
	maxComputerUsePreviewBytes      = 8 << 20
	maxComputerUsePreviewDimension  = 8192
	maxComputerUsePreviewPixelCount = 32 << 20
)

// computerUsePreviewFrameV1 is process-memory-only presentation state. It is
// deliberately separate from activity events and audit records so screenshots
// never enter either persistent seam.
type computerUsePreviewFrameV1 struct {
	LeaseID   string
	FrameID   string
	Revision  uint64
	MediaType string
	Width     int
	Height    int
	Data      []byte
	Cursor    *computerUsePreviewCursorV1
}

type computerUsePreviewCursorV1 struct {
	X    float64
	Y    float64
	Kind string
}

// ComputerUsePreviewStore holds at most one exact-window frame for the active
// goal-level lease. A new lease replaces the old bytes; terminal activity
// clears them. The store has no filesystem or network writer.
type ComputerUsePreviewStore struct {
	mu       sync.RWMutex
	revision uint64
	frame    *computerUsePreviewFrameV1
}

func NewComputerUsePreviewStore() *ComputerUsePreviewStore {
	return &ComputerUsePreviewStore{}
}

func (s *ComputerUsePreviewStore) Publish(
	leaseID string,
	image agent.ImageBlock,
) error {
	if s == nil {
		return nil
	}
	if leaseID == "" {
		return fmt.Errorf("computer-use preview lease_id is required")
	}
	if image.MediaType != "image/png" && image.MediaType != "image/jpeg" {
		return fmt.Errorf("computer-use preview media type is unsupported")
	}
	if image.Data == "" || len(image.Data) > base64.StdEncoding.EncodedLen(maxComputerUsePreviewBytes) {
		return fmt.Errorf("computer-use preview image is empty or too large")
	}
	data, err := base64.StdEncoding.Strict().DecodeString(image.Data)
	if err != nil || len(data) == 0 || len(data) > maxComputerUsePreviewBytes {
		return fmt.Errorf("computer-use preview image is invalid")
	}
	var width, height int
	switch image.MediaType {
	case "image/png":
		config, decodeErr := png.DecodeConfig(bytes.NewReader(data))
		if decodeErr != nil {
			return fmt.Errorf("computer-use preview PNG is invalid")
		}
		width, height = config.Width, config.Height
	case "image/jpeg":
		config, decodeErr := jpeg.DecodeConfig(bytes.NewReader(data))
		if decodeErr != nil {
			return fmt.Errorf("computer-use preview JPEG is invalid")
		}
		width, height = config.Width, config.Height
	}
	if width <= 0 || height <= 0 ||
		width > maxComputerUsePreviewDimension ||
		height > maxComputerUsePreviewDimension ||
		width > maxComputerUsePreviewPixelCount/height {
		return fmt.Errorf("computer-use preview dimensions are invalid")
	}

	digest := sha256.Sum256(data)
	frameID := fmt.Sprintf("%x", digest[:16])

	s.mu.Lock()
	defer s.mu.Unlock()
	var cursor *computerUsePreviewCursorV1
	if s.frame != nil && s.frame.LeaseID == leaseID && s.frame.Cursor != nil {
		copy := *s.frame.Cursor
		cursor = &copy
	}
	if s.frame != nil {
		clear(s.frame.Data)
	}
	s.revision++
	s.frame = &computerUsePreviewFrameV1{
		LeaseID: leaseID, FrameID: frameID, Revision: s.revision,
		MediaType: image.MediaType, Width: width, Height: height,
		Data: append([]byte(nil), data...), Cursor: cursor,
	}
	return nil
}

func (s *ComputerUsePreviewStore) SetCursor(
	leaseID string,
	x int,
	y int,
	kind string,
) bool {
	if s == nil || leaseID == "" || kind == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.frame == nil || s.frame.LeaseID != leaseID ||
		x < 0 || y < 0 || x >= s.frame.Width || y >= s.frame.Height {
		return false
	}
	denominatorX := max(s.frame.Width-1, 1)
	denominatorY := max(s.frame.Height-1, 1)
	s.revision++
	s.frame.Revision = s.revision
	s.frame.Cursor = &computerUsePreviewCursorV1{
		X:    float64(x) / float64(denominatorX),
		Y:    float64(y) / float64(denominatorY),
		Kind: kind,
	}
	return true
}

func (s *ComputerUsePreviewStore) Snapshot(
	leaseID string,
) (computerUsePreviewFrameV1, bool) {
	if s == nil || leaseID == "" {
		return computerUsePreviewFrameV1{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.frame == nil || s.frame.LeaseID != leaseID {
		return computerUsePreviewFrameV1{}, false
	}
	frame := *s.frame
	frame.Data = append([]byte(nil), s.frame.Data...)
	if s.frame.Cursor != nil {
		cursor := *s.frame.Cursor
		frame.Cursor = &cursor
	}
	return frame, true
}

func (s *ComputerUsePreviewStore) ClearLease(leaseID string) {
	if s == nil || leaseID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.frame == nil || s.frame.LeaseID != leaseID {
		return
	}
	clear(s.frame.Data)
	s.frame = nil
}
