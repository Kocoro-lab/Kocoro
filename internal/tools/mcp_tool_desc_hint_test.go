package tools

import (
	"strings"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/mcp"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

// File-producing tools carry the filename-choice hint; the model otherwise
// self-addresses intermediates as absolute paths under the visible session
// CWD (2026-08-02: browser_snapshot files written to ~/Desktop), which the
// artifact-scratch rewrite deliberately does not touch.
func TestMCPTool_Info_FileProducingToolsCarryFilenameHint(t *testing.T) {
	mgr := mcp.NewClientManager()

	for _, name := range []string{"browser_take_screenshot", "browser_snapshot"} {
		info := NewMCPTool("playwright", mcpgo.Tool{Name: name, Description: "d"}, mgr).Info()
		if !strings.Contains(info.Description, "artifact directory") {
			t.Errorf("%s: description must carry the filename hint, got: %s", name, info.Description)
		}
	}

	// Non-file-producing tools and other servers stay untouched.
	for _, tc := range []struct{ server, name string }{
		{"playwright", "browser_click"},
		{"gws", "browser_take_screenshot"},
	} {
		info := NewMCPTool(tc.server, mcpgo.Tool{Name: tc.name, Description: "d"}, mgr).Info()
		if strings.Contains(info.Description, "artifact directory") {
			t.Errorf("%s/%s: unexpected filename hint in description", tc.server, tc.name)
		}
	}
}

// The hint is appended after the truncation cap so a long server-supplied
// description can never swallow it.
func TestMCPTool_Info_FilenameHintSurvivesTruncation(t *testing.T) {
	mgr := mcp.NewClientManager()
	long := strings.Repeat("x", maxMCPDescLen+100)
	info := NewMCPTool("playwright", mcpgo.Tool{Name: "browser_snapshot", Description: long}, mgr).Info()
	if !strings.Contains(info.Description, "...") {
		t.Fatal("expected truncated description")
	}
	if !strings.Contains(info.Description, "artifact directory") {
		t.Error("filename hint must survive truncation")
	}
}
