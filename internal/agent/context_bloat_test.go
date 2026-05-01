package agent

import (
	"strings"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

func TestBuildContextBloatReminder_FileReadDominates(t *testing.T) {
	msgs := []client.Message{
		{Role: "assistant", Content: client.NewBlockContent([]client.ContentBlock{
			client.NewToolUseBlock("toolu_read", "file_read", nil),
		})},
		{Role: "user", Content: client.NewBlockContent([]client.ContentBlock{
			client.NewToolResultBlock("toolu_read", strings.Repeat("x", 9000), false),
		})},
	}
	got := buildContextBloatReminder(msgs, ContextBloatOptions{
		RecentToolResultBytes: 5000,
	})
	if !strings.Contains(got, "file_read") || !strings.Contains(got, "offset+limit") {
		t.Fatalf("unexpected reminder: %q", got)
	}
}

func TestBuildContextBloatReminder_SmallContextNoop(t *testing.T) {
	msgs := []client.Message{
		{Role: "assistant", Content: client.NewBlockContent([]client.ContentBlock{
			client.NewToolUseBlock("toolu_grep", "grep", nil),
		})},
		{Role: "user", Content: client.NewBlockContent([]client.ContentBlock{
			client.NewToolResultBlock("toolu_grep", "short", false),
		})},
	}
	if got := buildContextBloatReminder(msgs, ContextBloatOptions{RecentToolResultBytes: 5000}); got != "" {
		t.Fatalf("expected no reminder, got %q", got)
	}
}
