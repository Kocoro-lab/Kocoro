package tools

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/png"
	"os"
	"strings"
	"testing"
	"time"
)

func TestFinalizeCoordinateDisplayV1AtomicImageFrameAndDefensiveCopies(t *testing.T) {
	fixture := newCoordinateDisplayFinalizerFixture(
		t, 160, 120, 2, time.Date(2026, 7, 23, 1, 2, 3, 0, time.UTC))
	fixture.input.Profile.TargetLongEdgePX = 128
	artifact, err := FinalizeCoordinateDisplayV1(fixture.input)
	if err != nil {
		t.Fatal(err)
	}
	frame := artifact.Frame()
	if frame.Scope != "display" || !frame.Actionable || frame.DisplayID == nil || *frame.DisplayID != 2 ||
		frame.TargetPID != nil || frame.TargetBundleID != nil || frame.TargetWindowID != nil {
		t.Fatalf("display frame target authority = %+v", frame)
	}
	if frame.RawImage.WidthPX != 160 || frame.RawImage.HeightPX != 120 ||
		frame.FinalImage.WidthPX != 128 || frame.FinalImage.HeightPX != 96 ||
		frame.FinalImage.ByteLength != len(artifact.ImageBytes()) ||
		frame.FinalImage.SHA256 != captureWindowSHA256(artifact.ImageBytes()) {
		t.Fatalf("display image metadata = raw %+v final %+v", frame.RawImage, frame.FinalImage)
	}
	region := frame.TransformRegions[0]
	if region.DisplayID != 2 || region.PixelRect.Width != 128 || region.PixelRect.Height != 96 ||
		region.Affine.A != 80.0/128.0 || region.Affine.D != 60.0/96.0 ||
		region.Affine.TX != -100 || region.Affine.TY != 200 {
		t.Fatalf("display transform = %+v", region)
	}
	if err := frame.ValidateAgainst(fixture.input.Profile); err != nil {
		t.Fatalf("final display frame/profile pair invalid: %v", err)
	}
	bytesCopy := artifact.ImageBytes()
	bytesCopy[0] ^= 0xff
	if bytes.Equal(bytesCopy, artifact.ImageBytes()) {
		t.Fatal("display artifact exposed mutable image storage")
	}
	frame.TransformRegions[0].Affine.TX = 999
	if artifact.Frame().TransformRegions[0].Affine.TX == 999 {
		t.Fatal("display artifact exposed mutable frame storage")
	}
}

func TestFinalizeCoordinateDisplayV1RejectsStaleExpiredOrUnadmittedCapture(t *testing.T) {
	now := time.Date(2026, 7, 23, 1, 2, 8, 0, time.UTC)
	fixture := newCoordinateDisplayFinalizerFixture(t, 16, 12, 1, now.Add(-CoordinateCaptureMaxAgeV1))
	fixture.input.Now = now
	fixture.input.TTL = CoordinateFrameMaxTTLV1
	if _, err := FinalizeCoordinateDisplayV1(fixture.input); err != nil {
		t.Fatalf("inclusive age limit rejected: %v", err)
	}

	stale := fixture.input
	stale.Now = now.Add(time.Nanosecond)
	if _, err := FinalizeCoordinateDisplayV1(stale); err == nil {
		t.Fatal("stale display capture finalized")
	}
	expired := fixture.input
	expired.TTL = 4 * time.Second
	if _, err := FinalizeCoordinateDisplayV1(expired); err == nil {
		t.Fatal("already-expired display frame finalized")
	}
	tampered := fixture.input
	tampered.CapturePayload = append([]byte(nil), tampered.CapturePayload...)
	tampered.CapturePayload = bytes.Replace(
		tampered.CapturePayload,
		[]byte(`"display_id":2`), []byte(`"display_id":1`), 1)
	if _, err := FinalizeCoordinateDisplayV1(tampered); err == nil {
		t.Fatal("unadmitted display capture finalized")
	}
}

func TestCoordinateDisplayFinalizerDoesNotCallGenericCompression(t *testing.T) {
	source, err := os.ReadFile("coordinate_display_finalizer.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"compressImage(", "downscaleToFit(", "CaptureAndEncode("} {
		if strings.Contains(string(source), forbidden) {
			t.Fatalf("display finalizer depends on generic compression path %q", forbidden)
		}
	}
}

type coordinateDisplayFinalizerFixture struct {
	input CoordinateDisplayFinalizeInputV1
}

func newCoordinateDisplayFinalizerFixture(
	t *testing.T,
	width, height int,
	scale float64,
	captureAt time.Time,
) coordinateDisplayFinalizerFixture {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	coordinatePatternPixels(img)
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, img); err != nil {
		t.Fatal(err)
	}
	raw := encoded.Bytes()
	ref := CoordinateTopologyRefV1{TopologyID: "topo-display-finalizer", Generation: 3}
	request := CaptureCoordinateDisplayRequestV1{
		SchemaVersion: 1, TopologyRef: ref, DisplayID: 2,
	}
	bounds := CoordinateQuartzRectV1{
		X: -100, Y: 200, Width: float64(width) / scale, Height: float64(height) / scale,
	}
	helperBootID := "helper-display-finalizer"
	mediaType := "image/png"
	byteLength := len(raw)
	digest := captureWindowSHA256(raw)
	base64Image := base64.StdEncoding.EncodeToString(raw)
	capturedAt := captureAt.Format(time.RFC3339Nano)
	displayID := request.DisplayID
	capture := CaptureCoordinateDisplayResultV1{
		SchemaVersion: 1, Status: "captured", RetrySafe: false,
		TopologyRef: &ref, HelperBootID: &helperBootID, DisplayID: &displayID,
		DisplayQuartzBounds: &bounds, BackingScaleFactor: &scale, MediaType: &mediaType,
		WidthPX: &width, HeightPX: &height, ByteLength: &byteLength,
		SHA256: &digest, ImageBase64: &base64Image, CapturedAt: &capturedAt,
	}
	payload, err := EncodeCaptureCoordinateDisplayResultV1(capture)
	if err != nil {
		t.Fatal(err)
	}
	topologyBounds := DisplayTopologyRectV1(bounds)
	mainBounds := DisplayTopologyRectV1{X: 0, Y: 0, Width: 1, Height: 1}
	topology := DisplayTopologyV1{
		SchemaVersion: 1, TopologyID: ref.TopologyID,
		HelperBootID: helperBootID, Generation: ref.Generation,
		CapturedAt: captureAt.Add(-time.Second).Format(time.RFC3339Nano), MainDisplayID: 1,
		Displays: []DisplayTopologyDisplayV1{
			{
				DisplayID: 1, IsMain: true, IsActive: true, IsOnline: true,
				QuartzBounds: mainBounds, AppKitFrame: mainBounds, AppKitVisibleFrame: mainBounds,
				BackingScaleFactor: 1, PixelWidth: 1, PixelHeight: 1,
			},
			{
				DisplayID: 2, IsActive: true, IsOnline: true,
				QuartzBounds: topologyBounds, AppKitFrame: topologyBounds, AppKitVisibleFrame: topologyBounds,
				BackingScaleFactor: scale, PixelWidth: width, PixelHeight: height,
			},
		},
	}
	profile := CoordinateImageProfileV1{
		SchemaVersion: 1, ID: "provider-display", Version: 1,
		MediaType: "image/png", FallbackMediaType: "image/jpeg",
		TargetLongEdgePX: 1280, MaxLongEdgePX: 1568,
		MaxTotalPixels: 1_600_000, MaxEncodedBytes: CoordinateMaxRawImageBytesV1,
		JPEGQualityLadder: []int{90, 82, 74}, PaddingMode: "none",
		RequiresExactCoordinateDimensions: true,
	}
	return coordinateDisplayFinalizerFixture{input: CoordinateDisplayFinalizeInputV1{
		CapturePayload: payload, CaptureRequest: request, CurrentTopology: topology,
		CaptureLimits: CaptureCoordinateDisplayLimitsV1{
			MaxRawBytes: len(raw) + 1024, MaxNDJSONBytes: len(payload) + 1024,
			MaxPixels: width*height + 1,
		},
		StateID: "ax-display-state", Profile: profile,
		Now: captureAt.Add(time.Second), TTL: 5 * time.Second,
		FrameID: "frame-display-final",
	}}
}
