package daemon

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

// /local/auth/* handlers. Each handler:
//   1) Verifies s.auth is wired (else 503 platform_unsupported).
//   2) Decodes the request body into a typed struct.
//   3) Calls AuthManager and translates errors:
//        - *client.AuthError → passthrough HTTPCode + {error, message}
//        - other errors → 500 internal_error
//   4) Returns the current authSnapshot on success.

type authStateResp struct {
	authSnapshot
}

// authRegisterReq is the Desktop-facing wire shape. We accept a friendly
// `name` field and map it to Cloud's `full_name`; we also expose
// `username` as an optional override (Desktop UI today doesn't collect
// one — AuthManager.Register derives a unique one from email when this
// is blank).
type authRegisterReq struct {
	Email             string `json:"email"`
	Password          string `json:"password"`
	Name              string `json:"name,omitempty"`
	Username          string `json:"username,omitempty"`
	PreferredLanguage string `json:"preferred_language,omitempty"`
}

type authLoginReq struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// authResendReq / authForgotReq — Cloud's i18n contract uses `language`
// (one-shot, per-call) for these two endpoints, NOT `preferred_language`
// (which is the register-only field stored in auth.users). Wire field name
// must match exactly; passing the wrong key silently falls back to the
// user's DB default or `en`. See Cloud's email i18n contract.
type authResendReq struct {
	Email    string `json:"email,omitempty"`
	Language string `json:"language,omitempty"`
}

type authForgotReq struct {
	Email    string `json:"email"`
	Language string `json:"language,omitempty"`
}

// handleAuthState — GET /local/auth/state. Idempotent, no IO. Desktop
// calls this on startup before subscribing to /events.
func (s *Server) handleAuthState(w http.ResponseWriter, _ *http.Request) {
	if !s.requireAuth(w) {
		return
	}
	writeJSON(w, http.StatusOK, authStateResp{s.auth.Snapshot()})
}

// handleAuthRegister — POST /local/auth/register. Cloud 202 → daemon
// transitions to pending_verification.
func (s *Server) handleAuthRegister(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuth(w) {
		return
	}
	var req authRegisterReq
	if !decodeAuthBody(w, r, &req) {
		return
	}
	if err := s.auth.Register(r.Context(), client.AuthRegisterRequest{
		Email:             strings.TrimSpace(req.Email),
		Username:          strings.TrimSpace(req.Username),
		Password:          req.Password,
		FullName:          req.Name,
		PreferredLanguage: req.PreferredLanguage,
	}); err != nil {
		writeAuthError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, authStateResp{s.auth.Snapshot()})
}

// handleAuthLogin — POST /local/auth/login. Coalesces concurrent attempts
// via singleflight inside AuthManager.
func (s *Server) handleAuthLogin(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuth(w) {
		return
	}
	var req authLoginReq
	if !decodeAuthBody(w, r, &req) {
		return
	}
	if err := s.auth.Login(r.Context(), strings.TrimSpace(req.Email), req.Password); err != nil {
		writeAuthError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, authStateResp{s.auth.Snapshot()})
}

// handleAuthResendVerification — POST /local/auth/resend-verification.
// Empty body falls back to pendingEmail from state.
func (s *Server) handleAuthResendVerification(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuth(w) {
		return
	}
	var req authResendReq
	_ = json.NewDecoder(r.Body).Decode(&req) // empty body is permitted
	if err := s.auth.ResendVerification(r.Context(), strings.TrimSpace(req.Email), strings.TrimSpace(req.Language)); err != nil {
		writeAuthError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"sent": true})
}

// handleAuthForgotPassword — POST /local/auth/forgot-password.
// Anti-enumeration: always 200, regardless of email existence.
func (s *Server) handleAuthForgotPassword(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuth(w) {
		return
	}
	var req authForgotReq
	if !decodeAuthBody(w, r, &req) {
		return
	}
	if err := s.auth.ForgotPassword(r.Context(), strings.TrimSpace(req.Email), strings.TrimSpace(req.Language)); err != nil {
		writeAuthError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"sent": true})
}

// handleAuthSignOut — POST /local/auth/sign-out. Preserves Keychain
// api_key so the user can re-login without re-typing credentials.
func (s *Server) handleAuthSignOut(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuth(w) {
		return
	}
	s.auth.SignOut(r.Context(), false /* clearKeychain */)
	writeJSON(w, http.StatusOK, authStateResp{s.auth.Snapshot()})
}

// handleAuthSignOutFull — POST /local/auth/sign-out-full. Clears the
// Keychain api_key too. Use for "switch account" flows.
func (s *Server) handleAuthSignOutFull(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuth(w) {
		return
	}
	s.auth.SignOut(r.Context(), true /* clearKeychain */)
	writeJSON(w, http.StatusOK, authStateResp{s.auth.Snapshot()})
}

// --- Helpers ---

func (s *Server) requireAuth(w http.ResponseWriter) bool {
	if s == nil || s.auth == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error":   "platform_unsupported",
			"message": "Email/password authentication requires macOS Keychain. Configure cfg.APIKey via setup wizard or upgrade to macOS.",
		})
		return false
	}
	return true
}

func decodeAuthBody(w http.ResponseWriter, r *http.Request, out any) bool {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error":   "invalid_request",
			"message": err.Error(),
		})
		return false
	}
	return true
}

// writeAuthError converts AuthManager / AuthClient errors to HTTP. For
// *client.AuthError we passthrough the HTTPCode so Desktop sees the same
// status Cloud returned; for IsErrPlatformUnsupported we return 503;
// everything else is 500.
func writeAuthError(w http.ResponseWriter, err error) {
	if IsErrPlatformUnsupported(err) {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error":   "platform_unsupported",
			"message": err.Error(),
		})
		return
	}
	var ae *client.AuthError
	if errors.As(err, &ae) {
		status := ae.HTTPCode
		if status == 0 {
			status = http.StatusBadGateway
		}
		writeJSON(w, status, map[string]string{
			"error":   ae.Code,
			"message": ae.Message,
		})
		return
	}
	writeJSON(w, http.StatusInternalServerError, map[string]string{
		"error":   "internal_error",
		"message": err.Error(),
	})
}
