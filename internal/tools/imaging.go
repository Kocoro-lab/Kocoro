package tools

import (
	"context"
	"encoding/base64"
	"fmt"
	"image"
	"log"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

type screenGeometry struct {
	LogicalWidth  int
	LogicalHeight int
	CaptureWidth  int
	CaptureHeight int
}

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
	// Pre-decode size guard: refuse to allocate ~37 MB for an obvious garbage
	// payload. Log once so the abuse / bug is visible in audit-time triage.
	if len(src.Data) > MaxInlineBase64InputBytes {
		log.Printf("WARNING: CompressInlineImageSource: input base64 too large (%d bytes), skipping compression", len(src.Data))
		return src
	}
	// Fast path: skip decode only when under the inline byte cap AND under
	// the dimension cap. Wide PNG screenshots (e.g. 2588×690 Retina chrome)
	// compress to a few hundred KB but still exceed Anthropic's many-image
	// 2000px per-side limit, so a pure byte check lets them slip through to
	// the wire. Streaming-decode the base64 reader through DecodeConfig
	// (header-only) so we only pay decode cost for the few header bytes.
	if len(src.Data) <= client.MaxInlineImageBase64Bytes && !inlineSourceOversizeDim(src.Data) {
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

// inlineSourceOversizeDim returns true when the base64 payload's image header
// reports either edge above CompressionMaxDimension. Streams the base64 bytes
// through image.DecodeConfig, which reads only the header (PNG IHDR / WebP
// VP8 / JPEG SOFn / GIF LSD) — typically tens of bytes — so the decode cost
// stays bounded even for the 5 MB-base64 fast-path case. Returns false on
// any decode error: a malformed payload is the byte-cap path's problem and
// will surface at compressImage's image.Decode call. Image registrations
// (png/gif/jpeg/webp) are inherited from imaging_compress.go.
func inlineSourceOversizeDim(b64 string) bool {
	reader := base64.NewDecoder(base64.StdEncoding, strings.NewReader(b64))
	cfg, _, err := image.DecodeConfig(reader)
	if err != nil {
		return false
	}
	return cfg.Width > CompressionMaxDimension || cfg.Height > CompressionMaxDimension
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

func resizeImageContext(ctx context.Context, path string, maxDim int) error {
	return runBoundedLegacyImageResizeCommand(
		ctx,
		"sips",
		[]string{"--resampleHeightWidthMax", strconv.Itoa(maxDim), path},
		8*time.Second)
}

func imageFileDimensions(path string) (int, int, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, 0, err
	}
	defer file.Close()
	config, _, err := image.DecodeConfig(file)
	if err != nil {
		return 0, 0, err
	}
	if config.Width <= 0 || config.Height <= 0 {
		return 0, 0, fmt.Errorf("image has invalid dimensions")
	}
	return config.Width, config.Height, nil
}

func imageBlockDimensions(block agent.ImageBlock) (int, int, error) {
	if block.Data == "" {
		return 0, 0, fmt.Errorf("image block has no data")
	}
	config, _, err := image.DecodeConfig(base64.NewDecoder(
		base64.StdEncoding, strings.NewReader(block.Data)))
	if err != nil {
		return 0, 0, err
	}
	if config.Width <= 0 || config.Height <= 0 {
		return 0, 0, fmt.Errorf("image block has invalid dimensions")
	}
	return config.Width, config.Height, nil
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

// legacyMainDisplayCaptureArgs is intentionally separate from
// CaptureAndEncode. The latter is shared by AppleScript, Ghostty, browser
// fallbacks and the standalone screenshot tool; changing its fullscreen
// semantics would widen the regression surface. Legacy function computer uses
// -m so one declared tool canvas is always backed by exactly the main display.
func legacyMainDisplayCaptureArgs(output string) []string {
	return []string{"-x", "-m", output}
}

func runLegacyMainDisplayCapture(output string) error {
	return runBoundedLegacyMainDisplayCapture(
		"screencapture", legacyMainDisplayCaptureArgs(output), 8*time.Second)
}

func runLegacyMainDisplayCaptureContext(ctx context.Context, output string) error {
	return runBoundedLegacyMainDisplayCaptureContext(
		ctx, "screencapture", legacyMainDisplayCaptureArgs(output), 8*time.Second)
}

func runBoundedLegacyMainDisplayCapture(executable string, args []string, timeout time.Duration) error {
	return runBoundedLegacyMainDisplayCaptureContext(
		context.Background(), executable, args, timeout)
}

func runBoundedLegacyMainDisplayCaptureContext(
	parent context.Context,
	executable string,
	args []string,
	timeout time.Duration,
) error {
	return runBoundedLegacyCommand(parent, "main display capture", executable, args, timeout)
}

func runBoundedLegacyImageResizeCommand(
	parent context.Context,
	executable string,
	args []string,
	timeout time.Duration,
) error {
	return runBoundedLegacyCommand(parent, "main display image resize", executable, args, timeout)
}

func runBoundedLegacyCommand(
	parent context.Context,
	operation, executable string,
	args []string,
	timeout time.Duration,
) error {
	if parent == nil {
		return fmt.Errorf("%s context is required", operation)
	}
	if executable == "" || timeout <= 0 {
		return fmt.Errorf("%s command and positive timeout are required", operation)
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, args...)
	// If an unexpected descendant inherits stdout/stderr, do not let pipe
	// closure extend the capture timeout indefinitely after the direct process
	// has been killed. CombinedOutput still calls Wait, so the child is reaped
	// before this function returns.
	cmd.WaitDelay = 500 * time.Millisecond
	out, err := cmd.CombinedOutput()
	if parent.Err() != nil {
		return fmt.Errorf("%s canceled: %w", operation, parent.Err())
	}
	if ctx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("%s timed out after %s", operation, timeout)
	}
	if err != nil {
		return fmt.Errorf("%s: %v\n%s", operation, err, string(out))
	}
	return nil
}

// CaptureMainDisplayAndEncode is the legacy computer tool's dedicated capture
// path. A private directory contains every filename screencapture may emit, so
// unexpected multi-display sibling files are detected, rejected, and removed.
// The one admitted image is moved to a standalone temporary path before the
// scratch directory is deleted.
func CaptureMainDisplayAndEncode(maxDim int) (string, agent.ImageBlock, error) {
	return captureMainDisplayAndEncode(maxDim, runLegacyMainDisplayCapture)
}

func CaptureMainDisplayAndEncodeContext(
	ctx context.Context,
	maxDim int,
) (string, agent.ImageBlock, error) {
	return captureMainDisplayAndEncodeContext(
		ctx, maxDim, runLegacyMainDisplayCaptureContext, resizeImageContext)
}

func captureMainDisplayAndEncode(
	maxDim int,
	capture func(output string) error,
) (string, agent.ImageBlock, error) {
	if capture == nil {
		return "", agent.ImageBlock{}, fmt.Errorf("main display capture runner is required")
	}
	return captureMainDisplayAndEncodeContext(
		context.Background(),
		maxDim,
		func(_ context.Context, output string) error { return capture(output) },
		func(_ context.Context, path string, max int) error { return ResizeImage(path, max) })
}

func captureMainDisplayAndEncodeContext(
	ctx context.Context,
	maxDim int,
	capture func(context.Context, string) error,
	resize func(context.Context, string, int) error,
) (string, agent.ImageBlock, error) {
	if ctx == nil || capture == nil || resize == nil {
		return "", agent.ImageBlock{}, fmt.Errorf("main display capture context, runner, and resizer are required")
	}
	scratch, err := os.MkdirTemp("", "kocoro-legacy-main-capture-*")
	if err != nil {
		return "", agent.ImageBlock{}, fmt.Errorf("create main display capture directory: %w", err)
	}
	defer os.RemoveAll(scratch)

	output := filepath.Join(scratch, "main.png")
	if err := capture(ctx, output); err != nil {
		return "", agent.ImageBlock{}, err
	}
	if err := ctx.Err(); err != nil {
		return "", agent.ImageBlock{}, err
	}
	entries, err := os.ReadDir(scratch)
	if err != nil {
		return "", agent.ImageBlock{}, fmt.Errorf("inspect main display capture: %w", err)
	}
	if len(entries) != 1 || entries[0].Name() != "main.png" || !entries[0].Type().IsRegular() {
		return "", agent.ImageBlock{}, fmt.Errorf(
			"main display capture produced %d outputs; expected exactly main.png", len(entries))
	}
	if maxDim > 0 {
		if err := resize(ctx, output, maxDim); err != nil {
			return "", agent.ImageBlock{}, err
		}
	}
	if err := ctx.Err(); err != nil {
		return "", agent.ImageBlock{}, err
	}

	finalFile, err := os.CreateTemp("", "kocoro-legacy-main-*.png")
	if err != nil {
		return "", agent.ImageBlock{}, fmt.Errorf("create main display image: %w", err)
	}
	finalPath := finalFile.Name()
	keepFinal := false
	defer func() {
		if !keepFinal {
			_ = os.Remove(finalPath)
		}
	}()
	if err := finalFile.Close(); err != nil {
		return "", agent.ImageBlock{}, fmt.Errorf("close main display image: %w", err)
	}
	if err := os.Remove(finalPath); err != nil {
		return "", agent.ImageBlock{}, fmt.Errorf("prepare main display image: %w", err)
	}
	if err := os.Rename(output, finalPath); err != nil {
		return "", agent.ImageBlock{}, fmt.Errorf("retain main display image: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return "", agent.ImageBlock{}, err
	}

	block, err := EncodeImage(finalPath)
	if err != nil {
		return "", agent.ImageBlock{}, err
	}
	keepFinal = true
	return finalPath, block, nil
}

// GetMainScreenGeometry binds the legacy function computer tool's coordinate
// image space to the screenshot pixels that screencapture will produce while
// preserving the logical-point space consumed by CGEvent. Keeping both values
// prevents a 16:9 image from being interpreted as an old fixed 1280x800 canvas.
func GetMainScreenGeometry() (screenGeometry, error) {
	// NSScreen.screens[0] is the primary display (the one with the menu bar),
	// matching screencapture -m. NSScreen.mainScreen can instead follow the key
	// window onto another monitor and silently break the declared tool canvas.
	//
	// Read capture pixels from CoreGraphics instead of deriving them from
	// frame * backingScaleFactor. Virtual displays can report a logical
	// 1024x768 frame with scale 1 while screencapture emits 1280x960; binding
	// the declared tool space to CGDisplayPixelsWide/High keeps the screenshot
	// bytes and coordinate contract exact on those runners too.
	const script = `ObjC.import("AppKit"); ObjC.import("CoreGraphics"); var s=$.NSScreen.screens.objectAtIndex(0); var f=s.frame; var n=s.deviceDescription.objectForKey("NSScreenNumber"); var d=Number(n.unsignedIntValue); console.log(Number(f.size.width)+" "+Number(f.size.height)+" "+Number($.CGDisplayPixelsWide(d))+" "+Number($.CGDisplayPixelsHigh(d)))`
	out, err := exec.Command("/usr/bin/osascript", "-l", "JavaScript", "-e", script).CombinedOutput()
	if err == nil {
		if geometry, parseErr := parseNSScreenGeometry(string(out)); parseErr == nil {
			return geometry, nil
		}
	}

	// Fallback for restricted/minimal environments where osascript is unavailable.
	out, err = exec.Command("system_profiler", "SPDisplaysDataType").CombinedOutput()
	if err != nil {
		return screenGeometry{}, fmt.Errorf("screen dimensions: %v", err)
	}
	return parseScreenGeometry(string(out))
}

func parseNSScreenGeometry(output string) (screenGeometry, error) {
	var logicalWidth, logicalHeight, captureWidth, captureHeight int
	if _, err := fmt.Sscanf(
		strings.TrimSpace(output),
		"%d %d %d %d",
		&logicalWidth,
		&logicalHeight,
		&captureWidth,
		&captureHeight,
	); err != nil {
		return screenGeometry{}, fmt.Errorf("parse NSScreen geometry: %w", err)
	}
	if logicalWidth <= 0 || logicalHeight <= 0 ||
		captureWidth <= 0 || captureHeight <= 0 {
		return screenGeometry{}, fmt.Errorf("parse NSScreen geometry: invalid dimensions")
	}
	return screenGeometry{
		LogicalWidth:  logicalWidth,
		LogicalHeight: logicalHeight,
		CaptureWidth:  captureWidth,
		CaptureHeight: captureHeight,
	}, nil
}

// GetScreenDimensions returns the main display's logical point dimensions.
func GetScreenDimensions() (width, height int, err error) {
	geometry, err := GetMainScreenGeometry()
	if err != nil {
		return 0, 0, err
	}
	return geometry.LogicalWidth, geometry.LogicalHeight, nil
}

// resolutionRe matches "WxH" or "W x H" with optional surrounding text.
var resolutionRe = regexp.MustCompile(`(\d+)\s*x\s*(\d+)`)

func parseScreenDimensions(output string) (int, int, error) {
	geometry, err := parseScreenGeometry(output)
	if err != nil {
		return 0, 0, err
	}
	return geometry.LogicalWidth, geometry.LogicalHeight, nil
}

func parseScreenGeometry(output string) (screenGeometry, error) {
	type candidate struct {
		geometry screenGeometry
		main     bool
	}
	var candidates []candidate
	var current *candidate
	flush := func() {
		if current != nil && current.geometry.CaptureWidth > 0 && current.geometry.CaptureHeight > 0 {
			candidates = append(candidates, *current)
		}
		current = nil
	}

	for line := range strings.SplitSeq(output, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "Resolution:"):
			flush()
			match := resolutionRe.FindStringSubmatch(trimmed)
			if match == nil {
				continue
			}
			physicalWidth, _ := strconv.Atoi(match[1])
			physicalHeight, _ := strconv.Atoi(match[2])
			logicalWidth, logicalHeight := physicalWidth, physicalHeight
			if strings.Contains(strings.ToLower(trimmed), "retina") {
				logicalWidth /= 2
				logicalHeight /= 2
			}
			current = &candidate{geometry: screenGeometry{
				LogicalWidth: logicalWidth, LogicalHeight: logicalHeight,
				CaptureWidth: physicalWidth, CaptureHeight: physicalHeight,
			}}
		case strings.HasPrefix(trimmed, "UI Looks like:") && current != nil:
			if match := resolutionRe.FindStringSubmatch(trimmed); match != nil {
				current.geometry.LogicalWidth, _ = strconv.Atoi(match[1])
				current.geometry.LogicalHeight, _ = strconv.Atoi(match[2])
			}
		case strings.HasPrefix(trimmed, "Main Display:") && current != nil:
			current.main = strings.EqualFold(strings.TrimSpace(strings.TrimPrefix(trimmed, "Main Display:")), "yes")
		}
	}
	flush()
	for _, display := range candidates {
		if display.main {
			return display.geometry, nil
		}
	}
	if len(candidates) > 0 {
		// system_profiler omits "Main Display" on some one-display systems.
		return candidates[0].geometry, nil
	}
	return screenGeometry{}, fmt.Errorf("no display resolution found in system_profiler output")
}

// resizeDimensions mirrors sips --resampleHeightWidthMax: preserve aspect,
// never upscale, and bind the tool declaration to the final pixel dimensions.
func resizeDimensions(width, height, maxDimension int) (int, int) {
	if width <= 0 || height <= 0 || maxDimension <= 0 ||
		(width <= maxDimension && height <= maxDimension) {
		return width, height
	}
	scale := float64(maxDimension) / float64(max(width, height))
	return max(1, int(math.Round(float64(width)*scale))),
		max(1, int(math.Round(float64(height)*scale)))
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
