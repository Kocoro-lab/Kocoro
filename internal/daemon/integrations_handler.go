package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/tools"
)

// RefreshIntegrationTools re-pulls the caller's active integration tools from
// Cloud and (re)registers them on the live agent registry. Lightweight: it only
// touches integration tools (unlike RebuildAuthSensitiveTools, which also
// re-registers publish/image/cloud_delegate; and unlike /config/reload, which
// restarts MCP subprocesses). Bounded so a slow/unavailable gateway can't stall
// the caller. No-op (nil) when deps aren't ready.
func (s *Server) RefreshIntegrationTools(ctx context.Context) error {
	if s == nil || s.deps == nil || s.deps.GW == nil {
		return nil
	}
	// Serialize the list/build/live-swap transaction with auth, MCP health, and
	// reload so no cached catalog can land across an identity transition.
	unlock := s.deps.LockToolRegistryMutation()
	defer unlock()
	_, reg, _ := s.deps.Snapshot() // read the registry pointer under deps.mu
	if reg == nil {
		return nil
	}
	itCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	err := tools.RegisterIntegrationTools(itCtx, s.deps.GW, reg)
	// Keep the cached overlay in sync so a later MCP health rebuild preserves
	// the integration tools registered above (RebuildRegistryForHealth rebuilds
	// the live registry from the cached GatewayOverlay).
	s.syncToolOverlays(reg)
	return err
}

// resetIntegrationToolsForPrincipal applies the strict identity boundary for
// Cloud-owned integration schemas. Unlike an ordinary refresh, an auth epoch
// transition must never retain tools fetched under the previous API key when
// the new identity's list call is unavailable. Clear the entire integration
// source atomically first; only then may the new verified principal populate
// its own catalog.
func (s *Server) resetIntegrationToolsForPrincipal(ctx context.Context, hasPrincipal bool) error {
	if s == nil || s.deps == nil {
		return nil
	}
	unlock := s.deps.LockToolRegistryMutation()
	defer unlock()
	_, reg, _ := s.deps.Snapshot()
	if reg == nil {
		return nil
	}
	reg.RemoveSource(agent.SourceIntegration)
	s.syncToolOverlays(reg)
	if !hasPrincipal || s.deps.GW == nil {
		return nil
	}
	itCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	err := tools.RegisterIntegrationTools(itCtx, s.deps.GW, reg)
	s.syncToolOverlays(reg)
	return err
}

// InvalidateIntegrationTools removes the old catalog after AuthManager has
// synchronously swapped the live key and invalidated every captured generation.
// Keeping the generation boundary first closes the registry-lock wait window;
// the verified-principal transition repopulates the catalog.
func (s *Server) InvalidateIntegrationTools() {
	_ = s.resetIntegrationToolsForPrincipal(context.Background(), false)
}

// refreshIntegrationToolsAsync fires RefreshIntegrationTools in the background
// so it never delays the HTTP response.
//
// For an OAuth provider, `connect` returns an oauth_url and the connection
// only goes active AFTER the user completes OAuth in the browser — out of band
// from this daemon — so this call populates tools immediately only for a
// re-connect of an already-authorized provider; first-time activation reliably
// lands via the explicit POST /integrations/refresh (Desktop calls it once the
// connection is confirmed active), the sign-in refresh (OnAPIKeyChanged), or
// daemon restart. For a token-mode provider (Shopify) the connection is active
// on the connect response itself, so this refresh registers its tools right
// away. `delete` is immediate: the provider's tools are dropped on the next
// refresh.
func (s *Server) refreshIntegrationToolsAsync() {
	go func() {
		if err := s.RefreshIntegrationTools(context.Background()); err != nil {
			log.Printf("daemon: integration tools refresh failed (continuing): %v", err)
		}
	}()
}

// This file implements the generic integrations surface as a thin proxy to
// Shannon Cloud (mirrors slack_handler.go). The renderer only ever talks to
// localhost; the daemon attaches the user's API key and forwards to Cloud,
// which owns the per-provider OAuth exchange (the daemon has no public URL, so
// it cannot host the callback itself).

// integrationsCloudReady gates the integrations proxy endpoints: they forward
// to Shannon Cloud with the user's API key, so cloud must be enabled, a key
// must be present, and the gateway client must exist.
func (s *Server) integrationsCloudReady(w http.ResponseWriter) bool {
	if !s.requireDeps(w) {
		return false
	}
	cfg, _, _ := s.deps.Snapshot()
	if cfg == nil || !cfg.Cloud.Enabled || s.liveAPIKey(cfg) == "" || s.deps.GW == nil {
		writeError(w, http.StatusServiceUnavailable,
			"cloud channels not configured (need cloud.enabled and api_key)")
		return false
	}
	return true
}

// maxIntegrationConnectBodyBytes bounds the connect request body forwarded to
// Cloud. Token-mode providers (Shopify) send a small {params:{shop,
// access_token}} object well under 1 KiB; 64 KiB leaves generous headroom for
// future provider params. When it binds, the handler returns 413 — bump this
// constant if a provider ever needs more.
const maxIntegrationConnectBodyBytes = 64 << 10

// maxIntegrationConnectLogBodyBytes caps each response-derived value quoted
// in the connect-failure log: the parsed error/message fields and the raw
// body fallback when the body doesn't parse as the Cloud error shape (e.g.
// an HTML page from an intermediary). Failure bodies on the contract path
// are tiny ({"error","message"}); when this binds the log value ends
// truncated — bump the constant if a diagnosis ever needs more. The cap is
// per-field, so a pathological error+message pair still yields a ~4 KiB line.
const maxIntegrationConnectLogBodyBytes = 2 << 10

func truncateForConnectLog(s string) string {
	if len(s) > maxIntegrationConnectLogBodyBytes {
		return s[:maxIntegrationConnectLogBodyBytes]
	}
	return s
}

// logIntegrationConnectFailure records a structured warn log for a failed
// connect passthrough so provider/status/error-code/message are diagnosable
// offline (Desktop only shows the message in a dialog). Response-side content
// only: the request body may carry credentials and is never logged. The
// parsed branch is safe by contract (Cloud keeps {"error": "<code>",
// "message": "<detail>"} failure bodies credential-free); a NON-contract body
// carries no such guarantee — an intermediary or framework validation error
// can echo request input (e.g. a pydantic 422 quotes the submitted value) —
// so the raw body is quoted only when the request had no body to echo
// (OAuth connects); token-mode failures degrade to the body length. provider
// is an untrusted percent-decoded path segment, so it is %q-quoted everywhere
// to keep newline injection out of the log stream.
func logIntegrationConnectFailure(provider string, status int, body []byte, requestHadBody bool, elapsed time.Duration) {
	var parsed struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &parsed); err == nil && (parsed.Error != "" || parsed.Message != "") {
		log.Printf("daemon: WARNING: integration connect rejected by cloud provider=%q status=%d error=%q message=%q elapsed_ms=%d",
			truncateForConnectLog(provider), status, truncateForConnectLog(parsed.Error), truncateForConnectLog(parsed.Message), elapsed.Milliseconds())
		return
	}
	if requestHadBody {
		log.Printf("daemon: WARNING: integration connect rejected by cloud provider=%q status=%d unparsed_body_len=%d elapsed_ms=%d",
			truncateForConnectLog(provider), status, len(body), elapsed.Milliseconds())
		return
	}
	log.Printf("daemon: WARNING: integration connect rejected by cloud provider=%q status=%d body=%q elapsed_ms=%d",
		truncateForConnectLog(provider), status, truncateForConnectLog(string(body)), elapsed.Milliseconds())
}

// handleConnectIntegration proxies POST /integrations/{provider}/connect to
// Cloud, forwarding the client's JSON body verbatim. OAuth providers send no
// body and get back {connection_id, oauth_url} the renderer opens to complete
// authorization; token-mode providers (e.g. Shopify) send credentials in the
// body and get back an active connection directly. The body may carry
// long-lived credentials — never log or persist it. The response side is a
// verbatim Cloud passthrough, so the end-to-end invariant also relies on the
// Cloud contract keeping connect error bodies credential-free (never quoting
// a rejected token back).
func (s *Server) handleConnectIntegration(w http.ResponseWriter, r *http.Request) {
	if !s.integrationsCloudReady(w) {
		return
	}
	provider := strings.TrimSpace(r.PathValue("provider"))
	if provider == "" {
		writeError(w, http.StatusBadRequest, "provider is required")
		return
	}
	bodyReader := http.MaxBytesReader(w, r.Body, maxIntegrationConnectBodyBytes)
	defer bodyReader.Close()
	reqBody, err := io.ReadAll(bodyReader)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "connect body exceeds 64 KiB cap")
			return
		}
		writeError(w, http.StatusBadRequest, "read request body: "+err.Error())
		return
	}
	// A whitespace-only payload counts as absent so a renderer quirk (empty
	// string, stray newline) cannot flip an OAuth connect onto the token-mode
	// wire shape.
	if len(bytes.TrimSpace(reqBody)) == 0 {
		reqBody = nil
	}
	start := time.Now()
	status, body, err := s.deps.GW.IntegrationConnect(r.Context(), provider, reqBody)
	if err != nil {
		log.Printf("daemon: WARNING: integration connect transport failure provider=%q err=%v elapsed_ms=%d",
			truncateForConnectLog(provider), err, time.Since(start).Milliseconds())
		writeError(w, http.StatusBadGateway, "cloud request failed: "+err.Error())
		return
	}
	if status < 200 || status >= 300 {
		logIntegrationConnectFailure(provider, status, body, reqBody != nil, time.Since(start))
	}
	writeCloudPassthrough(w, status, body)
	if status >= 200 && status < 300 {
		s.refreshIntegrationToolsAsync()
	}
}

// handleRefreshIntegrations handles POST /integrations/refresh: re-pull the
// caller's active integration tools into the local agent registry. Desktop
// calls this once a connection is confirmed active (or after a disconnect) so
// the tools appear/disappear immediately, without a full /config/reload. Runs
// synchronously so the caller knows the refresh completed.
func (s *Server) handleRefreshIntegrations(w http.ResponseWriter, r *http.Request) {
	if !s.integrationsCloudReady(w) {
		return
	}
	if err := s.RefreshIntegrationTools(r.Context()); err != nil {
		writeError(w, http.StatusBadGateway, "integration tools refresh failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "refreshed"})
}

// handleListIntegrations proxies GET /integrations to Cloud.
func (s *Server) handleListIntegrations(w http.ResponseWriter, r *http.Request) {
	if !s.integrationsCloudReady(w) {
		return
	}
	status, body, err := s.deps.GW.ListIntegrations(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, "cloud request failed: "+err.Error())
		return
	}
	writeCloudPassthrough(w, status, body)
}

// handleGetIntegration proxies GET /integrations/{id} to Cloud.
func (s *Server) handleGetIntegration(w http.ResponseWriter, r *http.Request) {
	if !s.integrationsCloudReady(w) {
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}
	status, body, err := s.deps.GW.GetIntegration(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusBadGateway, "cloud request failed: "+err.Error())
		return
	}
	writeCloudPassthrough(w, status, body)
}

// handleDeleteIntegration proxies DELETE /integrations/{id} to Cloud.
func (s *Server) handleDeleteIntegration(w http.ResponseWriter, r *http.Request) {
	if !s.integrationsCloudReady(w) {
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}
	status, body, err := s.deps.GW.DeleteIntegration(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusBadGateway, "cloud request failed: "+err.Error())
		return
	}
	writeCloudPassthrough(w, status, body)
	if status >= 200 && status < 300 {
		s.refreshIntegrationToolsAsync()
	}
}
