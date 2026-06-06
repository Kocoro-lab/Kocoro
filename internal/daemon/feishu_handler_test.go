package daemon

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/config"
)

// newFeishuTestServer builds a minimal Server for exercising the Feishu/Lark
// app-install handlers' LOCAL branches (the cloud-readiness gate and required-
// field validation), which all return before any Cloud round-trip. auth is left
// nil so liveAPIKey falls back to cfg.APIKey; GW (when present) is a dummy that
// is never actually called on these paths.
func newFeishuTestServer(cloudEnabled, withGW bool) *Server {
	cfg := &config.Config{}
	cfg.Cloud.Enabled = cloudEnabled
	cfg.APIKey = "test-key"
	deps := &ServerDeps{Config: cfg}
	if withGW {
		deps.GW = client.NewGatewayClient("http://127.0.0.1:1", "test-key")
	}
	return &Server{deps: deps}
}

func TestHandleCreateFeishuAppInstall_GateAndValidation(t *testing.T) {
	tests := []struct {
		name         string
		cloudEnabled bool
		withGW       bool
		body         string
		wantStatus   int
	}{
		{"cloud disabled -> 503", false, true, `{"channel_type":"feishu","app_id":"a","app_secret":"b"}`, http.StatusServiceUnavailable},
		{"no gateway -> 503", true, false, `{"channel_type":"feishu","app_id":"a","app_secret":"b"}`, http.StatusServiceUnavailable},
		{"bad channel_type -> 400", true, true, `{"channel_type":"wechat","app_id":"a","app_secret":"b"}`, http.StatusBadRequest},
		{"missing channel_type -> 400", true, true, `{"app_id":"a","app_secret":"b"}`, http.StatusBadRequest},
		{"missing app_id -> 400", true, true, `{"channel_type":"feishu","app_secret":"b"}`, http.StatusBadRequest},
		{"missing app_secret -> 400", true, true, `{"channel_type":"lark","app_id":"a"}`, http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newFeishuTestServer(tt.cloudEnabled, tt.withGW)
			req := httptest.NewRequest(http.MethodPost, "/channels/feishu/app-installs", strings.NewReader(tt.body))
			rr := httptest.NewRecorder()
			s.handleCreateFeishuAppInstall(rr, req)
			if rr.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d (body: %s)", rr.Code, tt.wantStatus, rr.Body.String())
			}
		})
	}
}

func TestHandleListFeishuAppInstalls_CloudGate(t *testing.T) {
	s := newFeishuTestServer(false, false)
	req := httptest.NewRequest(http.MethodGet, "/channels/feishu/app-installs", nil)
	rr := httptest.NewRecorder()
	s.handleListFeishuAppInstalls(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 (body: %s)", rr.Code, rr.Body.String())
	}
}

func TestHandleDeleteFeishuAppInstall_GateAndValidation(t *testing.T) {
	t.Run("cloud disabled -> 503", func(t *testing.T) {
		s := newFeishuTestServer(false, false)
		req := httptest.NewRequest(http.MethodDelete, "/channels/feishu/app-installs/some-id", nil)
		req.SetPathValue("id", "some-id")
		rr := httptest.NewRecorder()
		s.handleDeleteFeishuAppInstall(rr, req)
		if rr.Code != http.StatusServiceUnavailable {
			t.Errorf("status = %d, want 503 (body: %s)", rr.Code, rr.Body.String())
		}
	})
	t.Run("blank id -> 400", func(t *testing.T) {
		s := newFeishuTestServer(true, true)
		req := httptest.NewRequest(http.MethodDelete, "/channels/feishu/app-installs/x", nil)
		req.SetPathValue("id", "   ")
		rr := httptest.NewRecorder()
		s.handleDeleteFeishuAppInstall(rr, req)
		if rr.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400 (body: %s)", rr.Code, rr.Body.String())
		}
	})
}
