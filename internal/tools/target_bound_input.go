package tools

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"time"
)

// The helper may finish its bounded 350ms AX postcondition check and restore
// the clipboard after the input commit deadline. Keep the transport waiting
// long enough to receive that typed acknowledgement instead of misclassifying
// a successful Electron input as commit_unknown.
const targetBoundInputTransportGraceV1 = 1250 * time.Millisecond

func targetBoundInputCancellationMarkerPathV1(
	request TargetBoundInputRequestV1, requestID int64,
) string {
	authority := fmt.Sprintf(
		"%d:%s:%d:%d", request.PID, request.BundleID, request.WindowID, requestID)
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(authority)))
	return "/tmp/kocoro-ax-target-input-cancel-v1-" + digest
}

type TargetBoundInputRequestV1 struct {
	SchemaVersion          int                    `json:"schema_version"`
	PID                    int                    `json:"pid"`
	BundleID               string                 `json:"bundle_id"`
	WindowID               uint32                 `json:"window_id"`
	ExpectedWindowAXBounds CoordinateQuartzRectV1 `json:"expected_window_ax_bounds"`
	Action                 string                 `json:"action"`
	Ref                    *string                `json:"ref"`
	Path                   *string                `json:"path"`
	ExpectedRole           *string                `json:"expected_role"`
	ExpectedFingerprint    *string                `json:"expected_fingerprint"`
	Text                   *string                `json:"text"`
	Key                    *string                `json:"key"`
	Keys                   *[]string              `json:"keys"`
	Modifiers              *[]string              `json:"modifiers"`
	CommitDeadlineAt       string                 `json:"commit_deadline_at"`
}

type TargetBoundInputRPCRequestV1 struct {
	ID     int64                     `json:"id"`
	Method string                    `json:"method"`
	Params TargetBoundInputRequestV1 `json:"params"`
}

type TargetBoundInputResultV1 struct {
	SchemaVersion     int     `json:"schema_version"`
	Status            string  `json:"status"`
	Action            string  `json:"action"`
	InputCommitted    bool    `json:"input_committed"`
	ClipboardTouched  bool    `json:"clipboard_touched"`
	ClipboardRestored bool    `json:"clipboard_restored"`
	Phase             string  `json:"phase"`
	FailureCode       *string `json:"failure_code"`
	RetrySafe         bool    `json:"retry_safe"`
	Postcondition     *string `json:"postcondition"`
}

var targetBoundInputRequestWireShapeV1 = coordinateObjectWireShape(false, map[string]coordinateWireShape{
	"id":     coordinateScalarWireShape(false),
	"method": coordinateScalarWireShape(false),
	"params": coordinateObjectWireShape(false, map[string]coordinateWireShape{
		"schema_version":            coordinateScalarWireShape(false),
		"pid":                       coordinateScalarWireShape(false),
		"bundle_id":                 coordinateScalarWireShape(false),
		"window_id":                 coordinateScalarWireShape(false),
		"expected_window_ax_bounds": coordinateQuartzRectWireShapeV1,
		"action":                    coordinateScalarWireShape(false),
		"ref":                       coordinateScalarWireShape(true),
		"path":                      coordinateScalarWireShape(true),
		"expected_role":             coordinateScalarWireShape(true),
		"expected_fingerprint":      coordinateScalarWireShape(true),
		"text":                      coordinateScalarWireShape(true),
		"key":                       coordinateScalarWireShape(true),
		"keys": coordinateNullableWireShape(
			coordinateArrayWireShape(coordinateScalarWireShape(false))),
		"modifiers": coordinateNullableWireShape(
			coordinateArrayWireShape(coordinateScalarWireShape(false))),
		"commit_deadline_at": coordinateScalarWireShape(false),
	}),
})

var targetBoundInputResultWireShapeV1 = coordinateObjectWireShape(false, map[string]coordinateWireShape{
	"schema_version":     coordinateScalarWireShape(false),
	"status":             coordinateScalarWireShape(false),
	"action":             coordinateScalarWireShape(false),
	"input_committed":    coordinateScalarWireShape(false),
	"clipboard_touched":  coordinateScalarWireShape(false),
	"clipboard_restored": coordinateScalarWireShape(false),
	"phase":              coordinateScalarWireShape(false),
	"failure_code":       coordinateScalarWireShape(true),
	"retry_safe":         coordinateScalarWireShape(false),
	"postcondition":      coordinateScalarWireShape(true),
})

func (request TargetBoundInputRequestV1) Validate() error {
	if request.SchemaVersion != 1 || request.PID <= 0 || request.WindowID == 0 {
		return fmt.Errorf("target_bound_input request authority is required")
	}
	if request.BundleID == "" || request.BundleID != strings.TrimSpace(request.BundleID) {
		return fmt.Errorf("target_bound_input bundle_id is invalid")
	}
	if err := validateCoordinateQuartzRect(
		"expected_window_ax_bounds", request.ExpectedWindowAXBounds); err != nil {
		return err
	}
	if _, err := time.Parse(time.RFC3339Nano, request.CommitDeadlineAt); err != nil {
		return fmt.Errorf("target_bound_input commit_deadline_at must be RFC3339: %w", err)
	}
	switch request.Action {
	case "type":
		elementBound := request.Ref != nil && validComputerUseRef(*request.Ref) &&
			request.Path != nil &&
			(*request.Path == "window[0]" || strings.HasPrefix(*request.Path, "window[0]/")) &&
			request.ExpectedRole != nil && strictTargetBoundIdentity(*request.ExpectedRole) &&
			request.ExpectedFingerprint != nil &&
			strictTargetBoundIdentity(*request.ExpectedFingerprint)
		windowBound := request.Ref == nil && request.Path == nil &&
			request.ExpectedRole == nil && request.ExpectedFingerprint == nil
		if (!elementBound && !windowBound) || request.Text == nil || *request.Text == "" ||
			request.Key != nil || request.Keys != nil || request.Modifiers != nil {
			return fmt.Errorf("target_bound_input type requires either element authority or exact window authority")
		}
	case "hotkey":
		if request.Ref != nil || request.Path != nil || request.ExpectedRole != nil ||
			request.ExpectedFingerprint != nil ||
			request.Text != nil || request.Key == nil || *request.Key == "" ||
			*request.Key != strings.TrimSpace(*request.Key) || request.Keys != nil ||
			request.Modifiers == nil || *request.Modifiers == nil ||
			len(*request.Modifiers) > 4 {
			return fmt.Errorf("target_bound_input hotkey requires key/modifiers and null text/keys")
		}
		if err := validateTargetBoundInputModifiersV1(*request.Modifiers); err != nil {
			return err
		}
	case "keypress":
		if request.Ref != nil || request.Path != nil || request.ExpectedRole != nil ||
			request.ExpectedFingerprint != nil ||
			request.Text != nil || request.Key != nil ||
			request.Keys == nil || len(*request.Keys) == 0 || len(*request.Keys) > 64 ||
			request.Modifiers == nil || *request.Modifiers == nil ||
			len(*request.Modifiers) > 4 {
			return fmt.Errorf("target_bound_input keypress requires keys/modifiers and null text/key")
		}
		if err := validateTargetBoundInputModifiersV1(*request.Modifiers); err != nil {
			return err
		}
		for _, key := range *request.Keys {
			if !validTargetBoundInputKeyV1(key) {
				return fmt.Errorf("target_bound_input keypress key %q is invalid", key)
			}
		}
	default:
		return fmt.Errorf("unsupported target_bound_input action %q", request.Action)
	}
	return nil
}

func validateTargetBoundInputModifiersV1(modifiers []string) error {
	valid := map[string]bool{
		"command": true, "shift": true, "option": true, "control": true,
	}
	seen := make(map[string]struct{}, len(modifiers))
	for _, modifier := range modifiers {
		if modifier == "" || modifier != strings.TrimSpace(modifier) ||
			!valid[modifier] {
			return fmt.Errorf("target_bound_input modifier is invalid")
		}
		if _, duplicate := seen[modifier]; duplicate {
			return fmt.Errorf("target_bound_input modifier is duplicated")
		}
		seen[modifier] = struct{}{}
	}
	return nil
}

func validTargetBoundInputKeyV1(key string) bool {
	if key == "" || key != strings.TrimSpace(key) {
		return false
	}
	switch key {
	case "return", "escape", "tab", "delete", "backspace", "home", "end",
		"pageup", "pagedown", "up", "down", "left", "right", "space":
		return true
	}
	return len(key) == 1 && key[0] >= 0x20 && key[0] <= 0x7e &&
		(key[0] < 'A' || key[0] > 'Z')
}

func strictTargetBoundIdentity(value string) bool {
	return value != "" && value == strings.TrimSpace(value)
}

func validComputerUseRef(value string) bool {
	if len(value) < 2 || value[0] != 'e' {
		return false
	}
	for _, digit := range value[1:] {
		if digit < '0' || digit > '9' {
			return false
		}
	}
	return true
}

func DecodeTargetBoundInputRPCRequestV1(payload []byte) (TargetBoundInputRPCRequestV1, error) {
	if err := validateCoordinateWireShape(
		"target_bound_input request v1", payload, targetBoundInputRequestWireShapeV1); err != nil {
		return TargetBoundInputRPCRequestV1{}, err
	}
	var envelope TargetBoundInputRPCRequestV1
	if err := decodeStrictCoordinateJSON(payload, &envelope); err != nil {
		return TargetBoundInputRPCRequestV1{}, fmt.Errorf("decode target_bound_input request v1: %w", err)
	}
	if envelope.ID <= 0 || envelope.Method != "target_bound_input" {
		return TargetBoundInputRPCRequestV1{}, fmt.Errorf("invalid target_bound_input RPC envelope")
	}
	if err := envelope.Params.Validate(); err != nil {
		return TargetBoundInputRPCRequestV1{}, err
	}
	return envelope, nil
}

func EncodeTargetBoundInputRPCRequestV1(envelope TargetBoundInputRPCRequestV1) ([]byte, error) {
	if envelope.ID <= 0 || envelope.Method != "target_bound_input" {
		return nil, fmt.Errorf("invalid target_bound_input RPC envelope")
	}
	if err := envelope.Params.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(envelope)
}

func (result TargetBoundInputResultV1) ValidateTaggedUnion() error {
	if result.SchemaVersion != 1 ||
		(result.Action != "type" && result.Action != "hotkey" &&
			result.Action != "keypress" && result.Action != "unknown") {
		return fmt.Errorf("invalid target_bound_input result schema or action")
	}
	if result.RetrySafe {
		return fmt.Errorf("target_bound_input mutation results are never retry-safe")
	}
	if result.ClipboardRestored && !result.ClipboardTouched {
		return fmt.Errorf("target_bound_input cannot restore an untouched clipboard")
	}
	if (result.Action == "hotkey" || result.Action == "keypress") &&
		(result.ClipboardTouched || result.ClipboardRestored) {
		return fmt.Errorf("target_bound_input key action cannot touch the clipboard")
	}
	if result.Action == "unknown" {
		if result.Status != "failed" || result.InputCommitted || result.Phase != "preflight" ||
			result.FailureCode == nil || *result.FailureCode != "invalid_request" ||
			result.Postcondition != nil {
			return fmt.Errorf("target_bound_input unknown action is reserved for invalid_request")
		}
		return nil
	}
	if result.Status == "verified" {
		if result.Action != "type" || !result.InputCommitted || result.Phase != "post_verification" ||
			result.FailureCode != nil || result.Postcondition == nil ||
			*result.Postcondition != "target_value_matches_expected_edit" ||
			(result.ClipboardTouched && !result.ClipboardRestored) {
			return fmt.Errorf("incoherent verified target_bound_input result")
		}
		return nil
	}
	if result.Status == "completed_unverified" {
		allowed := map[string]bool{
			"postcondition_not_declared":             true,
			"clipboard_restore_failed_after_commit":  true,
			"verification_redacted_sensitive_target": true,
			"target_value_readback_unavailable":      true,
			"target_value_noop_unverifiable":         true,
			"target_value_mismatch":                  true,
			"target_changed_during_verification":     true,
			"interference_detection_unavailable":     true,
			"cancelled_after_partial_input":          true,
			"event_post_failed":                      true,
			"modifier_release_unconfirmed":           true,
		}
		if result.FailureCode == nil {
			return fmt.Errorf("incoherent completed_unverified target_bound_input result")
		}
		validPhase := result.Phase == "post_verification"
		if result.Action == "keypress" &&
			(*result.FailureCode == "cancelled_after_partial_input" ||
				*result.FailureCode == "event_post_failed" ||
				*result.FailureCode == "modifier_release_unconfirmed") {
			validPhase = result.Phase == "action"
		}
		if !result.InputCommitted || !validPhase ||
			!allowed[*result.FailureCode] || result.Postcondition != nil {
			return fmt.Errorf("incoherent completed_unverified target_bound_input result")
		}
		if *result.FailureCode == "clipboard_restore_failed_after_commit" &&
			(!result.ClipboardTouched || result.ClipboardRestored) {
			return fmt.Errorf("clipboard restore failure tuple is incoherent")
		}
		if *result.FailureCode == "postcondition_not_declared" &&
			result.ClipboardTouched && !result.ClipboardRestored {
			return fmt.Errorf("completed clipboard input omitted restore failure")
		}
		return nil
	}
	if result.Status == "user_interference" {
		if result.Phase != "user_interference" || result.FailureCode == nil ||
			*result.FailureCode != "physical_input_interference" ||
			result.Postcondition != nil {
			return fmt.Errorf("incoherent user_interference target_bound_input result")
		}
		return nil
	}
	if result.Status != "failed" || result.InputCommitted || result.FailureCode == nil ||
		*result.FailureCode == "" || result.Postcondition != nil ||
		(result.Phase != "preflight" && result.Phase != "preparation" && result.Phase != "action") {
		return fmt.Errorf("incoherent failed target_bound_input result")
	}
	return nil
}

func DecodeTargetBoundInputResultV1(payload []byte) (TargetBoundInputResultV1, error) {
	if err := validateCoordinateWireShape(
		"target_bound_input result v1", payload, targetBoundInputResultWireShapeV1); err != nil {
		return TargetBoundInputResultV1{}, err
	}
	var result TargetBoundInputResultV1
	if err := decodeStrictCoordinateJSON(payload, &result); err != nil {
		return TargetBoundInputResultV1{}, fmt.Errorf("decode target_bound_input result v1: %w", err)
	}
	if err := result.ValidateTaggedUnion(); err != nil {
		return TargetBoundInputResultV1{}, err
	}
	return result, nil
}

func EncodeTargetBoundInputResultV1(result TargetBoundInputResultV1) ([]byte, error) {
	if err := result.ValidateTaggedUnion(); err != nil {
		return nil, err
	}
	return json.Marshal(result)
}

type TargetBoundInputCommitUnknownErrorV1 struct{ cause error }

func (err *TargetBoundInputCommitUnknownErrorV1) Error() string {
	return fmt.Sprintf("target_bound_input commit unknown (not retry-safe): %v", err.cause)
}
func (err *TargetBoundInputCommitUnknownErrorV1) Unwrap() error   { return err.cause }
func (err *TargetBoundInputCommitUnknownErrorV1) RetrySafe() bool { return false }
func (err *TargetBoundInputCommitUnknownErrorV1) CommitUnknown() bool {
	return true
}

func newTargetBoundInputCommitUnknownV1(cause error) error {
	if cause == nil {
		cause = fmt.Errorf("missing valid helper result")
	}
	return &TargetBoundInputCommitUnknownErrorV1{cause: cause}
}

func (client *AXClient) TargetBoundInputV1(
	ctx context.Context,
	request TargetBoundInputRequestV1,
) (TargetBoundInputResultV1, error) {
	if runtime.GOOS != "darwin" {
		return TargetBoundInputResultV1{}, fmt.Errorf("ax_server is macOS-only")
	}
	return client.targetBoundInputV1(ctx, request)
}

func (client *AXClient) targetBoundInputV1(
	ctx context.Context,
	request TargetBoundInputRequestV1,
) (TargetBoundInputResultV1, error) {
	if err := ctx.Err(); err != nil {
		return TargetBoundInputResultV1{}, err
	}
	if err := request.Validate(); err != nil {
		return TargetBoundInputResultV1{}, err
	}
	if err := client.Ensure(ctx); err != nil {
		return TargetBoundInputResultV1{}, err
	}

	id := client.nextID.Add(1)
	cancellationMarker := targetBoundInputCancellationMarkerPathV1(request, id)
	_ = os.Remove(cancellationMarker)
	defer os.Remove(cancellationMarker)
	payload, err := EncodeTargetBoundInputRPCRequestV1(TargetBoundInputRPCRequestV1{
		ID: id, Method: "target_bound_input", Params: request,
	})
	if err != nil {
		return TargetBoundInputResultV1{}, err
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
		return TargetBoundInputResultV1{}, err
	}
	written, writeErr := client.writer.Write(payload)
	if writeErr == nil && written < len(payload) {
		writeErr = io.ErrShortWrite
	}
	client.writeMu.Unlock()
	if writeErr != nil {
		removePending()
		return TargetBoundInputResultV1{}, newTargetBoundInputCommitUnknownV1(
			fmt.Errorf("ax_server target-bound input write: %w", writeErr))
	}

	commitDeadline, _ := time.Parse(time.RFC3339Nano, request.CommitDeadlineAt)
	now := time.Now()
	hardDeadline := commitDeadline.Add(targetBoundInputTransportGraceV1)
	if minimum := now.Add(targetBoundInputTransportGraceV1); hardDeadline.Before(minimum) {
		hardDeadline = minimum
	}
	if maximum := now.Add(2*time.Second + targetBoundInputTransportGraceV1); hardDeadline.After(maximum) {
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
				return TargetBoundInputResultV1{}, newTargetBoundInputCommitUnknownV1(fmt.Errorf(
					"ax_server RPC error %d: %s", response.Error.Code, response.Error.Message))
			}
			result, decodeErr := DecodeTargetBoundInputResultV1(response.Result)
			if decodeErr != nil {
				return TargetBoundInputResultV1{}, newTargetBoundInputCommitUnknownV1(fmt.Errorf(
					"decode target-bound input result: %w", decodeErr))
			}
			// "unknown" is the helper's strict, typed preflight rejection. It
			// explicitly proves that no input committed, so preserve the result
			// for the caller instead of turning a malformed request into a
			// misleading commit-unknown error.
			if result.Action == "unknown" {
				return result, nil
			}
			if result.Action != request.Action {
				return TargetBoundInputResultV1{}, newTargetBoundInputCommitUnknownV1(
					fmt.Errorf(
						"target-bound input response action mismatch: requested %q, received %q",
						request.Action,
						result.Action,
					))
			}
			return result, nil
		case <-ctxDone:
			ctxDone = nil
			if err := writeCoordinateDragCancellationMarker(cancellationMarker); err != nil {
				cancellationSignalError = err
			}
		case <-timer.C:
			removePending()
			if cancellationSignalError != nil {
				return TargetBoundInputResultV1{}, newTargetBoundInputCommitUnknownV1(fmt.Errorf(
					"helper cleanup acknowledgement timed out; cancellation signal: %w",
					cancellationSignalError))
			}
			return TargetBoundInputResultV1{}, newTargetBoundInputCommitUnknownV1(
				fmt.Errorf("helper cleanup acknowledgement timed out after commit deadline"))
		}
	}
}
