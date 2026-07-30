package tools

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"math"
	"strings"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"golang.org/x/image/draw"
)

const (
	// CoordinateCaptureMaxFutureSkewV1 tolerates only a small helper/daemon
	// clock-ordering difference. Anything further in the future is not trusted.
	CoordinateCaptureMaxFutureSkewV1 = 2 * time.Second
	// CoordinateCaptureMaxAgeV1 prevents a structurally valid capture from being
	// replayed into a fresh actionable frame.
	CoordinateCaptureMaxAgeV1 = 5 * time.Second
	// CoordinateFrameMaxTTLV1 bounds every artifact produced by this builder.
	// Expiry is measured from captured_at, not from finalization completion.
	CoordinateFrameMaxTTLV1 = 30 * time.Second
)

// CoordinateWindowFinalizeInputV1 contains every authority needed to turn a
// raw helper payload into one immutable actionable window artifact. The raw
// payload is admitted here so callers cannot assert that a result was already
// checked and bypass topology/window/image validation.
type CoordinateWindowFinalizeInputV1 struct {
	CapturePayload  []byte
	CaptureRequest  CaptureCoordinateWindowRequestV1
	CurrentTopology DisplayTopologyV1
	CaptureLimits   CaptureCoordinateWindowLimitsV1
	StateID         string
	Profile         CoordinateImageProfileV1
	Now             time.Time
	TTL             time.Duration
	FrameID         string
}

// CoordinateWindowArtifactV1 keeps its bytes and frame private. Accessors
// return copies so a consumer cannot mutate the image/frame association after
// validation.
type CoordinateWindowArtifactV1 struct {
	imageBytes []byte
	mediaType  string
	frame      CoordinateFrameV1
}

func (artifact CoordinateWindowArtifactV1) ImageBytes() []byte {
	return append([]byte(nil), artifact.imageBytes...)
}

func (artifact CoordinateWindowArtifactV1) ImageBlock() agent.ImageBlock {
	return agent.ImageBlock{
		MediaType: artifact.mediaType,
		Data:      base64.StdEncoding.EncodeToString(artifact.imageBytes),
	}
}

func (artifact CoordinateWindowArtifactV1) Frame() CoordinateFrameV1 {
	return cloneCoordinateFrameV1(artifact.frame)
}

// FinalizeCoordinateWindowV1 admits a raw capture and atomically returns the
// exact final image bytes plus the only CoordinateFrame authorized to map
// pixels in those bytes. It is intentionally separate from generic image
// compression and is used by ComputerUseTool's exact-window coordinate path.
func FinalizeCoordinateWindowV1(
	input CoordinateWindowFinalizeInputV1,
) (CoordinateWindowArtifactV1, error) {
	if strings.TrimSpace(input.StateID) == "" || input.StateID != strings.TrimSpace(input.StateID) ||
		strings.TrimSpace(input.FrameID) == "" || input.FrameID != strings.TrimSpace(input.FrameID) {
		return CoordinateWindowArtifactV1{}, fmt.Errorf("coordinate finalizer requires nonempty state_id and frame_id")
	}
	if input.Now.IsZero() {
		return CoordinateWindowArtifactV1{}, fmt.Errorf("coordinate finalizer requires trusted now")
	}
	if input.TTL <= 0 || input.TTL > CoordinateFrameMaxTTLV1 {
		return CoordinateWindowArtifactV1{}, fmt.Errorf("coordinate frame TTL must be in (0, %s]", CoordinateFrameMaxTTLV1)
	}
	if err := input.Profile.Validate(); err != nil {
		return CoordinateWindowArtifactV1{}, fmt.Errorf("coordinate finalizer profile: %w", err)
	}

	capture, err := AdmitCaptureCoordinateWindowV1(
		input.CapturePayload,
		input.CaptureRequest,
		input.CurrentTopology,
		input.CaptureLimits)
	if err != nil {
		return CoordinateWindowArtifactV1{}, fmt.Errorf("coordinate finalizer capture admission: %w", err)
	}
	if capture.Status != "captured" {
		return CoordinateWindowArtifactV1{}, fmt.Errorf("coordinate finalizer requires a captured result")
	}

	capturedAt, err := time.Parse(time.RFC3339Nano, *capture.CapturedAt)
	if err != nil {
		return CoordinateWindowArtifactV1{}, fmt.Errorf("coordinate finalizer captured_at: %w", err)
	}
	if capturedAt.After(input.Now.Add(CoordinateCaptureMaxFutureSkewV1)) {
		return CoordinateWindowArtifactV1{}, fmt.Errorf("coordinate capture exceeds future-skew tolerance")
	}
	if input.Now.Sub(capturedAt) > CoordinateCaptureMaxAgeV1 {
		return CoordinateWindowArtifactV1{}, fmt.Errorf("coordinate capture is stale")
	}
	expiresAt := capturedAt.Add(input.TTL)
	if !input.Now.Before(expiresAt) {
		return CoordinateWindowArtifactV1{}, fmt.Errorf("coordinate frame would already be expired")
	}

	raw, err := base64.StdEncoding.Strict().DecodeString(*capture.ImageBase64)
	if err != nil || base64.StdEncoding.EncodeToString(raw) != *capture.ImageBase64 {
		return CoordinateWindowArtifactV1{}, fmt.Errorf("coordinate capture image_base64 is not canonical")
	}
	if len(raw) != *capture.ByteLength || captureWindowSHA256(raw) != *capture.SHA256 {
		return CoordinateWindowArtifactV1{}, fmt.Errorf("coordinate capture bytes do not match admitted metadata")
	}
	if err := validateCaptureCoordinateWindowPNG(raw, *capture.WidthPX, *capture.HeightPX); err != nil {
		return CoordinateWindowArtifactV1{}, fmt.Errorf("coordinate capture PNG revalidation: %w", err)
	}
	source, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		return CoordinateWindowArtifactV1{}, fmt.Errorf("coordinate finalizer decode PNG: %w", err)
	}

	finalWidth, finalHeight := coordinateFinalDimensionsV1(
		*capture.WidthPX,
		*capture.HeightPX,
		input.Profile)
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
			return CoordinateWindowArtifactV1{}, err
		}
	}
	if len(finalBytes) > input.Profile.MaxEncodedBytes {
		mediaType = "image/jpeg"
		finalBytes = nil
		jpegImage := coordinateCompositeOnWhiteV1(finalImage)
		for _, quality := range input.Profile.JPEGQualityLadder {
			candidate, encodeErr := coordinateEncodeJPEGV1(jpegImage, quality)
			if encodeErr != nil {
				return CoordinateWindowArtifactV1{}, encodeErr
			}
			if len(candidate) <= input.Profile.MaxEncodedBytes {
				finalBytes = candidate
				break
			}
		}
		if len(finalBytes) == 0 {
			return CoordinateWindowArtifactV1{}, fmt.Errorf("coordinate image exceeds profile byte cap after JPEG ladder")
		}
	}
	if base64.StdEncoding.EncodedLen(len(finalBytes)) > client.MaxInlineImageBase64Bytes {
		return CoordinateWindowArtifactV1{}, fmt.Errorf("coordinate image exceeds inline base64 cap")
	}
	if err := validateCoordinateFinalImageV1(finalBytes, mediaType, finalWidth, finalHeight); err != nil {
		return CoordinateWindowArtifactV1{}, err
	}

	displayID := *capture.DisplayID
	targetPID := *capture.PID
	targetBundleID := *capture.BundleID
	targetWindowID := int(*capture.WindowID)
	quartzRect := *capture.WindowQuartzBounds
	affineA := quartzRect.Width / float64(finalWidth)
	affineD := quartzRect.Height / float64(finalHeight)
	frame := CoordinateFrameV1{
		SchemaVersion:      1,
		FrameID:            input.FrameID,
		TopologyRef:        *capture.TopologyRef,
		StateID:            input.StateID,
		Scope:              "window",
		Actionable:         true,
		DisplayID:          &displayID,
		TargetPID:          &targetPID,
		TargetBundleID:     &targetBundleID,
		TargetWindowID:     &targetWindowID,
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
				A: affineA, D: affineD, TX: quartzRect.X, TY: quartzRect.Y,
			},
		}},
		CreatedAt: capturedAt.UTC().Format(time.RFC3339Nano),
		ExpiresAt: expiresAt.UTC().Format(time.RFC3339Nano),
	}
	if err := frame.ValidateAgainst(input.Profile); err != nil {
		return CoordinateWindowArtifactV1{}, fmt.Errorf("coordinate final frame validation: %w", err)
	}

	return CoordinateWindowArtifactV1{
		imageBytes: append([]byte(nil), finalBytes...),
		mediaType:  mediaType,
		frame:      cloneCoordinateFrameV1(frame),
	}, nil
}

func coordinateFinalDimensionsV1(
	width, height int,
	profile CoordinateImageProfileV1,
) (int, int) {
	scale := 1.0
	longEdge := width
	if height > longEdge {
		longEdge = height
	}
	if longEdge > profile.TargetLongEdgePX {
		scale = float64(profile.TargetLongEdgePX) / float64(longEdge)
	}
	pixelScale := math.Sqrt(float64(profile.MaxTotalPixels) / (float64(width) * float64(height)))
	if pixelScale < scale {
		scale = pixelScale
	}
	if scale > 1 {
		scale = 1
	}
	finalWidth := int(math.Floor(float64(width) * scale))
	finalHeight := int(math.Floor(float64(height) * scale))
	if finalWidth < 1 {
		finalWidth = 1
	}
	if finalHeight < 1 {
		finalHeight = 1
	}
	for int64(finalWidth)*int64(finalHeight) > int64(profile.MaxTotalPixels) {
		if finalWidth >= finalHeight && finalWidth > 1 {
			finalWidth--
		} else if finalHeight > 1 {
			finalHeight--
		} else {
			break
		}
	}
	return finalWidth, finalHeight
}

func coordinateScaleImageV1(source image.Image, width, height int) image.Image {
	destination := image.NewRGBA(image.Rect(0, 0, width, height))
	draw.CatmullRom.Scale(
		destination,
		destination.Bounds(),
		source,
		source.Bounds(),
		draw.Over,
		nil)
	return destination
}

func coordinateEncodePNGV1(source image.Image) ([]byte, error) {
	var output bytes.Buffer
	if err := png.Encode(&output, source); err != nil {
		return nil, fmt.Errorf("coordinate finalizer encode PNG: %w", err)
	}
	return output.Bytes(), nil
}

func coordinateCompositeOnWhiteV1(source image.Image) image.Image {
	bounds := source.Bounds()
	destination := image.NewNRGBA(image.Rect(0, 0, bounds.Dx(), bounds.Dy()))
	for y := 0; y < bounds.Dy(); y++ {
		for x := 0; x < bounds.Dx(); x++ {
			pixel := color.NRGBAModel.Convert(source.At(bounds.Min.X+x, bounds.Min.Y+y)).(color.NRGBA)
			alpha := uint32(pixel.A)
			destination.SetNRGBA(x, y, color.NRGBA{
				R: uint8((uint32(pixel.R)*alpha + 255*(255-alpha) + 127) / 255),
				G: uint8((uint32(pixel.G)*alpha + 255*(255-alpha) + 127) / 255),
				B: uint8((uint32(pixel.B)*alpha + 255*(255-alpha) + 127) / 255),
				A: 255,
			})
		}
	}
	return destination
}

func coordinateEncodeJPEGV1(source image.Image, quality int) ([]byte, error) {
	var output bytes.Buffer
	if err := jpeg.Encode(&output, source, &jpeg.Options{Quality: quality}); err != nil {
		return nil, fmt.Errorf("coordinate finalizer encode JPEG q=%d: %w", quality, err)
	}
	return output.Bytes(), nil
}

func validateCoordinateFinalImageV1(
	data []byte,
	mediaType string,
	expectedWidth, expectedHeight int,
) error {
	var decoded image.Image
	var err error
	switch mediaType {
	case "image/png":
		if err := validateCaptureCoordinateWindowPNG(data, expectedWidth, expectedHeight); err != nil {
			return fmt.Errorf("coordinate final PNG validation: %w", err)
		}
		decoded, err = png.Decode(bytes.NewReader(data))
	case "image/jpeg":
		decoded, err = jpeg.Decode(bytes.NewReader(data))
	default:
		return fmt.Errorf("coordinate final image media type %q is unsupported", mediaType)
	}
	if err != nil {
		return fmt.Errorf("coordinate final image full decode: %w", err)
	}
	if decoded.Bounds().Dx() != expectedWidth || decoded.Bounds().Dy() != expectedHeight {
		return fmt.Errorf("coordinate final image dimensions mismatch")
	}
	return nil
}

func cloneCoordinateFrameV1(frame CoordinateFrameV1) CoordinateFrameV1 {
	clone := frame
	if frame.DisplayID != nil {
		value := *frame.DisplayID
		clone.DisplayID = &value
	}
	if frame.TargetPID != nil {
		value := *frame.TargetPID
		clone.TargetPID = &value
	}
	if frame.TargetBundleID != nil {
		value := *frame.TargetBundleID
		clone.TargetBundleID = &value
	}
	if frame.TargetWindowID != nil {
		value := *frame.TargetWindowID
		clone.TargetWindowID = &value
	}
	clone.TransformRegions = append([]CoordinateTransformRegionV1(nil), frame.TransformRegions...)
	return clone
}
