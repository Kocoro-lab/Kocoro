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

func TestSanitizedRunMessages_EmptyInputReturnsEmpty(t *testing.T) {
	a := &AgentLoop{}
	got := a.SanitizedRunMessages()
	if len(got) != 0 {
		t.Fatalf("expected empty result, got %d entries", len(got))
	}
}

func TestFilterOversizeImages_AggregateCap(t *testing.T) {
	// 6 messages each carrying an 5 MB image = 30 MB aggregate, over 25 MB cap.
	// Expectation: oldest image(s) get replaced with aggregate placeholder until
	// total falls back under 25 MB.
	const perImageBytes = 5 * 1024 * 1024 // exactly 5 MB
	mkMsg := func() client.Message {
		data := strings.Repeat("A", perImageBytes)
		return client.Message{
			Role: "user",
			Content: client.NewBlockContent([]client.ContentBlock{{
				Type:   "image",
				Source: &client.ImageSource{Type: "base64", MediaType: "image/png", Data: data},
			}}),
		}
	}
	messages := []client.Message{mkMsg(), mkMsg(), mkMsg(), mkMsg(), mkMsg(), mkMsg()}
	filterOversizeImages(messages)

	// Recompute total. Each remaining image's Source.Data length should sum ≤ 25 MB.
	total := 0
	dropped := 0
	for _, m := range messages {
		for _, b := range m.Content.Blocks() {
			if b.Type == "image" && b.Source != nil {
				total += len(b.Source.Data)
			}
			if b.Type == "text" && strings.Contains(b.Text, "aggregate base64") {
				dropped++
			}
		}
	}
	if total > MaxAggregateImageBase64Bytes {
		t.Fatalf("aggregate total %d exceeds cap %d", total, MaxAggregateImageBase64Bytes)
	}
	if dropped == 0 {
		t.Fatal("expected at least one image dropped by aggregate cap")
	}
	// The OLDEST messages should be the ones dropped — message[0] should be a text placeholder.
	if messages[0].Content.Blocks()[0].Type != "text" {
		t.Fatal("expected oldest message to be replaced first")
	}
}
