package tools

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"math"
	"os"
	"strings"
	"testing"
	"time"
)

func TestFinalizeCoordinateWindowV1NoOpPNGMetadataAffineAndDefensiveCopies(t *testing.T) {
	fixture := newCoordinateFinalizerFixture(t, 16, 12, 1, coordinateSolidPixels, time.Date(2026, 7, 22, 12, 3, 30, 0, time.UTC))
	artifact, err := FinalizeCoordinateWindowV1(fixture.input)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(artifact.ImageBytes(), fixture.png) {
		t.Fatal("no-op PNG finalization changed canonical capture bytes")
	}
	block := artifact.ImageBlock()
	if block.MediaType != "image/png" || block.Data != base64.StdEncoding.EncodeToString(fixture.png) {
		t.Fatalf("image block = %+v", block)
	}
	frame := artifact.Frame()
	if err := frame.ValidateAgainst(fixture.input.Profile); err != nil {
		t.Fatalf("final frame/profile pair invalid: %v", err)
	}
	if frame.FinalImage.ByteLength != len(fixture.png) ||
		frame.FinalImage.SHA256 != captureWindowSHA256(fixture.png) ||
		frame.FinalImage.WidthPX != 16 || frame.FinalImage.HeightPX != 12 ||
		frame.RawImage.WidthPX != 16 || frame.RawImage.HeightPX != 12 {
		t.Fatalf("frame image metadata = %+v raw=%+v", frame.FinalImage, frame.RawImage)
	}
	if frame.FrameID != "frame-final-001" || frame.StateID != "ax-state-001" ||
		frame.TopologyRef != fixture.input.CaptureRequest.TopologyRef ||
		frame.TargetPID == nil || *frame.TargetPID != fixture.input.CaptureRequest.PID ||
		frame.TargetBundleID == nil || *frame.TargetBundleID != fixture.input.CaptureRequest.BundleID ||
		frame.TargetWindowID == nil || *frame.TargetWindowID != int(fixture.input.CaptureRequest.WindowID) {
		t.Fatalf("frame lost capture authority: %+v", frame)
	}
	region := frame.TransformRegions[0]
	if region.Affine.B != 0 || region.Affine.C != 0 ||
		region.Affine.A != frame.CapturedQuartzRect.Width/float64(frame.FinalImage.WidthPX) ||
		region.Affine.D != frame.CapturedQuartzRect.Height/float64(frame.FinalImage.HeightPX) ||
		region.Affine.TX != frame.CapturedQuartzRect.X || region.Affine.TY != frame.CapturedQuartzRect.Y {
		t.Fatalf("affine = %+v", region.Affine)
	}
	wantCreated := fixture.captureAt.Format(time.RFC3339Nano)
	wantExpires := fixture.captureAt.Add(fixture.input.TTL).Format(time.RFC3339Nano)
	if frame.CreatedAt != wantCreated || frame.ExpiresAt != wantExpires {
		t.Fatalf("frame times = %s..%s, want %s..%s", frame.CreatedAt, frame.ExpiresAt, wantCreated, wantExpires)
	}

	// Accessors must not let a caller mutate the atomically-built artifact.
	bytesCopy := artifact.ImageBytes()
	bytesCopy[0] ^= 0xff
	if bytes.Equal(bytesCopy, artifact.ImageBytes()) {
		t.Fatal("ImageBytes exposed mutable artifact storage")
	}
	frame.TargetPID = intPointer(999)
	frame.TransformRegions[0].Affine.TX = 999
	frameAgain := artifact.Frame()
	if frameAgain.TargetPID == nil || *frameAgain.TargetPID == 999 || frameAgain.TransformRegions[0].Affine.TX == 999 {
		t.Fatal("Frame exposed mutable pointer or slice storage")
	}
}

func TestFinalizeCoordinateWindowV1DownscalesWithoutUpscaling(t *testing.T) {
	t.Run("retina long edge", func(t *testing.T) {
		fixture := newCoordinateFinalizerFixture(t, 160, 120, 2, coordinatePatternPixels, time.Date(2026, 7, 22, 12, 3, 30, 0, time.UTC))
		fixture.input.Profile.TargetLongEdgePX = 128
		artifact, err := FinalizeCoordinateWindowV1(fixture.input)
		if err != nil {
			t.Fatal(err)
		}
		frame := artifact.Frame()
		if frame.FinalImage.WidthPX != 128 || frame.FinalImage.HeightPX != 96 {
			t.Fatalf("retina final dimensions = %dx%d", frame.FinalImage.WidthPX, frame.FinalImage.HeightPX)
		}
		if frame.RawImage.WidthPX != 160 || frame.CapturedQuartzRect.Width != 80 ||
			math.Abs(frame.TransformRegions[0].Affine.A-0.625) > 1e-12 {
			t.Fatalf("retina geometry drifted: %+v", frame)
		}
	})

	t.Run("small source is not enlarged", func(t *testing.T) {
		fixture := newCoordinateFinalizerFixture(t, 20, 10, 1, coordinatePatternPixels, time.Date(2026, 7, 22, 12, 3, 30, 0, time.UTC))
		fixture.input.Profile.TargetLongEdgePX = 100
		artifact, err := FinalizeCoordinateWindowV1(fixture.input)
		if err != nil {
			t.Fatal(err)
		}
		frame := artifact.Frame()
		if frame.FinalImage.WidthPX != 20 || frame.FinalImage.HeightPX != 10 {
			t.Fatalf("small source was upscaled to %dx%d", frame.FinalImage.WidthPX, frame.FinalImage.HeightPX)
		}
	})

	t.Run("square total pixel cap", func(t *testing.T) {
		fixture := newCoordinateFinalizerFixture(t, 100, 100, 1, coordinatePatternPixels, time.Date(2026, 7, 22, 12, 3, 30, 0, time.UTC))
		fixture.input.Profile.TargetLongEdgePX = 100
		fixture.input.Profile.MaxTotalPixels = 2_500
		artifact, err := FinalizeCoordinateWindowV1(fixture.input)
		if err != nil {
			t.Fatal(err)
		}
		frame := artifact.Frame()
		if frame.FinalImage.WidthPX != 50 || frame.FinalImage.HeightPX != 50 {
			t.Fatalf("pixel-capped dimensions = %dx%d", frame.FinalImage.WidthPX, frame.FinalImage.HeightPX)
		}
	})
}

func TestFinalizeCoordinateWindowV1JPEGFallbackAndOversizeFailure(t *testing.T) {
	fixture := newCoordinateFinalizerFixture(t, 128, 128, 1, coordinateNoisePixels, time.Date(2026, 7, 22, 12, 3, 30, 0, time.UTC))
	fixture.input.Profile.TargetLongEdgePX = 128
	fixture.input.Profile.MaxTotalPixels = 128 * 128
	fixture.input.Profile.MaxEncodedBytes = 8_000
	fixture.input.Profile.JPEGQualityLadder = []int{80, 40, 10}
	artifact, err := FinalizeCoordinateWindowV1(fixture.input)
	if err != nil {
		t.Fatal(err)
	}
	frame := artifact.Frame()
	if frame.FinalImage.MediaType != "image/jpeg" || artifact.ImageBlock().MediaType != "image/jpeg" {
		t.Fatalf("oversize PNG did not use JPEG ladder: %+v", frame.FinalImage)
	}
	if len(artifact.ImageBytes()) > fixture.input.Profile.MaxEncodedBytes {
		t.Fatalf("JPEG fallback bytes = %d, cap = %d", len(artifact.ImageBytes()), fixture.input.Profile.MaxEncodedBytes)
	}

	tooSmall := fixture.input
	tooSmall.Profile.MaxEncodedBytes = 10
	if _, err := FinalizeCoordinateWindowV1(tooSmall); err == nil {
		t.Fatal("finalizer accepted output when PNG and every JPEG quality exceeded the cap")
	}
}

func TestFinalizeCoordinateWindowV1JPEGFallbackUsesWhiteMatte(t *testing.T) {
	fixture := newCoordinateFinalizerFixture(t, 64, 64, 1, coordinateTransparentNoisePixels, time.Date(2026, 7, 22, 12, 3, 30, 0, time.UTC))
	fixture.input.Profile.TargetLongEdgePX = 64
	fixture.input.Profile.MaxTotalPixels = 64 * 64
	fixture.input.Profile.MaxEncodedBytes = 1_000
	fixture.input.Profile.JPEGQualityLadder = []int{90, 50, 10}
	artifact, err := FinalizeCoordinateWindowV1(fixture.input)
	if err != nil {
		t.Fatal(err)
	}
	if artifact.ImageBlock().MediaType != "image/jpeg" {
		t.Fatal("transparent oversize PNG did not use JPEG fallback")
	}
	decoded, err := jpeg.Decode(bytes.NewReader(artifact.ImageBytes()))
	if err != nil {
		t.Fatal(err)
	}
	r, g, b, _ := decoded.At(32, 32).RGBA()
	if r < 0xf000 || g < 0xf000 || b < 0xf000 {
		t.Fatalf("transparent pixel was not composited onto white: rgba16=(%x,%x,%x)", r, g, b)
	}
}

func TestFinalizeCoordinateWindowV1RejectsUnadmittedCaptureBytes(t *testing.T) {
	fixture := newCoordinateFinalizerFixture(t, 16, 12, 1, coordinateSolidPixels, time.Date(2026, 7, 22, 12, 3, 30, 0, time.UTC))
	var payload map[string]any
	if err := json.Unmarshal(fixture.input.CapturePayload, &payload); err != nil {
		t.Fatal(err)
	}
	payload["image_base64"] = payload["image_base64"].(string) + "\n"
	mutated, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	fixture.input.CapturePayload = mutated
	if _, err := FinalizeCoordinateWindowV1(fixture.input); err == nil {
		t.Fatal("finalizer bypassed strict capture admission/canonical base64 validation")
	}
}

func TestFinalizeCoordinateWindowV1TimestampAndTTLGates(t *testing.T) {
	now := time.Date(2026, 7, 22, 12, 3, 30, 0, time.UTC)

	atLimit := newCoordinateFinalizerFixture(t, 16, 12, 1, coordinateSolidPixels, now.Add(-CoordinateCaptureMaxAgeV1))
	atLimit.input.Now = now
	atLimit.input.TTL = CoordinateFrameMaxTTLV1
	artifact, err := FinalizeCoordinateWindowV1(atLimit.input)
	if err != nil {
		t.Fatalf("inclusive age/TTL limits rejected: %v", err)
	}
	if got := artifact.Frame().ExpiresAt; got != atLimit.captureAt.Add(CoordinateFrameMaxTTLV1).Format(time.RFC3339Nano) {
		t.Fatalf("expiry %s is not relative to captured_at", got)
	}

	stale := newCoordinateFinalizerFixture(t, 16, 12, 1, coordinateSolidPixels, now.Add(-CoordinateCaptureMaxAgeV1-time.Nanosecond))
	stale.input.Now = now
	if _, err := FinalizeCoordinateWindowV1(stale.input); err == nil {
		t.Fatal("stale capture was finalized")
	}

	future := newCoordinateFinalizerFixture(t, 16, 12, 1, coordinateSolidPixels, now.Add(CoordinateCaptureMaxFutureSkewV1+time.Nanosecond))
	future.input.Now = now
	if _, err := FinalizeCoordinateWindowV1(future.input); err == nil {
		t.Fatal("capture beyond future-skew tolerance was finalized")
	}

	nearFuture := newCoordinateFinalizerFixture(t, 16, 12, 1, coordinateSolidPixels, now.Add(CoordinateCaptureMaxFutureSkewV1))
	nearFuture.input.Now = now
	nearArtifact, err := FinalizeCoordinateWindowV1(nearFuture.input)
	if err != nil {
		t.Fatalf("inclusive future-skew tolerance rejected: %v", err)
	}
	frame := nearArtifact.Frame()
	if _, err := MapCoordinatePixelCenterV1(
		frame, frame.TopologyRef, frame.StateID, frame.FrameID, now, 0, 0,
	); CoordinateMapErrorCodeV1(err) != "frame_not_yet_valid" {
		t.Fatalf("future-created frame map error = %v", err)
	}

	badTTL := atLimit.input
	badTTL.TTL = CoordinateFrameMaxTTLV1 + time.Nanosecond
	if _, err := FinalizeCoordinateWindowV1(badTTL); err == nil {
		t.Fatal("TTL above the v1 bound was accepted")
	}
	badTTL.TTL = 0
	if _, err := FinalizeCoordinateWindowV1(badTTL); err == nil {
		t.Fatal("zero TTL was accepted")
	}
	badIdentity := atLimit.input
	badIdentity.FrameID = " "
	if _, err := FinalizeCoordinateWindowV1(badIdentity); err == nil {
		t.Fatal("blank injected frame_id was accepted")
	}
	badIdentity = atLimit.input
	badIdentity.StateID = ""
	if _, err := FinalizeCoordinateWindowV1(badIdentity); err == nil {
		t.Fatal("blank AX state_id was accepted")
	}
	badIdentity = atLimit.input
	badIdentity.FrameID = " frame-final-001"
	if _, err := FinalizeCoordinateWindowV1(badIdentity); err == nil {
		t.Fatal("whitespace-tainted frame_id was accepted")
	}
	badIdentity = atLimit.input
	badIdentity.StateID = "ax-state-001 "
	if _, err := FinalizeCoordinateWindowV1(badIdentity); err == nil {
		t.Fatal("whitespace-tainted state_id was accepted")
	}
}

func TestCoordinateWindowFinalizerDoesNotCallGenericCompression(t *testing.T) {
	source, err := os.ReadFile("coordinate_window_finalizer.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"compressImage(", "downscaleToFit(", "CaptureAndEncode("} {
		if strings.Contains(string(source), forbidden) {
			t.Fatalf("coordinate-only finalizer depends on generic compression path %q", forbidden)
		}
	}
}

type coordinateFinalizerFixture struct {
	input     CoordinateWindowFinalizeInputV1
	png       []byte
	captureAt time.Time
}

func newCoordinateFinalizerFixture(
	t *testing.T,
	width, height int,
	scale float64,
	pixels func(*image.NRGBA),
	captureAt time.Time,
) coordinateFinalizerFixture {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	pixels(img)
	var pngBytes bytes.Buffer
	if err := png.Encode(&pngBytes, img); err != nil {
		t.Fatal(err)
	}
	raw := pngBytes.Bytes()
	bounds := CoordinateQuartzRectV1{
		X: -100, Y: 200,
		Width: float64(width) / scale, Height: float64(height) / scale,
	}
	topologyRef := CoordinateTopologyRefV1{TopologyID: "topo-finalizer-001", Generation: 3}
	request := CaptureCoordinateWindowRequestV1{
		SchemaVersion: 1,
		TopologyRef:   topologyRef,
		PID:           4242, BundleID: "com.example.fixture", WindowID: 7001,
		ExpectedQuartzBounds: bounds,
	}
	helperBootID := "helper-finalizer-001"
	pid := request.PID
	bundleID := request.BundleID
	windowID := request.WindowID
	displayID := uint32(9)
	mediaType := "image/png"
	byteLength := len(raw)
	digest := captureWindowSHA256(raw)
	base64Image := base64.StdEncoding.EncodeToString(raw)
	capturedAt := captureAt.Format(time.RFC3339Nano)
	capture := CaptureCoordinateWindowResultV1{
		SchemaVersion: 1, Status: "captured", RetrySafe: false,
		TopologyRef: &topologyRef, HelperBootID: &helperBootID,
		PID: &pid, BundleID: &bundleID, WindowID: &windowID,
		WindowQuartzBounds: &bounds, DisplayID: &displayID,
		BackingScaleFactor: &scale, MediaType: &mediaType,
		WidthPX: &width, HeightPX: &height, ByteLength: &byteLength,
		SHA256: &digest, ImageBase64: &base64Image, CapturedAt: &capturedAt,
	}
	payload, err := EncodeCaptureCoordinateWindowResultV1(capture)
	if err != nil {
		t.Fatal(err)
	}
	topologyBounds := DisplayTopologyRectV1{X: bounds.X, Y: bounds.Y, Width: bounds.Width, Height: bounds.Height}
	topology := DisplayTopologyV1{
		SchemaVersion: 1, TopologyID: topologyRef.TopologyID,
		HelperBootID: helperBootID, Generation: topologyRef.Generation,
		CapturedAt:    captureAt.Add(-time.Second).Format(time.RFC3339Nano),
		MainDisplayID: displayID,
		Displays: []DisplayTopologyDisplayV1{{
			DisplayID: displayID, IsMain: true, IsActive: true, IsOnline: true,
			QuartzBounds: topologyBounds, AppKitFrame: topologyBounds, AppKitVisibleFrame: topologyBounds,
			BackingScaleFactor: scale, PixelWidth: width, PixelHeight: height,
		}},
	}
	profile := CoordinateImageProfileV1{
		SchemaVersion: 1, ID: "provider-finalizer", Version: 1,
		MediaType: "image/png", FallbackMediaType: "image/jpeg",
		TargetLongEdgePX: 1280, MaxLongEdgePX: 1568,
		MaxTotalPixels: 1_600_000, MaxEncodedBytes: CoordinateMaxRawImageBytesV1,
		JPEGQualityLadder: []int{90, 82, 74}, PaddingMode: "none",
		RequiresExactCoordinateDimensions: true,
	}
	return coordinateFinalizerFixture{
		input: CoordinateWindowFinalizeInputV1{
			CapturePayload: payload, CaptureRequest: request,
			CurrentTopology: topology,
			CaptureLimits: CaptureCoordinateWindowLimitsV1{
				MaxRawBytes: len(raw) + 1024, MaxNDJSONBytes: len(payload) + 1024,
				MaxPixels: width*height + 1,
			},
			StateID: "ax-state-001", Profile: profile,
			Now: captureAt.Add(time.Second), TTL: 5 * time.Second,
			FrameID: "frame-final-001",
		},
		png: append([]byte(nil), raw...), captureAt: captureAt,
	}
}

func coordinateSolidPixels(img *image.NRGBA) {
	for y := 0; y < img.Bounds().Dy(); y++ {
		for x := 0; x < img.Bounds().Dx(); x++ {
			img.SetNRGBA(x, y, color.NRGBA{R: 20, G: 40, B: 80, A: 255})
		}
	}
}

func coordinatePatternPixels(img *image.NRGBA) {
	for y := 0; y < img.Bounds().Dy(); y++ {
		for x := 0; x < img.Bounds().Dx(); x++ {
			img.SetNRGBA(x, y, color.NRGBA{
				R: uint8((x * 17) % 251), G: uint8((y * 29) % 251),
				B: uint8((x*7 + y*11) % 251), A: 255,
			})
		}
	}
}

func coordinateNoisePixels(img *image.NRGBA) {
	var state uint32 = 0x1234_5678
	for y := 0; y < img.Bounds().Dy(); y++ {
		for x := 0; x < img.Bounds().Dx(); x++ {
			state = state*1_664_525 + 1_013_904_223
			img.SetNRGBA(x, y, color.NRGBA{
				R: uint8(state), G: uint8(state >> 8), B: uint8(state >> 16), A: 255,
			})
		}
	}
}

func coordinateTransparentNoisePixels(img *image.NRGBA) {
	coordinateNoisePixels(img)
	for index := 3; index < len(img.Pix); index += 4 {
		img.Pix[index] = 0
	}
}

func intPointer(value int) *int { return &value }
