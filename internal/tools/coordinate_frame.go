package tools

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"
)

const coordinatePixelOriginV1 = "top_left_pixel_centers"

// CoordinateMaxRawImageBytesV1 is the maximum encoded-image byte length a
// v1 CoordinateFrame may declare before base64 transport. It intentionally
// follows the established image pipeline target so its 4/3 expansion remains
// below client.MaxInlineImageBase64Bytes without changing generic compression.
const CoordinateMaxRawImageBytesV1 = TargetRawImageBytes

type CoordinateTopologyRefV1 struct {
	TopologyID string `json:"topology_id"`
	Generation uint64 `json:"generation"`
}

type CoordinateQuartzRectV1 struct {
	X      float64 `json:"x"`
	Y      float64 `json:"y"`
	Width  float64 `json:"width"`
	Height float64 `json:"height"`
}

type CoordinatePixelRectV1 struct {
	X      int `json:"x"`
	Y      int `json:"y"`
	Width  int `json:"width"`
	Height int `json:"height"`
}

type CoordinateRawImageV1 struct {
	WidthPX  int `json:"width_px"`
	HeightPX int `json:"height_px"`
}

type CoordinateFinalImageV1 struct {
	MediaType  string `json:"media_type"`
	WidthPX    int    `json:"width_px"`
	HeightPX   int    `json:"height_px"`
	ByteLength int    `json:"byte_length"`
	SHA256     string `json:"sha256"`
}

type CoordinateProviderProfileRefV1 struct {
	ID      string `json:"id"`
	Version int    `json:"version"`
}

type CoordinateAffineV1 struct {
	A  float64 `json:"a"`
	B  float64 `json:"b"`
	C  float64 `json:"c"`
	D  float64 `json:"d"`
	TX float64 `json:"tx"`
	TY float64 `json:"ty"`
}

type CoordinateTransformRegionV1 struct {
	DisplayID  uint32                 `json:"display_id"`
	PixelRect  CoordinatePixelRectV1  `json:"pixel_rect"`
	QuartzRect CoordinateQuartzRectV1 `json:"quartz_rect"`
	Affine     CoordinateAffineV1     `json:"affine"`
}

type CoordinateFrameV1 struct {
	SchemaVersion      int                            `json:"schema_version"`
	FrameID            string                         `json:"frame_id"`
	TopologyRef        CoordinateTopologyRefV1        `json:"topology_ref"`
	StateID            string                         `json:"state_id"`
	Scope              string                         `json:"scope"`
	Actionable         bool                           `json:"actionable"`
	DisplayID          *uint32                        `json:"display_id"`
	TargetPID          *int                           `json:"target_pid"`
	TargetBundleID     *string                        `json:"target_bundle_id"`
	TargetWindowID     *int                           `json:"target_window_id"`
	CapturedQuartzRect CoordinateQuartzRectV1         `json:"captured_quartz_rect"`
	RawImage           CoordinateRawImageV1           `json:"raw_image"`
	FinalImage         CoordinateFinalImageV1         `json:"final_image"`
	ProviderProfile    CoordinateProviderProfileRefV1 `json:"provider_profile"`
	PixelOrigin        string                         `json:"pixel_origin"`
	TransformRegions   []CoordinateTransformRegionV1  `json:"transform_regions"`
	CreatedAt          string                         `json:"created_at"`
	ExpiresAt          string                         `json:"expires_at"`
}

type CoordinateImageProfileV1 struct {
	SchemaVersion                     int    `json:"schema_version"`
	ID                                string `json:"id"`
	Version                           int    `json:"version"`
	MediaType                         string `json:"media_type"`
	FallbackMediaType                 string `json:"fallback_media_type"`
	TargetLongEdgePX                  int    `json:"target_long_edge_px"`
	MaxLongEdgePX                     int    `json:"max_long_edge_px"`
	MaxTotalPixels                    int    `json:"max_total_pixels"`
	MaxEncodedBytes                   int    `json:"max_encoded_bytes"`
	JPEGQualityLadder                 []int  `json:"jpeg_quality_ladder"`
	PaddingMode                       string `json:"padding_mode"`
	RequiresExactCoordinateDimensions bool   `json:"requires_exact_coordinate_dimensions"`
}

var coordinateImageProfileWireShapeV1 = coordinateObjectWireShape(false, map[string]coordinateWireShape{
	"schema_version":                       coordinateScalarWireShape(false),
	"id":                                   coordinateScalarWireShape(false),
	"version":                              coordinateScalarWireShape(false),
	"media_type":                           coordinateScalarWireShape(false),
	"fallback_media_type":                  coordinateScalarWireShape(false),
	"target_long_edge_px":                  coordinateScalarWireShape(false),
	"max_long_edge_px":                     coordinateScalarWireShape(false),
	"max_total_pixels":                     coordinateScalarWireShape(false),
	"max_encoded_bytes":                    coordinateScalarWireShape(false),
	"jpeg_quality_ladder":                  coordinateArrayWireShape(coordinateScalarWireShape(false)),
	"padding_mode":                         coordinateScalarWireShape(false),
	"requires_exact_coordinate_dimensions": coordinateScalarWireShape(false),
})

var coordinateTransformRegionWireShapeV1 = coordinateObjectWireShape(false, map[string]coordinateWireShape{
	"display_id":  coordinateScalarWireShape(false),
	"pixel_rect":  coordinatePixelRectWireShapeV1,
	"quartz_rect": coordinateQuartzRectWireShapeV1,
	"affine":      coordinateAffineWireShapeV1,
})

var coordinateFrameWireShapeV1 = coordinateObjectWireShape(false, map[string]coordinateWireShape{
	"schema_version":       coordinateScalarWireShape(false),
	"frame_id":             coordinateScalarWireShape(false),
	"topology_ref":         coordinateTopologyRefWireShapeV1,
	"state_id":             coordinateScalarWireShape(false),
	"scope":                coordinateScalarWireShape(false),
	"actionable":           coordinateScalarWireShape(false),
	"display_id":           coordinateScalarWireShape(true),
	"target_pid":           coordinateScalarWireShape(true),
	"target_bundle_id":     coordinateScalarWireShape(true),
	"target_window_id":     coordinateScalarWireShape(true),
	"captured_quartz_rect": coordinateQuartzRectWireShapeV1,
	"raw_image": coordinateObjectWireShape(false, map[string]coordinateWireShape{
		"width_px":  coordinateScalarWireShape(false),
		"height_px": coordinateScalarWireShape(false),
	}),
	"final_image": coordinateObjectWireShape(false, map[string]coordinateWireShape{
		"media_type":  coordinateScalarWireShape(false),
		"width_px":    coordinateScalarWireShape(false),
		"height_px":   coordinateScalarWireShape(false),
		"byte_length": coordinateScalarWireShape(false),
		"sha256":      coordinateScalarWireShape(false),
	}),
	"provider_profile": coordinateObjectWireShape(false, map[string]coordinateWireShape{
		"id":      coordinateScalarWireShape(false),
		"version": coordinateScalarWireShape(false),
	}),
	"pixel_origin":      coordinateScalarWireShape(false),
	"transform_regions": coordinateArrayWireShape(coordinateTransformRegionWireShapeV1),
	"created_at":        coordinateScalarWireShape(false),
	"expires_at":        coordinateScalarWireShape(false),
})

func DecodeCoordinateImageProfileV1(payload []byte) (CoordinateImageProfileV1, error) {
	if err := validateCoordinateWireShape("coordinate image profile v1", payload, coordinateImageProfileWireShapeV1); err != nil {
		return CoordinateImageProfileV1{}, err
	}
	var profile CoordinateImageProfileV1
	if err := decodeStrictCoordinateJSON(payload, &profile); err != nil {
		return CoordinateImageProfileV1{}, fmt.Errorf("decode coordinate image profile v1: %w", err)
	}
	if err := profile.Validate(); err != nil {
		return CoordinateImageProfileV1{}, err
	}
	return profile, nil
}

func EncodeCoordinateImageProfileV1(profile CoordinateImageProfileV1) ([]byte, error) {
	if err := profile.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(profile)
}

func (profile CoordinateImageProfileV1) Validate() error {
	if profile.SchemaVersion != 1 {
		return fmt.Errorf("unsupported coordinate image profile schema_version %d", profile.SchemaVersion)
	}
	if profile.ID == "" || profile.Version <= 0 {
		return fmt.Errorf("coordinate image profile identity is required")
	}
	if profile.MediaType != "image/png" {
		return fmt.Errorf("coordinate image profile media_type must be image/png")
	}
	if profile.FallbackMediaType != "image/jpeg" {
		return fmt.Errorf("coordinate image profile fallback_media_type must be image/jpeg")
	}
	if profile.TargetLongEdgePX <= 0 || profile.MaxLongEdgePX <= 0 || profile.TargetLongEdgePX > profile.MaxLongEdgePX {
		return fmt.Errorf("coordinate image profile long-edge limits are invalid")
	}
	if profile.MaxTotalPixels <= 0 || profile.MaxEncodedBytes <= 0 {
		return fmt.Errorf("coordinate image profile size limits must be positive")
	}
	if profile.MaxEncodedBytes > CoordinateMaxRawImageBytesV1 {
		return fmt.Errorf(
			"coordinate image profile max_encoded_bytes exceeds v1 raw-byte safety cap %d",
			CoordinateMaxRawImageBytesV1)
	}
	if len(profile.JPEGQualityLadder) == 0 {
		return fmt.Errorf("coordinate image profile jpeg_quality_ladder is required")
	}
	previous := 101
	for _, quality := range profile.JPEGQualityLadder {
		if quality <= 0 || quality > 100 || quality >= previous {
			return fmt.Errorf("coordinate image profile jpeg_quality_ladder must be strictly descending in [1, 100]")
		}
		previous = quality
	}
	if profile.PaddingMode != "none" && profile.PaddingMode != "letterbox" {
		return fmt.Errorf("coordinate image profile padding_mode must be none or letterbox")
	}
	if !profile.RequiresExactCoordinateDimensions {
		return fmt.Errorf("coordinate image profile must require exact coordinate dimensions")
	}
	return nil
}

func DecodeCoordinateFrameV1(payload []byte) (CoordinateFrameV1, error) {
	if err := validateCoordinateWireShape("coordinate frame v1", payload, coordinateFrameWireShapeV1); err != nil {
		return CoordinateFrameV1{}, err
	}
	var frame CoordinateFrameV1
	if err := decodeStrictCoordinateJSON(payload, &frame); err != nil {
		return CoordinateFrameV1{}, fmt.Errorf("decode coordinate frame v1: %w", err)
	}
	if err := frame.Validate(); err != nil {
		return CoordinateFrameV1{}, err
	}
	return frame, nil
}

// AdmitCoordinateFrameV1 is the only decoder intended for a production action
// path. Structural decoding alone is insufficient because a valid frame can
// still exceed the selected provider's declared image limits.
func AdmitCoordinateFrameV1(payload []byte, profile CoordinateImageProfileV1) (CoordinateFrameV1, error) {
	frame, err := DecodeCoordinateFrameV1(payload)
	if err != nil {
		return CoordinateFrameV1{}, err
	}
	if err := frame.ValidateAgainst(profile); err != nil {
		return CoordinateFrameV1{}, fmt.Errorf("admit coordinate frame v1: %w", err)
	}
	return frame, nil
}

func EncodeCoordinateFrameV1(frame CoordinateFrameV1) ([]byte, error) {
	if err := frame.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(frame)
}

func (frame CoordinateFrameV1) Validate() error {
	if frame.SchemaVersion != 1 {
		return fmt.Errorf("unsupported coordinate frame schema_version %d", frame.SchemaVersion)
	}
	if frame.FrameID == "" || frame.StateID == "" || frame.TopologyRef.TopologyID == "" || frame.TopologyRef.Generation == 0 {
		return fmt.Errorf("coordinate frame identity and authority are required")
	}
	switch frame.Scope {
	case "window":
		if frame.DisplayID == nil || *frame.DisplayID == 0 || frame.TargetPID == nil || *frame.TargetPID <= 0 ||
			frame.TargetBundleID == nil || *frame.TargetBundleID == "" ||
			frame.TargetWindowID == nil || *frame.TargetWindowID <= 0 {
			return fmt.Errorf("window coordinate frame target identity is required")
		}
	case "display":
		if frame.DisplayID == nil || *frame.DisplayID == 0 || frame.TargetPID != nil || frame.TargetBundleID != nil || frame.TargetWindowID != nil {
			return fmt.Errorf("display coordinate frame must identify only its display")
		}
	case "desktop":
		if frame.DisplayID != nil || frame.TargetPID != nil || frame.TargetBundleID != nil || frame.TargetWindowID != nil {
			return fmt.Errorf("desktop coordinate frame cannot carry a singular target")
		}
		if frame.Actionable {
			return fmt.Errorf("desktop coordinate frame is overview-only and cannot be actionable")
		}
	default:
		return fmt.Errorf("invalid coordinate frame scope %q", frame.Scope)
	}
	if err := validateCoordinateQuartzRect("captured_quartz_rect", frame.CapturedQuartzRect); err != nil {
		return err
	}
	if frame.RawImage.WidthPX <= 0 || frame.RawImage.HeightPX <= 0 ||
		frame.FinalImage.WidthPX <= 0 || frame.FinalImage.HeightPX <= 0 || frame.FinalImage.ByteLength <= 0 {
		return fmt.Errorf("coordinate frame image dimensions and byte_length must be positive")
	}
	if frame.FinalImage.MediaType != "image/png" && frame.FinalImage.MediaType != "image/jpeg" {
		return fmt.Errorf("coordinate frame final image media_type is unsupported")
	}
	if !validLowerHexSHA256(frame.FinalImage.SHA256) {
		return fmt.Errorf("coordinate frame final image sha256 must be 64 lowercase hex characters")
	}
	if frame.ProviderProfile.ID == "" || frame.ProviderProfile.Version <= 0 {
		return fmt.Errorf("coordinate frame provider profile identity is required")
	}
	if frame.PixelOrigin != coordinatePixelOriginV1 {
		return fmt.Errorf("coordinate frame pixel_origin must be %q", coordinatePixelOriginV1)
	}
	if len(frame.TransformRegions) == 0 {
		return fmt.Errorf("coordinate frame transform_regions are required")
	}
	for index, region := range frame.TransformRegions {
		if err := validateCoordinateTransformRegion(frame, region); err != nil {
			return fmt.Errorf("coordinate frame transform region %d: %w", index, err)
		}
	}
	if frame.Actionable {
		if len(frame.TransformRegions) != 1 {
			return fmt.Errorf("actionable coordinate frame must have exactly one transform region")
		}
		region := frame.TransformRegions[0]
		if frame.DisplayID == nil || region.DisplayID != *frame.DisplayID {
			return fmt.Errorf("actionable coordinate frame transform must match display_id")
		}
		if err := validateActionableCoordinateAffine(region); err != nil {
			return err
		}
	}
	created, err := time.Parse(time.RFC3339Nano, frame.CreatedAt)
	if err != nil {
		return fmt.Errorf("coordinate frame created_at must be RFC3339: %w", err)
	}
	expires, err := time.Parse(time.RFC3339Nano, frame.ExpiresAt)
	if err != nil {
		return fmt.Errorf("coordinate frame expires_at must be RFC3339: %w", err)
	}
	if !created.Before(expires) {
		return fmt.Errorf("coordinate frame created_at must precede expires_at")
	}
	return nil
}

func (frame CoordinateFrameV1) ValidateAgainst(profile CoordinateImageProfileV1) error {
	if err := frame.Validate(); err != nil {
		return err
	}
	if err := profile.Validate(); err != nil {
		return err
	}
	if frame.ProviderProfile.ID != profile.ID || frame.ProviderProfile.Version != profile.Version {
		return fmt.Errorf("coordinate frame provider profile identity does not match")
	}
	if frame.FinalImage.MediaType != profile.MediaType && frame.FinalImage.MediaType != profile.FallbackMediaType {
		return fmt.Errorf("coordinate frame media_type is not declared by provider profile")
	}
	if frame.FinalImage.WidthPX > profile.MaxLongEdgePX || frame.FinalImage.HeightPX > profile.MaxLongEdgePX {
		return fmt.Errorf("coordinate frame exceeds provider profile max_long_edge_px")
	}
	if int64(frame.FinalImage.WidthPX)*int64(frame.FinalImage.HeightPX) > int64(profile.MaxTotalPixels) {
		return fmt.Errorf("coordinate frame exceeds provider profile max_total_pixels")
	}
	if frame.FinalImage.ByteLength > profile.MaxEncodedBytes {
		return fmt.Errorf("coordinate frame exceeds provider profile max_encoded_bytes")
	}
	if frame.Actionable {
		region := frame.TransformRegions[0]
		horizontalPadding := region.PixelRect.X > 0 ||
			region.PixelRect.Width < frame.FinalImage.WidthPX
		verticalPadding := region.PixelRect.Y > 0 ||
			region.PixelRect.Height < frame.FinalImage.HeightPX
		switch profile.PaddingMode {
		case "none":
			if horizontalPadding || verticalPadding {
				return fmt.Errorf("coordinate frame declares padding under a no-padding profile")
			}
		case "letterbox":
			left := region.PixelRect.X
			right := frame.FinalImage.WidthPX - region.PixelRect.X - region.PixelRect.Width
			top := region.PixelRect.Y
			bottom := frame.FinalImage.HeightPX - region.PixelRect.Y - region.PixelRect.Height
			if horizontalPadding && verticalPadding {
				return fmt.Errorf("letterbox content must fill one canvas axis")
			}
			if left-right < -1 || left-right > 1 || top-bottom < -1 || top-bottom > 1 {
				return fmt.Errorf("letterbox coordinate frame padding must be centered")
			}
		}
	}
	return nil
}

func validateCoordinateTransformRegion(frame CoordinateFrameV1, region CoordinateTransformRegionV1) error {
	if region.DisplayID == 0 {
		return fmt.Errorf("display_id must not be zero")
	}
	rect := region.PixelRect
	if rect.X < 0 || rect.Y < 0 || rect.Width <= 0 || rect.Height <= 0 ||
		rect.X > frame.FinalImage.WidthPX-rect.Width || rect.Y > frame.FinalImage.HeightPX-rect.Height {
		return fmt.Errorf("pixel_rect must be inside final_image")
	}
	if err := validateCoordinateQuartzRect("quartz_rect", region.QuartzRect); err != nil {
		return err
	}
	if !coordinateQuartzRectContainsRect(frame.CapturedQuartzRect, region.QuartzRect) {
		return fmt.Errorf("quartz_rect must be inside captured_quartz_rect")
	}
	affine := region.Affine
	if !finiteCoordinate(affine.A) || !finiteCoordinate(affine.B) || !finiteCoordinate(affine.C) ||
		!finiteCoordinate(affine.D) || !finiteCoordinate(affine.TX) || !finiteCoordinate(affine.TY) ||
		math.Abs(affine.A*affine.D-affine.B*affine.C) < 1e-15 {
		return fmt.Errorf("affine must be finite and invertible")
	}
	for _, pixel := range [][2]float64{
		{float64(rect.X) + 0.5, float64(rect.Y) + 0.5},
		{float64(rect.X+rect.Width) - 0.5, float64(rect.Y) + 0.5},
		{float64(rect.X) + 0.5, float64(rect.Y+rect.Height) - 0.5},
		{float64(rect.X+rect.Width) - 0.5, float64(rect.Y+rect.Height) - 0.5},
	} {
		point := applyCoordinateAffine(affine, pixel[0], pixel[1])
		if !coordinateQuartzRectContainsPoint(region.QuartzRect, point.X, point.Y) {
			return fmt.Errorf("affine maps pixel centers outside quartz_rect")
		}
	}
	return nil
}

const actionableAffineToleranceV1 = 0.000000001

func validateActionableCoordinateAffine(region CoordinateTransformRegionV1) error {
	pixel := region.PixelRect
	quartz := region.QuartzRect
	expectedA := quartz.Width / float64(pixel.Width)
	expectedD := quartz.Height / float64(pixel.Height)
	expectedTX := quartz.X - expectedA*float64(pixel.X)
	expectedTY := quartz.Y - expectedD*float64(pixel.Y)
	affine := region.Affine
	if math.Abs(affine.B) > actionableAffineToleranceV1 || math.Abs(affine.C) > actionableAffineToleranceV1 ||
		math.Abs(affine.A-expectedA) > actionableAffineToleranceV1 ||
		math.Abs(affine.D-expectedD) > actionableAffineToleranceV1 ||
		math.Abs(affine.TX-expectedTX) > actionableAffineToleranceV1 ||
		math.Abs(affine.TY-expectedTY) > actionableAffineToleranceV1 {
		return fmt.Errorf("actionable coordinate frame affine does not exactly bind pixel_rect to quartz_rect")
	}
	return nil
}

func validateCoordinateQuartzRect(field string, rect CoordinateQuartzRectV1) error {
	if !finiteCoordinate(rect.X) || !finiteCoordinate(rect.Y) ||
		!finiteCoordinate(rect.Width) || !finiteCoordinate(rect.Height) || rect.Width <= 0 || rect.Height <= 0 {
		return fmt.Errorf("%s must be finite with positive size", field)
	}
	return nil
}

func coordinateQuartzRectContainsRect(outer, inner CoordinateQuartzRectV1) bool {
	return inner.X >= outer.X && inner.Y >= outer.Y &&
		inner.X+inner.Width <= outer.X+outer.Width && inner.Y+inner.Height <= outer.Y+outer.Height
}

func coordinateQuartzRectContainsPoint(rect CoordinateQuartzRectV1, x, y float64) bool {
	return finiteCoordinate(x) && finiteCoordinate(y) &&
		x >= rect.X && y >= rect.Y && x < rect.X+rect.Width && y < rect.Y+rect.Height
}

func validLowerHexSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, character := range value {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

type CoordinateMappedPointV1 struct {
	DisplayID uint32
	X         float64
	Y         float64
}

type CoordinateMapErrorV1 struct {
	Code    string
	Message string
}

func (err *CoordinateMapErrorV1) Error() string {
	return err.Code + ": " + err.Message
}

func CoordinateMapErrorCodeV1(err error) string {
	var mapError *CoordinateMapErrorV1
	if errors.As(err, &mapError) {
		return mapError.Code
	}
	return ""
}

func coordinateMapError(code, message string) error {
	return &CoordinateMapErrorV1{Code: code, Message: message}
}

// Region selection uses only the independently implementable idea of testing
// a display-local pixel center against display intersections. Provenance:
// Peekaboo v3.9.7, commit 0cd07acfdb50415aee3476471bad3e79d517dc9f.
// No Peekaboo source code is copied here.
func MapCoordinatePixelCenterV1(
	frame CoordinateFrameV1,
	currentTopology CoordinateTopologyRefV1,
	currentStateID string,
	frameID string,
	now time.Time,
	x float64,
	y float64,
) (CoordinateMappedPointV1, error) {
	if err := frame.Validate(); err != nil {
		return CoordinateMappedPointV1{}, coordinateMapError("invalid_frame", err.Error())
	}
	if currentTopology.TopologyID == "" || currentTopology.Generation == 0 || currentTopology != frame.TopologyRef {
		return CoordinateMappedPointV1{}, coordinateMapError("stale_topology", "current topology does not match frame authority")
	}
	if currentStateID == "" || currentStateID != frame.StateID {
		return CoordinateMappedPointV1{}, coordinateMapError("stale_state", "current state does not match frame state")
	}
	if frameID == "" || frameID != frame.FrameID {
		return CoordinateMappedPointV1{}, coordinateMapError("stale_frame", "requested frame_id does not match")
	}
	if !frame.Actionable {
		return CoordinateMappedPointV1{}, coordinateMapError("frame_not_actionable", "frame is observation-only")
	}
	created, _ := time.Parse(time.RFC3339Nano, frame.CreatedAt)
	if !now.IsZero() && now.Before(created) {
		return CoordinateMappedPointV1{}, coordinateMapError("frame_not_yet_valid", "frame creation time has not been reached")
	}
	expires, _ := time.Parse(time.RFC3339Nano, frame.ExpiresAt)
	if now.IsZero() || !now.Before(expires) {
		return CoordinateMappedPointV1{}, coordinateMapError("frame_expired", "frame TTL has expired")
	}
	if !finiteCoordinate(x) || !finiteCoordinate(y) || math.Trunc(x) != x || math.Trunc(y) != y {
		return CoordinateMappedPointV1{}, coordinateMapError("invalid_coordinate", "coordinate must contain finite integer pixel indices")
	}
	if x < 0 || y < 0 ||
		x >= float64(frame.FinalImage.WidthPX) || y >= float64(frame.FinalImage.HeightPX) {
		return CoordinateMappedPointV1{}, coordinateMapError("outside_final_image", "coordinate is outside final_image")
	}

	pixelCenterX := x + 0.5
	pixelCenterY := y + 0.5
	matches := make([]CoordinateTransformRegionV1, 0, 1)
	for _, region := range frame.TransformRegions {
		if coordinatePixelRectContainsPoint(region.PixelRect, pixelCenterX, pixelCenterY) {
			matches = append(matches, region)
		}
	}
	if len(matches) == 0 {
		return CoordinateMappedPointV1{}, coordinateMapError("coordinate_gap", "coordinate does not belong to a display transform region")
	}
	if len(matches) != 1 {
		return CoordinateMappedPointV1{}, coordinateMapError("coordinate_overlap", "coordinate belongs to multiple display transform regions")
	}
	region := matches[0]
	mapped := applyCoordinateAffine(region.Affine, pixelCenterX, pixelCenterY)
	if !coordinateQuartzRectContainsPoint(region.QuartzRect, mapped.X, mapped.Y) {
		return CoordinateMappedPointV1{}, coordinateMapError("mapped_outside_region", "affine result is outside its quartz_rect")
	}
	return CoordinateMappedPointV1{DisplayID: region.DisplayID, X: mapped.X, Y: mapped.Y}, nil
}

func coordinatePixelRectContainsPoint(rect CoordinatePixelRectV1, x, y float64) bool {
	return x >= float64(rect.X) && y >= float64(rect.Y) &&
		x < float64(rect.X+rect.Width) && y < float64(rect.Y+rect.Height)
}

func applyCoordinateAffine(affine CoordinateAffineV1, x, y float64) CoordinateMappedPointV1 {
	return CoordinateMappedPointV1{
		X: affine.A*x + affine.C*y + affine.TX,
		Y: affine.B*x + affine.D*y + affine.TY,
	}
}
