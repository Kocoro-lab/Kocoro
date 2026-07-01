package daemon

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

// handleKoeRealtimeMint relays Koe's request for an OpenAI Realtime ephemeral
// client secret to the Cloud gateway. Koe (the voice front brain) runs as a
// separate `shan koe` process and must never hold a long-lived credential — it
// asks the daemon, which mints via Cloud using its own API key (the via-daemon
// design; Koe never sees the OpenAI key). This replaces C-minimal's direct
// dev-key mint. The gateway's {value, expires_at, session} body is forwarded
// verbatim. Localhost-only, like the rest of the daemon HTTP surface.
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

	raw, err := gw.MintRealtime(r.Context(), req.Model, req.Voice)
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
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(raw)
}

// handleKoeAudition synthesizes a short TTS sample of a voice so Settings can
// preview it ("listen to this voice"). Mirrors handleKoeRealtimeMint: the daemon
// holds no OpenAI key, so it relays through the Cloud gateway (which pins the TTS
// model and resolves the user from X-API-Key). The audio bytes are streamed back
// to the Desktop verbatim with their Content-Type. Unlike the realtime path, this
// is a one-shot TTS clip, not a live session — the previewed timbre is close to
// but not identical to the realtime voice (separate OpenAI model).
func (s *Server) handleKoeAudition(w http.ResponseWriter, r *http.Request) {
	gw := s.cloudGateway()
	if gw == nil {
		writeError(w, http.StatusServiceUnavailable, "cloud not configured (sign in, or set cloud.enabled + api_key)")
		return
	}
	var req struct {
		Voice string `json:"voice"`
		Text  string `json:"text"`
	}
	// Both optional — the gateway/llm-service defaults the sample line and rejects
	// a voice outside its allowlist.
	_ = json.NewDecoder(r.Body).Decode(&req)

	audio, contentType, err := gw.SynthesizeSpeech(r.Context(), req.Voice, req.Text)
	if err != nil {
		var apiErr *client.APIError
		if errors.As(err, &apiErr) {
			writeError(w, apiErr.StatusCode, "audition failed: "+apiErr.Body)
			return
		}
		writeError(w, http.StatusBadGateway, "audition relay failed: "+err.Error())
		return
	}
	w.Header().Set("Content-Type", contentType)
	_, _ = w.Write(audio)
}

// handleKoeRealtimeUsage relays Koe's realtime usage report (from a response.done
// event: model, response_id, token details) to the Cloud usage-ingest endpoint
// via the daemon's API key. Koe never holds a credential and never sees pricing —
// it sends token counts, Cloud computes cost server-side + debits quota. Mirrors
// handleKoeRealtimeMint. The Cloud {cost_usd, billable_tokens, ...} body is
// forwarded verbatim (Koe ignores it; it's for observability).
func (s *Server) handleKoeRealtimeUsage(w http.ResponseWriter, r *http.Request) {
	gw := s.cloudGateway()
	if gw == nil {
		writeError(w, http.StatusServiceUnavailable, "cloud not configured (sign in, or set cloud.enabled + api_key)")
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "read usage body: "+err.Error())
		return
	}
	raw, err := gw.SendRealtimeUsage(r.Context(), body)
	if err != nil {
		var apiErr *client.APIError
		if errors.As(err, &apiErr) {
			writeError(w, apiErr.StatusCode, "usage ingest failed: "+apiErr.Body)
			return
		}
		writeError(w, http.StatusBadGateway, "usage relay failed: "+err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(raw)
}
