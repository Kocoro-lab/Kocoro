package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/config"
	"github.com/Kocoro-lab/ShanClaw/internal/daemon"
	"github.com/Kocoro-lab/ShanClaw/internal/keychain"
)

// fakeCloud is a minimal httptest server that AnswerRoute-style responds
// to /api/v1/auth/* — covering register, login, api-keys, me, resend,
// forgot-password. Tests configure responses per test before issuing the
// HTTP request to the daemon-side mux.
type fakeCloud struct {
	server *httptest.Server
	routes map[string]http.HandlerFunc
	mu     sync.Mutex
	calls  []string // method+path log
}

func newFakeCloud(t *testing.T) *fakeCloud {
	fc := &fakeCloud{routes: map[string]http.HandlerFunc{}}
	fc.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Method + " " + r.URL.Path
		fc.mu.Lock()
		fc.calls = append(fc.calls, key)
		h, ok := fc.routes[key]
		fc.mu.Unlock()
		if !ok {
			t.Logf("unexpected cloud call %s — returning 500", key)
			http.Error(w, `{"error":"not_found"}`, http.StatusInternalServerError)
			return
		}
		h(w, r)
	}))
	t.Cleanup(fc.server.Close)
	return fc
}

func (fc *fakeCloud) on(method, path string, status int, body any) {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	fc.routes[method+" "+path] = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if body != nil {
			_ = json.NewEncoder(w).Encode(body)
		}
	}
}

func (fc *fakeCloud) onErr(method, path string, status int, code, message string) {
	fc.on(method, path, status, map[string]string{"error": code, "message": message})
}

func (fc *fakeCloud) callCount(key string) int {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	n := 0
	for _, c := range fc.calls {
		if c == key {
			n++
		}
	}
	return n
}

// authMux builds an AuthManager + minimal Server and returns an
// http.Handler wiring /local/auth/* against the fake Cloud. Each test
// gets its own MemBackend, cfg, gw, manager — no cross-test leakage.
type authMux struct {
	mux      *http.ServeMux
	mgr      *daemon.AuthManager
	srv      *daemon.Server
	cfg      *config.Config
	keychain *keychain.Store
	bus      *daemon.EventBus
	gw       *client.GatewayClient
}

func newAuthMux(t *testing.T, fc *fakeCloud) *authMux {
	cfg := &config.Config{}
	kc := keychain.NewStore(keychain.NewMemBackend(), nil)
	gw := client.NewGatewayClient(fc.server.URL, "")
	authClient := client.NewAuthClient(fc.server.URL, fc.server.Client())

	mgr := daemon.NewAuthManager(daemon.AuthManagerConfig{
		Keychain: kc,
		Cloud:    authClient,
		Gateway:  gw,
		Cfg:      cfg,
	})
	bus := daemon.NewEventBus()
	mgr.SetEventBus(bus)

	srv := daemon.NewServer(0, nil, nil, "test")
	srv.SetAuth(mgr)

	mux := http.NewServeMux()
	srv.RegisterAuthRoutes(mux)

	return &authMux{
		mux:      mux,
		mgr:      mgr,
		srv:      srv,
		cfg:      cfg,
		keychain: kc,
		bus:      bus,
		gw:       gw,
	}
}

func (am *authMux) do(t *testing.T, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, rdr)
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	am.mux.ServeHTTP(w, req)
	return w
}

// --- Test scenarios ---

func TestAuthMock_RegisterToLogin_FullFlow(t *testing.T) {
	fc := newFakeCloud(t)
	am := newAuthMux(t, fc)

	// 1) Register → 202 → pending_verification
	fc.on(http.MethodPost, "/api/v1/auth/register", http.StatusAccepted, map[string]bool{"verification_sent": true})
	w := am.do(t, "POST", "/local/auth/register", map[string]string{
		"email": "alice@example.com", "password": "pw1234", "name": "Alice",
	})
	if w.Code != http.StatusAccepted {
		t.Fatalf("register status=%d body=%s", w.Code, w.Body.String())
	}

	// 2) Login while not yet verified → 403 email_not_verified → pending_verification
	fc.onErr(http.MethodPost, "/api/v1/auth/login", http.StatusForbidden,
		"email_not_verified", "Please verify your email")
	w = am.do(t, "POST", "/local/auth/login", map[string]string{
		"email": "alice@example.com", "password": "pw1234",
	})
	if w.Code != http.StatusForbidden {
		t.Fatalf("login should passthrough 403, got %d", w.Code)
	}

	// 3) Resend verification using pending email (empty body)
	fc.on(http.MethodPost, "/api/v1/auth/resend-verification", http.StatusOK, map[string]bool{"sent": true})
	w = am.do(t, "POST", "/local/auth/resend-verification", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("resend status=%d", w.Code)
	}

	// 4) User verifies in browser; daemon-side we now switch Cloud to return 200 for login
	fc.on(http.MethodPost, "/api/v1/auth/login", http.StatusOK, client.AuthLoginResponse{
		AccessToken: "at", RefreshToken: "rt", ExpiresIn: 3600,
		User: client.AuthUser{ID: "user-1", Email: "alice@example.com", EmailVerified: true, Name: "Alice"},
	})
	fc.on(http.MethodPost, "/api/v1/auth/api-keys", http.StatusOK, client.AuthAPIKeyResponse{APIKey: "sk_minted"})

	w = am.do(t, "POST", "/local/auth/login", map[string]string{
		"email": "alice@example.com", "password": "pw1234",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("login status=%d body=%s", w.Code, w.Body.String())
	}
	if am.mgr.State() != daemon.AuthStateSignedIn {
		t.Fatalf("state=%q want signed_in", am.mgr.State())
	}
	if am.gw.APIKey() != "sk_minted" {
		t.Fatalf("gateway api key=%q", am.gw.APIKey())
	}
	if u, k, _ := am.keychain.GetActiveUserAndKey(); u != "user-1" || k != "sk_minted" {
		t.Fatalf("keychain user=%q key=%q", u, k)
	}

	// 5) State endpoint reflects signed_in with user info
	w = am.do(t, "GET", "/local/auth/state", nil)
	var snap struct {
		State string `json:"state"`
		User  struct {
			Email string `json:"email"`
		} `json:"user"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &snap)
	if snap.State != "signed_in" || snap.User.Email != "alice@example.com" {
		t.Fatalf("state body=%s", w.Body.String())
	}

	// 6) Sign-out clears the active session but preserves the per-user key
	w = am.do(t, "POST", "/local/auth/sign-out", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("sign-out status=%d", w.Code)
	}
	if u, _ := am.keychain.CurrentUserID(); u != "" {
		t.Fatalf("sign-out should clear active user, got %q", u)
	}
	if k, _ := am.keychain.GetAPIKey(); k != "" {
		t.Fatalf("sign-out should clear active key, got %q", k)
	}
	if k, _ := am.keychain.Read(keychain.ServiceDaemonAPIKey, "user-1"); k != "sk_minted" {
		t.Fatalf("sign-out should preserve per-user key, got %q", k)
	}
	if am.gw.APIKey() != "" {
		t.Fatalf("sign-out should clear gateway api key, got %q", am.gw.APIKey())
	}

	// 7) Sign back in reuses existing Keychain key (no api-keys call)
	beforeAPIKeysCalls := fc.callCount("POST /api/v1/auth/api-keys")
	w = am.do(t, "POST", "/local/auth/login", map[string]string{
		"email": "alice@example.com", "password": "pw1234",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("re-login status=%d", w.Code)
	}
	if got := fc.callCount("POST /api/v1/auth/api-keys"); got != beforeAPIKeysCalls {
		t.Fatalf("re-login should NOT call /auth/api-keys (key was already in Keychain); got %d calls", got-beforeAPIKeysCalls)
	}

	// 8) Sign-out-full clears Keychain
	w = am.do(t, "POST", "/local/auth/sign-out-full", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("sign-out-full status=%d", w.Code)
	}
	if k, _ := am.keychain.GetAPIKey(); k != "" {
		t.Fatalf("sign-out-full should clear keychain, got %q", k)
	}
}

func TestAuthMock_BootstrapWithStaleKey_ClearsKeychain(t *testing.T) {
	fc := newFakeCloud(t)
	am := newAuthMux(t, fc)
	// Pre-populate Keychain as if from a prior session.
	_ = am.keychain.SetAPIKey("user-1", "sk_stale")

	// /me returns 401 — Bootstrap should clear Keychain.
	fc.onErr(http.MethodGet, "/api/v1/auth/me", http.StatusUnauthorized, "invalid_api_key", "")

	am.mgr.Bootstrap(context.Background())

	if k, _ := am.keychain.GetAPIKey(); k != "" {
		t.Fatalf("stale keychain key should be cleared, got %q", k)
	}
	if am.mgr.State() != daemon.AuthStateSignedOut {
		t.Fatalf("state=%q want signed_out", am.mgr.State())
	}
}

func TestAuthMock_AuthStateChangedEvents_LiveStream(t *testing.T) {
	fc := newFakeCloud(t)
	am := newAuthMux(t, fc)

	ch := am.bus.Subscribe()
	defer am.bus.Unsubscribe(ch)

	fc.on(http.MethodPost, "/api/v1/auth/login", http.StatusOK, client.AuthLoginResponse{
		AccessToken: "at",
		User:        client.AuthUser{ID: "user-1", Email: "a@b.c"},
	})
	fc.on(http.MethodPost, "/api/v1/auth/api-keys", http.StatusOK, client.AuthAPIKeyResponse{APIKey: "sk"})

	go func() {
		am.do(t, "POST", "/local/auth/login", map[string]string{"email": "a@b.c", "password": "pw"})
	}()

	// Expect transitions: logging_in → bootstrapping_key → signed_in
	got := []string{}
	deadline := time.After(2 * time.Second)
	for len(got) < 3 {
		select {
		case evt := <-ch:
			if evt.Type != daemon.EventAuthStateChanged {
				continue
			}
			var payload struct {
				State string `json:"state"`
			}
			_ = json.Unmarshal(evt.Payload, &payload)
			got = append(got, payload.State)
		case <-deadline:
			t.Fatalf("timed out waiting for auth_state_changed events; got %v", got)
		}
	}
	want := []string{"logging_in", "bootstrapping_key", "signed_in"}
	for i, w := range want {
		if got[i] != w {
			t.Fatalf("transition[%d]=%q want %q (got %v)", i, got[i], w, got)
		}
	}
}

func TestAuthMock_PlatformUnsupported(t *testing.T) {
	// Server with nil AuthManager (simulates non-darwin).
	srv := daemon.NewServer(0, nil, nil, "test")
	mux := http.NewServeMux()
	srv.RegisterAuthRoutes(mux)

	req := httptest.NewRequest("GET", "/local/auth/state", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d want 503", w.Code)
	}
	if !strings.Contains(w.Body.String(), "platform_unsupported") {
		t.Fatalf("body=%s", w.Body.String())
	}
}
