package share

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/session"
)

func TestSanitize_DropsSystemInjectedMessages(t *testing.T) {
	msgs := []client.Message{
		{Role: "user", Content: client.NewTextContent("hello")},
		{Role: "assistant", Content: client.NewTextContent("nudge text")},
		{Role: "user", Content: client.NewTextContent("world")},
	}
	meta := []session.MessageMeta{
		{},
		{SystemInjected: true},
		{},
	}
	got, gotMeta := Sanitize(msgs, meta, Options{})
	if len(got) != 2 || got[0].Content.Text() != "hello" || got[1].Content.Text() != "world" {
		t.Fatalf("expected SystemInjected message dropped, got %+v", got)
	}
	if len(gotMeta) != 2 {
		t.Fatalf("meta length mismatch: %d", len(gotMeta))
	}
}

func TestSanitize_RemovesThinkingAndDocumentBlocks(t *testing.T) {
	msgs := []client.Message{{
		Role: "assistant",
		Content: client.NewBlockContent([]client.ContentBlock{
			{Type: "thinking", Thinking: "internal reasoning"},
			{Type: "text", Text: "the answer is 42"},
			{Type: "redacted_thinking", Data: "blob"},
			{Type: "document", Source: &client.ImageSource{Type: "base64", MediaType: "application/pdf", Data: "PDFBYTES"}},
		}),
	}}
	got, _ := Sanitize(msgs, nil, Options{})
	if len(got) != 1 {
		t.Fatalf("expected 1 message, got %d", len(got))
	}
	blocks := got[0].Content.Blocks()
	if len(blocks) != 1 || blocks[0].Type != "text" {
		t.Fatalf("expected only text block to survive, got %+v", blocks)
	}
}

func TestSanitize_PreservesImageBase64(t *testing.T) {
	const data = "iVBORw0KGgoAAAANSUhEUg"
	msgs := []client.Message{{
		Role: "user",
		Content: client.NewBlockContent([]client.ContentBlock{
			{Type: "image", Source: &client.ImageSource{Type: "base64", MediaType: "image/png", Data: data}},
		}),
	}}
	got, _ := Sanitize(msgs, nil, Options{})
	blocks := got[0].Content.Blocks()
	if len(blocks) != 1 || blocks[0].Source == nil || blocks[0].Source.Data != data {
		t.Fatalf("image base64 should be preserved verbatim, got %+v", blocks)
	}
}

func TestSanitize_StripsSystemReminder(t *testing.T) {
	text := "hello\n<system-reminder>\nshhh secret reminder\n</system-reminder>\nworld"
	out := sanitizeText(text, Options{})
	if strings.Contains(out, "system-reminder") || strings.Contains(out, "secret reminder") {
		t.Fatalf("system-reminder should be stripped, got %q", out)
	}
	if !strings.Contains(out, "hello") || !strings.Contains(out, "world") {
		t.Fatalf("surrounding text should survive, got %q", out)
	}
}

func TestSanitize_CollapsesHomePath(t *testing.T) {
	out := sanitizeText("see /Users/alice/code/foo.go for details", Options{HomeDir: "/Users/alice"})
	if !strings.Contains(out, "~/code/foo.go") {
		t.Fatalf("home-dir should collapse to ~, got %q", out)
	}
}

func TestSanitize_CollapsesAttachmentPath(t *testing.T) {
	in := "open /Users/bob/.shannon/tmp/attachments/abc123/report.pdf to view"
	out := sanitizeText(in, Options{})
	if !strings.Contains(out, "[attachment: report.pdf]") {
		t.Fatalf("attachment path should collapse, got %q", out)
	}
	if strings.Contains(out, "abc123") {
		t.Fatalf("nonce leaked: %q", out)
	}
}

func TestSanitize_StripsFileHintPath(t *testing.T) {
	in := "[User attached image: photo.png (12345 bytes) at path: /Users/carol/.shannon/tmp/attachments/xyz/photo.png — included inline below.]"
	out := sanitizeText(in, Options{})
	// First the attachment-path regex collapses the inner path. Then the file
	// hint envelope strips the trailing path-and-size annotation.
	if !strings.Contains(out, "[User attached image: photo.png]") {
		t.Fatalf("file hint should be reduced to name only, got %q", out)
	}
	if strings.Contains(out, "/Users/") || strings.Contains(out, "/attachments/") {
		t.Fatalf("path tail leaked: %q", out)
	}
}

func TestSanitize_RedactsAPIKeyShapes(t *testing.T) {
	// Prefixes are split (`"xo"+"xb-"`) so the literal tokens never appear
	// contiguously in source — GitHub's secret-scanning push protection
	// flags real-shaped tokens even in test fixtures, blocking pushes.
	// The strings still test the regex correctly because they're concatenated
	// at compile time before sanitizeText sees them.
	slack := "xo" + "xb-1234567890-1234567890123-abcdefghijklmnopqrstuvwxyz"
	github := "gh" + "p_abcdefghijklmnopqrstuvwxyz0123456"
	openai := "sk-" + "abc123def456ghi789jkl"
	cases := map[string]string{
		"export OPENAI_API_KEY=" + openai: "API key value",
		"GITHUB_TOKEN=" + github:          "GitHub PAT",
		"slack token " + slack:            "Slack bot token",
	}
	for in, label := range cases {
		out := sanitizeText(in, Options{})
		if !strings.Contains(out, "[REDACTED]") {
			t.Errorf("%s not redacted: input=%q output=%q", label, in, out)
		}
	}
}

func TestSanitize_RedactsEnvVarKeepsName(t *testing.T) {
	// Bash command style: keep the variable NAME so the reader can still
	// tell what was being set, only redact the value.
	out := sanitizeText("DATABASE_URL=postgres://user:pw@host/db ./migrate", Options{})
	if !strings.Contains(out, "DATABASE_URL=[REDACTED]") {
		t.Fatalf("env name should survive, got %q", out)
	}
	if strings.Contains(out, "postgres://") {
		t.Fatalf("env value leaked: %q", out)
	}
}

func TestSanitize_ToolUseInputRedaction(t *testing.T) {
	// Two redaction paths exercised:
	//   - AWS_SECRET_ACCESS_KEY → caught by audit.RedactSecrets' KEY/SECRET/
	//     TOKEN/PASSWORD env-var rule. Whole token becomes [REDACTED] —
	//     variable name is intentionally lost because audit treats the name
	//     itself as a strong signal that the LINE is sensitive (better to
	//     err on the side of redacting too much).
	//   - DATABASE_URL → not caught by audit (no secret-keyword in name);
	//     reEnvVarAssign preserves the name so the share-page reader can
	//     still see "something at this env var was set" without the value.
	input, _ := json.Marshal(map[string]any{
		"command":  "AWS_SECRET_ACCESS_KEY=verysecret aws s3 cp /Users/dave/secret.txt s3://b/",
		"db":       "DATABASE_URL=postgres://u:p@h/db",
		"timeout":  30,
	})
	msgs := []client.Message{{
		Role: "assistant",
		Content: client.NewBlockContent([]client.ContentBlock{
			{Type: "tool_use", ID: "toolu_1", Name: "bash", Input: input},
		}),
	}}
	got, _ := Sanitize(msgs, nil, Options{HomeDir: "/Users/dave"})
	out := string(got[0].Content.Blocks()[0].Input)
	if strings.Contains(out, "verysecret") {
		t.Fatalf("AWS secret value leaked: %s", out)
	}
	if !strings.Contains(out, "[REDACTED]") {
		t.Fatalf("expected [REDACTED] sentinel for AWS_SECRET_ACCESS_KEY: %s", out)
	}
	if !strings.Contains(out, "DATABASE_URL=[REDACTED]") {
		t.Fatalf("non-secret-named env var should preserve its name: %s", out)
	}
	if strings.Contains(out, "postgres://u:p") {
		t.Fatalf("DATABASE_URL value leaked: %s", out)
	}
	if !strings.Contains(out, "~/secret.txt") {
		t.Fatalf("tool_use home-dir not collapsed: %s", out)
	}
}

func TestSanitize_ToolResultBlocksRecurse(t *testing.T) {
	msgs := []client.Message{{
		Role: "user",
		Content: client.NewBlockContent([]client.ContentBlock{
			{
				Type:      "tool_result",
				ToolUseID: "toolu_1",
				ToolContent: []client.ContentBlock{
					{Type: "text", Text: "see /Users/eve/secret.env"},
					{Type: "thinking", Thinking: "should be dropped"},
					{Type: "image", Source: &client.ImageSource{Type: "base64", MediaType: "image/png", Data: "kept"}},
				},
			},
		}),
	}}
	got, _ := Sanitize(msgs, nil, Options{HomeDir: "/Users/eve"})
	blocks := got[0].Content.Blocks()
	if len(blocks) != 1 || blocks[0].Type != "tool_result" {
		t.Fatalf("expected single tool_result block, got %+v", blocks)
	}
	nested, ok := blocks[0].ToolContent.([]client.ContentBlock)
	if !ok {
		t.Fatalf("ToolContent should be []ContentBlock, got %T", blocks[0].ToolContent)
	}
	if len(nested) != 2 {
		t.Fatalf("expected 2 nested blocks after dropping thinking, got %d: %+v", len(nested), nested)
	}
	if nested[0].Type != "text" || !strings.Contains(nested[0].Text, "~/secret.env") {
		t.Fatalf("nested text not sanitized: %+v", nested[0])
	}
	if nested[1].Type != "image" || nested[1].Source.Data != "kept" {
		t.Fatalf("nested image should pass through unchanged: %+v", nested[1])
	}
}

func TestSanitize_ToolResultStringTruncates(t *testing.T) {
	huge := strings.Repeat("AB", maxToolResultChars) // 2× the cap
	msgs := []client.Message{{
		Role: "user",
		Content: client.NewBlockContent([]client.ContentBlock{
			{Type: "tool_result", ToolUseID: "toolu_1", ToolContent: huge},
		}),
	}}
	got, _ := Sanitize(msgs, nil, Options{})
	out, ok := got[0].Content.Blocks()[0].ToolContent.(string)
	if !ok {
		t.Fatalf("tool_result string should stay string, got %T", got[0].Content.Blocks()[0].ToolContent)
	}
	if len([]rune(out)) > maxToolResultChars {
		t.Fatalf("expected truncation to %d runes, got %d", maxToolResultChars, len([]rune(out)))
	}
	if !strings.Contains(out, "[truncated]") {
		t.Fatalf("expected truncation marker, got tail %q", out[len(out)-50:])
	}
}

func TestSanitize_DropsMessageWithOnlyDroppableBlocks(t *testing.T) {
	msgs := []client.Message{
		{Role: "assistant", Content: client.NewBlockContent([]client.ContentBlock{
			{Type: "thinking", Thinking: "alone"},
		})},
		{Role: "user", Content: client.NewTextContent("survivor")},
	}
	got, gotMeta := Sanitize(msgs, nil, Options{})
	if len(got) != 1 || got[0].Content.Text() != "survivor" {
		t.Fatalf("expected only the survivor message, got %+v", got)
	}
	if len(gotMeta) != 1 {
		t.Fatalf("meta length should match messages: %d", len(gotMeta))
	}
}

func TestSanitize_HandlesShorterMetaSlice(t *testing.T) {
	msgs := []client.Message{
		{Role: "user", Content: client.NewTextContent("a")},
		{Role: "assistant", Content: client.NewTextContent("b")},
		{Role: "user", Content: client.NewTextContent("c")},
	}
	got, _ := Sanitize(msgs, []session.MessageMeta{{}}, Options{})
	if len(got) != 3 {
		t.Fatalf("short meta slice should not drop trailing messages, got %d", len(got))
	}
}

func TestTruncateRunes_PreservesUnicodeBoundaries(t *testing.T) {
	out := truncateRunes("你好世界你好世界你好世界", 5)
	if len([]rune(out)) > 5 {
		t.Fatalf("rune count exceeds cap: %d", len([]rune(out)))
	}
}
