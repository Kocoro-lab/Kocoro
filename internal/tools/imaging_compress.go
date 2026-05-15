package tools

import (
	"bytes"
	"fmt"
	"image"
	"image/jpeg"

	_ "image/gif" // register GIF decoder
	_ "image/png" // register PNG decoder

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp" // register WebP decoder
)

// MaxImagePixelBudget is the upper bound on Width*Height accepted by
// compressImage before any pixel buffer is allocated. 64 MP matches
// Anthropic's documented single-image dimension cap (8000×8000) — anything
// larger would be rejected upstream anyway. Without this guard, a 30 MB PNG
// header claiming 100000×100000 px would allocate ~40 GB of RGBA inside
// image.Decode before downscaleToFit ever runs. The DoS surface is real
// because CompressInlineImageSource accepts cloud-pushed inline base64.
const MaxImagePixelBudget = 64_000_000

// compressImage takes raw image bytes and a media-type hint and returns either
// the same bytes (when ≤ TargetRawImageBytes) or a JPEG-compressed version
// that base64-encodes under client.MaxInlineImageBase64Bytes.
//
// Output mediaType is "image/jpeg" when conversion happened; otherwise input
// mediaType is returned unchanged. Errors when decode fails, dimensions
// exceed MaxImagePixelBudget, or every fallback overshoots.
func compressImage(data []byte, mediaType string) ([]byte, string, error) {
	// Header-only dimension guard runs BEFORE the byte-size fast path. A
	// uniform-color PNG of 100000×100000 px compresses to a few hundred KB
	// (zlib RLE-style) — it would slide through the byte threshold and only
	// blow up at image.Decode allocating ~40 GB of RGBA. DecodeConfig parses
	// IHDR / VP8 / GIF header only, no pixel buffer commit. When DecodeConfig
	// fails (corrupt header or unrecognized format) we fall through; the
	// fast path preserves the legacy "passthrough invalid bytes" behavior
	// downstream callers may rely on, and the slow path's image.Decode
	// produces a clean decode error.
	//
	// oversizeDim trips when max(W,H) > CompressionMaxDimension. Anthropic
	// applies a 2000px per-side limit when a single request carries >20
	// images ("many-image requests"), separate from the 8000px single-image
	// limit. Wide PNG screenshots compress small (a 2588×690 screenshot of
	// UI chrome zlibs to ~600 KB) so the byte fast path alone lets them
	// reach the wire and earn a 400. The dimension flag forces them through
	// downscaleToFit even when bytes are tiny.
	oversizeDim := false
	if cfg, _, cfgErr := image.DecodeConfig(bytes.NewReader(data)); cfgErr == nil {
		if int64(cfg.Width)*int64(cfg.Height) > MaxImagePixelBudget {
			return nil, "", fmt.Errorf(
				"image dimensions exceed pixel budget: %d×%d (limit %d px)",
				cfg.Width, cfg.Height, MaxImagePixelBudget,
			)
		}
		oversizeDim = cfg.Width > CompressionMaxDimension || cfg.Height > CompressionMaxDimension
	}

	if !oversizeDim && len(data) <= TargetRawImageBytes {
		return data, mediaType, nil
	}

	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, "", fmt.Errorf("decode image: %w", err)
	}

	img = downscaleToFit(img, CompressionMaxDimension)

	for _, q := range []int{80, 60, 40, 20} {
		out, err := encodeJPEG(img, q)
		if err != nil {
			return nil, "", err
		}
		if len(out) <= TargetRawImageBytes {
			return out, "image/jpeg", nil
		}
	}

	img = downscaleToFit(img, CompressionFallbackDimension)
	out, err := encodeJPEG(img, 20)
	if err != nil {
		return nil, "", err
	}
	if len(out) > TargetRawImageBytes {
		return nil, "", fmt.Errorf("image too large after fallback compression: %d bytes", len(out))
	}
	return out, "image/jpeg", nil
}

func downscaleToFit(img image.Image, max int) image.Image {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= max && h <= max {
		return img
	}
	var newW, newH int
	if w > h {
		newW = max
		newH = h * max / w
	} else {
		newH = max
		newW = w * max / h
	}
	if newW < 1 {
		newW = 1
	}
	if newH < 1 {
		newH = 1
	}
	dst := image.NewRGBA(image.Rect(0, 0, newW, newH))
	draw.CatmullRom.Scale(dst, dst.Bounds(), img, b, draw.Over, nil)
	return dst
}

func encodeJPEG(img image.Image, quality int) ([]byte, error) {
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality}); err != nil {
		return nil, fmt.Errorf("encode jpeg q=%d: %w", quality, err)
	}
	return buf.Bytes(), nil
}
