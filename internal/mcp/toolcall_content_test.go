package mcp

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	mcpproto "github.com/mark3labs/mcp-go/mcp"
)

// Images returned by an MCP tool used to be json.Marshal'd into the result
// string. The vision path can never consume a string, the generic result
// truncation then cut the payload mid-base64, and the model was billed for
// thousands of tokens of line noise. These tests pin the split.

func textBlock(s string) mcpproto.Content {
	return mcpproto.TextContent{Type: "text", Text: s}
}

func imageBlock(data, mime string) mcpproto.Content {
	return mcpproto.ImageContent{Type: "image", Data: data, MIMEType: mime}
}

func TestSplitToolCallContent_ImageStaysStructured(t *testing.T) {
	// A long base64 payload: the exact shape that used to be truncated into
	// meaningless half-base64.
	payload := strings.Repeat("iVBORw0KGgoAAAANSUhEUg", 4000)

	out := splitToolCallContent([]mcpproto.Content{
		textBlock("### Result"),
		imageBlock(payload, "image/png"),
	})

	if len(out.Images) != 1 {
		t.Fatalf("got %d images, want 1", len(out.Images))
	}
	if out.Images[0].Base64 != payload {
		t.Fatal("image base64 was altered in transit")
	}
	if out.Images[0].MIMEType != "image/png" {
		t.Fatalf("mime type = %q", out.Images[0].MIMEType)
	}
	if strings.Contains(out.Text, payload) {
		t.Fatal("base64 payload leaked into the text content")
	}
	if strings.Contains(out.Text, `"type":"image"`) {
		t.Fatal("image block was still JSON-marshalled into the text")
	}
}

func TestSplitToolCallContent_ImageSummaryWaitsForDecode(t *testing.T) {
	// The client layer cannot know whether base64 decode/compression will
	// succeed, so it must not claim that an image was delivered yet.
	out := splitToolCallContent([]mcpproto.Content{imageBlock("Zm9v", "image/png")})

	if out.Text != "" {
		t.Fatalf("pre-decode image summary leaked into text: %q", out.Text)
	}
}

func TestSplitToolCallContent_TextOnlyIsUnchanged(t *testing.T) {
	// The overwhelmingly common case must be byte-identical to the old
	// behavior — this is a prompt-cache surface.
	out := splitToolCallContent([]mcpproto.Content{
		textBlock("first"),
		textBlock("second"),
	})

	if out.Text != "first\nsecond" {
		t.Fatalf("text = %q, want %q", out.Text, "first\nsecond")
	}
	if out.Images != nil {
		t.Fatalf("text-only result produced %d images", len(out.Images))
	}
}

func TestSplitToolCallContent_EmptyResult(t *testing.T) {
	out := splitToolCallContent(nil)
	if out.Text != "" || out.Images != nil {
		t.Fatalf("empty result produced %+v", out)
	}
}

func TestSplitToolCallContent_ImageCapReportsDropCount(t *testing.T) {
	blocks := []mcpproto.Content{textBlock("gallery")}
	for i := 0; i < maxToolCallImages+3; i++ {
		blocks = append(blocks, imageBlock(fmt.Sprintf("img%d", i), "image/png"))
	}

	out := splitToolCallContent(blocks)

	if len(out.Images) != maxToolCallImages {
		t.Fatalf("kept %d images, want the cap %d", len(out.Images), maxToolCallImages)
	}
	if out.DroppedImages != 3 {
		t.Fatalf("dropped count = %d, want 3", out.DroppedImages)
	}
	// The kept images must be the FIRST ones, in order.
	for i, img := range out.Images {
		if want := fmt.Sprintf("img%d", i); img.Base64 != want {
			t.Fatalf("image %d = %q, want %q (order not preserved)", i, img.Base64, want)
		}
	}
}

func TestSplitToolCallContent_NonImageNonTextKeepsJSONFallback(t *testing.T) {
	// Audio and embedded resources have no consumer in the loop, so a
	// self-describing JSON blob is still the most useful thing the model can
	// see. Only images changed.
	audio := mcpproto.AudioContent{Type: "audio", Data: "AAAA", MIMEType: "audio/wav"}
	out := splitToolCallContent([]mcpproto.Content{audio})

	if len(out.Images) != 0 {
		t.Fatal("audio was misrouted into Images")
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(out.Text), &decoded); err != nil {
		t.Fatalf("audio fallback is not valid JSON: %v (%q)", err, out.Text)
	}
	if decoded["type"] != "audio" {
		t.Fatalf("audio fallback lost its type: %v", decoded)
	}
}

func TestSplitToolCallContent_MixedOrderTextPreserved(t *testing.T) {
	// Text ordering carries meaning (playwright emits "### Result" then
	// "### Page" then "### Snapshot"). Image reporting happens after decode
	// in the tool layer, so the split itself keeps only source text here.
	out := splitToolCallContent([]mcpproto.Content{
		textBlock("A"),
		imageBlock("Zm9v", "image/jpeg"),
		textBlock("B"),
	})

	lines := strings.Split(out.Text, "\n")
	if len(lines) != 2 || lines[0] != "A" || lines[1] != "B" {
		t.Fatalf("text order broken: %q", out.Text)
	}
}
