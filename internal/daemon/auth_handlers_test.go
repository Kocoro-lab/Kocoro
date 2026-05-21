package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/config"
	"github.com/Kocoro-lab/ShanClaw/internal/keychain"
)

// handlerFixture builds a Server wired with an AuthManager pointing at a
// fake Cloud, ready to serve /local/auth/*. Reuses the same fake-Cloud
// pattern as auth_test.go (httptest server + handler map).
type handlerFixture struct {
	srv         *Server
	cloud       *httptest.Server
	cloudRoutes map[string]http.HandlerFunc
	manager     *AuthManager
	keychain    *keychain.Store
	cfg         *config.Config
}

func newHandlerFixture(t *testing.T) *handlerFixture {
	hf := &handlerFixture{cloudRoutes: map[string]http.HandlerFunc{}}
	hf.cloud = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Method + " " + r.URL.Path
		h, ok := hf.cloudRoutes[key]
		if !ok {
			t.Fatalf("unexpected cloud call: %s", key)
		}
		h(w, r)
	}))
	t.Cleanup(hf.cloud.Close)

	hf.cfg = &config.Config{}
	hf.keychain = keychain.NewStore(keychain.NewMemBackend(), nil)
	gw := client.NewGatewayClient(hf.cloud.URL, "")
	authClient := client.NewAuthClient(hf.cloud.URL, hf.cloud.Client())
	hf.manager = NewAuthManager(AuthManagerConfig{
		Keychain: hf.keychain,
		Cloud:    authClient,
		Gateway:  gw,
		Cfg:      hf.cfg,
	})

	hf.srv = &Server{
		eventBus: NewEventBus(),
		auth:     hf.manager,
	}
	hf.manager.SetEventBus(hf.srv.eventBus)
	return hf
}

func (hf *handlerFixture) cloudOn(method, path string, status int, body any) {
	hf.cloudRoutes[method+" "+path] = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if body != nil {
			_ = json.NewEncoder(w).Encode(body)
		}
	}
}

func (hf *handlerFixture) cloudErr(method, path string, status int, code string) {
	hf.cloudOn(method, path, status, map[string]string{"error": code, "message": ""})
}

func (hf *handlerFixture) do(t *testing.T, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		buf, _ := json.Marshal(body)
		reader = bytes.NewReader(buf)
	}
	var req *http.Request
	if reader != nil {
		req = httptest.NewRequest(method, path, reader)
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	w := httptest.NewRecorder()
	// Dispatch by path — we only register the auth handlers in the test.
	mux := http.NewServeMux()
	mux.HandleFunc("GET /local/auth/state", hf.srv.handleAuthState)
	mux.HandleFunc("POST /local/auth/register", hf.srv.handleAuthRegister)
	mux.HandleFunc("POST /local/auth/login", hf.srv.handleAuthLogin)
	mux.HandleFunc("POST /local/auth/resend-verification", hf.srv.handleAuthResendVerification)
	mux.HandleFunc("POST /local/auth/forgot-password", hf.srv.handleAuthForgotPassword)
	mux.HandleFunc("POST /local/auth/sign-out", hf.srv.handleAuthSignOut)
	mux.HandleFunc("POST /local/auth/sign-out-full", hf.srv.handleAuthSignOutFull)
	mux.ServeHTTP(w, req)
	return w
}

// --- Tests ---

func TestHandleAuthState_DefaultSignedOut(t *testing.T) {
	hf := newHandlerFixture(t)
	w := hf.do(t, "GET", "/local/auth/state", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
	var snap authSnapshot
	_ = json.Unmarshal(w.Body.Bytes(), &snap)
	if snap.State != AuthStateSignedOut {
		t.Fatalf("state=%q", snap.State)
	}
}

func TestHandleAuthRegister_202(t *testing.T) {
	hf := newHandlerFixture(t)
	hf.cloudOn("POST", "/api/v1/auth/register", http.StatusAccepted, nil)

	w := hf.do(t, "POST", "/local/auth/register", map[string]string{
		"email": "alice@example.com", "password": "pw1", "name": "Alice",
	})
	if w.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var snap authSnapshot
	_ = json.Unmarshal(w.Body.Bytes(), &snap)
	if snap.State != AuthStatePendingVerification {
		t.Fatalf("state=%q", snap.State)
	}
}

func TestHandleAuthRegister_PassthroughCloudError(t *testing.T) {
	hf := newHandlerFixture(t)
	hf.cloudErr("POST", "/api/v1/auth/register", http.StatusConflict, "email_taken")

	w := hf.do(t, "POST", "/local/auth/register", map[string]string{
		"email": "alice@example.com", "password": "pw1",
	})
	if w.Code != http.StatusConflict {
		t.Fatalf("status=%d, expected passthrough 409", w.Code)
	}
	var body map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body["error"] != "email_taken" {
		t.Fatalf("error=%q", body["error"])
	}
}

func TestHandleAuthLogin_200(t *testing.T) {
	hf := newHandlerFixture(t)
	hf.cloudOn("POST", "/api/v1/auth/login", http.StatusOK, client.AuthLoginResponse{
		AccessToken: "at",
		User:        client.AuthUser{ID: "user-1", Email: "alice@example.com"},
	})
	hf.cloudOn("POST", "/api/v1/auth/api-keys", http.StatusOK, client.AuthAPIKeyResponse{APIKey: "sk_new"})

	w := hf.do(t, "POST", "/local/auth/login", map[string]string{
		"email": "alice@example.com", "password": "pw1",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var snap authSnapshot
	_ = json.Unmarshal(w.Body.Bytes(), &snap)
	if snap.State != AuthStateSignedIn {
		t.Fatalf("state=%q", snap.State)
	}
	if hf.cfg.APIKey != "sk_new" {
		t.Fatalf("cfg.APIKey=%q", hf.cfg.APIKey)
	}
}

func TestHandleAuthLogin_403_NotVerified(t *testing.T) {
	hf := newHandlerFixture(t)
	hf.cloudErr("POST", "/api/v1/auth/login", http.StatusForbidden, "email_not_verified")

	w := hf.do(t, "POST", "/local/auth/login", map[string]string{
		"email": "a@b.c", "password": "pw1",
	})
	if w.Code != http.StatusForbidden {
		t.Fatalf("status=%d", w.Code)
	}
	var body map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body["error"] != "email_not_verified" {
		t.Fatalf("error=%q", body["error"])
	}
}

func TestHandleAuthLogin_401_InvalidCreds(t *testing.T) {
	hf := newHandlerFixture(t)
	hf.cloudErr("POST", "/api/v1/auth/login", http.StatusUnauthorized, "invalid_credentials")

	w := hf.do(t, "POST", "/local/auth/login", map[string]string{
		"email": "a@b.c", "password": "wrong",
	})
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d", w.Code)
	}
}

func TestHandleAuthResendVerification_UsesPending(t *testing.T) {
	hf := newHandlerFixture(t)
	hf.cloudOn("POST", "/api/v1/auth/register", http.StatusAccepted, nil)
	hf.do(t, "POST", "/local/auth/register", map[string]string{
		"email": "alice@example.com", "password": "pw1",
	})

	var receivedEmail string
	hf.cloudRoutes["POST /api/v1/auth/resend-verification"] = func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		receivedEmail = body["email"]
		w.WriteHeader(http.StatusOK)
	}

	w := hf.do(t, "POST", "/local/auth/resend-verification", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if receivedEmail != "alice@example.com" {
		t.Fatalf("receivedEmail=%q", receivedEmail)
	}
}

func TestHandleAuthForgotPassword_AlwaysOK(t *testing.T) {
	hf := newHandlerFixture(t)
	hf.cloudOn("POST", "/api/v1/auth/forgot-password", http.StatusOK, nil)
	w := hf.do(t, "POST", "/local/auth/forgot-password", map[string]string{"email": "a@b.c"})
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
}

func TestHandleAuthSignOut_PreservesKeychain(t *testing.T) {
	hf := newHandlerFixture(t)
	_ = hf.keychain.SetAPIKey("user-1", "sk_test")
	hf.manager.setState(AuthStateSignedIn, &client.AuthUser{ID: "user-1"}, "")

	w := hf.do(t, "POST", "/local/auth/sign-out", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
	if k, _ := hf.keychain.GetAPIKey(); k != "sk_test" {
		t.Fatalf("sign-out should preserve keychain key, got %q", k)
	}
}

func TestHandleAuthSignOutFull_ClearsKeychain(t *testing.T) {
	hf := newHandlerFixture(t)
	_ = hf.keychain.SetAPIKey("user-1", "sk_test")
	hf.manager.setState(AuthStateSignedIn, &client.AuthUser{ID: "user-1"}, "")

	w := hf.do(t, "POST", "/local/auth/sign-out-full", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
	if k, _ := hf.keychain.GetAPIKey(); k != "" {
		t.Fatalf("sign-out-full should clear keychain key, got %q", k)
	}
}

func TestHandleAuthState_PlatformUnsupported(t *testing.T) {
	srv := &Server{eventBus: NewEventBus(), auth: nil}
	req := httptest.NewRequest("GET", "/local/auth/state", nil)
	w := httptest.NewRecorder()
	srv.handleAuthState(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d", w.Code)
	}
	var body map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body["error"] != "platform_unsupported" {
		t.Fatalf("error=%q", body["error"])
	}
}

func TestHandleAuthRegister_InvalidJSON(t *testing.T) {
	hf := newHandlerFixture(t)
	req := httptest.NewRequest("POST", "/local/auth/register", strings.NewReader("not json"))
	w := httptest.NewRecorder()
	hf.srv.handleAuthRegister(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d", w.Code)
	}
}

// /events SSE should emit auth_state_changed; quick smoke (subscribe via
// bus, trigger a transition, ensure we see the event).
func TestHandleAuthLogin_EmitsAuthStateChanged(t *testing.T) {
	hf := newHandlerFixture(t)
	hf.cloudOn("POST", "/api/v1/auth/login", http.StatusOK, client.AuthLoginResponse{
		AccessToken: "at",
		User:        client.AuthUser{ID: "user-1", Email: "a@b.c"},
	})
	hf.cloudOn("POST", "/api/v1/auth/api-keys", http.StatusOK, client.AuthAPIKeyResponse{APIKey: "sk_new"})

	ch := hf.srv.eventBus.Subscribe()
	defer hf.srv.eventBus.Unsubscribe(ch)

	go func() {
		_ = hf.manager.Login(context.Background(), "a@b.c", "pw1")
	}()

	// Expect a series of auth_state_changed events. The terminal one must be signed_in.
	gotSignedIn := false
	for i := 0; i < 8 && !gotSignedIn; i++ {
		evt := <-ch
		if evt.Type != EventAuthStateChanged {
			continue
		}
		var snap authSnapshot
		_ = json.Unmarshal(evt.Payload, &snap)
		if snap.State == AuthStateSignedIn {
			gotSignedIn = true
		}
	}
	if !gotSignedIn {
		t.Fatal("did not observe signed_in transition on event bus")
	}
}
