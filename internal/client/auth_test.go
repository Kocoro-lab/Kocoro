package client

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fakeCloud struct {
	t        *testing.T
	server   *httptest.Server
	handlers map[string]http.HandlerFunc
	lastReq  *http.Request
	lastBody []byte
}

func newFakeCloud(t *testing.T) *fakeCloud {
	fc := &fakeCloud{t: t, handlers: map[string]http.HandlerFunc{}}
	fc.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fc.lastReq = r
		body, _ := io.ReadAll(r.Body)
		fc.lastBody = body
		r.Body = io.NopCloser(strings.NewReader(string(body)))
		key := r.Method + " " + r.URL.Path
		h, ok := fc.handlers[key]
		if !ok {
			t.Fatalf("unexpected request: %s", key)
		}
		h(w, r)
	}))
	t.Cleanup(fc.server.Close)
	return fc
}

func (fc *fakeCloud) on(method, path string, status int, body any) {
	fc.handlers[method+" "+path] = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if body != nil {
			_ = json.NewEncoder(w).Encode(body)
		}
	}
}

func (fc *fakeCloud) client() *AuthClient {
	return NewAuthClient(fc.server.URL, fc.server.Client())
}

// --- Register ---

func TestAuthClient_Register_202(t *testing.T) {
	fc := newFakeCloud(t)
	fc.on(http.MethodPost, "/api/v1/auth/register", http.StatusAccepted, map[string]any{
		"verification_sent": true,
	})
	c := fc.client()
	err := c.Register(context.Background(), AuthRegisterRequest{
		Provider: "email", Email: "a@b.c", Username: "alice", Password: "pw1", FullName: "Alice",
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	var sent map[string]any
	_ = json.Unmarshal(fc.lastBody, &sent)
	if sent["email"] != "a@b.c" {
		t.Fatalf("sent body email=%v", sent["email"])
	}
	// Pin the wire contract: provider/full_name/username are the canonical
	// Cloud field names — drift here is the bug that started this fix.
	if sent["provider"] != "email" {
		t.Fatalf("sent body provider=%v, want \"email\"", sent["provider"])
	}
	if sent["full_name"] != "Alice" {
		t.Fatalf("sent body full_name=%v, want \"Alice\"", sent["full_name"])
	}
	if sent["username"] != "alice" {
		t.Fatalf("sent body username=%v, want \"alice\"", sent["username"])
	}
}

func TestAuthClient_Register_409(t *testing.T) {
	fc := newFakeCloud(t)
	fc.on(http.MethodPost, "/api/v1/auth/register", http.StatusConflict, map[string]string{
		"error": "email_taken", "message": "Email already registered",
	})
	err := fc.client().Register(context.Background(), AuthRegisterRequest{
		Email: "a@b.c", Password: "pw1",
	})
	ae, ok := IsAuthError(err)
	if !ok {
		t.Fatalf("expected AuthError, got %T %v", err, err)
	}
	if ae.HTTPCode != http.StatusConflict {
		t.Fatalf("HTTPCode=%d", ae.HTTPCode)
	}
	if ae.Code != "email_taken" {
		t.Fatalf("Code=%q", ae.Code)
	}
	if !strings.Contains(ae.Error(), "email_taken") {
		t.Fatalf("Error msg=%q", ae.Error())
	}
}

// --- Login ---

func TestAuthClient_Login_200(t *testing.T) {
	fc := newFakeCloud(t)
	fc.on(http.MethodPost, "/api/v1/auth/login", http.StatusOK, AuthLoginResponse{
		AccessToken:  "at",
		RefreshToken: "rt",
		ExpiresIn:    3600,
		User: AuthUser{
			ID: "u1", Email: "a@b.c", EmailVerified: true, Name: "Alice",
		},
	})
	resp, err := fc.client().Login(context.Background(), AuthLoginRequest{
		Email: "a@b.c", Password: "pw1",
	})
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if resp.AccessToken != "at" || resp.RefreshToken != "rt" {
		t.Fatalf("tokens not parsed: %+v", resp)
	}
	if resp.User.ID != "u1" {
		t.Fatalf("user not parsed: %+v", resp.User)
	}
}

func TestAuthClient_Login_401_InvalidCreds(t *testing.T) {
	fc := newFakeCloud(t)
	fc.on(http.MethodPost, "/api/v1/auth/login", http.StatusUnauthorized, map[string]string{
		"error": "invalid_credentials", "message": "Wrong email or password",
	})
	_, err := fc.client().Login(context.Background(), AuthLoginRequest{Email: "a@b.c", Password: "x"})
	ae, ok := IsAuthError(err)
	if !ok {
		t.Fatalf("expected AuthError, got %v", err)
	}
	if ae.Code != "invalid_credentials" {
		t.Fatalf("Code=%q", ae.Code)
	}
}

func TestAuthClient_Login_403_NotVerified(t *testing.T) {
	fc := newFakeCloud(t)
	fc.on(http.MethodPost, "/api/v1/auth/login", http.StatusForbidden, map[string]string{
		"error": "email_not_verified", "message": "Please verify your email",
	})
	_, err := fc.client().Login(context.Background(), AuthLoginRequest{Email: "a@b.c", Password: "pw1"})
	ae, ok := IsAuthError(err)
	if !ok {
		t.Fatalf("expected AuthError, got %v", err)
	}
	if ae.Code != "email_not_verified" || ae.HTTPCode != http.StatusForbidden {
		t.Fatalf("got %+v", ae)
	}
}

// --- Verify / resend / forgot / reset ---

func TestAuthClient_VerifyEmail_200(t *testing.T) {
	fc := newFakeCloud(t)
	fc.on(http.MethodPost, "/api/v1/auth/verify-email", http.StatusOK, map[string]bool{"ok": true})
	if err := fc.client().VerifyEmail(context.Background(), "tok"); err != nil {
		t.Fatalf("VerifyEmail: %v", err)
	}
}

func TestAuthClient_ResendVerification_200(t *testing.T) {
	fc := newFakeCloud(t)
	fc.on(http.MethodPost, "/api/v1/auth/resend-verification", http.StatusOK, map[string]bool{"sent": true})
	if err := fc.client().ResendVerification(context.Background(), "a@b.c", ""); err != nil {
		t.Fatalf("ResendVerification: %v", err)
	}
}

func TestAuthClient_ForgotPassword_200(t *testing.T) {
	fc := newFakeCloud(t)
	fc.on(http.MethodPost, "/api/v1/auth/forgot-password", http.StatusOK, map[string]bool{"sent": true})
	if err := fc.client().ForgotPassword(context.Background(), "a@b.c", ""); err != nil {
		t.Fatalf("ForgotPassword: %v", err)
	}
}

func TestAuthClient_ResetPassword_200(t *testing.T) {
	fc := newFakeCloud(t)
	fc.on(http.MethodPost, "/api/v1/auth/reset-password", http.StatusOK, map[string]bool{"ok": true})
	if err := fc.client().ResetPassword(context.Background(), "tok", "newpw"); err != nil {
		t.Fatalf("ResetPassword: %v", err)
	}
}

// --- Refresh ---

func TestAuthClient_Refresh_200(t *testing.T) {
	fc := newFakeCloud(t)
	fc.on(http.MethodPost, "/api/v1/auth/refresh", http.StatusOK, AuthRefreshResponse{
		AccessToken: "at2", RefreshToken: "rt2", ExpiresIn: 3600,
	})
	resp, err := fc.client().Refresh(context.Background(), "rt")
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if resp.AccessToken != "at2" {
		t.Fatalf("got %+v", resp)
	}
}

func TestAuthClient_Refresh_401(t *testing.T) {
	fc := newFakeCloud(t)
	fc.on(http.MethodPost, "/api/v1/auth/refresh", http.StatusUnauthorized, map[string]string{
		"error": "invalid_refresh_token",
	})
	_, err := fc.client().Refresh(context.Background(), "rt")
	ae, ok := IsAuthError(err)
	if !ok {
		t.Fatalf("expected AuthError, got %v", err)
	}
	if ae.Code != "invalid_refresh_token" {
		t.Fatalf("Code=%q", ae.Code)
	}
}

// --- Me ---

func TestAuthClient_Me_BearerJWT(t *testing.T) {
	fc := newFakeCloud(t)
	fc.on(http.MethodGet, "/api/v1/auth/me", http.StatusOK, map[string]any{
		"id": "u1", "email": "a@b.c", "email_verified": true, "name": "Alice", "tier": "free",
	})
	user, err := fc.client().Me(context.Background(), "jwt-token")
	if err != nil {
		t.Fatalf("Me: %v", err)
	}
	if user.ID != "u1" || user.Email != "a@b.c" {
		t.Fatalf("got %+v", user)
	}
	if got := fc.lastReq.Header.Get("Authorization"); got != "Bearer jwt-token" {
		t.Fatalf("Authorization header=%q", got)
	}
}

func TestAuthClient_Me_NestedUserShape(t *testing.T) {
	fc := newFakeCloud(t)
	fc.on(http.MethodGet, "/api/v1/auth/me", http.StatusOK, map[string]any{
		"user": map[string]any{"id": "u2", "email": "b@c.d"},
	})
	user, err := fc.client().Me(context.Background(), "jwt")
	if err != nil {
		t.Fatalf("Me: %v", err)
	}
	if user.ID != "u2" {
		t.Fatalf("nested user shape not handled: %+v", user)
	}
}

func TestAuthClient_MeWithAPIKey_XAPIKey(t *testing.T) {
	fc := newFakeCloud(t)
	fc.on(http.MethodGet, "/api/v1/auth/me", http.StatusOK, map[string]any{
		"id": "u1", "email": "a@b.c",
	})
	_, err := fc.client().MeWithAPIKey(context.Background(), "sk_abc")
	if err != nil {
		t.Fatalf("MeWithAPIKey: %v", err)
	}
	if got := fc.lastReq.Header.Get("X-API-Key"); got != "sk_abc" {
		t.Fatalf("X-API-Key=%q", got)
	}
	if fc.lastReq.Header.Get("Authorization") != "" {
		t.Fatalf("Authorization should be empty when using api key")
	}
}

// TestAuthClient_MeWithAPIKey_RealCloudShape pins the authoritative Cloud
// wire shape (shannon-cloud MeResponse): the canonical id is the top-level
// `user_id` (NOT `id`) and the plan is the top-level `tier`, with no nested
// `user` object. The prior decoder only read `user.id` / `id`, so against
// this real body it returned an empty ID with a populated tier — the
// production "signed-in, max tier, empty user_id" bug. This test decodes a
// producer-accurate body so the regression cannot reappear silently.
func TestAuthClient_MeWithAPIKey_RealCloudShape(t *testing.T) {
	fc := newFakeCloud(t)
	fc.on(http.MethodGet, "/api/v1/auth/me", http.StatusOK, map[string]any{
		"user_id":   "real-user-uuid",
		"tenant_id": "tenant-1",
		"email":     "max@example.com",
		"username":  "max",
		"tier":      "max",
	})
	user, err := fc.client().MeWithAPIKey(context.Background(), "sk_abc")
	if err != nil {
		t.Fatalf("MeWithAPIKey: %v", err)
	}
	if user.ID != "real-user-uuid" {
		t.Fatalf("user_id not mapped to ID: %+v", user)
	}
	if user.Tier != "max" {
		t.Fatalf("top-level tier not mapped: %+v", user)
	}
}

func TestAuthClient_MeWithAPIKey_401(t *testing.T) {
	fc := newFakeCloud(t)
	fc.on(http.MethodGet, "/api/v1/auth/me", http.StatusUnauthorized, map[string]string{
		"error": "invalid_api_key", "message": "API key revoked",
	})
	_, err := fc.client().MeWithAPIKey(context.Background(), "sk_stale")
	ae, ok := IsAuthError(err)
	if !ok {
		t.Fatalf("expected AuthError, got %v", err)
	}
	if ae.Code != "invalid_api_key" {
		t.Fatalf("Code=%q", ae.Code)
	}
}

// --- CreateAPIKey ---

func TestAuthClient_CreateAPIKey_200(t *testing.T) {
	fc := newFakeCloud(t)
	fc.on(http.MethodPost, "/api/v1/auth/api-keys", http.StatusOK, AuthAPIKeyResponse{
		APIKey: "sk_new", KeyID: "kid-1",
	})
	resp, err := fc.client().CreateAPIKey(context.Background(), "jwt", "kocoro-daemon")
	if err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}
	if resp.APIKey != "sk_new" {
		t.Fatalf("got %+v", resp)
	}
	if got := fc.lastReq.Header.Get("Authorization"); got != "Bearer jwt" {
		t.Fatalf("Authorization=%q", got)
	}
	var sent map[string]string
	_ = json.Unmarshal(fc.lastBody, &sent)
	if sent["name"] != "kocoro-daemon" {
		t.Fatalf("sent body name=%q", sent["name"])
	}
}

// --- Error envelope fallbacks ---

func TestAuthClient_ErrorFallback_NoEnvelope(t *testing.T) {
	fc := newFakeCloud(t)
	fc.handlers[http.MethodPost+" /api/v1/auth/login"] = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("internal err — not json"))
	}
	_, err := fc.client().Login(context.Background(), AuthLoginRequest{Email: "a@b.c", Password: "p"})
	ae, ok := IsAuthError(err)
	if !ok {
		t.Fatalf("expected AuthError fallback, got %v", err)
	}
	if ae.Code != "server_error" {
		t.Fatalf("expected server_error fallback, got %q", ae.Code)
	}
	if !strings.Contains(string(ae.Raw), "not json") {
		t.Fatalf("Raw lost: %q", string(ae.Raw))
	}
}

// Regression: Cloud returns 404 (often an HTML "not found" page from the
// router, not the {error, message} envelope) when an auth endpoint is
// not deployed. Daemon must surface a *distinct* code so Desktop does
// not confuse it with "bad request" or assume the daemon-side route is
// missing. This was the 2026-05-20 confusion that landed in Desktop's
// Bug 1 report.
func TestAuthClient_ErrorFallback_404_CloudEndpointMissing(t *testing.T) {
	fc := newFakeCloud(t)
	fc.handlers[http.MethodPost+" /api/v1/auth/login"] = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("<html>404 page not found</html>"))
	}
	_, err := fc.client().Login(context.Background(), AuthLoginRequest{Email: "a@b.c", Password: "p"})
	ae, ok := IsAuthError(err)
	if !ok {
		t.Fatalf("expected AuthError, got %v", err)
	}
	if ae.HTTPCode != http.StatusNotFound {
		t.Fatalf("HTTPCode=%d want 404", ae.HTTPCode)
	}
	if ae.Code != "cloud_endpoint_unavailable" {
		t.Fatalf("Code=%q want cloud_endpoint_unavailable (was bad_request before 2026-05-20 fix)", ae.Code)
	}
	if !strings.Contains(ae.Message, "Cloud") {
		t.Fatalf("Message=%q should mention Cloud (Desktop renders this as the user-visible hint)", ae.Message)
	}
}

// Cloud returns 404 WITH a proper {error,message} envelope — daemon
// preserves whatever Cloud sent verbatim (no fallback override). This
// keeps Cloud the authoritative source of truth when it bothers to
// produce a structured error.
func TestAuthClient_404_WithCloudEnvelope_PassthroughCode(t *testing.T) {
	fc := newFakeCloud(t)
	fc.on(http.MethodPost, "/api/v1/auth/login", http.StatusNotFound, map[string]string{
		"error": "user_not_found", "message": "No such user",
	})
	_, err := fc.client().Login(context.Background(), AuthLoginRequest{Email: "a@b.c", Password: "p"})
	ae, ok := IsAuthError(err)
	if !ok {
		t.Fatalf("expected AuthError, got %v", err)
	}
	if ae.Code != "user_not_found" {
		t.Fatalf("Code=%q want user_not_found (Cloud envelope must take priority over fallback)", ae.Code)
	}
}

// When Cloud sends a valid envelope but with an empty Message field, daemon
// MUST NOT overwrite it with a generated fallback string — Cloud's empty
// is intentional. Only the "no envelope at all" path uses fallbackMessage.
func TestAuthClient_RespectsEmptyCloudMessage(t *testing.T) {
	fc := newFakeCloud(t)
	// 500 with a real Cloud envelope but no message.
	fc.on(http.MethodPost, "/api/v1/auth/login", http.StatusInternalServerError, map[string]string{
		"error": "server_error", "message": "",
	})
	_, err := fc.client().Login(context.Background(), AuthLoginRequest{Email: "a@b.c", Password: "p"})
	ae, ok := IsAuthError(err)
	if !ok {
		t.Fatalf("expected AuthError, got %v", err)
	}
	if ae.Code != "server_error" {
		t.Fatalf("Code=%q want server_error (from Cloud envelope)", ae.Code)
	}
	if ae.Message != "" {
		t.Fatalf("Message=%q — Cloud explicitly returned empty, daemon must not fabricate one", ae.Message)
	}
}

// And conversely: when Cloud sends NO envelope (raw HTML / plain text body),
// daemon DOES fill in a sensible fallback so Desktop UIs are not blank.
func TestAuthClient_FillsFallbackMessage_WhenNoEnvelope(t *testing.T) {
	fc := newFakeCloud(t)
	fc.handlers[http.MethodPost+" /api/v1/auth/login"] = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("ngnix: 500 Internal Server Error"))
	}
	_, err := fc.client().Login(context.Background(), AuthLoginRequest{Email: "a@b.c", Password: "p"})
	ae, ok := IsAuthError(err)
	if !ok {
		t.Fatalf("expected AuthError, got %v", err)
	}
	if ae.Message == "" {
		t.Fatal("Message should be filled with fallback when Cloud envelope is absent")
	}
}

func TestAuthClient_ErrorFallback_429(t *testing.T) {
	fc := newFakeCloud(t)
	fc.on(http.MethodPost, "/api/v1/auth/login", http.StatusTooManyRequests, nil)
	_, err := fc.client().Login(context.Background(), AuthLoginRequest{Email: "a@b.c", Password: "p"})
	ae, ok := IsAuthError(err)
	if !ok {
		t.Fatalf("expected AuthError, got %v", err)
	}
	if ae.Code != "rate_limited" {
		t.Fatalf("Code=%q", ae.Code)
	}
}

// --- Transport errors are not AuthErrors ---

func TestAuthClient_TransportError_NotAuthError(t *testing.T) {
	c := NewAuthClient("http://127.0.0.1:1", nil) // unreachable
	err := c.Register(context.Background(), AuthRegisterRequest{Email: "a@b.c", Password: "p"})
	if err == nil {
		t.Fatal("expected error")
	}
	var ae *AuthError
	if errors.As(err, &ae) {
		t.Fatalf("transport error should NOT be AuthError, got %v", ae)
	}
}
