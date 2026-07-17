package daemon

import (
	"context"
	"net/http"
	"strings"
)

// refreshIntegrationToolsAsync re-syncs the local agent's integration tools
// after a connection change. Fired best-effort in the background so it never
// delays the HTTP response (RebuildAuthSensitiveTools is internally bounded).
//
// A `connect` returns an oauth_url and the connection only goes active AFTER
// the user completes OAuth in the browser — out of band from this daemon — so
// this call populates tools immediately only for a re-connect of an
// already-authorized provider. First-time activation reliably lands via the
// sign-in refresh (OnAPIKeyChanged), a POST /config/reload after the OAuth
// flow, or daemon restart. `delete` is immediate: the provider's tools are
// dropped on the next refresh.
func (s *Server) refreshIntegrationToolsAsync() {
	go s.RebuildAuthSensitiveTools(context.Background())
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

// handleConnectIntegration proxies POST /integrations/{provider}/connect to
// Cloud. Cloud starts the OAuth flow and returns 201 {connection_id, oauth_url}
// the renderer opens to complete authorization.
func (s *Server) handleConnectIntegration(w http.ResponseWriter, r *http.Request) {
	if !s.integrationsCloudReady(w) {
		return
	}
	provider := strings.TrimSpace(r.PathValue("provider"))
	if provider == "" {
		writeError(w, http.StatusBadRequest, "provider is required")
		return
	}
	status, body, err := s.deps.GW.IntegrationConnect(r.Context(), provider)
	if err != nil {
		writeError(w, http.StatusBadGateway, "cloud request failed: "+err.Error())
		return
	}
	writeCloudPassthrough(w, status, body)
	if status >= 200 && status < 300 {
		s.refreshIntegrationToolsAsync()
	}
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
