package daemon

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
)

// A risk decision contains three short scalar fields. Bodies above 4 KiB are
// invalid rather than a power-user workload, so there is intentionally no
// override; binding means a malformed/hostile request receives HTTP 400.
const maxConsequentialRiskDecisionBodyBytes = 4 << 10

func (s *Server) requireConsequentialRiskLocalDesktop(w http.ResponseWriter, r *http.Request) bool {
	w.Header().Set("Cache-Control", "no-store")
	if !consequentialRiskRequestIsLoopback(r) || s == nil ||
		s.consequentialRiskHTTPAuthorizer == nil || !s.consequentialRiskHTTPAuthorizer(r) {
		writeError(w, http.StatusForbidden, "local presence confirmation required")
		return false
	}
	return true
}

func consequentialRiskRequestIsLoopback(r *http.Request) bool {
	if r == nil {
		return false
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (s *Server) consequentialRiskBrokerOrError(w http.ResponseWriter) *ConsequentialRiskBroker {
	if s != nil && s.consequentialRiskBroker != nil {
		return s.consequentialRiskBroker
	}
	writeErrorCode(w, http.StatusServiceUnavailable,
		"computer_use_risk_confirmation_unavailable",
		"computer-use risk confirmation is unavailable")
	return nil
}

func (s *Server) handleConsequentialRiskMethodNotAllowed(allow string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.requireConsequentialRiskLocalDesktop(w, r) {
			return
		}
		w.Header().Set("Allow", allow)
		writeErrorCode(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
	}
}

func (s *Server) handleConsequentialRiskDetail(w http.ResponseWriter, r *http.Request) {
	if !s.requireConsequentialRiskLocalDesktop(w, r) {
		return
	}
	broker := s.consequentialRiskBrokerOrError(w)
	if broker == nil {
		return
	}
	intent, err := broker.Detail(r.PathValue("intent_id"))
	if err != nil {
		writeConsequentialRiskBrokerError(w, err)
		return
	}
	payload, err := json.Marshal(intent)
	if err != nil {
		writeErrorCode(w, http.StatusInternalServerError,
			"computer_use_risk_confirmation_failed",
			"computer-use risk confirmation failed")
		return
	}
	writeComputerUseWireJSON(w, http.StatusOK, payload)
}

func (s *Server) handleConsequentialRiskDecision(w http.ResponseWriter, r *http.Request) {
	if !s.requireConsequentialRiskLocalDesktop(w, r) {
		return
	}
	broker := s.consequentialRiskBrokerOrError(w)
	if broker == nil {
		return
	}
	body := http.MaxBytesReader(w, r.Body, maxConsequentialRiskDecisionBodyBytes)
	defer body.Close()
	payload, err := io.ReadAll(body)
	if err != nil {
		writeInvalidConsequentialRiskDecision(w)
		return
	}
	var request ConsequentialRiskDecisionRequestV1
	// This daemon-local strict codec rejects duplicate keys recursively,
	// unknown members, and trailing JSON values.
	if err := decodeStrictComputerUseAppPolicyJSON(payload, &request); err != nil {
		writeInvalidConsequentialRiskDecision(w)
		return
	}
	if request.IntentID != r.PathValue("intent_id") {
		writeErrorCode(w, http.StatusConflict,
			"computer_use_risk_intent_mismatch",
			"computer-use risk intent does not match the request path")
		return
	}
	response, err := broker.Decide(request)
	if err != nil {
		if errors.Is(err, ErrConsequentialRiskDecisionInvalid) {
			writeInvalidConsequentialRiskDecision(w)
			return
		}
		writeConsequentialRiskBrokerError(w, err)
		return
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		writeErrorCode(w, http.StatusInternalServerError,
			"computer_use_risk_confirmation_failed",
			"computer-use risk confirmation failed")
		return
	}
	writeComputerUseWireJSON(w, http.StatusOK, encoded)
}

func writeInvalidConsequentialRiskDecision(w http.ResponseWriter) {
	writeErrorCode(w, http.StatusBadRequest,
		"invalid_computer_use_risk_decision",
		"invalid computer-use risk decision")
}

func writeConsequentialRiskBrokerError(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrConsequentialRiskIntentUnavailable) {
		writeErrorCode(w, http.StatusGone,
			"computer_use_risk_intent_unavailable",
			"computer-use risk intent is unavailable")
		return
	}
	writeErrorCode(w, http.StatusInternalServerError,
		"computer_use_risk_confirmation_failed",
		"computer-use risk confirmation failed")
}
