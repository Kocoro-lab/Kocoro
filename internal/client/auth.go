package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

func defaultAuthHTTPClient() *http.Client {
	return &http.Client{Timeout: 30 * time.Second}
}

// Provider values for AuthRegisterRequest.Provider. Cloud uses this
// field to discriminate the auth flow — sending an empty/unknown value
// causes Cloud to default to its OAuth-only path and reject with
// "Only 'google' provider is supported". AuthManager.Register always
// sets ProviderEmail; direct AuthClient callers (e.g. live tests) must
// set it explicitly too.
const (
	ProviderEmail  = "email"
	ProviderGoogle = "google" // reserved for future use; daemon does not currently exercise this path
)

// AuthClient calls Cloud /api/v1/auth/* endpoints. Lifecycle:
//   - daemon Bootstrap → MeWithAPIKey  (no JWT yet, restoring from Keychain)
//   - Desktop POST /local/auth/register → Register (no auth)
//   - Desktop POST /local/auth/login → Login (no auth) → CreateAPIKey (Bearer JWT)
//   - daemon background → Refresh (refresh_token, periodic)
//
// AuthClient is HTTP-only; it does not maintain login state. AuthManager
// in internal/daemon owns the state machine and decides which method to call.
type AuthClient struct {
	baseURL    string
	httpClient *http.Client
}

// NewAuthClient builds an AuthClient against baseURL. If httpClient is nil
// a 30s-timeout default is created. Production calls this with gw.HTTPClient()
// so all Cloud traffic shares one transport.
func NewAuthClient(baseURL string, httpClient *http.Client) *AuthClient {
	if httpClient == nil {
		httpClient = defaultAuthHTTPClient()
	}
	return &AuthClient{
		baseURL:    strings.TrimRight(baseURL, "/"),
		httpClient: httpClient,
	}
}

// AuthError is returned for non-2xx responses that carry the Cloud
// {error, message} envelope. AuthManager / handlers use errors.As to pull
// out Code (stable wire contract) and HTTPCode (for transport passthrough
// to Desktop). Raw is preserved for audit logging of unrecognized shapes.
type AuthError struct {
	HTTPCode int
	Code     string
	Message  string
	Raw      []byte
}

func (e *AuthError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("auth: %s (%d): %s", e.Code, e.HTTPCode, e.Message)
	}
	return fmt.Sprintf("auth: %s (%d)", e.Code, e.HTTPCode)
}

// IsAuthError unwraps err looking for *AuthError. Convenience for callers
// that don't want to import errors directly.
func IsAuthError(err error) (*AuthError, bool) {
	var ae *AuthError
	if errors.As(err, &ae) {
		return ae, true
	}
	return nil, false
}

// AuthUser mirrors the user object Cloud returns from login / register /
// me. Optional fields are zero-valued when absent.
type AuthUser struct {
	ID            string `json:"id"`
	Email         string `json:"email"`
	EmailVerified bool   `json:"email_verified"`
	Name          string `json:"name,omitempty"`
	Tier          string `json:"tier,omitempty"`
}

// AuthRegisterRequest matches POST /api/v1/auth/register. Cloud uses the
// `provider` discriminator to route the request — without it Cloud falls
// back to OAuth-only handling and rejects with "Only 'google' provider
// is supported". AuthManager.Register always sets Provider = ProviderEmail
// before the call lands here. Direct AuthClient callers (live tests, ad-hoc
// scripts) must set Provider themselves.
//
// `username` is required by Cloud (3-50 chars, globally unique) but the
// Desktop UI does not collect it — AuthManager derives it from the email
// prefix + a random hex suffix to keep collisions vanishingly rare.
//
// `full_name` (not `name`) is the wire field name on Cloud — see Cloud
// API doc §1. The handler in internal/daemon/auth_handlers.go accepts
// the friendlier `name` from Desktop and maps it here.
type AuthRegisterRequest struct {
	Provider          string `json:"provider"`
	Email             string `json:"email"`
	Username          string `json:"username"`
	Password          string `json:"password"`
	FullName          string `json:"full_name,omitempty"`
	PreferredLanguage string `json:"preferred_language,omitempty"`
}

// AuthLoginRequest matches POST /api/v1/auth/login.
type AuthLoginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// AuthLoginResponse is returned by /auth/login on 200. AccessToken /
// RefreshToken are RAM-only (never persisted to disk per the daemon
// design — Keychain holds api_key, not JWTs).
//
// Wire shape note: Cloud returns the canonical identifier as `user_id`
// at the TOP LEVEL of the response — the nested `user` object only
// carries profile fields (email, username, name, picture). The unmarshal
// hook below copies `user_id` into `User.ID` so downstream daemon code
// (auth.go's keychain SetAPIKey, applyAPIKey, ws auth) can keep using
// `resp.User.ID` uniformly without caring about the wire-level split.
type AuthLoginResponse struct {
	UserID       string   `json:"user_id"`
	TenantID     string   `json:"tenant_id,omitempty"`
	AccessToken  string   `json:"access_token"`
	RefreshToken string   `json:"refresh_token"`
	ExpiresIn    int      `json:"expires_in"`
	User         AuthUser `json:"user"`
}

// UnmarshalJSON copies the top-level `user_id` into `User.ID` after
// default decoding. Without this hook `User.ID` stays empty (Cloud
// doesn't put `id` inside the nested `user` object) and the daemon's
// Keychain SetAPIKey call panics with "requires non-empty userID".
func (r *AuthLoginResponse) UnmarshalJSON(data []byte) error {
	type alias AuthLoginResponse
	var tmp alias
	if err := json.Unmarshal(data, &tmp); err != nil {
		return err
	}
	*r = AuthLoginResponse(tmp)
	if r.User.ID == "" {
		r.User.ID = r.UserID
	}
	return nil
}

// AuthRefreshResponse is returned by /auth/refresh on 200.
type AuthRefreshResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
}

// AuthAPIKeyResponse is returned by /auth/api-keys on 200.
type AuthAPIKeyResponse struct {
	APIKey string `json:"api_key"`
	KeyID  string `json:"key_id,omitempty"`
}

// Register submits an email/password registration. Cloud returns 202 on
// success and dispatches a verification email; nil err on 202, non-nil
// (*AuthError) on validation / conflict / rate-limit failures.
func (c *AuthClient) Register(ctx context.Context, req AuthRegisterRequest) error {
	_, err := c.do(ctx, http.MethodPost, "/api/v1/auth/register", req, authNone, nil)
	return err
}

// Login exchanges email/password for JWT pair. AuthError code is one of
// invalid_credentials / email_not_verified / rate_limited / invalid_request.
func (c *AuthClient) Login(ctx context.Context, req AuthLoginRequest) (*AuthLoginResponse, error) {
	var resp AuthLoginResponse
	if _, err := c.do(ctx, http.MethodPost, "/api/v1/auth/login", req, authNone, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// VerifyEmail consumes a verification token (from the email link). Cloud
// users normally hit a browser GET endpoint; this JSON form exists for
// programmatic flows (Deep Link future, daemon admin tools).
func (c *AuthClient) VerifyEmail(ctx context.Context, token string) error {
	_, err := c.do(ctx, http.MethodPost, "/api/v1/auth/verify-email",
		map[string]string{"token": token}, authNone, nil)
	return err
}

// ResendVerification triggers a fresh verification email. Cloud always
// returns 200 with sent:true (anti-enumeration); err is non-nil only on
// transport failures or true rate-limit responses.
//
// `language` overrides the locale for THIS email only. Cloud's i18n
// contract is explicit: this endpoint reads `language` (NOT
// `preferred_language`), and an empty / unknown value falls back to the
// user's stored `auth.users.preferred_language`. Pass "" to defer to
// the DB default.
func (c *AuthClient) ResendVerification(ctx context.Context, email, language string) error {
	body := map[string]string{"email": email}
	if language != "" {
		body["language"] = language
	}
	_, err := c.do(ctx, http.MethodPost, "/api/v1/auth/resend-verification",
		body, authNone, nil)
	return err
}

// ForgotPassword initiates a reset flow. Same anti-enumeration semantics
// as ResendVerification. `language` semantics match ResendVerification —
// Cloud expects `language`, not `preferred_language`.
func (c *AuthClient) ForgotPassword(ctx context.Context, email, language string) error {
	body := map[string]string{"email": email}
	if language != "" {
		body["language"] = language
	}
	_, err := c.do(ctx, http.MethodPost, "/api/v1/auth/forgot-password",
		body, authNone, nil)
	return err
}

// ResetPassword consumes a reset token and sets a new password. Cloud
// invalidates all refresh tokens on success — daemon must re-login.
func (c *AuthClient) ResetPassword(ctx context.Context, token, newPassword string) error {
	_, err := c.do(ctx, http.MethodPost, "/api/v1/auth/reset-password", map[string]string{
		"token":        token,
		"new_password": newPassword,
	}, authNone, nil)
	return err
}

// Refresh trades a refresh_token for a fresh access_token (+ rotated
// refresh_token). 401 indicates password reset / revocation — caller
// should sign out.
func (c *AuthClient) Refresh(ctx context.Context, refreshToken string) (*AuthRefreshResponse, error) {
	var resp AuthRefreshResponse
	_, err := c.do(ctx, http.MethodPost, "/api/v1/auth/refresh",
		map[string]string{"refresh_token": refreshToken}, authNone, &resp)
	if err != nil {
		return nil, err
	}
	return &resp, nil
}

// Me returns the authenticated user's profile using a Bearer JWT.
func (c *AuthClient) Me(ctx context.Context, accessToken string) (*AuthUser, error) {
	var resp meResponse
	_, err := c.do(ctx, http.MethodGet, "/api/v1/auth/me", nil, authBearer(accessToken), &resp)
	if err != nil {
		return nil, err
	}
	return userFromMe(&resp), nil
}

// MeWithAPIKey returns the authenticated user's profile using X-API-Key.
// Bootstrap calls this on daemon startup to validate the Keychain key.
func (c *AuthClient) MeWithAPIKey(ctx context.Context, apiKey string) (*AuthUser, error) {
	var resp meResponse
	_, err := c.do(ctx, http.MethodGet, "/api/v1/auth/me", nil, authAPIKey(apiKey), &resp)
	if err != nil {
		return nil, err
	}
	return userFromMe(&resp), nil
}

// CreateAPIKey requests a new long-lived api_key (sk_…). Called after a
// successful Login when the Keychain has no key for this user — the
// returned APIKey is the only chance to capture the full value.
func (c *AuthClient) CreateAPIKey(ctx context.Context, accessToken, name string) (*AuthAPIKeyResponse, error) {
	var resp AuthAPIKeyResponse
	body := map[string]string{}
	if name != "" {
		body["name"] = name
	}
	_, err := c.do(ctx, http.MethodPost, "/api/v1/auth/api-keys", body, authBearer(accessToken), &resp)
	if err != nil {
		return nil, err
	}
	return &resp, nil
}

// meResponse handles Cloud's /me schema which wraps the user object under
// {"user": {...}}. Callers receive a clean *AuthUser.
type meResponse struct {
	User AuthUser `json:"user"`
	// Also accept flat-shape responses where Cloud returns user fields at
	// the top level (used by login/register paths).
	ID            string `json:"id,omitempty"`
	Email         string `json:"email,omitempty"`
	EmailVerified bool   `json:"email_verified,omitempty"`
	Name          string `json:"name,omitempty"`
	Tier          string `json:"tier,omitempty"`
}

func userFromMe(r *meResponse) *AuthUser {
	if r.User.ID != "" {
		u := r.User
		return &u
	}
	return &AuthUser{
		ID:            r.ID,
		Email:         r.Email,
		EmailVerified: r.EmailVerified,
		Name:          r.Name,
		Tier:          r.Tier,
	}
}

// --- Internal request plumbing ---

type authMode struct {
	header string
	value  string
}

var authNone = authMode{}

func authBearer(token string) authMode {
	return authMode{header: "Authorization", value: "Bearer " + token}
}

func authAPIKey(key string) authMode {
	return authMode{header: "X-API-Key", value: key}
}

// do executes one Cloud HTTP call. On 2xx and out != nil, it decodes the
// response body into out. On non-2xx, it parses the Cloud error envelope
// into *AuthError; transport / decode failures are returned as plain
// errors.
func (c *AuthClient) do(ctx context.Context, method, path string, body any, auth authMode, out any) (*http.Response, error) {
	var bodyReader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal request: %w", err)
		}
		bodyReader = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	if auth.header != "" {
		req.Header.Set(auth.header, auth.value)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return resp, fmt.Errorf("read body: %w", err)
	}

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if out == nil || len(raw) == 0 {
			return resp, nil
		}
		if err := json.Unmarshal(raw, out); err != nil {
			return resp, fmt.Errorf("decode response: %w", err)
		}
		return resp, nil
	}

	return resp, parseAuthError(resp.StatusCode, raw)
}

// parseAuthError turns a non-2xx body into *AuthError. Falls back to a
// derived code if Cloud didn't supply one (transport / framework error).
//
// Special-case HTTP 404: when Cloud has not deployed the requested auth
// endpoint, it returns a 404 (often a stock HTML page from the router,
// not the {error, message} envelope). We translate that into a stable
// code `cloud_endpoint_unavailable` with a Desktop-actionable message
// so the UI does not surface "bad_request" (which legacy fallback emitted)
// and lead users / developers to suspect a daemon-side route problem.
// This was the 2026-05-20 confusion: Desktop saw `404 bad_request` on
// login/forgot-password/resend-verification and concluded the daemon
// route was unregistered, when in fact the routes were always registered
// and Cloud was returning 404.
func parseAuthError(code int, raw []byte) error {
	ae := &AuthError{HTTPCode: code, Raw: raw}
	cloudEnvelopeSeen := false
	if len(raw) > 0 {
		var env struct {
			Error   string `json:"error"`
			Message string `json:"message"`
		}
		if json.Unmarshal(raw, &env) == nil && (env.Error != "" || env.Message != "") {
			ae.Code = env.Error
			ae.Message = env.Message
			cloudEnvelopeSeen = true
		}
	}
	if ae.Code == "" {
		ae.Code = fallbackCode(code)
	}
	// Only auto-fill Message when Cloud DIDN'T give a structured envelope.
	// If Cloud sent {"error":"x","message":""} we treat the empty Message
	// as intentional and pass it through verbatim. The fallback is for the
	// "Cloud returned an HTML 404 / raw text 5xx" path where Desktop UIs
	// would otherwise show a blank error.
	if ae.Message == "" && !cloudEnvelopeSeen {
		ae.Message = fallbackMessage(code, ae.Code)
	}
	return ae
}

func fallbackCode(code int) string {
	switch {
	case code == http.StatusUnauthorized:
		return "unauthorized"
	case code == http.StatusForbidden:
		return "forbidden"
	case code == http.StatusNotFound:
		return "cloud_endpoint_unavailable"
	case code == http.StatusTooManyRequests:
		return "rate_limited"
	case code >= 500:
		return "server_error"
	case code >= 400:
		return "bad_request"
	}
	return "http_error"
}

func fallbackMessage(code int, derivedCode string) string {
	if derivedCode == "cloud_endpoint_unavailable" {
		return "Cloud has not deployed this auth endpoint yet — daemon-side routing is fine. Ask the Cloud team to deploy the corresponding /api/v1/auth/* route, or point cfg.endpoint at an environment that has it."
	}
	switch {
	case code >= 500:
		return "Cloud server returned an internal error."
	case code == http.StatusTooManyRequests:
		return "Rate limit hit — slow down and retry later."
	case code == http.StatusUnauthorized:
		return "Authentication failed."
	case code == http.StatusForbidden:
		return "Forbidden."
	}
	return ""
}
