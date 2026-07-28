package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime"
	"strings"
	"time"
	"unicode"
	"unicode/utf16"
)

const backgroundTargetedInputMaximumUTF16V1 = 2048

// BackgroundTargetedInputRequestV1 is a distinct capability from the existing
// foreground target_bound_input RPC. It binds one already-focused editable AX
// element inside an exact non-frontmost process and one exact process that must
// remain frontmost for the entire commit. It never authorizes activation.
type BackgroundTargetedInputRequestV1 struct {
	SchemaVersion                int                       `json:"schema_version"`
	Input                        TargetBoundInputRequestV1 `json:"input"`
	FocusedRef                   string                    `json:"focused_ref"`
	FocusedPath                  string                    `json:"focused_path"`
	ExpectedFocusedRole          string                    `json:"expected_focused_role"`
	ExpectedFocusedFingerprint   string                    `json:"expected_focused_fingerprint"`
	TargetLaunchDate             string                    `json:"target_launch_date"`
	PreservedFrontmostPID        int                       `json:"preserved_frontmost_pid"`
	PreservedFrontmostBundleID   string                    `json:"preserved_frontmost_bundle_id"`
	PreservedFrontmostLaunchDate string                    `json:"preserved_frontmost_launch_date"`
}

type BackgroundTargetedInputRPCRequestV1 struct {
	ID     int64                            `json:"id"`
	Method string                           `json:"method"`
	Params BackgroundTargetedInputRequestV1 `json:"params"`
}

var backgroundTargetedInputRequestWireShapeV1 = coordinateObjectWireShape(false, map[string]coordinateWireShape{
	"id":     coordinateScalarWireShape(false),
	"method": coordinateScalarWireShape(false),
	"params": coordinateObjectWireShape(false, map[string]coordinateWireShape{
		"schema_version":                  coordinateScalarWireShape(false),
		"input":                           targetBoundInputParamsWireShapeV1,
		"focused_ref":                     coordinateScalarWireShape(false),
		"focused_path":                    coordinateScalarWireShape(false),
		"expected_focused_role":           coordinateScalarWireShape(false),
		"expected_focused_fingerprint":    coordinateScalarWireShape(false),
		"target_launch_date":              coordinateScalarWireShape(false),
		"preserved_frontmost_pid":         coordinateScalarWireShape(false),
		"preserved_frontmost_bundle_id":   coordinateScalarWireShape(false),
		"preserved_frontmost_launch_date": coordinateScalarWireShape(false),
	}),
})

func (request BackgroundTargetedInputRequestV1) Validate() error {
	if request.SchemaVersion != 1 {
		return fmt.Errorf("background_targeted_input schema_version is invalid")
	}
	if err := request.Input.Validate(); err != nil {
		return err
	}
	if request.Input.Action != "type" && request.Input.Action != "keypress" {
		return fmt.Errorf("background_targeted_input supports type or keypress")
	}
	if !validComputerUseRef(request.FocusedRef) ||
		!strictTargetBoundIdentity(request.FocusedPath) ||
		(request.FocusedPath != "window[0]" &&
			!strings.HasPrefix(request.FocusedPath, "window[0]/")) ||
		!strictTargetBoundIdentity(request.ExpectedFocusedRole) ||
		!strictTargetBoundIdentity(request.ExpectedFocusedFingerprint) {
		return fmt.Errorf("background_targeted_input focused element authority is invalid")
	}
	switch request.ExpectedFocusedRole {
	case "AXTextField", "AXTextArea", "AXComboBox":
	default:
		return fmt.Errorf("background_targeted_input requires an editable focused role")
	}
	if request.Input.Action == "type" {
		if request.Input.Ref == nil || *request.Input.Ref != request.FocusedRef ||
			request.Input.Path == nil || *request.Input.Path != request.FocusedPath ||
			request.Input.ExpectedRole == nil ||
			*request.Input.ExpectedRole != request.ExpectedFocusedRole ||
			request.Input.ExpectedFingerprint == nil ||
			*request.Input.ExpectedFingerprint != request.ExpectedFocusedFingerprint ||
			request.Input.Text == nil ||
			strings.IndexFunc(*request.Input.Text, unicode.IsControl) >= 0 ||
			len(utf16.Encode([]rune(*request.Input.Text))) >
				backgroundTargetedInputMaximumUTF16V1 {
			return fmt.Errorf("background_targeted_input type authority is inconsistent")
		}
	}
	if request.Input.Action == "keypress" &&
		backgroundTargetedInputConsequentialKeyV1(
			*request.Input.Keys,
			*request.Input.Modifiers,
		) {
		return fmt.Errorf("background_targeted_input consequential key is not supported")
	}
	if request.PreservedFrontmostPID <= 0 ||
		request.PreservedFrontmostPID == request.Input.PID ||
		request.PreservedFrontmostBundleID == "" ||
		request.PreservedFrontmostBundleID !=
			strings.TrimSpace(request.PreservedFrontmostBundleID) {
		return fmt.Errorf("background_targeted_input preserved frontmost authority is invalid")
	}
	for field, value := range map[string]string{
		"target_launch_date":              request.TargetLaunchDate,
		"preserved_frontmost_launch_date": request.PreservedFrontmostLaunchDate,
	} {
		if _, err := time.Parse(time.RFC3339Nano, value); err != nil {
			return fmt.Errorf("background_targeted_input %s must be RFC3339: %w", field, err)
		}
	}
	return nil
}

func backgroundTargetedInputConsequentialKeyV1(
	keys []string,
	modifiers []string,
) bool {
	modifierSet := make(map[string]struct{}, len(modifiers))
	for _, modifier := range modifiers {
		modifierSet[canonicalComputerUseHotkeyTokenV1(modifier)] = struct{}{}
	}
	has := func(modifier string) bool {
		_, exists := modifierSet[modifier]
		return exists
	}
	for _, raw := range keys {
		switch canonicalComputerUseHotkeyTokenV1(raw) {
		case "return", "enter":
			if len(modifierSet) == 0 || has("command") || has("control") {
				return true
			}
		case "delete", "backspace", "forwarddelete":
			if has("shift") || has("command") {
				return true
			}
		}
	}
	return false
}

func DecodeBackgroundTargetedInputRPCRequestV1(
	payload []byte,
) (BackgroundTargetedInputRPCRequestV1, error) {
	if err := validateCoordinateWireShape(
		"background_targeted_input request v1",
		payload,
		backgroundTargetedInputRequestWireShapeV1,
	); err != nil {
		return BackgroundTargetedInputRPCRequestV1{}, err
	}
	var envelope BackgroundTargetedInputRPCRequestV1
	if err := decodeStrictCoordinateJSON(payload, &envelope); err != nil {
		return BackgroundTargetedInputRPCRequestV1{},
			fmt.Errorf("decode background_targeted_input request v1: %w", err)
	}
	if envelope.ID <= 0 || envelope.Method != "background_targeted_input" {
		return BackgroundTargetedInputRPCRequestV1{},
			fmt.Errorf("invalid background_targeted_input RPC envelope")
	}
	if err := envelope.Params.Validate(); err != nil {
		return BackgroundTargetedInputRPCRequestV1{}, err
	}
	return envelope, nil
}

func EncodeBackgroundTargetedInputRPCRequestV1(
	envelope BackgroundTargetedInputRPCRequestV1,
) ([]byte, error) {
	if envelope.ID <= 0 || envelope.Method != "background_targeted_input" {
		return nil, fmt.Errorf("invalid background_targeted_input RPC envelope")
	}
	if err := envelope.Params.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(envelope)
}

func (client *AXClient) BackgroundTargetedInputV1(
	ctx context.Context,
	request BackgroundTargetedInputRequestV1,
) (TargetBoundInputResultV1, error) {
	if runtime.GOOS != "darwin" {
		return TargetBoundInputResultV1{}, fmt.Errorf("ax_server is macOS-only")
	}
	if err := request.Validate(); err != nil {
		return TargetBoundInputResultV1{}, err
	}
	return client.targetBoundInputRPCV1(
		ctx,
		request.Input,
		func(id int64) ([]byte, error) {
			return EncodeBackgroundTargetedInputRPCRequestV1(
				BackgroundTargetedInputRPCRequestV1{
					ID: id, Method: "background_targeted_input", Params: request,
				},
			)
		},
	)
}
