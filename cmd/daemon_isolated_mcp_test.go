package cmd

import (
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/config"
	daemonpkg "github.com/Kocoro-lab/ShanClaw/internal/daemon"
	"github.com/Kocoro-lab/ShanClaw/internal/mcp"
)

func isolatedMCPTestConfig() *config.Config {
	return &config.Config{
		MCPServers: map[string]mcp.MCPServerConfig{
			"playwright":       {Command: "playwright-mcp"},
			"google-workspace": {Command: "uvx"},
			"already-off":      {Command: "noop", Disabled: true},
		},
	}
}

func TestRestrictMCPServersToAllowlist(t *testing.T) {
	tests := []struct {
		name        string
		allowlist   string
		wantKept    []string
		wantEnabled map[string]bool
	}{
		{
			name:        "empty allowlist keeps MCP fully disabled",
			allowlist:   "",
			wantKept:    nil,
			wantEnabled: map[string]bool{"playwright": false, "google-workspace": false, "already-off": false},
		},
		{
			name:        "named server survives, every other server is disabled",
			allowlist:   "playwright",
			wantKept:    []string{"playwright"},
			wantEnabled: map[string]bool{"playwright": true, "google-workspace": false, "already-off": false},
		},
		{
			name:        "whitespace and multiple names",
			allowlist:   " playwright , google-workspace ",
			wantKept:    []string{"google-workspace", "playwright"},
			wantEnabled: map[string]bool{"playwright": true, "google-workspace": true, "already-off": false},
		},
		{
			// A typo must not fall through to "start everything": the caller
			// treats an empty return as "keep MCP off", so the run stays as
			// contained as it was before the flag existed.
			name:        "unknown name fails closed",
			allowlist:   "playwrite",
			wantKept:    nil,
			wantEnabled: map[string]bool{"playwright": false, "google-workspace": false, "already-off": false},
		},
		{
			// An explicitly disabled server is not resurrected by naming it.
			name:        "allowlisting a disabled server does not enable it",
			allowlist:   "already-off",
			wantKept:    nil,
			wantEnabled: map[string]bool{"playwright": false, "google-workspace": false, "already-off": false},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := isolatedMCPTestConfig()
			kept := daemonpkg.RestrictMCPServersToAllowlist(cfg, tt.allowlist)

			if len(kept) != len(tt.wantKept) {
				t.Fatalf("kept = %v, want %v", kept, tt.wantKept)
			}
			for i, name := range tt.wantKept {
				if kept[i] != name {
					t.Fatalf("kept = %v, want %v", kept, tt.wantKept)
				}
			}
			for name, wantEnabled := range tt.wantEnabled {
				if enabled := !cfg.MCPServers[name].Disabled; enabled != wantEnabled {
					t.Errorf("%s enabled = %v, want %v", name, enabled, wantEnabled)
				}
			}
		})
	}
}

func TestRestrictMCPServersToAllowlistHandlesNilConfig(t *testing.T) {
	if kept := daemonpkg.RestrictMCPServersToAllowlist(nil, "playwright"); kept != nil {
		t.Errorf("nil config should keep MCP off, got %v", kept)
	}
}
