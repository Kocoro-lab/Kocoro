package daemon

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/config"
)

// newIntegrationsTestServer builds a minimal Server for exercising the
// integrations handlers' LOCAL branches (the cloud-readiness gate and the
// blank path-parameter validation), which all return before any Cloud
// round-trip. auth is left nil so liveAPIKey falls back to cfg.APIKey; GW (when
// present) is a dummy that is never actually called on these paths.
func newIntegrationsTestServer(cloudEnabled, withGW bool) *Server {
	cfg := &config.Config{}
	cfg.Cloud.Enabled = cloudEnabled
	cfg.APIKey = "test-key"
	deps := &ServerDeps{Config: cfg}
	if withGW {
		deps.GW = client.NewGatewayClient("http://127.0.0.1:1", "test-key")
	}
	return &Server{deps: deps}
}

func TestHandleConnectIntegration_GateAndValidation(t *testing.T) {
	t.Run("cloud disabled -> 503", func(t *testing.T) {
		s := newIntegrationsTestServer(false, true)
		req := httptest.NewRequest(http.MethodPost, "/integrations/figma/connect", nil)
		req.SetPathValue("provider", "figma")
		rr := httptest.NewRecorder()
		s.handleConnectIntegration(rr, req)
		if rr.Code != http.StatusServiceUnavailable {
			t.Errorf("status = %d, want 503 (body: %s)", rr.Code, rr.Body.String())
		}
	})
	t.Run("no gateway -> 503", func(t *testing.T) {
		s := newIntegrationsTestServer(true, false)
		req := httptest.NewRequest(http.MethodPost, "/integrations/figma/connect", nil)
		req.SetPathValue("provider", "figma")
		rr := httptest.NewRecorder()
		s.handleConnectIntegration(rr, req)
		if rr.Code != http.StatusServiceUnavailable {
			t.Errorf("status = %d, want 503 (body: %s)", rr.Code, rr.Body.String())
		}
	})
	t.Run("blank provider -> 400", func(t *testing.T) {
		s := newIntegrationsTestServer(true, true)
		req := httptest.NewRequest(http.MethodPost, "/integrations/x/connect", nil)
		req.SetPathValue("provider", "   ")
		rr := httptest.NewRecorder()
		s.handleConnectIntegration(rr, req)
		if rr.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400 (body: %s)", rr.Code, rr.Body.String())
		}
	})
}

func TestHandleListIntegrations_CloudGate(t *testing.T) {
	s := newIntegrationsTestServer(false, false)
	req := httptest.NewRequest(http.MethodGet, "/integrations", nil)
	rr := httptest.NewRecorder()
	s.handleListIntegrations(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 (body: %s)", rr.Code, rr.Body.String())
	}
}

func TestHandleGetIntegration_GateAndValidation(t *testing.T) {
	t.Run("cloud disabled -> 503", func(t *testing.T) {
		s := newIntegrationsTestServer(false, false)
		req := httptest.NewRequest(http.MethodGet, "/integrations/some-id", nil)
		req.SetPathValue("id", "some-id")
		rr := httptest.NewRecorder()
		s.handleGetIntegration(rr, req)
		if rr.Code != http.StatusServiceUnavailable {
			t.Errorf("status = %d, want 503 (body: %s)", rr.Code, rr.Body.String())
		}
	})
	t.Run("blank id -> 400", func(t *testing.T) {
		s := newIntegrationsTestServer(true, true)
		req := httptest.NewRequest(http.MethodGet, "/integrations/x", nil)
		req.SetPathValue("id", "   ")
		rr := httptest.NewRecorder()
		s.handleGetIntegration(rr, req)
		if rr.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400 (body: %s)", rr.Code, rr.Body.String())
		}
	})
}

func TestHandleDeleteIntegration_GateAndValidation(t *testing.T) {
	t.Run("cloud disabled -> 503", func(t *testing.T) {
		s := newIntegrationsTestServer(false, false)
		req := httptest.NewRequest(http.MethodDelete, "/integrations/some-id", nil)
		req.SetPathValue("id", "some-id")
		rr := httptest.NewRecorder()
		s.handleDeleteIntegration(rr, req)
		if rr.Code != http.StatusServiceUnavailable {
			t.Errorf("status = %d, want 503 (body: %s)", rr.Code, rr.Body.String())
		}
	})
	t.Run("blank id -> 400", func(t *testing.T) {
		s := newIntegrationsTestServer(true, true)
		req := httptest.NewRequest(http.MethodDelete, "/integrations/x", nil)
		req.SetPathValue("id", "   ")
		rr := httptest.NewRecorder()
		s.handleDeleteIntegration(rr, req)
		if rr.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400 (body: %s)", rr.Code, rr.Body.String())
		}
	})
}
