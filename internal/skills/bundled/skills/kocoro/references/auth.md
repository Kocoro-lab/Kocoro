# Auth

## What is this?

Email/password authentication for Kocoro Desktop. The daemon proxies registration, login, verification, and password reset to Shannon Cloud (`apiv1.kocoro.ai/api/v1/auth/*`), stores the issued long-lived API key in the OS credential store (service `ai.kocoro.daemon.api_key` — macOS Keychain or Windows Credential Manager), and broadcasts state changes over `/events` SSE.

**Platform support**: macOS and Windows. On Linux (and other platforms without an OS credential store) the daemon falls back to the legacy `~/.shannon/config.yaml` `api_key` path; `/local/auth/*` endpoints return 503 `platform_unsupported`.

## State Machine

```
        signed_out ─────────────┐
            │                   │
   POST /login                  │
   POST /register               │
            ▼                   │
       logging_in       pending_verification ◄─── (resend, /register 202)
            │                   ▲
   ┌────────┼───────────────────┘ (Cloud 403 email_not_verified)
   │        │
   │   bootstrapping_key  (Cloud /login 200; daemon calling /auth/api-keys)
   │        │
   │        ▼
   └─►  signed_in   ─── sign-out / WS 401 ──► signed_out
            │
        WS connected to Cloud
```

## API Endpoints

All endpoints listen on `127.0.0.1:7533` (daemon HTTP). Localhost-only, no auth (the daemon is trusted-local; permission engine enforces tool gates). All POST endpoints require `Content-Type: application/json`, including endpoints with an empty body; missing or different media types return 415 `unsupported_media_type`.

### Get current auth state
- Method: GET
- Path: `/local/auth/state`
- Response: `{"state": "signed_in", "user": {...}, "pending_email": "", "last_error_code": "", "updated_at": "..."}`
- Notes: Idempotent; safe to poll on startup before subscribing to /events.

### Register
- Method: POST
- Path: `/local/auth/register`
- Body: `{"email": "...", "password": "...", "name": "...", "preferred_language": "zh-CN"}`
- Response: 202 + state snapshot (state="pending_verification")
- Cloud errors (passthrough HTTP code): `invalid_email` 400, `weak_password` 400, `email_taken` 409, `rate_limited` 429.

### Login
- Method: POST
- Path: `/local/auth/login`
- Body: `{"email": "...", "password": "..."}`
- Response: 200 + state snapshot (state="signed_in")
- Side effects on success: api_key bootstrapped into Keychain (if absent), GatewayClient + WS Client receive the key, WS connection starts.
- Cloud errors:
  - 401 `invalid_credentials`
  - 403 `email_not_verified` (daemon also transitions to pending_verification; UI should surface "Resend verification" button)
  - 429 `rate_limited`
- Concurrent attempts for the same email are coalesced via singleflight — duplicate POSTs return the same result without hammering Cloud.

### Resend verification email
- Method: POST
- Path: `/local/auth/resend-verification`
- Body: `{"email": "..."}` — or empty body uses pending_email captured during register/login.
- Invalid JSON returns 400 `invalid_request`.
- Response: 200 — anti-enumeration: Cloud always returns success.

### Forgot password
- Method: POST
- Path: `/local/auth/forgot-password`
- Body: `{"email": "..."}`
- Response: 200 — anti-enumeration. Daemon state is NOT changed (user may be signed_in / signed_out / pending_verification when initiating reset).

### Sign out
- Method: POST
- Path: `/local/auth/sign-out`
- Body: empty JSON request body is allowed, but still send `Content-Type: application/json`.
- Response: 200 + state snapshot (state="signed_out")
- Side effects: WS connection closed; access_token + refresh_token cleared from RAM; `cfg.APIKey` cleared in-memory; auth-sensitive tools (cloud_delegate, publish_to_web, etc.) rebuilt with empty key (effectively disabled); `current_user_id` cleared so daemon restart stays signed_out. **The per-user Keychain api_key entry is preserved** — after the user enters email/password again, daemon can reuse the existing key instead of minting a new one.

### Sign out (full clear)
- Method: POST
- Path: `/local/auth/sign-out-full`
- Body: empty JSON request body is allowed, but still send `Content-Type: application/json`.
- Response: 200 + state snapshot (state="signed_out")
- Same as sign-out plus: Keychain api_key + current_user_id are deleted. Use for "switch account" flows.

### Adopt key (Google / external OAuth)
- Method: POST
- Path: `/local/auth/adopt-key`
- Body: `{"api_key": "sk_..."}`
- Response: 200 + state snapshot (state="signed_in")
- Purpose: install an externally-obtained api_key into the daemon's live auth. A login flow that exchanges credentials with Cloud directly (Google/OAuth) and never transits `/local/auth/login` has the key in hand but no way to reach `applyAPIKey` — on macOS the daemon owns the live key via Keychain + AuthManager, and `PATCH /config` cannot update it (`api_key` is a protected field and config reload is a no-op for the in-process key). Without this endpoint a post-logout re-login leaves the GatewayClient authenticating with an empty key (Cloud returns 401; the user appears as the free/anonymous tier). adopt-key converges that flow onto the SAME post-login state as email login.
- Side effects on success: validates the key against Cloud (`/auth/me`), writes it to the Keychain under the resolved `user_id`, applies it to GatewayClient + WS Client, transitions to `signed_in`, starts the WS connection. Safe while already signed_in (account switch): WS is restarted around the key swap and stale email JWTs are cleared.
- Hardening: the key is validated with Cloud BEFORE any mutation. On an invalid key or a `/auth/me` response with no resolvable `user_id`, the daemon stores nothing and leaves the current/pending auth state untouched.
- Errors:
  - 400 `invalid_request` (missing / empty `api_key`)
  - 401 `invalid_api_key` (Cloud rejected the key — passthrough)
  - 503 `platform_unsupported` (Linux & others without an OS credential store; macOS + Windows are supported; legacy `cfg.APIKey` yaml path applies)
- Client note: a **404** here means the daemon predates this endpoint — that (or an unreachable daemon) is the ONLY signal a client should use to fall back to the legacy config.yaml path. A 401 / 500 is a hard failure and must NOT fall back.

## Events on /events SSE stream

`auth_state_changed` — emitted on every state transition. Payload:

```json
{
  "state": "signed_in",
  "user": {
    "id": "user-uuid",
    "email": "alice@example.com",
    "email_verified": true,
    "name": "Alice"
  },
  "pending_email": "",
  "last_error_code": "",
  "updated_at": "2026-05-19T12:34:56Z"
}
```

`last_error_code` carries the most recent terminal-transition Cloud error code (e.g. `invalid_credentials`, `email_not_verified`) so UIs can render targeted hint text. It clears on the next successful transition.

**Recommended Desktop boot sequence**: `GET /local/auth/state` for the initial snapshot, then subscribe to `/events` for increments. Don't rely on events alone — transitions emitted during daemon startup (before SSE is subscribed) only live in the in-memory ring.

## Keychain Layout

| Service | Account | Value |
|---------|---------|-------|
| `ai.kocoro.daemon.api_key` | `<user_id>` (UUID) | Long-lived `sk_…` Cloud API key |
| `ai.kocoro.daemon.api_key` | `legacy` | Placeholder used by the yaml→Keychain migration; renamed to `<user_id>` after first /auth/me call |
| `ai.kocoro.daemon.state` | `current_user_id` | Active user_id (selects which api_key entry is "live"); cleared by normal sign-out |

To inspect from the command line:

```bash
security find-generic-password -s "ai.kocoro.daemon.api_key" -w
```

## Common Scenarios

### "Daemon won't connect to Cloud after restart"
Check `GET /local/auth/state`. If state is `signed_out` with `last_error_code: "invalid_api_key"`, the Keychain key was rejected by Cloud (revoked, password reset, etc.). User must POST /local/auth/login to obtain a fresh key.

If state is `signed_out` with `last_error_code: "ws_unauthorized"`, Cloud actively rejected the WebSocket upgrade with 401 — daemon auto-cleared Keychain. Same recovery: login again.

### "Switched to a different account"
POST /local/auth/sign-out-full → POST /local/auth/login with the new credentials. The simple sign-out preserves the prior per-user Keychain entry (intended for "log back in as the same user" flows), while clearing `current_user_id` so daemon restart does not auto-sign in — sign-out-full is the right call for account swap.

### "Email verification pending forever"
Daemon stores `pending_email` in RAM only. If daemon restarts the field is empty, but the user can still POST /local/auth/login with their credentials — Cloud returns 403 `email_not_verified` which deterministically restores pending_verification state. The user can also POST /local/auth/resend-verification with the email explicitly.

### "Migration from old yaml-stored api_key"
On daemon startup (macOS + Windows), config.yaml's `api_key` field is moved into the OS credential store under account `legacy` and the yaml field is stripped (backup written to `config.yaml.pre-migrate-<ts>.bak`). AuthManager.Bootstrap then calls /auth/me to resolve the real user_id and renames the entry. If /me returns 401 the migration leaves the credential store populated; the next login over the same key will adopt the entry properly.
