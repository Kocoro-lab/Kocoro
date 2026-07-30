package daemon

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
)

// A policy mutation contains only schema_version, bundle_id, and one enum;
// normal payloads are below 512 bytes. 16 KiB leaves ample forward-compatible
// room while bounding unauthenticated allocation. Oversize requests fail 400.
// There is intentionally no runtime override in V1; if the typed schema later
// grows legitimately, raise this named contract cap with fixture/client tests
// rather than introducing a generic daemon body-size knob.
const maxComputerUseAppPolicyBodyBytes = 16 << 10

type computerUseAppPolicyUpdateRequest struct {
	SchemaVersion int                          `json:"schema_version"`
	BundleID      string                       `json:"bundle_id"`
	Decision      ComputerUseAppPolicyDecision `json:"decision"`
}

type computerUseAppPolicyRevokeRequest struct {
	SchemaVersion int    `json:"schema_version"`
	BundleID      string `json:"bundle_id"`
}

func (s *Server) handleComputerUseAppPolicy(w http.ResponseWriter, r *http.Request) {
	if !requireComputerUseLocalPresence(w, r) {
		return
	}
	if !s.requireDeps(w) {
		return
	}
	store := s.deps.computerUseAppPolicyStore()
	switch r.Method {
	case http.MethodGet:
		snapshot, err := store.Snapshot()
		if err != nil {
			writeErrorCode(w, http.StatusServiceUnavailable, "computer_use_app_policy_unavailable", "computer-use app policy is unavailable")
			return
		}
		writeComputerUseAppPolicySnapshot(w, snapshot)
	case http.MethodPut:
		var request computerUseAppPolicyUpdateRequest
		if err := decodeComputerUseAppPolicyRequest(w, r, &request); err != nil ||
			request.SchemaVersion != computerUseAppPolicySchemaVersion ||
			request.Decision != ComputerUseAppPolicyAsk && request.Decision != ComputerUseAppPolicyBlocked {
			writeErrorCode(w, http.StatusBadRequest, "invalid_computer_use_app_policy_request", "invalid computer-use app policy request")
			return
		}
		normalized, err := normalizeComputerUseBundleID(request.BundleID)
		if err != nil || normalized != request.BundleID {
			writeErrorCode(w, http.StatusBadRequest, "invalid_computer_use_app_policy_request", "invalid computer-use app policy request")
			return
		}
		snapshot, err := store.Update(request.BundleID, request.Decision)
		if errors.Is(err, ErrComputerUseAppPolicyBuiltIn) {
			writeErrorCode(w, http.StatusConflict, "computer_use_app_policy_immutable", "built-in computer-use app policy is immutable")
			return
		}
		if err != nil {
			writeErrorCode(w, http.StatusServiceUnavailable, "computer_use_app_policy_unavailable", "computer-use app policy is unavailable")
			return
		}
		s.auditHTTPOp(http.MethodPut, "/local/computer-use/app-policy", "updated "+request.BundleID+" to "+string(request.Decision))
		writeComputerUseAppPolicySnapshot(w, snapshot)
	case http.MethodDelete:
		var request computerUseAppPolicyRevokeRequest
		if err := decodeComputerUseAppPolicyRequest(w, r, &request); err != nil || request.SchemaVersion != computerUseAppPolicySchemaVersion {
			writeErrorCode(w, http.StatusBadRequest, "invalid_computer_use_app_policy_request", "invalid computer-use app policy request")
			return
		}
		normalized, err := normalizeComputerUseBundleID(request.BundleID)
		if err != nil || normalized != request.BundleID {
			writeErrorCode(w, http.StatusBadRequest, "invalid_computer_use_app_policy_request", "invalid computer-use app policy request")
			return
		}
		snapshot, err := store.Revoke(request.BundleID)
		if errors.Is(err, ErrComputerUseAppPolicyBuiltIn) {
			writeErrorCode(w, http.StatusConflict, "computer_use_app_policy_immutable", "built-in computer-use app policy is immutable")
			return
		}
		if err != nil {
			writeErrorCode(w, http.StatusServiceUnavailable, "computer_use_app_policy_unavailable", "computer-use app policy is unavailable")
			return
		}
		s.auditHTTPOp(http.MethodDelete, "/local/computer-use/app-policy", "revoked "+request.BundleID)
		writeComputerUseAppPolicySnapshot(w, snapshot)
	default:
		w.Header().Set("Allow", "GET, PUT, DELETE")
		writeErrorCode(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
	}
}

func decodeComputerUseAppPolicyRequest(w http.ResponseWriter, r *http.Request, target any) error {
	body := http.MaxBytesReader(w, r.Body, maxComputerUseAppPolicyBodyBytes)
	defer body.Close()
	data, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	return decodeStrictComputerUseAppPolicyJSON(data, target)
}

func writeComputerUseAppPolicySnapshot(w http.ResponseWriter, snapshot ComputerUseAppPolicySnapshot) {
	payload, err := json.Marshal(snapshot)
	if err != nil {
		writeErrorCode(w, http.StatusInternalServerError, "computer_use_app_policy_failed", "computer-use app policy response failed")
		return
	}
	writeComputerUseWireJSON(w, http.StatusOK, payload)
}
