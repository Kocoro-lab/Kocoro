package daemon

import (
	"strings"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

// testTinyPNGBase64 is a 1x1 transparent PNG. Small enough that
// CompressInlineImageSource passes it through (well under the 2000x2000 ladder).
const testTinyPNGBase64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAAC0lEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="

// TestContentBlocksToInjected_LowersTextImageDocument verifies the HTTP
// inject-path lowering: text folds into the returned string, image/document
// blocks become agent.InjectedFile entries in order.
func TestContentBlocksToInjected_LowersTextImageDocument(t *testing.T) {
	blocks := []RequestContentBlock{
		{Type: "text", Text: "look at this"},
		{Type: "image", Source: &client.ImageSource{Type: "base64", MediaType: "image/png", Data: testTinyPNGBase64}},
		{Type: "document", Source: &client.ImageSource{Type: "base64", MediaType: "application/pdf", Data: "JVBERi0xLjQK"}},
	}
	text, files := contentBlocksToInjected(blocks)
	if text != "look at this" {
		t.Errorf("text = %q, want %q", text, "look at this")
	}
	if len(files) != 2 {
		t.Fatalf("files = %d, want 2: %+v", len(files), files)
	}
	if files[0].Type != "image" || files[0].Data == "" {
		t.Errorf("image file wrong: %+v", files[0])
	}
	// Documents pass through uncompressed → media type + data are exact.
	if files[1].Type != "document" || files[1].MediaType != "application/pdf" || files[1].Data != "JVBERi0xLjQK" {
		t.Errorf("document file wrong: %+v", files[1])
	}
}

func TestContentBlocksToInjected_Empty(t *testing.T) {
	text, files := contentBlocksToInjected(nil)
	if text != "" || files != nil {
		t.Errorf("empty input should yield empty: text=%q files=%v", text, files)
	}
}

// TestContentBlocksToInjected_FileRefFolderHintFoldsIntoText proves file_ref
// blocks that resolve to a text hint (folders, zips, oversize, errors) fold
// into the injected text rather than producing a file — and that the file_ref
// disk-resolution path runs in the inject lowering.
func TestContentBlocksToInjected_FileRefFolderHintFoldsIntoText(t *testing.T) {
	dir := t.TempDir()
	blocks := []RequestContentBlock{
		{Type: "file_ref", FilePath: dir, Filename: "myfolder"},
	}
	text, files := contentBlocksToInjected(blocks)
	if len(files) != 0 {
		t.Errorf("folder ref should produce no files, got %+v", files)
	}
	if !strings.Contains(text, "myfolder") || !strings.Contains(text, dir) {
		t.Errorf("folder hint not folded into text: %q", text)
	}
}
