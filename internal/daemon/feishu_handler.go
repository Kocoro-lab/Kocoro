package daemon

import (
	"net/http"
	"strings"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

// feishuCloudReady gates the Feishu/Lark app-install endpoints: they all proxy
// to Shannon Cloud with the user's API key, so cloud must be enabled, a key must
// be present, and the gateway client must exist. Writes a 503 and returns false
// when any precondition is missing.
func (s *Server) feishuCloudReady(w http.ResponseWriter) bool {
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

// writeCloudPassthrough relays Cloud's status code and raw body verbatim so the
// LLM sees Cloud's field-level errors unchanged, without this layer re-modelling
// them. Shared by the Feishu/Slack/WeChat channel-setup proxy handlers.
func writeCloudPassthrough(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// handleCreateFeishuAppInstall proxies POST /channels/feishu/app-installs to
// Cloud's POST /api/v1/channels/feishu/app-installs using the current user's
// API key. It lets the LLM (guided by the kocoro skill) register a self-built
// Feishu/Lark application from chat without ever handling the daemon's API key
// itself — credentials only travel over localhost to here, and the key is
// attached server-side by the gateway client.
//
// New-architecture note: the bot connects over a Cloud-driven larkws long
// connection, so there is no Encrypt Key and no webhook URL to configure.
// Request body: {channel_type, app_id, app_secret, agent_name?, display_name?}.
// channel_type MUST be "feishu" or "lark"; agent_name is optional (empty lets
// Cloud bind the default agent); display_name is optional.
func (s *Server) handleCreateFeishuAppInstall(w http.ResponseWriter, r *http.Request) {
	if !s.feishuCloudReady(w) {
		return
	}

	var req client.FeishuAppInstallRequest
	if !decodeBody(w, r, &req) {
		return
	}
	req.ChannelType = strings.TrimSpace(req.ChannelType)
	req.AppID = strings.TrimSpace(req.AppID)
	req.AppSecret = strings.TrimSpace(req.AppSecret)
	req.AgentName = strings.TrimSpace(req.AgentName)
	req.DisplayName = strings.TrimSpace(req.DisplayName)

	// Validate required fields locally so a misformed call gets a clear error
	// without a Cloud round trip. channel_type is required (no default): the
	// user must say whether this is Feishu or Lark.
	switch req.ChannelType {
	case "feishu", "lark":
		// ok
	default:
		writeError(w, http.StatusBadRequest,
			"channel_type is required and must be 'feishu' or 'lark'")
		return
	}
	if req.AppID == "" || req.AppSecret == "" {
		writeError(w, http.StatusBadRequest,
			"app_id and app_secret are both required")
		return
	}

	status, body, err := s.deps.GW.CreateFeishuAppInstall(r.Context(), req)
	if err != nil {
		writeError(w, http.StatusBadGateway, "cloud request failed: "+err.Error())
		return
	}
	writeCloudPassthrough(w, status, body)
}

// handleListFeishuAppInstalls proxies GET /channels/feishu/app-installs to Cloud.
// The LLM uses this to show the user their registered bots and to find the
// install id required to unbind one via DELETE.
func (s *Server) handleListFeishuAppInstalls(w http.ResponseWriter, r *http.Request) {
	if !s.feishuCloudReady(w) {
		return
	}
	status, body, err := s.deps.GW.ListFeishuAppInstalls(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, "cloud request failed: "+err.Error())
		return
	}
	writeCloudPassthrough(w, status, body)
}

// handleDeleteFeishuAppInstall proxies DELETE /channels/feishu/app-installs/{id}
// to Cloud, unbinding a registered Feishu/Lark bot. The id comes from the list
// endpoint above. This is destructive — the kocoro skill confirms with the user
// before calling it.
func (s *Server) handleDeleteFeishuAppInstall(w http.ResponseWriter, r *http.Request) {
	if !s.feishuCloudReady(w) {
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}
	status, body, err := s.deps.GW.DeleteFeishuAppInstall(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusBadGateway, "cloud request failed: "+err.Error())
		return
	}
	writeCloudPassthrough(w, status, body)
}
