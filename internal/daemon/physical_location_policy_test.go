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

func (physicalLocationPolicyTool) Run(context.Context, string) (agent.ToolResult, error) {
	return agent.ToolResult{}, nil
}

func (physicalLocationPolicyTool) RequiresApproval() bool { return false }

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

func TestImplicitPhysicalLocationRequest(t *testing.T) {
	positives := []string{
		"搜索我现在的位置，并查询该地今天的天气。",
		"Where am I right now?",
		"我在哪？",
		"現在地を調べて",
		// Direct phrasings previously missed by the lookup-verb list.
		"Get my current location.",
		"Tell me my current location",
		"What's my current location?",
		"查一下我现在的位置",
	}
	for _, request := range positives {
		if !implicitPhysicalLocationRequestV1(request) {
			t.Errorf("request %q was not detected", request)
		}
	}
	negatives := []string{
		"打开天气应用，读取应用里当前显示的位置和天气。",
		"Open Maps and show its current-location marker.",
		"查询东京今天的天气。",
		"修复“当前位置”组件无法显示的问题。",
		"帮我写一个获取当前位置的函数。",
		"用 CoreLocation 实现获取设备位置，查一下 API 文档。",
		"Implement a search box that finds my current location on the map.",
		"現在地を取得する関数を実装して、動作を調べて。",
		// Abstract where-am-I and UI-position phrasings that previously
		// classified and stripped the registry from unrelated runs.
		"我在哪个分支？",
		"我在哪个目录下",
		"帮我确定光标当前位置",
		"获取滚动条当前位置",
		"Determine the device location field in this payload.",
		"Where am I in the Git history?",
	}
	for _, request := range negatives {
		if implicitPhysicalLocationRequestV1(request) {
			t.Errorf("request %q was wrongly classified as a device-location request", request)
		}
	}
}

func TestImplicitPhysicalLocationPolicyKillSwitch(t *testing.T) {
	t.Setenv("SHANNON_PHYSICAL_LOCATION_POLICY", "0")
	if implicitPhysicalLocationRequestV1("Where am I right now?") {
		t.Fatal("kill switch did not disable the classifier")
	}
}

func TestApplyImplicitPhysicalLocationToolPolicy(t *testing.T) {
	registry := agent.NewToolRegistry()
	for _, name := range []string{
		"computer_use", "bash", "http", "browser", "web_search", "web_fetch",
		"browser_tabs", "browser_navigate", "weather_provider",
		"session_search", "memory_recall", "use_skill", "current_time",
		"calculate", "think", "ask_user_question",
	} {
		registry.Register(physicalLocationPolicyTool(name))
	}
	applyImplicitPhysicalLocationToolPolicyV1(registry)
	for _, removed := range []string{
		"computer_use", "bash", "http", "browser", "web_search", "web_fetch",
		"browser_tabs", "browser_navigate", "weather_provider",
		// Location oracles over stored data, and the timezone reporter.
		"session_search", "memory_recall", "use_skill", "current_time",
	} {
		if registry.Has(removed) {
			t.Errorf("unsafe location inference tool %q remains", removed)
		}
	}
	for _, preserved := range []string{"calculate", "think", "ask_user_question"} {
		if !registry.Has(preserved) {
			t.Errorf("unrelated tool %q was removed", preserved)
		}
	}
}
