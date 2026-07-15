package koe

// On-demand Wireless camera client. The paired Python server lives inside the
// existing audio carrier and therefore reuses its one MediaManager(WEBRTC)
// consumer; this client never opens camera hardware or a second media session.
// Contract: ~/Desktop/koe-camera-snapshot-uds-spec.md v0.1.

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"time"
)

const (
	cameraSnapshotProto       = "0.1"
	maxCameraMetadataBytes    = 64 * 1024
	maxCameraSnapshotJPEGSize = 3 * 1024 * 1024
	cameraSnapshotTimeout     = 3 * time.Second
)

// CameraSnapshot is a transient, in-memory image. Callers must not persist it or
// log DataURL; the Realtime image conversation item is its only product sink.
type CameraSnapshot struct {
	JPEG       []byte
	MediaType  string
	CapturedAt string
}

func (s CameraSnapshot) DataURL() string {
	return "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(s.JPEG)
}

type cameraSnapshotRequest struct {
	Type      string `json:"type"`
	Proto     string `json:"proto"`
	RequestID string `json:"request_id"`
}

type cameraSnapshotResponse struct {
	Type       string `json:"type"`
	Proto      string `json:"proto"`
	RequestID  string `json:"request_id"`
	OK         bool   `json:"ok"`
	MediaType  string `json:"media_type,omitempty"`
	CapturedAt string `json:"captured_at,omitempty"`
	Error      *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

func newCameraRequestID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}

func writeCameraJSON(w io.Writer, value any) error {
	body, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(body) == 0 || len(body) > maxCameraMetadataBytes {
		return fmt.Errorf("koe[camera]: request metadata size %d is invalid", len(body))
	}
	var size [4]byte
	binary.BigEndian.PutUint32(size[:], uint32(len(body)))
	if _, err := w.Write(size[:]); err != nil {
		return err
	}
	_, err = w.Write(body)
	return err
}

func readCameraJSON(r io.Reader, value any) error {
	var size [4]byte
	if _, err := io.ReadFull(r, size[:]); err != nil {
		return err
	}
	n := binary.BigEndian.Uint32(size[:])
	if n == 0 || n > maxCameraMetadataBytes {
		return fmt.Errorf("koe[camera]: response metadata size %d is invalid", n)
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return err
	}
	if err := json.Unmarshal(body, value); err != nil {
		return fmt.Errorf("koe[camera]: invalid response JSON: %w", err)
	}
	return nil
}

// CaptureCameraSnapshot performs one private UDS request and returns the current
// JPEG. It is side-effect-free outside the active Realtime conversation.
func CaptureCameraSnapshot(ctx context.Context, socketPath string) (CameraSnapshot, error) {
	return requestCameraSnapshot(ctx, socketPath, "camera.snapshot")
}

// ProbeCameraCarrier validates the private socket and protocol without asking the
// carrier to read or encode a camera frame. It preserves the idle privacy and
// zero-session invariants while giving the startup checklist first-hand evidence.
func ProbeCameraCarrier(ctx context.Context, socketPath string) error {
	_, err := requestCameraSnapshot(ctx, socketPath, "camera.probe")
	return err
}

func requestCameraSnapshot(ctx context.Context, socketPath, requestType string) (CameraSnapshot, error) {
	if socketPath == "" {
		return CameraSnapshot{}, fmt.Errorf("koe[camera]: no camera socket (--camera-socket)")
	}
	requestID, err := newCameraRequestID()
	if err != nil {
		return CameraSnapshot{}, fmt.Errorf("koe[camera]: request id: %w", err)
	}
	request := cameraSnapshotRequest{
		Type: requestType, Proto: cameraSnapshotProto, RequestID: requestID,
	}
	dialCtx, cancel := context.WithTimeout(ctx, cameraSnapshotTimeout)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(dialCtx, "unix", socketPath)
	if err != nil {
		return CameraSnapshot{}, fmt.Errorf("koe[camera]: dial snapshot carrier: %w", err)
	}
	defer conn.Close()
	deadline := time.Now().Add(cameraSnapshotTimeout)
	_ = conn.SetDeadline(deadline)
	if err := writeCameraJSON(conn, request); err != nil {
		return CameraSnapshot{}, fmt.Errorf("koe[camera]: send snapshot request: %w", err)
	}
	var response cameraSnapshotResponse
	if err := readCameraJSON(conn, &response); err != nil {
		return CameraSnapshot{}, fmt.Errorf("koe[camera]: read snapshot response: %w", err)
	}
	if response.Type != requestType+".result" || response.Proto != cameraSnapshotProto || response.RequestID != requestID {
		return CameraSnapshot{}, fmt.Errorf("koe[camera]: invalid snapshot response identity")
	}
	var size [4]byte
	if _, err := io.ReadFull(conn, size[:]); err != nil {
		return CameraSnapshot{}, fmt.Errorf("koe[camera]: read JPEG length: %w", err)
	}
	jpegLen := binary.BigEndian.Uint32(size[:])
	if !response.OK {
		if jpegLen != 0 {
			return CameraSnapshot{}, fmt.Errorf("koe[camera]: failed response carried image bytes")
		}
		if response.Error == nil || response.Error.Code == "" {
			return CameraSnapshot{}, fmt.Errorf("koe[camera]: snapshot failed")
		}
		return CameraSnapshot{}, fmt.Errorf("koe[camera]: %s", response.Error.Code)
	}
	if requestType == "camera.probe" {
		if jpegLen != 0 {
			return CameraSnapshot{}, fmt.Errorf("koe[camera]: probe returned image bytes")
		}
		return CameraSnapshot{}, nil
	}
	if response.MediaType != "image/jpeg" {
		return CameraSnapshot{}, fmt.Errorf("koe[camera]: unsupported media type %q", response.MediaType)
	}
	if jpegLen < 4 || jpegLen > maxCameraSnapshotJPEGSize {
		return CameraSnapshot{}, fmt.Errorf("koe[camera]: JPEG size %d is invalid", jpegLen)
	}
	jpeg := make([]byte, jpegLen)
	if _, err := io.ReadFull(conn, jpeg); err != nil {
		return CameraSnapshot{}, fmt.Errorf("koe[camera]: read JPEG: %w", err)
	}
	if jpeg[0] != 0xff || jpeg[1] != 0xd8 || jpeg[len(jpeg)-2] != 0xff || jpeg[len(jpeg)-1] != 0xd9 {
		return CameraSnapshot{}, fmt.Errorf("koe[camera]: response is not a complete JPEG")
	}
	return CameraSnapshot{JPEG: jpeg, MediaType: response.MediaType, CapturedAt: response.CapturedAt}, nil
}
