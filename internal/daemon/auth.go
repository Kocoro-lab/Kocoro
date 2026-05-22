package daemon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/config"
	"github.com/Kocoro-lab/ShanClaw/internal/keychain"
)

// AuthState enumerates the daemon's email/password login lifecycle. It is
// the sole observable state used by Desktop (via GET /local/auth/state and
// EventAuthStateChanged) to decide UI affordances.
type AuthState string

const (
	AuthStateSignedOut           AuthState = "signed_out"
	AuthStatePendingVerification AuthState = "pending_verification"
	AuthStateLoggingIn           AuthState = "logging_in"
	AuthStateBootstrappingKey    AuthState = "bootstrapping_key"
	AuthStateSignedIn            AuthState = "signed_in"
)

// EventAuthStateChanged is emitted on every state transition. Payload is
// authSnapshot serialized as JSON.
const EventAuthStateChanged = "auth_state_changed"

// authSnapshot is the wire shape returned by /local/auth/state and emitted
// on auth_state_changed. PendingEmail / LastErrorCode are non-empty only
// while they make sense (pending_verification / right after a failed
// transition); UpdatedAt lets Desktop dedupe rapid emits.
type authSnapshot struct {
	State         AuthState        `json:"state"`
	User          *client.AuthUser `json:"user,omitempty"`
	PendingEmail  string           `json:"pending_email,omitempty"`
	LastErrorCode string           `json:"last_error_code,omitempty"`
	UpdatedAt     time.Time        `json:"updated_at"`
}

// AuthManagerConfig collects the daemon dependencies AuthManager needs to
// operate. All fields are required EXCEPT Cfg (legacy callers/tests may still
// pass it), OnAPIKeyChanged (nil → skip tool rebuild), and WSController
// (nil → skip WS lifecycle).
type AuthManagerConfig struct {
	Keychain        *keychain.Store
	Cloud           *client.AuthClient
	Gateway         *client.GatewayClient
	WSClient        *Client
	Cfg             *config.Config
	OnAPIKeyChanged func(context.Context)
	Logger          *log.Logger
}

// AuthManager owns the daemon-side authentication state machine. All state
// changes go through setState(); concurrent operations on the same email
// are coalesced via singleflight.
type AuthManager struct {
	mu           sync.RWMutex
	state        AuthState
	user         *client.AuthUser
	pendingEmail string
	accessToken  string // RAM only — never persisted (Keychain holds api_key)
	refreshToken string // RAM only
	lastErr      string
	updatedAt    time.Time

	kc              *keychain.Store
	cloud           *client.AuthClient
	gw              *client.GatewayClient
	wsClient        *Client
	wsCtl           *WSController
	bus             *EventBus
	onAPIKeyChanged func(context.Context)
	logger          *log.Logger
	sf              singleflight.Group
}

// NewAuthManager builds an AuthManager. WSController and EventBus are
// installed separately (SetWSController, SetEventBus) because they often
// need a reference to the AuthManager during their own construction —
// avoid the circular dependency at New().
func NewAuthManager(cfg AuthManagerConfig) *AuthManager {
	logger := cfg.Logger
	if logger == nil {
		logger = log.Default()
	}
	return &AuthManager{
		state:           AuthStateSignedOut,
		updatedAt:       time.Now(),
		kc:              cfg.Keychain,
		cloud:           cfg.Cloud,
		gw:              cfg.Gateway,
		wsClient:        cfg.WSClient,
		onAPIKeyChanged: cfg.OnAPIKeyChanged,
		logger:          logger,
	}
}

// SetEventBus installs the bus AuthManager emits auth_state_changed on.
// Calling with nil disables emission (used by tests that want to inspect
// state directly without consuming events).
func (a *AuthManager) SetEventBus(bus *EventBus) {
	a.mu.Lock()
	a.bus = bus
	a.mu.Unlock()
}

// SetWSController installs the controller used to start/stop the WS
// connection on sign-in / sign-out boundaries.
func (a *AuthManager) SetWSController(ctl *WSController) {
	a.mu.Lock()
	a.wsCtl = ctl
	a.mu.Unlock()
}

// Snapshot returns a value-copy of the current auth state. Safe to expose
// to handlers and tests.
func (a *AuthManager) Snapshot() authSnapshot {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.snapshotLocked()
}

func (a *AuthManager) snapshotLocked() authSnapshot {
	snap := authSnapshot{
		State:         a.state,
		PendingEmail:  a.pendingEmail,
		LastErrorCode: a.lastErr,
		UpdatedAt:     a.updatedAt,
	}
	if a.user != nil {
		u := *a.user
		snap.User = &u
	}
	return snap
}

// State returns the current AuthState (cheap, no allocation).
func (a *AuthManager) State() AuthState {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.state
}

// setState is the single mutator. Emits auth_state_changed whenever any
// terminal observable field (state OR last_error_code) changes — both
// drive Desktop UI affordances, so a state-stable error transition
// (e.g. Bootstrap signed_out→signed_out with "invalid_api_key" code)
// still needs to reach subscribers.
//
// Side-effects beyond simple field writes:
//   - Entering signed_out: clears user/pendingEmail/tokens (terminal cleanup).
//   - Entering signed_in: clears pendingEmail (verification done) but
//     KEEPS tokens (they're the live session credentials).
//
// Event emission happens AFTER releasing the AuthManager lock so handlers
// reading from the bus cannot deadlock against the mutator.
func (a *AuthManager) setState(s AuthState, user *client.AuthUser, errCode string) {
	a.mu.Lock()
	prev := a.state
	prevErr := a.lastErr
	a.state = s
	if user != nil {
		u := *user
		a.user = &u
	}
	if s == AuthStateSignedOut {
		a.user = nil
		a.pendingEmail = ""
		a.accessToken = ""
		a.refreshToken = ""
	}
	if s == AuthStateSignedIn {
		// Verification consumed; the email no longer needs surfacing.
		a.pendingEmail = ""
	}
	a.lastErr = errCode
	a.updatedAt = time.Now()
	snap := a.snapshotLocked()
	bus := a.bus
	a.mu.Unlock()

	if prev == s && prevErr == errCode {
		return
	}
	if bus != nil {
		payload, _ := json.Marshal(snap)
		bus.Emit(Event{Type: EventAuthStateChanged, Payload: payload})
	}
}

// --- Public API used by /local/auth/* handlers ---

// Bootstrap recovers a prior sign-in from Keychain on daemon startup. The
// algorithm:
//  1. Keychain empty → stay signed_out (default state, no event needed).
//  2. Active user == AccountLegacy → call /auth/me with the legacy key,
//     resolve real user_id, rename the entry, then enter signed_in.
//  3. Otherwise → call /auth/me; 401 means stale key, clear Keychain and
//     stay signed_out. Network error → optimistic signed_in (WS will
//     retry; if it 401s, HandleWSAuthFailure tears down).
//
// Bootstrap is non-blocking by design — cmd/daemon.go launches it in a
// goroutine so the HTTP server is up immediately.
func (a *AuthManager) Bootstrap(ctx context.Context) {
	if a.kc == nil {
		a.logger.Printf("auth: bootstrap skipped (keychain unsupported)")
		return
	}
	userID, apiKey, err := a.kc.GetActiveUserAndKey()
	if err != nil {
		a.logger.Printf("auth: bootstrap read keychain: %v", err)
		return
	}
	if apiKey == "" {
		return
	}

	a.applyAPIKey(ctx, apiKey)

	probeCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	user, err := a.cloud.MeWithAPIKey(probeCtx, apiKey)
	if err != nil {
		if ae, ok := client.IsAuthError(err); ok && ae.HTTPCode == 401 {
			a.logger.Printf("auth: bootstrap /me rejected api_key, clearing")
			_ = a.kc.DeleteAPIKey()
			_ = a.kc.ClearActiveUser()
			a.applyAPIKey(ctx, "")
			a.setState(AuthStateSignedOut, nil, "invalid_api_key")
			return
		}
		// Network / transport error — assume the key is still valid and
		// let WS reconnect surface a 401 if it isn't. This keeps the
		// daemon usable on flaky networks where /me times out. We do
		// NOT set lastErr here: optimistic sign-in is a success outcome
		// from Desktop's perspective (UI should show signed_in cleanly),
		// and WS 401 will run HandleWSAuthFailure if the key really is
		// invalid.
		a.logger.Printf("auth: bootstrap /me failed (%v) — optimistic signed_in", err)
		a.setState(AuthStateSignedIn, nil, "")
		a.startWS(ctx)
		return
	}

	if userID == keychain.AccountLegacy && user.ID != "" {
		if err := a.kc.RenameLegacy(user.ID); err != nil {
			a.logger.Printf("auth: bootstrap rename legacy → %s: %v", user.ID, err)
		}
	}
	a.setState(AuthStateSignedIn, user, "")
	a.startWS(ctx)
}

// Register proxies POST /api/v1/auth/register to Cloud and transitions to
// pending_verification on 202. The daemon enforces provider="email" here
// (Cloud rejects requests without an explicit provider as "Only 'google'
// provider is supported"). When the caller did not supply a Username,
// one is derived from the email prefix plus a random hex suffix so the
// 3-50 char + globally-unique Cloud constraint holds without forcing
// Desktop to collect a username field.
func (a *AuthManager) Register(ctx context.Context, req client.AuthRegisterRequest) error {
	if a.kc == nil {
		return errPlatformUnsupported
	}
	req.Provider = client.ProviderEmail
	if req.Username == "" {
		req.Username = deriveUsername(req.Email)
	}
	if err := a.cloud.Register(ctx, req); err != nil {
		return err
	}
	a.mu.Lock()
	a.pendingEmail = req.Email
	a.mu.Unlock()
	a.setState(AuthStatePendingVerification, nil, "")
	return nil
}

// deriveUsername builds a Cloud-compatible username from an email
// address. Cloud requires 3-50 chars, must be globally unique. Strategy:
//  1. Take everything before '@' (or whole string if no '@').
//  2. Lowercase + replace non-[a-z0-9_] with '_'.
//  3. Truncate to 30 chars to leave room for the random suffix.
//  4. Pad with 'x' to 3 chars if the email prefix was unusually short.
//  5. Append '_' + 8 random hex chars (4 bytes of crypto/rand = 2³² space).
//
// Collision math: per-signup probability that the derived username clashes
// with an existing user sharing the same email prefix ≈ N / 2³².
//   - N = 10K same-prefix users  → ~2.3e-6 per signup
//   - N = 100K same-prefix users → ~2.3e-5 per signup
//
// On the rare 409 Cloud-side username clash the user just retries and the
// fresh crypto/rand suffix almost certainly succeeds — self-healing UX,
// no daemon-level retry needed. Cloud's global uniqueness check is the
// authoritative gate; the random suffix only minimises how often users
// hit the collision path.
//
// crypto/rand failure is unreachable on a healthy kernel; if Read errors
// we still produce a valid username with a time-derived suffix so the
// signup proceeds rather than blocking the user.
func deriveUsername(email string) string {
	at := strings.Index(email, "@")
	prefix := email
	if at > 0 {
		prefix = email[:at]
	}
	var b strings.Builder
	b.Grow(len(prefix))
	for _, r := range strings.ToLower(prefix) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	sanitized := b.String()
	if len(sanitized) > 30 {
		sanitized = sanitized[:30]
	}
	for len(sanitized) < 3 {
		sanitized += "x"
	}
	var buf [4]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// CSPRNG failure path — unreachable in practice. Fall back to a
		// time-derived suffix so the registration still completes; the
		// uniqueness gap is acceptable for the once-per-process worst case.
		now := time.Now().UnixNano()
		return fmt.Sprintf("%s_%x", sanitized, uint32(now))
	}
	return sanitized + "_" + hex.EncodeToString(buf[:])
}

// Login coalesces concurrent attempts for the same email via singleflight,
// then delegates to doLogin.
func (a *AuthManager) Login(ctx context.Context, email, password string) error {
	if a.kc == nil {
		return errPlatformUnsupported
	}
	_, err, _ := a.sf.Do("login:"+email, func() (any, error) {
		return nil, a.doLogin(ctx, email, password)
	})
	return err
}

func (a *AuthManager) doLogin(ctx context.Context, email, password string) error {
	a.setState(AuthStateLoggingIn, nil, "")
	resp, err := a.cloud.Login(ctx, client.AuthLoginRequest{Email: email, Password: password})
	if err != nil {
		if ae, ok := client.IsAuthError(err); ok && ae.Code == "email_not_verified" {
			a.mu.Lock()
			a.pendingEmail = email
			a.mu.Unlock()
			a.setState(AuthStatePendingVerification, nil, ae.Code)
		} else {
			code := ""
			if ae, ok := client.IsAuthError(err); ok {
				code = ae.Code
			}
			a.setState(AuthStateSignedOut, nil, code)
		}
		return err
	}

	a.mu.Lock()
	a.accessToken = resp.AccessToken
	a.refreshToken = resp.RefreshToken
	a.mu.Unlock()
	a.setState(AuthStateBootstrappingKey, &resp.User, "")

	apiKey, err := a.kc.Read(keychain.ServiceDaemonAPIKey, resp.User.ID)
	if err != nil {
		a.setState(AuthStateSignedOut, nil, "keychain_read_failed")
		return fmt.Errorf("keychain read existing api_key: %w", err)
	}
	if apiKey == "" {
		keyResp, err := a.cloud.CreateAPIKey(ctx, resp.AccessToken, "kocoro-daemon")
		if err != nil {
			a.setState(AuthStateSignedOut, nil, "api_key_bootstrap_failed")
			return fmt.Errorf("create api_key: %w", err)
		}
		apiKey = keyResp.APIKey
		if err := a.kc.SetAPIKey(resp.User.ID, apiKey); err != nil {
			a.setState(AuthStateSignedOut, nil, "keychain_write_failed")
			return fmt.Errorf("keychain write: %w", err)
		}
	} else {
		// Re-affirm current_user_id even when the key was already present
		// (e.g. user signed back in on the same machine after sign-out).
		_ = a.kc.SetAPIKey(resp.User.ID, apiKey)
	}

	a.applyAPIKey(ctx, apiKey)
	a.setState(AuthStateSignedIn, &resp.User, "")
	a.startWS(ctx)
	return nil
}

// ResendVerification proxies POST /api/v1/auth/resend-verification. If
// email is empty, uses the pendingEmail captured during register/login.
// `language` is the per-call locale override (Cloud contract uses the
// `language` field name, NOT `preferred_language`); empty string lets
// Cloud fall back to the user's stored DB default.
func (a *AuthManager) ResendVerification(ctx context.Context, email, language string) error {
	if a.kc == nil {
		return errPlatformUnsupported
	}
	if email == "" {
		a.mu.RLock()
		email = a.pendingEmail
		a.mu.RUnlock()
	}
	if email == "" {
		return fmt.Errorf("no email provided and no pending verification")
	}
	return a.cloud.ResendVerification(ctx, email, language)
}

// ForgotPassword proxies POST /api/v1/auth/forgot-password. Does not
// change daemon state — user may be signed_out, pending_verification, or
// even signed_in. `language` semantics match ResendVerification.
func (a *AuthManager) ForgotPassword(ctx context.Context, email, language string) error {
	if a.kc == nil {
		return errPlatformUnsupported
	}
	return a.cloud.ForgotPassword(ctx, email, language)
}

// SignOut tears down the in-memory session. When clearKeychain is false
// the api_key is preserved so the user can sign back in without re-typing
// credentials (Login will re-affirm). When clearKeychain is true (the
// /sign-out-full endpoint) the Keychain entry is removed too.
func (a *AuthManager) SignOut(ctx context.Context, clearKeychain bool) {
	if a.kc == nil {
		return
	}
	a.stopWS()
	a.mu.Lock()
	a.accessToken = ""
	a.refreshToken = ""
	a.mu.Unlock()
	a.applyAPIKey(ctx, "")
	if clearKeychain {
		_ = a.kc.DeleteAPIKey()
		_ = a.kc.ClearActiveUser()
	}
	a.setState(AuthStateSignedOut, nil, "")
}

// HandleWSAuthFailure is the callback the WS client invokes when Cloud
// rejects the upgrade with 401. The 401 is a stable signal that the
// api_key has been revoked (vs network noise) so we wipe Keychain and
// drop to signed_out — the user must re-login to get a fresh key.
func (a *AuthManager) HandleWSAuthFailure() {
	if a.kc == nil {
		return
	}
	a.logger.Printf("auth: ws rejected with 401; clearing keychain and signing out")
	_ = a.kc.DeleteAPIKey()
	_ = a.kc.ClearActiveUser()
	a.stopWS()
	a.mu.Lock()
	a.accessToken = ""
	a.refreshToken = ""
	a.mu.Unlock()
	a.applyAPIKey(context.Background(), "")
	a.setState(AuthStateSignedOut, nil, "ws_unauthorized")
}

// --- Internal helpers ---

var errPlatformUnsupported = errors.New("auth: platform unsupported (macOS Keychain required)")

// IsErrPlatformUnsupported reports whether err signals that AuthManager
// declined to act because Keychain is unavailable on this OS. Handlers
// use this to return HTTP 503 platform_unsupported.
func IsErrPlatformUnsupported(err error) bool {
	return errors.Is(err, errPlatformUnsupported)
}

// applyAPIKey is the single chokepoint that fan-outs an api_key change to
// the dependencies that consume it:
//   - GatewayClient.SetAPIKey   (X-API-Key on all Cloud HTTP requests)
//   - WS client.SetAPIKey       (Authorization: Bearer on WS upgrade)
//   - OnAPIKeyChanged callback  (rebuilds auth-sensitive tools from the
//     GatewayClient's synchronized live key)
//
// Pass "" to clear all live consumers uniformly.
func (a *AuthManager) applyAPIKey(ctx context.Context, key string) {
	a.gw.SetAPIKey(key)
	if a.wsClient != nil {
		a.wsClient.SetAPIKey(key)
	}
	a.mu.RLock()
	cb := a.onAPIKeyChanged
	a.mu.RUnlock()
	if cb != nil {
		cb(ctx)
	}
}

func (a *AuthManager) startWS(ctx context.Context) {
	a.mu.RLock()
	ctl := a.wsCtl
	a.mu.RUnlock()
	if ctl != nil {
		ctl.Start(ctx)
	}
}

func (a *AuthManager) stopWS() {
	a.mu.RLock()
	ctl := a.wsCtl
	a.mu.RUnlock()
	if ctl != nil {
		ctl.Stop()
	}
}
