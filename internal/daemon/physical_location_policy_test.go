package daemon

import (
	"context"
	"strings"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
)

type physicalLocationPolicyTool string

func (t physicalLocationPolicyTool) Info() agent.ToolInfo {
	return agent.ToolInfo{Name: string(t)}
}

func TestImplicitPhysicalLocationGuidanceStatesCapabilityBoundary(t *testing.T) {
	for _, required := range []string{
		"no GPS location integration",
		"Do not suggest enabling location permissions",
		"Ask the user for a city or region",
	} {
		if !strings.Contains(implicitPhysicalLocationUnavailableContextV1, required) {
			t.Errorf("guidance missing %q", required)
		}
	}
}

func (physicalLocationPolicyTool) Run(context.Context, string) (agent.ToolResult, error) {
	return agent.ToolResult{}, nil
}

func (physicalLocationPolicyTool) RequiresApproval() bool { return false }

func TestImplicitPhysicalLocationRequest(t *testing.T) {
	for _, request := range []string{
		"搜索我现在的位置，并查询该地今天的天气。",
		"Where am I right now?",
		"現在地を調べて",
	} {
		if !implicitPhysicalLocationRequestV1(request) {
			t.Errorf("request %q was not detected", request)
		}
	}
	for _, request := range []string{
		"打开天气应用，读取应用里当前显示的位置和天气。",
		"Open Maps and show its current-location marker.",
		"查询东京今天的天气。",
		"修复“当前位置”组件无法显示的问题。",
	} {
		if implicitPhysicalLocationRequestV1(request) {
			t.Errorf("explicit or city request %q was blocked", request)
		}
	}
}

func TestApplyImplicitPhysicalLocationToolPolicy(t *testing.T) {
	registry := agent.NewToolRegistry()
	for _, name := range []string{
		"computer_use", "bash", "http", "browser", "web_search", "web_fetch",
		"browser_tabs", "browser_navigate", "weather_provider",
		"current_time", "calculate", "think", "ask_user_question",
	} {
		registry.Register(physicalLocationPolicyTool(name))
	}
	if !applyImplicitPhysicalLocationToolPolicyV1(
		registry,
		"搜索我现在的位置，并查询该地今天的天气。",
	) {
		t.Fatal("location policy did not activate")
	}
	for _, removed := range []string{
		"computer_use", "bash", "http", "browser", "web_search", "web_fetch",
		"browser_tabs", "browser_navigate", "weather_provider",
	} {
		if registry.Has(removed) {
			t.Errorf("unsafe location inference tool %q remains", removed)
		}
	}
	for _, preserved := range []string{"current_time", "calculate", "think", "ask_user_question"} {
		if !registry.Has(preserved) {
			t.Errorf("unrelated tool %q was removed", preserved)
		}
	}
}
