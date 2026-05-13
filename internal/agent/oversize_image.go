package agent

import (
	"fmt"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

// MaxAggregateImageBase64Bytes caps the SUM of all image base64 payloads in a
// single request. Anthropic's hard request-body limit is 32 MB; we keep 25 MB
// to leave headroom for system prompt + text + tool schemas.
const MaxAggregateImageBase64Bytes = 25 * 1024 * 1024

// filterOversizeImages enforces two caps:
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
				nb, nestedChanged, removed := dropImagesFromToolResultForAggregate(b)
				if nestedChanged {
					total -= removed
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

func dropImagesFromToolResultForAggregate(b client.ContentBlock) (client.ContentBlock, bool, int) {
	nested, ok := b.ToolContent.([]client.ContentBlock)
	if !ok {
		return b, false, 0
	}
	newNested := make([]client.ContentBlock, len(nested))
	changed := false
	removed := 0
	for k, nb := range nested {
		if nb.Type == "image" && nb.Source != nil && len(nb.Source.Data) > 0 {
			removed += len(nb.Source.Data)
			newNested[k] = aggregateImagePlaceholder()
			changed = true
			continue
		}
		newNested[k] = nb
	}
	if !changed {
		return b, false, 0
	}
	out := b
	out.ToolContent = newNested
	return out, true, removed
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
