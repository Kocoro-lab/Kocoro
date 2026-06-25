package tui

import (
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

// TestSearchHistory: Ctrl+R finds the most-recent past input containing the
// query (case-insensitive); empty/no-match returns ok=false.
func TestSearchHistory(t *testing.T) {
	h := []string{"deploy the app", "run tests", "deploy again"}
	if got, ok := searchHistory(h, "deploy"); !ok || got != "deploy again" {
		t.Errorf("want most-recent match 'deploy again', got %q ok=%v", got, ok)
	}
	if got, ok := searchHistory(h, "TESTS"); !ok || got != "run tests" {
		t.Errorf("case-insensitive match failed, got %q", got)
	}
	if _, ok := searchHistory(h, "nope"); ok {
		t.Error("no match should return ok=false")
	}
	if _, ok := searchHistory(h, ""); ok {
		t.Error("empty query should return ok=false")
	}
}

// TestForkMessages: a fork copies all messages into an INDEPENDENT slice so the
// original conversation is untouched when the fork continues.
func TestForkMessages(t *testing.T) {
	src := []client.Message{
		{Role: "user", Content: client.NewTextContent("a")},
		{Role: "assistant", Content: client.NewTextContent("b")},
	}
	fork := forkMessages(src)
	if len(fork) != 2 {
		t.Fatalf("fork should copy all messages, got %d", len(fork))
	}
	fork = append(fork, client.Message{Role: "user", Content: client.NewTextContent("c")})
	if len(src) != 2 {
		t.Error("fork must be an independent slice (appending changed src)")
	}
}
