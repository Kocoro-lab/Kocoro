package tools

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"
)

type CaptureCoordinateDisplayRequestV1 struct {
	SchemaVersion int                     `json:"schema_version"`
	TopologyRef   CoordinateTopologyRefV1 `json:"topology_ref"`
	DisplayID     uint32                  `json:"display_id"`
}

type CaptureCoordinateDisplayRPCRequestV1 struct {
	ID     int                               `json:"id"`
	Method string                            `json:"method"`
	Params CaptureCoordinateDisplayRequestV1 `json:"params"`
}

var captureCoordinateDisplayRequestWireShapeV1 = coordinateObjectWireShape(false, map[string]coordinateWireShape{
	"id":     coordinateScalarWireShape(false),
	"method": coordinateScalarWireShape(false),
	"params": coordinateObjectWireShape(false, map[string]coordinateWireShape{
		"schema_version": coordinateScalarWireShape(false),
		"topology_ref":   coordinateTopologyRefWireShapeV1,
		"display_id":     coordinateScalarWireShape(false),
	}),
})

func DecodeCaptureCoordinateDisplayRPCRequestV1(payload []byte) (CaptureCoordinateDisplayRPCRequestV1, error) {
	if err := validateCoordinateWireShape(
		"capture_coordinate_display request v1", payload, captureCoordinateDisplayRequestWireShapeV1); err != nil {
		return CaptureCoordinateDisplayRPCRequestV1{}, err
	}
	var envelope CaptureCoordinateDisplayRPCRequestV1
	if err := decodeStrictCoordinateJSON(payload, &envelope); err != nil {
		return CaptureCoordinateDisplayRPCRequestV1{}, fmt.Errorf("decode capture_coordinate_display request v1: %w", err)
	}
	if envelope.ID <= 0 || envelope.Method != "capture_coordinate_display" {
		return CaptureCoordinateDisplayRPCRequestV1{}, fmt.Errorf("invalid capture_coordinate_display RPC envelope")
	}
	if err := envelope.Params.Validate(); err != nil {
		return CaptureCoordinateDisplayRPCRequestV1{}, err
	}
	return envelope, nil
}

func (request CaptureCoordinateDisplayRequestV1) Validate() error {
	if request.SchemaVersion != 1 || request.TopologyRef.TopologyID == "" ||
		request.TopologyRef.Generation == 0 || request.DisplayID == 0 {
		return fmt.Errorf("capture_coordinate_display request display authority is required")
	}
	if request.TopologyRef.TopologyID != strings.TrimSpace(request.TopologyRef.TopologyID) {
		return fmt.Errorf("capture_coordinate_display topology_id contains surrounding whitespace")
	}
	return nil
}

type CaptureCoordinateDisplayResultV1 struct {
	SchemaVersion       int                      `json:"schema_version"`
	Status              string                   `json:"status"`
	FailureCode         *string                  `json:"failure_code"`
	RetrySafe           bool                     `json:"retry_safe"`
	TopologyRef         *CoordinateTopologyRefV1 `json:"topology_ref"`
	HelperBootID        *string                  `json:"helper_boot_id"`
	DisplayID           *uint32                  `json:"display_id"`
	DisplayQuartzBounds *CoordinateQuartzRectV1  `json:"display_quartz_bounds"`
	BackingScaleFactor  *float64                 `json:"backing_scale_factor"`
	MediaType           *string                  `json:"media_type"`
	WidthPX             *int                     `json:"width_px"`
	HeightPX            *int                     `json:"height_px"`
	ByteLength          *int                     `json:"byte_length"`
	SHA256              *string                  `json:"sha256"`
	ImageBase64         *string                  `json:"image_base64"`
	CapturedAt          *string                  `json:"captured_at"`
}

var captureCoordinateDisplayResultWireShapeV1 = coordinateObjectWireShape(false, map[string]coordinateWireShape{
	"schema_version":        coordinateScalarWireShape(false),
	"status":                coordinateScalarWireShape(false),
	"failure_code":          coordinateScalarWireShape(true),
	"retry_safe":            coordinateScalarWireShape(false),
	"topology_ref":          coordinateNullableWireShape(coordinateTopologyRefWireShapeV1),
	"helper_boot_id":        coordinateScalarWireShape(true),
	"display_id":            coordinateScalarWireShape(true),
	"display_quartz_bounds": coordinateNullableWireShape(coordinateQuartzRectWireShapeV1),
	"backing_scale_factor":  coordinateScalarWireShape(true),
	"media_type":            coordinateScalarWireShape(true),
	"width_px":              coordinateScalarWireShape(true),
	"height_px":             coordinateScalarWireShape(true),
	"byte_length":           coordinateScalarWireShape(true),
	"sha256":                coordinateScalarWireShape(true),
	"image_base64":          coordinateScalarWireShape(true),
	"captured_at":           coordinateScalarWireShape(true),
})

func DecodeCaptureCoordinateDisplayResultV1(payload []byte) (CaptureCoordinateDisplayResultV1, error) {
	if err := validateCoordinateWireShape(
		"capture_coordinate_display result v1", payload, captureCoordinateDisplayResultWireShapeV1); err != nil {
		return CaptureCoordinateDisplayResultV1{}, err
	}
	var result CaptureCoordinateDisplayResultV1
	if err := decodeStrictCoordinateJSON(payload, &result); err != nil {
		return CaptureCoordinateDisplayResultV1{}, fmt.Errorf("decode capture_coordinate_display result v1: %w", err)
	}
	if err := result.ValidateTaggedUnion(); err != nil {
		return CaptureCoordinateDisplayResultV1{}, err
	}
	return result, nil
}

func EncodeCaptureCoordinateDisplayResultV1(result CaptureCoordinateDisplayResultV1) ([]byte, error) {
	if err := result.ValidateTaggedUnion(); err != nil {
		return nil, err
	}
	return json.Marshal(result)
}

func (result CaptureCoordinateDisplayResultV1) ValidateTaggedUnion() error {
	if result.SchemaVersion != 1 {
		return fmt.Errorf("unsupported capture_coordinate_display result schema_version %d", result.SchemaVersion)
	}
	successFields := []bool{
		result.TopologyRef != nil, result.HelperBootID != nil, result.DisplayID != nil,
		result.DisplayQuartzBounds != nil, result.BackingScaleFactor != nil,
		result.MediaType != nil, result.WidthPX != nil, result.HeightPX != nil,
		result.ByteLength != nil, result.SHA256 != nil, result.ImageBase64 != nil,
		result.CapturedAt != nil,
	}
	switch result.Status {
	case "captured":
		if result.FailureCode != nil || result.RetrySafe {
			return fmt.Errorf("captured display result cannot carry failure metadata")
		}
		for _, present := range successFields {
			if !present {
				return fmt.Errorf("captured display result requires every success field")
			}
		}
		if result.TopologyRef.TopologyID == "" || result.TopologyRef.Generation == 0 ||
			*result.HelperBootID == "" || *result.DisplayID == 0 ||
			!finiteCoordinate(*result.BackingScaleFactor) || *result.BackingScaleFactor <= 0 ||
			*result.MediaType != "image/png" || *result.WidthPX <= 0 || *result.HeightPX <= 0 ||
			*result.ByteLength <= 0 || !validLowerHexSHA256(*result.SHA256) || *result.ImageBase64 == "" {
			return fmt.Errorf("captured display result contains invalid success values")
		}
		if err := validateCoordinateQuartzRect("display_quartz_bounds", *result.DisplayQuartzBounds); err != nil {
			return err
		}
		if _, err := time.Parse(time.RFC3339Nano, *result.CapturedAt); err != nil {
			return fmt.Errorf("captured_at must be RFC3339: %w", err)
		}
	case "failed":
		if result.FailureCode == nil || *result.FailureCode == "" {
			return fmt.Errorf("failed display result requires a known failure_code")
		}
		expectedRetrySafe, valid := captureCoordinateDisplayFailurePolicy(*result.FailureCode)
		if !valid || result.RetrySafe != expectedRetrySafe {
			return fmt.Errorf("failed display result failure_code/retry_safe policy mismatch")
		}
		for _, present := range successFields {
			if present {
				return fmt.Errorf("failed display result cannot carry success fields")
			}
		}
	default:
		return fmt.Errorf("invalid capture_coordinate_display status %q", result.Status)
	}
	return nil
}

func captureCoordinateDisplayFailurePolicy(code string) (retrySafe bool, valid bool) {
	switch code {
	case "topology_unavailable", "stale_topology", "display_not_found",
		"display_not_actionable", "capture_timeout", "capture_failed", "topology_changed":
		return true, true
	case "invalid_request", "image_too_large", "invalid_png",
		"image_dimensions_mismatch", "response_too_large":
		return false, true
	default:
		return false, false
	}
}

type CaptureCoordinateDisplayLimitsV1 struct {
	MaxRawBytes    int
	MaxNDJSONBytes int
	MaxPixels      int
}

func AdmitCaptureCoordinateDisplayV1(
	payload []byte,
	request CaptureCoordinateDisplayRequestV1,
	currentTopology DisplayTopologyV1,
	limits CaptureCoordinateDisplayLimitsV1,
) (CaptureCoordinateDisplayResultV1, error) {
	if err := request.Validate(); err != nil {
		return CaptureCoordinateDisplayResultV1{}, err
	}
	if limits.MaxRawBytes <= 0 || limits.MaxNDJSONBytes <= 0 || limits.MaxPixels <= 0 {
		return CaptureCoordinateDisplayResultV1{}, fmt.Errorf("capture_coordinate_display limits must be positive")
	}
	if len(payload) > limits.MaxNDJSONBytes {
		return CaptureCoordinateDisplayResultV1{}, fmt.Errorf("capture_coordinate_display result exceeds NDJSON cap")
	}
	result, err := DecodeCaptureCoordinateDisplayResultV1(payload)
	if err != nil {
		return CaptureCoordinateDisplayResultV1{}, err
	}
	if result.Status == "failed" {
		return result, nil
	}
	if err := currentTopology.Validate(); err != nil {
		return CaptureCoordinateDisplayResultV1{}, fmt.Errorf("current display topology: %w", err)
	}
	currentRef := CoordinateTopologyRefV1{
		TopologyID: currentTopology.TopologyID, Generation: currentTopology.Generation,
	}
	if request.TopologyRef != currentRef || *result.TopologyRef != currentRef {
		return CaptureCoordinateDisplayResultV1{}, fmt.Errorf("display capture topology authority mismatch")
	}
	if *result.HelperBootID != currentTopology.HelperBootID {
		return CaptureCoordinateDisplayResultV1{}, fmt.Errorf("display capture helper boot identity mismatch")
	}
	topologyCapturedAt, err := time.Parse(time.RFC3339Nano, currentTopology.CapturedAt)
	if err != nil {
		return CaptureCoordinateDisplayResultV1{}, fmt.Errorf("parse current topology captured_at: %w", err)
	}
	captureCapturedAt, err := time.Parse(time.RFC3339Nano, *result.CapturedAt)
	if err != nil {
		return CaptureCoordinateDisplayResultV1{}, fmt.Errorf("parse display capture captured_at: %w", err)
	}
	if !captureCapturedAt.After(topologyCapturedAt) {
		return CaptureCoordinateDisplayResultV1{}, fmt.Errorf("display capture timestamp must be later than current topology")
	}
	if *result.DisplayID != request.DisplayID {
		return CaptureCoordinateDisplayResultV1{}, fmt.Errorf("display capture target identity mismatch")
	}
	var display *DisplayTopologyDisplayV1
	for index := range currentTopology.Displays {
		if currentTopology.Displays[index].DisplayID == request.DisplayID {
			display = &currentTopology.Displays[index]
			break
		}
	}
	if display == nil {
		return CaptureCoordinateDisplayResultV1{}, fmt.Errorf("display capture target is absent from topology")
	}
	if !display.IsActive || !display.IsOnline || display.IsAsleep ||
		display.MirrorMasterDisplayID != nil || display.RotationDegrees != 0 {
		return CaptureCoordinateDisplayResultV1{}, fmt.Errorf("display capture target is not actionable")
	}
	wantBounds := CoordinateQuartzRectV1(display.QuartzBounds)
	if *result.DisplayQuartzBounds != wantBounds {
		return CaptureCoordinateDisplayResultV1{}, fmt.Errorf("display capture Quartz bounds mismatch")
	}
	if math.Abs(*result.BackingScaleFactor-display.BackingScaleFactor) > 0.000001 {
		return CaptureCoordinateDisplayResultV1{}, fmt.Errorf("display capture backing scale mismatch")
	}
	if *result.WidthPX != display.PixelWidth || *result.HeightPX != display.PixelHeight {
		return CaptureCoordinateDisplayResultV1{}, fmt.Errorf("display capture dimensions do not match topology pixels")
	}
	if *result.WidthPX > limits.MaxPixels / *result.HeightPX {
		return CaptureCoordinateDisplayResultV1{}, fmt.Errorf("display capture exceeds decoded pixel cap")
	}
	if captureCoordinateWindowBase64HasWhitespace(*result.ImageBase64) {
		return CaptureCoordinateDisplayResultV1{}, fmt.Errorf("display capture image_base64 is not canonical")
	}
	decodedLength := base64.StdEncoding.DecodedLen(len(*result.ImageBase64))
	if strings.HasSuffix(*result.ImageBase64, "=") {
		decodedLength--
	}
	if strings.HasSuffix(*result.ImageBase64, "==") {
		decodedLength--
	}
	if decodedLength > limits.MaxRawBytes {
		return CaptureCoordinateDisplayResultV1{}, fmt.Errorf("display capture image_base64 exceeds raw byte cap")
	}
	raw, err := base64.StdEncoding.Strict().DecodeString(*result.ImageBase64)
	if err != nil || base64.StdEncoding.EncodeToString(raw) != *result.ImageBase64 {
		return CaptureCoordinateDisplayResultV1{}, fmt.Errorf("display capture image_base64 is not canonical")
	}
	if len(raw) > limits.MaxRawBytes {
		return CaptureCoordinateDisplayResultV1{}, fmt.Errorf("display capture exceeds raw byte cap")
	}
	if len(raw) != *result.ByteLength {
		return CaptureCoordinateDisplayResultV1{}, fmt.Errorf("display capture byte_length mismatch")
	}
	if captureWindowSHA256(raw) != *result.SHA256 {
		return CaptureCoordinateDisplayResultV1{}, fmt.Errorf("display capture sha256 mismatch")
	}
	if err := validateCaptureCoordinateWindowPNG(raw, *result.WidthPX, *result.HeightPX); err != nil {
		return CaptureCoordinateDisplayResultV1{}, fmt.Errorf("display capture PNG: %w", err)
	}
	return result, nil
}

func ReadCaptureCoordinateDisplayV1(
	ctx context.Context,
	caller displayTopologyRPCCaller,
	request CaptureCoordinateDisplayRequestV1,
	currentTopology DisplayTopologyV1,
	limits CaptureCoordinateDisplayLimitsV1,
) (CaptureCoordinateDisplayResultV1, error) {
	if err := request.Validate(); err != nil {
		return CaptureCoordinateDisplayResultV1{}, err
	}
	payload, err := caller.Call(ctx, "capture_coordinate_display", request)
	if err != nil {
		return CaptureCoordinateDisplayResultV1{}, fmt.Errorf("capture_coordinate_display RPC: %w", err)
	}
	return AdmitCaptureCoordinateDisplayV1(payload, request, currentTopology, limits)
}

func (client *AXClient) CaptureCoordinateDisplayV1(
	ctx context.Context,
	request CaptureCoordinateDisplayRequestV1,
	currentTopology DisplayTopologyV1,
	limits CaptureCoordinateDisplayLimitsV1,
) (CaptureCoordinateDisplayResultV1, error) {
	return ReadCaptureCoordinateDisplayV1(ctx, client, request, currentTopology, limits)
}
