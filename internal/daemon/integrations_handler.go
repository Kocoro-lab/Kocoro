package daemon

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

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
	// Serialize with other live-registry refreshes so overlapping calls can't
	// apply stale snapshots out of order.
	s.toolRefreshMu.Lock()
	defer s.toolRefreshMu.Unlock()
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
	s.syncGatewayOverlay(reg)
	return err
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
	status, body, err := s.deps.GW.IntegrationConnect(r.Context(), provider, reqBody)
	if err != nil {
		writeError(w, http.StatusBadGateway, "cloud request failed: "+err.Error())
		return
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
