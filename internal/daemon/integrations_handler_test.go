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
	"sync/atomic"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/config"
	"github.com/Kocoro-lab/ShanClaw/internal/keychain"
	"github.com/Kocoro-lab/ShanClaw/internal/tools"
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

// TestHandleConnectIntegration_ForwardsBody pins the connect-param contract
// (Shopify / Jira / Confluence / Salesforce declare a subdomain): the client's
// JSON body must reach Cloud verbatim with Content-Type set, and a connect for
// a provider with no declared params must keep sending no body and no
// Content-Type.
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
		w.Write([]byte(`{"connection_id":"c1","oauth_url":"https://auth.example/x","status":"pending"}`))
	}))
	defer cloud.Close()

	cfg := &config.Config{}
	cfg.Cloud.Enabled = true
	cfg.APIKey = "test-key"
	s := &Server{deps: &ServerDeps{Config: cfg, GW: client.NewGatewayClient(cloud.URL, "test-key")}}

	t.Run("connect params forwarded verbatim", func(t *testing.T) {
		payload := `{"params":{"subdomain":"mystore"}}`
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
		if rr.Body.String() != `{"connection_id":"c1","oauth_url":"https://auth.example/x","status":"pending"}` {
			t.Errorf("response not passed through verbatim: %s", rr.Body.String())
		}
	})

	t.Run("param-less connect stays body-less", func(t *testing.T) {
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
// post-connect tool refresh: an accepted connect must fire the async
// integration-tool refresh (a best-effort backstop — the connection only goes
// active after browser OAuth), and a rejected connect must pass Cloud's error
// through untouched without kicking a pointless registry refresh.
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
			strings.NewReader(`{"params":{"subdomain":"mystore"}}`))
		req.SetPathValue("provider", "shopify")
		return req
	}

	t.Run("2xx fires async tool refresh", func(t *testing.T) {
		toolsFetched := make(chan struct{}, 1)
		s, cloud := newServer(http.StatusCreated, `{"connection_id":"c1","oauth_url":"https://auth.example/x","status":"pending"}`, toolsFetched)
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
	paramReq := func(provider string) *http.Request {
		req := httptest.NewRequest(http.MethodPost, "/integrations/p/connect",
			strings.NewReader(`{"params":{"subdomain":"mystore"}}`))
		req.SetPathValue("provider", provider)
		return req
	}
	paramlessReq := func(provider string) *http.Request {
		req := httptest.NewRequest(http.MethodPost, "/integrations/p/connect", nil)
		req.SetPathValue("provider", provider)
		return req
	}
	// The invariant is that the SUBMITTED VALUE never reaches the log. Param
	// names are deliberately not asserted on: Cloud's own contract messages
	// legitimately name the offending parameter ("invalid format for
	// parameter: subdomain"), and that message is quoted by design.
	assertNoRequestBodyEcho := func(t *testing.T, logged string) {
		t.Helper()
		if strings.Contains(logged, "mystore") {
			t.Errorf("failure log leaked the submitted param value (log: %s)", logged)
		}
	}

	t.Run("contract error body logs code and message", func(t *testing.T) {
		logBuf := captureLog(t)
		s, cloud := newServer(http.StatusBadRequest, `{"error":"invalid_shop_domain","message":"invalid format for parameter: subdomain"}`)
		defer cloud.Close()
		s.handleConnectIntegration(httptest.NewRecorder(), paramReq("shopify"))
		logged := logBuf.String()
		for _, want := range []string{"integration connect rejected by cloud", `provider="shopify"`, "status=400", `error="invalid_shop_domain"`, `message="invalid format for parameter: subdomain"`} {
			if !strings.Contains(logged, want) {
				t.Errorf("failure log missing %q (log: %s)", want, logged)
			}
		}
		assertNoRequestBodyEcho(t, logged)
	})

	t.Run("non-contract body on a connect with params logs length only", func(t *testing.T) {
		// A framework validation error can echo request input (pydantic 422
		// carries the submitted value in detail[].input); the request had a
		// body, so the raw body must never be quoted.
		echo := `{"detail":[{"loc":["params","subdomain"],"input":"mystore"}]}`
		logBuf := captureLog(t)
		s, cloud := newServer(http.StatusUnprocessableEntity, echo)
		defer cloud.Close()
		s.handleConnectIntegration(httptest.NewRecorder(), paramReq("shopify"))
		logged := logBuf.String()
		for _, want := range []string{`provider="shopify"`, "status=422", "unparsed_body_len="} {
			if !strings.Contains(logged, want) {
				t.Errorf("failure log missing %q (log: %s)", want, logged)
			}
		}
		assertNoRequestBodyEcho(t, logged)
	})

	t.Run("non-contract body on a body-less connect quotes the body", func(t *testing.T) {
		logBuf := captureLog(t)
		s, cloud := newServer(http.StatusBadGateway, "<html>upstream down</html>")
		defer cloud.Close()
		s.handleConnectIntegration(httptest.NewRecorder(), paramlessReq("notion"))
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
		s.handleConnectIntegration(httptest.NewRecorder(), paramlessReq("notion"))
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
		s, cloud := newServer(http.StatusBadRequest, `{"error":"invalid_request"}`)
		defer cloud.Close()
		s.handleConnectIntegration(httptest.NewRecorder(), paramlessReq("x\nfake: forged line"))
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
		s.handleConnectIntegration(httptest.NewRecorder(), paramlessReq("notion"))
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
		s.handleConnectIntegration(httptest.NewRecorder(), paramReq("shopify"))
		logged := logBuf.String()
		for _, want := range []string{"integration connect transport failure", `provider="shopify"`, "err="} {
			if !strings.Contains(logged, want) {
				t.Errorf("transport-failure log missing %q (log: %s)", want, logged)
			}
		}
		assertNoRequestBodyEcho(t, logged)
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

func TestIntegrationTools_WS401SignOutClearsPreviousPrincipalCatalog(t *testing.T) {
	cfg := &config.Config{}
	reg := agent.NewToolRegistry()
	gw := client.NewGatewayClient("http://127.0.0.1:1", "old-key")
	reg.Register(tools.NewIntegrationTool(client.ServerToolSchema{Name: "x_mentions"}, gw))
	deps := &ServerDeps{Config: cfg, Registry: reg, GW: gw, ShannonDir: t.TempDir()}
	s := NewServer(0, nil, deps, "test")
	kc := keychain.NewStore(keychain.NewMemBackend(), nil)
	_ = kc.SetAPIKey("account-a", "old-key")
	auth := NewAuthManager(AuthManagerConfig{Keychain: kc, Gateway: gw})
	auth.setState(AuthStateSignedIn, &client.AuthUser{ID: "account-a"}, "")
	s.SetAuth(auth)

	auth.HandleWSAuthFailure()
	if _, ok := reg.Get("x_mentions"); ok {
		t.Fatal("WS 401 sign-out retained the previous principal's integration tool")
	}
	for _, tool := range deps.GatewayOverlay {
		if sourcer, ok := tool.(agent.ToolSourcer); ok && sourcer.ToolSource() == agent.SourceIntegration {
			t.Fatalf("WS 401 sign-out retained integration in gateway overlay: %s", tool.Info().Name)
		}
	}
}

func TestIntegrationTools_AccountSwitchTransientListFailureStaysEmpty(t *testing.T) {
	cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/auth/me":
			_ = json.NewEncoder(w).Encode(client.AuthUser{ID: "account-b", Email: "b@example.com"})
		case "/api/v1/integrations/tools":
			http.Error(w, "temporary outage", http.StatusBadGateway)
		default:
			t.Fatalf("unexpected cloud path %s", r.URL.Path)
		}
	}))
	defer cloud.Close()

	cfg := &config.Config{}
	reg := agent.NewToolRegistry()
	gw := client.NewGatewayClient(cloud.URL, "old-key")
	reg.Register(tools.NewIntegrationTool(client.ServerToolSchema{Name: "x_mentions"}, gw))
	deps := &ServerDeps{Config: cfg, Registry: reg, GW: gw, ShannonDir: t.TempDir()}
	s := NewServer(0, nil, deps, "test")
	kc := keychain.NewStore(keychain.NewMemBackend(), nil)
	_ = kc.SetAPIKey("account-a", "old-key")
	auth := NewAuthManager(AuthManagerConfig{
		Keychain: kc,
		Cloud:    client.NewAuthClient(cloud.URL, cloud.Client()),
		Gateway:  gw,
		OnAPIKeyChanging: func(context.Context) {
			s.InvalidateIntegrationTools()
		},
		OnAPIKeyChanged: func(ctx context.Context) {
			s.RebuildAuthSensitiveTools(ctx)
		},
	})
	auth.setState(AuthStateSignedIn, &client.AuthUser{ID: "account-a"}, "")
	s.SetAuth(auth)

	if err := auth.AdoptKey(context.Background(), "new-key"); err != nil {
		t.Fatalf("AdoptKey: %v", err)
	}
	if id, _, ok := auth.VerifiedPrincipal(); !ok || id != "account-b" {
		t.Fatalf("verified principal = %q, %t", id, ok)
	}
	if _, ok := reg.Get("x_mentions"); ok {
		t.Fatal("account switch transient failure retained account A's tool")
	}
}

func TestIntegrationTools_SamePrincipalKeyRotationRepopulatesNewCatalog(t *testing.T) {
	var catalogKeys []string
	cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/auth/me":
			_ = json.NewEncoder(w).Encode(client.AuthUser{ID: "account-a", Email: "a@example.com"})
		case "/api/v1/integrations/tools":
			key := r.Header.Get("X-API-Key")
			catalogKeys = append(catalogKeys, key)
			if key != "new-key" {
				t.Fatalf("catalog fetched with key %q, want new-key", key)
			}
			_ = json.NewEncoder(w).Encode([]client.ServerToolSchema{{Name: "x_new_catalog"}})
		default:
			t.Fatalf("unexpected cloud path %s", r.URL.Path)
		}
	}))
	defer cloud.Close()

	cfg := &config.Config{}
	reg := agent.NewToolRegistry()
	gw := client.NewGatewayClient(cloud.URL, "old-key")
	reg.Register(tools.NewIntegrationTool(client.ServerToolSchema{Name: "x_old_catalog"}, gw))
	deps := &ServerDeps{Config: cfg, Registry: reg, GW: gw, ShannonDir: t.TempDir()}
	s := NewServer(0, nil, deps, "test")
	kc := keychain.NewStore(keychain.NewMemBackend(), nil)
	_ = kc.SetAPIKey("account-a", "old-key")
	auth := NewAuthManager(AuthManagerConfig{
		Keychain: kc,
		Cloud:    client.NewAuthClient(cloud.URL, cloud.Client()),
		Gateway:  gw,
		OnAPIKeyChanging: func(context.Context) {
			s.InvalidateIntegrationTools()
		},
		OnAPIKeyChanged: func(ctx context.Context) {
			s.RebuildAuthSensitiveTools(ctx)
		},
	})
	auth.setState(AuthStateSignedIn, &client.AuthUser{ID: "account-a"}, "")
	_, oldEpoch, _ := auth.VerifiedPrincipal()
	s.SetAuth(auth)
	auth.applyAPIKey(context.Background(), "old-key")
	if _, ok := reg.Get("x_old_catalog"); ok || len(catalogKeys) != 0 {
		t.Fatalf("same-byte credential mutation retained old catalog or refetched before verified binding: present=%t keys=%v", ok, catalogKeys)
	}

	if err := auth.AdoptKey(context.Background(), "new-key"); err != nil {
		t.Fatalf("AdoptKey: %v", err)
	}
	_, newEpoch, ok := auth.VerifiedPrincipal()
	if !ok || newEpoch <= oldEpoch {
		t.Fatalf("principal epoch old=%d new=%d verified=%t", oldEpoch, newEpoch, ok)
	}
	if _, ok := reg.Get("x_old_catalog"); ok {
		t.Fatal("same-principal key rotation retained old catalog")
	}
	if _, ok := reg.Get("x_new_catalog"); !ok {
		t.Fatal("same-principal key rotation did not repopulate new catalog")
	}
	if len(catalogKeys) != 1 || catalogKeys[0] != "new-key" {
		t.Fatalf("catalog keys = %v", catalogKeys)
	}
}

func TestAuthManager_KeySwapInvalidatesCapturedToolsBeforeBlockingRegistryCallback(t *testing.T) {
	var uploadCalls atomic.Int32
	var executeCalls atomic.Int32
	var oldCatalogCalls atomic.Int32
	var newCatalogCalls atomic.Int32
	cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/auth/me":
			if got := r.Header.Get("X-API-Key"); got != "new-key" {
				t.Errorf("auth probe key = %q, want new-key", got)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"user_id": "account-b",
				"email":   "b@example.com",
			})
		case r.URL.Path == "/api/v1/integrations/tools":
			switch r.Header.Get("X-API-Key") {
			case "old-key":
				oldCatalogCalls.Add(1)
				_ = json.NewEncoder(w).Encode([]client.ServerToolSchema{{Name: "x_old_catalog"}})
			case "new-key":
				newCatalogCalls.Add(1)
				_ = json.NewEncoder(w).Encode([]client.ServerToolSchema{{Name: "x_new_catalog"}})
			default:
				t.Errorf("catalog key = %q", r.Header.Get("X-API-Key"))
				http.Error(w, "unexpected key", http.StatusUnauthorized)
			}
		case r.URL.Path == "/api/v1/uploads":
			uploadCalls.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"uploads": []any{}, "total_count": 0})
		case strings.HasSuffix(r.URL.Path, "/execute"):
			executeCalls.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"output":  map[string]any{"ok": true},
			})
		default:
			t.Errorf("unexpected cloud path %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer cloud.Close()

	baseline := agent.NewToolRegistry()
	reg := baseline.Clone()
	cfg := &config.Config{
		Endpoint: cloud.URL,
		APIKey:   "old-key",
		Cloud:    config.CloudConfig{Enabled: true},
	}
	gw := client.NewGatewayClient(cloud.URL, "old-key")
	deps := &ServerDeps{
		Config:      cfg,
		GW:          gw,
		Registry:    reg,
		BaselineReg: baseline,
		ShannonDir:  t.TempDir(),
	}
	server := NewServer(0, nil, deps, "test")
	callbackEntered := make(chan struct{})
	releaseCallback := make(chan struct{})
	var released atomic.Bool
	release := func() {
		if released.CompareAndSwap(false, true) {
			close(releaseCallback)
		}
	}
	t.Cleanup(release)
	auth := NewAuthManager(AuthManagerConfig{
		Keychain: keychain.NewStore(keychain.NewMemBackend(), nil),
		Cloud:    client.NewAuthClient(cloud.URL, cloud.Client()),
		Gateway:  gw,
		OnAPIKeyChanging: func(context.Context) {
			close(callbackEntered)
			<-releaseCallback
			server.InvalidateIntegrationTools()
		},
		OnAPIKeyChanged: func(ctx context.Context) {
			server.RebuildAuthSensitiveTools(ctx)
		},
	})
	server.SetAuth(auth)
	auth.setState(AuthStateSignedIn, &client.AuthUser{ID: "account-a"}, "")
	if got := oldCatalogCalls.Load(); got != 1 {
		t.Fatalf("initial old catalog fetches = %d, want 1", got)
	}

	captured := tools.CloneWithRuntimeConfig(reg, cfg)
	oldPost, ok := captured.Get("list_my_published_files")
	if !ok {
		t.Fatal("initial principal did not register list_my_published_files")
	}
	oldIntegration, ok := captured.Get("x_old_catalog")
	if !ok {
		t.Fatal("initial principal did not register x_old_catalog")
	}

	adoptDone := make(chan error, 1)
	go func() {
		adoptDone <- auth.AdoptKey(context.Background(), "new-key")
	}()
	<-callbackEntered

	// The registry callback is deliberately blocked before clearing the live
	// registry. Generation invalidation, rather than pointer removal, must make
	// both previously captured tools fail before dispatch in this window.
	if got := gw.APIKey(); got != "new-key" {
		t.Fatalf("gateway key during callback = %q, want new-key", got)
	}
	if generation, active := gw.IntegrationGeneration(); active {
		t.Fatalf("integration generation %d remained active during callback", generation)
	}
	if _, ok := reg.Get("list_my_published_files"); !ok {
		t.Fatal("test did not reach the intended pre-clear callback window")
	}
	if _, ok := reg.Get("x_old_catalog"); !ok {
		t.Fatal("old integration catalog cleared before the blocked callback resumed")
	}
	for _, capturedTool := range []agent.Tool{oldPost, oldIntegration} {
		result, err := capturedTool.Run(context.Background(), `{}`)
		if err != nil {
			t.Fatalf("%s Run: %v", capturedTool.Info().Name, err)
		}
		if !result.IsError || result.ErrorCategory != agent.ErrCategoryBusiness ||
			!result.SideEffectKnownNoEffect || result.SideEffectOutcomeUnknown {
			t.Fatalf("%s stale result = %#v", capturedTool.Info().Name, result)
		}
	}
	if got := uploadCalls.Load(); got != 0 {
		t.Fatalf("stale post overlay dispatched %d HTTP request(s)", got)
	}
	if got := executeCalls.Load(); got != 0 {
		t.Fatalf("stale integration tool dispatched %d HTTP request(s)", got)
	}
	select {
	case err := <-adoptDone:
		t.Fatalf("AdoptKey completed inside blocked registry callback: %v", err)
	case <-time.After(25 * time.Millisecond):
	}

	release()
	select {
	case err := <-adoptDone:
		if err != nil {
			t.Fatalf("AdoptKey: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("AdoptKey did not complete after registry callback resumed")
	}
	if id, _, ok := auth.VerifiedPrincipal(); !ok || id != "account-b" {
		t.Fatalf("verified principal = %q, %t; want account-b", id, ok)
	}
	if _, active := gw.IntegrationGeneration(); !active {
		t.Fatal("new verified principal did not activate a fresh generation")
	}
	newPost, ok := reg.Get("list_my_published_files")
	if !ok || newPost == oldPost {
		t.Fatalf("new principal post overlay = %T present=%t; want a fresh tool", newPost, ok)
	}
	if _, ok := reg.Get("x_new_catalog"); !ok {
		t.Fatal("new principal did not rebuild its integration catalog")
	}
	if _, ok := reg.Get("x_old_catalog"); ok {
		t.Fatal("new principal retained the old integration catalog")
	}
	result, err := newPost.Run(context.Background(), `{}`)
	if err != nil || result.IsError {
		t.Fatalf("new principal post overlay result = %#v, err=%v", result, err)
	}
	if got := uploadCalls.Load(); got != 1 {
		t.Fatalf("new principal post overlay dispatched %d requests, want 1", got)
	}
	if got := newCatalogCalls.Load(); got != 1 {
		t.Fatalf("new principal catalog fetches = %d, want 1", got)
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
