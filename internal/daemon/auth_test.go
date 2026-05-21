package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/config"
	"github.com/Kocoro-lab/ShanClaw/internal/keychain"
)

// authFixture builds an AuthManager wired to a fake Cloud (httptest), an
// in-memory keychain, a real (but never-Connected) WS Client, an EventBus
// for assertions, and a *config.Config that auth_test mutates to verify
// cfg.APIKey propagation.
type authFixture struct {
	t            *testing.T
	manager      *AuthManager
	keychain     *keychain.Store
	cfg          *config.Config
	gw           *client.GatewayClient
	bus          *EventBus
	cloudServer  *httptest.Server
	handlers     map[string]http.HandlerFunc
	apiKeyCalls  int
	cb           func()
	cbMu         sync.Mutex
}

func newAuthFixture(t *testing.T) *authFixture {
	f := &authFixture{
		t:        t,
		handlers: map[string]http.HandlerFunc{},
	}
	f.cloudServer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Method + " " + r.URL.Path
		h, ok := f.handlers[key]
		if !ok {
			t.Fatalf("unexpected cloud call: %s", key)
		}
		h(w, r)
	}))
	t.Cleanup(f.cloudServer.Close)

	be := keychain.NewMemBackend()
	f.keychain = keychain.NewStore(be, nil)
	f.cfg = &config.Config{}
	f.gw = client.NewGatewayClient(f.cloudServer.URL, "")
	f.bus = NewEventBus()
	authClient := client.NewAuthClient(f.cloudServer.URL, f.cloudServer.Client())

	f.manager = NewAuthManager(AuthManagerConfig{
		Keychain: f.keychain,
		Cloud:    authClient,
		Gateway:  f.gw,
		WSClient: nil, // no WS in unit tests
		Cfg:      f.cfg,
		OnAPIKeyChanged: func(ctx context.Context) {
			f.cbMu.Lock()
			f.apiKeyCalls++
			cb := f.cb
			f.cbMu.Unlock()
			if cb != nil {
				cb()
			}
		},
	})
	f.manager.SetEventBus(f.bus)
	return f
}

func (f *authFixture) on(method, path string, status int, body any) {
	f.handlers[method+" "+path] = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if body != nil {
			_ = json.NewEncoder(w).Encode(body)
		}
	}
}

func (f *authFixture) onError(method, path string, status int, code, message string) {
	f.on(method, path, status, map[string]string{"error": code, "message": message})
}

// waitEvent blocks until an event of type t is emitted or the channel
// runs dry. Calling this without an active subscription will hang —
// always subscribe before triggering the action.
func (f *authFixture) waitEvent(ch <-chan Event, want string) Event {
	f.t.Helper()
	for {
		select {
		case evt, ok := <-ch:
			if !ok {
				f.t.Fatalf("event channel closed waiting for %q", want)
			}
			if evt.Type == want {
				return evt
			}
		}
	}
}

// --- State machine: initial / register / pending_verification ---

func TestAuthManager_InitialStateIsSignedOut(t *testing.T) {
	f := newAuthFixture(t)
	if s := f.manager.State(); s != AuthStateSignedOut {
		t.Fatalf("initial state=%q want signed_out", s)
	}
	snap := f.manager.Snapshot()
	if snap.User != nil || snap.PendingEmail != "" {
		t.Fatalf("snapshot leaked fields: %+v", snap)
	}
}

func TestAuthManager_Register_202_TransitionsPendingVerification(t *testing.T) {
	f := newAuthFixture(t)
	f.on(http.MethodPost, "/api/v1/auth/register", http.StatusAccepted, map[string]bool{"verification_sent": true})

	ch := f.bus.Subscribe()
	defer f.bus.Unsubscribe(ch)

	err := f.manager.Register(context.Background(), client.AuthRegisterRequest{
		Email: "alice@example.com", Password: "pw1234567", FullName: "Alice",
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if s := f.manager.State(); s != AuthStatePendingVerification {
		t.Fatalf("state=%q want pending_verification", s)
	}
	if snap := f.manager.Snapshot(); snap.PendingEmail != "alice@example.com" {
		t.Fatalf("PendingEmail=%q", snap.PendingEmail)
	}
	evt := f.waitEvent(ch, EventAuthStateChanged)
	var payload authSnapshot
	_ = json.Unmarshal(evt.Payload, &payload)
	if payload.State != AuthStatePendingVerification {
		t.Fatalf("event payload state=%q", payload.State)
	}
}

func TestAuthManager_Register_409_DoesNotTransition(t *testing.T) {
	f := newAuthFixture(t)
	f.onError(http.MethodPost, "/api/v1/auth/register", http.StatusConflict, "email_taken", "")
	err := f.manager.Register(context.Background(), client.AuthRegisterRequest{
		Email: "alice@example.com", Password: "pw1",
	})
	if _, ok := client.IsAuthError(err); !ok {
		t.Fatalf("expected AuthError, got %v", err)
	}
	if s := f.manager.State(); s != AuthStateSignedOut {
		t.Fatalf("state=%q want signed_out", s)
	}
}

// --- Login paths ---

func TestAuthManager_Login_Success_PreexistingKeychainKey(t *testing.T) {
	f := newAuthFixture(t)
	// Keychain already has a key from a prior session.
	_ = f.keychain.SetAPIKey("user-1", "sk_old")

	f.on(http.MethodPost, "/api/v1/auth/login", http.StatusOK, client.AuthLoginResponse{
		AccessToken: "at", RefreshToken: "rt", ExpiresIn: 3600,
		User: client.AuthUser{ID: "user-1", Email: "alice@example.com"},
	})

	ch := f.bus.Subscribe()
	defer f.bus.Unsubscribe(ch)

	if err := f.manager.Login(context.Background(), "alice@example.com", "pw1"); err != nil {
		t.Fatalf("Login: %v", err)
	}
	if s := f.manager.State(); s != AuthStateSignedIn {
		t.Fatalf("state=%q want signed_in", s)
	}
	if f.cfg.APIKey != "sk_old" {
		t.Fatalf("cfg.APIKey=%q want sk_old (preexisting reused)", f.cfg.APIKey)
	}
	if f.apiKeyCalls == 0 {
		t.Fatal("OnAPIKeyChanged not invoked")
	}
}

func TestAuthManager_Login_Success_BootstrapsNewKey(t *testing.T) {
	f := newAuthFixture(t)
	f.on(http.MethodPost, "/api/v1/auth/login", http.StatusOK, client.AuthLoginResponse{
		AccessToken: "at", RefreshToken: "rt",
		User: client.AuthUser{ID: "user-1", Email: "a@b.c"},
	})
	f.on(http.MethodPost, "/api/v1/auth/api-keys", http.StatusOK, client.AuthAPIKeyResponse{
		APIKey: "sk_freshly_minted",
	})

	if err := f.manager.Login(context.Background(), "a@b.c", "pw1"); err != nil {
		t.Fatalf("Login: %v", err)
	}
	if f.cfg.APIKey != "sk_freshly_minted" {
		t.Fatalf("cfg.APIKey=%q", f.cfg.APIKey)
	}
	u, k, _ := f.keychain.GetActiveUserAndKey()
	if u != "user-1" || k != "sk_freshly_minted" {
		t.Fatalf("keychain user=%q key=%q", u, k)
	}
}

func TestAuthManager_Login_403_EmailNotVerified(t *testing.T) {
	f := newAuthFixture(t)
	f.onError(http.MethodPost, "/api/v1/auth/login", http.StatusForbidden,
		"email_not_verified", "Please verify your email")

	if err := f.manager.Login(context.Background(), "alice@example.com", "pw1"); err == nil {
		t.Fatal("expected error")
	}
	if s := f.manager.State(); s != AuthStatePendingVerification {
		t.Fatalf("state=%q want pending_verification", s)
	}
	snap := f.manager.Snapshot()
	if snap.PendingEmail != "alice@example.com" {
		t.Fatalf("PendingEmail=%q", snap.PendingEmail)
	}
	if snap.LastErrorCode != "email_not_verified" {
		t.Fatalf("LastErrorCode=%q", snap.LastErrorCode)
	}
}

func TestAuthManager_Login_401_InvalidCredentials(t *testing.T) {
	f := newAuthFixture(t)
	f.onError(http.MethodPost, "/api/v1/auth/login", http.StatusUnauthorized,
		"invalid_credentials", "")

	if err := f.manager.Login(context.Background(), "alice@example.com", "wrong"); err == nil {
		t.Fatal("expected error")
	}
	if s := f.manager.State(); s != AuthStateSignedOut {
		t.Fatalf("state=%q want signed_out", s)
	}
	if snap := f.manager.Snapshot(); snap.LastErrorCode != "invalid_credentials" {
		t.Fatalf("LastErrorCode=%q", snap.LastErrorCode)
	}
}

func TestAuthManager_Login_APIKeyBootstrapFails(t *testing.T) {
	f := newAuthFixture(t)
	f.on(http.MethodPost, "/api/v1/auth/login", http.StatusOK, client.AuthLoginResponse{
		AccessToken: "at",
		User:        client.AuthUser{ID: "user-1", Email: "a@b.c"},
	})
	f.onError(http.MethodPost, "/api/v1/auth/api-keys", http.StatusInternalServerError,
		"server_error", "")

	err := f.manager.Login(context.Background(), "a@b.c", "pw")
	if err == nil {
		t.Fatal("expected error")
	}
	if s := f.manager.State(); s != AuthStateSignedOut {
		t.Fatalf("state=%q want signed_out", s)
	}
	if snap := f.manager.Snapshot(); snap.LastErrorCode != "api_key_bootstrap_failed" {
		t.Fatalf("LastErrorCode=%q", snap.LastErrorCode)
	}
}

// --- Sign-out paths ---

func TestAuthManager_SignOut_PreservesKeychain(t *testing.T) {
	f := newAuthFixture(t)
	_ = f.keychain.SetAPIKey("user-1", "sk_test")
	f.cfg.APIKey = "sk_test"
	f.manager.setState(AuthStateSignedIn, &client.AuthUser{ID: "user-1", Email: "a@b.c"}, "")

	f.manager.SignOut(context.Background(), false /* clearKeychain */)

	if s := f.manager.State(); s != AuthStateSignedOut {
		t.Fatalf("state=%q", s)
	}
	if f.cfg.APIKey != "" {
		t.Fatalf("cfg.APIKey should be cleared, got %q", f.cfg.APIKey)
	}
	if k, _ := f.keychain.GetAPIKey(); k != "sk_test" {
		t.Fatalf("keychain key should be preserved, got %q", k)
	}
}

func TestAuthManager_SignOut_FullClearsKeychain(t *testing.T) {
	f := newAuthFixture(t)
	_ = f.keychain.SetAPIKey("user-1", "sk_test")
	f.manager.setState(AuthStateSignedIn, &client.AuthUser{ID: "user-1"}, "")

	f.manager.SignOut(context.Background(), true /* clearKeychain */)

	if u, _ := f.keychain.CurrentUserID(); u != "" {
		t.Fatalf("current_user_id should be cleared, got %q", u)
	}
	if k, _ := f.keychain.GetAPIKey(); k != "" {
		t.Fatalf("api_key should be cleared, got %q", k)
	}
}

// --- Bootstrap ---

func TestAuthManager_Bootstrap_EmptyKeychain_NoOp(t *testing.T) {
	f := newAuthFixture(t)
	f.manager.Bootstrap(context.Background())
	if s := f.manager.State(); s != AuthStateSignedOut {
		t.Fatalf("state=%q", s)
	}
}

func TestAuthManager_Bootstrap_ValidKey_SignsIn(t *testing.T) {
	f := newAuthFixture(t)
	_ = f.keychain.SetAPIKey("user-1", "sk_valid")
	f.on(http.MethodGet, "/api/v1/auth/me", http.StatusOK, map[string]any{
		"id": "user-1", "email": "alice@example.com",
	})

	f.manager.Bootstrap(context.Background())

	if s := f.manager.State(); s != AuthStateSignedIn {
		t.Fatalf("state=%q want signed_in", s)
	}
	if f.cfg.APIKey != "sk_valid" {
		t.Fatalf("cfg.APIKey=%q", f.cfg.APIKey)
	}
}

func TestAuthManager_Bootstrap_StaleKey_ClearsKeychain(t *testing.T) {
	f := newAuthFixture(t)
	_ = f.keychain.SetAPIKey("user-1", "sk_stale")
	f.onError(http.MethodGet, "/api/v1/auth/me", http.StatusUnauthorized,
		"invalid_api_key", "")

	f.manager.Bootstrap(context.Background())

	if s := f.manager.State(); s != AuthStateSignedOut {
		t.Fatalf("state=%q want signed_out", s)
	}
	if k, _ := f.keychain.GetAPIKey(); k != "" {
		t.Fatalf("stale keychain key should be cleared, got %q", k)
	}
	if snap := f.manager.Snapshot(); snap.LastErrorCode != "invalid_api_key" {
		t.Fatalf("LastErrorCode=%q", snap.LastErrorCode)
	}
}

func TestAuthManager_Bootstrap_NetworkError_OptimisticSignIn(t *testing.T) {
	f := newAuthFixture(t)
	_ = f.keychain.SetAPIKey("user-1", "sk_maybevalid")
	// /me returns 500 (treated as transient).
	f.on(http.MethodGet, "/api/v1/auth/me", http.StatusInternalServerError, nil)

	f.manager.Bootstrap(context.Background())

	if s := f.manager.State(); s != AuthStateSignedIn {
		t.Fatalf("state=%q want optimistic signed_in", s)
	}
	if k, _ := f.keychain.GetAPIKey(); k != "sk_maybevalid" {
		t.Fatalf("optimistic bootstrap must not delete key, got %q", k)
	}
}

func TestAuthManager_Bootstrap_LegacyAccount_Renames(t *testing.T) {
	f := newAuthFixture(t)
	// Simulate the yaml→Keychain migration: legacy account.
	_ = f.keychain.Write(keychain.ServiceDaemonAPIKey, keychain.AccountLegacy, "sk_legacy")
	_ = f.keychain.Write(keychain.ServiceDaemonState, keychain.AccountCurrentUser, keychain.AccountLegacy)

	f.on(http.MethodGet, "/api/v1/auth/me", http.StatusOK, map[string]any{
		"id": "real-user-uuid", "email": "alice@example.com",
	})

	f.manager.Bootstrap(context.Background())

	u, k, _ := f.keychain.GetActiveUserAndKey()
	if u != "real-user-uuid" || k != "sk_legacy" {
		t.Fatalf("post-rename user=%q key=%q", u, k)
	}
}

// --- WS 401 ---

func TestAuthManager_HandleWSAuthFailure_ClearsAll(t *testing.T) {
	f := newAuthFixture(t)
	_ = f.keychain.SetAPIKey("user-1", "sk_revoked")
	f.cfg.APIKey = "sk_revoked"
	f.manager.setState(AuthStateSignedIn, &client.AuthUser{ID: "user-1"}, "")

	f.manager.HandleWSAuthFailure()

	if s := f.manager.State(); s != AuthStateSignedOut {
		t.Fatalf("state=%q", s)
	}
	if f.cfg.APIKey != "" {
		t.Fatalf("cfg.APIKey=%q", f.cfg.APIKey)
	}
	if k, _ := f.keychain.GetAPIKey(); k != "" {
		t.Fatalf("keychain key should be cleared, got %q", k)
	}
	if snap := f.manager.Snapshot(); snap.LastErrorCode != "ws_unauthorized" {
		t.Fatalf("LastErrorCode=%q", snap.LastErrorCode)
	}
}

// --- ResendVerification ---

func TestAuthManager_ResendVerification_UsesPendingEmail(t *testing.T) {
	f := newAuthFixture(t)
	f.on(http.MethodPost, "/api/v1/auth/register", http.StatusAccepted, nil)
	_ = f.manager.Register(context.Background(), client.AuthRegisterRequest{
		Email: "alice@example.com", Password: "pw1",
	})

	var sentEmail string
	f.handlers[http.MethodPost+" /api/v1/auth/resend-verification"] = func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		sentEmail = body["email"]
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{}"))
	}

	if err := f.manager.ResendVerification(context.Background(), "", ""); err != nil {
		t.Fatalf("ResendVerification: %v", err)
	}
	if sentEmail != "alice@example.com" {
		t.Fatalf("sentEmail=%q want pending email", sentEmail)
	}
}

func TestAuthManager_ResendVerification_NoEmailNoPending(t *testing.T) {
	f := newAuthFixture(t)
	err := f.manager.ResendVerification(context.Background(), "", "")
	if err == nil || !strings.Contains(err.Error(), "no email") {
		t.Fatalf("expected 'no email' error, got %v", err)
	}
}

// --- Singleflight (concurrent login coalesces) ---

func TestAuthManager_Login_Singleflight(t *testing.T) {
	f := newAuthFixture(t)
	var loginCalls int
	var mu sync.Mutex
	f.handlers[http.MethodPost+" /api/v1/auth/login"] = func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		loginCalls++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(client.AuthLoginResponse{
			AccessToken: "at",
			User:        client.AuthUser{ID: "user-1", Email: "a@b.c"},
		})
	}
	f.on(http.MethodPost, "/api/v1/auth/api-keys", http.StatusOK, client.AuthAPIKeyResponse{APIKey: "sk_x"})

	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = f.manager.Login(context.Background(), "a@b.c", "pw")
		}()
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if loginCalls != 1 {
		t.Fatalf("loginCalls=%d want 1 (singleflight should coalesce)", loginCalls)
	}
}

// --- Platform unsupported ---

// --- Regression coverage for review fixes ---

// Login from pending_verification must clear pendingEmail on success. The
// snapshot leaking a stale pending_email after sign-in is a UX bug
// (Desktop sees "signed in" AND a "please verify" banner).
func TestAuthManager_LoginFromPendingVerification_ClearsPendingEmail(t *testing.T) {
	f := newAuthFixture(t)
	f.on(http.MethodPost, "/api/v1/auth/register", http.StatusAccepted, nil)
	if err := f.manager.Register(context.Background(), client.AuthRegisterRequest{
		Email: "alice@example.com", Password: "pw1234",
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if f.manager.Snapshot().PendingEmail != "alice@example.com" {
		t.Fatal("setup expected pending_email after register")
	}

	f.on(http.MethodPost, "/api/v1/auth/login", http.StatusOK, client.AuthLoginResponse{
		AccessToken: "at",
		User:        client.AuthUser{ID: "user-1", Email: "alice@example.com", EmailVerified: true},
	})
	f.on(http.MethodPost, "/api/v1/auth/api-keys", http.StatusOK, client.AuthAPIKeyResponse{APIKey: "sk_x"})

	if err := f.manager.Login(context.Background(), "alice@example.com", "pw1234"); err != nil {
		t.Fatalf("Login: %v", err)
	}
	snap := f.manager.Snapshot()
	if snap.State != AuthStateSignedIn {
		t.Fatalf("state=%q", snap.State)
	}
	if snap.PendingEmail != "" {
		t.Fatalf("pendingEmail should be cleared on signed_in, got %q", snap.PendingEmail)
	}
}

// SignOut clears tokens explicitly; setState(signed_out) on the error
// paths inside doLogin (e.g. api_key_bootstrap_failed) must also clear
// them so they don't leak past the failed run.
func TestAuthManager_DoLoginErrorAfterTokens_ClearsTokens(t *testing.T) {
	f := newAuthFixture(t)
	f.on(http.MethodPost, "/api/v1/auth/login", http.StatusOK, client.AuthLoginResponse{
		AccessToken: "at", RefreshToken: "rt",
		User: client.AuthUser{ID: "user-1", Email: "a@b.c"},
	})
	// Force the api-keys mint to fail so we hit the bootstrapping_key →
	// signed_out path AFTER tokens were stored.
	f.onError(http.MethodPost, "/api/v1/auth/api-keys", http.StatusInternalServerError, "server_error", "")

	_ = f.manager.Login(context.Background(), "a@b.c", "pw")
	f.manager.mu.RLock()
	at, rt := f.manager.accessToken, f.manager.refreshToken
	f.manager.mu.RUnlock()
	if at != "" || rt != "" {
		t.Fatalf("tokens not cleared on signed_out: at=%q rt=%q", at, rt)
	}
}

// Bootstrap on a stale Keychain key must emit an auth_state_changed
// event even though the state didn't change (signed_out → signed_out)
// because the lastErr changed. Pure event subscribers must learn about
// the failure without polling /state.
func TestAuthManager_BootstrapStaleKey_EmitsEventOnLastErrChange(t *testing.T) {
	f := newAuthFixture(t)
	_ = f.keychain.SetAPIKey("user-1", "sk_stale")
	f.onError(http.MethodGet, "/api/v1/auth/me", http.StatusUnauthorized, "invalid_api_key", "")

	ch := f.bus.Subscribe()
	defer f.bus.Unsubscribe(ch)

	f.manager.Bootstrap(context.Background())

	// Drain available events; assert at least one with last_error_code set.
	deadline := time.After(500 * time.Millisecond)
	var sawInvalidKey bool
	for !sawInvalidKey {
		select {
		case evt := <-ch:
			if evt.Type != EventAuthStateChanged {
				continue
			}
			var snap authSnapshot
			_ = json.Unmarshal(evt.Payload, &snap)
			if snap.State == AuthStateSignedOut && snap.LastErrorCode == "invalid_api_key" {
				sawInvalidKey = true
			}
		case <-deadline:
			t.Fatal("bootstrap stale-key did not emit auth_state_changed with invalid_api_key")
		}
	}
}

// Optimistic offline bootstrap (signed_in via a key that /me can't
// validate due to network error) must NOT poison last_error_code —
// Desktop should render a clean signed_in state. WS 401 (if any) will
// later trigger HandleWSAuthFailure which DOES set a code.
func TestAuthManager_BootstrapOffline_NoLastErr(t *testing.T) {
	f := newAuthFixture(t)
	_ = f.keychain.SetAPIKey("user-1", "sk_maybevalid")
	// 500 → transport error path (not 401).
	f.on(http.MethodGet, "/api/v1/auth/me", http.StatusInternalServerError, nil)

	f.manager.Bootstrap(context.Background())

	snap := f.manager.Snapshot()
	if snap.State != AuthStateSignedIn {
		t.Fatalf("state=%q want signed_in", snap.State)
	}
	if snap.LastErrorCode != "" {
		t.Fatalf("LastErrorCode=%q want empty (optimistic sign-in is a success outcome)", snap.LastErrorCode)
	}
}

// OnAPIKeyChanged must observe cfg.APIKey already set to the new value
// when the callback fires (tool re-registration depends on this). This
// pins the contract that applyAPIKey writes cfg.APIKey BEFORE invoking
// the callback.
func TestAuthManager_OnAPIKeyChanged_SeesUpdatedCfg(t *testing.T) {
	fc := newFakeCloudForCfgTest(t)
	cfg := &config.Config{}
	be := keychain.NewMemBackend()
	kc := keychain.NewStore(be, nil)
	gw := client.NewGatewayClient(fc.URL, "")
	authClient := client.NewAuthClient(fc.URL, fc.Client())

	var observedKey string
	var calls int
	mgr := NewAuthManager(AuthManagerConfig{
		Keychain: kc, Cloud: authClient, Gateway: gw, Cfg: cfg,
		OnAPIKeyChanged: func(ctx context.Context) {
			calls++
			observedKey = cfg.APIKey
		},
	})
	mgr.SetEventBus(NewEventBus())

	if err := mgr.Login(context.Background(), "a@b.c", "pw"); err != nil {
		t.Fatalf("Login: %v", err)
	}
	if calls == 0 {
		t.Fatal("OnAPIKeyChanged was not invoked")
	}
	if observedKey != "sk_new" {
		t.Fatalf("OnAPIKeyChanged observed cfg.APIKey=%q at callback time; expected the new key", observedKey)
	}

	// Sign-out: callback fires again with cfg.APIKey already cleared.
	mgr.SignOut(context.Background(), false)
	if observedKey != "" {
		t.Fatalf("after sign-out, OnAPIKeyChanged should observe empty cfg.APIKey, got %q", observedKey)
	}
}

// Helper for the cfg-observation test: a minimal fake Cloud that
// returns canned responses for login + api-keys without the per-test
// route table that authFixture maintains.
func newFakeCloudForCfgTest(t *testing.T) *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/auth/login", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(client.AuthLoginResponse{
			AccessToken: "at",
			User:        client.AuthUser{ID: "user-1", Email: "a@b.c"},
		})
	})
	mux.HandleFunc("POST /api/v1/auth/api-keys", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(client.AuthAPIKeyResponse{APIKey: "sk_new"})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// Regression: the daemon MUST inject provider="email" before forwarding
// the register call to Cloud — without it, Cloud rejects with
// "Only 'google' provider is supported". This bug surfaced live on
// 2026-05-20 with the Desktop sign-up form returning that exact message.
//
// Also pins:
//   - daemon derives `username` when caller leaves it blank (Cloud requires it)
//   - daemon maps Desktop's friendly `name` to Cloud's `full_name` field name
func TestAuthManager_Register_InjectsProviderAndDerivesUsername(t *testing.T) {
	f := newAuthFixture(t)

	var seenBody map[string]any
	f.handlers[http.MethodPost+" /api/v1/auth/register"] = func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&seenBody)
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte("{}"))
	}

	// Simulate the Desktop path: handler forwards FullName but no Provider,
	// no Username. AuthManager must fill both before hitting Cloud.
	if err := f.manager.Register(context.Background(), client.AuthRegisterRequest{
		Email:    "zhaichen@alioyun.com",
		Password: "pw1234567",
		FullName: "Chen",
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	if seenBody["provider"] != "email" {
		t.Fatalf("Cloud saw provider=%v, want \"email\" (this was the live 2026-05-20 bug)", seenBody["provider"])
	}
	if seenBody["full_name"] != "Chen" {
		t.Fatalf("Cloud saw full_name=%v, want \"Chen\"", seenBody["full_name"])
	}
	username, _ := seenBody["username"].(string)
	if username == "" {
		t.Fatalf("Cloud saw empty username — daemon must auto-derive")
	}
	if !strings.HasPrefix(username, "zhaichen_") {
		t.Fatalf("derived username=%q should start with email-prefix \"zhaichen_\"", username)
	}
	if len(username) < 3 || len(username) > 50 {
		t.Fatalf("derived username=%q violates Cloud's 3-50 char rule (len=%d)", username, len(username))
	}
}

func TestDeriveUsername_Sanitization(t *testing.T) {
	cases := []struct {
		email      string
		wantPrefix string
		minLen     int
	}{
		{"alice@example.com", "alice_", 9},
		{"ZhaiChen@alioyun.com", "zhaichen_", 9},
		{"name.with.dots@x.y", "name_with_dots_", 10},
		{"weird+chars#@y.z", "weird_chars__", 10},
		{"ab@x.y", "ab", 3}, // short prefix is padded then hex-suffixed
		{"no-at-sign-just-string", "no_at_sign_just_string_", 10},
	}
	for _, tc := range cases {
		got := deriveUsername(tc.email)
		if !strings.HasPrefix(got, tc.wantPrefix) {
			t.Errorf("deriveUsername(%q)=%q, want prefix %q", tc.email, got, tc.wantPrefix)
		}
		if len(got) < tc.minLen || len(got) > 50 {
			t.Errorf("deriveUsername(%q)=%q, len=%d outside [%d, 50]", tc.email, got, len(got), tc.minLen)
		}
		// Charset: lowercase alphanumerics + underscore.
		for i, r := range got {
			ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_'
			if !ok {
				t.Errorf("deriveUsername(%q)=%q has invalid char %q at %d", tc.email, got, r, i)
				break
			}
		}
	}
}

// Two calls with the same email must yield different usernames — the
// random suffix is what makes Cloud's "globally unique" constraint hold
// without a server round-trip to check availability.
func TestDeriveUsername_RandomSuffixUniqueness(t *testing.T) {
	a := deriveUsername("alice@example.com")
	b := deriveUsername("alice@example.com")
	if a == b {
		t.Fatalf("deriveUsername should produce unique suffixes per call; got %q twice", a)
	}
}

func TestAuthManager_NilKeychain_ReturnsErrPlatformUnsupported(t *testing.T) {
	cfg := &config.Config{}
	gw := client.NewGatewayClient("http://localhost", "")
	mgr := NewAuthManager(AuthManagerConfig{
		Keychain: nil, // platform unsupported
		Cloud:    client.NewAuthClient("http://localhost", nil),
		Gateway:  gw,
		Cfg:      cfg,
	})
	err := mgr.Login(context.Background(), "a@b.c", "pw")
	if !IsErrPlatformUnsupported(err) {
		t.Fatalf("expected platform unsupported, got %v", err)
	}
}
