package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"runtime"
	"strings"
	"time"
)

const (
	coordinateMouseEndpointToleranceV1 = 2.0
	coordinateMouseTransportGraceV1    = 150 * time.Millisecond
)

type CoordinateMouseEventPointV1 struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

type CoordinateMouseRiskAssertionV1 struct {
	Kind                 string                                  `json:"kind"`
	RiskKind             string                                  `json:"risk_kind"`
	ElementRef           string                                  `json:"element_ref"`
	ExpectedRole         string                                  `json:"expected_role"`
	ExpectedFingerprint  string                                  `json:"expected_fingerprint"`
	CoordinateAuthority  ConsequentialRiskCoordinateAuthorityV1  `json:"coordinate_authority"`
	DestinationAssertion SemanticPressRiskDestinationAssertionV2 `json:"destination_assertion"`
}

type CoordinateMouseEventRequestV1 struct {
	SchemaVersion              int                             `json:"schema_version"`
	TopologyRef                CoordinateTopologyRefV1         `json:"topology_ref"`
	HelperBootID               string                          `json:"helper_boot_id"`
	PID                        int                             `json:"pid"`
	BundleID                   string                          `json:"bundle_id"`
	WindowID                   uint32                          `json:"window_id"`
	ExpectedWindowQuartzBounds CoordinateQuartzRectV1          `json:"expected_window_quartz_bounds"`
	DisplayID                  uint32                          `json:"display_id"`
	QuartzPoint                CoordinateMouseEventPointV1     `json:"quartz_point"`
	Action                     string                          `json:"action"`
	Button                     *string                         `json:"button"`
	ClickCount                 *int                            `json:"click_count"`
	Modifiers                  []string                        `json:"modifiers"`
	CommitDeadlineAt           string                          `json:"commit_deadline_at"`
	RiskAssertion              *CoordinateMouseRiskAssertionV1 `json:"risk_assertion"`
}

type CoordinateMouseEventRPCRequestV1 struct {
	ID     int64                         `json:"id"`
	Method string                        `json:"method"`
	Params CoordinateMouseEventRequestV1 `json:"params"`
}

type CoordinateMouseEventPointerEndpointV1 struct {
	Requested CoordinateMouseEventPointV1  `json:"requested"`
	Observed  *CoordinateMouseEventPointV1 `json:"observed"`
	Tolerance float64                      `json:"tolerance"`
	Verified  bool                         `json:"verified"`
}

type CoordinateMouseEventResultV1 struct {
	SchemaVersion          int                                    `json:"schema_version"`
	Status                 string                                 `json:"status"`
	Action                 string                                 `json:"action"`
	PrimaryActionCommitted bool                                   `json:"primary_action_committed"`
	PointerMotionCommitted bool                                   `json:"pointer_motion_committed"`
	Phase                  string                                 `json:"phase"`
	FailureCode            *string                                `json:"failure_code"`
	RetrySafe              bool                                   `json:"retry_safe"`
	PointerEndpoint        *CoordinateMouseEventPointerEndpointV1 `json:"pointer_endpoint"`
}

var coordinateMousePointWireShapeV1 = coordinateObjectWireShape(false, map[string]coordinateWireShape{
	"x": coordinateScalarWireShape(false),
	"y": coordinateScalarWireShape(false),
})

var coordinateMouseRiskAssertionWireShapeV1 = coordinateObjectWireShape(false, map[string]coordinateWireShape{
	"kind":                 coordinateScalarWireShape(false),
	"risk_kind":            coordinateScalarWireShape(false),
	"element_ref":          coordinateScalarWireShape(false),
	"expected_role":        coordinateScalarWireShape(false),
	"expected_fingerprint": coordinateScalarWireShape(false),
	"coordinate_authority": consequentialRiskCoordinateAuthorityWireShapeV1,
	"destination_assertion": coordinateObjectWireShape(false, map[string]coordinateWireShape{
		"kind":                  coordinateScalarWireShape(false),
		"expected_window_title": coordinateScalarWireShape(false),
	}),
})

var coordinateMouseRequestWireShapeV1 = coordinateObjectWireShape(false, map[string]coordinateWireShape{
	"id":     coordinateScalarWireShape(false),
	"method": coordinateScalarWireShape(false),
	"params": coordinateObjectWireShape(false, map[string]coordinateWireShape{
		"schema_version":                coordinateScalarWireShape(false),
		"topology_ref":                  coordinateTopologyRefWireShapeV1,
		"helper_boot_id":                coordinateScalarWireShape(false),
		"pid":                           coordinateScalarWireShape(false),
		"bundle_id":                     coordinateScalarWireShape(false),
		"window_id":                     coordinateScalarWireShape(false),
		"expected_window_quartz_bounds": coordinateQuartzRectWireShapeV1,
		"display_id":                    coordinateScalarWireShape(false),
		"quartz_point":                  coordinateMousePointWireShapeV1,
		"action":                        coordinateScalarWireShape(false),
		"button":                        coordinateScalarWireShape(true),
		"click_count":                   coordinateScalarWireShape(true),
		"modifiers": coordinateArrayWireShape(
			coordinateScalarWireShape(false)),
		"commit_deadline_at": coordinateScalarWireShape(false),
		"risk_assertion":     coordinateNullableWireShape(coordinateMouseRiskAssertionWireShapeV1),
	}),
})

var coordinateMouseEndpointWireShapeV1 = coordinateObjectWireShape(false, map[string]coordinateWireShape{
	"requested": coordinateMousePointWireShapeV1,
	"observed":  coordinateNullableWireShape(coordinateMousePointWireShapeV1),
	"tolerance": coordinateScalarWireShape(false),
	"verified":  coordinateScalarWireShape(false),
})

var coordinateMouseResultWireShapeV1 = coordinateObjectWireShape(false, map[string]coordinateWireShape{
	"schema_version":           coordinateScalarWireShape(false),
	"status":                   coordinateScalarWireShape(false),
	"action":                   coordinateScalarWireShape(false),
	"primary_action_committed": coordinateScalarWireShape(false),
	"pointer_motion_committed": coordinateScalarWireShape(false),
	"phase":                    coordinateScalarWireShape(false),
	"failure_code":             coordinateScalarWireShape(true),
	"retry_safe":               coordinateScalarWireShape(false),
	"pointer_endpoint":         coordinateNullableWireShape(coordinateMouseEndpointWireShapeV1),
})

func DecodeCoordinateMouseEventRPCRequestV1(payload []byte) (CoordinateMouseEventRPCRequestV1, error) {
	if err := validateCoordinateWireShape(
		"coordinate_mouse_event request v1",
		payload,
		coordinateMouseRequestWireShapeV1,
	); err != nil {
		return CoordinateMouseEventRPCRequestV1{}, err
	}
	var envelope CoordinateMouseEventRPCRequestV1
	if err := decodeStrictCoordinateJSON(payload, &envelope); err != nil {
		return CoordinateMouseEventRPCRequestV1{}, fmt.Errorf("decode coordinate_mouse_event request v1: %w", err)
	}
	if envelope.ID <= 0 || envelope.Method != "coordinate_mouse_event" {
		return CoordinateMouseEventRPCRequestV1{}, fmt.Errorf("invalid coordinate_mouse_event RPC envelope")
	}
	if err := envelope.Params.Validate(); err != nil {
		return CoordinateMouseEventRPCRequestV1{}, err
	}
	return envelope, nil
}

func EncodeCoordinateMouseEventRPCRequestV1(envelope CoordinateMouseEventRPCRequestV1) ([]byte, error) {
	if envelope.ID <= 0 || envelope.Method != "coordinate_mouse_event" {
		return nil, fmt.Errorf("invalid coordinate_mouse_event RPC envelope")
	}
	if err := envelope.Params.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(envelope)
}

func (request CoordinateMouseEventRequestV1) Validate() error {
	if request.SchemaVersion != 1 || request.TopologyRef.Generation == 0 || request.PID <= 0 ||
		request.WindowID == 0 || request.DisplayID == 0 {
		return fmt.Errorf("coordinate_mouse_event request authority is required")
	}
	for name, value := range map[string]string{
		"topology_id":    request.TopologyRef.TopologyID,
		"helper_boot_id": request.HelperBootID,
		"bundle_id":      request.BundleID,
	} {
		if value == "" || value != strings.TrimSpace(value) {
			return fmt.Errorf("coordinate_mouse_event %s is invalid", name)
		}
	}
	if err := validateCoordinateQuartzRect(
		"expected_window_quartz_bounds",
		request.ExpectedWindowQuartzBounds,
	); err != nil {
		return err
	}
	if err := validateCoordinateMousePointV1("quartz_point", request.QuartzPoint); err != nil {
		return err
	}
	if _, err := time.Parse(time.RFC3339Nano, request.CommitDeadlineAt); err != nil {
		return fmt.Errorf("coordinate_mouse_event commit_deadline_at must be RFC3339: %w", err)
	}
	if request.Modifiers == nil {
		return fmt.Errorf("coordinate_mouse_event modifiers must be an explicit array")
	}
	if err := validateTargetBoundInputModifiersV1(request.Modifiers); err != nil {
		return fmt.Errorf("coordinate_mouse_event modifiers are invalid: %w", err)
	}
	switch request.Action {
	case "move":
		if request.Button != nil || request.ClickCount != nil || request.RiskAssertion != nil {
			return fmt.Errorf("coordinate_mouse_event move requires null button, click_count, and risk_assertion")
		}
	case "click":
		if request.Button == nil || !validOpenAIComputerClickButtonV1(*request.Button) ||
			request.ClickCount == nil || *request.ClickCount < 1 || *request.ClickCount > 3 {
			return fmt.Errorf("coordinate_mouse_event click requires an admitted button and click_count in [1,3]")
		}
	default:
		return fmt.Errorf("unsupported coordinate_mouse_event action %q", request.Action)
	}
	if request.RiskAssertion != nil {
		if err := request.RiskAssertion.ValidateFor(request); err != nil {
			return err
		}
	}
	return nil
}

func (assertion CoordinateMouseRiskAssertionV1) ValidateFor(request CoordinateMouseEventRequestV1) error {
	if assertion.Kind != "consequential_click_v1" || !validConsequentialRiskKindV1(assertion.RiskKind) ||
		request.Action != "click" || request.Button == nil || *request.Button != "left" ||
		request.ClickCount == nil || *request.ClickCount != 1 ||
		!validComputerUseRef(assertion.ElementRef) ||
		!consequentialRiskRolePatternV1.MatchString(assertion.ExpectedRole) ||
		!consequentialRiskFingerprintV1.MatchString(assertion.ExpectedFingerprint) {
		return fmt.Errorf("coordinate_mouse_event risk assertion identity is invalid")
	}
	if err := assertion.CoordinateAuthority.Validate(); err != nil {
		return err
	}
	coordinate := assertion.CoordinateAuthority
	if coordinate.TopologyRef != request.TopologyRef || coordinate.HelperBootID != request.HelperBootID ||
		coordinate.DisplayID != request.DisplayID ||
		coordinate.QuartzPoint.X != request.QuartzPoint.X ||
		coordinate.QuartzPoint.Y != request.QuartzPoint.Y {
		return fmt.Errorf("coordinate_mouse_event risk assertion does not bind the exact request point")
	}
	commitDeadline, _ := time.Parse(time.RFC3339Nano, request.CommitDeadlineAt)
	frameExpiry, _ := time.Parse(time.RFC3339Nano, coordinate.FrameExpiresAt)
	if commitDeadline.After(frameExpiry) {
		return fmt.Errorf("coordinate_mouse_event risk commit deadline exceeds coordinate frame authority")
	}
	if assertion.DestinationAssertion.Kind != "exact_window_title" ||
		validateConsequentialRiskLabelV1(
			"risk_expected_window_title", assertion.DestinationAssertion.ExpectedWindowTitle) != nil {
		return fmt.Errorf("coordinate_mouse_event risk destination assertion is invalid")
	}
	return nil
}

func DecodeCoordinateMouseEventResultV1(payload []byte) (CoordinateMouseEventResultV1, error) {
	if err := validateCoordinateWireShape(
		"coordinate_mouse_event result v1",
		payload,
		coordinateMouseResultWireShapeV1,
	); err != nil {
		return CoordinateMouseEventResultV1{}, err
	}
	var result CoordinateMouseEventResultV1
	if err := decodeStrictCoordinateJSON(payload, &result); err != nil {
		return CoordinateMouseEventResultV1{}, fmt.Errorf("decode coordinate_mouse_event result v1: %w", err)
	}
	if err := result.ValidateTaggedUnion(); err != nil {
		return CoordinateMouseEventResultV1{}, err
	}
	return result, nil
}

func EncodeCoordinateMouseEventResultV1(result CoordinateMouseEventResultV1) ([]byte, error) {
	if err := result.ValidateTaggedUnion(); err != nil {
		return nil, err
	}
	return json.Marshal(result)
}

func (result CoordinateMouseEventResultV1) ValidateTaggedUnion() error {
	if result.SchemaVersion != 1 ||
		(result.Action != "move" && result.Action != "click" && result.Action != "unknown") {
		return fmt.Errorf("invalid coordinate_mouse_event result schema or action")
	}
	if result.RetrySafe {
		return fmt.Errorf("coordinate_mouse_event mutation results are never retry-safe")
	}
	if result.Action == "unknown" &&
		(result.Status != "failed" || result.FailureCode == nil || *result.FailureCode != "invalid_request") {
		return fmt.Errorf("unknown coordinate mouse action is reserved for strict invalid_request failures")
	}
	if result.PointerEndpoint != nil {
		if err := result.PointerEndpoint.Validate(); err != nil {
			return err
		}
	}

	switch result.Status {
	case "completed":
		if result.Action != "move" || !result.PrimaryActionCommitted || !result.PointerMotionCommitted ||
			result.Phase != "post_verification" || result.FailureCode != nil ||
			result.PointerEndpoint == nil || !result.PointerEndpoint.Verified {
			return fmt.Errorf("invalid completed coordinate_mouse_event result")
		}
	case "completed_unverified":
		if !result.PrimaryActionCommitted || !result.PointerMotionCommitted ||
			result.Phase != "post_verification" || result.FailureCode == nil || result.PointerEndpoint == nil {
			return fmt.Errorf("invalid completed_unverified coordinate_mouse_event result")
		}
		switch result.Action {
		case "move":
			validEndpointMiss := *result.FailureCode == "pointer_endpoint_not_verified" &&
				!result.PointerEndpoint.Verified
			validMonitorLoss := *result.FailureCode == "interference_detection_unavailable"
			validModifierRelease := *result.FailureCode == "modifier_release_unconfirmed"
			if !validEndpointMiss && !validMonitorLoss && !validModifierRelease {
				return fmt.Errorf("invalid unverified move result")
			}
		case "click":
			validVerified := *result.FailureCode == "click_postcondition_not_declared" && result.PointerEndpoint.Verified
			validMiss := *result.FailureCode == "pointer_endpoint_not_verified_after_commit" && !result.PointerEndpoint.Verified
			validInterrupted := *result.FailureCode == "input_sequence_interrupted_after_commit"
			validMonitorLoss := *result.FailureCode == "interference_detection_unavailable"
			validModifierRelease := *result.FailureCode == "modifier_release_unconfirmed"
			if !validVerified && !validMiss && !validInterrupted &&
				!validMonitorLoss && !validModifierRelease {
				return fmt.Errorf("invalid unverified click result")
			}
		}
	case "user_interference":
		preCommit := !result.PrimaryActionCommitted && !result.PointerMotionCommitted &&
			result.PointerEndpoint == nil
		postMove := result.PointerMotionCommitted && result.PointerEndpoint != nil &&
			(result.Action == "click" || result.PrimaryActionCommitted)
		if result.FailureCode == nil || *result.FailureCode != "physical_input_interference" ||
			result.Phase != "user_interference" || (!preCommit && !postMove) {
			return fmt.Errorf("invalid user_interference coordinate_mouse_event result")
		}
	case "failed":
		if result.PrimaryActionCommitted || result.FailureCode == nil || *result.FailureCode == "" {
			return fmt.Errorf("invalid failed coordinate_mouse_event result")
		}
		if err := validateCoordinateMouseFailureV1(result); err != nil {
			return err
		}
	default:
		return fmt.Errorf("invalid coordinate_mouse_event status %q", result.Status)
	}
	return nil
}

func (endpoint CoordinateMouseEventPointerEndpointV1) Validate() error {
	if err := validateCoordinateMousePointV1("pointer_endpoint.requested", endpoint.Requested); err != nil {
		return err
	}
	if endpoint.Tolerance != coordinateMouseEndpointToleranceV1 {
		return fmt.Errorf("coordinate_mouse_event endpoint tolerance must be %.0f", coordinateMouseEndpointToleranceV1)
	}
	verified := false
	if endpoint.Observed != nil {
		if err := validateCoordinateMousePointV1("pointer_endpoint.observed", *endpoint.Observed); err != nil {
			return err
		}
		verified = math.Abs(endpoint.Requested.X-endpoint.Observed.X) <= endpoint.Tolerance &&
			math.Abs(endpoint.Requested.Y-endpoint.Observed.Y) <= endpoint.Tolerance
	}
	if endpoint.Verified != verified {
		return fmt.Errorf("coordinate_mouse_event endpoint verified flag contradicts geometry")
	}
	return nil
}

func validateCoordinateMouseFailureV1(result CoordinateMouseEventResultV1) error {
	code := *result.FailureCode
	if result.Action == "unknown" && code != "invalid_request" {
		return fmt.Errorf("unknown coordinate mouse action requires invalid_request failure")
	}
	if code == "interference_detection_unavailable" {
		validPreflight := result.Phase == "preflight" &&
			!result.PointerMotionCommitted && result.PointerEndpoint == nil
		validPostMoveClick := result.Action == "click" && result.Phase == "action" &&
			result.PointerMotionCommitted && result.PointerEndpoint != nil &&
			result.PointerEndpoint.Verified
		if !validPreflight && !validPostMoveClick {
			return fmt.Errorf("invalid interference-monitor failure")
		}
		return nil
	}
	preflightNoSideEffect := map[string]bool{
		"invalid_request": true, "request_expired": true, "topology_unavailable": true,
		"stale_topology": true, "helper_boot_mismatch": true, "process_not_live": true,
		"process_identity_mismatch": true, "window_not_found": true,
		"window_identity_mismatch": true, "window_not_actionable": true,
		"window_bounds_mismatch": true, "display_not_found": true,
		"display_not_actionable": true, "point_outside_window": true,
		"point_outside_display": true, "point_occluded": true,
		"modifier_press_failed": true, "cancelled_before_input": true,
		"modifier_release_unconfirmed": true,
	}
	if preflightNoSideEffect[code] {
		if result.Phase != "preflight" || result.PointerMotionCommitted || result.PointerEndpoint != nil {
			return fmt.Errorf("invalid preflight coordinate_mouse_event failure")
		}
		return nil
	}
	switch code {
	case "event_preparation_failed":
		if result.Action != "click" || result.Phase != "preparation" ||
			result.PointerMotionCommitted || result.PointerEndpoint != nil {
			return fmt.Errorf("invalid click preparation failure")
		}
	case "pointer_move_failed":
		if result.Phase != "pointer_move" || result.PointerMotionCommitted || result.PointerEndpoint != nil {
			return fmt.Errorf("invalid pointer move failure")
		}
	case "pointer_endpoint_not_verified":
		if result.Action != "click" || result.Phase != "pointer_move" ||
			!result.PointerMotionCommitted || result.PointerEndpoint == nil || result.PointerEndpoint.Verified {
			return fmt.Errorf("invalid pointer endpoint failure")
		}
	case "target_changed_before_click", "request_expired_before_click", "input_commit_blocked",
		"risk_destination_drift", "risk_hit_target_drift", "risk_hit_target_unavailable":
		if result.Action != "click" || result.Phase != "action" ||
			!result.PointerMotionCommitted || result.PointerEndpoint == nil || !result.PointerEndpoint.Verified {
			return fmt.Errorf("invalid post-move click action failure")
		}
	default:
		return fmt.Errorf("unknown coordinate_mouse_event failure_code %q", code)
	}
	return nil
}

func validateCoordinateMousePointV1(field string, point CoordinateMouseEventPointV1) error {
	if !finiteCoordinate(point.X) || !finiteCoordinate(point.Y) {
		return fmt.Errorf("%s must contain finite coordinates", field)
	}
	return nil
}

// CoordinateMouseEventCommitUnknownErrorV1 means the mutation request reached
// the transport write boundary, but no valid helper result proved whether its
// side effect committed. Retrying automatically could duplicate a click.
type CoordinateMouseEventCommitUnknownErrorV1 struct {
	cause error
}

func (err *CoordinateMouseEventCommitUnknownErrorV1) Error() string {
	return fmt.Sprintf("coordinate_mouse_event commit unknown (not retry-safe): %v", err.cause)
}

func (err *CoordinateMouseEventCommitUnknownErrorV1) Unwrap() error { return err.cause }

func (err *CoordinateMouseEventCommitUnknownErrorV1) RetrySafe() bool { return false }

func (err *CoordinateMouseEventCommitUnknownErrorV1) CommitUnknown() bool { return true }

func newCoordinateMouseCommitUnknownV1(cause error) error {
	if cause == nil {
		cause = fmt.Errorf("missing valid helper result")
	}
	return &CoordinateMouseEventCommitUnknownErrorV1{cause: cause}
}

// CoordinateMouseEventV1 sends the new mutation RPC without delegating to the
// legacy mouse_event path. Every ambiguity after Write begins is surfaced as
// commit-unknown and is intentionally never retried.
func (client *AXClient) CoordinateMouseEventV1(
	ctx context.Context,
	request CoordinateMouseEventRequestV1,
) (CoordinateMouseEventResultV1, error) {
	if runtime.GOOS != "darwin" {
		return CoordinateMouseEventResultV1{}, fmt.Errorf("ax_server is macOS-only")
	}
	return client.coordinateMouseEventV1(ctx, request)
}

func (client *AXClient) coordinateMouseEventV1(
	ctx context.Context,
	request CoordinateMouseEventRequestV1,
) (CoordinateMouseEventResultV1, error) {
	if err := ctx.Err(); err != nil {
		return CoordinateMouseEventResultV1{}, err
	}
	if err := request.Validate(); err != nil {
		return CoordinateMouseEventResultV1{}, err
	}
	if err := client.Ensure(ctx); err != nil {
		return CoordinateMouseEventResultV1{}, err
	}

	id := client.nextID.Add(1)
	payload, err := EncodeCoordinateMouseEventRPCRequestV1(CoordinateMouseEventRPCRequestV1{
		ID: id, Method: "coordinate_mouse_event", Params: request,
	})
	if err != nil {
		return CoordinateMouseEventResultV1{}, err
	}
	payload = append(payload, '\n')

	responseChannel := make(chan AXResponse, 1)
	client.pendingMu.Lock()
	client.pending[id] = responseChannel
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
		return CoordinateMouseEventResultV1{}, err
	}
	written, writeErr := client.writer.Write(payload)
	if writeErr == nil && written < len(payload) {
		writeErr = io.ErrShortWrite
	}
	client.writeMu.Unlock()
	if writeErr != nil {
		removePending()
		return CoordinateMouseEventResultV1{}, newCoordinateMouseCommitUnknownV1(
			fmt.Errorf("ax_server coordinate mutation write: %w", writeErr))
	}
	commitDeadline, _ := time.Parse(time.RFC3339Nano, request.CommitDeadlineAt)
	now := time.Now()
	hardDeadline := commitDeadline.Add(coordinateMouseTransportGraceV1)
	// An already-expired request still needs a small response window so the
	// helper can return its typed request_expired acknowledgement. Never allow a
	// stale fixture or minor clock skew to turn that explicit result into a
	// transport ambiguity immediately after Write.
	minimumHardDeadline := now.Add(coordinateMouseTransportGraceV1)
	if hardDeadline.Before(minimumHardDeadline) {
		hardDeadline = minimumHardDeadline
	}
	maximumHardDeadline := now.Add(3*time.Second + coordinateMouseTransportGraceV1)
	if hardDeadline.After(maximumHardDeadline) {
		hardDeadline = maximumHardDeadline
	}
	timer := time.NewTimer(time.Until(hardDeadline))
	defer timer.Stop()

	select {
	case response := <-responseChannel:
		removePending()
		if response.Error != nil {
			return CoordinateMouseEventResultV1{}, newCoordinateMouseCommitUnknownV1(
				fmt.Errorf("ax_server RPC error %d: %s", response.Error.Code, response.Error.Message))
		}
		result, decodeErr := DecodeCoordinateMouseEventResultV1(response.Result)
		if decodeErr != nil {
			return CoordinateMouseEventResultV1{}, newCoordinateMouseCommitUnknownV1(
				fmt.Errorf("decode coordinate mutation result: %w", decodeErr))
		}
		if result.Action != request.Action {
			return CoordinateMouseEventResultV1{}, newCoordinateMouseCommitUnknownV1(
				fmt.Errorf("coordinate mutation response action mismatch"))
		}
		if result.PointerEndpoint != nil && result.PointerEndpoint.Requested != request.QuartzPoint {
			return CoordinateMouseEventResultV1{}, newCoordinateMouseCommitUnknownV1(
				fmt.Errorf("coordinate mutation response point mismatch"))
		}
		return result, nil
	case <-timer.C:
		removePending()
		return CoordinateMouseEventResultV1{}, newCoordinateMouseCommitUnknownV1(
			fmt.Errorf("helper acknowledgement timed out after commit deadline"))
	}
}
