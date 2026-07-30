package tools

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

func createMinimalPNG() []byte {
	img := image.NewRGBA(image.Rect(0, 0, 1, 1))
	img.Set(0, 0, color.RGBA{255, 0, 0, 255})
	var buf bytes.Buffer
	png.Encode(&buf, img)
	return buf.Bytes()
}

func createMinimalJPEG() []byte {
	img := image.NewRGBA(image.Rect(0, 0, 1, 1))
	img.Set(0, 0, color.RGBA{0, 255, 0, 255})
	var buf bytes.Buffer
	jpeg.Encode(&buf, img, nil)
	return buf.Bytes()
}

func TestEncodeImage(t *testing.T) {
	pngData := createMinimalPNG()
	path := filepath.Join(t.TempDir(), "test.png")
	if err := os.WriteFile(path, pngData, 0644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}

	block, err := EncodeImage(path)
	if err != nil {
		t.Fatalf("EncodeImage error: %v", err)
	}
	if block.MediaType != "image/png" {
		t.Errorf("expected MediaType 'image/png', got %q", block.MediaType)
	}

	decoded, err := base64.StdEncoding.DecodeString(block.Data)
	if err != nil {
		t.Fatalf("base64 decode error: %v", err)
	}
	if !bytes.Equal(decoded, pngData) {
		t.Error("decoded base64 data does not match original PNG bytes")
	}
}

func TestEncodeImage_FileNotFound(t *testing.T) {
	_, err := EncodeImage("/nonexistent/file.png")
	if err == nil {
		t.Error("expected error for nonexistent file")
	}
}

func TestEncodeImage_JPEG(t *testing.T) {
	jpegData := createMinimalJPEG()
	path := filepath.Join(t.TempDir(), "test.jpg")
	if err := os.WriteFile(path, jpegData, 0644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}

	block, err := EncodeImage(path)
	if err != nil {
		t.Fatalf("EncodeImage error: %v", err)
	}
	if block.MediaType != "image/jpeg" {
		t.Errorf("expected MediaType 'image/jpeg', got %q", block.MediaType)
	}
}

func TestEncodeImage_JPEG_Uppercase(t *testing.T) {
	jpegData := createMinimalJPEG()
	path := filepath.Join(t.TempDir(), "test.JPEG")
	if err := os.WriteFile(path, jpegData, 0644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}

	block, err := EncodeImage(path)
	if err != nil {
		t.Fatalf("EncodeImage error: %v", err)
	}
	if block.MediaType != "image/jpeg" {
		t.Errorf("expected MediaType 'image/jpeg', got %q", block.MediaType)
	}
}

func TestGetScreenDimensions(t *testing.T) {
	w, h, err := GetScreenDimensions()
	if err != nil {
		t.Skipf("skipping: no display available (%v)", err)
	}
	if w <= 0 {
		t.Errorf("expected width > 0, got %d", w)
	}
	if h <= 0 {
		t.Errorf("expected height > 0, got %d", h)
	}
}

func TestParseScreenDimensions_Resolution(t *testing.T) {
	output := `Graphics/Displays:

    Apple M2 Pro:

      Chipset Model: Apple M2 Pro
      Type: GPU
      Bus: Built-In
      Total Number of Cores: 19
      Vendor: Apple (0x106b)
      Metal Support: Metal 3
      Displays:
        Color LCD:
          Display Type: Built-In Retina LCD
          Resolution: 1512 x 982
          Main Display: Yes
          Mirror: Off
          Online: Yes
          Automatically Adjust Brightness: Yes
          Connection Type: Internal
`
	w, h, err := parseScreenDimensions(output)
	if err != nil {
		t.Fatalf("parseScreenDimensions error: %v", err)
	}
	if w != 1512 {
		t.Errorf("expected width 1512, got %d", w)
	}
	if h != 982 {
		t.Errorf("expected height 982, got %d", h)
	}
}

func TestParseScreenDimensions_UILooksLike(t *testing.T) {
	output := `Graphics/Displays:

    Apple M1:

      Chipset Model: Apple M1
      Displays:
        Color LCD:
          Display Type: Built-In Retina LCD
          Resolution: 2560 x 1600 (Retina)
          UI Looks like: 1440 x 900 @ 120.00Hz
          Main Display: Yes
`
	w, h, err := parseScreenDimensions(output)
	if err != nil {
		t.Fatalf("parseScreenDimensions error: %v", err)
	}
	if w != 1440 {
		t.Errorf("expected width 1440, got %d", w)
	}
	if h != 900 {
		t.Errorf("expected height 900, got %d", h)
	}
}

func TestParseScreenDimensions_RetinaStripped(t *testing.T) {
	output := `Graphics/Displays:

    Apple M2:

      Displays:
        Color LCD:
          Resolution: 2880 x 1800 (Retina)
          Main Display: Yes
`
	w, h, err := parseScreenDimensions(output)
	if err != nil {
		t.Fatalf("parseScreenDimensions error: %v", err)
	}
	if w != 1440 {
		t.Errorf("expected logical width 1440, got %d", w)
	}
	if h != 900 {
		t.Errorf("expected logical height 900, got %d", h)
	}
}

func TestParseScreenDimensions_NoDisplay(t *testing.T) {
	output := `Graphics/Displays:

    Apple M2 Pro:

      Chipset Model: Apple M2 Pro
`
	_, _, err := parseScreenDimensions(output)
	if err == nil {
		t.Error("expected error for output with no display info")
	}
}

func TestParseScreenGeometry_PreservesLogicalAndCaptureDimensions(t *testing.T) {
	output := `Graphics/Displays:
      Displays:
        Color LCD:
          Resolution: 5120 x 2880 (Retina)
          UI Looks like: 2560 x 1440
          Main Display: Yes
`
	geometry, err := parseScreenGeometry(output)
	if err != nil {
		t.Fatal(err)
	}
	if geometry.LogicalWidth != 2560 || geometry.LogicalHeight != 1440 ||
		geometry.CaptureWidth != 5120 || geometry.CaptureHeight != 2880 {
		t.Fatalf("geometry = %+v", geometry)
	}
}

func TestParseScreenGeometry_SelectsMainDisplayInsteadOfFirstDisplay(t *testing.T) {
	output := `Graphics/Displays:
      Displays:
        Side Display:
          Resolution: 3840 x 2160
          UI Looks like: 1920 x 1080
          Main Display: No
        Studio Display:
          Resolution: 5120 x 2880 (Retina)
          UI Looks like: 2560 x 1440
          Main Display: Yes
`
	geometry, err := parseScreenGeometry(output)
	if err != nil {
		t.Fatal(err)
	}
	if geometry != (screenGeometry{
		LogicalWidth: 2560, LogicalHeight: 1440,
		CaptureWidth: 5120, CaptureHeight: 2880,
	}) {
		t.Fatalf("main geometry = %+v", geometry)
	}
}

func TestLegacyMainDisplayCaptureArgumentsSelectOnlyMainDisplay(t *testing.T) {
	args := legacyMainDisplayCaptureArgs("/tmp/main.png")
	if !slices.Contains(args, "-m") {
		t.Fatalf("legacy capture args %v do not select only the main display", args)
	}
	if args[len(args)-1] != "/tmp/main.png" {
		t.Fatalf("legacy capture output = %q", args[len(args)-1])
	}
}

func TestCaptureMainDisplayRejectsAndCleansMultipleOutputs(t *testing.T) {
	var scratchDir string
	path, _, err := captureMainDisplayAndEncode(0, func(output string) error {
		scratchDir = filepath.Dir(output)
		for _, name := range []string{filepath.Base(output), "main 2.png"} {
			file, createErr := os.Create(filepath.Join(scratchDir, name))
			if createErr != nil {
				return createErr
			}
			if encodeErr := png.Encode(file, image.NewRGBA(image.Rect(0, 0, 4, 3))); encodeErr != nil {
				file.Close()
				return encodeErr
			}
			if closeErr := file.Close(); closeErr != nil {
				return closeErr
			}
		}
		return nil
	})
	if err == nil || path != "" {
		t.Fatalf("path=%q err=%v, want fail-closed multi-output capture", path, err)
	}
	if _, statErr := os.Stat(scratchDir); !os.IsNotExist(statErr) {
		t.Fatalf("scratch capture directory survived: %v", statErr)
	}
}

func TestCaptureMainDisplayReturnsOneImageAndCleansScratch(t *testing.T) {
	var scratchDir string
	path, block, err := captureMainDisplayAndEncode(0, func(output string) error {
		scratchDir = filepath.Dir(output)
		file, createErr := os.Create(output)
		if createErr != nil {
			return createErr
		}
		if encodeErr := png.Encode(file, image.NewRGBA(image.Rect(0, 0, 16, 9))); encodeErr != nil {
			file.Close()
			return encodeErr
		}
		return file.Close()
	})
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(path)
	if block.MediaType != "image/png" || block.Data == "" {
		t.Fatalf("block = %+v", block)
	}
	if width, height, dimErr := imageFileDimensions(path); dimErr != nil || width != 16 || height != 9 {
		t.Fatalf("dimensions=%dx%d err=%v", width, height, dimErr)
	}
	if _, statErr := os.Stat(scratchDir); !os.IsNotExist(statErr) {
		t.Fatalf("scratch capture directory survived: %v", statErr)
	}
}

func TestBoundedLegacyMainDisplayCaptureKillsAndReapsOnTimeout(t *testing.T) {
	started := time.Now()
	err := runBoundedLegacyMainDisplayCapture("/bin/sleep", []string{"5"}, 50*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("timeout error = %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("timed-out capture returned after %s; child was not promptly killed and reaped", elapsed)
	}
}

func TestBoundedLegacyMainDisplayCaptureStopsOnParentCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	started := time.Now()
	err := runBoundedLegacyMainDisplayCaptureContext(
		ctx, "/bin/sleep", []string{"5"}, 8*time.Second)
	if err == nil || !strings.Contains(err.Error(), "canceled") {
		t.Fatalf("cancellation error = %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("cancelled capture returned after %s; child was not killed and reaped", elapsed)
	}
}

func TestBoundedLegacyImageResizeStopsOnParentCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	started := time.Now()
	err := runBoundedLegacyImageResizeCommand(
		ctx, "/bin/sleep", []string{"5"}, 8*time.Second)
	if err == nil || !strings.Contains(err.Error(), "canceled") {
		t.Fatalf("cancellation error = %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("cancelled resize returned after %s; child was not killed and reaped", elapsed)
	}
}

func TestCaptureMainDisplayCleansScratchOnRunnerFailure(t *testing.T) {
	var scratchDir string
	path, _, err := captureMainDisplayAndEncode(0, func(output string) error {
		scratchDir = filepath.Dir(output)
		return fmt.Errorf("synthetic capture failure")
	})
	if err == nil || path != "" {
		t.Fatalf("path=%q err=%v", path, err)
	}
	if _, statErr := os.Stat(scratchDir); !os.IsNotExist(statErr) {
		t.Fatalf("failed capture scratch survived: %v", statErr)
	}
}

func TestCaptureMainDisplayContextCancellationDuringResizeCleansEveryOutput(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var scratchDir string
	resizeStarted := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		path, _, err := captureMainDisplayAndEncodeContext(
			ctx,
			1280,
			func(_ context.Context, output string) error {
				scratchDir = filepath.Dir(output)
				file, createErr := os.Create(output)
				if createErr != nil {
					return createErr
				}
				if encodeErr := png.Encode(file, image.NewRGBA(image.Rect(0, 0, 16, 9))); encodeErr != nil {
					file.Close()
					return encodeErr
				}
				return file.Close()
			},
			func(resizeCtx context.Context, _ string, _ int) error {
				close(resizeStarted)
				<-resizeCtx.Done()
				return resizeCtx.Err()
			})
		if path != "" {
			done <- fmt.Errorf("cancelled resize returned path %q", path)
			return
		}
		done <- err
	}()
	<-resizeStarted
	cancel()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "context canceled") {
			t.Fatalf("resize cancellation error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("context cancellation did not stop resize pipeline")
	}
	if _, statErr := os.Stat(scratchDir); !os.IsNotExist(statErr) {
		t.Fatalf("cancelled resize scratch survived: %v", statErr)
	}
}

func TestResizeDimensionsMatchesLongestEdgeWithoutAspectMismatch(t *testing.T) {
	w, h := resizeDimensions(5120, 2880, 1280)
	if w != 1280 || h != 720 {
		t.Fatalf("resize = %dx%d, want 1280x720", w, h)
	}
	w, h = resizeDimensions(1000, 750, 1280)
	if w != 1000 || h != 750 {
		t.Fatalf("no-upscale resize = %dx%d", w, h)
	}
}

func TestScaleCoordinates(t *testing.T) {
	// API: 1280x800, Screen: 1440x900
	x, y := ScaleCoordinates(640, 400, 1280, 800, 1440, 900)
	if x != 720 {
		t.Errorf("expected x=720, got %d", x)
	}
	if y != 450 {
		t.Errorf("expected y=450, got %d", y)
	}
}

func TestScaleCoordinates_Identity(t *testing.T) {
	x, y := ScaleCoordinates(100, 200, 1280, 800, 1280, 800)
	if x != 100 {
		t.Errorf("expected x=100, got %d", x)
	}
	if y != 200 {
		t.Errorf("expected y=200, got %d", y)
	}
}

func TestScaleCoordinates_Origin(t *testing.T) {
	x, y := ScaleCoordinates(0, 0, 1280, 800, 1920, 1080)
	if x != 0 {
		t.Errorf("expected x=0, got %d", x)
	}
	if y != 0 {
		t.Errorf("expected y=0, got %d", y)
	}
}

func TestScaleCoordinates_MaxCorner(t *testing.T) {
	x, y := ScaleCoordinates(1280, 800, 1280, 800, 1920, 1080)
	if x != 1920 {
		t.Errorf("expected x=1920, got %d", x)
	}
	if y != 1080 {
		t.Errorf("expected y=1080, got %d", y)
	}
}

func TestClampCoordinates(t *testing.T) {
	x, y := ClampCoordinates(-10, 1000, 1280, 800)
	if x != 0 {
		t.Errorf("expected x=0, got %d", x)
	}
	if y != 799 {
		t.Errorf("expected y=799, got %d", y)
	}
}

func TestClampCoordinates_InBounds(t *testing.T) {
	x, y := ClampCoordinates(500, 400, 1280, 800)
	if x != 500 {
		t.Errorf("expected x=500, got %d", x)
	}
	if y != 400 {
		t.Errorf("expected y=400, got %d", y)
	}
}

func TestClampCoordinates_BothNegative(t *testing.T) {
	x, y := ClampCoordinates(-5, -10, 1920, 1080)
	if x != 0 {
		t.Errorf("expected x=0, got %d", x)
	}
	if y != 0 {
		t.Errorf("expected y=0, got %d", y)
	}
}

func TestClampCoordinates_BothOverflow(t *testing.T) {
	x, y := ClampCoordinates(2000, 1500, 1280, 800)
	if x != 1279 {
		t.Errorf("expected x=1279, got %d", x)
	}
	if y != 799 {
		t.Errorf("expected y=799, got %d", y)
	}
}

func TestClampCoordinates_ExactBoundary(t *testing.T) {
	x, y := ClampCoordinates(1280, 800, 1280, 800)
	if x != 1279 {
		t.Errorf("expected x=1279, got %d", x)
	}
	if y != 799 {
		t.Errorf("expected y=799, got %d", y)
	}
}

func TestEncodeImage_CompressesOversizePNGUnderInlineLimit(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 1800, 1800))
	var state uint32 = 1
	for i := 0; i < len(img.Pix); i += 4 {
		state = state*1664525 + 1013904223
		img.Pix[i] = byte(state >> 24)
		state = state*1664525 + 1013904223
		img.Pix[i+1] = byte(state >> 24)
		state = state*1664525 + 1013904223
		img.Pix[i+2] = byte(state >> 24)
		img.Pix[i+3] = 255
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	if base64.StdEncoding.EncodedLen(buf.Len()) <= client.MaxInlineImageBase64Bytes {
		t.Fatalf("test fixture must exceed inline limit; encoded len=%d",
			base64.StdEncoding.EncodedLen(buf.Len()))
	}

	path := filepath.Join(t.TempDir(), "large.png")
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	block, err := EncodeImage(path)
	if err != nil {
		t.Fatalf("EncodeImage should compress oversized PNG, got error: %v", err)
	}
	if got := len(block.Data); got > client.MaxInlineImageBase64Bytes {
		t.Fatalf("encoded image exceeds inline limit: got %d, max %d",
			got, client.MaxInlineImageBase64Bytes)
	}
	if block.MediaType != "image/jpeg" {
		t.Fatalf("oversized PNG should be converted to JPEG, got %q", block.MediaType)
	}
}
