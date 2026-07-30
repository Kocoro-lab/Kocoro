package tools

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image/png"
	"strings"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

// CoordinateDisplayFinalizeInputV1 carries the complete authority needed to
// convert one exact-display helper payload into an actionable image/frame pair.
// The helper payload is deliberately re-admitted inside the finalizer.
type CoordinateDisplayFinalizeInputV1 struct {
	CapturePayload  []byte
	CaptureRequest  CaptureCoordinateDisplayRequestV1
	CurrentTopology DisplayTopologyV1
	CaptureLimits   CaptureCoordinateDisplayLimitsV1
	StateID         string
	Profile         CoordinateImageProfileV1
	Now             time.Time
	TTL             time.Duration
	FrameID         string
}

// CoordinateDisplayArtifactV1 owns the only frame authorized for its private
// final bytes. Accessors return copies so the association stays immutable.
type CoordinateDisplayArtifactV1 struct {
	imageBytes []byte
	mediaType  string
	frame      CoordinateFrameV1
}

func (artifact CoordinateDisplayArtifactV1) ImageBytes() []byte {
	return append([]byte(nil), artifact.imageBytes...)
}

func (artifact CoordinateDisplayArtifactV1) ImageBlock() agent.ImageBlock {
	return agent.ImageBlock{
		MediaType: artifact.mediaType,
		Data:      base64.StdEncoding.EncodeToString(artifact.imageBytes),
	}
}

func (artifact CoordinateDisplayArtifactV1) Frame() CoordinateFrameV1 {
	return cloneCoordinateFrameV1(artifact.frame)
}

// FinalizeCoordinateDisplayV1 admits a raw exact-display capture and
// atomically emits its provider-sized bytes and immutable CoordinateFrame. It
// never calls generic screenshot compression and cannot create a desktop
// mosaic: one artifact contains exactly one display and one transform region.
func FinalizeCoordinateDisplayV1(
	input CoordinateDisplayFinalizeInputV1,
) (CoordinateDisplayArtifactV1, error) {
	if strings.TrimSpace(input.StateID) == "" || input.StateID != strings.TrimSpace(input.StateID) ||
		strings.TrimSpace(input.FrameID) == "" || input.FrameID != strings.TrimSpace(input.FrameID) {
		return CoordinateDisplayArtifactV1{}, fmt.Errorf("coordinate display finalizer requires nonempty state_id and frame_id")
	}
	if input.Now.IsZero() {
		return CoordinateDisplayArtifactV1{}, fmt.Errorf("coordinate display finalizer requires trusted now")
	}
	if input.TTL <= 0 || input.TTL > CoordinateFrameMaxTTLV1 {
		return CoordinateDisplayArtifactV1{}, fmt.Errorf("coordinate display frame TTL must be in (0, %s]", CoordinateFrameMaxTTLV1)
	}
	if err := input.Profile.Validate(); err != nil {
		return CoordinateDisplayArtifactV1{}, fmt.Errorf("coordinate display finalizer profile: %w", err)
	}

	capture, err := AdmitCaptureCoordinateDisplayV1(
		input.CapturePayload,
		input.CaptureRequest,
		input.CurrentTopology,
		input.CaptureLimits)
	if err != nil {
		return CoordinateDisplayArtifactV1{}, fmt.Errorf("coordinate display finalizer capture admission: %w", err)
	}
	if capture.Status != "captured" {
		return CoordinateDisplayArtifactV1{}, fmt.Errorf("coordinate display finalizer requires a captured result")
	}

	capturedAt, err := time.Parse(time.RFC3339Nano, *capture.CapturedAt)
	if err != nil {
		return CoordinateDisplayArtifactV1{}, fmt.Errorf("coordinate display finalizer captured_at: %w", err)
	}
	if capturedAt.After(input.Now.Add(CoordinateCaptureMaxFutureSkewV1)) {
		return CoordinateDisplayArtifactV1{}, fmt.Errorf("coordinate display capture exceeds future-skew tolerance")
	}
	if input.Now.Sub(capturedAt) > CoordinateCaptureMaxAgeV1 {
		return CoordinateDisplayArtifactV1{}, fmt.Errorf("coordinate display capture is stale")
	}
	expiresAt := capturedAt.Add(input.TTL)
	if !input.Now.Before(expiresAt) {
		return CoordinateDisplayArtifactV1{}, fmt.Errorf("coordinate display frame would already be expired")
	}

	raw, err := base64.StdEncoding.Strict().DecodeString(*capture.ImageBase64)
	if err != nil || base64.StdEncoding.EncodeToString(raw) != *capture.ImageBase64 {
		return CoordinateDisplayArtifactV1{}, fmt.Errorf("coordinate display capture image_base64 is not canonical")
	}
	if len(raw) != *capture.ByteLength || captureWindowSHA256(raw) != *capture.SHA256 {
		return CoordinateDisplayArtifactV1{}, fmt.Errorf("coordinate display capture bytes do not match admitted metadata")
	}
	if err := validateCaptureCoordinateWindowPNG(raw, *capture.WidthPX, *capture.HeightPX); err != nil {
		return CoordinateDisplayArtifactV1{}, fmt.Errorf("coordinate display capture PNG revalidation: %w", err)
	}
	source, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		return CoordinateDisplayArtifactV1{}, fmt.Errorf("coordinate display finalizer decode PNG: %w", err)
	}

	finalWidth, finalHeight := coordinateFinalDimensionsV1(
		*capture.WidthPX, *capture.HeightPX, input.Profile)
	finalImage := source
	resized := finalWidth != *capture.WidthPX || finalHeight != *capture.HeightPX
	if resized {
		finalImage = coordinateScaleImageV1(source, finalWidth, finalHeight)
	}
	finalBytes := raw
	mediaType := "image/png"
	if resized || len(finalBytes) > input.Profile.MaxEncodedBytes {
		finalBytes, err = coordinateEncodePNGV1(finalImage)
		if err != nil {
			return CoordinateDisplayArtifactV1{}, err
		}
	}
	if len(finalBytes) > input.Profile.MaxEncodedBytes {
		mediaType = "image/jpeg"
		finalBytes = nil
		jpegImage := coordinateCompositeOnWhiteV1(finalImage)
		for _, quality := range input.Profile.JPEGQualityLadder {
			candidate, encodeErr := coordinateEncodeJPEGV1(jpegImage, quality)
			if encodeErr != nil {
				return CoordinateDisplayArtifactV1{}, encodeErr
			}
			if len(candidate) <= input.Profile.MaxEncodedBytes {
				finalBytes = candidate
				break
			}
		}
		if len(finalBytes) == 0 {
			return CoordinateDisplayArtifactV1{}, fmt.Errorf("coordinate display image exceeds profile byte cap after JPEG ladder")
		}
	}
	if base64.StdEncoding.EncodedLen(len(finalBytes)) > client.MaxInlineImageBase64Bytes {
		return CoordinateDisplayArtifactV1{}, fmt.Errorf("coordinate display image exceeds inline base64 cap")
	}
	if err := validateCoordinateFinalImageV1(
		finalBytes, mediaType, finalWidth, finalHeight); err != nil {
		return CoordinateDisplayArtifactV1{}, err
	}

	displayID := *capture.DisplayID
	quartzRect := *capture.DisplayQuartzBounds
	frame := CoordinateFrameV1{
		SchemaVersion:      1,
		FrameID:            input.FrameID,
		TopologyRef:        *capture.TopologyRef,
		StateID:            input.StateID,
		Scope:              "display",
		Actionable:         true,
		DisplayID:          &displayID,
		CapturedQuartzRect: quartzRect,
		RawImage: CoordinateRawImageV1{
			WidthPX: *capture.WidthPX, HeightPX: *capture.HeightPX,
		},
		FinalImage: CoordinateFinalImageV1{
			MediaType: mediaType, WidthPX: finalWidth, HeightPX: finalHeight,
			ByteLength: len(finalBytes), SHA256: captureWindowSHA256(finalBytes),
		},
		ProviderProfile: CoordinateProviderProfileRefV1{
			ID: input.Profile.ID, Version: input.Profile.Version,
		},
		PixelOrigin: coordinatePixelOriginV1,
		TransformRegions: []CoordinateTransformRegionV1{{
			DisplayID:  displayID,
			PixelRect:  CoordinatePixelRectV1{Width: finalWidth, Height: finalHeight},
			QuartzRect: quartzRect,
			Affine: CoordinateAffineV1{
				A:  quartzRect.Width / float64(finalWidth),
				D:  quartzRect.Height / float64(finalHeight),
				TX: quartzRect.X,
				TY: quartzRect.Y,
			},
		}},
		CreatedAt: capturedAt.UTC().Format(time.RFC3339Nano),
		ExpiresAt: expiresAt.UTC().Format(time.RFC3339Nano),
	}
	if err := frame.ValidateAgainst(input.Profile); err != nil {
		return CoordinateDisplayArtifactV1{}, fmt.Errorf("coordinate display final frame validation: %w", err)
	}

	return CoordinateDisplayArtifactV1{
		imageBytes: append([]byte(nil), finalBytes...),
		mediaType:  mediaType,
		frame:      cloneCoordinateFrameV1(frame),
	}, nil
}
