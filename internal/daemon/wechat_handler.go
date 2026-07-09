package daemon

import (
	"net/http"
	"strings"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

// wechat_handler.go implements the personal-WeChat (iLink) surface as a thin
// proxy to Shannon Cloud (mirrors slack_handler.go / feishu_handler.go). The
// renderer only ever talks to localhost; the daemon attaches the user's API key
// and forwards to Cloud, which owns the iLink QR-login, the long-poll message
// pump, and install persistence (the daemon has no public URL and cannot host
// those). WeChat connects by QR scan, so the create flow is two proxied calls —
// qr-start then a polled qr-wait — rather than a credential POST.

// wechatCloudReady gates the WeChat proxy endpoints: they forward to Cloud with
// the user's API key, so cloud must be enabled, a key must be present, and the
// gateway client must exist.
func (s *Server) wechatCloudReady(w http.ResponseWriter) bool {
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

// handleWeChatQRStart proxies POST /channels/wechat/qr-start to Cloud. Request
// body: {agent_name?, display_name?}. Cloud fetches an iLink QR code and returns
// {session_key, qrcode, qrcode_img} the renderer displays for scanning.
func (s *Server) handleWeChatQRStart(w http.ResponseWriter, r *http.Request) {
	if !s.wechatCloudReady(w) {
		return
	}
	var req client.WeChatQRStartRequest
	if !decodeBody(w, r, &req) {
		return
	}
	req.AgentName = strings.TrimSpace(req.AgentName)
	req.DisplayName = strings.TrimSpace(req.DisplayName)

	status, body, err := s.deps.GW.WeChatQRStart(r.Context(), req)
	if err != nil {
		writeError(w, http.StatusBadGateway, "cloud request failed: "+err.Error())
		return
	}
	writeCloudPassthrough(w, status, body)
}

// handleWeChatQRWait proxies POST /channels/wechat/qr-wait to Cloud. Request
// body: {session_key}. Cloud performs one long-poll of the iLink login status
// and returns {status, install?}; the renderer calls this repeatedly until
// status is "confirmed" or "expired".
func (s *Server) handleWeChatQRWait(w http.ResponseWriter, r *http.Request) {
	if !s.wechatCloudReady(w) {
		return
	}
	var req client.WeChatQRWaitRequest
	if !decodeBody(w, r, &req) {
		return
	}
	req.SessionKey = strings.TrimSpace(req.SessionKey)
	if req.SessionKey == "" {
		writeError(w, http.StatusBadRequest, "session_key is required")
		return
	}

	status, body, err := s.deps.GW.WeChatQRWait(r.Context(), req)
	if err != nil {
		writeError(w, http.StatusBadGateway, "cloud request failed: "+err.Error())
		return
	}
	writeCloudPassthrough(w, status, body)
}

// handleListWeChatInstalls proxies GET /channels/wechat/installs to Cloud.
func (s *Server) handleListWeChatInstalls(w http.ResponseWriter, r *http.Request) {
	if !s.wechatCloudReady(w) {
		return
	}
	status, body, err := s.deps.GW.ListWeChatInstalls(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, "cloud request failed: "+err.Error())
		return
	}
	writeCloudPassthrough(w, status, body)
}

// handleDeleteWeChatInstall proxies DELETE /channels/wechat/installs/{id} to Cloud.
func (s *Server) handleDeleteWeChatInstall(w http.ResponseWriter, r *http.Request) {
	if !s.wechatCloudReady(w) {
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}
	status, body, err := s.deps.GW.DeleteWeChatInstall(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusBadGateway, "cloud request failed: "+err.Error())
		return
	}
	writeCloudPassthrough(w, status, body)
}
