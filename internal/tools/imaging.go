package tools

import (
	"encoding/base64"
	"fmt"
	"log"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

const (
	DefaultAPIWidth  = 1280
	DefaultAPIHeight = 800

	// TargetRawImageBytes is the raw-bytes ceiling we aim for before base64.
	// Base64 inflates by 4/3, so 3.75 MB raw → 5 MB encoded. We leave 4 KB of
	// headroom under client.MaxInlineImageBase64Bytes because Anthropic's
	// boundary check is `> 5242880 bytes` and the exact-equal case has been
	// observed to fail on whitespace/padding edge cases.
	TargetRawImageBytes = (5*1024*1024 - 4096) * 3 / 4 // 3,929,088 → base64 ≈ 5,238,784

	// CompressionMaxDimension caps the longest edge after first-pass resize.
	CompressionMaxDimension = 2000

	// CompressionFallbackDimension kicks in if the JPEG quality ladder can't
	// reach TargetRawImageBytes at CompressionMaxDimension.
	CompressionFallbackDimension = 1000
)

// EncodeImage reads an image file and returns it as a base64-encoded ImageBlock.
// If the file's raw bytes exceed TargetRawImageBytes, it's recompressed
// (decode → resize → JPEG quality ladder) so the base64 output fits under
// client.MaxInlineImageBase64Bytes.
func EncodeImage(path string) (agent.ImageBlock, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return agent.ImageBlock{}, err
	}

	mediaType := "image/png"
	lower := strings.ToLower(path)
	switch {
	case strings.HasSuffix(lower, ".jpg"), strings.HasSuffix(lower, ".jpeg"):
		mediaType = "image/jpeg"
	case strings.HasSuffix(lower, ".gif"):
		mediaType = "image/gif"
	case strings.HasSuffix(lower, ".webp"):
		mediaType = "image/webp"
	}

	compressed, outMediaType, err := compressImage(data, mediaType)
	if err != nil {
		return agent.ImageBlock{}, fmt.Errorf("compress image %s: %w", path, err)
	}

	return agent.ImageBlock{
		MediaType: outMediaType,
		Data:      base64.StdEncoding.EncodeToString(compressed),
	}, nil
}

// EncodeImageBytes is like EncodeImage but takes the bytes directly instead of
// reading from a file path. Used by attachment paths where the bytes are
// already in memory. mediaType is the source format hint; the output may be
// different ("image/jpeg") if compression triggered.
func EncodeImageBytes(data []byte, mediaType string) (agent.ImageBlock, error) {
	compressed, outMediaType, err := compressImage(data, mediaType)
	if err != nil {
		return agent.ImageBlock{}, fmt.Errorf("compress image: %w", err)
	}
	return agent.ImageBlock{
		MediaType: outMediaType,
		Data:      base64.StdEncoding.EncodeToString(compressed),
	}, nil
}

// MaxInlineBase64InputBytes guards CompressInlineImageSource against very
// large base64 inputs from cloud/Desktop. Without this, a 50 MB base64 string
// would allocate ~37 MB just to decode before we discover the image is
// undecodable. The wire-time sanitizer (Layer 2) will replace anything over
// the inline cap with a placeholder anyway, so failing fast here is safe.
const MaxInlineBase64InputBytes = 30 * 1024 * 1024

// CompressInlineImageSource takes an already-base64-encoded image block source
// and returns either the same source (if under the inline cap) or a recompressed
// one. Used by `daemon.resolveContentBlocks` so cloud/Desktop pushing inline
// image content blocks doesn't bypass Layer 1.
//
// If decoding fails (corrupt base64 or undecodable image), or if the input
// exceeds MaxInlineBase64InputBytes, the original source is returned unchanged
// — the wire-time sanitizer (Layer 2) will replace it with a text placeholder
// if it's still oversize. Failures log a warning so silent oversize-image
// drops are diagnosable.
func CompressInlineImageSource(src *client.ImageSource) *client.ImageSource {
	if src == nil {
		return src
	}
	// Fast path: under the inline cap, nothing to do.
	if len(src.Data) <= client.MaxInlineImageBase64Bytes {
		return src
	}
	// Pre-decode size guard: refuse to allocate ~37 MB for an obvious garbage
	// payload. Log once so the abuse / bug is visible in audit-time triage.
	if len(src.Data) > MaxInlineBase64InputBytes {
		log.Printf("WARNING: CompressInlineImageSource: input base64 too large (%d bytes), skipping compression", len(src.Data))
		return src
	}
	raw, err := base64.StdEncoding.DecodeString(src.Data)
	if err != nil {
		log.Printf("WARNING: CompressInlineImageSource: base64 decode failed: %v", err)
		return src
	}
	compressed, mt, err := compressImage(raw, src.MediaType)
	if err != nil {
		log.Printf("WARNING: CompressInlineImageSource: compressImage failed: %v", err)
		return src
	}
	return &client.ImageSource{
		Type:      src.Type,
		MediaType: mt,
		Data:      base64.StdEncoding.EncodeToString(compressed),
	}
}

// ResizeImage resizes an image so its longest edge is at most maxDim pixels.
// Uses macOS sips command.
func ResizeImage(path string, maxDim int) error {
	out, err := exec.Command("sips", "--resampleHeightWidthMax", strconv.Itoa(maxDim), path).CombinedOutput()
	if err != nil {
		return fmt.Errorf("sips resize: %v\n%s", err, string(out))
	}
	return nil
}

// CaptureAndEncode takes a fullscreen screenshot (-x flag for no sound), resizes, and base64-encodes.
// Returns the file path and encoded image block.
func CaptureAndEncode(maxDim int) (string, agent.ImageBlock, error) {
	f, err := os.CreateTemp("", "shannon-capture-*.png")
	if err != nil {
		return "", agent.ImageBlock{}, fmt.Errorf("create temp file: %v", err)
	}
	path := f.Name()
	f.Close()

	out, err := exec.Command("screencapture", "-x", path).CombinedOutput()
	if err != nil {
		os.Remove(path)
		return "", agent.ImageBlock{}, fmt.Errorf("screencapture: %v\n%s", err, string(out))
	}

	if maxDim > 0 {
		if err := ResizeImage(path, maxDim); err != nil {
			os.Remove(path)
			return "", agent.ImageBlock{}, err
		}
	}

	block, err := EncodeImage(path)
	if err != nil {
		os.Remove(path)
		return "", agent.ImageBlock{}, err
	}

	return path, block, nil
}

// GetScreenDimensions returns the logical screen dimensions (points, not physical pixels)
// of the main display. Uses Quartz CGDisplayPixelsWide/High which returns the coordinate
// space that CGEvent mouse clicks operate in. Falls back to system_profiler parsing.
func GetScreenDimensions() (width, height int, err error) {
	// Primary: Quartz CGDisplayPixelsWide/High — returns logical points (what CGEvent uses)
	out, err := exec.Command("python3", "-c",
		`import Quartz; d=Quartz.CGMainDisplayID(); print(Quartz.CGDisplayPixelsWide(d), Quartz.CGDisplayPixelsHigh(d))`).CombinedOutput()
	if err == nil {
		var w, h int
		if _, parseErr := fmt.Sscanf(strings.TrimSpace(string(out)), "%d %d", &w, &h); parseErr == nil && w > 0 && h > 0 {
			return w, h, nil
		}
	}

	// Fallback: system_profiler (may return physical pixels on Retina without "UI Looks like:")
	out, err = exec.Command("system_profiler", "SPDisplaysDataType").CombinedOutput()
	if err != nil {
		return 0, 0, fmt.Errorf("screen dimensions: %v", err)
	}
	return parseScreenDimensions(string(out))
}

// resolutionRe matches "WxH" or "W x H" with optional surrounding text.
var resolutionRe = regexp.MustCompile(`(\d+)\s*x\s*(\d+)`)

func parseScreenDimensions(output string) (int, int, error) {
	// Prefer "UI Looks like:" (logical resolution on Retina) over raw "Resolution:".
	for line := range strings.SplitSeq(output, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "UI Looks like:") {
			m := resolutionRe.FindStringSubmatch(trimmed)
			if m != nil {
				w, _ := strconv.Atoi(m[1])
				h, _ := strconv.Atoi(m[2])
				return w, h, nil
			}
		}
	}

	// Fall back to "Resolution:" line.
	for line := range strings.SplitSeq(output, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "Resolution:") {
			m := resolutionRe.FindStringSubmatch(trimmed)
			if m != nil {
				w, _ := strconv.Atoi(m[1])
				h, _ := strconv.Atoi(m[2])
				return w, h, nil
			}
		}
	}

	return 0, 0, fmt.Errorf("no display resolution found in system_profiler output")
}

// ScaleCoordinates maps coordinates from API space to logical screen space.
func ScaleCoordinates(apiX, apiY, apiW, apiH, screenW, screenH int) (int, int) {
	x := apiX * screenW / apiW
	y := apiY * screenH / apiH
	return x, y
}

// ClampCoordinates ensures coordinates are within display bounds (0 to max-1).
func ClampCoordinates(x, y, maxW, maxH int) (int, int) {
	if x < 0 {
		x = 0
	}
	if y < 0 {
		y = 0
	}
	if x >= maxW {
		x = maxW - 1
	}
	if y >= maxH {
		y = maxH - 1
	}
	return x, y
}
