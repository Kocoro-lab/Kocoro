package tools

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// coordinateDragMaximumWaypointsV1 is tied to the helper's bounded synthetic
// path budget: at most 48 pointer samples are prepared before mouseDown, so
// every provider waypoint can be preserved without silently collapsing the
// trajectory or allocating an unbounded event sequence.
const coordinateDragMaximumWaypointsV1 = 48

type CoordinateDragWaypointV1 struct {
	DisplayID   uint32                      `json:"display_id"`
	QuartzPoint CoordinateMouseEventPointV1 `json:"quartz_point"`
}

type CoordinateDragRequestV1 struct {
	SchemaVersion              int                         `json:"schema_version"`
	TopologyRef                CoordinateTopologyRefV1     `json:"topology_ref"`
	HelperBootID               string                      `json:"helper_boot_id"`
	PID                        int                         `json:"pid"`
	BundleID                   string                      `json:"bundle_id"`
	WindowID                   uint32                      `json:"window_id"`
	ExpectedWindowQuartzBounds CoordinateQuartzRectV1      `json:"expected_window_quartz_bounds"`
	StartDisplayID             uint32                      `json:"start_display_id"`
	EndDisplayID               uint32                      `json:"end_display_id"`
	StartQuartzPoint           CoordinateMouseEventPointV1 `json:"start_quartz_point"`
	EndQuartzPoint             CoordinateMouseEventPointV1 `json:"end_quartz_point"`
	Waypoints                  []CoordinateDragWaypointV1  `json:"waypoints"`
	Button                     string                      `json:"button"`
	Modifiers                  []string                    `json:"modifiers"`
	DurationMS                 int                         `json:"duration_ms"`
	EndTargetPolicy            string                      `json:"end_target_policy"`
	CommitDeadlineAt           string                      `json:"commit_deadline_at"`
}

type CoordinateDragRPCRequestV1 struct {
	ID     int64                   `json:"id"`
	Method string                  `json:"method"`
	Params CoordinateDragRequestV1 `json:"params"`
}

type CoordinateDragResultV1 struct {
	SchemaVersion          int                                    `json:"schema_version"`
	Status                 string                                 `json:"status"`
	DragCommitted          bool                                   `json:"drag_committed"`
	MouseDownCommitted     bool                                   `json:"mouse_down_committed"`
	PointerMotionCommitted bool                                   `json:"pointer_motion_committed"`
	MouseUpCommitted       bool                                   `json:"mouse_up_committed"`
	PossibleDropSideEffect bool                                   `json:"possible_drop_side_effect"`
	Phase                  string                                 `json:"phase"`
	FailureCode            *string                                `json:"failure_code"`
	RetrySafe              bool                                   `json:"retry_safe"`
	Postcondition          *string                                `json:"postcondition"`
	PointerEndpoint        *CoordinateMouseEventPointerEndpointV1 `json:"pointer_endpoint"`
}

var coordinateDragRequestWireShapeV1 = coordinateObjectWireShape(false, map[string]coordinateWireShape{
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
		"start_display_id":              coordinateScalarWireShape(false),
		"end_display_id":                coordinateScalarWireShape(false),
		"start_quartz_point":            coordinateMousePointWireShapeV1,
		"end_quartz_point":              coordinateMousePointWireShapeV1,
		"waypoints": coordinateArrayWireShape(coordinateObjectWireShape(false, map[string]coordinateWireShape{
			"display_id":   coordinateScalarWireShape(false),
			"quartz_point": coordinateMousePointWireShapeV1,
		})),
		"button": coordinateScalarWireShape(false),
		"modifiers": coordinateArrayWireShape(
			coordinateScalarWireShape(false)),
		"duration_ms":        coordinateScalarWireShape(false),
		"end_target_policy":  coordinateScalarWireShape(false),
		"commit_deadline_at": coordinateScalarWireShape(false),
	}),
})

var coordinateDragResultWireShapeV1 = coordinateObjectWireShape(false, map[string]coordinateWireShape{
	"schema_version":            coordinateScalarWireShape(false),
	"status":                    coordinateScalarWireShape(false),
	"drag_committed":            coordinateScalarWireShape(false),
	"mouse_down_committed":      coordinateScalarWireShape(false),
	"pointer_motion_committed":  coordinateScalarWireShape(false),
	"mouse_up_committed":        coordinateScalarWireShape(false),
	"possible_drop_side_effect": coordinateScalarWireShape(false),
	"phase":                     coordinateScalarWireShape(false),
	"failure_code":              coordinateScalarWireShape(true),
	"retry_safe":                coordinateScalarWireShape(false),
	"postcondition":             coordinateScalarWireShape(true),
	"pointer_endpoint":          coordinateNullableWireShape(coordinateMouseEndpointWireShapeV1),
})

func DecodeCoordinateDragRPCRequestV1(payload []byte) (CoordinateDragRPCRequestV1, error) {
	if err := validateCoordinateWireShape("coordinate_drag request v1", payload, coordinateDragRequestWireShapeV1); err != nil {
		return CoordinateDragRPCRequestV1{}, err
	}
	var envelope CoordinateDragRPCRequestV1
	if err := decodeStrictCoordinateJSON(payload, &envelope); err != nil {
		return CoordinateDragRPCRequestV1{}, fmt.Errorf("decode coordinate_drag request v1: %w", err)
	}
	if envelope.ID <= 0 || envelope.Method != "coordinate_drag" {
		return CoordinateDragRPCRequestV1{}, fmt.Errorf("invalid coordinate_drag RPC envelope")
	}
	if err := envelope.Params.Validate(); err != nil {
		return CoordinateDragRPCRequestV1{}, err
	}
	return envelope, nil
}

func EncodeCoordinateDragRPCRequestV1(envelope CoordinateDragRPCRequestV1) ([]byte, error) {
	if envelope.ID <= 0 || envelope.Method != "coordinate_drag" {
		return nil, fmt.Errorf("invalid coordinate_drag RPC envelope")
	}
	if err := envelope.Params.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(envelope)
}

func (request CoordinateDragRequestV1) Validate() error {
	if request.SchemaVersion != 1 || request.TopologyRef.Generation == 0 ||
		request.PID <= 0 || request.WindowID == 0 ||
		request.StartDisplayID == 0 || request.EndDisplayID == 0 {
		return fmt.Errorf("coordinate_drag authority is required")
	}
	for name, value := range map[string]string{
		"topology_id":    request.TopologyRef.TopologyID,
		"helper_boot_id": request.HelperBootID,
		"bundle_id":      request.BundleID,
	} {
		if value == "" || value != strings.TrimSpace(value) {
			return fmt.Errorf("coordinate_drag %s is invalid", name)
		}
	}
	if err := validateCoordinateQuartzRect("expected_window_quartz_bounds", request.ExpectedWindowQuartzBounds); err != nil {
		return err
	}
	if err := validateCoordinateMousePointV1("start_quartz_point", request.StartQuartzPoint); err != nil {
		return err
	}
	if err := validateCoordinateMousePointV1("end_quartz_point", request.EndQuartzPoint); err != nil {
		return err
	}
	if len(request.Waypoints) < 2 || len(request.Waypoints) > coordinateDragMaximumWaypointsV1 {
		return fmt.Errorf("coordinate_drag requires 2..%d waypoints", coordinateDragMaximumWaypointsV1)
	}
	for index, waypoint := range request.Waypoints {
		if waypoint.DisplayID == 0 {
			return fmt.Errorf("coordinate_drag waypoint %d display authority is required", index)
		}
		if err := validateCoordinateMousePointV1(
			fmt.Sprintf("waypoints[%d].quartz_point", index),
			waypoint.QuartzPoint,
		); err != nil {
			return err
		}
		if index > 0 && waypoint.QuartzPoint == request.Waypoints[index-1].QuartzPoint {
			return fmt.Errorf("coordinate_drag adjacent waypoints must be distinct")
		}
	}
	first, last := request.Waypoints[0], request.Waypoints[len(request.Waypoints)-1]
	if first.DisplayID != request.StartDisplayID ||
		first.QuartzPoint != request.StartQuartzPoint ||
		last.DisplayID != request.EndDisplayID ||
		last.QuartzPoint != request.EndQuartzPoint {
		return fmt.Errorf("coordinate_drag waypoint endpoints do not match request authority")
	}
	if request.Button != "left" || request.DurationMS < 120 || request.DurationMS > 800 ||
		request.EndTargetPolicy != "same_window" {
		return fmt.Errorf("coordinate_drag policy is invalid")
	}
	if request.Modifiers == nil {
		return fmt.Errorf("coordinate_drag modifiers must be an explicit array")
	}
	if err := validateTargetBoundInputModifiersV1(request.Modifiers); err != nil {
		return fmt.Errorf("coordinate_drag modifiers are invalid: %w", err)
	}
	if _, err := time.Parse(time.RFC3339Nano, request.CommitDeadlineAt); err != nil {
		return fmt.Errorf("coordinate_drag commit_deadline_at must be RFC3339: %w", err)
	}
	return nil
}

func DecodeCoordinateDragResultV1(payload []byte) (CoordinateDragResultV1, error) {
	if err := validateCoordinateWireShape("coordinate_drag result v1", payload, coordinateDragResultWireShapeV1); err != nil {
		return CoordinateDragResultV1{}, err
	}
	var result CoordinateDragResultV1
	if err := decodeStrictCoordinateJSON(payload, &result); err != nil {
		return CoordinateDragResultV1{}, fmt.Errorf("decode coordinate_drag result v1: %w", err)
	}
	if err := result.ValidateTaggedUnion(); err != nil {
		return CoordinateDragResultV1{}, err
	}
	return result, nil
}

func EncodeCoordinateDragResultV1(result CoordinateDragResultV1) ([]byte, error) {
	if err := result.ValidateTaggedUnion(); err != nil {
		return nil, err
	}
	return json.Marshal(result)
}

func (result CoordinateDragResultV1) ValidateTaggedUnion() error {
	if result.SchemaVersion != 1 || result.RetrySafe {
		return fmt.Errorf("coordinate_drag result schema/retry policy is invalid")
	}
	if result.PointerEndpoint != nil {
		if err := result.PointerEndpoint.Validate(); err != nil {
			return err
		}
	}
	switch result.Status {
	case "completed_unverified":
		if !result.DragCommitted || !result.MouseDownCommitted || !result.PossibleDropSideEffect ||
			result.Phase != "post_verification" || result.FailureCode == nil ||
			result.Postcondition != nil || result.PointerEndpoint == nil {
			return fmt.Errorf("invalid completed_unverified coordinate_drag result")
		}
		if *result.FailureCode == "modifier_release_unconfirmed" {
			return nil
		}
		if result.MouseUpCommitted {
			if *result.FailureCode == "interference_detection_unavailable" {
				return nil
			}
			if result.PointerEndpoint.Verified {
				if !result.PointerMotionCommitted ||
					*result.FailureCode != "drop_postcondition_not_declared" {
					return fmt.Errorf("drag endpoint was verified without a drop postcondition")
				}
			} else if !result.PointerMotionCommitted ||
				*result.FailureCode != "pointer_endpoint_not_verified" {
				return fmt.Errorf("invalid endpoint-unverified coordinate_drag result")
			}
		} else if *result.FailureCode != "mouse_up_post_unverified" {
			return fmt.Errorf("invalid mouseUp-unverified coordinate_drag result")
		}
	case "user_interference":
		allowed := map[string]bool{
			"pointer_interference": true, "cancelled_during_drag": true,
			"request_expired_during_drag": true, "drag_event_post_failed": true,
			"end_target_changed": true, "cancelled_before_drop": true,
			"request_expired_before_drop": true,
			"topology_unavailable":        true, "stale_topology": true,
			"helper_boot_mismatch": true, "process_not_live": true,
			"process_identity_mismatch": true, "window_not_found": true,
			"window_identity_mismatch": true, "window_not_actionable": true,
			"window_bounds_mismatch": true, "start_outside_window": true,
			"end_outside_window": true, "waypoint_outside_window": true,
			"waypoint_display_not_actionable": true, "start_display_not_actionable": true,
			"end_display_not_actionable":  true,
			"physical_input_interference": true, "interference_detection_unavailable": true,
		}
		preCommit := !result.DragCommitted && !result.MouseDownCommitted &&
			!result.PointerMotionCommitted && !result.MouseUpCommitted &&
			!result.PossibleDropSideEffect && result.Phase == "user_interference" &&
			result.FailureCode != nil && *result.FailureCode == "physical_input_interference" &&
			result.Postcondition == nil && result.PointerEndpoint == nil
		afterDown := result.DragCommitted && result.MouseDownCommitted && result.MouseUpCommitted &&
			result.PossibleDropSideEffect && result.Phase == "cleanup" &&
			result.FailureCode != nil && allowed[*result.FailureCode] &&
			result.Postcondition == nil && result.PointerEndpoint != nil
		if !preCommit && !afterDown {
			return fmt.Errorf("invalid user_interference coordinate_drag result")
		}
	case "failed":
		preflight := map[string]bool{
			"invalid_request": true, "request_expired": true,
			"topology_unavailable": true, "stale_topology": true,
			"helper_boot_mismatch": true, "process_not_live": true,
			"process_identity_mismatch": true, "window_not_found": true,
			"window_identity_mismatch": true, "window_not_actionable": true,
			"window_bounds_mismatch": true, "start_outside_window": true,
			"end_outside_window": true, "waypoint_outside_window": true,
			"waypoint_display_not_actionable": true, "start_display_not_actionable": true,
			"end_display_not_actionable": true, "start_point_occluded": true,
			"end_point_occluded": true, "cancelled": true,
			"interference_detection_unavailable": true,
			"modifier_press_failed":              true, "cancelled_before_input": true,
			"modifier_release_unconfirmed": true,
		}
		exactPhase := result.FailureCode != nil &&
			((preflight[*result.FailureCode] && result.Phase == "preflight") ||
				(*result.FailureCode == "event_preparation_failed" && result.Phase == "preparation") ||
				(*result.FailureCode == "mouse_down_failed" && result.Phase == "action"))
		if result.DragCommitted || result.MouseDownCommitted ||
			result.PointerMotionCommitted || result.MouseUpCommitted ||
			result.PossibleDropSideEffect || result.FailureCode == nil ||
			!exactPhase || result.Postcondition != nil || result.PointerEndpoint != nil {
			return fmt.Errorf("invalid failed coordinate_drag result")
		}
	default:
		return fmt.Errorf("invalid coordinate_drag status %q", result.Status)
	}
	return nil
}

type CoordinateDragCommitUnknownErrorV1 struct{ cause error }

const coordinateDragTransportGraceV1 = 150 * time.Millisecond

func coordinateDragCancellationMarkerPath(helperBootID string, requestID int64) string {
	digest := sha256.Sum256([]byte(fmt.Sprintf("%s:%d", helperBootID, requestID)))
	// LaunchServices may not preserve the daemon's TMPDIR for the helper. /tmp
	// is the shared sticky temp directory visible to both process identities.
	return filepath.Join("/tmp", fmt.Sprintf("kocoro-ax-drag-cancel-v1-%x", digest[:]))
}

func writeCoordinateDragCancellationMarker(path string) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	return file.Close()
}

func (err *CoordinateDragCommitUnknownErrorV1) Error() string {
	return fmt.Sprintf("coordinate_drag commit unknown (not retry-safe): %v", err.cause)
}
func (err *CoordinateDragCommitUnknownErrorV1) Unwrap() error       { return err.cause }
func (err *CoordinateDragCommitUnknownErrorV1) RetrySafe() bool     { return false }
func (err *CoordinateDragCommitUnknownErrorV1) CommitUnknown() bool { return true }

func newCoordinateDragCommitUnknownV1(cause error) error {
	if cause == nil {
		cause = fmt.Errorf("missing valid helper result")
	}
	return &CoordinateDragCommitUnknownErrorV1{cause: cause}
}

func (client *AXClient) CoordinateDragV1(
	ctx context.Context, request CoordinateDragRequestV1,
) (CoordinateDragResultV1, error) {
	if runtime.GOOS != "darwin" {
		return CoordinateDragResultV1{}, fmt.Errorf("ax_server is macOS-only")
	}
	return client.coordinateDragV1(ctx, request)
}

func (client *AXClient) coordinateDragV1(
	ctx context.Context, request CoordinateDragRequestV1,
) (CoordinateDragResultV1, error) {
	if err := ctx.Err(); err != nil {
		return CoordinateDragResultV1{}, err
	}
	if err := request.Validate(); err != nil {
		return CoordinateDragResultV1{}, err
	}
	if err := client.Ensure(ctx); err != nil {
		return CoordinateDragResultV1{}, err
	}
	id := client.nextID.Add(1)
	cancellationMarker := coordinateDragCancellationMarkerPath(request.HelperBootID, id)
	// A client restart may reuse a request ID while the helper boot stays live.
	// Remove a stale marker before the new request becomes visible to Swift.
	_ = os.Remove(cancellationMarker)
	defer os.Remove(cancellationMarker)
	payload, err := EncodeCoordinateDragRPCRequestV1(CoordinateDragRPCRequestV1{
		ID: id, Method: "coordinate_drag", Params: request,
	})
	if err != nil {
		return CoordinateDragResultV1{}, err
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
		return CoordinateDragResultV1{}, err
	}
	written, writeErr := client.writer.Write(payload)
	if writeErr == nil && written < len(payload) {
		writeErr = io.ErrShortWrite
	}
	client.writeMu.Unlock()
	if writeErr != nil {
		removePending()
		return CoordinateDragResultV1{}, newCoordinateDragCommitUnknownV1(writeErr)
	}
	commitDeadline, _ := time.Parse(time.RFC3339Nano, request.CommitDeadlineAt)
	hardDeadline := commitDeadline.Add(coordinateDragTransportGraceV1)
	maximumHardDeadline := time.Now().Add(3*time.Second + coordinateDragTransportGraceV1)
	if hardDeadline.After(maximumHardDeadline) {
		hardDeadline = maximumHardDeadline
	}
	timer := time.NewTimer(time.Until(hardDeadline))
	defer timer.Stop()
	// Once the mutation bytes are written, parent cancellation must not release
	// the coordinator's action barrier while the synchronous helper may still
	// hold the mouse button. Wait for the helper's typed mouseUp/cleanup ack or
	// the request deadline plus a small transport grace. The helper polls the
	// same request deadline on every path step, so the hard deadline is also the
	// conservative quiescence boundary.
	ctxDone := ctx.Done()
	var cancellationSignalError error
	for {
		select {
		case response := <-responses:
			removePending()
			if response.Error != nil {
				return CoordinateDragResultV1{}, newCoordinateDragCommitUnknownV1(
					fmt.Errorf("ax_server RPC error %d: %s", response.Error.Code, response.Error.Message))
			}
			result, err := DecodeCoordinateDragResultV1(response.Result)
			if err != nil {
				return CoordinateDragResultV1{}, newCoordinateDragCommitUnknownV1(err)
			}
			if result.PointerEndpoint != nil && result.PointerEndpoint.Requested != request.EndQuartzPoint {
				return CoordinateDragResultV1{}, newCoordinateDragCommitUnknownV1(
					fmt.Errorf("coordinate_drag response endpoint mismatch"))
			}
			return result, nil
		case <-ctxDone:
			// The helper's socket loop is intentionally synchronous. Signal its
			// in-flight operation out-of-band, then continue waiting for mouseUp.
			ctxDone = nil
			if err := writeCoordinateDragCancellationMarker(cancellationMarker); err != nil {
				cancellationSignalError = err
			}
		case <-timer.C:
			removePending()
			if cancellationSignalError != nil {
				return CoordinateDragResultV1{}, newCoordinateDragCommitUnknownV1(fmt.Errorf(
					"helper cleanup acknowledgement timed out; cancellation signal: %w",
					cancellationSignalError))
			}
			return CoordinateDragResultV1{}, newCoordinateDragCommitUnknownV1(
				fmt.Errorf("helper cleanup acknowledgement timed out after commit deadline"))
		}
	}
}
