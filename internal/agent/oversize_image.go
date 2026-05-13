package agent

import (
	"fmt"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

// filterOversizeImages scans every message's content blocks (and any nested
// tool_result blocks) and replaces any image whose base64 source data length
// exceeds client.MaxInlineImageBase64Bytes with a text placeholder. The
// replacement is in-place.
//
// Wire-time guard for Anthropic's per-image 5 MB hard limit. Even if a tool
// produces an oversize image (MCP server, cloud-pushed inline image, or a
// session loaded from disk before EncodeImage compression existed), this
// guard ensures the request never reaches Anthropic in a state that triggers
// the "image exceeds 5 MB maximum" 400.
//
// Pairs with filterOldImages (which prunes by count); this one prunes by size.
func filterOversizeImages(messages []client.Message) {
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
