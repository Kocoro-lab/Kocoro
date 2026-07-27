package agent

import (
	"context"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

type exposureTestTool struct {
	name     string
	source   ToolSource
	exposure ToolExposure
	explicit bool
}

func (t *exposureTestTool) Info() ToolInfo {
	return ToolInfo{
		Name:        t.name,
		Description: "exposure test tool",
		Parameters:  map[string]any{"type": "object", "properties": map[string]any{}},
	}
}

func (t *exposureTestTool) Run(context.Context, string) (ToolResult, error) {
	return ToolResult{}, nil
}

func (t *exposureTestTool) RequiresApproval() bool { return false }
func (t *exposureTestTool) ToolSource() ToolSource { return t.source }

func (t *exposureTestTool) ToolExposure() ToolExposure {
	if !t.explicit {
		return ToolExposureDefault
	}
	return t.exposure
}

func TestEffectiveToolExposure_DefaultsBySource(t *testing.T) {
	tests := []struct {
		name   string
		source ToolSource
		want   ToolExposure
	}{
		{name: "local defaults direct", source: SourceLocal, want: ToolExposureDirect},
		{name: "mcp defaults deferred", source: SourceMCP, want: ToolExposureDeferred},
		{name: "gateway defaults deferred", source: SourceGateway, want: ToolExposureDeferred},
		{name: "integration defaults deferred", source: SourceIntegration, want: ToolExposureDeferred},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tool := &exposureTestTool{name: "test_tool", source: tc.source}
			if got := EffectiveToolExposure(tool); got != tc.want {
				t.Fatalf("EffectiveToolExposure() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestEffectiveToolExposure_ExplicitOverrideWins(t *testing.T) {
	tests := []struct {
		name     string
		source   ToolSource
		explicit ToolExposure
	}{
		{name: "local deferred override", source: SourceLocal, explicit: ToolExposureDeferred},
		{name: "mcp direct override", source: SourceMCP, explicit: ToolExposureDirect},
		{name: "gateway direct override", source: SourceGateway, explicit: ToolExposureDirect},
		{name: "integration direct override", source: SourceIntegration, explicit: ToolExposureDirect},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tool := &exposureTestTool{
				name:     "test_tool",
				source:   tc.source,
				exposure: tc.explicit,
				explicit: true,
			}
			if got := EffectiveToolExposure(tool); got != tc.explicit {
				t.Fatalf("EffectiveToolExposure() = %v, want explicit %v", got, tc.explicit)
			}
		})
	}
}

func TestDeferredToolNames_DirectToolUnaffectedByDeferredNeighbor(t *testing.T) {
	reg := NewToolRegistry()
	reg.Register(&exposureTestTool{name: "file_read", source: SourceLocal})
	reg.Register(&exposureTestTool{
		name:     "computer",
		source:   SourceLocal,
		exposure: ToolExposureDeferred,
		explicit: true,
	})
	reg.Register(&exposureTestTool{
		name:     "gateway_opener",
		source:   SourceGateway,
		exposure: ToolExposureDirect,
		explicit: true,
	})
	reg.Register(&exposureTestTool{name: "gateway_long_tail", source: SourceGateway})

	deferred := deferredToolNames(reg)
	if deferred["file_read"] {
		t.Fatal("Direct local tool became Deferred because a Deferred neighbor was registered")
	}
	if deferred["gateway_opener"] {
		t.Fatal("explicit Direct gateway tool became Deferred")
	}
	for _, name := range []string{"computer", "gateway_long_tail"} {
		if !deferred[name] {
			t.Fatalf("expected %q to be Deferred, got %v", name, deferred)
		}
	}
}

func TestBuildActiveSchemas_IncludesExplicitDirectDynamicSource(t *testing.T) {
	reg := NewToolRegistry()
	reg.Register(&exposureTestTool{name: "file_read", source: SourceLocal})
	reg.Register(&exposureTestTool{
		name:     "gateway_opener",
		source:   SourceGateway,
		exposure: ToolExposureDirect,
		explicit: true,
	})
	reg.Register(&exposureTestTool{name: "gateway_long_tail", source: SourceGateway})

	schemas := buildActiveSchemas(reg, deferredToolNames(reg))
	got := liveToolNames(schemas)
	want := []string{"file_read", "gateway_opener"}
	if len(got) != len(want) {
		t.Fatalf("active schema names = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("active schema names = %v, want %v", got, want)
		}
	}
}

func TestWorkingSet_SyncToolsetInvalidatesOnSourceOrExposureChange(t *testing.T) {
	reg := NewToolRegistry()
	reg.Register(&exposureTestTool{name: "same_schema", source: SourceLocal})

	ws := NewWorkingSet()
	if !ws.SyncToolset(reg) {
		t.Fatal("initial SyncToolset must establish a fingerprint")
	}
	ws.Add("same_schema", client.Tool{Type: "function"})

	reg.Register(&exposureTestTool{name: "same_schema", source: SourceMCP})
	if !ws.SyncToolset(reg) {
		t.Fatal("source change with identical schema must invalidate the working set")
	}
	if ws.Len() != 0 {
		t.Fatalf("working set length after source change = %d, want 0", ws.Len())
	}

	ws.Add("same_schema", client.Tool{Type: "function"})
	reg.Register(&exposureTestTool{
		name:     "same_schema",
		source:   SourceMCP,
		exposure: ToolExposureDirect,
		explicit: true,
	})
	if !ws.SyncToolset(reg) {
		t.Fatal("exposure change with identical schema must invalidate the working set")
	}
	if ws.Len() != 0 {
		t.Fatalf("working set length after exposure change = %d, want 0", ws.Len())
	}
}
