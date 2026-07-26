package tools

import (
	"bytes"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"strings"
	"testing"
	"time"
)

func TestFitCoordinateWindowArtifactToCanvasV1CentersContentAndRejectsPaddingCoordinates(t *testing.T) {
	fixture := newCoordinateFinalizerFixture(
		t,
		16,
		12,
		1,
		coordinatePatternPixels,
		time.Date(2026, 7, 22, 12, 3, 30, 0, time.UTC),
	)
	artifact, err := FinalizeCoordinateWindowV1(fixture.input)
	if err != nil {
		t.Fatal(err)
	}
	profile, err := anthropicFixedCanvasProfileV1(1280, 800)
	if err != nil {
		t.Fatal(err)
	}
	fitted, err := FitCoordinateWindowArtifactToCanvasV1(artifact, 1280, 800, profile)
	if err != nil {
		t.Fatal(err)
	}

	frame := fitted.Frame()
	if frame.FinalImage.WidthPX != 1280 || frame.FinalImage.HeightPX != 800 {
		t.Fatalf("fixed canvas = %dx%d", frame.FinalImage.WidthPX, frame.FinalImage.HeightPX)
	}
	if err := frame.ValidateAgainst(profile); err != nil {
		t.Fatalf("fixed frame/profile rejected: %v", err)
	}
	region := frame.TransformRegions[0]
	if region.PixelRect != (CoordinatePixelRectV1{X: 106, Y: 0, Width: 1067, Height: 800}) {
		t.Fatalf("content rect = %+v", region.PixelRect)
	}

	if _, err := MapCoordinatePixelCenterV1(
		frame,
		frame.TopologyRef,
		frame.StateID,
		frame.FrameID,
		fixture.input.Now,
		0,
		400,
	); CoordinateMapErrorCodeV1(err) != "coordinate_gap" {
		t.Fatalf("left padding mapping error = %v", err)
	}
	mapped, err := MapCoordinatePixelCenterV1(
		frame,
		frame.TopologyRef,
		frame.StateID,
		frame.FrameID,
		fixture.input.Now,
		float64(region.PixelRect.X),
		float64(region.PixelRect.Y),
	)
	if err != nil {
		t.Fatal(err)
	}
	if !coordinateQuartzRectContainsPoint(frame.CapturedQuartzRect, mapped.X, mapped.Y) {
		t.Fatalf("content coordinate mapped outside target window: %+v", mapped)
	}

	decoded, _, err := image.Decode(bytes.NewReader(fitted.ImageBytes()))
	if err != nil {
		t.Fatal(err)
	}
	r, g, b, a := decoded.At(0, 400).RGBA()
	if uint8(r>>8) != coordinateLetterboxBackgroundV1.R ||
		uint8(g>>8) != coordinateLetterboxBackgroundV1.G ||
		uint8(b>>8) != coordinateLetterboxBackgroundV1.B ||
		uint8(a>>8) != coordinateLetterboxBackgroundV1.A {
		t.Fatalf("padding pixel = rgba(%d,%d,%d,%d)", r>>8, g>>8, b>>8, a>>8)
	}
}

func TestFitCoordinateWindowArtifactToCanvasV1RequiresDeclaredLetterboxProfile(t *testing.T) {
	fixture := newCoordinateFinalizerFixture(
		t,
		12,
		16,
		1,
		coordinatePatternPixels,
		time.Date(2026, 7, 22, 12, 3, 30, 0, time.UTC),
	)
	artifact, err := FinalizeCoordinateWindowV1(fixture.input)
	if err != nil {
		t.Fatal(err)
	}
	profile, err := anthropicFixedCanvasProfileV1(1280, 800)
	if err != nil {
		t.Fatal(err)
	}
	profile.PaddingMode = "none"
	_, err = FitCoordinateWindowArtifactToCanvasV1(artifact, 1280, 800, profile)
	if err == nil || !strings.Contains(err.Error(), "requires letterbox") {
		t.Fatalf("error = %v", err)
	}
}
