package tools

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const coordinatePixelScrollTransportGraceV1 = 150 * time.Millisecond

type CoordinatePixelScrollRequestV1 struct {
	SchemaVersion              int                         `json:"schema_version"`
	TopologyRef                CoordinateTopologyRefV1     `json:"topology_ref"`
	HelperBootID               string                      `json:"helper_boot_id"`
	PID                        int                         `json:"pid"`
	BundleID                   string                      `json:"bundle_id"`
	WindowID                   uint32                      `json:"window_id"`
	ExpectedWindowQuartzBounds CoordinateQuartzRectV1      `json:"expected_window_quartz_bounds"`
	DisplayID                  uint32                      `json:"display_id"`
	QuartzPoint                CoordinateMouseEventPointV1 `json:"quartz_point"`
	Unit                       string                      `json:"unit"`
	ProviderDeltaX             int64                       `json:"provider_delta_x"`
	ProviderDeltaY             int64                       `json:"provider_delta_y"`
	ProviderToQuartzScaleX     float64                     `json:"provider_to_quartz_scale_x"`
	ProviderToQuartzScaleY     float64                     `json:"provider_to_quartz_scale_y"`
	CGPointDeltaAxis1          int64                       `json:"cg_point_delta_axis1"`
	CGPointDeltaAxis2          int64                       `json:"cg_point_delta_axis2"`
	Modifiers                  []string                    `json:"modifiers"`
	TargetPolicy               string                      `json:"target_policy"`
	CommitDeadlineAt           string                      `json:"commit_deadline_at"`
}

type CoordinatePixelScrollRPCRequestV1 struct {
	ID     int64                          `json:"id"`
	Method string                         `json:"method"`
	Params CoordinatePixelScrollRequestV1 `json:"params"`
}

type CoordinatePixelScrollAcknowledgementV1 struct {
	QuartzPoint            CoordinateMouseEventPointV1 `json:"quartz_point"`
	Unit                   string                      `json:"unit"`
	ProviderDeltaX         int64                       `json:"provider_delta_x"`
	ProviderDeltaY         int64                       `json:"provider_delta_y"`
	ProviderToQuartzScaleX float64                     `json:"provider_to_quartz_scale_x"`
	ProviderToQuartzScaleY float64                     `json:"provider_to_quartz_scale_y"`
	CGPointDeltaAxis1      int64                       `json:"cg_point_delta_axis1"`
	CGPointDeltaAxis2      int64                       `json:"cg_point_delta_axis2"`
}

type CoordinatePixelScrollResultV1 struct {
	SchemaVersion          int                                     `json:"schema_version"`
	Status                 string                                  `json:"status"`
	PointerMoveCommitState string                                  `json:"pointer_move_commit_state"`
	ScrollCommitState      string                                  `json:"scroll_commit_state"`
	Phase                  string                                  `json:"phase"`
	FailureCode            *string                                 `json:"failure_code"`
	RetrySafe              bool                                    `json:"retry_safe"`
	Requested              *CoordinatePixelScrollAcknowledgementV1 `json:"requested"`
	PointerEndpoint        *CoordinateMouseEventPointerEndpointV1  `json:"pointer_endpoint"`
}

var coordinatePixelScrollRequestWireShapeV1 = coordinateObjectWireShape(false, map[string]coordinateWireShape{
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
		"unit":                          coordinateScalarWireShape(false),
		"provider_delta_x":              coordinateScalarWireShape(false),
		"provider_delta_y":              coordinateScalarWireShape(false),
		"provider_to_quartz_scale_x":    coordinateScalarWireShape(false),
		"provider_to_quartz_scale_y":    coordinateScalarWireShape(false),
		"cg_point_delta_axis1":          coordinateScalarWireShape(false),
		"cg_point_delta_axis2":          coordinateScalarWireShape(false),
		"modifiers": coordinateArrayWireShape(
			coordinateScalarWireShape(false)),
		"target_policy":      coordinateScalarWireShape(false),
		"commit_deadline_at": coordinateScalarWireShape(false),
	}),
})

var coordinatePixelScrollAcknowledgementWireShapeV1 = coordinateObjectWireShape(false, map[string]coordinateWireShape{
	"quartz_point":               coordinateMousePointWireShapeV1,
	"unit":                       coordinateScalarWireShape(false),
	"provider_delta_x":           coordinateScalarWireShape(false),
	"provider_delta_y":           coordinateScalarWireShape(false),
	"provider_to_quartz_scale_x": coordinateScalarWireShape(false),
	"provider_to_quartz_scale_y": coordinateScalarWireShape(false),
	"cg_point_delta_axis1":       coordinateScalarWireShape(false),
	"cg_point_delta_axis2":       coordinateScalarWireShape(false),
})

var coordinatePixelScrollResultWireShapeV1 = coordinateObjectWireShape(false, map[string]coordinateWireShape{
	"schema_version":            coordinateScalarWireShape(false),
	"status":                    coordinateScalarWireShape(false),
	"pointer_move_commit_state": coordinateScalarWireShape(false),
	"scroll_commit_state":       coordinateScalarWireShape(false),
	"phase":                     coordinateScalarWireShape(false),
	"failure_code":              coordinateScalarWireShape(true),
	"retry_safe":                coordinateScalarWireShape(false),
	"requested":                 coordinateNullableWireShape(coordinatePixelScrollAcknowledgementWireShapeV1),
	"pointer_endpoint":          coordinateNullableWireShape(coordinateMouseEndpointWireShapeV1),
})

func DecodeCoordinatePixelScrollRPCRequestV1(payload []byte) (CoordinatePixelScrollRPCRequestV1, error) {
	if err := validateCoordinateWireShape(
		"coordinate_pixel_scroll request v1", payload, coordinatePixelScrollRequestWireShapeV1,
	); err != nil {
		return CoordinatePixelScrollRPCRequestV1{}, err
	}
	var envelope CoordinatePixelScrollRPCRequestV1
	if err := decodeStrictCoordinateJSON(payload, &envelope); err != nil {
		return CoordinatePixelScrollRPCRequestV1{},
			fmt.Errorf("decode coordinate_pixel_scroll request v1: %w", err)
	}
	if envelope.ID <= 0 || envelope.Method != "coordinate_pixel_scroll" {
		return CoordinatePixelScrollRPCRequestV1{}, fmt.Errorf("invalid coordinate_pixel_scroll RPC envelope")
	}
	if err := envelope.Params.Validate(); err != nil {
		return CoordinatePixelScrollRPCRequestV1{}, err
	}
	return envelope, nil
}

func EncodeCoordinatePixelScrollRPCRequestV1(
	envelope CoordinatePixelScrollRPCRequestV1,
) ([]byte, error) {
	if envelope.ID <= 0 || envelope.Method != "coordinate_pixel_scroll" {
		return nil, fmt.Errorf("invalid coordinate_pixel_scroll RPC envelope")
	}
	if err := envelope.Params.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(envelope)
}

func coordinatePixelScrollProviderDeltasV1(providerX, providerY int64) bool {
	const limit = int64(math.MaxInt32)
	return providerX >= -limit && providerX <= limit &&
		providerY >= -limit && providerY <= limit &&
		(providerX != 0 || providerY != 0)
}

func coordinatePixelScrollCGDeltasV1(
	providerX, providerY int64,
	scaleX, scaleY float64,
) (int64, int64, bool) {
	if !coordinatePixelScrollProviderDeltasV1(providerX, providerY) ||
		!finiteCoordinate(scaleX) || !finiteCoordinate(scaleY) ||
		scaleX <= 0 || scaleY <= 0 {
		return 0, 0, false
	}
	rawAxis1 := -float64(providerY) * scaleY
	rawAxis2 := -float64(providerX) * scaleX
	if !finiteCoordinate(rawAxis1) || !finiteCoordinate(rawAxis2) {
		return 0, 0, false
	}
	axis1 := math.Round(rawAxis1)
	axis2 := math.Round(rawAxis2)
	const limit = float64(math.MaxInt32)
	if axis1 < -limit || axis1 > limit || axis2 < -limit || axis2 > limit {
		return 0, 0, false
	}
	if providerY != 0 && axis1 == 0 || providerX != 0 && axis2 == 0 {
		return 0, 0, false
	}
	return int64(axis1), int64(axis2), true
}

func coordinatePixelScrollAcknowledgementV1(
	request CoordinatePixelScrollRequestV1,
) CoordinatePixelScrollAcknowledgementV1 {
	return CoordinatePixelScrollAcknowledgementV1{
		QuartzPoint: request.QuartzPoint, Unit: request.Unit,
		ProviderDeltaX: request.ProviderDeltaX, ProviderDeltaY: request.ProviderDeltaY,
		ProviderToQuartzScaleX: request.ProviderToQuartzScaleX,
		ProviderToQuartzScaleY: request.ProviderToQuartzScaleY,
		CGPointDeltaAxis1:      request.CGPointDeltaAxis1,
		CGPointDeltaAxis2:      request.CGPointDeltaAxis2,
	}
}

func (request CoordinatePixelScrollRequestV1) Validate() error {
	if request.SchemaVersion != 1 || request.TopologyRef.Generation == 0 ||
		request.PID <= 0 || request.WindowID == 0 || request.DisplayID == 0 {
		return fmt.Errorf("coordinate_pixel_scroll authority is required")
	}
	for name, value := range map[string]string{
		"topology_id": request.TopologyRef.TopologyID, "helper_boot_id": request.HelperBootID,
		"bundle_id": request.BundleID,
	} {
		if value == "" || value != strings.TrimSpace(value) {
			return fmt.Errorf("coordinate_pixel_scroll %s is invalid", name)
		}
	}
	if err := validateCoordinateQuartzRect(
		"expected_window_quartz_bounds", request.ExpectedWindowQuartzBounds,
	); err != nil {
		return err
	}
	if err := validateCoordinateMousePointV1("quartz_point", request.QuartzPoint); err != nil {
		return err
	}
	if request.Unit != "pixel" || request.TargetPolicy != "same_window" {
		return fmt.Errorf("coordinate_pixel_scroll policy is invalid")
	}
	if request.Modifiers == nil {
		return fmt.Errorf("coordinate_pixel_scroll modifiers must be an explicit array")
	}
	if err := validateTargetBoundInputModifiersV1(request.Modifiers); err != nil {
		return fmt.Errorf("coordinate_pixel_scroll modifiers are invalid: %w", err)
	}
	axis1, axis2, ok := coordinatePixelScrollCGDeltasV1(
		request.ProviderDeltaX, request.ProviderDeltaY,
		request.ProviderToQuartzScaleX, request.ProviderToQuartzScaleY)
	if !ok || request.CGPointDeltaAxis1 != axis1 ||
		request.CGPointDeltaAxis2 != axis2 {
		return fmt.Errorf("coordinate_pixel_scroll provider deltas are invalid")
	}
	if _, err := time.Parse(time.RFC3339Nano, request.CommitDeadlineAt); err != nil {
		return fmt.Errorf("coordinate_pixel_scroll commit_deadline_at must be RFC3339: %w", err)
	}
	return nil
}

func DecodeCoordinatePixelScrollResultV1(payload []byte) (CoordinatePixelScrollResultV1, error) {
	if err := validateCoordinateWireShape(
		"coordinate_pixel_scroll result v1", payload, coordinatePixelScrollResultWireShapeV1,
	); err != nil {
		return CoordinatePixelScrollResultV1{}, err
	}
	var result CoordinatePixelScrollResultV1
	if err := decodeStrictCoordinateJSON(payload, &result); err != nil {
		return CoordinatePixelScrollResultV1{},
			fmt.Errorf("decode coordinate_pixel_scroll result v1: %w", err)
	}
	if err := result.ValidateTaggedUnion(); err != nil {
		return CoordinatePixelScrollResultV1{}, err
	}
	return result, nil
}

func EncodeCoordinatePixelScrollResultV1(result CoordinatePixelScrollResultV1) ([]byte, error) {
	if err := result.ValidateTaggedUnion(); err != nil {
		return nil, err
	}
	return json.Marshal(result)
}

func (ack CoordinatePixelScrollAcknowledgementV1) Validate() error {
	if err := validateCoordinateMousePointV1("requested.quartz_point", ack.QuartzPoint); err != nil {
		return err
	}
	axis1, axis2, ok := coordinatePixelScrollCGDeltasV1(
		ack.ProviderDeltaX, ack.ProviderDeltaY,
		ack.ProviderToQuartzScaleX, ack.ProviderToQuartzScaleY)
	if !ok || ack.Unit != "pixel" ||
		ack.CGPointDeltaAxis1 != axis1 || ack.CGPointDeltaAxis2 != axis2 {
		return fmt.Errorf("coordinate_pixel_scroll acknowledgement changed provider semantics")
	}
	return nil
}

func (result CoordinatePixelScrollResultV1) ValidateTaggedUnion() error {
	if result.SchemaVersion != 1 || result.RetrySafe {
		return fmt.Errorf("coordinate_pixel_scroll result schema/retry policy is invalid")
	}
	validState := func(value string) bool {
		return value == "not_committed" || value == "committed" || value == "unknown"
	}
	if !validState(result.PointerMoveCommitState) || !validState(result.ScrollCommitState) {
		return fmt.Errorf("coordinate_pixel_scroll commit state is invalid")
	}
	if result.Requested != nil {
		if err := result.Requested.Validate(); err != nil {
			return err
		}
	}
	if result.PointerEndpoint != nil {
		if err := result.PointerEndpoint.Validate(); err != nil {
			return err
		}
		if result.Requested == nil ||
			result.PointerEndpoint.Requested != result.Requested.QuartzPoint {
			return fmt.Errorf("coordinate_pixel_scroll pointer endpoint does not match request")
		}
	}
	code := ""
	if result.FailureCode != nil {
		code = *result.FailureCode
	}
	switch result.Status {
	case "failed":
		preflight := map[string]bool{
			"invalid_request": true, "request_expired": true, "cancelled": true,
			"topology_unavailable": true, "stale_topology": true,
			"helper_boot_mismatch": true, "process_not_live": true,
			"process_identity_mismatch": true, "window_not_found": true,
			"window_identity_mismatch": true, "window_not_actionable": true,
			"window_bounds_mismatch": true, "display_not_found": true,
			"display_not_actionable": true, "point_outside_window": true,
			"point_outside_display": true, "point_occluded": true,
			"interference_detection_unavailable": true,
			"modifier_press_failed":              true, "cancelled_before_input": true,
			"modifier_release_unconfirmed": true,
		}
		validPhase := preflight[code] && result.Phase == "preflight" ||
			code == "event_preparation_failed" && result.Phase == "preparation" ||
			code == "pointer_move_not_committed" && result.Phase == "pointer_move_commit"
		if result.PointerMoveCommitState != "not_committed" ||
			result.ScrollCommitState != "not_committed" || result.FailureCode == nil ||
			!validPhase || result.PointerEndpoint != nil {
			return fmt.Errorf("invalid failed coordinate_pixel_scroll result")
		}
		if code == "invalid_request" {
			if result.Requested != nil {
				return fmt.Errorf("invalid request cannot acknowledge untrusted scroll semantics")
			}
		} else if result.Requested == nil {
			return fmt.Errorf("coordinate_pixel_scroll failure omitted request acknowledgement")
		}
	case "committed_unverified":
		if result.PointerMoveCommitState != "committed" ||
			result.FailureCode == nil || result.Requested == nil ||
			result.PointerEndpoint == nil {
			return fmt.Errorf("invalid committed_unverified coordinate_pixel_scroll result")
		}
		beforeScroll := result.ScrollCommitState == "not_committed" &&
			result.Phase == "between_commits"
		afterScroll := result.ScrollCommitState == "committed" &&
			result.Phase == "post_verification"
		switch code {
		case "scroll_postcondition_not_declared":
			if !afterScroll || !result.PointerEndpoint.Verified {
				return fmt.Errorf("normal pixel scroll acknowledgement is invalid")
			}
		case "cancelled_before_scroll", "request_expired_before_scroll",
			"scroll_not_committed", "scroll_event_creation_failed",
			"scroll_event_type_mismatch", "scroll_event_location_mismatch",
			"scroll_event_continuity_mismatch", "scroll_event_delta_mismatch",
			"scroll_commit_gate_rejected":
			if !beforeScroll {
				return fmt.Errorf("invalid pre-scroll terminal acknowledgement")
			}
		case "pointer_endpoint_not_verified":
			if !beforeScroll || result.PointerEndpoint.Verified {
				return fmt.Errorf("invalid pointer endpoint acknowledgement")
			}
		case "cancelled_after_scroll", "request_expired_after_scroll":
			if !afterScroll {
				return fmt.Errorf("invalid post-scroll terminal acknowledgement")
			}
		case "interference_detection_unavailable":
			if !beforeScroll && !afterScroll {
				return fmt.Errorf("invalid interference monitor acknowledgement")
			}
		case "modifier_release_unconfirmed":
			if !beforeScroll && !afterScroll {
				return fmt.Errorf("invalid modifier release acknowledgement")
			}
		default:
			return fmt.Errorf("invalid committed_unverified failure code")
		}
	case "user_interference":
		allowed := map[string]bool{
			"physical_input_interference": true, "topology_unavailable": true,
			"stale_topology": true, "helper_boot_mismatch": true,
			"process_not_live": true, "process_identity_mismatch": true,
			"window_not_found": true, "window_identity_mismatch": true,
			"window_not_actionable": true, "window_bounds_mismatch": true,
			"display_not_found": true, "display_not_actionable": true,
			"point_outside_window": true, "point_outside_display": true,
			"point_occluded": true,
		}
		preCommit := result.PointerMoveCommitState == "not_committed" &&
			result.ScrollCommitState == "not_committed" && result.PointerEndpoint == nil
		postMove := result.PointerMoveCommitState == "committed" &&
			(result.ScrollCommitState == "not_committed" || result.ScrollCommitState == "committed") &&
			result.PointerEndpoint != nil
		validPlacement := postMove
		if code == "physical_input_interference" {
			validPlacement = preCommit || postMove
		}
		if result.Phase != "user_interference" || result.FailureCode == nil ||
			!allowed[code] || result.Requested == nil || !validPlacement {
			return fmt.Errorf("invalid user_interference coordinate_pixel_scroll result")
		}
	case "commit_unknown":
		unknownMove := result.PointerMoveCommitState == "unknown" &&
			result.ScrollCommitState == "not_committed" &&
			code == "pointer_move_commit_unknown"
		unknownScroll := result.PointerMoveCommitState == "committed" &&
			result.ScrollCommitState == "unknown" &&
			code == "scroll_commit_unknown"
		if result.FailureCode == nil || result.Requested == nil ||
			(!unknownMove && !unknownScroll) ||
			(unknownMove && result.Phase != "pointer_move_commit") ||
			(unknownScroll && result.Phase != "scroll_commit") ||
			(unknownScroll && result.PointerEndpoint == nil) {
			return fmt.Errorf("invalid commit_unknown coordinate_pixel_scroll result")
		}
	default:
		return fmt.Errorf("invalid coordinate_pixel_scroll status %q", result.Status)
	}
	return nil
}

type CoordinatePixelScrollCommitUnknownErrorV1 struct{ cause error }

func (err *CoordinatePixelScrollCommitUnknownErrorV1) Error() string {
	return fmt.Sprintf("coordinate_pixel_scroll commit unknown (not retry-safe): %v", err.cause)
}
func (err *CoordinatePixelScrollCommitUnknownErrorV1) Unwrap() error       { return err.cause }
func (err *CoordinatePixelScrollCommitUnknownErrorV1) RetrySafe() bool     { return false }
func (err *CoordinatePixelScrollCommitUnknownErrorV1) CommitUnknown() bool { return true }

func newCoordinatePixelScrollCommitUnknownV1(cause error) error {
	if cause == nil {
		cause = fmt.Errorf("missing valid helper result")
	}
	return &CoordinatePixelScrollCommitUnknownErrorV1{cause: cause}
}

func coordinatePixelScrollCancellationMarkerPath(helperBootID string, requestID int64) string {
	digest := sha256.Sum256([]byte(fmt.Sprintf("%s:%d", helperBootID, requestID)))
	return filepath.Join(
		"/tmp", fmt.Sprintf("kocoro-ax-pixel-scroll-cancel-v1-%x", digest[:]))
}

func writeCoordinatePixelScrollCancellationMarker(path string) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	return file.Close()
}

func (client *AXClient) CoordinatePixelScrollV1(
	ctx context.Context, request CoordinatePixelScrollRequestV1,
) (CoordinatePixelScrollResultV1, error) {
	if runtime.GOOS != "darwin" {
		return CoordinatePixelScrollResultV1{}, fmt.Errorf("ax_server is macOS-only")
	}
	return client.coordinatePixelScrollV1(ctx, request)
}

func (client *AXClient) coordinatePixelScrollV1(
	ctx context.Context, request CoordinatePixelScrollRequestV1,
) (CoordinatePixelScrollResultV1, error) {
	if err := ctx.Err(); err != nil {
		return CoordinatePixelScrollResultV1{}, err
	}
	if err := request.Validate(); err != nil {
		return CoordinatePixelScrollResultV1{}, err
	}
	if err := client.Ensure(ctx); err != nil {
		return CoordinatePixelScrollResultV1{}, err
	}
	id := client.nextID.Add(1)
	marker := coordinatePixelScrollCancellationMarkerPath(request.HelperBootID, id)
	_ = os.Remove(marker)
	defer os.Remove(marker)
	payload, err := EncodeCoordinatePixelScrollRPCRequestV1(
		CoordinatePixelScrollRPCRequestV1{
			ID: id, Method: "coordinate_pixel_scroll", Params: request,
		})
	if err != nil {
		return CoordinatePixelScrollResultV1{}, err
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
		return CoordinatePixelScrollResultV1{}, err
	}
	written, writeErr := client.writer.Write(payload)
	if writeErr == nil && written < len(payload) {
		writeErr = io.ErrShortWrite
	}
	client.writeMu.Unlock()
	if writeErr != nil {
		removePending()
		return CoordinatePixelScrollResultV1{},
			newCoordinatePixelScrollCommitUnknownV1(writeErr)
	}
	commitDeadline, _ := time.Parse(time.RFC3339Nano, request.CommitDeadlineAt)
	now := time.Now()
	hardDeadline := commitDeadline.Add(coordinatePixelScrollTransportGraceV1)
	minimumHardDeadline := now.Add(coordinatePixelScrollTransportGraceV1)
	if hardDeadline.Before(minimumHardDeadline) {
		hardDeadline = minimumHardDeadline
	}
	maximumHardDeadline := now.Add(3*time.Second + coordinatePixelScrollTransportGraceV1)
	if hardDeadline.After(maximumHardDeadline) {
		hardDeadline = maximumHardDeadline
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
				return CoordinatePixelScrollResultV1{},
					newCoordinatePixelScrollCommitUnknownV1(fmt.Errorf(
						"ax_server RPC error %d: %s",
						response.Error.Code, response.Error.Message))
			}
			result, err := DecodeCoordinatePixelScrollResultV1(response.Result)
			if err != nil {
				return CoordinatePixelScrollResultV1{},
					newCoordinatePixelScrollCommitUnknownV1(err)
			}
			expected := coordinatePixelScrollAcknowledgementV1(request)
			if result.Requested == nil || *result.Requested != expected ||
				result.PointerEndpoint != nil &&
					result.PointerEndpoint.Requested != request.QuartzPoint {
				return CoordinatePixelScrollResultV1{},
					newCoordinatePixelScrollCommitUnknownV1(
						fmt.Errorf("coordinate_pixel_scroll response acknowledgement mismatch"))
			}
			return result, nil
		case <-ctxDone:
			ctxDone = nil
			if err := writeCoordinatePixelScrollCancellationMarker(marker); err != nil {
				cancellationSignalError = err
			}
		case <-timer.C:
			removePending()
			if cancellationSignalError != nil {
				return CoordinatePixelScrollResultV1{},
					newCoordinatePixelScrollCommitUnknownV1(fmt.Errorf(
						"helper acknowledgement timed out; cancellation signal: %w",
						cancellationSignalError))
			}
			return CoordinatePixelScrollResultV1{},
				newCoordinatePixelScrollCommitUnknownV1(
					fmt.Errorf("helper acknowledgement timed out after commit deadline"))
		}
	}
}
