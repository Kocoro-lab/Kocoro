package agent

import (
	"strings"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

func TestBuildContextBloatSuggestion_FileReadDominates(t *testing.T) {
	msgs := []client.Message{
		{Role: "assistant", Content: client.NewBlockContent([]client.ContentBlock{
			client.NewToolUseBlock("toolu_read", "file_read", nil),
		})},
		{Role: "user", Content: client.NewBlockContent([]client.ContentBlock{
			client.NewToolResultBlock("toolu_read", strings.Repeat("x", 9000), false),
		})},
	}
	got := buildContextBloatSuggestion(msgs, ContextBloatOptions{
		TotalToolResultBytes: 5000,
	})
	if !strings.Contains(got, "file_read") || !strings.Contains(got, "offset+limit") {
		t.Fatalf("unexpected reminder: %q", got)
	}
}

func TestBuildContextBloatSuggestion_SmallContextNoop(t *testing.T) {
	msgs := []client.Message{
		{Role: "assistant", Content: client.NewBlockContent([]client.ContentBlock{
			client.NewToolUseBlock("toolu_grep", "grep", nil),
		})},
		{Role: "user", Content: client.NewBlockContent([]client.ContentBlock{
			client.NewToolResultBlock("toolu_grep", "short", false),
		})},
	}
	if got := buildContextBloatSuggestion(msgs, ContextBloatOptions{TotalToolResultBytes: 5000}); got != "" {
		t.Fatalf("expected no reminder, got %q", got)
	}
}
