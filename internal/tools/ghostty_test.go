package tools

import (
	"context"
	"testing"
)

func TestAgentColor_Deterministic(t *testing.T) {
	c1 := agentColor("research")
	c2 := agentColor("research")
	if c1 != c2 {
		t.Errorf("same name produced different colors: %s vs %s", c1, c2)
	}
}

func TestAgentColor_DifferentNames(t *testing.T) {
	c1 := agentColor("research")
	c2 := agentColor("code")
	if c1 == c2 {
		t.Errorf("different names produced same color: %s", c1)
	}
}

func TestAgentColor_ValidHex(t *testing.T) {
	c := agentColor("test")
	if len(c) != 7 || c[0] != '#' {
		t.Errorf("expected #RRGGBB format, got: %s", c)
	}
}

func TestHslToRGB(t *testing.T) {
	r, g, b := hslToRGB(0, 1.0, 0.5)
	if r != 255 || g != 0 || b != 0 {
		t.Errorf("expected (255,0,0), got (%d,%d,%d)", r, g, b)
	}
	r, g, b = hslToRGB(120, 1.0, 0.5)
	if r != 0 || g != 255 || b != 0 {
		t.Errorf("expected (0,255,0), got (%d,%d,%d)", r, g, b)
	}
}

func TestTabRegistry_AddAndLookup(t *testing.T) {
	reg := newTabRegistry()
	reg.add("research", tabRef{windowIndex: 1, tabIndex: 2})
	ref, ok := reg.lookup("research")
	if !ok {
		t.Fatal("expected to find 'research'")
	}
	if ref.windowIndex != 1 || ref.tabIndex != 2 {
		t.Errorf("unexpected ref: %+v", ref)
	}
}

func TestTabRegistry_LookupMissing(t *testing.T) {
	reg := newTabRegistry()
	_, ok := reg.lookup("nonexistent")
	if ok {
		t.Error("expected lookup to fail for missing tab")
	}
}

func TestTabRegistry_List(t *testing.T) {
	reg := newTabRegistry()
	reg.add("code", tabRef{windowIndex: 1, tabIndex: 1})
	reg.add("test", tabRef{windowIndex: 1, tabIndex: 2})
	tabs := reg.list()
	if len(tabs) != 2 {
		t.Errorf("expected 2 tabs, got %d", len(tabs))
	}
}

func TestGhosttyTool_Info(t *testing.T) {
	tool := &GhosttyTool{tabs: newTabRegistry()}
	info := tool.Info()
	if info.Name != "ghostty" {
		t.Errorf("expected name 'ghostty', got %s", info.Name)
	}
	if !containsString(info.Required, "action") || !containsString(info.Required, "description") {
		t.Errorf("expected Required to contain 'action' and 'description', got %v", info.Required)
	}
}

func TestGhosttyTool_RequiresApproval(t *testing.T) {
	tool := &GhosttyTool{tabs: newTabRegistry()}
	if !tool.RequiresApproval() {
		t.Error("expected RequiresApproval() = true")
	}
}

func TestGhosttyTool_InvalidAction(t *testing.T) {
	tool := &GhosttyTool{tabs: newTabRegistry()}
	result, err := tool.Run(context.Background(), `{"action":"bogus","description":"test invalid action"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError {
		t.Error("expected error for unknown action")
	}
}

func TestGhosttyTool_InvalidJSON(t *testing.T) {
	tool := &GhosttyTool{tabs: newTabRegistry()}
	result, err := tool.Run(context.Background(), `not json`)
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError {
		t.Error("expected error for invalid JSON")
	}
}

func TestGhosttyTool_ListTabs_Empty(t *testing.T) {
	tool := &GhosttyTool{tabs: newTabRegistry()}
	result, err := tool.Run(context.Background(), `{"action":"list_tabs","description":"test empty tab list"}`)
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Errorf("unexpected error: %s", result.Content)
	}
}

func TestGhosttyTool_SendInput_MissingTarget(t *testing.T) {
	tool := &GhosttyTool{tabs: newTabRegistry()}
	result, err := tool.Run(context.Background(), `{"action":"send_input","text":"hello"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError {
		t.Error("expected error when target is missing")
	}
}

func TestGhosttyTool_SendInput_UnknownTarget(t *testing.T) {
	tool := &GhosttyTool{tabs: newTabRegistry()}
	result, err := tool.Run(context.Background(), `{"action":"send_input","target":"nonexistent","text":"hello"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError {
		t.Error("expected error for unknown target tab")
	}
}

func TestResolveTitle(t *testing.T) {
	if got := resolveTitle("custom", "ls"); got != "custom" {
		t.Errorf("expected 'custom', got %s", got)
	}
	if got := resolveTitle("", "/usr/bin/ls -la"); got != "ls" {
		t.Errorf("expected 'ls', got %s", got)
	}
	if got := resolveTitle("", ""); got != "terminal" {
		t.Errorf("expected 'terminal', got %s", got)
	}
}
