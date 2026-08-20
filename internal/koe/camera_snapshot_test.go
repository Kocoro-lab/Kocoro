package koe

import (
	"context"
	"encoding/binary"
	"net"
	"os"
	"path/filepath"
	"testing"
)

func runCameraServer(t *testing.T, handler func(net.Conn, cameraSnapshotRequest)) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "kc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "camera.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		var req cameraSnapshotRequest
		if readCameraJSON(conn, &req) == nil {
			handler(conn, req)
		}
	}()
	return path
}

func writeCameraResponse(t *testing.T, conn net.Conn, response cameraSnapshotResponse, jpeg []byte) {
	t.Helper()
	if err := writeCameraJSON(conn, response); err != nil {
		t.Error(err)
		return
	}
	var size [4]byte
	binary.BigEndian.PutUint32(size[:], uint32(len(jpeg)))
	_, _ = conn.Write(size[:])
	_, _ = conn.Write(jpeg)
}

func TestCaptureCameraSnapshotCanonicalRoundTrip(t *testing.T) {
	want := []byte{0xff, 0xd8, 1, 2, 3, 0xff, 0xd9}
	path := runCameraServer(t, func(conn net.Conn, req cameraSnapshotRequest) {
		if req.Type != "camera.snapshot" || req.Proto != cameraSnapshotProto || req.RequestID == "" {
			t.Errorf("request = %#v", req)
		}
		writeCameraResponse(t, conn, cameraSnapshotResponse{
			Type: "camera.snapshot.result", Proto: cameraSnapshotProto,
			RequestID: req.RequestID, OK: true, MediaType: "image/jpeg", CapturedAt: "2026-07-16T00:00:00Z",
		}, want)
	})
	got, err := CaptureCameraSnapshot(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got.JPEG) != string(want) || got.CapturedAt == "" {
		t.Fatalf("snapshot = %#v", got)
	}
	if got.DataURL() != "data:image/jpeg;base64,/9gBAgP/2Q==" {
		t.Fatalf("data URL = %q", got.DataURL())
	}
}

func TestProbeCameraCarrierDoesNotRequestJPEG(t *testing.T) {
	path := runCameraServer(t, func(conn net.Conn, req cameraSnapshotRequest) {
		if req.Type != "camera.probe" || req.Proto != cameraVideoProto {
			t.Errorf("probe request = %#v", req)
		}
		writeCameraResponse(t, conn, cameraSnapshotResponse{
			Type: "camera.probe.result", Proto: cameraVideoProto, RequestID: req.RequestID, OK: true,
		}, nil)
	})
	if err := ProbeCameraCarrier(context.Background(), path); err != nil {
		t.Fatal(err)
	}
}

func TestCaptureCameraSnapshotStableRemoteFailure(t *testing.T) {
	path := runCameraServer(t, func(conn net.Conn, req cameraSnapshotRequest) {
		response := cameraSnapshotResponse{Type: "camera.snapshot.result", Proto: cameraSnapshotProto, RequestID: req.RequestID}
		response.Error = &struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}{Code: "frame_unavailable", Message: "current camera frame is unavailable"}
		writeCameraResponse(t, conn, response, nil)
	})
	if _, err := CaptureCameraSnapshot(context.Background(), path); err == nil || err.Error() != "koe[camera]: frame_unavailable" {
		t.Fatalf("error = %v", err)
	}
}

func TestCaptureCameraSnapshotRejectsBadJPEG(t *testing.T) {
	path := runCameraServer(t, func(conn net.Conn, req cameraSnapshotRequest) {
		writeCameraResponse(t, conn, cameraSnapshotResponse{
			Type: "camera.snapshot.result", Proto: cameraSnapshotProto, RequestID: req.RequestID, OK: true, MediaType: "image/jpeg",
		}, []byte("not-a-jpeg"))
	})
	if _, err := CaptureCameraSnapshot(context.Background(), path); err == nil {
		t.Fatal("bad JPEG must fail")
	}
}

func TestCaptureCameraVideoFrameCanonicalRoundTrip(t *testing.T) {
	want := []byte{0, 0, 0, 1, 0x67, 1, 2, 3, 0, 0, 0, 1, 0x65, 4, 5, 6}
	path := runCameraServer(t, func(conn net.Conn, req cameraSnapshotRequest) {
		if req.Type != "camera.video_frame" || req.Proto != cameraVideoProto || req.RequestID == "" {
			t.Errorf("request = %#v", req)
		}
		writeCameraResponse(t, conn, cameraSnapshotResponse{
			Type: "camera.video_frame.result", Proto: cameraVideoProto,
			RequestID: req.RequestID, OK: true, MediaType: "video/h264", CapturedAt: "2026-08-20T00:00:00Z",
		}, want)
	})
	got, err := CaptureCameraVideoFrame(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("video frame = %x, want %x", got, want)
	}
}

func TestCaptureCameraVideoFrameRejectsWrongMediaAndPayload(t *testing.T) {
	for _, test := range []struct {
		name      string
		mediaType string
		payload   []byte
	}{
		{name: "wrong media", mediaType: "image/jpeg", payload: []byte{0, 0, 0, 1, 0x65}},
		{name: "not annex b", mediaType: "video/h264", payload: []byte("not-h264")},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := runCameraServer(t, func(conn net.Conn, req cameraSnapshotRequest) {
				writeCameraResponse(t, conn, cameraSnapshotResponse{
					Type: "camera.video_frame.result", Proto: cameraVideoProto,
					RequestID: req.RequestID, OK: true, MediaType: test.mediaType,
				}, test.payload)
			})
			if _, err := CaptureCameraVideoFrame(context.Background(), path); err == nil {
				t.Fatal("invalid video frame must fail")
			}
		})
	}
}
