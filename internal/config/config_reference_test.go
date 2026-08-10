package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigReferenceTracksAgentDefaults(t *testing.T) {
	path := filepath.Join("..", "..", "docs", "config-reference.md")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	wants := []string{
		fmt.Sprintf("max_iterations: %d", DefaultAgentMaxIterations),
		fmt.Sprintf("max_tokens: %d", DefaultAgentMaxTokens),
		"emergency model/tool-round fuse",
		"resolve from the selected model",
	}
	for _, want := range wants {
		if !strings.Contains(text, want) {
			t.Errorf("config reference does not document %q", want)
		}
	}
}
