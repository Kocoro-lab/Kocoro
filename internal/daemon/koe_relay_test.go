package daemon

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/config"
)

func TestHandleKoeRealtimeUsageQueuesWithoutWaitingForGateway(t *testing.T) {
	dir := t.TempDir()
	deps := &ServerDeps{
		Config:     &config.Config{Endpoint: "https://cloud.example", APIKey: "test-key", Cloud: config.CloudConfig{Enabled: true}},
		ShannonDir: dir,
		GW:         nil,
	}
	s := NewServer(0, nil, deps, "test")
	body := realtimeUsageFixture("response-queued")
	principal, ok := s.realtimeUsagePrincipal()
	if !ok {
		t.Fatal("legacy fallback principal unavailable")
	}
	req := httptest.NewRequest(http.MethodPost, "/koe/realtime/usage", bytes.NewReader(body))
	req.Header.Set(client.RealtimeUsagePrincipalHeader, principal)
	res := httptest.NewRecorder()
	s.handleKoeRealtimeUsage(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s; want 200 durable handoff", res.Code, res.Body.String())
	}
	var status map[string]string
	if err := json.Unmarshal(res.Body.Bytes(), &status); err != nil {
		t.Fatalf("response: %v", err)
	}
	if status["status"] != "queued" {
		t.Fatalf("status body = %#v, want queued", status)
	}
	paths, err := s.realtimeUsageOutboxStore().pendingPaths(principal)
	if err != nil || len(paths) != 1 {
		t.Fatalf("queued paths = %v, err=%v; want one", paths, err)
	}
	if info, err := os.Stat(paths[0]); err != nil {
		t.Fatalf("stat queued entry: %v", err)
	} else if info.Mode().Perm() != 0o600 {
		t.Fatalf("queued entry mode = %o, want 600", info.Mode().Perm())
	}
	if info, err := os.Stat(filepath.Dir(paths[0])); err != nil {
		t.Fatalf("stat principal outbox: %v", err)
	} else if info.Mode().Perm() != 0o700 {
		t.Fatalf("principal outbox mode = %o, want 700", info.Mode().Perm())
	}
	if info, err := os.Stat(filepath.Dir(filepath.Dir(paths[0]))); err != nil {
		t.Fatalf("stat realtime usage outbox: %v", err)
	} else if info.Mode().Perm() != 0o700 {
		t.Fatalf("realtime usage outbox mode = %o, want 700", info.Mode().Perm())
	}
}

func TestHandleKoeRealtimeUsageUsesCurrentPrincipalForLegacyClient(t *testing.T) {
	deps := &ServerDeps{
		Config:     &config.Config{Endpoint: "https://cloud.example", APIKey: "test-key", Cloud: config.CloudConfig{Enabled: true}},
		ShannonDir: t.TempDir(),
	}
	s := NewServer(0, nil, deps, "test")
	req := httptest.NewRequest(http.MethodPost, "/koe/realtime/usage", bytes.NewReader(realtimeUsageFixture("response-no-principal")))
	res := httptest.NewRecorder()
	s.handleKoeRealtimeUsage(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s; want durable legacy handoff", res.Code, res.Body.String())
	}
	principal, ok := s.realtimeUsagePrincipal()
	if !ok {
		t.Fatal("current principal unavailable")
	}
	paths, err := s.realtimeUsageOutboxStore().pendingPaths(principal)
	if err != nil || len(paths) != 1 {
		t.Fatalf("legacy request queued paths=%v err=%v; want one", paths, err)
	}
}

func TestHandleKoeRealtimeUsageAcceptsMissingPrincipalForAuthenticatedClient(t *testing.T) {
	gw := client.NewGatewayClient("https://cloud.example", "test-key")
	deps := &ServerDeps{
		Config:     &config.Config{Endpoint: "https://cloud.example", APIKey: "test-key", Cloud: config.CloudConfig{Enabled: true}},
		ShannonDir: t.TempDir(),
		GW:         gw,
	}
	s := NewServer(0, nil, deps, "test")
	auth := NewAuthManager(AuthManagerConfig{Gateway: gw})
	auth.setState(AuthStateSignedIn, &client.AuthUser{ID: "account-a"}, "")
	s.SetAuth(auth)

	req := httptest.NewRequest(http.MethodPost, "/koe/realtime/usage", bytes.NewReader(realtimeUsageFixture("response-auth-legacy")))
	res := httptest.NewRecorder()
	s.handleKoeRealtimeUsage(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s; want legacy durable handoff", res.Code, res.Body.String())
	}
	principal, ok := s.realtimeUsagePrincipal()
	if !ok {
		t.Fatal("verified account principal unavailable")
	}
	paths, err := s.realtimeUsageOutboxStore().pendingPaths(principal)
	if err != nil || len(paths) != 1 {
		t.Fatalf("authenticated legacy request queued paths=%v err=%v; want one", paths, err)
	}
}

func TestHandleKoeRealtimeUsageMissingPrincipalStaysFailClosedAcrossAccountSwitch(t *testing.T) {
	gw := client.NewGatewayClient("https://cloud.example", "test-key")
	deps := &ServerDeps{
		Config:     &config.Config{Endpoint: "https://cloud.example", APIKey: "test-key", Cloud: config.CloudConfig{Enabled: true}},
		ShannonDir: t.TempDir(),
		GW:         gw,
	}
	s := NewServer(0, nil, deps, "test")
	auth := NewAuthManager(AuthManagerConfig{Gateway: gw})
	s.SetAuth(auth)

	auth.setState(AuthStateSignedIn, &client.AuthUser{ID: "account-a"}, "")
	req := httptest.NewRequest(http.MethodPost, "/koe/realtime/usage", bytes.NewReader(realtimeUsageFixture("response-account-a")))
	res := httptest.NewRecorder()
	s.handleKoeRealtimeUsage(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("account-a status = %d, body=%s; want legacy durable handoff", res.Code, res.Body.String())
	}

	auth.setState(AuthStateSignedIn, &client.AuthUser{ID: "account-b"}, "")
	req = httptest.NewRequest(http.MethodPost, "/koe/realtime/usage", bytes.NewReader(realtimeUsageFixture("response-account-b")))
	res = httptest.NewRecorder()
	s.handleKoeRealtimeUsage(res, req)
	if res.Code != http.StatusServiceUnavailable {
		t.Fatalf("account-b status = %d, body=%s; want fail-closed 503", res.Code, res.Body.String())
	}
	for _, account := range []string{"account-a", "account-b"} {
		principal := realtimeUsagePrincipalFingerprint("account", account)
		paths, err := s.realtimeUsageOutboxStore().pendingPaths(principal)
		want := 0
		if account == "account-a" {
			want = 1
		}
		if err != nil || len(paths) != want {
			t.Fatalf("account %s queued paths=%v err=%v; want %d", account, paths, err, want)
		}
	}
}

func TestHandleKoeRealtimeUsageKeepsExplicitPrincipalAcrossAccountSwitch(t *testing.T) {
	gw := client.NewGatewayClient("https://cloud.example", "test-key")
	deps := &ServerDeps{
		Config:     &config.Config{Endpoint: "https://cloud.example", APIKey: "test-key", Cloud: config.CloudConfig{Enabled: true}},
		ShannonDir: t.TempDir(),
		GW:         gw,
	}
	s := NewServer(0, nil, deps, "test")
	auth := NewAuthManager(AuthManagerConfig{Gateway: gw})
	s.SetAuth(auth)
	auth.setState(AuthStateSignedIn, &client.AuthUser{ID: "account-a"}, "")
	principalA := realtimeUsagePrincipalFingerprint("account", "account-a")
	auth.setState(AuthStateSignedIn, &client.AuthUser{ID: "account-b"}, "")

	req := httptest.NewRequest(http.MethodPost, "/koe/realtime/usage", bytes.NewReader(realtimeUsageFixture("response-explicit-a")))
	req.Header.Set(client.RealtimeUsagePrincipalHeader, principalA)
	res := httptest.NewRecorder()
	s.handleKoeRealtimeUsage(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s; want explicit principal accepted", res.Code, res.Body.String())
	}
	paths, err := s.realtimeUsageOutboxStore().pendingPaths(principalA)
	if err != nil || len(paths) != 1 {
		t.Fatalf("old account paths=%v err=%v; want one", paths, err)
	}
	principalB := realtimeUsagePrincipalFingerprint("account", "account-b")
	paths, err = s.realtimeUsageOutboxStore().pendingPaths(principalB)
	if err != nil || len(paths) != 0 {
		t.Fatalf("new account paths=%v err=%v; want none", paths, err)
	}
}

func TestHandleKoeRealtimeUsageRejectsMalformedSessionPrincipal(t *testing.T) {
	deps := &ServerDeps{
		Config:     &config.Config{Endpoint: "https://cloud.example", APIKey: "test-key", Cloud: config.CloudConfig{Enabled: true}},
		ShannonDir: t.TempDir(),
	}
	s := NewServer(0, nil, deps, "test")
	req := httptest.NewRequest(http.MethodPost, "/koe/realtime/usage", bytes.NewReader(realtimeUsageFixture("response-bad-principal")))
	req.Header.Set(client.RealtimeUsagePrincipalHeader, "not-a-principal")
	res := httptest.NewRecorder()
	s.handleKoeRealtimeUsage(res, req)
	if res.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body=%s; want 400", res.Code, res.Body.String())
	}
}

func TestAddRealtimeUsagePrincipalKeepsBootstrapSecretShapeAndAddsOpaqueBinding(t *testing.T) {
	raw, err := addRealtimeUsagePrincipal([]byte(`{"value":"ephemeral","expires_at":123}`), testRealtimePrincipalA)
	if err != nil {
		t.Fatalf("add principal: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["value"] != "ephemeral" || body["usage_principal"] != testRealtimePrincipalA {
		t.Fatalf("bootstrap response = %#v", body)
	}
	if _, err := addRealtimeUsagePrincipal([]byte("null"), testRealtimePrincipalA); err == nil {
		t.Fatal("null bootstrap response must be rejected")
	}
}

func TestRealtimeBootstrapLeaseBindsCredentialAndPrincipalGeneration(t *testing.T) {
	gw := client.NewGatewayClient("https://cloud.example", "key-a")
	gw.BindIntegrationPrincipal("account-a", 7)
	s := &Server{
		deps: &ServerDeps{
			GW:     gw,
			Config: &config.Config{Endpoint: "https://cloud.example", Cloud: config.CloudConfig{Enabled: true}},
		},
		auth: &AuthManager{},
	}
	var callbackPrincipal, callbackKey string
	body, principal, err := s.withRealtimeUsageGatewayLease(gw, func(bound string) (json.RawMessage, error) {
		callbackPrincipal = bound
		callbackKey = gw.APIKey()
		return json.RawMessage(`{"value":"ephemeral"}`), nil
	})
	if err != nil {
		t.Fatalf("lease: %v", err)
	}
	if string(body) != `{"value":"ephemeral"}` {
		t.Fatalf("body = %s", body)
	}
	want := realtimeUsagePrincipalFingerprint("account", "account-a")
	if principal != want || callbackPrincipal != want {
		t.Fatalf("principal = %q callback = %q, want %q", principal, callbackPrincipal, want)
	}
	if callbackKey != "key-a" {
		t.Fatalf("callback key = %q, want key-a", callbackKey)
	}
}
