package tools

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"image/png"
	"math"
	"strings"
	"time"
)

type CaptureCoordinateWindowRequestV1 struct {
	SchemaVersion        int                     `json:"schema_version"`
	TopologyRef          CoordinateTopologyRefV1 `json:"topology_ref"`
	PID                  int                     `json:"pid"`
	BundleID             string                  `json:"bundle_id"`
	WindowID             uint32                  `json:"window_id"`
	ExpectedQuartzBounds CoordinateQuartzRectV1  `json:"expected_quartz_bounds"`
}

type CaptureCoordinateWindowRPCRequestV1 struct {
	ID     int                              `json:"id"`
	Method string                           `json:"method"`
	Params CaptureCoordinateWindowRequestV1 `json:"params"`
}

var captureCoordinateWindowRequestWireShapeV1 = coordinateObjectWireShape(false, map[string]coordinateWireShape{
	"id":     coordinateScalarWireShape(false),
	"method": coordinateScalarWireShape(false),
	"params": coordinateObjectWireShape(false, map[string]coordinateWireShape{
		"schema_version":         coordinateScalarWireShape(false),
		"topology_ref":           coordinateTopologyRefWireShapeV1,
		"pid":                    coordinateScalarWireShape(false),
		"bundle_id":              coordinateScalarWireShape(false),
		"window_id":              coordinateScalarWireShape(false),
		"expected_quartz_bounds": coordinateQuartzRectWireShapeV1,
	}),
})

func DecodeCaptureCoordinateWindowRPCRequestV1(payload []byte) (CaptureCoordinateWindowRPCRequestV1, error) {
	if err := validateCoordinateWireShape("capture_coordinate_window request v1", payload, captureCoordinateWindowRequestWireShapeV1); err != nil {
		return CaptureCoordinateWindowRPCRequestV1{}, err
	}
	var envelope CaptureCoordinateWindowRPCRequestV1
	if err := decodeStrictCoordinateJSON(payload, &envelope); err != nil {
		return CaptureCoordinateWindowRPCRequestV1{}, fmt.Errorf("decode capture_coordinate_window request v1: %w", err)
	}
	if envelope.ID <= 0 || envelope.Method != "capture_coordinate_window" {
		return CaptureCoordinateWindowRPCRequestV1{}, fmt.Errorf("invalid capture_coordinate_window RPC envelope")
	}
	if err := envelope.Params.Validate(); err != nil {
		return CaptureCoordinateWindowRPCRequestV1{}, err
	}
	return envelope, nil
}

func (request CaptureCoordinateWindowRequestV1) Validate() error {
	if request.SchemaVersion != 1 || request.TopologyRef.TopologyID == "" ||
		request.TopologyRef.Generation == 0 || request.PID <= 0 || request.BundleID == "" || request.WindowID == 0 {
		return fmt.Errorf("capture_coordinate_window request identity and authority are required")
	}
	if err := validateCoordinateQuartzRect("expected_quartz_bounds", request.ExpectedQuartzBounds); err != nil {
		return err
	}
	return nil
}

type CaptureCoordinateWindowResultV1 struct {
	SchemaVersion      int                      `json:"schema_version"`
	Status             string                   `json:"status"`
	FailureCode        *string                  `json:"failure_code"`
	RetrySafe          bool                     `json:"retry_safe"`
	TopologyRef        *CoordinateTopologyRefV1 `json:"topology_ref"`
	HelperBootID       *string                  `json:"helper_boot_id"`
	PID                *int                     `json:"pid"`
	BundleID           *string                  `json:"bundle_id"`
	WindowID           *uint32                  `json:"window_id"`
	WindowQuartzBounds *CoordinateQuartzRectV1  `json:"window_quartz_bounds"`
	DisplayID          *uint32                  `json:"display_id"`
	BackingScaleFactor *float64                 `json:"backing_scale_factor"`
	MediaType          *string                  `json:"media_type"`
	WidthPX            *int                     `json:"width_px"`
	HeightPX           *int                     `json:"height_px"`
	ByteLength         *int                     `json:"byte_length"`
	SHA256             *string                  `json:"sha256"`
	ImageBase64        *string                  `json:"image_base64"`
	CapturedAt         *string                  `json:"captured_at"`
}

var captureCoordinateWindowResultWireShapeV1 = coordinateObjectWireShape(false, map[string]coordinateWireShape{
	"schema_version":       coordinateScalarWireShape(false),
	"status":               coordinateScalarWireShape(false),
	"failure_code":         coordinateScalarWireShape(true),
	"retry_safe":           coordinateScalarWireShape(false),
	"topology_ref":         coordinateNullableWireShape(coordinateTopologyRefWireShapeV1),
	"helper_boot_id":       coordinateScalarWireShape(true),
	"pid":                  coordinateScalarWireShape(true),
	"bundle_id":            coordinateScalarWireShape(true),
	"window_id":            coordinateScalarWireShape(true),
	"window_quartz_bounds": coordinateNullableWireShape(coordinateQuartzRectWireShapeV1),
	"display_id":           coordinateScalarWireShape(true),
	"backing_scale_factor": coordinateScalarWireShape(true),
	"media_type":           coordinateScalarWireShape(true),
	"width_px":             coordinateScalarWireShape(true),
	"height_px":            coordinateScalarWireShape(true),
	"byte_length":          coordinateScalarWireShape(true),
	"sha256":               coordinateScalarWireShape(true),
	"image_base64":         coordinateScalarWireShape(true),
	"captured_at":          coordinateScalarWireShape(true),
})

func DecodeCaptureCoordinateWindowResultV1(payload []byte) (CaptureCoordinateWindowResultV1, error) {
	if err := validateCoordinateWireShape("capture_coordinate_window result v1", payload, captureCoordinateWindowResultWireShapeV1); err != nil {
		return CaptureCoordinateWindowResultV1{}, err
	}
	var result CaptureCoordinateWindowResultV1
	if err := decodeStrictCoordinateJSON(payload, &result); err != nil {
		return CaptureCoordinateWindowResultV1{}, fmt.Errorf("decode capture_coordinate_window result v1: %w", err)
	}
	if err := result.ValidateTaggedUnion(); err != nil {
		return CaptureCoordinateWindowResultV1{}, err
	}
	return result, nil
}

func EncodeCaptureCoordinateWindowResultV1(result CaptureCoordinateWindowResultV1) ([]byte, error) {
	if err := result.ValidateTaggedUnion(); err != nil {
		return nil, err
	}
	return json.Marshal(result)
}

func (result CaptureCoordinateWindowResultV1) ValidateTaggedUnion() error {
	if result.SchemaVersion != 1 {
		return fmt.Errorf("unsupported capture_coordinate_window result schema_version %d", result.SchemaVersion)
	}
	successFields := []bool{
		result.TopologyRef != nil, result.HelperBootID != nil, result.PID != nil,
		result.BundleID != nil, result.WindowID != nil, result.WindowQuartzBounds != nil,
		result.DisplayID != nil, result.BackingScaleFactor != nil, result.MediaType != nil,
		result.WidthPX != nil, result.HeightPX != nil, result.ByteLength != nil,
		result.SHA256 != nil, result.ImageBase64 != nil, result.CapturedAt != nil,
	}
	switch result.Status {
	case "captured":
		if result.FailureCode != nil || result.RetrySafe {
			return fmt.Errorf("captured result cannot carry failure metadata")
		}
		for _, present := range successFields {
			if !present {
				return fmt.Errorf("captured result requires every success field")
			}
		}
		if result.TopologyRef.TopologyID == "" || result.TopologyRef.Generation == 0 ||
			*result.HelperBootID == "" || *result.PID <= 0 || *result.BundleID == "" ||
			*result.WindowID == 0 || *result.DisplayID == 0 ||
			!finiteCoordinate(*result.BackingScaleFactor) || *result.BackingScaleFactor <= 0 ||
			*result.MediaType != "image/png" || *result.WidthPX <= 0 || *result.HeightPX <= 0 ||
			*result.ByteLength <= 0 || !validLowerHexSHA256(*result.SHA256) || *result.ImageBase64 == "" {
			return fmt.Errorf("captured result contains invalid success values")
		}
		if err := validateCoordinateQuartzRect("window_quartz_bounds", *result.WindowQuartzBounds); err != nil {
			return err
		}
		if _, err := time.Parse(time.RFC3339Nano, *result.CapturedAt); err != nil {
			return fmt.Errorf("captured_at must be RFC3339: %w", err)
		}
	case "failed":
		if result.FailureCode == nil || *result.FailureCode == "" {
			return fmt.Errorf("failed result requires a known failure_code")
		}
		expectedRetrySafe, valid := captureCoordinateWindowFailurePolicy(*result.FailureCode)
		if !valid || result.RetrySafe != expectedRetrySafe {
			return fmt.Errorf("failed result failure_code/retry_safe policy mismatch")
		}
		for _, present := range successFields {
			if present {
				return fmt.Errorf("failed result cannot carry success fields")
			}
		}
	default:
		return fmt.Errorf("invalid capture_coordinate_window status %q", result.Status)
	}
	return nil
}

func captureCoordinateWindowFailurePolicy(code string) (retrySafe bool, valid bool) {
	switch code {
	case "topology_unavailable", "stale_topology", "window_not_found",
		"window_not_actionable", "window_bounds_mismatch", "display_not_actionable",
		"capture_timeout", "capture_failed", "topology_changed", "window_changed":
		return true, true
	case "invalid_request", "process_identity_mismatch", "window_identity_mismatch",
		"image_too_large", "invalid_png", "image_dimensions_mismatch", "response_too_large":
		return false, true
	default:
		return false, false
	}
}

type CaptureCoordinateWindowLimitsV1 struct {
	MaxRawBytes    int
	MaxNDJSONBytes int
	MaxPixels      int
}

func AdmitCaptureCoordinateWindowV1(
	payload []byte,
	request CaptureCoordinateWindowRequestV1,
	currentTopology DisplayTopologyV1,
	limits CaptureCoordinateWindowLimitsV1,
) (CaptureCoordinateWindowResultV1, error) {
	if err := request.Validate(); err != nil {
		return CaptureCoordinateWindowResultV1{}, err
	}
	if limits.MaxRawBytes <= 0 || limits.MaxNDJSONBytes <= 0 || limits.MaxPixels <= 0 {
		return CaptureCoordinateWindowResultV1{}, fmt.Errorf("capture_coordinate_window limits must be positive")
	}
	if len(payload) > limits.MaxNDJSONBytes {
		return CaptureCoordinateWindowResultV1{}, fmt.Errorf("capture_coordinate_window result exceeds NDJSON cap")
	}
	result, err := DecodeCaptureCoordinateWindowResultV1(payload)
	if err != nil {
		return CaptureCoordinateWindowResultV1{}, err
	}
	if result.Status == "failed" {
		return result, nil
	}
	if err := currentTopology.Validate(); err != nil {
		return CaptureCoordinateWindowResultV1{}, fmt.Errorf("current display topology: %w", err)
	}
	currentRef := CoordinateTopologyRefV1{
		TopologyID: currentTopology.TopologyID,
		Generation: currentTopology.Generation,
	}
	if request.TopologyRef != currentRef || *result.TopologyRef != currentRef {
		return CaptureCoordinateWindowResultV1{}, fmt.Errorf("capture topology authority mismatch")
	}
	if *result.HelperBootID != currentTopology.HelperBootID {
		return CaptureCoordinateWindowResultV1{}, fmt.Errorf("capture helper boot identity mismatch")
	}
	topologyCapturedAt, err := time.Parse(time.RFC3339Nano, currentTopology.CapturedAt)
	if err != nil {
		return CaptureCoordinateWindowResultV1{}, fmt.Errorf("parse current topology captured_at: %w", err)
	}
	captureCapturedAt, err := time.Parse(time.RFC3339Nano, *result.CapturedAt)
	if err != nil {
		return CaptureCoordinateWindowResultV1{}, fmt.Errorf("parse capture captured_at: %w", err)
	}
	if !captureCapturedAt.After(topologyCapturedAt) {
		return CaptureCoordinateWindowResultV1{}, fmt.Errorf("capture timestamp must be later than current topology")
	}
	if *result.PID != request.PID || *result.BundleID != request.BundleID || *result.WindowID != request.WindowID {
		return CaptureCoordinateWindowResultV1{}, fmt.Errorf("capture target identity mismatch")
	}
	if !captureCoordinateWindowRectsCorrelate(*result.WindowQuartzBounds, request.ExpectedQuartzBounds) {
		return CaptureCoordinateWindowResultV1{}, fmt.Errorf("capture window bounds mismatch")
	}

	var display *DisplayTopologyDisplayV1
	actionableContainingCount := 0
	for index := range currentTopology.Displays {
		candidate := &currentTopology.Displays[index]
		if candidate.IsActive && candidate.IsOnline && !candidate.IsAsleep &&
			candidate.MirrorMasterDisplayID == nil && candidate.RotationDegrees == 0 &&
			captureCoordinateWindowRectContains(candidate.QuartzBounds, *result.WindowQuartzBounds) {
			actionableContainingCount++
			if candidate.DisplayID == *result.DisplayID {
				display = candidate
			}
		}
	}
	if actionableContainingCount != 1 || display == nil {
		return CaptureCoordinateWindowResultV1{}, fmt.Errorf("capture display is not actionable")
	}
	if math.Abs(*result.BackingScaleFactor-display.BackingScaleFactor) > 0.000001 {
		return CaptureCoordinateWindowResultV1{}, fmt.Errorf("capture backing scale mismatch")
	}
	expectedWidth := result.WindowQuartzBounds.Width * display.BackingScaleFactor
	expectedHeight := result.WindowQuartzBounds.Height * display.BackingScaleFactor
	if !finiteCoordinate(expectedWidth) || !finiteCoordinate(expectedHeight) ||
		math.Abs(math.Round(expectedWidth)-expectedWidth) > 0.000001 ||
		math.Abs(math.Round(expectedHeight)-expectedHeight) > 0.000001 ||
		*result.WidthPX != int(math.Round(expectedWidth)) || *result.HeightPX != int(math.Round(expectedHeight)) {
		return CaptureCoordinateWindowResultV1{}, fmt.Errorf("capture image dimensions do not match Quartz bounds and backing scale")
	}
	if *result.WidthPX > limits.MaxPixels / *result.HeightPX {
		return CaptureCoordinateWindowResultV1{}, fmt.Errorf("capture image exceeds decoded pixel cap")
	}
	if captureCoordinateWindowBase64HasWhitespace(*result.ImageBase64) {
		return CaptureCoordinateWindowResultV1{}, fmt.Errorf("capture image_base64 exceeds raw byte cap or is not canonical")
	}
	decodedLength := base64.StdEncoding.DecodedLen(len(*result.ImageBase64))
	if strings.HasSuffix(*result.ImageBase64, "=") {
		decodedLength--
	}
	if strings.HasSuffix(*result.ImageBase64, "==") {
		decodedLength--
	}
	if decodedLength > limits.MaxRawBytes {
		return CaptureCoordinateWindowResultV1{}, fmt.Errorf("capture image_base64 exceeds raw byte cap")
	}
	raw, err := base64.StdEncoding.Strict().DecodeString(*result.ImageBase64)
	if err != nil {
		return CaptureCoordinateWindowResultV1{}, fmt.Errorf("capture image_base64: %w", err)
	}
	if len(raw) > limits.MaxRawBytes {
		return CaptureCoordinateWindowResultV1{}, fmt.Errorf("capture image exceeds raw byte cap")
	}
	if len(raw) != *result.ByteLength {
		return CaptureCoordinateWindowResultV1{}, fmt.Errorf("capture byte_length mismatch")
	}
	if captureWindowSHA256(raw) != *result.SHA256 {
		return CaptureCoordinateWindowResultV1{}, fmt.Errorf("capture sha256 mismatch")
	}
	if err := validateCaptureCoordinateWindowPNG(raw, *result.WidthPX, *result.HeightPX); err != nil {
		return CaptureCoordinateWindowResultV1{}, err
	}
	return result, nil
}

func captureCoordinateWindowBase64HasWhitespace(value string) bool {
	for _, character := range value {
		switch character {
		case ' ', '\t', '\r', '\n':
			return true
		}
	}
	return false
}

func captureCoordinateWindowRectsCorrelate(left, right CoordinateQuartzRectV1) bool {
	return math.Abs(left.X-right.X) <= 2 && math.Abs(left.Y-right.Y) <= 2 &&
		math.Abs(left.Width-right.Width) <= 2 && math.Abs(left.Height-right.Height) <= 2
}

func captureCoordinateWindowRectContains(outer DisplayTopologyRectV1, inner CoordinateQuartzRectV1) bool {
	return inner.X >= outer.X && inner.Y >= outer.Y &&
		inner.X+inner.Width <= outer.X+outer.Width &&
		inner.Y+inner.Height <= outer.Y+outer.Height
}

func captureWindowSHA256(raw []byte) string {
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:])
}

func validateCaptureCoordinateWindowPNG(raw []byte, expectedWidth, expectedHeight int) error {
	if len(raw) < 24 || !bytes.Equal(raw[:8], []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a}) {
		return fmt.Errorf("capture is not a PNG")
	}
	if err := validateCaptureCoordinateWindowPNGChunks(raw); err != nil {
		return err
	}
	config, err := png.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("decode capture PNG header: %w", err)
	}
	if config.Width != expectedWidth || config.Height != expectedHeight {
		return fmt.Errorf("capture PNG header dimensions mismatch")
	}
	image, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("fully decode capture PNG: %w", err)
	}
	if image.Bounds().Dx() != expectedWidth || image.Bounds().Dy() != expectedHeight {
		return fmt.Errorf("capture decoded PNG dimensions mismatch")
	}
	return nil
}

func validateCaptureCoordinateWindowPNGChunks(raw []byte) error {
	offset := 8
	chunkIndex := 0
	sawIDAT := false
	sawIEND := false
	for offset < len(raw) {
		if len(raw)-offset < 12 {
			return fmt.Errorf("truncated capture PNG chunk")
		}
		length := int(binary.BigEndian.Uint32(raw[offset : offset+4]))
		if length < 0 || length > len(raw)-offset-12 {
			return fmt.Errorf("invalid capture PNG chunk length")
		}
		typeStart := offset + 4
		dataEnd := typeStart + 4 + length
		chunkType := string(raw[typeStart : typeStart+4])
		expectedCRC := binary.BigEndian.Uint32(raw[dataEnd : dataEnd+4])
		if crc32.ChecksumIEEE(raw[typeStart:dataEnd]) != expectedCRC {
			return fmt.Errorf("capture PNG chunk CRC mismatch")
		}
		if chunkIndex == 0 && (chunkType != "IHDR" || length != 13) {
			return fmt.Errorf("capture PNG must begin with IHDR")
		}
		if chunkType == "IDAT" {
			sawIDAT = true
		}
		if chunkType == "IEND" {
			if length != 0 || dataEnd+4 != len(raw) {
				return fmt.Errorf("capture PNG has invalid IEND or trailing data")
			}
			sawIEND = true
			break
		}
		offset = dataEnd + 4
		chunkIndex++
	}
	if !sawIDAT || !sawIEND {
		return fmt.Errorf("capture PNG is missing IDAT or IEND")
	}
	return nil
}

func ReadCaptureCoordinateWindowV1(
	ctx context.Context,
	caller displayTopologyRPCCaller,
	request CaptureCoordinateWindowRequestV1,
	currentTopology DisplayTopologyV1,
	limits CaptureCoordinateWindowLimitsV1,
) (CaptureCoordinateWindowResultV1, error) {
	if err := request.Validate(); err != nil {
		return CaptureCoordinateWindowResultV1{}, err
	}
	payload, err := caller.Call(ctx, "capture_coordinate_window", request)
	if err != nil {
		return CaptureCoordinateWindowResultV1{}, fmt.Errorf("capture_coordinate_window RPC: %w", err)
	}
	return AdmitCaptureCoordinateWindowV1(payload, request, currentTopology, limits)
}

func (client *AXClient) CaptureCoordinateWindowV1(
	ctx context.Context,
	request CaptureCoordinateWindowRequestV1,
	currentTopology DisplayTopologyV1,
	limits CaptureCoordinateWindowLimitsV1,
) (CaptureCoordinateWindowResultV1, error) {
	return ReadCaptureCoordinateWindowV1(ctx, client, request, currentTopology, limits)
}
