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

// compressImage takes raw image bytes and a media-type hint and returns either
// the same bytes (when ≤ TargetRawImageBytes) or a JPEG-compressed version
// that base64-encodes under client.MaxInlineImageBase64Bytes.
//
// Output mediaType is "image/jpeg" when conversion happened; otherwise input
// mediaType is returned unchanged. Errors only when decode fails or every
// fallback overshoots.
func compressImage(data []byte, mediaType string) ([]byte, string, error) {
	if len(data) <= TargetRawImageBytes {
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
