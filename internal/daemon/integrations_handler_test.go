package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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

	t.Run("whitespace-only body treated as absent", func(t *testing.T) {
		got = captured{}
		req := httptest.NewRequest(http.MethodPost, "/integrations/figma/connect", strings.NewReader(" \n\t"))
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

// TestHandleConnectIntegration_RefreshGate pins the 2xx guard on the
// post-connect tool refresh: a token-mode success (connection active on the
// connect response itself) must fire the async integration-tool refresh, and
// a rejected credential must pass Cloud's error through untouched without
// kicking a pointless registry refresh.
func TestHandleConnectIntegration_RefreshGate(t *testing.T) {
	newServer := func(connectStatus int, connectBody string, toolsFetched chan struct{}) (*Server, *httptest.Server) {
		cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v1/integrations/tools" {
				toolsFetched <- struct{}{}
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(`[]`))
				return
			}
			w.WriteHeader(connectStatus)
			w.Write([]byte(connectBody))
		}))
		cfg := &config.Config{}
		cfg.Cloud.Enabled = true
		cfg.APIKey = "test-key"
		s := &Server{deps: &ServerDeps{
			Config:   cfg,
			Registry: agent.NewToolRegistry(),
			GW:       client.NewGatewayClient(cloud.URL, "test-key"),
		}}
		return s, cloud
	}
	connectReq := func() *http.Request {
		req := httptest.NewRequest(http.MethodPost, "/integrations/shopify/connect",
			strings.NewReader(`{"params":{"shop":"s.myshopify.com","access_token":"shpat_x"}}`))
		req.SetPathValue("provider", "shopify")
		return req
	}

	t.Run("2xx fires async tool refresh", func(t *testing.T) {
		toolsFetched := make(chan struct{}, 1)
		s, cloud := newServer(http.StatusCreated, `{"connection_id":"c1","status":"active"}`, toolsFetched)
		defer cloud.Close()
		rr := httptest.NewRecorder()
		s.handleConnectIntegration(rr, connectReq())
		if rr.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201 (body: %s)", rr.Code, rr.Body.String())
		}
		select {
		case <-toolsFetched:
		case <-time.After(3 * time.Second):
			t.Error("tool refresh not fired after a 2xx connect")
		}
	})

	t.Run("non-2xx passes through and skips refresh", func(t *testing.T) {
		toolsFetched := make(chan struct{}, 1)
		s, cloud := newServer(http.StatusUnauthorized, `{"error":"invalid access token"}`, toolsFetched)
		defer cloud.Close()
		rr := httptest.NewRecorder()
		s.handleConnectIntegration(rr, connectReq())
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401 (body: %s)", rr.Code, rr.Body.String())
		}
		if rr.Body.String() != `{"error":"invalid access token"}` {
			t.Errorf("error body not passed through verbatim: %s", rr.Body.String())
		}
		select {
		case <-toolsFetched:
			t.Error("tool refresh must not fire on a rejected connect")
		case <-time.After(200 * time.Millisecond):
		}
	})
}

// captureLog redirects the global log output to a buffer for the duration of
// the test, restoring the previous writer on cleanup (never a hardcoded
// os.Stderr — an outer redirection must survive).
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })
	return &buf
}

// TestHandleConnectIntegration_FailureLogging pins the connect-failure log's
// diagnostic content and its two safety invariants: request-body credentials
// never reach the log (not even via a response that echoes request input),
// and an attacker-controlled provider segment cannot forge log lines.
func TestHandleConnectIntegration_FailureLogging(t *testing.T) {
	newServer := func(connectStatus int, connectBody string) (*Server, *httptest.Server) {
		cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(connectStatus)
			w.Write([]byte(connectBody))
		}))
		cfg := &config.Config{}
		cfg.Cloud.Enabled = true
		cfg.APIKey = "test-key"
		s := &Server{deps: &ServerDeps{
			Config:   cfg,
			Registry: agent.NewToolRegistry(),
			GW:       client.NewGatewayClient(cloud.URL, "test-key"),
		}}
		return s, cloud
	}
	tokenReq := func(provider string) *http.Request {
		req := httptest.NewRequest(http.MethodPost, "/integrations/p/connect",
			strings.NewReader(`{"params":{"shop":"s.myshopify.com","access_token":"shpat_x"}}`))
		req.SetPathValue("provider", provider)
		return req
	}
	oauthReq := func(provider string) *http.Request {
		req := httptest.NewRequest(http.MethodPost, "/integrations/p/connect", nil)
		req.SetPathValue("provider", provider)
		return req
	}
	assertNoCredentials := func(t *testing.T, logged string) {
		t.Helper()
		for _, leak := range []string{"shpat_x", "s.myshopify.com", "access_token"} {
			if strings.Contains(logged, leak) {
				t.Errorf("failure log leaked request-body content %q (log: %s)", leak, logged)
			}
		}
	}

	t.Run("contract error body logs code and message", func(t *testing.T) {
		logBuf := captureLog(t)
		s, cloud := newServer(http.StatusUnauthorized, `{"error":"invalid_token","message":"token was rejected"}`)
		defer cloud.Close()
		s.handleConnectIntegration(httptest.NewRecorder(), tokenReq("shopify"))
		logged := logBuf.String()
		for _, want := range []string{"integration connect rejected by cloud", `provider="shopify"`, "status=401", `error="invalid_token"`, `message="token was rejected"`} {
			if !strings.Contains(logged, want) {
				t.Errorf("failure log missing %q (log: %s)", want, logged)
			}
		}
		assertNoCredentials(t, logged)
	})

	t.Run("non-contract body on a token-mode connect logs length only", func(t *testing.T) {
		// A framework validation error can echo request input (pydantic 422
		// carries the submitted value in detail[].input); the request had a
		// body, so the raw body must never be quoted.
		echo := `{"detail":[{"loc":["params","access_token"],"input":"shpat_x"}]}`
		logBuf := captureLog(t)
		s, cloud := newServer(http.StatusUnprocessableEntity, echo)
		defer cloud.Close()
		s.handleConnectIntegration(httptest.NewRecorder(), tokenReq("shopify"))
		logged := logBuf.String()
		for _, want := range []string{`provider="shopify"`, "status=422", "unparsed_body_len="} {
			if !strings.Contains(logged, want) {
				t.Errorf("failure log missing %q (log: %s)", want, logged)
			}
		}
		assertNoCredentials(t, logged)
	})

	t.Run("non-contract body on a body-less connect quotes the body", func(t *testing.T) {
		logBuf := captureLog(t)
		s, cloud := newServer(http.StatusBadGateway, "<html>upstream down</html>")
		defer cloud.Close()
		s.handleConnectIntegration(httptest.NewRecorder(), oauthReq("notion"))
		logged := logBuf.String()
		for _, want := range []string{`provider="notion"`, "status=502", `body="<html>upstream down</html>"`} {
			if !strings.Contains(logged, want) {
				t.Errorf("failure log missing %q (log: %s)", want, logged)
			}
		}
	})

	t.Run("quoted body is capped", func(t *testing.T) {
		logBuf := captureLog(t)
		s, cloud := newServer(http.StatusBadGateway, strings.Repeat("a", 3*maxIntegrationConnectLogBodyBytes))
		defer cloud.Close()
		s.handleConnectIntegration(httptest.NewRecorder(), oauthReq("notion"))
		logged := logBuf.String()
		if !strings.Contains(logged, strings.Repeat("a", maxIntegrationConnectLogBodyBytes)) {
			t.Errorf("capped body prefix missing from log (log len %d)", len(logged))
		}
		if strings.Contains(logged, strings.Repeat("a", maxIntegrationConnectLogBodyBytes+1)) {
			t.Errorf("logged body exceeds %d-byte cap (log len %d)", maxIntegrationConnectLogBodyBytes, len(logged))
		}
	})

	t.Run("provider newline cannot forge log lines", func(t *testing.T) {
		logBuf := captureLog(t)
		s, cloud := newServer(http.StatusUnauthorized, `{"error":"invalid_token"}`)
		defer cloud.Close()
		s.handleConnectIntegration(httptest.NewRecorder(), oauthReq("x\nfake: forged line"))
		logged := logBuf.String()
		if strings.Contains(logged, "\nfake: forged line") {
			t.Errorf("provider newline reached the log stream unescaped (log: %s)", logged)
		}
		if !strings.Contains(logged, `provider="x\nfake: forged line"`) {
			t.Errorf("provider not %%q-escaped (log: %s)", logged)
		}
	})

	t.Run("3xx is logged", func(t *testing.T) {
		// 304 is a non-redirect 3xx the HTTP client returns as-is; it must not
		// fall through the old >=400 gate silently.
		logBuf := captureLog(t)
		s, cloud := newServer(http.StatusNotModified, "")
		defer cloud.Close()
		s.handleConnectIntegration(httptest.NewRecorder(), oauthReq("notion"))
		if logged := logBuf.String(); !strings.Contains(logged, "status=304") {
			t.Errorf("3xx connect failure not logged (log: %s)", logged)
		}
	})

	t.Run("transport failure logged distinctly", func(t *testing.T) {
		logBuf := captureLog(t)
		cfg := &config.Config{}
		cfg.Cloud.Enabled = true
		cfg.APIKey = "test-key"
		s := &Server{deps: &ServerDeps{
			Config: cfg,
			GW:     client.NewGatewayClient("http://127.0.0.1:1", "test-key"),
		}}
		s.handleConnectIntegration(httptest.NewRecorder(), tokenReq("shopify"))
		logged := logBuf.String()
		for _, want := range []string{"integration connect transport failure", `provider="shopify"`, "err="} {
			if !strings.Contains(logged, want) {
				t.Errorf("transport-failure log missing %q (log: %s)", want, logged)
			}
		}
		assertNoCredentials(t, logged)
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
