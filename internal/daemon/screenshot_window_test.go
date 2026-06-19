package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestScreenshotWindow_RejectsEmptyBody(t *testing.T) {
	s := &Server{}
	req := httptest.NewRequest(http.MethodPost, "/local/screenshot/window", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	s.handleScreenshotWindow(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestScreenshotWindow_MapsDeniedTo403(t *testing.T) {
	orig := captureWindowVia
	defer func() { captureWindowVia = orig }()
	captureWindowVia = func(ctx context.Context, params map[string]any) (json.RawMessage, error) {
		return json.RawMessage(`{"ok":false,"code":"screen_recording_denied"}`), nil
	}
	s := &Server{}
	req := httptest.NewRequest(http.MethodPost, "/local/screenshot/window", strings.NewReader(`{"pid":1234}`))
	rec := httptest.NewRecorder()
	s.handleScreenshotWindow(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	var body struct{ Code string `json:"code"` }
	json.Unmarshal(rec.Body.Bytes(), &body)
	if body.Code != "screen_recording_denied" {
		t.Fatalf("code = %q, want screen_recording_denied", body.Code)
	}
}

func TestScreenshotWindow_SuccessReturnsImage(t *testing.T) {
	orig := captureWindowVia
	defer func() { captureWindowVia = orig }()
	captureWindowVia = func(ctx context.Context, params map[string]any) (json.RawMessage, error) {
		return json.RawMessage(`{"ok":true,"image_base64":"AAAA","width":100,"height":50}`), nil
	}
	s := &Server{}
	req := httptest.NewRequest(http.MethodPost, "/local/screenshot/window", strings.NewReader(`{"pid":1234}`))
	rec := httptest.NewRecorder()
	s.handleScreenshotWindow(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body struct {
		ImageBase64 string `json:"image_base64"`
		Width       int    `json:"width"`
	}
	json.Unmarshal(rec.Body.Bytes(), &body)
	if body.ImageBase64 != "AAAA" || body.Width != 100 {
		t.Fatalf("body = %+v, want image AAAA width 100", body)
	}
}
