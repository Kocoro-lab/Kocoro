package daemon

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

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
		// Auth-managed daemons cannot infer which long-lived realtime session
		// produced a legacy report after an account switch. Only the legacy
		// endpoint/key deployment, whose credential is process-scoped and does
		// not support in-process account switching, may derive this binding.
		if s.auth != nil {
			writeError(w, http.StatusBadRequest, "realtime usage principal required")
			return
		}
		var ok bool
		principal, ok = s.realtimeUsagePrincipal()
		if !ok {
			writeError(w, http.StatusBadRequest, "realtime usage principal required")
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
