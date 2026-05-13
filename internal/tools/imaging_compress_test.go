package tools

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"strings"
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

func TestCompressInlineImageSource_OversizePassesThroughCompression(t *testing.T) {
	raw := makeNoisePNG(t, 1800, 1800)
	encoded := base64.StdEncoding.EncodeToString(raw)
	if len(encoded) <= client.MaxInlineImageBase64Bytes {
		t.Fatalf("test fixture must exceed inline limit; encoded len=%d", len(encoded))
	}
	src := &client.ImageSource{Type: "base64", MediaType: "image/png", Data: encoded}
	out := CompressInlineImageSource(src)
	if out == src {
		t.Fatal("expected new source after compression")
	}
	if len(out.Data) > client.MaxInlineImageBase64Bytes {
		t.Fatalf("compressed inline source still over limit: %d", len(out.Data))
	}
	if out.MediaType != "image/jpeg" {
		t.Fatalf("expected media type image/jpeg after compression, got %q", out.MediaType)
	}
}

func TestCompressInlineImageSource_SmallPassesThroughUntouched(t *testing.T) {
	encoded := base64.StdEncoding.EncodeToString([]byte("tiny"))
	src := &client.ImageSource{Type: "base64", MediaType: "image/png", Data: encoded}
	out := CompressInlineImageSource(src)
	if out != src {
		t.Fatal("small source should be returned unchanged (same pointer)")
	}
}

func TestCompressInlineImageSource_TooLargeInputReturnsUnchangedSrc(t *testing.T) {
	// Input larger than MaxInlineBase64InputBytes (30 MB) should be returned
	// unchanged (same pointer) — the function refuses to allocate for it.
	// Layer 2 (filterOversizeImages) will replace it with a placeholder downstream.
	huge := strings.Repeat("A", MaxInlineBase64InputBytes+1)
	src := &client.ImageSource{Type: "base64", MediaType: "image/png", Data: huge}
	out := CompressInlineImageSource(src)
	if out != src {
		t.Fatal("input over MaxInlineBase64InputBytes should return src unchanged (same pointer)")
	}
}

func TestCompressInlineImageSource_GarbageBase64ReturnsUnchangedSrc(t *testing.T) {
	// base64 decode failure → return src unchanged. Caller (Layer 2) gets the
	// chance to wipe with a placeholder.
	garbage := strings.Repeat("@", client.MaxInlineImageBase64Bytes+10) // not valid base64
	src := &client.ImageSource{Type: "base64", MediaType: "image/png", Data: garbage}
	out := CompressInlineImageSource(src)
	if out != src {
		t.Fatal("undecodable base64 should return src unchanged (same pointer)")
	}
}

func TestCompressInlineImageSource_UndecodableImageReturnsUnchangedSrc(t *testing.T) {
	// base64 decodes fine but the bytes are not a recognized image format →
	// compressImage fails, function returns src unchanged.
	// Raw size must produce base64 encoding > MaxInlineImageBase64Bytes (5 MB)
	// to bypass the fast path, but stay under MaxInlineBase64InputBytes (30 MB)
	// so we reach the actual decode/compress branch.
	notAnImage := bytes.Repeat([]byte{0xFF}, client.MaxInlineImageBase64Bytes)
	encoded := base64.StdEncoding.EncodeToString(notAnImage)
	if len(encoded) <= client.MaxInlineImageBase64Bytes {
		t.Fatalf("fixture must exceed inline cap to enter the compression branch; len=%d", len(encoded))
	}
	src := &client.ImageSource{Type: "base64", MediaType: "image/png", Data: encoded}
	out := CompressInlineImageSource(src)
	if out != src {
		t.Fatal("undecodable image bytes should return src unchanged (same pointer)")
	}
}
