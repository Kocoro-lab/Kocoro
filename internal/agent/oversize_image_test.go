package agent

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/png"
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

// TestFilterOversizeImages_AggregateCap_PartialToolResultDrop pins the
// incremental-drop contract on multi-image tool_results. Workload: a 10-page
// PDF rendered as one tool_result with 10 nested 5 MB images = 50 MB total,
// 2× over the 25 MB cap. Pre-fix behavior nuked ALL 10 pages. Correct
// behavior drops the oldest until the aggregate falls under cap, leaving
// the most recent pages intact so the model still has something to work with.
func TestFilterOversizeImages_AggregateCap_PartialToolResultDrop(t *testing.T) {
	const perImageBytes = 5 * 1024 * 1024 // 5 MB each
	const pages = 10                       // 10 × 5 MB = 50 MB > 25 MB cap
	nested := make([]client.ContentBlock, pages)
	for i := range nested {
		nested[i] = client.ContentBlock{
			Type:   "image",
			Source: &client.ImageSource{Type: "base64", MediaType: "image/png", Data: strings.Repeat("A", perImageBytes)},
		}
	}
	messages := []client.Message{{
		Role: "user",
		Content: client.NewBlockContent([]client.ContentBlock{{
			Type:        "tool_result",
			ToolUseID:   "toolu_pdf",
			ToolContent: nested,
		}}),
	}}
	filterOversizeImages(messages)

	// Count survivors inside the tool_result.
	var keptImages, droppedPlaceholders int
	tr := messages[0].Content.Blocks()[0]
	if tr.Type != "tool_result" {
		t.Fatalf("expected first block to remain a tool_result, got %s", tr.Type)
	}
	nestedOut, ok := tr.ToolContent.([]client.ContentBlock)
	if !ok {
		t.Fatalf("tool_result.ToolContent should remain []ContentBlock, got %T", tr.ToolContent)
	}
	for _, nb := range nestedOut {
		switch nb.Type {
		case "image":
			keptImages++
		case "text":
			if strings.Contains(nb.Text, "aggregate base64") {
				droppedPlaceholders++
			}
		}
	}

	// At cap=25 MB and per-image=5 MB, exactly the oldest 5 pages must be
	// dropped (50 MB - 5*5 MB = 25 MB). Pre-fix nuked all 10.
	if keptImages == 0 {
		t.Fatal("regression: all images dropped, partial-drop contract broken")
	}
	if droppedPlaceholders+keptImages != pages {
		t.Fatalf("block count mismatch: kept=%d dropped=%d expected total=%d",
			keptImages, droppedPlaceholders, pages)
	}
	// Strong assertion on the exact arithmetic so off-by-one regressions
	// in the running-total math surface immediately.
	wantDropped := (pages*perImageBytes - MaxAggregateImageBase64Bytes + perImageBytes - 1) / perImageBytes
	if droppedPlaceholders != wantDropped {
		t.Errorf("dropped=%d, want exactly %d (cap arithmetic check)", droppedPlaceholders, wantDropped)
	}

	// Oldest dropped first: the placeholders should appear at the start of
	// the nested slice, with surviving images at the end.
	for i := 0; i < droppedPlaceholders; i++ {
		if nestedOut[i].Type != "text" {
			t.Errorf("nested[%d] should be text placeholder (oldest dropped first), got %s", i, nestedOut[i].Type)
		}
	}
	for i := droppedPlaceholders; i < pages; i++ {
		if nestedOut[i].Type != "image" {
			t.Errorf("nested[%d] should be image (newer kept), got %s", i, nestedOut[i].Type)
		}
	}
}

// --- Pass 0 (dimension cap) tests ---

// makeOversizeDimImageBlock returns an image block whose base64 payload
// decodes to a >2000px image but stays well under the byte cap.
// Uniform-gray PNG, ~few hundred bytes raw.
func makeOversizeDimImageBlock(t *testing.T, w, h int) client.ContentBlock {
	t.Helper()
	return client.ContentBlock{
		Type: "image",
		Source: &client.ImageSource{
			Type:      "base64",
			MediaType: "image/png",
			Data:      base64.StdEncoding.EncodeToString(makePixelBytes(t, w, h)),
		},
	}
}

// makePixelBytes is a thin shim over image/png so the test stays
// portable. Uses uniform Gray so PNG zlib RLE keeps bytes small.
func makePixelBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewGray(image.Rect(0, 0, w, h))
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestFilterOversizeImages_DimPass_ManyImages verifies that with > 20
// images present, an oversize-dim block is replaced with the dimension
// placeholder.
func TestFilterOversizeImages_DimPass_ManyImages(t *testing.T) {
	var blocks []client.ContentBlock
	blocks = append(blocks, makeOversizeDimImageBlock(t, 2588, 690))
	// Pad with 20 small images so total count is 21.
	for i := 0; i < 20; i++ {
		blocks = append(blocks, makeSmallImageBlock())
	}
	messages := []client.Message{
		{Role: "user", Content: client.NewBlockContent(blocks)},
	}
	filterOversizeImages(messages)
	out := messages[0].Content.Blocks()
	if out[0].Type != "text" {
		t.Fatalf("oversize-dim block not replaced; type=%q", out[0].Type)
	}
	if !strings.Contains(out[0].Text, "dimension exceeds") {
		t.Fatalf("placeholder text mismatch: %q", out[0].Text)
	}
}

// TestFilterOversizeImages_DimPass_FewImages verifies that with <= 20
// images present, an oversize-dim block is left alone — Anthropic's
// 2000-px rule does not apply to few-image requests.
func TestFilterOversizeImages_DimPass_FewImages(t *testing.T) {
	blocks := []client.ContentBlock{
		makeOversizeDimImageBlock(t, 2588, 690),
	}
	// Pad up to exactly 20 — boundary case.
	for i := 0; i < 19; i++ {
		blocks = append(blocks, makeSmallImageBlock())
	}
	messages := []client.Message{
		{Role: "user", Content: client.NewBlockContent(blocks)},
	}
	filterOversizeImages(messages)
	out := messages[0].Content.Blocks()
	if out[0].Type != "image" {
		t.Fatalf("oversize-dim block touched at boundary count=20: type=%q", out[0].Type)
	}
}

// TestFilterOversizeImages_DimPass_NestedToolResult covers the case
// where the oversize-dim image is inside a tool_result block (typical
// for file_read / screenshot output).
func TestFilterOversizeImages_DimPass_NestedToolResult(t *testing.T) {
	var blocks []client.ContentBlock
	toolResult := client.ContentBlock{
		Type: "tool_result",
		ToolContent: []client.ContentBlock{
			makeOversizeDimImageBlock(t, 2588, 690),
		},
	}
	blocks = append(blocks, toolResult)
	// Add 21 sibling images so total count > 20.
	for i := 0; i < 21; i++ {
		blocks = append(blocks, makeSmallImageBlock())
	}
	messages := []client.Message{
		{Role: "user", Content: client.NewBlockContent(blocks)},
	}
	filterOversizeImages(messages)
	out := messages[0].Content.Blocks()
	nested, ok := out[0].ToolContent.([]client.ContentBlock)
	if !ok {
		t.Fatalf("tool_result shape lost")
	}
	if nested[0].Type != "text" {
		t.Fatalf("nested oversize-dim image not replaced; type=%q", nested[0].Type)
	}
}

// TestFilterOversizeImages_DimPass_NormalDimSurvives ensures a normal-
// dimension image stays untouched even when image count > 20 (we don't
// over-correct).
func TestFilterOversizeImages_DimPass_NormalDimSurvives(t *testing.T) {
	var blocks []client.ContentBlock
	// 1500x1000 — well under 2000 cap.
	blocks = append(blocks, makeOversizeDimImageBlock(t, 1500, 1000))
	for i := 0; i < 25; i++ {
		blocks = append(blocks, makeSmallImageBlock())
	}
	messages := []client.Message{
		{Role: "user", Content: client.NewBlockContent(blocks)},
	}
	filterOversizeImages(messages)
	out := messages[0].Content.Blocks()
	if out[0].Type != "image" {
		t.Fatalf("legitimate-dim image touched: type=%q", out[0].Type)
	}
}

// TestFilterOversizeImages_DimPass_CrossMessage exercises the realistic
// many-turn shape: oversize-dim images spread across several user
// messages so totalImageCount has to walk all messages, not just one.
// Without the cross-message walk, a session that accumulated 5
// screenshots over 5 turns (25 total) would never trip Pass 0 because
// no single message has > 20 images.
func TestFilterOversizeImages_DimPass_CrossMessage(t *testing.T) {
	var messages []client.Message
	const turns, perTurn = 5, 5
	for turn := 0; turn < turns; turn++ {
		blocks := make([]client.ContentBlock, 0, perTurn)
		// First image per turn is oversize-dim; others are small.
		blocks = append(blocks, makeOversizeDimImageBlock(t, 2588, 690))
		for i := 1; i < perTurn; i++ {
			blocks = append(blocks, makeSmallImageBlock())
		}
		messages = append(messages, client.Message{
			Role:    "user",
			Content: client.NewBlockContent(blocks),
		})
	}
	// Sanity: 5 turns × 5 images = 25 (> manyImageThreshold).
	if got := totalImageCount(messages); got != turns*perTurn {
		t.Fatalf("setup count mismatch: got %d, want %d", got, turns*perTurn)
	}

	filterOversizeImages(messages)

	stripped := 0
	for _, m := range messages {
		for _, b := range m.Content.Blocks() {
			if b.Type == "text" && strings.Contains(b.Text, "dimension exceeds") {
				stripped++
			}
		}
	}
	if stripped != turns {
		t.Fatalf("cross-message Pass 0 expected %d strips (one per turn), got %d", turns, stripped)
	}
}
