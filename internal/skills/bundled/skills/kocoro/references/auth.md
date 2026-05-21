# Auth

## What is this?

Email/password authentication for Kocoro Desktop. The daemon proxies registration, login, verification, and password reset to Shannon Cloud (`apiv1.kocoro.ai/api/v1/auth/*`), stores the issued long-lived API key in the macOS Keychain (service `ai.kocoro.daemon.api_key`), and broadcasts state changes over `/events` SSE.

**Platform support**: macOS only. On Linux/Windows the daemon falls back to the legacy `~/.shannon/config.yaml` `api_key` path; `/local/auth/*` endpoints return 503 `platform_unsupported`.

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

All endpoints listen on `127.0.0.1:7533` (daemon HTTP). Localhost-only, no auth (the daemon is trusted-local; permission engine enforces tool gates).

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
- Response: 200 — anti-enumeration: Cloud always returns success.

### Forgot password
- Method: POST
- Path: `/local/auth/forgot-password`
- Body: `{"email": "..."}`
- Response: 200 — anti-enumeration. Daemon state is NOT changed (user may be signed_in / signed_out / pending_verification when initiating reset).

### Sign out
- Method: POST
- Path: `/local/auth/sign-out`
- Response: 200 + state snapshot (state="signed_out")
- Side effects: WS connection closed; access_token + refresh_token cleared from RAM; `cfg.APIKey` cleared in-memory; auth-sensitive tools (cloud_delegate, publish_to_web, etc.) rebuilt with empty key (effectively disabled). **Keychain api_key is preserved** — user can sign back in without re-typing credentials.

### Sign out (full clear)
- Method: POST
- Path: `/local/auth/sign-out-full`
- Response: 200 + state snapshot (state="signed_out")
- Same as sign-out plus: Keychain api_key + current_user_id are deleted. Use for "switch account" flows.

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
| `ai.kocoro.daemon.state` | `current_user_id` | Active user_id (selects which api_key entry is "live") |

To inspect from the command line:

```bash
security find-generic-password -s "ai.kocoro.daemon.api_key" -w
```

## Common Scenarios

### "Daemon won't connect to Cloud after restart"
Check `GET /local/auth/state`. If state is `signed_out` with `last_error_code: "invalid_api_key"`, the Keychain key was rejected by Cloud (revoked, password reset, etc.). User must POST /local/auth/login to obtain a fresh key.

If state is `signed_out` with `last_error_code: "ws_unauthorized"`, Cloud actively rejected the WebSocket upgrade with 401 — daemon auto-cleared Keychain. Same recovery: login again.

### "Switched to a different account"
POST /local/auth/sign-out-full → POST /local/auth/login with the new credentials. The simple sign-out preserves Keychain (intended for "log back in as the same user" flows) — sign-out-full is the right call for account swap.

### "Email verification pending forever"
Daemon stores `pending_email` in RAM only. If daemon restarts the field is empty, but the user can still POST /local/auth/login with their credentials — Cloud returns 403 `email_not_verified` which deterministically restores pending_verification state. The user can also POST /local/auth/resend-verification with the email explicitly.

### "Migration from old yaml-stored api_key"
On daemon startup (macOS only), config.yaml's `api_key` field is moved into Keychain under account `legacy` and the yaml field is stripped (backup written to `config.yaml.pre-migrate-<ts>.bak`). AuthManager.Bootstrap then calls /auth/me to resolve the real user_id and renames the entry. If /me returns 401 the migration leaves Keychain populated; the next login over the same key will adopt the entry properly.
