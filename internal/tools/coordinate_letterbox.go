package tools

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	_ "image/jpeg"
	_ "image/png"
	"math"

	xdraw "golang.org/x/image/draw"
)

var coordinateLetterboxBackgroundV1 = color.NRGBA{R: 24, G: 24, B: 24, A: 255}

func anthropicFixedCanvasProfileV1(widthPX, heightPX int) (CoordinateImageProfileV1, error) {
	if widthPX <= 0 || heightPX <= 0 {
		return CoordinateImageProfileV1{}, fmt.Errorf("Anthropic fixed canvas dimensions must be positive")
	}
	longEdge := widthPX
	if heightPX > longEdge {
		longEdge = heightPX
	}
	profile := CoordinateImageProfileV1{
		SchemaVersion:                     1,
		ID:                                "anthropic_fixed_canvas",
		Version:                           1,
		MediaType:                         "image/png",
		FallbackMediaType:                 "image/jpeg",
		TargetLongEdgePX:                  longEdge,
		MaxLongEdgePX:                     longEdge,
		MaxTotalPixels:                    widthPX * heightPX,
		MaxEncodedBytes:                   CoordinateMaxRawImageBytesV1,
		JPEGQualityLadder:                 []int{90, 82, 74},
		PaddingMode:                       "letterbox",
		RequiresExactCoordinateDimensions: true,
	}
	if err := profile.Validate(); err != nil {
		return CoordinateImageProfileV1{}, err
	}
	return profile, nil
}

// FitCoordinateWindowArtifactToCanvasV1 turns one exact-window artifact into a
// stable provider canvas. Only the centered content rectangle maps to Quartz;
// clicks in padding fail with coordinate_gap.
func FitCoordinateWindowArtifactToCanvasV1(
	artifact CoordinateWindowArtifactV1,
	canvasWidthPX int,
	canvasHeightPX int,
	profile CoordinateImageProfileV1,
) (CoordinateWindowArtifactV1, error) {
	if canvasWidthPX <= 0 || canvasHeightPX <= 0 {
		return CoordinateWindowArtifactV1{}, fmt.Errorf("fixed coordinate canvas dimensions must be positive")
	}
	if profile.PaddingMode != "letterbox" {
		return CoordinateWindowArtifactV1{}, fmt.Errorf("fixed coordinate canvas requires letterbox profile")
	}
	if err := artifact.frame.Validate(); err != nil {
		return CoordinateWindowArtifactV1{}, fmt.Errorf("fixed coordinate canvas source frame: %w", err)
	}
	if len(artifact.imageBytes) == 0 {
		return CoordinateWindowArtifactV1{}, fmt.Errorf("fixed coordinate canvas source image is empty")
	}
	source, _, err := image.Decode(bytes.NewReader(artifact.imageBytes))
	if err != nil {
		return CoordinateWindowArtifactV1{}, fmt.Errorf("decode fixed coordinate canvas source: %w", err)
	}
	sourceWidth := source.Bounds().Dx()
	sourceHeight := source.Bounds().Dy()
	if sourceWidth <= 0 || sourceHeight <= 0 ||
		sourceWidth != artifact.frame.FinalImage.WidthPX ||
		sourceHeight != artifact.frame.FinalImage.HeightPX {
		return CoordinateWindowArtifactV1{}, fmt.Errorf("fixed coordinate canvas source dimensions do not match frame")
	}

	scale := math.Min(
		float64(canvasWidthPX)/float64(sourceWidth),
		float64(canvasHeightPX)/float64(sourceHeight),
	)
	contentWidth := int(math.Round(float64(sourceWidth) * scale))
	contentHeight := int(math.Round(float64(sourceHeight) * scale))
	if contentWidth < 1 {
		contentWidth = 1
	}
	if contentHeight < 1 {
		contentHeight = 1
	}
	if contentWidth > canvasWidthPX {
		contentWidth = canvasWidthPX
	}
	if contentHeight > canvasHeightPX {
		contentHeight = canvasHeightPX
	}
	offsetX := (canvasWidthPX - contentWidth) / 2
	offsetY := (canvasHeightPX - contentHeight) / 2

	canvas := image.NewNRGBA(image.Rect(0, 0, canvasWidthPX, canvasHeightPX))
	draw.Draw(canvas, canvas.Bounds(), &image.Uniform{C: coordinateLetterboxBackgroundV1}, image.Point{}, draw.Src)
	xdraw.CatmullRom.Scale(
		canvas,
		image.Rect(offsetX, offsetY, offsetX+contentWidth, offsetY+contentHeight),
		source,
		source.Bounds(),
		xdraw.Src,
		nil,
	)

	finalBytes, err := coordinateEncodePNGV1(canvas)
	if err != nil {
		return CoordinateWindowArtifactV1{}, err
	}
	mediaType := "image/png"
	if len(finalBytes) > profile.MaxEncodedBytes {
		mediaType = "image/jpeg"
		finalBytes = nil
		for _, quality := range profile.JPEGQualityLadder {
			candidate, encodeErr := coordinateEncodeJPEGV1(canvas, quality)
			if encodeErr != nil {
				return CoordinateWindowArtifactV1{}, encodeErr
			}
			if len(candidate) <= profile.MaxEncodedBytes {
				finalBytes = candidate
				break
			}
		}
		if len(finalBytes) == 0 {
			return CoordinateWindowArtifactV1{}, fmt.Errorf("fixed coordinate canvas exceeds encoded byte cap")
		}
	}
	if err := validateCoordinateFinalImageV1(
		finalBytes,
		mediaType,
		canvasWidthPX,
		canvasHeightPX,
	); err != nil {
		return CoordinateWindowArtifactV1{}, err
	}

	frame := cloneCoordinateFrameV1(artifact.frame)
	frame.FinalImage = CoordinateFinalImageV1{
		MediaType: mediaType, WidthPX: canvasWidthPX, HeightPX: canvasHeightPX,
		ByteLength: len(finalBytes), SHA256: captureWindowSHA256(finalBytes),
	}
	frame.ProviderProfile = CoordinateProviderProfileRefV1{
		ID: profile.ID, Version: profile.Version,
	}
	quartz := frame.CapturedQuartzRect
	affineA := quartz.Width / float64(contentWidth)
	affineD := quartz.Height / float64(contentHeight)
	frame.TransformRegions = []CoordinateTransformRegionV1{{
		DisplayID: *frame.DisplayID,
		PixelRect: CoordinatePixelRectV1{
			X: offsetX, Y: offsetY, Width: contentWidth, Height: contentHeight,
		},
		QuartzRect: quartz,
		Affine: CoordinateAffineV1{
			A: affineA, D: affineD,
			TX: quartz.X - affineA*float64(offsetX),
			TY: quartz.Y - affineD*float64(offsetY),
		},
	}}
	if err := frame.ValidateAgainst(profile); err != nil {
		return CoordinateWindowArtifactV1{}, fmt.Errorf("fixed coordinate canvas frame: %w", err)
	}
	return CoordinateWindowArtifactV1{
		imageBytes: append([]byte(nil), finalBytes...),
		mediaType:  mediaType,
		frame:      frame,
	}, nil
}
