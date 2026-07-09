package daemon

import (
	"net/http"
	"strings"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

// This file implements the bring-your-own-app Slack surface as a thin proxy to
// Shannon Cloud (mirrors feishu_handler.go). The renderer only ever talks to
// localhost; the daemon attaches the user's API key and forwards to Cloud,
// which owns the per-app OAuth exchange + Events API ingestion (the daemon has
// no public URL, so it cannot host those itself). The legacy shared-app Slack
// OAuth flow and the Feishu proxy are untouched.

// slackCloudReady gates the Slack app-install proxy endpoints: they forward to
// Shannon Cloud with the user's API key, so cloud must be enabled, a key must
// be present, and the gateway client must exist.
func (s *Server) slackCloudReady(w http.ResponseWriter) bool {
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

// handleSlackCloudInfo returns the Cloud base URL so the renderer can build the
// Slack app manifest's Events request_url + OAuth redirect_url (both must point
// at this Cloud). GET /channels/slack/cloud-info.
func (s *Server) handleSlackCloudInfo(w http.ResponseWriter, _ *http.Request) {
	if !s.slackCloudReady(w) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"base_url": s.deps.GW.CloudBaseURL()})
}

// handleCreateSlackAppInstall proxies POST /channels/slack/app-installs to
// Cloud. Request body: {app_id, client_id, client_secret, signing_secret,
// agent_name?, display_name?}. Cloud stores the app and returns an
// authorize_url the renderer opens to complete OAuth.
func (s *Server) handleCreateSlackAppInstall(w http.ResponseWriter, r *http.Request) {
	if !s.slackCloudReady(w) {
		return
	}
	var req client.SlackAppInstallRequest
	if !decodeBody(w, r, &req) {
		return
	}
	req.AppID = strings.TrimSpace(req.AppID)
	req.ClientID = strings.TrimSpace(req.ClientID)
	req.ClientSecret = strings.TrimSpace(req.ClientSecret)
	req.SigningSecret = strings.TrimSpace(req.SigningSecret)
	req.AgentName = strings.TrimSpace(req.AgentName)
	req.DisplayName = strings.TrimSpace(req.DisplayName)

	if req.AppID == "" || req.ClientID == "" || req.ClientSecret == "" || req.SigningSecret == "" {
		writeError(w, http.StatusBadRequest,
			"app_id, client_id, client_secret and signing_secret are all required")
		return
	}

	status, body, err := s.deps.GW.CreateSlackAppInstall(r.Context(), req)
	if err != nil {
		writeError(w, http.StatusBadGateway, "cloud request failed: "+err.Error())
		return
	}
	writeCloudPassthrough(w, status, body)
}

// handleListSlackAppInstalls proxies GET /channels/slack/app-installs to Cloud.
func (s *Server) handleListSlackAppInstalls(w http.ResponseWriter, r *http.Request) {
	if !s.slackCloudReady(w) {
		return
	}
	status, body, err := s.deps.GW.ListSlackAppInstalls(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, "cloud request failed: "+err.Error())
		return
	}
	writeCloudPassthrough(w, status, body)
}

// handleDeleteSlackAppInstall proxies DELETE /channels/slack/app-installs/{id}.
func (s *Server) handleDeleteSlackAppInstall(w http.ResponseWriter, r *http.Request) {
	if !s.slackCloudReady(w) {
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}
	status, body, err := s.deps.GW.DeleteSlackAppInstall(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusBadGateway, "cloud request failed: "+err.Error())
		return
	}
	writeCloudPassthrough(w, status, body)
}
