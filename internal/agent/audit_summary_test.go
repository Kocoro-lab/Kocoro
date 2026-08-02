package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/audit"
)

type contentFreeAuditTool struct{}

func (*contentFreeAuditTool) Info() ToolInfo {
	return ToolInfo{Name: "content_free_audit_test"}
}
func (*contentFreeAuditTool) Run(context.Context, string) (ToolResult, error) {
	return ToolResult{}, nil
}
func (*contentFreeAuditTool) RequiresApproval() bool { return false }
func (*contentFreeAuditTool) AuditSummaries(string, string) (string, string) {
	return `{"catalog_ids":["official:pptx"]}`, `{"state":"offered"}`
}

func TestAgentLoopUsesToolOwnedContentFreeAuditSummary(t *testing.T) {
	dir := t.TempDir()
	logger, err := audit.NewAuditLogger(dir)
	if err != nil {
		t.Fatal(err)
	}
	registry := NewToolRegistry()
	registry.Register(&contentFreeAuditTool{})
	loop := NewAgentLoop(nil, registry, "medium", "", 1, 1, 1, nil, logger, nil)
	loop.logAudit("content_free_audit_test", `{"reason":"PRIVATE USER TEXT"}`, "PRIVATE RESULT TEXT", "allow", true, 12, nil)
	if err := logger.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "audit.log"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "PRIVATE") {
		t.Fatalf("private tool text leaked into audit log: %s", data)
	}
	if !strings.Contains(string(data), "official:pptx") || !strings.Contains(string(data), `\"state\":\"offered\"`) {
		t.Fatalf("content-free audit summary missing: %s", data)
	}
}
