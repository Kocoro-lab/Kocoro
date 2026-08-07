package agent

import (
	"fmt"
	"strings"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

func TestWorkingSet_AddAndContains(t *testing.T) {
	ws := NewWorkingSet()
	schema := client.Tool{Type: "function", Function: client.FunctionDef{Name: "browser_navigate"}}

	ws.Add("browser_navigate", schema)
	if !ws.Contains("browser_navigate") {
		t.Error("working set should contain added tool")
	}
	if ws.Contains("browser_click") {
		t.Error("working set should not contain unadded tool")
	}
}

func TestWorkingSet_Schemas(t *testing.T) {
	ws := NewWorkingSet()
	ws.Add("b_tool", client.Tool{Type: "function", Function: client.FunctionDef{Name: "b_tool"}})
	ws.Add("a_tool", client.Tool{Type: "function", Function: client.FunctionDef{Name: "a_tool"}})

	schemas := ws.Schemas()
	if len(schemas) != 2 {
		t.Fatalf("expected 2 schemas, got %d", len(schemas))
	}
	if _, ok := schemas["a_tool"]; !ok {
		t.Error("schemas copy should contain a_tool")
	}
	if _, ok := schemas["b_tool"]; !ok {
		t.Error("schemas copy should contain b_tool")
	}
}

func TestWorkingSet_Len(t *testing.T) {
	ws := NewWorkingSet()
	if ws.Len() != 0 {
		t.Error("empty working set should have length 0")
	}
	ws.Add("x", client.Tool{Type: "function", Function: client.FunctionDef{Name: "x"}})
	if ws.Len() != 1 {
		t.Errorf("expected length 1, got %d", ws.Len())
	}
	ws.Add("x", client.Tool{Type: "function", Function: client.FunctionDef{Name: "x"}})
	if ws.Len() != 1 {
		t.Errorf("expected length 1 after duplicate add, got %d", ws.Len())
	}
}

func TestWorkingSet_EvictsOldestSchemaAtCountLimit(t *testing.T) {
	ws := NewWorkingSet()
	for i := 0; i <= workingSetSchemaCountLimit; i++ {
		name := fmt.Sprintf("tool_%02d", i)
		ws.Add(name, client.Tool{Type: "function", Function: client.FunctionDef{Name: name}})
	}
	if ws.Len() != workingSetSchemaCountLimit {
		t.Fatalf("working set length = %d, want %d", ws.Len(), workingSetSchemaCountLimit)
	}
	if ws.Contains("tool_00") {
		t.Fatal("working set retained the oldest schema past the count limit")
	}
	if !ws.Contains(fmt.Sprintf("tool_%02d", workingSetSchemaCountLimit)) {
		t.Fatal("working set evicted the newest schema")
	}
}

func TestWorkingSet_DoesNotPersistSchemaOverTokenLimit(t *testing.T) {
	ws := NewWorkingSet()
	name := "oversized"
	ws.Add(name, client.Tool{
		Type: "function",
		Function: client.FunctionDef{
			Name:        name,
			Description: strings.Repeat("x", workingSetSchemaTokenLimit*4),
		},
	})
	if ws.Contains(name) || ws.Len() != 0 {
		t.Fatalf("oversized schema persisted in working set: %+v", ws.Schemas())
	}
}

func TestWorkingSet_TokenLimitEvictsOldestBeforeNewSchema(t *testing.T) {
	ws := NewWorkingSet()
	description := strings.Repeat("x", workingSetSchemaTokenLimit*2)
	ws.Add("old", client.Tool{Type: "function", Function: client.FunctionDef{Name: "old", Description: description}})
	ws.Add("new", client.Tool{Type: "function", Function: client.FunctionDef{Name: "new", Description: description}})
	if ws.Contains("old") {
		t.Fatal("token budget retained the oldest schema")
	}
	if !ws.Contains("new") {
		t.Fatal("token budget evicted the newest schema")
	}
}

func TestWorkingSet_Invalidate(t *testing.T) {
	ws := NewWorkingSet()
	ws.Add("x", client.Tool{Type: "function", Function: client.FunctionDef{Name: "x"}})
	ws.Invalidate()
	if ws.Contains("x") {
		t.Error("invalidated working set should be empty")
	}
	if ws.Len() != 0 {
		t.Error("invalidated working set should have length 0")
	}
	if ws.Fingerprint() != "" {
		t.Error("invalidated working set should clear fingerprint")
	}
}

func TestWorkingSet_SyncToolsetInvalidatesOnFingerprintChange(t *testing.T) {
	reg1 := NewToolRegistry()
	reg1.Register(&mockTool{name: "bash"})
	reg1.Register(&mockMCPTool{name: "browser_click"})

	reg2 := NewToolRegistry()
	reg2.Register(&mockTool{name: "bash"})
	reg2.Register(&mockMCPTool{name: "browser_type"})

	ws := NewWorkingSet()
	if !ws.SyncToolset(reg1) {
		t.Error("first sync should establish fingerprint")
	}

	ws.Add("browser_click", buildToolSchema(&mockMCPTool{name: "browser_click"}))
	if !ws.Contains("browser_click") {
		t.Fatal("working set should contain warmed schema before fingerprint change")
	}

	if ws.SyncToolset(reg1) {
		t.Error("same toolset fingerprint should not invalidate working set")
	}
	if !ws.Contains("browser_click") {
		t.Fatal("working set should survive same fingerprint")
	}

	if !ws.SyncToolset(reg2) {
		t.Error("changed toolset fingerprint should invalidate working set")
	}
	if ws.Contains("browser_click") {
		t.Error("working set should clear stale warmed schemas on fingerprint change")
	}
}
