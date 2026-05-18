package agent

import (
	"strings"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

// InjectedFile is a minimal attachment carrier for mid-turn InjectedMessage.
// The daemon layer owns the full RemoteFile / RequestContentBlock formats
// and converts down to InjectedFile before crossing the package boundary;
// defining the type here keeps internal/agent free of internal/daemon
// imports (the reverse import already exists and the cycle must not close).
//
// Wire constraint: client.ImageSource (the eventual destination type) only
// accepts Type="base64" + MediaType + Data — NO url field. So URL-only
// RemoteFiles must be downloaded + base64-inlined by the daemon BEFORE
// reaching this struct.
//
// Types:
//   - "image"    → Data is base64, MediaType set ("image/png" etc.)
//   - "document" → Data is base64 PDF bytes, MediaType="application/pdf"
//   - "text"     → Data is raw extracted text (cloud-extracted DOCX/etc.)
type InjectedFile struct {
	Type      string
	MediaType string
	Data      string
}

// drainInjected pulls all pending messages from ch without blocking.
func drainInjected(ch chan InjectedMessage) []InjectedMessage {
	var out []InjectedMessage
drain:
	for {
		select {
		case msg, ok := <-ch:
			if !ok {
				break drain
			}
			out = append(out, msg)
		default:
			break drain
		}
	}
	return out
}

// buildInjectedUserMessage converts drained InjectedMessages into a single
// user-role Message ready to append. Returns (zero, false) when input is
// empty. When NO Files are present, emits a plain text content (preserving
// the legacy "[New message from user]\n" prefix for byte-identity with the
// pre-refactor code path). When ANY File is present, emits a multimodal
// block array: a leading text block (joined inject texts) followed by one
// content block per InjectedFile.
func buildInjectedUserMessage(drained []InjectedMessage) (client.Message, bool) {
	if len(drained) == 0 {
		return client.Message{}, false
	}
	texts := make([]string, 0, len(drained))
	var files []InjectedFile
	for _, m := range drained {
		if m.Text != "" {
			texts = append(texts, m.Text)
		}
		files = append(files, m.Files...)
	}
	combinedText := strings.Join(texts, "\n\n")
	if len(files) == 0 {
		// Text-only path: keep the legacy prefix so the LLM-visible payload
		// is byte-identical to the pre-refactor inject behavior.
		return client.Message{
			Role:    "user",
			Content: client.NewTextContent("[New message from user]\n" + combinedText),
		}, true
	}
	blocks := make([]client.ContentBlock, 0, 1+len(files))
	if combinedText != "" {
		blocks = append(blocks, client.ContentBlock{Type: "text", Text: combinedText})
	}
	for _, f := range files {
		blocks = append(blocks, injectedFileToBlock(f))
	}
	return client.Message{
		Role:    "user",
		Content: client.NewBlockContent(blocks),
	}, true
}

// injectedFileToBlock lowers a single InjectedFile to a client.ContentBlock.
// Kocoro's client.ContentBlock supports "document" with a base64 ImageSource
// (see internal/daemon/attachment.go materializeInlineDocument which emits
// exactly this shape for PDFs). For unknown / unrepresentable file types,
// emit a text marker rather than silently dropping the attachment.
func injectedFileToBlock(f InjectedFile) client.ContentBlock {
	switch f.Type {
	case "image":
		return client.ContentBlock{
			Type:   "image",
			Source: &client.ImageSource{Type: "base64", MediaType: f.MediaType, Data: f.Data},
		}
	case "document":
		return client.ContentBlock{
			Type:   "document",
			Source: &client.ImageSource{Type: "base64", MediaType: f.MediaType, Data: f.Data},
		}
	case "text":
		return client.ContentBlock{Type: "text", Text: f.Data}
	default:
		return client.ContentBlock{Type: "text", Text: "[unsupported attachment: " + f.Type + "]"}
	}
}
