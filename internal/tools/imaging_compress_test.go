package tools

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/gif"
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

// TestCompressImage_RejectsPixelBomb verifies the DecodeConfig pre-check
// blocks payloads whose declared dimensions would allocate too much RGBA
// memory. Without the guard, a header claiming W×H > MaxImagePixelBudget
// would let image.Decode commit ~W*H*4 bytes before downscaleToFit even
// runs. Uses a uniform Gray bitmap so PNG zlib compresses tightly and the
// fixture stays small on disk (under a few MB) while still claiming 9000×9000
// (81 MP > 64 MP budget) in its IHDR header.
func TestCompressImage_RejectsPixelBomb(t *testing.T) {
	const dim = 9000 // 9000×9000 = 81 MP > MaxImagePixelBudget (64 MP)
	img := image.NewGray(image.Rect(0, 0, dim, dim))
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode pixel-bomb fixture: %v", err)
	}
	// Note: a uniform Gray bitmap zlib-compresses to ~100 KB even at 9000×9000,
	// well under TargetRawImageBytes. This is exactly why the dimension guard
	// must run BEFORE the byte-size fast path — a pixel bomb does not have to
	// be a large byte payload.
	_, _, err := compressImage(buf.Bytes(), "image/png")
	if err == nil {
		t.Fatal("expected dimension-budget rejection for 9000×9000 PNG, got nil")
	}
	if !strings.Contains(err.Error(), "pixel budget") {
		t.Errorf("error should mention 'pixel budget', got: %v", err)
	}
}

// TestCompressImage_OversizeDimSmallBytes_GetsResized covers Anthropic's
// many-image 2000px constraint: when a single request contains >20 images,
// per-side limit drops from 8000 to 2000 px. A wide Retina screenshot (e.g.
// 2588×690 PNG of UI chrome) zlib-compresses well below TargetRawImageBytes
// (3.75 MB), so the legacy byte-only fast path passes it through unchanged
// and Anthropic returns 400. The dimension fast-path guard must trigger
// re-encode whenever max(W,H) > CompressionMaxDimension regardless of size.
func TestCompressImage_OversizeDimSmallBytes_GetsResized(t *testing.T) {
	// Uniform Gray PNG at 2588×690 — same dimensions as the production
	// screenshot that triggered the original 400. PNG compresses uniform
	// gray to a few KB, well under TargetRawImageBytes.
	img := image.NewGray(image.Rect(0, 0, 2588, 690))
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode fixture: %v", err)
	}
	if len(buf.Bytes()) > TargetRawImageBytes {
		t.Fatalf("fixture too big (%d > %d) — defeats test premise",
			len(buf.Bytes()), TargetRawImageBytes)
	}
	data, mt, err := compressImage(buf.Bytes(), "image/png")
	if err != nil {
		t.Fatalf("compressImage: %v", err)
	}
	// Production path: compressImage always re-encodes as JPEG when it
	// touches the slow path (downscaleToFit → encodeJPEG). Pin it.
	if mt != "image/jpeg" {
		t.Fatalf("oversize-dim path must return image/jpeg, got %q", mt)
	}
	out, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode output: %v", err)
	}
	if out.Bounds().Dx() > CompressionMaxDimension || out.Bounds().Dy() > CompressionMaxDimension {
		t.Fatalf("output not resized under %dpx: got %dx%d",
			CompressionMaxDimension, out.Bounds().Dx(), out.Bounds().Dy())
	}
}

// TestCompressImage_BoundaryDim2000_Passthrough verifies the dimension check
// uses strict `>` not `>=`. Anthropic's error message says "exceed max
// allowed size: 2000 pixels", and `downscaleToFit` is a no-op at exactly the
// limit. So a 2000×2000 image must NOT be re-encoded — passthrough preserves
// the original PNG bytes, keeping prompt cache byte-stable and avoiding a
// pointless JPEG re-encode at the boundary.
func TestCompressImage_BoundaryDim2000_Passthrough(t *testing.T) {
	img := image.NewGray(image.Rect(0, 0, CompressionMaxDimension, CompressionMaxDimension))
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode: %v", err)
	}
	if len(buf.Bytes()) > TargetRawImageBytes {
		t.Fatalf("fixture too big (%d > %d); test premise broken",
			len(buf.Bytes()), TargetRawImageBytes)
	}
	data, mt, err := compressImage(buf.Bytes(), "image/png")
	if err != nil {
		t.Fatalf("compressImage: %v", err)
	}
	if !bytes.Equal(data, buf.Bytes()) || mt != "image/png" {
		t.Fatalf("exactly 2000x2000 must passthrough unchanged; got %d bytes, mt=%q (input %d bytes)",
			len(data), mt, len(buf.Bytes()))
	}
}

// TestCompressImage_BoundaryDim2001_TriggersResize verifies one pixel above
// the cap forces re-encode, regardless of byte size.
func TestCompressImage_BoundaryDim2001_TriggersResize(t *testing.T) {
	img := image.NewGray(image.Rect(0, 0, CompressionMaxDimension+1, 100))
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode: %v", err)
	}
	if len(buf.Bytes()) > TargetRawImageBytes {
		t.Fatalf("fixture too big; test premise broken")
	}
	data, mt, err := compressImage(buf.Bytes(), "image/png")
	if err != nil {
		t.Fatalf("compressImage: %v", err)
	}
	if mt != "image/jpeg" {
		t.Fatalf("2001x100 must trigger JPEG re-encode, got %q", mt)
	}
	out, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode JPEG: %v", err)
	}
	if out.Bounds().Dx() > CompressionMaxDimension {
		t.Fatalf("output not resized: %d > %d", out.Bounds().Dx(), CompressionMaxDimension)
	}
}

// TestCompressImage_OversizeDimAndOversizeBytes covers the dual-violation
// case: 3024×1964 / 6.3 MB raw bytes — original 17:47-binary code already
// downscaled this via the byte path. The fix must preserve that behavior,
// not get into a redundant re-encode loop.
func TestCompressImage_OversizeDimAndOversizeBytes(t *testing.T) {
	raw := makeNoisePNG(t, 3024, 1964)
	if len(raw) <= TargetRawImageBytes {
		t.Fatalf("fixture must exceed byte cap; got %d", len(raw))
	}
	data, mt, err := compressImage(raw, "image/png")
	if err != nil {
		t.Fatalf("compressImage: %v", err)
	}
	if mt != "image/jpeg" {
		t.Fatalf("expected JPEG, got %q", mt)
	}
	out, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode JPEG: %v", err)
	}
	if out.Bounds().Dx() > CompressionMaxDimension || out.Bounds().Dy() > CompressionMaxDimension {
		t.Fatalf("output not within %dpx: %dx%d", CompressionMaxDimension, out.Bounds().Dx(), out.Bounds().Dy())
	}
	if encoded := base64.StdEncoding.EncodedLen(len(data)); encoded > client.MaxInlineImageBase64Bytes {
		t.Fatalf("encoded base64 over inline limit: %d", encoded)
	}
}

// TestCompressInlineImageSource_OversizeDimSmallBytes_GetsResized covers the
// same Anthropic many-image 2000px rule, but on the inline-image path
// (daemon.resolveContentBlocks → CompressInlineImageSource). Without a
// dimension check, a small-byte but wide screenshot pushed inline by
// cloud/Desktop bypasses compressImage entirely because the function
// short-circuits when base64 length is under MaxInlineImageBase64Bytes.
func TestCompressInlineImageSource_OversizeDimSmallBytes_GetsResized(t *testing.T) {
	img := image.NewGray(image.Rect(0, 0, 2588, 690))
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode fixture: %v", err)
	}
	encoded := base64.StdEncoding.EncodeToString(buf.Bytes())
	if len(encoded) > client.MaxInlineImageBase64Bytes {
		t.Fatalf("fixture too big for test premise: %d > %d",
			len(encoded), client.MaxInlineImageBase64Bytes)
	}
	src := &client.ImageSource{Type: "base64", MediaType: "image/png", Data: encoded}
	out := CompressInlineImageSource(src)
	if out == src {
		t.Fatal("expected re-encoded source (different pointer) for 2588×690 input")
	}
	raw, err := base64.StdEncoding.DecodeString(out.Data)
	if err != nil {
		t.Fatalf("decode b64 output: %v", err)
	}
	if out.MediaType != "image/jpeg" {
		t.Fatalf("oversize-dim inline path must produce image/jpeg, got %q", out.MediaType)
	}
	decoded, err := jpeg.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("decode JPEG: %v", err)
	}
	if decoded.Bounds().Dx() > CompressionMaxDimension || decoded.Bounds().Dy() > CompressionMaxDimension {
		t.Fatalf("inline source not resized under %dpx: got %dx%d",
			CompressionMaxDimension, decoded.Bounds().Dx(), decoded.Bounds().Dy())
	}
}

// makeOversizeGIF builds a wide single-frame GIF that exceeds
// CompressionMaxDimension on one edge but stays small in bytes — the LZW
// encoder packs uniform-palette pixels very tightly. Used to lock in the
// contract that GIF headers (Logical Screen Descriptor) parse correctly
// through inlineSourceOversizeDim and that compressImage forces re-encode
// on the oversize-dim path for GIF input.
func makeOversizeGIF(t *testing.T) []byte {
	t.Helper()
	pal := []color.Color{color.RGBA{0, 0, 0, 255}, color.RGBA{255, 255, 255, 255}}
	img := image.NewPaletted(image.Rect(0, 0, 2100, 50), pal)
	var buf bytes.Buffer
	if err := gif.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestInlineSourceOversizeDim_FormatMatrix locks in the multi-format
// contract documented on inlineSourceOversizeDim: PNG / GIF / WebP all
// route through image.DecodeConfig via the package-level decoder
// registrations (image/png, image/gif, golang.org/x/image/webp). A
// future Go upgrade or vendored-dep refresh that drops one of those
// registrations would otherwise silently regress to "always returns
// false" → oversize-dim images leak through to the wire.
//
// WebP fixtures are pre-computed because Go's standard library + the
// x/image WebP package are decode-only. Both were produced by `cwebp
// -lossless` against a uniform-gray PNG of the stated dimensions, so
// the entire payload is under 60 bytes — safe to inline.
func TestInlineSourceOversizeDim_FormatMatrix(t *testing.T) {
	cases := []struct {
		name      string
		makeRaw   func(t *testing.T) []byte
		wantTrip  bool
		mediaHint string
	}{
		{
			name:      "png_2588x690_trip",
			makeRaw:   func(t *testing.T) []byte { return makeOversizeGrayPNG(t, 2588, 690) },
			wantTrip:  true,
			mediaHint: "image/png",
		},
		{
			name:      "png_1500x1000_quiet",
			makeRaw:   func(t *testing.T) []byte { return makeOversizeGrayPNG(t, 1500, 1000) },
			wantTrip:  false,
			mediaHint: "image/png",
		},
		{
			name:      "gif_2100x50_trip",
			makeRaw:   makeOversizeGIF,
			wantTrip:  true,
			mediaHint: "image/gif",
		},
		{
			name: "webp_2001x100_trip",
			makeRaw: func(t *testing.T) []byte {
				raw, err := base64.StdEncoding.DecodeString(webpFixture2001x100)
				if err != nil {
					t.Fatal(err)
				}
				return raw
			},
			wantTrip:  true,
			mediaHint: "image/webp",
		},
		{
			name: "webp_200x100_quiet",
			makeRaw: func(t *testing.T) []byte {
				raw, err := base64.StdEncoding.DecodeString(webpFixture200x100)
				if err != nil {
					t.Fatal(err)
				}
				return raw
			},
			wantTrip:  false,
			mediaHint: "image/webp",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			raw := c.makeRaw(t)
			b64 := base64.StdEncoding.EncodeToString(raw)
			got := inlineSourceOversizeDim(b64)
			if got != c.wantTrip {
				t.Fatalf("%s: inlineSourceOversizeDim returned %v, want %v", c.name, got, c.wantTrip)
			}
		})
	}
}

// makeOversizeGrayPNG returns a uniform-gray PNG of given dimensions —
// small bytes thanks to PNG zlib RLE.
func makeOversizeGrayPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewGray(image.Rect(0, 0, w, h))
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// webpFixture* are minimal lossless WebP files generated by
// `cwebp -lossless` against a uniform-gray PNG of the named dimensions.
// Encoded fully here so the tests have no external dependency. Verified
// by image.DecodeConfig returning the documented dimensions.
const (
	webpFixture2001x100 = "UklGRiwAAABXRUJQVlA4TCAAAAAv0McYAAcQEf0PKGjbhil/+N1xRP8z/Oc///nPf/4XAg=="
	webpFixture200x100  = "UklGRiAAAABXRUJQVlA4TBQAAAAvx8AYAAcQEf0PACjS//8S0f9UOA=="
)
