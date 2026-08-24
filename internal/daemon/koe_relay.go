package daemon

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

// realtimeUsageLegacyBinding is the daemon-local identity for a pre-principal
// Koe process. Older clients cannot echo the bootstrap principal, so the first
// headerless report is admitted only against the currently verified account and
// its auth epoch. Keeping that epoch pinned makes a later account switch fail
// closed instead of rebinding an old session's report to the new account.
type realtimeUsageLegacyBinding struct {
	auth      *AuthManager
	gateway   *client.GatewayClient
	accountID string
	epoch     uint64
	principal string
}

func (s *Server) realtimeUsageLegacyPrincipal() (string, bool) {
	if s == nil || s.auth == nil || s.deps == nil || s.deps.GW == nil {
		return "", false
	}
	accountID, epoch, ok := s.auth.VerifiedPrincipal()
	if !ok {
		return "", false
	}
	boundAccount, bound := s.deps.GW.IntegrationPrincipal()
	if !bound || boundAccount != accountID {
		return "", false
	}
	candidate := realtimeUsageLegacyBinding{
		auth:      s.auth,
		gateway:   s.deps.GW,
		accountID: accountID,
		epoch:     epoch,
		principal: realtimeUsagePrincipalFingerprint("account", accountID),
	}
	s.realtimeUsageLegacyMu.Lock()
	defer s.realtimeUsageLegacyMu.Unlock()
	if s.realtimeUsageLegacyBound {
		if s.realtimeUsageLegacy != candidate {
			return "", false
		}
		return candidate.principal, true
	}
	s.realtimeUsageLegacy = candidate
	s.realtimeUsageLegacyBound = true
	return candidate.principal, true
}

// handleKoeRealtimeMint relays Koe's request for a Realtime ephemeral
// client secret to the Cloud gateway. Koe (the voice front brain) runs as a
// separate `shan koe` process and must never hold a long-lived credential — it
// asks the daemon, which mints via Cloud using its own API key (the via-daemon
// design; Koe never sees the provider key). This replaces C-minimal's direct
// dev-key mint. The gateway's {value, expires_at, session} shape is forwarded
// with an opaque usage-principal binding. Localhost-only, like the rest of the daemon HTTP surface.
func (s *Server) handleKoeRealtimeMint(w http.ResponseWriter, r *http.Request) {
	gw := s.cloudGateway()
	if gw == nil {
		writeError(w, http.StatusServiceUnavailable, "cloud not configured (sign in, or set cloud.enabled + api_key)")
		return
	}
	var req struct {
		Model string `json:"model"`
		Voice string `json:"voice"`
	}
	// Both fields are optional — the gateway defaults the allowlisted model.
	_ = json.NewDecoder(r.Body).Decode(&req)

	raw, principal, err := s.withRealtimeUsageGatewayLease(gw, func(string) (json.RawMessage, error) {
		return gw.MintRealtime(r.Context(), req.Model, req.Voice)
	})
	if err != nil {
		// Forward the Cloud status so Koe distinguishes 503 (no OpenAI key) /
		// 400 (model not allowlisted) from a flat relay failure.
		var apiErr *client.APIError
		if errors.As(err, &apiErr) {
			writeError(w, apiErr.StatusCode, "mint failed: "+apiErr.Body)
			return
		}
		writeError(w, http.StatusBadGateway, "mint relay failed: "+err.Error())
		return
	}
	raw, err = addRealtimeUsagePrincipal(raw, principal)
	if err != nil {
		writeError(w, http.StatusBadGateway, "mint response invalid")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(raw)
}

func (s *Server) handleKoeRealtimeSDP(w http.ResponseWriter, r *http.Request) {
	gw := s.cloudGateway()
	if gw == nil {
		writeError(w, http.StatusServiceUnavailable, "cloud not configured (sign in, or set cloud.enabled + api_key)")
		return
	}
	var req struct {
		Provider string `json:"provider"`
		Model    string `json:"model"`
		OfferSDP string `json:"offer_sdp"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid SDP request")
		return
	}
	raw, principal, err := s.withRealtimeUsageGatewayLease(gw, func(string) (json.RawMessage, error) {
		return gw.ExchangeRealtimeSDP(r.Context(), req.Provider, req.Model, req.OfferSDP)
	})
	if err != nil {
		var apiErr *client.APIError
		if errors.As(err, &apiErr) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(apiErr.StatusCode)
			_, _ = w.Write([]byte(apiErr.Body))
			return
		}
		writeError(w, http.StatusBadGateway, "SDP relay failed: "+err.Error())
		return
	}
	raw, err = addRealtimeUsagePrincipal(raw, principal)
	if err != nil {
		writeError(w, http.StatusBadGateway, "SDP response invalid")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(raw)
}

// handleKoeRealtimeUsage durably accepts Koe's realtime usage report (from a
// response.done event: model, response_id, token details). The daemon returns
// after the private outbox handoff; its worker forwards the raw body to Cloud,
// where cost and quota are computed server-side.
func (s *Server) handleKoeRealtimeUsage(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, realtimeUsageMaxBodyBytes))
	if err != nil {
		writeError(w, http.StatusBadRequest, "read usage body: "+err.Error())
		return
	}
	if _, err := realtimeUsageResponseID(body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid usage body")
		return
	}
	principal := strings.TrimSpace(r.Header.Get(client.RealtimeUsagePrincipalHeader))
	if principal == "" {
		var ok bool
		if s.auth != nil {
			// Rolling upgrades leave an older Koe process without the principal
			// header. Admit its first report only under the verified local
			// account+epoch captured by realtimeUsageLegacyPrincipal; a later
			// account switch therefore cannot rebind the old process to the new
			// account.
			principal, ok = s.realtimeUsageLegacyPrincipal()
		} else {
			principal, ok = s.realtimeUsagePrincipal()
		}
		if !ok {
			// 503 keeps pre-upgrade clients' retry path alive while the daemon
			// is signed out, switching accounts, or still binding its gateway.
			writeError(w, http.StatusServiceUnavailable, "realtime usage principal unavailable")
			return
		}
	} else if !validRealtimeUsagePrincipal(principal) {
		writeError(w, http.StatusBadRequest, "invalid realtime usage principal")
		return
	}
	outbox := s.realtimeUsageOutboxStore()
	if outbox == nil {
		writeError(w, http.StatusServiceUnavailable, "realtime usage persistence is not configured")
		return
	}
	created, err := outbox.enqueue(body, principal)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "realtime usage persistence failed")
		return
	}
	if !created {
		writeJSON(w, http.StatusOK, map[string]string{"status": "queued", "deduplicated": "true"})
		return
	}
	// The durable handoff is the request's success boundary. Cloud delivery is
	// owned by the background worker so a Koe process can safely return from its
	// synchronous report call without waiting for an external provider.
	writeJSON(w, http.StatusOK, map[string]string{"status": "queued"})
}
