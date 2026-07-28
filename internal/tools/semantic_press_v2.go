package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"runtime"
	"strings"
	"time"
)

const semanticPressTransportGraceV2 = 150 * time.Millisecond

// SemanticPressRequestV2 is the complete, immutable authority for one AXPress
// attempt. It deliberately has no visual or synthetic-input fallback.
type SemanticPressRequestV2 struct {
	SchemaVersion            int                                      `json:"schema_version"`
	PID                      int                                      `json:"pid"`
	BundleID                 string                                   `json:"bundle_id"`
	WindowID                 uint32                                   `json:"window_id"`
	Ref                      string                                   `json:"ref"`
	Path                     string                                   `json:"path"`
	ExpectedRole             string                                   `json:"expected_role"`
	ExpectedFingerprint      string                                   `json:"expected_fingerprint"`
	FallbackPolicy           string                                   `json:"fallback_policy"`
	InterferencePolicy       string                                   `json:"interference_policy"`
	CommitDeadlineAt         string                                   `json:"commit_deadline_at"`
	RiskDestinationAssertion *SemanticPressRiskDestinationAssertionV2 `json:"risk_destination_assertion"`
}

type SemanticPressRiskDestinationAssertionV2 struct {
	Kind                string `json:"kind"`
	ExpectedWindowTitle string `json:"expected_window_title"`
}

type SemanticPressRPCRequestV2 struct {
	ID     int64                  `json:"id"`
	Method string                 `json:"method"`
	Params SemanticPressRequestV2 `json:"params"`
}

type SemanticPressResultV2 struct {
	SchemaVersion int     `json:"schema_version"`
	Status        string  `json:"status"`
	CommitState   string  `json:"commit_state"`
	Phase         string  `json:"phase"`
	FailureCode   *string `json:"failure_code"`
	Postcondition *string `json:"postcondition"`
	RetrySafe     bool    `json:"retry_safe"`
}

var semanticPressRequestWireShapeV2 = coordinateObjectWireShape(false, map[string]coordinateWireShape{
	"id":     coordinateScalarWireShape(false),
	"method": coordinateScalarWireShape(false),
	"params": coordinateObjectWireShape(false, map[string]coordinateWireShape{
		"schema_version":       coordinateScalarWireShape(false),
		"pid":                  coordinateScalarWireShape(false),
		"bundle_id":            coordinateScalarWireShape(false),
		"window_id":            coordinateScalarWireShape(false),
		"ref":                  coordinateScalarWireShape(false),
		"path":                 coordinateScalarWireShape(false),
		"expected_role":        coordinateScalarWireShape(false),
		"expected_fingerprint": coordinateScalarWireShape(false),
		"fallback_policy":      coordinateScalarWireShape(false),
		"interference_policy":  coordinateScalarWireShape(false),
		"commit_deadline_at":   coordinateScalarWireShape(false),
		"risk_destination_assertion": coordinateNullableWireShape(coordinateObjectWireShape(false, map[string]coordinateWireShape{
			"kind":                  coordinateScalarWireShape(false),
			"expected_window_title": coordinateScalarWireShape(false),
		})),
	}),
})

var semanticPressResultWireShapeV2 = coordinateObjectWireShape(false, map[string]coordinateWireShape{
	"schema_version": coordinateScalarWireShape(false),
	"status":         coordinateScalarWireShape(false),
	"commit_state":   coordinateScalarWireShape(false),
	"phase":          coordinateScalarWireShape(false),
	"failure_code":   coordinateScalarWireShape(true),
	"postcondition":  coordinateScalarWireShape(true),
	"retry_safe":     coordinateScalarWireShape(false),
})

func (request SemanticPressRequestV2) Validate() error {
	if request.SchemaVersion != 2 || request.PID <= 0 || request.WindowID == 0 {
		return fmt.Errorf("semantic_press_v2 authority is required")
	}
	for name, value := range map[string]string{
		"bundle_id":            request.BundleID,
		"ref":                  request.Ref,
		"path":                 request.Path,
		"expected_role":        request.ExpectedRole,
		"expected_fingerprint": request.ExpectedFingerprint,
	} {
		if value == "" || value != strings.TrimSpace(value) {
			return fmt.Errorf("semantic_press_v2 %s is invalid", name)
		}
	}
	if !validComputerUseRef(request.Ref) ||
		(request.Path != "window[0]" && !strings.HasPrefix(request.Path, "window[0]/")) ||
		request.FallbackPolicy != "none" ||
		request.InterferencePolicy != "global_physical" &&
			request.InterferencePolicy != "target_foreground" {
		return fmt.Errorf("semantic_press_v2 ref/path/fallback policy is invalid")
	}
	if _, err := time.Parse(time.RFC3339Nano, request.CommitDeadlineAt); err != nil {
		return fmt.Errorf("semantic_press_v2 commit_deadline_at must be RFC3339: %w", err)
	}
	if request.RiskDestinationAssertion != nil {
		if request.RiskDestinationAssertion.Kind != "exact_window_title" ||
			validateConsequentialRiskLabelV1("risk_expected_window_title", request.RiskDestinationAssertion.ExpectedWindowTitle) != nil {
			return fmt.Errorf("semantic_press_v2 risk authority is invalid")
		}
	}
	return nil
}

func DecodeSemanticPressRPCRequestV2(payload []byte) (SemanticPressRPCRequestV2, error) {
	if err := validateCoordinateWireShape(
		"semantic_press_v2 request", payload, semanticPressRequestWireShapeV2); err != nil {
		return SemanticPressRPCRequestV2{}, err
	}
	var envelope SemanticPressRPCRequestV2
	if err := decodeStrictCoordinateJSON(payload, &envelope); err != nil {
		return SemanticPressRPCRequestV2{}, fmt.Errorf("decode semantic_press_v2 request: %w", err)
	}
	if envelope.ID <= 0 || envelope.Method != "semantic_press_v2" {
		return SemanticPressRPCRequestV2{}, fmt.Errorf("invalid semantic_press_v2 RPC envelope")
	}
	if err := envelope.Params.Validate(); err != nil {
		return SemanticPressRPCRequestV2{}, err
	}
	return envelope, nil
}

func EncodeSemanticPressRPCRequestV2(envelope SemanticPressRPCRequestV2) ([]byte, error) {
	if envelope.ID <= 0 || envelope.Method != "semantic_press_v2" {
		return nil, fmt.Errorf("invalid semantic_press_v2 RPC envelope")
	}
	if err := envelope.Params.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(envelope)
}

func (result SemanticPressResultV2) ValidateTaggedUnion() error {
	if result.SchemaVersion != 2 || result.RetrySafe || result.Postcondition != nil {
		return fmt.Errorf("semantic_press_v2 result schema/retry/postcondition is invalid")
	}
	switch result.Status {
	case "completed_unverified":
		if result.CommitState == "unknown" {
			if result.Phase != "action" || result.FailureCode == nil ||
				*result.FailureCode != "ax_press_commit_unknown" {
				return fmt.Errorf("invalid commit-unknown semantic_press_v2 result")
			}
			return nil
		}
		allowed := map[string]bool{
			"postcondition_not_declared":          true,
			"interference_detection_unavailable":  true,
			"ax_messaging_timeout_restore_failed": true,
		}
		if result.CommitState != "committed" || result.Phase != "post_verification" ||
			result.FailureCode == nil || !allowed[*result.FailureCode] {
			return fmt.Errorf("invalid completed_unverified semantic_press_v2 result")
		}
	case "user_interference":
		if (result.CommitState != "not_committed" && result.CommitState != "committed" &&
			result.CommitState != "unknown") || result.Phase != "user_interference" || result.FailureCode == nil ||
			*result.FailureCode != "physical_input_interference" {
			return fmt.Errorf("invalid user_interference semantic_press_v2 result")
		}
	case "failed":
		preflight := map[string]bool{
			"invalid_request": true, "request_expired": true,
			"process_not_live": true, "process_identity_mismatch": true,
			"window_not_found": true, "window_ambiguous": true,
			"path_not_found": true, "role_mismatch": true,
			"fingerprint_mismatch": true, "fingerprint_not_found": true,
			"fingerprint_ambiguous": true, "sensitive_target": true,
			"enabled_unknown": true, "target_disabled": true,
			"ax_press_unavailable":               true,
			"interference_detection_unavailable": true,
			"ax_messaging_timeout_unavailable":   true,
			"risk_destination_drift":             true,
			"risk_destination_unavailable":       true,
		}
		exactPhase := result.FailureCode != nil &&
			((preflight[*result.FailureCode] && result.Phase == "preflight") ||
				(*result.FailureCode == "ax_press_failed" && result.Phase == "action"))
		if result.CommitState != "not_committed" || !exactPhase {
			return fmt.Errorf("invalid failed semantic_press_v2 result")
		}
	default:
		// Schema v2 has no declared causal predicate, therefore "verified" is
		// intentionally not a legal acknowledgement.
		return fmt.Errorf("invalid semantic_press_v2 status %q", result.Status)
	}
	return nil
}

func DecodeSemanticPressResultV2(payload []byte) (SemanticPressResultV2, error) {
	if err := validateCoordinateWireShape(
		"semantic_press_v2 result", payload, semanticPressResultWireShapeV2); err != nil {
		return SemanticPressResultV2{}, err
	}
	var result SemanticPressResultV2
	if err := decodeStrictCoordinateJSON(payload, &result); err != nil {
		return SemanticPressResultV2{}, fmt.Errorf("decode semantic_press_v2 result: %w", err)
	}
	if err := result.ValidateTaggedUnion(); err != nil {
		return SemanticPressResultV2{}, err
	}
	return result, nil
}

func EncodeSemanticPressResultV2(result SemanticPressResultV2) ([]byte, error) {
	if err := result.ValidateTaggedUnion(); err != nil {
		return nil, err
	}
	return json.Marshal(result)
}

type SemanticPressCommitUnknownErrorV2 struct{ cause error }

func (err *SemanticPressCommitUnknownErrorV2) Error() string {
	return fmt.Sprintf("semantic_press_v2 commit unknown (not retry-safe): %v", err.cause)
}
func (err *SemanticPressCommitUnknownErrorV2) Unwrap() error       { return err.cause }
func (err *SemanticPressCommitUnknownErrorV2) RetrySafe() bool     { return false }
func (err *SemanticPressCommitUnknownErrorV2) CommitUnknown() bool { return true }

func newSemanticPressCommitUnknownV2(cause error) error {
	if cause == nil {
		cause = fmt.Errorf("missing valid helper result")
	}
	return &SemanticPressCommitUnknownErrorV2{cause: cause}
}

func (client *AXClient) SemanticPressV2(
	ctx context.Context, request SemanticPressRequestV2,
) (SemanticPressResultV2, error) {
	if runtime.GOOS != "darwin" {
		return SemanticPressResultV2{}, fmt.Errorf("ax_server is macOS-only")
	}
	return client.semanticPressV2(ctx, request)
}

func (client *AXClient) semanticPressV2(
	ctx context.Context, request SemanticPressRequestV2,
) (SemanticPressResultV2, error) {
	if err := ctx.Err(); err != nil {
		return SemanticPressResultV2{}, err
	}
	if err := request.Validate(); err != nil {
		return SemanticPressResultV2{}, err
	}
	if err := client.Ensure(ctx); err != nil {
		return SemanticPressResultV2{}, err
	}
	id := client.nextID.Add(1)
	payload, err := EncodeSemanticPressRPCRequestV2(SemanticPressRPCRequestV2{
		ID: id, Method: "semantic_press_v2", Params: request,
	})
	if err != nil {
		return SemanticPressResultV2{}, err
	}
	payload = append(payload, '\n')
	responses := make(chan AXResponse, 1)
	client.pendingMu.Lock()
	client.pending[id] = responses
	client.pendingMu.Unlock()
	removePending := func() {
		client.pendingMu.Lock()
		delete(client.pending, id)
		client.pendingMu.Unlock()
	}
	client.writeMu.Lock()
	if err := ctx.Err(); err != nil {
		client.writeMu.Unlock()
		removePending()
		return SemanticPressResultV2{}, err
	}
	written, writeErr := client.writer.Write(payload)
	if writeErr == nil && written < len(payload) {
		writeErr = io.ErrShortWrite
	}
	client.writeMu.Unlock()
	if writeErr != nil {
		removePending()
		return SemanticPressResultV2{}, newSemanticPressCommitUnknownV2(
			fmt.Errorf("ax_server semantic_press_v2 write: %w", writeErr))
	}
	commitDeadline, _ := time.Parse(time.RFC3339Nano, request.CommitDeadlineAt)
	now := time.Now()
	hardDeadline := commitDeadline.Add(semanticPressTransportGraceV2)
	if minimum := now.Add(semanticPressTransportGraceV2); hardDeadline.Before(minimum) {
		hardDeadline = minimum
	}
	if maximum := now.Add(2*time.Second + semanticPressTransportGraceV2); hardDeadline.After(maximum) {
		hardDeadline = maximum
	}
	timer := time.NewTimer(time.Until(hardDeadline))
	defer timer.Stop()
	select {
	case response := <-responses:
		removePending()
		if response.Error != nil {
			return SemanticPressResultV2{}, newSemanticPressCommitUnknownV2(fmt.Errorf(
				"ax_server RPC error %d: %s", response.Error.Code, response.Error.Message))
		}
		result, decodeErr := DecodeSemanticPressResultV2(response.Result)
		if decodeErr != nil {
			return SemanticPressResultV2{}, newSemanticPressCommitUnknownV2(decodeErr)
		}
		return result, nil
	case <-timer.C:
		removePending()
		return SemanticPressResultV2{}, newSemanticPressCommitUnknownV2(
			fmt.Errorf("helper acknowledgement timed out after commit deadline"))
	}
}
