package agent

import (
	"encoding/base64"
	"fmt"
	"image"
	"strings"

	_ "image/gif"  // header parse for filterOversizeImages dim guard
	_ "image/jpeg" // header parse for filterOversizeImages dim guard
	_ "image/png"  // header parse for filterOversizeImages dim guard

	_ "golang.org/x/image/webp" // header parse for filterOversizeImages dim guard

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

// wireImageDimensionCap is the per-side pixel limit applied at the wire
// sanitizer when a request carries more than manyImageThreshold images.
// Mirrors tools.CompressionMaxDimension; kept as a separate constant
// because internal/agent must not import internal/tools (tools depends
// on agent for ToolRegistry). Adjust both in lockstep — TestWireImage*
// pins the contract.
const wireImageDimensionCap = 2000

// manyImageThreshold is the inclusive lower bound that triggers Pass 0's
// dimension check. Anthropic's documented many-image rule kicks in when
// the request carries >20 images: per-side limit drops from 8000 to 2000.
// We engage Pass 0 strictly above 20 so single high-resolution images
// stay untouched.
const manyImageThreshold = 20

// MaxAggregateImageBase64Bytes caps the SUM of all image base64 payloads in a
// single request. Anthropic's hard request-body limit is 32 MB; this leaves
// ~7 MB headroom for system prompt, text, and tool schemas.
//
// Workload: a user reading 20+ screenshots in parallel (vision-heavy batch)
// or accumulating large images across many turns within one session.
// Symptom when binds: oldest images replaced with a "[image removed: aggregate
// base64 across this request exceeded N bytes]" text placeholder, paired with
// an "img_aggregate_strip" cache-compact event in cache-debug.log.
// Override: not user-configurable — file an issue if your workload routinely
// exceeds 25 MB of compressed inline images per request.
const MaxAggregateImageBase64Bytes = 25 * 1024 * 1024

// filterOversizeImages enforces three caps:
//  0. Per-image dimension (many-image only): when total image count >
//     manyImageThreshold (20), any image with either edge >
//     wireImageDimensionCap (2000 px) is replaced with a placeholder.
//     Backstop for the source-time tools.compressImage /
//     tools.CompressInlineImageSource dimension check: catches images
//     that bypass Layer 1 entirely — pre-fix persisted sessions, MCP
//     server-produced image blocks, anything that never went through
//     EncodeImage/CompressInlineImageSource. Without this pass, those
//     paths can hit Anthropic's "many-image requests: 2000 pixels" 400.
//  1. Per-image: any image > client.MaxInlineImageBase64Bytes (5 MB) is replaced
//     with a placeholder. This prevents Anthropic's per-image 400.
//  2. Aggregate: if the SUM of all remaining image source bytes across all
//     messages exceeds MaxAggregateImageBase64Bytes (25 MB), the OLDEST images
//     are dropped first until the total fits. This prevents Anthropic's 32 MB
//     request-body 400.
//
// Wire-time guard for Anthropic's per-image 5 MB hard limit + 32 MB request
// body. Even if a tool produces an oversize image (MCP server, cloud-pushed
// inline image, or a session loaded from disk before EncodeImage compression
// existed), this guard ensures the request never reaches Anthropic in a state
// that triggers the "image exceeds 5 MB maximum" 400 or the aggregate cap.
//
// Pairs with filterOldImages (count-based) — this one is size-based.
func filterOversizeImages(messages []client.Message) {
	// Pass 0: per-image dimension cap (many-image scenarios only).
	enforcePerImageDimensionCap(messages)

	// Pass 1: per-image cap.
	for i := range messages {
		if !messages[i].Content.HasBlocks() {
			continue
		}
		oldBlocks := messages[i].Content.Blocks()
		newBlocks := make([]client.ContentBlock, len(oldBlocks))
		changed := false
		for j, b := range oldBlocks {
			switch b.Type {
			case "image":
				if oversizeImageSource(b.Source) {
					newBlocks[j] = oversizeImagePlaceholder()
					changed = true
					continue
				}
				newBlocks[j] = b
			case "tool_result":
				nb, nestedChanged := sanitizeToolResultImages(b)
				if nestedChanged {
					changed = true
				}
				newBlocks[j] = nb
			default:
				newBlocks[j] = b
			}
		}
		if changed {
			oldContent := messages[i].Content
			messages[i].Content = client.NewBlockContent(newBlocks)
			client.LogCacheCompactEvent("img_oversize_strip", i, oldContent, messages[i].Content)
		}
	}

	// Pass 2: aggregate cap. Drop oldest images first.
	enforceAggregateImageCap(messages)
}

func enforceAggregateImageCap(messages []client.Message) {
	total := 0
	for i := range messages {
		if !messages[i].Content.HasBlocks() {
			continue
		}
		for _, b := range messages[i].Content.Blocks() {
			if b.Type == "image" && b.Source != nil {
				total += len(b.Source.Data)
			}
			if b.Type == "tool_result" {
				if nested, ok := b.ToolContent.([]client.ContentBlock); ok {
					for _, nb := range nested {
						if nb.Type == "image" && nb.Source != nil {
							total += len(nb.Source.Data)
						}
					}
				}
			}
		}
	}
	if total <= MaxAggregateImageBase64Bytes {
		return
	}
	// Drop oldest images first until under cap.
	for i := range messages {
		if total <= MaxAggregateImageBase64Bytes {
			return
		}
		if !messages[i].Content.HasBlocks() {
			continue
		}
		oldBlocks := messages[i].Content.Blocks()
		newBlocks := make([]client.ContentBlock, len(oldBlocks))
		changed := false
		for j, b := range oldBlocks {
			if total <= MaxAggregateImageBase64Bytes {
				newBlocks[j] = b
				continue
			}
			switch b.Type {
			case "image":
				if b.Source != nil && len(b.Source.Data) > 0 {
					total -= len(b.Source.Data)
					newBlocks[j] = aggregateImagePlaceholder()
					changed = true
					continue
				}
				newBlocks[j] = b
			case "tool_result":
				nb, nestedChanged, newTotal := dropImagesFromToolResultForAggregate(b, total)
				if nestedChanged {
					total = newTotal
					changed = true
				}
				newBlocks[j] = nb
			default:
				newBlocks[j] = b
			}
		}
		if changed {
			oldContent := messages[i].Content
			messages[i].Content = client.NewBlockContent(newBlocks)
			client.LogCacheCompactEvent("img_aggregate_strip", i, oldContent, messages[i].Content)
		}
	}
}

func aggregateImagePlaceholder() client.ContentBlock {
	return client.ContentBlock{
		Type: "text",
		Text: fmt.Sprintf("[image removed: aggregate base64 across this request exceeded %d bytes]", MaxAggregateImageBase64Bytes),
	}
}

// dropImagesFromToolResultForAggregate evicts nested images from a tool_result
// block ONE AT A TIME (oldest first) until the running aggregate total falls
// under MaxAggregateImageBase64Bytes. Previously this function dropped ALL
// nested images unconditionally once entered — a 20-page PDF in a single
// tool_result lost all 20 pages when only the oldest 2-3 needed to go.
// Returns (modified block, whether anything changed, new running total).
func dropImagesFromToolResultForAggregate(b client.ContentBlock, currentTotal int) (client.ContentBlock, bool, int) {
	nested, ok := b.ToolContent.([]client.ContentBlock)
	if !ok {
		return b, false, currentTotal
	}
	newNested := make([]client.ContentBlock, len(nested))
	changed := false
	total := currentTotal
	for k, nb := range nested {
		if total <= MaxAggregateImageBase64Bytes {
			newNested[k] = nb
			continue
		}
		if nb.Type == "image" && nb.Source != nil && len(nb.Source.Data) > 0 {
			total -= len(nb.Source.Data)
			newNested[k] = aggregateImagePlaceholder()
			changed = true
			continue
		}
		newNested[k] = nb
	}
	if !changed {
		return b, false, currentTotal
	}
	out := b
	out.ToolContent = newNested
	return out, true, total
}

func oversizeImageSource(s *client.ImageSource) bool {
	return s != nil && len(s.Data) > client.MaxInlineImageBase64Bytes
}

func oversizeImagePlaceholder() client.ContentBlock {
	return client.ContentBlock{
		Type: "text",
		Text: fmt.Sprintf("[image exceeds inline image limit (%d bytes), removed]", client.MaxInlineImageBase64Bytes),
	}
}

func sanitizeToolResultImages(b client.ContentBlock) (client.ContentBlock, bool) {
	nested, ok := b.ToolContent.([]client.ContentBlock)
	if !ok {
		return b, false
	}
	newNested := make([]client.ContentBlock, len(nested))
	changed := false
	for k, nb := range nested {
		if nb.Type == "image" && oversizeImageSource(nb.Source) {
			newNested[k] = oversizeImagePlaceholder()
			changed = true
			continue
		}
		newNested[k] = nb
	}
	if !changed {
		return b, false
	}
	out := b
	out.ToolContent = newNested
	return out, true
}

// enforcePerImageDimensionCap is Pass 0 of filterOversizeImages. When the
// total image count exceeds manyImageThreshold (20), every image block
// whose header reports either side > wireImageDimensionCap (2000 px) is
// replaced with a text placeholder. Decoded once per image via header-
// only image.DecodeConfig (PNG IHDR / JPEG SOFn / GIF LSD / WebP VP8),
// which scans tens of bytes — bounded cost even on the 5 MB-base64
// fast-path payload.
//
// Returns silently for image counts ≤ 20 so single-image and few-image
// requests carrying a legitimate >2000px image still pass through (those
// use Anthropic's 8000-px-per-side single-image limit). DecodeConfig
// errors are treated as "not oversize" — a malformed payload is the
// byte-cap path's problem and the per-image / aggregate passes that
// follow will deal with it if it's actually too large in bytes.
func enforcePerImageDimensionCap(messages []client.Message) {
	if totalImageCount(messages) <= manyImageThreshold {
		return
	}
	for i := range messages {
		if !messages[i].Content.HasBlocks() {
			continue
		}
		oldBlocks := messages[i].Content.Blocks()
		newBlocks := make([]client.ContentBlock, len(oldBlocks))
		changed := false
		for j, b := range oldBlocks {
			switch b.Type {
			case "image":
				if dimensionOversize(b.Source) {
					newBlocks[j] = dimensionPlaceholder()
					changed = true
					continue
				}
				newBlocks[j] = b
			case "tool_result":
				nb, nestedChanged := sanitizeToolResultDimensions(b)
				if nestedChanged {
					changed = true
				}
				newBlocks[j] = nb
			default:
				newBlocks[j] = b
			}
		}
		if changed {
			oldContent := messages[i].Content
			messages[i].Content = client.NewBlockContent(newBlocks)
			client.LogCacheCompactEvent("img_dim_strip", i, oldContent, messages[i].Content)
		}
	}
}

// totalImageCount counts top-level image blocks AND images nested inside
// tool_result blocks. Both shapes contribute to Anthropic's per-request
// image tally and so both shapes contribute to the many-image threshold
// decision.
func totalImageCount(messages []client.Message) int {
	n := 0
	for _, m := range messages {
		if !m.Content.HasBlocks() {
			continue
		}
		for _, b := range m.Content.Blocks() {
			switch b.Type {
			case "image":
				n++
			case "tool_result":
				if nested, ok := b.ToolContent.([]client.ContentBlock); ok {
					for _, nb := range nested {
						if nb.Type == "image" {
							n++
						}
					}
				}
			}
		}
	}
	return n
}

// dimensionOversize returns true when the image source's header reports
// either edge above wireImageDimensionCap. Streams the base64 reader
// through image.DecodeConfig so only the header bytes are decoded.
// Returns false on nil source, empty data, or any decode error.
func dimensionOversize(s *client.ImageSource) bool {
	if s == nil || s.Data == "" {
		return false
	}
	reader := base64.NewDecoder(base64.StdEncoding, strings.NewReader(s.Data))
	cfg, _, err := image.DecodeConfig(reader)
	if err != nil {
		return false
	}
	return cfg.Width > wireImageDimensionCap || cfg.Height > wireImageDimensionCap
}

// dimensionPlaceholder is the text block injected when an image is
// dropped by the dimension cap. Distinct text from byte-cap placeholders
// so audit/cache-debug logs disambiguate the drop reason.
func dimensionPlaceholder() client.ContentBlock {
	return client.ContentBlock{
		Type: "text",
		Text: fmt.Sprintf("[image removed: dimension exceeds %dpx for many-image request]", wireImageDimensionCap),
	}
}

// sanitizeToolResultDimensions mirrors sanitizeToolResultImages but for
// the dimension cap: replaces oversize-dimension images nested inside a
// tool_result with placeholders, leaving the surrounding tool_result
// shape intact.
func sanitizeToolResultDimensions(b client.ContentBlock) (client.ContentBlock, bool) {
	nested, ok := b.ToolContent.([]client.ContentBlock)
	if !ok {
		return b, false
	}
	newNested := make([]client.ContentBlock, len(nested))
	changed := false
	for k, nb := range nested {
		if nb.Type == "image" && dimensionOversize(nb.Source) {
			newNested[k] = dimensionPlaceholder()
			changed = true
			continue
		}
		newNested[k] = nb
	}
	if !changed {
		return b, false
	}
	out := b
	out.ToolContent = newNested
	return out, true
}
