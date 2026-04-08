package tui

import (
	"strings"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/config"
)

func TestRunDoctor_ContainsVersion(t *testing.T) {
	cfg := &config.Config{
		Endpoint: "https://api.example.com",
		APIKey:   "sk-test-key-1234567890",
	}
	result := runDoctor(cfg, "0.0.42", 28)
	if !strings.Contains(result, "0.0.42") {
		t.Error("doctor output should contain version")
	}
}

func TestRunDoctor_APIKeyConfigured(t *testing.T) {
	cfg := &config.Config{APIKey: "sk-test-key-1234567890"}
	result := runDoctor(cfg, "0.0.1", 0)
	if !strings.Contains(result, "configured") {
		t.Error("should show API key as configured")
	}
}

func TestRunDoctor_APIKeyMissing(t *testing.T) {
	cfg := &config.Config{}
	result := runDoctor(cfg, "0.0.1", 0)
	if !strings.Contains(result, "not configured") {
		t.Error("should show API key as not configured")
	}
}

func TestRunDoctor_ShowsToolCount(t *testing.T) {
	cfg := &config.Config{}
	result := runDoctor(cfg, "0.0.1", 28)
	if !strings.Contains(result, "28 registered") {
		t.Errorf("should show tool count, got:\n%s", result)
	}
}

func TestMaskAPIKey(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"sk-test-key-1234567890", "sk-t...7890"},
		{"short", "***"},
		{"", "***"},
	}
	for _, tt := range tests {
		got := maskAPIKey(tt.in)
		if got != tt.want {
			t.Errorf("maskAPIKey(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
