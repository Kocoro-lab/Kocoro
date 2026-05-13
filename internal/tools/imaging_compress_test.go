package tools

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

// makeNoisePNG creates a w×h RGBA noise PNG — uncompressible by PNG palette,
// forcing JPEG fallback in the compression pipeline.
func makeNoisePNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	var s uint32 = 1
	for i := 0; i < len(img.Pix); i += 4 {
		s = s*1664525 + 1013904223
		img.Pix[i] = byte(s >> 24)
		s = s*1664525 + 1013904223
		img.Pix[i+1] = byte(s >> 24)
		s = s*1664525 + 1013904223
		img.Pix[i+2] = byte(s >> 24)
		img.Pix[i+3] = 255
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestCompressImage_SmallImageDirectPassthrough(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 100, 100))
	img.Set(0, 0, color.RGBA{255, 0, 0, 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	data, mt, err := compressImage(buf.Bytes(), "image/png")
	if err != nil {
		t.Fatalf("compressImage: %v", err)
	}
	if !bytes.Equal(data, buf.Bytes()) {
		t.Fatal("small image should pass through unchanged")
	}
	if mt != "image/png" {
		t.Fatalf("media type should remain image/png, got %q", mt)
	}
}

func TestCompressImage_OversizeNoisePNG_ConvertsToJPEG(t *testing.T) {
	raw := makeNoisePNG(t, 1800, 1800)
	data, mt, err := compressImage(raw, "image/png")
	if err != nil {
		t.Fatalf("compressImage: %v", err)
	}
	if mt != "image/jpeg" {
		t.Fatalf("oversize noise PNG should become JPEG, got %q", mt)
	}
	if encoded := base64.StdEncoding.EncodedLen(len(data)); encoded > client.MaxInlineImageBase64Bytes {
		t.Fatalf("encoded base64 length over inline limit: %d > %d",
			encoded, client.MaxInlineImageBase64Bytes)
	}
	if _, err := jpeg.Decode(bytes.NewReader(data)); err != nil {
		t.Fatalf("output not valid JPEG: %v", err)
	}
}

func TestCompressImage_HugeImage_UnderInlineLimit(t *testing.T) {
	raw := makeNoisePNG(t, 4000, 4000)
	data, mt, err := compressImage(raw, "image/png")
	if err != nil {
		t.Fatalf("compressImage: %v", err)
	}
	if mt != "image/jpeg" {
		t.Fatalf("expected JPEG, got %q", mt)
	}
	if encoded := base64.StdEncoding.EncodedLen(len(data)); encoded > client.MaxInlineImageBase64Bytes {
		t.Fatalf("encoded base64 still over limit: %d", encoded)
	}
	img, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("not valid JPEG: %v", err)
	}
	// Output must be at most CompressionMaxDimension on the longest edge.
	// Covers both primary-resize and fallback-resize cases (fallback is
	// smaller still).
	if img.Bounds().Dx() > CompressionMaxDimension {
		t.Fatalf("output not resized: %dx%d", img.Bounds().Dx(), img.Bounds().Dy())
	}
}

func TestCompressImage_DecodesByMagicNotExtension(t *testing.T) {
	// Use an oversize PNG with a lying media type. The function must ignore
	// the media-type hint and route to image.Decode (which sniffs magic
	// bytes), then re-encode as JPEG under the inline limit.
	raw := makeNoisePNG(t, 1800, 1800)
	data, mt, err := compressImage(raw, "application/octet-stream")
	if err != nil {
		t.Fatalf("compressImage with unknown media type errored: %v", err)
	}
	if mt != "image/jpeg" {
		t.Fatalf("expected JPEG output (decode-by-magic + re-encode), got %q", mt)
	}
	if encoded := base64.StdEncoding.EncodedLen(len(data)); encoded > client.MaxInlineImageBase64Bytes {
		t.Fatalf("encoded base64 over inline limit: %d", encoded)
	}
}

// TestCompressImage_IsDeterministic locks in the prompt-cache stability
// prerequisite: same input must produce byte-identical output, otherwise
// prompt-cache hash drift causes silent $0.10+/turn regressions.
func TestCompressImage_IsDeterministic(t *testing.T) {
	raw := makeNoisePNG(t, 1800, 1800)
	d1, mt1, err := compressImage(raw, "image/png")
	if err != nil {
		t.Fatalf("compressImage call 1: %v", err)
	}
	d2, mt2, err := compressImage(raw, "image/png")
	if err != nil {
		t.Fatalf("compressImage call 2: %v", err)
	}
	if mt1 != mt2 {
		t.Fatalf("media type drift: %q vs %q", mt1, mt2)
	}
	if !bytes.Equal(d1, d2) {
		t.Fatalf("non-deterministic output: len1=%d len2=%d", len(d1), len(d2))
	}
}
