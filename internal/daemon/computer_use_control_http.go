package daemon

import (
	"context"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/guicontrol"
)

// These versioned control payloads are below 1 KiB and have no extensible
// content fields. The cap prevents an unauthenticated-size memory allocation;
// it has no override because a larger body cannot be valid for this schema.
const (
	maxComputerUseControlBodyBytes   = 16 << 10
	defaultComputerUseExpiryInterval = time.Second
)

func (s *Server) runComputerUseExpiryLoop(ctx context.Context) {
	interval := s.computerUseExpiryInterval
	if interval <= 0 {
		interval = defaultComputerUseExpiryInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if s.computerUseCoordinator != nil {
				s.computerUseCoordinator.ExpireStale()
			}
		}
	}
}

func requireComputerUseLocalPresence(w http.ResponseWriter, r *http.Request) bool {
	if localPresenceAuthorized(r) {
		return true
	}
	writeError(w, http.StatusForbidden, "local presence confirmation required")
	return false
}

func (s *Server) computerUseCoordinatorOrError(w http.ResponseWriter) *guicontrol.Coordinator {
	if s != nil && s.computerUseCoordinator != nil {
		return s.computerUseCoordinator
	}
	writeErrorCode(
		w,
		http.StatusServiceUnavailable,
		"computer_use_control_unavailable",
		"computer-use control is unavailable")
	return nil
}

func readComputerUseControlBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	body := http.MaxBytesReader(w, r.Body, maxComputerUseControlBodyBytes)
	defer body.Close()
	return io.ReadAll(body)
}

func writeComputerUseWireJSON(w http.ResponseWriter, status int, payload []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(payload)
}

func (s *Server) handleComputerUseMethodNotAllowed(allow string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireComputerUseLocalPresence(w, r) {
			return
		}
		w.Header().Set("Allow", allow)
		writeErrorCode(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
	}
}

func (s *Server) handleComputerUseActivity(w http.ResponseWriter, r *http.Request) {
	if !requireComputerUseLocalPresence(w, r) {
		return
	}
	coordinator := s.computerUseCoordinatorOrError(w)
	if coordinator == nil {
		return
	}
	// Handler() is intentionally ticker-free for offline tests and embedded
	// callers, so GET remains an authoritative lazy-expiry boundary too.
	coordinator.ExpireStale()
	payload, err := guicontrol.EncodeComputerUseActivitySnapshot(coordinator.Snapshot())
	if err != nil {
		writeErrorCode(
			w,
			http.StatusInternalServerError,
			"computer_use_control_failed",
			"computer-use activity snapshot is unavailable")
		return
	}
	writeComputerUseWireJSON(w, http.StatusOK, payload)
}

func (s *Server) handleComputerUseControl(w http.ResponseWriter, r *http.Request) {
	if !requireComputerUseLocalPresence(w, r) {
		return
	}
	coordinator := s.computerUseCoordinatorOrError(w)
	if coordinator == nil {
		return
	}
	payload, err := readComputerUseControlBody(w, r)
	if err != nil {
		writeErrorCode(
			w,
			http.StatusBadRequest,
			"invalid_computer_use_control_request",
			"invalid computer-use control request")
		return
	}
	request, err := guicontrol.DecodeComputerUseControlRequest(payload)
	if err != nil {
		writeErrorCode(
			w,
			http.StatusBadRequest,
			"invalid_computer_use_control_request",
			"invalid computer-use control request")
		return
	}
	response, err := coordinator.Control(request)
	if err != nil {
		writeComputerUseCoordinatorError(w, err)
		return
	}
	if request.Action == guicontrol.ComputerUseControlTakeOver {
		if err := coordinator.AwaitActionQuiescence(r.Context(), request.LeaseID); err != nil {
			writeComputerUseCoordinatorError(w, err)
			return
		}
		response.Quiesced = true
		if snapshot := coordinator.Snapshot(); snapshot.Active != nil &&
			snapshot.Active.LeaseID == request.LeaseID {
			response.Revision = snapshot.Revision
			response.LeaseState = snapshot.Active.LeaseState
		}
	}
	encoded, err := guicontrol.EncodeComputerUseControlResponse(response)
	if err != nil {
		writeErrorCode(
			w,
			http.StatusInternalServerError,
			"computer_use_control_failed",
			"computer-use control failed")
		return
	}
	writeComputerUseWireJSON(w, http.StatusOK, encoded)
}

func (s *Server) handleComputerUseHeartbeat(w http.ResponseWriter, r *http.Request) {
	if !requireComputerUseLocalPresence(w, r) {
		return
	}
	coordinator := s.computerUseCoordinatorOrError(w)
	if coordinator == nil {
		return
	}
	payload, err := readComputerUseControlBody(w, r)
	if err != nil {
		writeErrorCode(
			w,
			http.StatusBadRequest,
			"invalid_computer_use_heartbeat_request",
			"invalid computer-use heartbeat request")
		return
	}
	request, err := guicontrol.DecodeComputerUseHeartbeatRequest(payload)
	if err != nil {
		writeErrorCode(
			w,
			http.StatusBadRequest,
			"invalid_computer_use_heartbeat_request",
			"invalid computer-use heartbeat request")
		return
	}
	response, err := coordinator.Heartbeat(request.LeaseID)
	if err != nil {
		writeComputerUseCoordinatorError(w, err)
		return
	}
	encoded, err := guicontrol.EncodeComputerUseHeartbeatResponse(response)
	if err != nil {
		writeErrorCode(
			w,
			http.StatusInternalServerError,
			"computer_use_control_failed",
			"computer-use heartbeat failed")
		return
	}
	writeComputerUseWireJSON(w, http.StatusOK, encoded)
}

func writeComputerUseCoordinatorError(w http.ResponseWriter, err error) {
	var staleLease *guicontrol.StaleLeaseError
	var expired *guicontrol.LeaseExpiredError
	var stopped *guicontrol.StoppedTurnError
	var invalidTransition *guicontrol.InvalidTransitionError
	var idempotencyConflict *guicontrol.IdempotencyConflictError
	var actionInProgress *guicontrol.ActionInProgressError
	switch {
	case errors.As(err, &expired):
		writeErrorCode(w, http.StatusGone, "computer_use_lease_expired", "computer-use lease expired")
	case errors.As(err, &staleLease):
		writeErrorCode(w, http.StatusConflict, "computer_use_stale_lease", "computer-use lease is stale")
	case errors.As(err, &stopped):
		writeErrorCode(w, http.StatusConflict, "computer_use_stopped", "computer-use workflow was stopped")
	case errors.As(err, &invalidTransition):
		writeErrorCode(w, http.StatusConflict, "computer_use_invalid_transition", "computer-use control transition is invalid")
	case errors.As(err, &idempotencyConflict):
		writeErrorCode(w, http.StatusConflict, "computer_use_idempotency_conflict", "computer-use idempotency key conflicts with an earlier request")
	case errors.As(err, &actionInProgress):
		writeErrorCode(w, http.StatusConflict, "computer_use_action_in_progress", "computer-use action cancellation is still in progress")
	default:
		writeErrorCode(w, http.StatusInternalServerError, "computer_use_control_failed", "computer-use control failed")
	}
}
