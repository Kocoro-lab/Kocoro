package tools

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const semanticScrollTransportGraceV1 = 150 * time.Millisecond

type SemanticScrollRequestV1 struct {
	SchemaVersion       int    `json:"schema_version"`
	PID                 int    `json:"pid"`
	BundleID            string `json:"bundle_id"`
	WindowID            uint32 `json:"window_id"`
	Ref                 string `json:"ref"`
	Path                string `json:"path"`
	ExpectedRole        string `json:"expected_role"`
	ExpectedFingerprint string `json:"expected_fingerprint"`
	Axis                string `json:"axis"`
	Direction           string `json:"direction"`
	Steps               int    `json:"steps"`
	FallbackPolicy      string `json:"fallback_policy"`
	InterferencePolicy  string `json:"interference_policy"`
	CommitDeadlineAt    string `json:"commit_deadline_at"`
}

type SemanticScrollRPCRequestV1 struct {
	ID     int64                   `json:"id"`
	Method string                  `json:"method"`
	Params SemanticScrollRequestV1 `json:"params"`
}

type SemanticScrollResultV1 struct {
	SchemaVersion  int      `json:"schema_version"`
	Status         string   `json:"status"`
	CommitState    string   `json:"commit_state"`
	Phase          string   `json:"phase"`
	FailureCode    *string  `json:"failure_code"`
	RetrySafe      bool     `json:"retry_safe"`
	Postcondition  *string  `json:"postcondition"`
	InitialValue   *float64 `json:"initial_value"`
	FinalValue     *float64 `json:"final_value"`
	StepsCompleted int      `json:"steps_completed"`
	ExpectedSteps  int      `json:"expected_steps"`
}

var semanticScrollRequestWireShapeV1 = coordinateObjectWireShape(false, map[string]coordinateWireShape{
	"id": coordinateScalarWireShape(false), "method": coordinateScalarWireShape(false),
	"params": coordinateObjectWireShape(false, map[string]coordinateWireShape{
		"schema_version": coordinateScalarWireShape(false), "pid": coordinateScalarWireShape(false),
		"bundle_id": coordinateScalarWireShape(false), "window_id": coordinateScalarWireShape(false),
		"ref": coordinateScalarWireShape(false), "path": coordinateScalarWireShape(false),
		"expected_role":        coordinateScalarWireShape(false),
		"expected_fingerprint": coordinateScalarWireShape(false),
		"axis":                 coordinateScalarWireShape(false), "direction": coordinateScalarWireShape(false),
		"steps": coordinateScalarWireShape(false), "fallback_policy": coordinateScalarWireShape(false),
		"interference_policy": coordinateScalarWireShape(false),
		"commit_deadline_at":  coordinateScalarWireShape(false),
	}),
})

var semanticScrollResultWireShapeV1 = coordinateObjectWireShape(false, map[string]coordinateWireShape{
	"schema_version": coordinateScalarWireShape(false), "status": coordinateScalarWireShape(false),
	"commit_state": coordinateScalarWireShape(false), "phase": coordinateScalarWireShape(false),
	"failure_code": coordinateScalarWireShape(true), "retry_safe": coordinateScalarWireShape(false),
	"postcondition": coordinateScalarWireShape(true), "initial_value": coordinateScalarWireShape(true),
	"final_value": coordinateScalarWireShape(true), "steps_completed": coordinateScalarWireShape(false),
	"expected_steps": coordinateScalarWireShape(false),
})

func (request SemanticScrollRequestV1) Validate() error {
	if request.SchemaVersion != 1 || request.PID <= 0 || request.WindowID == 0 {
		return fmt.Errorf("semantic_scroll_v1 authority is required")
	}
	for name, value := range map[string]string{
		"bundle_id": request.BundleID, "ref": request.Ref, "path": request.Path,
		"expected_role": request.ExpectedRole, "expected_fingerprint": request.ExpectedFingerprint,
	} {
		if value == "" || value != strings.TrimSpace(value) {
			return fmt.Errorf("semantic_scroll_v1 %s is invalid", name)
		}
	}
	if !validComputerUseRef(request.Ref) ||
		(request.Path != "window[0]" && !strings.HasPrefix(request.Path, "window[0]/")) ||
		(request.Axis != "vertical" && request.Axis != "horizontal") ||
		(request.Direction != "increment" && request.Direction != "decrement") ||
		request.Steps < 1 || request.Steps > 10 ||
		request.FallbackPolicy != "report_unsupported" ||
		request.InterferencePolicy != "global_physical" &&
			request.InterferencePolicy != "target_foreground" {
		return fmt.Errorf("semantic_scroll_v1 target or step authority is invalid")
	}
	if _, err := time.Parse(time.RFC3339Nano, request.CommitDeadlineAt); err != nil {
		return fmt.Errorf("semantic_scroll_v1 commit_deadline_at must be RFC3339: %w", err)
	}
	return nil
}

func DecodeSemanticScrollRPCRequestV1(payload []byte) (SemanticScrollRPCRequestV1, error) {
	if err := validateCoordinateWireShape(
		"semantic_scroll_v1 request", payload, semanticScrollRequestWireShapeV1); err != nil {
		return SemanticScrollRPCRequestV1{}, err
	}
	var envelope SemanticScrollRPCRequestV1
	if err := decodeStrictCoordinateJSON(payload, &envelope); err != nil {
		return envelope, fmt.Errorf("decode semantic_scroll_v1 request: %w", err)
	}
	if envelope.ID <= 0 || envelope.Method != "semantic_scroll_v1" {
		return envelope, fmt.Errorf("invalid semantic_scroll_v1 RPC envelope")
	}
	if err := envelope.Params.Validate(); err != nil {
		return envelope, err
	}
	return envelope, nil
}

func EncodeSemanticScrollRPCRequestV1(envelope SemanticScrollRPCRequestV1) ([]byte, error) {
	if envelope.ID <= 0 || envelope.Method != "semantic_scroll_v1" {
		return nil, fmt.Errorf("invalid semantic_scroll_v1 RPC envelope")
	}
	if err := envelope.Params.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(envelope)
}

func (result SemanticScrollResultV1) ValidateTaggedUnion() error {
	if result.SchemaVersion != 1 || result.RetrySafe || result.ExpectedSteps < 1 ||
		result.ExpectedSteps > 10 || result.StepsCompleted < 0 ||
		result.StepsCompleted > result.ExpectedSteps {
		return fmt.Errorf("semantic_scroll_v1 result schema/steps/retry policy is invalid")
	}
	if result.InitialValue != nil && (math.IsNaN(*result.InitialValue) || math.IsInf(*result.InitialValue, 0)) ||
		result.FinalValue != nil && (math.IsNaN(*result.FinalValue) || math.IsInf(*result.FinalValue, 0)) {
		return fmt.Errorf("semantic_scroll_v1 result values must be finite")
	}
	nilPayload := result.Postcondition == nil
	switch result.Status {
	case "verified":
		if result.CommitState != "committed" || result.Phase != "post_verification" ||
			result.FailureCode != nil || result.Postcondition == nil ||
			*result.Postcondition != "scroll_value_changed_in_direction" ||
			result.InitialValue == nil || result.FinalValue == nil ||
			result.StepsCompleted != result.ExpectedSteps {
			return fmt.Errorf("invalid verified semantic_scroll_v1 result")
		}
	case "completed_unverified":
		if result.CommitState == "unknown" {
			if result.Phase != "action" || result.FailureCode == nil ||
				*result.FailureCode != "ax_scroll_commit_unknown" || !nilPayload {
				return fmt.Errorf("invalid commit-unknown semantic_scroll_v1 result")
			}
			return nil
		}
		allowed := map[string]bool{
			"scroll_value_unchanged": true, "scroll_value_wrong_direction": true,
			"scroll_value_not_observed": true, "scroll_boundary_reached": true,
			"scroll_target_changed": true, "ax_scroll_failed": true,
			"interference_detection_unavailable":  true,
			"ax_messaging_timeout_restore_failed": true,
		}
		if result.CommitState != "committed" || result.Phase != "post_verification" ||
			result.FailureCode == nil || !allowed[*result.FailureCode] || !nilPayload {
			return fmt.Errorf("invalid completed_unverified semantic_scroll_v1 result")
		}
	case "user_interference":
		if (result.CommitState != "not_committed" && result.CommitState != "committed" &&
			result.CommitState != "unknown") || result.Phase != "user_interference" ||
			result.FailureCode == nil ||
			(*result.FailureCode != "physical_input_interference" &&
				*result.FailureCode != "target_foreground_interference") ||
			!nilPayload {
			return fmt.Errorf("invalid user_interference semantic_scroll_v1 result")
		}
		if result.CommitState == "not_committed" && result.StepsCompleted != 0 {
			return fmt.Errorf("not_committed user_interference cannot report completed steps")
		}
	case "cancelled":
		if (result.CommitState != "not_committed" && result.CommitState != "committed" &&
			result.CommitState != "unknown") || result.Phase != "cancelled" ||
			result.FailureCode == nil || *result.FailureCode != "controller_cancelled" || !nilPayload {
			return fmt.Errorf("invalid cancelled semantic_scroll_v1 result")
		}
		if result.CommitState == "not_committed" && result.StepsCompleted != 0 {
			return fmt.Errorf("not_committed cancellation cannot report completed steps")
		}
	case "fallback_required":
		if result.CommitState != "not_committed" || result.Phase != "preflight" ||
			result.FailureCode == nil || *result.FailureCode != "ax_scroll_metric_unsupported" ||
			result.StepsCompleted != 0 || !nilPayload {
			return fmt.Errorf("invalid fallback_required semantic_scroll_v1 result")
		}
	case "failed":
		preflight := map[string]bool{
			"invalid_request": true, "request_expired": true, "process_not_live": true,
			"process_identity_mismatch": true, "window_not_found": true, "window_ambiguous": true,
			"path_not_found": true, "role_mismatch": true, "fingerprint_mismatch": true,
			"fingerprint_not_found": true, "fingerprint_ambiguous": true,
			"sensitive_target": true, "enabled_unknown": true, "target_disabled": true,
			"scroll_boundary": true, "interference_detection_unavailable": true,
			"ax_messaging_timeout_unavailable": true,
		}
		exactPhase := result.FailureCode != nil &&
			((preflight[*result.FailureCode] && result.Phase == "preflight") ||
				(*result.FailureCode == "ax_scroll_failed" && result.Phase == "action"))
		if result.CommitState != "not_committed" || !exactPhase ||
			result.StepsCompleted != 0 || !nilPayload {
			return fmt.Errorf("invalid failed semantic_scroll_v1 result")
		}
	default:
		return fmt.Errorf("invalid semantic_scroll_v1 status %q", result.Status)
	}
	return nil
}

func DecodeSemanticScrollResultV1(payload []byte) (SemanticScrollResultV1, error) {
	if err := validateCoordinateWireShape(
		"semantic_scroll_v1 result", payload, semanticScrollResultWireShapeV1); err != nil {
		return SemanticScrollResultV1{}, err
	}
	var result SemanticScrollResultV1
	if err := decodeStrictCoordinateJSON(payload, &result); err != nil {
		return result, fmt.Errorf("decode semantic_scroll_v1 result: %w", err)
	}
	if err := result.ValidateTaggedUnion(); err != nil {
		return result, err
	}
	return result, nil
}

type SemanticScrollCommitUnknownErrorV1 struct{ cause error }

func (err *SemanticScrollCommitUnknownErrorV1) Error() string {
	return fmt.Sprintf("semantic_scroll_v1 commit unknown (not retry-safe): %v", err.cause)
}
func (err *SemanticScrollCommitUnknownErrorV1) Unwrap() error       { return err.cause }
func (err *SemanticScrollCommitUnknownErrorV1) RetrySafe() bool     { return false }
func (err *SemanticScrollCommitUnknownErrorV1) CommitUnknown() bool { return true }

func newSemanticScrollCommitUnknownV1(cause error) error {
	if cause == nil {
		cause = fmt.Errorf("missing valid helper result")
	}
	return &SemanticScrollCommitUnknownErrorV1{cause: cause}
}

func semanticScrollCancellationAuthorityV1(requestID int64, request SemanticScrollRequestV1) string {
	fields := []string{
		"semantic-scroll-v1", fmt.Sprintf("%d", requestID), fmt.Sprintf("%d", request.PID),
		request.BundleID, fmt.Sprintf("%d", request.WindowID), request.Ref, request.Path,
		request.ExpectedRole, request.ExpectedFingerprint, request.Axis, request.Direction,
		fmt.Sprintf("%d", request.Steps), request.CommitDeadlineAt,
	}
	var authority strings.Builder
	for _, field := range fields {
		fmt.Fprintf(&authority, "%d:%s", len([]byte(field)), field)
	}
	return authority.String()
}

func semanticScrollCancellationMarkerPathV1(
	requestID int64, request SemanticScrollRequestV1,
) string {
	digest := sha256.Sum256([]byte(semanticScrollCancellationAuthorityV1(requestID, request)))
	return filepath.Join("/tmp", fmt.Sprintf("kocoro-ax-scroll-cancel-v1-%x", digest[:]))
}

func writeSemanticScrollCancellationMarkerV1(path string) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	return file.Close()
}

func (client *AXClient) SemanticScrollV1(
	ctx context.Context, request SemanticScrollRequestV1,
) (SemanticScrollResultV1, error) {
	if runtime.GOOS != "darwin" {
		return SemanticScrollResultV1{}, fmt.Errorf("ax_server is macOS-only")
	}
	return client.semanticScrollV1(ctx, request)
}

func (client *AXClient) semanticScrollV1(
	ctx context.Context, request SemanticScrollRequestV1,
) (SemanticScrollResultV1, error) {
	if err := ctx.Err(); err != nil {
		return SemanticScrollResultV1{}, err
	}
	if err := request.Validate(); err != nil {
		return SemanticScrollResultV1{}, err
	}
	if err := client.Ensure(ctx); err != nil {
		return SemanticScrollResultV1{}, err
	}
	id := client.nextID.Add(1)
	cancellationMarker := semanticScrollCancellationMarkerPathV1(id, request)
	// The monotonically increasing ID is process-local. Remove only the exact
	// authority-derived stale marker before exposing this request to Swift.
	_ = os.Remove(cancellationMarker)
	preserveCancellationFence := false
	defer func() {
		if !preserveCancellationFence {
			_ = os.Remove(cancellationMarker)
		}
	}()
	payload, err := EncodeSemanticScrollRPCRequestV1(SemanticScrollRPCRequestV1{
		ID: id, Method: "semantic_scroll_v1", Params: request,
	})
	if err != nil {
		return SemanticScrollResultV1{}, err
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
		return SemanticScrollResultV1{}, err
	}
	written, writeErr := client.writer.Write(payload)
	if writeErr == nil && written < len(payload) {
		writeErr = io.ErrShortWrite
	}
	client.writeMu.Unlock()
	if writeErr != nil {
		removePending()
		return SemanticScrollResultV1{}, newSemanticScrollCommitUnknownV1(
			fmt.Errorf("ax_server semantic_scroll_v1 write: %w", writeErr))
	}
	commitDeadline, _ := time.Parse(time.RFC3339Nano, request.CommitDeadlineAt)
	now := time.Now()
	hardDeadline := commitDeadline.Add(semanticScrollTransportGraceV1)
	if minimum := now.Add(semanticScrollTransportGraceV1); hardDeadline.Before(minimum) {
		hardDeadline = minimum
	}
	if maximum := now.Add(3*time.Second + semanticScrollTransportGraceV1); hardDeadline.After(maximum) {
		hardDeadline = maximum
	}
	timer := time.NewTimer(time.Until(hardDeadline))
	defer timer.Stop()
	ctxDone := ctx.Done()
	var cancellationSignalError error
	for {
		select {
		case response := <-responses:
			removePending()
			if response.Error != nil {
				return SemanticScrollResultV1{}, newSemanticScrollCommitUnknownV1(fmt.Errorf(
					"ax_server RPC error %d: %s", response.Error.Code, response.Error.Message))
			}
			result, decodeErr := DecodeSemanticScrollResultV1(response.Result)
			if decodeErr != nil || result.ExpectedSteps != request.Steps {
				if decodeErr == nil {
					decodeErr = fmt.Errorf("semantic_scroll_v1 expected_steps mismatch")
				}
				return SemanticScrollResultV1{}, newSemanticScrollCommitUnknownV1(decodeErr)
			}
			return result, nil
		case <-ctxDone:
			// Preserve coordinator quiescence after the write boundary. Signal the
			// synchronous helper out-of-band, then wait for its typed stopped ack.
			ctxDone = nil
			if err := writeSemanticScrollCancellationMarkerV1(cancellationMarker); err != nil &&
				!errors.Is(err, os.ErrExist) {
				cancellationSignalError = err
			}
		case <-timer.C:
			removePending()
			// The helper may still be blocked in a read-only AX call. Preserve an
			// authority-scoped cancellation fence so that, if it later resumes, the
			// next pre-commit checkpoint prevents any further AX action. A leaked
			// fence is safer than releasing the global GUI lease while a timed-out
			// helper can still mutate the Mac. Normal typed acknowledgements remove
			// their marker; a hard-timeout fence intentionally survives rather than
			// making an unsafe liveness claim about the helper.
			if _, statErr := os.Stat(cancellationMarker); errors.Is(statErr, os.ErrNotExist) {
				if markerErr := writeSemanticScrollCancellationMarkerV1(cancellationMarker); markerErr != nil &&
					!errors.Is(markerErr, os.ErrExist) && cancellationSignalError == nil {
					cancellationSignalError = markerErr
				}
			}
			preserveCancellationFence = true
			if cancellationSignalError != nil {
				return SemanticScrollResultV1{}, newSemanticScrollCommitUnknownV1(fmt.Errorf(
					"helper cancellation acknowledgement timed out; cancellation signal: %w",
					cancellationSignalError))
			}
			return SemanticScrollResultV1{}, newSemanticScrollCommitUnknownV1(
				fmt.Errorf("helper acknowledgement timed out after commit deadline"))
		}
	}
}
