package daemon

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
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

// TestHandleConnectIntegration_ForwardsBody pins the token-mode connect
// contract (Shopify): the client's JSON body must reach Cloud verbatim with
// Content-Type set, and a body-less OAuth connect must keep sending no body
// and no Content-Type.
func TestHandleConnectIntegration_ForwardsBody(t *testing.T) {
	type captured struct {
		body        string
		contentType string
	}
	var got captured
	cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = captured{body: string(b), contentType: r.Header.Get("Content-Type")}
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"connection_id":"c1","status":"active"}`))
	}))
	defer cloud.Close()

	cfg := &config.Config{}
	cfg.Cloud.Enabled = true
	cfg.APIKey = "test-key"
	s := &Server{deps: &ServerDeps{Config: cfg, GW: client.NewGatewayClient(cloud.URL, "test-key")}}

	t.Run("token-mode body forwarded verbatim", func(t *testing.T) {
		payload := `{"params":{"shop":"mystore.myshopify.com","access_token":"shpat_x"}}`
		req := httptest.NewRequest(http.MethodPost, "/integrations/shopify/connect", strings.NewReader(payload))
		req.SetPathValue("provider", "shopify")
		rr := httptest.NewRecorder()
		s.handleConnectIntegration(rr, req)
		if rr.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201 (body: %s)", rr.Code, rr.Body.String())
		}
		if got.body != payload {
			t.Errorf("cloud received body %q, want %q", got.body, payload)
		}
		if got.contentType != "application/json" {
			t.Errorf("cloud received Content-Type %q, want application/json", got.contentType)
		}
		if rr.Body.String() != `{"connection_id":"c1","status":"active"}` {
			t.Errorf("response not passed through verbatim: %s", rr.Body.String())
		}
	})

	t.Run("oauth connect stays body-less", func(t *testing.T) {
		got = captured{}
		req := httptest.NewRequest(http.MethodPost, "/integrations/figma/connect", nil)
		req.SetPathValue("provider", "figma")
		rr := httptest.NewRecorder()
		s.handleConnectIntegration(rr, req)
		if rr.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201 (body: %s)", rr.Code, rr.Body.String())
		}
		if got.body != "" {
			t.Errorf("cloud received unexpected body %q, want empty", got.body)
		}
		if got.contentType != "" {
			t.Errorf("cloud received unexpected Content-Type %q, want empty", got.contentType)
		}
	})

	t.Run("oversized body -> 413 before any cloud call", func(t *testing.T) {
		got = captured{contentType: "sentinel"}
		req := httptest.NewRequest(http.MethodPost, "/integrations/shopify/connect",
			strings.NewReader(strings.Repeat("x", maxIntegrationConnectBodyBytes+1)))
		req.SetPathValue("provider", "shopify")
		rr := httptest.NewRecorder()
		s.handleConnectIntegration(rr, req)
		if rr.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("status = %d, want 413 (body: %s)", rr.Code, rr.Body.String())
		}
		if got.contentType != "sentinel" {
			t.Error("cloud must not be called when the body exceeds the cap")
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

// TestRefreshIntegrationTools_SyncsGatewayOverlay guards the fix for the
// health-rebuild staleness bug: after an in-place refresh, the cached
// GatewayOverlay must include the integration tools, so a later MCP health
// rebuild (which rebuilds from the cached overlay) does not drop them.
func TestRefreshIntegrationTools_SyncsGatewayOverlay(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode([]client.ServerToolSchema{{Name: "notion_search"}})
	}))
	defer server.Close()

	cfg := &config.Config{}
	cfg.Cloud.Enabled = true
	cfg.APIKey = "test-key"
	reg := agent.NewToolRegistry()
	deps := &ServerDeps{Config: cfg, Registry: reg, GW: client.NewGatewayClient(server.URL, "test-key")}
	s := &Server{deps: deps}

	if err := s.RefreshIntegrationTools(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if _, ok := reg.Get("notion_search"); !ok {
		t.Error("notion_search should be registered in the live registry")
	}
	found := false
	for _, tl := range deps.GatewayOverlay {
		if tl.Info().Name == "notion_search" {
			found = true
		}
	}
	if !found {
		t.Error("integration tool must be in GatewayOverlay after refresh (survives MCP health rebuild)")
	}
}

func TestHandleRefreshIntegrations_CloudGate(t *testing.T) {
	s := newIntegrationsTestServer(false, false)
	req := httptest.NewRequest(http.MethodPost, "/integrations/refresh", nil)
	rr := httptest.NewRecorder()
	s.handleRefreshIntegrations(rr, req)
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
