package daemon

import (
	"strings"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
)

var implicitPhysicalLocationSafeToolsV1 = map[string]struct{}{
	"ask_user_question": {},
	"calculate":         {},
	"current_time":      {},
	"think":             {},
}

const implicitPhysicalLocationUnavailableContextV1 = "Device physical location capability: unavailable for this request. Do not infer location from IP, network, browser personalization, time zone, or desktop apps. Do not suggest enabling location permissions because this runtime has no GPS location integration. Ask the user for a city or region."

func implicitPhysicalLocationRequestV1(request string) bool {
	request = strings.ToLower(strings.TrimSpace(request))
	if request == "" {
		return false
	}
	for _, explicitApp := range []string{
		"weather app", "maps app", "open weather", "open maps",
		"天气应用", "天气 app", "地图应用", "地图 app", "打开天气", "打开地图",
		"天気アプリ", "マップアプリ",
	} {
		if strings.Contains(request, explicitApp) {
			return false
		}
	}
	// Development work ABOUT location features is not a request FOR the device's
	// location. Without this fence, "帮我写一个获取当前位置的函数" matches
	// 当前位置+获取 and strips the entire tool registry from a coding run.
	for _, developmentContext := range []string{
		"function", "component", "implement", "debug", " code", "codebase",
		"函数", "代码", "组件", "实现", "写一个", "写个", "修复", "实装",
		"関数", "コード", "コンポーネント", "実装",
	} {
		if strings.Contains(request, developmentContext) {
			return false
		}
	}
	for _, directRequest := range []string{
		"where am i", "locate me", "我在哪", "定位我", "どこにいる",
	} {
		if strings.Contains(request, directRequest) {
			return true
		}
	}
	hasLocation := false
	for _, location := range []string{
		"my current location", "device location", "computer location",
		"我现在的位置", "我的当前位置", "当前位置", "设备位置", "电脑位置", "本机位置",
		"現在地", "今いる場所",
	} {
		if strings.Contains(request, location) {
			hasLocation = true
			break
		}
	}
	if !hasLocation {
		return false
	}
	for _, lookup := range []string{
		"search", "find", "determine", "what is", "weather",
		"搜索", "查找", "查询", "获取", "确定", "天气",
		"調べ", "検索", "天気",
	} {
		if strings.Contains(request, lookup) {
			return true
		}
	}
	return false
}

func applyImplicitPhysicalLocationToolPolicyV1(
	registry *agent.ToolRegistry,
	request string,
) bool {
	if registry == nil || !implicitPhysicalLocationRequestV1(request) {
		return false
	}
	for _, name := range registry.Names() {
		if _, safe := implicitPhysicalLocationSafeToolsV1[name]; !safe {
			registry.Remove(name)
		}
	}
	return true
}
