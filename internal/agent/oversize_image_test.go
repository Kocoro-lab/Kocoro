package agent

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

func makeOversizeImageBlock() client.ContentBlock {
	data := strings.Repeat("A", client.MaxInlineImageBase64Bytes+100)
	return client.ContentBlock{
		Type: "image",
		Source: &client.ImageSource{
			Type:      "base64",
			MediaType: "image/png",
			Data:      data,
		},
	}
}

func makeSmallImageBlock() client.ContentBlock {
	data := base64.StdEncoding.EncodeToString([]byte("tiny png placeholder"))
	return client.ContentBlock{
		Type: "image",
		Source: &client.ImageSource{
			Type:      "base64",
			MediaType: "image/png",
			Data:      data,
		},
	}
}

func TestFilterOversizeImages_ReplacesTopLevelImageBlock(t *testing.T) {
	messages := []client.Message{
		{Role: "user", Content: client.NewBlockContent([]client.ContentBlock{makeOversizeImageBlock()})},
	}
	filterOversizeImages(messages)
	blocks := messages[0].Content.Blocks()
	if blocks[0].Type != "text" {
		t.Fatalf("oversize image not replaced; got type %q", blocks[0].Type)
	}
	if !strings.Contains(blocks[0].Text, "exceeds inline image limit") {
		t.Fatalf("placeholder missing expected text: %q", blocks[0].Text)
	}
}

func TestFilterOversizeImages_ReplacesNestedToolResultImage(t *testing.T) {
	nested := []client.ContentBlock{
		{Type: "text", Text: "[Image: foo.png]"},
		makeOversizeImageBlock(),
	}
	tr := client.ContentBlock{Type: "tool_result", ToolUseID: "call_1", ToolContent: nested}
	messages := []client.Message{
		{Role: "user", Content: client.NewBlockContent([]client.ContentBlock{tr})},
	}
	filterOversizeImages(messages)
	outer := messages[0].Content.Blocks()[0]
	inner, ok := outer.ToolContent.([]client.ContentBlock)
	if !ok {
		t.Fatalf("tool_result content type changed: %T", outer.ToolContent)
	}
	if inner[1].Type != "text" {
		t.Fatalf("nested oversize image not replaced; got %q", inner[1].Type)
	}
}

func TestFilterOversizeImages_LeavesSmallImagesAlone(t *testing.T) {
	messages := []client.Message{
		{Role: "user", Content: client.NewBlockContent([]client.ContentBlock{makeSmallImageBlock()})},
	}
	filterOversizeImages(messages)
	blocks := messages[0].Content.Blocks()
	if blocks[0].Type != "image" {
		t.Fatalf("small image wrongly replaced; got type %q", blocks[0].Type)
	}
}
