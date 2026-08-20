package koe

// Wireless camera client. The paired Python server lives inside the existing
// audio carrier and therefore reuses its one MediaManager(Local) consumer; this
// client never opens camera hardware or a second media session.
// Contract: ~/Desktop/koe-camera-snapshot-uds-spec.md v0.2.

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
	cameraVideoProto          = "0.2"
	maxCameraMetadataBytes    = 64 * 1024
	maxCameraSnapshotJPEGSize = 3 * 1024 * 1024
	maxCameraH264FrameSize    = 2 * 1024 * 1024
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
	response, jpeg, err := requestCameraPayload(ctx, socketPath, "camera.snapshot", cameraSnapshotProto)
	if err != nil {
		return CameraSnapshot{}, err
	}
	if response.MediaType != "image/jpeg" {
		return CameraSnapshot{}, fmt.Errorf("koe[camera]: unsupported media type %q", response.MediaType)
	}
	if len(jpeg) < 4 || len(jpeg) > maxCameraSnapshotJPEGSize {
		return CameraSnapshot{}, fmt.Errorf("koe[camera]: JPEG size %d is invalid", len(jpeg))
	}
	if jpeg[0] != 0xff || jpeg[1] != 0xd8 || jpeg[len(jpeg)-2] != 0xff || jpeg[len(jpeg)-1] != 0xd9 {
		return CameraSnapshot{}, fmt.Errorf("koe[camera]: response is not a complete JPEG")
	}
	return CameraSnapshot{JPEG: jpeg, MediaType: response.MediaType, CapturedAt: response.CapturedAt}, nil
}

// CaptureCameraVideoFrame returns one independently decodable constrained-
// baseline H264 access unit for Qwen's provider-native WebRTC video track.
func CaptureCameraVideoFrame(ctx context.Context, socketPath string) ([]byte, error) {
	response, frame, err := requestCameraPayload(ctx, socketPath, "camera.video_frame", cameraVideoProto)
	if err != nil {
		return nil, err
	}
	if response.MediaType != "video/h264" {
		return nil, fmt.Errorf("koe[camera]: unsupported media type %q", response.MediaType)
	}
	if len(frame) < 5 || len(frame) > maxCameraH264FrameSize {
		return nil, fmt.Errorf("koe[camera]: H264 frame size %d is invalid", len(frame))
	}
	if !hasAnnexBStartCode(frame) {
		return nil, fmt.Errorf("koe[camera]: response is not Annex-B H264")
	}
	return frame, nil
}

// ProbeCameraCarrier validates the private socket and protocol without asking the
// carrier to read or encode a camera frame. It preserves the idle privacy and
// zero-session invariants while giving the startup checklist first-hand evidence.
func ProbeCameraCarrier(ctx context.Context, socketPath string) error {
	_, _, err := requestCameraPayload(ctx, socketPath, "camera.probe", cameraVideoProto)
	return err
}

func requestCameraPayload(ctx context.Context, socketPath, requestType, proto string) (cameraSnapshotResponse, []byte, error) {
	if socketPath == "" {
		return cameraSnapshotResponse{}, nil, fmt.Errorf("koe[camera]: no camera socket (--camera-socket)")
	}
	requestID, err := newCameraRequestID()
	if err != nil {
		return cameraSnapshotResponse{}, nil, fmt.Errorf("koe[camera]: request id: %w", err)
	}
	request := cameraSnapshotRequest{
		Type: requestType, Proto: proto, RequestID: requestID,
	}
	dialCtx, cancel := context.WithTimeout(ctx, cameraSnapshotTimeout)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(dialCtx, "unix", socketPath)
	if err != nil {
		return cameraSnapshotResponse{}, nil, fmt.Errorf("koe[camera]: dial camera carrier: %w", err)
	}
	defer conn.Close()
	deadline := time.Now().Add(cameraSnapshotTimeout)
	_ = conn.SetDeadline(deadline)
	if err := writeCameraJSON(conn, request); err != nil {
		return cameraSnapshotResponse{}, nil, fmt.Errorf("koe[camera]: send camera request: %w", err)
	}
	var response cameraSnapshotResponse
	if err := readCameraJSON(conn, &response); err != nil {
		return cameraSnapshotResponse{}, nil, fmt.Errorf("koe[camera]: read camera response: %w", err)
	}
	if response.Type != requestType+".result" || response.Proto != proto || response.RequestID != requestID {
		return cameraSnapshotResponse{}, nil, fmt.Errorf("koe[camera]: invalid camera response identity")
	}
	var size [4]byte
	if _, err := io.ReadFull(conn, size[:]); err != nil {
		return cameraSnapshotResponse{}, nil, fmt.Errorf("koe[camera]: read payload length: %w", err)
	}
	payloadLen := binary.BigEndian.Uint32(size[:])
	if !response.OK {
		if payloadLen != 0 {
			return cameraSnapshotResponse{}, nil, fmt.Errorf("koe[camera]: failed response carried media bytes")
		}
		if response.Error == nil || response.Error.Code == "" {
			return cameraSnapshotResponse{}, nil, fmt.Errorf("koe[camera]: request failed")
		}
		return cameraSnapshotResponse{}, nil, fmt.Errorf("koe[camera]: %s", response.Error.Code)
	}
	if requestType == "camera.probe" {
		if payloadLen != 0 {
			return cameraSnapshotResponse{}, nil, fmt.Errorf("koe[camera]: probe returned media bytes")
		}
		return response, nil, nil
	}
	if payloadLen == 0 || payloadLen > maxCameraSnapshotJPEGSize {
		return cameraSnapshotResponse{}, nil, fmt.Errorf("koe[camera]: payload size %d is invalid", payloadLen)
	}
	payload := make([]byte, payloadLen)
	if _, err := io.ReadFull(conn, payload); err != nil {
		return cameraSnapshotResponse{}, nil, fmt.Errorf("koe[camera]: read media payload: %w", err)
	}
	return response, payload, nil
}

func hasAnnexBStartCode(frame []byte) bool {
	return len(frame) >= 4 && (string(frame[:4]) == "\x00\x00\x00\x01" || string(frame[:3]) == "\x00\x00\x01")
}
