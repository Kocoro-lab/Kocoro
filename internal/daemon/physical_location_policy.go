package daemon

import (
	"os"
	"strings"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
)

// implicitPhysicalLocationSafeToolsV1 — current_time is deliberately absent:
// with empty arguments it reports the system timezone/utc_offset, which is
// exactly the timezone inference this policy exists to prevent.
var implicitPhysicalLocationSafeToolsV1 = map[string]struct{}{
	"ask_user_question": {},
	"calculate":         {},
	"think":             {},
}

const implicitPhysicalLocationUnavailableContextV1 = "Device physical location capability: unavailable for this request. Do not infer location from IP, network, browser personalization, time zone, or desktop apps. Do not suggest enabling location permissions because this runtime has no GPS location integration. Ask the user for a city or region."

// physicalLocationPolicyEnabledV1 is the field escape hatch: the classifier is
// substring-based and known-imperfect, and a false positive silently strips the
// entire tool registry. Default on.
func physicalLocationPolicyEnabledV1() bool {
	return os.Getenv("SHANNON_PHYSICAL_LOCATION_POLICY") != "0"
}

func implicitPhysicalLocationRequestV1(request string) bool {
	if !physicalLocationPolicyEnabledV1() {
		return false
	}
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
	// Development or UI work ABOUT a location/position is not a request FOR the
	// device's location. Without this fence, "帮我写一个获取当前位置的函数" or
	// "帮我确定光标当前位置" match a location phrase plus a lookup verb and strip
	// the entire tool registry from an unrelated run. False negatives here fall
	// back to the pre-policy state; false positives cripple a run — prefer FN.
	for _, developmentContext := range []string{
		"function", "component", "implement", "debug", " code", "codebase",
		"cursor", "scroll", "element", "payload", "field", "git ",
		"函数", "代码", "组件", "实现", "写一个", "写个", "修复", "实装",
		"光标", "滚动条", "元素",
		"関数", "コード", "コンポーネント", "実装", "カーソル",
	} {
		if strings.Contains(request, developmentContext) {
			return false
		}
	}
	if directPhysicalLocationQuestionV1(request) {
		return true
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
		"search", "find", "determine", "what is", "what's", "get", "tell", "show", "weather",
		"搜索", "查找", "查询", "查", "获取", "确定", "告诉我", "看看", "天气",
		"調べ", "検索", "教えて", "天気",
	} {
		if strings.Contains(request, lookup) {
			return true
		}
	}
	return false
}

// directPhysicalLocationQuestionV1 anchors the bare where-am-I forms to the end
// of the ask: "我在哪个分支" and "Where am I in the Git history" are everyday
// abstract questions, so mid-sentence occurrences must not classify. The
// unambiguous verb forms match anywhere.
func directPhysicalLocationQuestionV1(request string) bool {
	for _, phrase := range []string{"定位我", "locate me"} {
		if strings.Contains(request, phrase) {
			return true
		}
	}
	trimmed := strings.TrimRight(request, " ?？。.!！")
	trimmed = strings.TrimSuffix(trimmed, " right now")
	trimmed = strings.TrimRight(trimmed, " ")
	for _, suffix := range []string{"我在哪", "我在哪里", "我在哪儿", "where am i", "どこにいる"} {
		if strings.HasSuffix(trimmed, suffix) {
			return true
		}
	}
	return false
}

// applyImplicitPhysicalLocationToolPolicyV1 strips the registry down to the
// safe set. The CALLER owns the placement invariant: this must be the last
// registry mutation before the loop is constructed — any tools.Register* that
// runs after it escapes the strip (session_search and memory_recall are
// location oracles over past sessions and memory).
func applyImplicitPhysicalLocationToolPolicyV1(registry *agent.ToolRegistry) {
	if registry == nil {
		return
	}
	for _, name := range registry.Names() {
		if _, safe := implicitPhysicalLocationSafeToolsV1[name]; !safe {
			registry.Remove(name)
		}
	}
}
