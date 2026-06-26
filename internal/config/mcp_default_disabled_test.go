package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAppendDefaultAgentDisabledMCPServer(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	os.WriteFile(cfgPath, []byte("endpoint: https://example.com\n"), 0644)

	if err := AppendDefaultAgentDisabledMCPServer(dir, "longbridge"); err != nil {
		t.Fatalf("append: %v", err)
	}
	data, _ := os.ReadFile(cfgPath)
	if !strings.Contains(string(data), "default_agent_disabled") {
		t.Errorf("config should have mcp.default_agent_disabled, got:\n%s", data)
	}
	if !strings.Contains(string(data), "longbridge") {
		t.Errorf("config should contain longbridge, got:\n%s", data)
	}

	// Idempotent
	if err := AppendDefaultAgentDisabledMCPServer(dir, "longbridge"); err != nil {
		t.Fatalf("re-append: %v", err)
	}
	data, _ = os.ReadFile(cfgPath)
	if strings.Count(string(data), "- longbridge") > 1 {
		t.Errorf("duplicate longbridge not deduped:\n%s", data)
	}

	// Second server — both survive; pre-existing key preserved
	if err := AppendDefaultAgentDisabledMCPServer(dir, "notion"); err != nil {
		t.Fatalf("append second: %v", err)
	}
	data, _ = os.ReadFile(cfgPath)
	if !strings.Contains(string(data), "longbridge") || !strings.Contains(string(data), "notion") {
		t.Errorf("expected both servers, got:\n%s", data)
	}
	if !strings.Contains(string(data), "endpoint") {
		t.Errorf("endpoint key lost:\n%s", data)
	}
}

func TestAppendDefaultAgentDisabledMCPServer_EmptyName(t *testing.T) {
	if err := AppendDefaultAgentDisabledMCPServer(t.TempDir(), ""); err == nil {
		t.Errorf("expected error for empty server name")
	}
}

func TestRemoveDefaultAgentDisabledMCPServer(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := AppendDefaultAgentDisabledMCPServer(dir, "longbridge"); err != nil {
		t.Fatal(err)
	}
	if err := AppendDefaultAgentDisabledMCPServer(dir, "notion"); err != nil {
		t.Fatal(err)
	}

	if err := RemoveDefaultAgentDisabledMCPServer(dir, "longbridge"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	data, _ := os.ReadFile(cfgPath)
	if strings.Contains(string(data), "- longbridge") {
		t.Errorf("longbridge should be removed, got:\n%s", data)
	}
	if !strings.Contains(string(data), "notion") {
		t.Errorf("notion should remain, got:\n%s", data)
	}

	// Remove the last one — block cleaned up
	if err := RemoveDefaultAgentDisabledMCPServer(dir, "notion"); err != nil {
		t.Fatalf("remove last: %v", err)
	}
	data, _ = os.ReadFile(cfgPath)
	if strings.Contains(string(data), "- notion") || strings.Contains(string(data), "default_agent_disabled") {
		t.Errorf("empty default_agent_disabled key should be dropped, got:\n%s", data)
	}

	// Absent server is a no-op
	if err := RemoveDefaultAgentDisabledMCPServer(dir, "never_added"); err != nil {
		t.Errorf("removing absent server should not error: %v", err)
	}
	// Non-existent config is a no-op
	if err := RemoveDefaultAgentDisabledMCPServer(t.TempDir(), "longbridge"); err != nil {
		t.Errorf("removing from non-existent config should be no-op: %v", err)
	}
}
