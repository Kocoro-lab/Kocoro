package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/config"
	"github.com/Kocoro-lab/ShanClaw/internal/cwdctx"
)

// TestBuiltinTools_RejectMissingRequiredFields is the registry-wide guard for
// the tool contract documented in AGENTS.md: every builtin that advertises
// required fields must reject each missing field as a
// ValidationError before it can reach a syscall, subprocess, GUI action, or
// external service.
func TestBuiltinTools_RejectMissingRequiredFields(t *testing.T) {
	cfg := &config.Config{Provider: "ollama"}
	reg, _, cleanup := RegisterLocalTools(cfg, nil)
	defer cleanup()
	ctx := cwdctx.WithSessionCWD(context.Background(), t.TempDir())

	tools := reg.All()
	// Runtime-conditional builtins are not present in the base registry until
	// their daemon dependency is available. Exercise their argument gates
	// directly so "all registered tools pass" cannot hide a missing validation
	// bug in Cloud, Calendar, session-search, or memory surfaces.
	tools = append(tools,
		&SessionSearchTool{},
		&MemoryTool{},
		&CalendarCheckPermissionTool{},
		&CalendarRequestPermissionTool{},
		&CalendarListSourcesTool{},
		&CalendarListEventsTool{},
		&CalendarGetEventTool{},
		&CalendarCreateEventTool{},
		&CalendarUpdateEventTool{},
		&CalendarDeleteEventTool{},
		&CloudDelegateTool{},
		&PublishToWebTool{},
		&GenerateImageTool{},
		&EditImageTool{},
		&ListPublishedFilesTool{},
		&RetractPublishedFileTool{},
	)

	for _, tool := range tools {
		info := tool.Info()
		if len(info.Required) == 0 {
			continue
		}
		for _, missing := range info.Required {
			missing := missing
			t.Run(info.Name+"/missing_"+missing, func(t *testing.T) {
				args := validRequiredArgs(info)
				delete(args, missing)
				argsJSON, err := json.Marshal(args)
				if err != nil {
					t.Fatal(err)
				}
				result, err := tool.Run(ctx, string(argsJSON))
				if err != nil {
					t.Fatalf("Run returned framework error: %v", err)
				}
				if !result.IsError {
					t.Fatalf("missing %q unexpectedly succeeded: %q", missing, result.Content)
				}
				if !strings.HasPrefix(result.Content, "[validation error]") {
					t.Fatalf("missing %q returned a non-standard error: %q", missing, result.Content)
				}
				if !strings.Contains(result.Content, missing) {
					t.Fatalf("error for missing %q did not identify the field: %q", missing, result.Content)
				}
			})
		}
	}
}

func validRequiredArgs(info agent.ToolInfo) map[string]any {
	args := make(map[string]any, len(info.Required))
	properties, _ := info.Parameters["properties"].(map[string]any)
	for _, name := range info.Required {
		value := any("contract-test")
		if property, ok := properties[name].(map[string]any); ok {
			switch property["type"] {
			case "integer", "number":
				value = 1
			case "boolean":
				value = true
			case "array":
				value = []any{"contract-test"}
			case "object":
				value = map[string]any{"contract": "test"}
			}
		}
		args[name] = value
	}
	return args
}

func TestServerTool_InfoExtractsRequiredFields(t *testing.T) {
	tool := NewServerTool(client.ServerToolSchema{
		Name: "remote_search",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{"type": "string"},
			},
			"required": []any{"query"},
		},
	}, nil)

	info := tool.Info()
	if len(info.Required) != 1 || info.Required[0] != "query" {
		t.Fatalf("Info.Required = %#v, want [query]", info.Required)
	}
}
