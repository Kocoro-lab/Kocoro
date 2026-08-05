package agent

import (
	"encoding/json"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

const (
	maxFallbackPreambleRunes         = 200
	maxToolContinuationPreambleRunes = 320
)

var toolContinuationOpeners = []string{
	"let me ",
	"now let me ",
	"next let me ",
	"next, let me ",
	"i'll now ",
	"i will now ",
	"i'm going to ",
	"i am going to ",
	"接下来我会",
	"接下来让我",
	"现在让我",
	"下面我将",
	"次に",
	"これから",
}

var toolContinuationActionWords = map[string]struct{}{
	"read":     {},
	"fetch":    {},
	"open":     {},
	"inspect":  {},
	"check":    {},
	"search":   {},
	"retrieve": {},
	"load":     {},
	"call":     {},
	"use":      {},
	"browse":   {},
	"verify":   {},
}

var toolContinuationActionFragments = []string{
	"look up",
	"读取",
	"查看",
	"打开",
	"检查",
	"搜索",
	"调用",
	"使用",
	"确认",
	"読む",
	"開く",
	"確認",
	"検索",
	"呼び出",
	"使う",
}

// looksLikeToolContinuationPreamble identifies a narrow class of future-tool
// narration that cannot be a final answer. It intentionally requires both a
// future-action opener and an inspection/tool verb so short complete answers
// that merely mention next steps are not forced into another model call.
func looksLikeToolContinuationPreamble(text string) bool {
	text = strings.TrimSpace(text)
	if text == "" || utf8.RuneCountInString(text) > maxToolContinuationPreambleRunes || strings.Contains(text, "\n\n") {
		return false
	}
	lower := strings.ToLower(text)
	hasOpener := false
	for _, opener := range toolContinuationOpeners {
		if strings.HasPrefix(lower, opener) {
			hasOpener = true
			break
		}
	}
	if !hasOpener {
		return false
	}
	for _, word := range strings.FieldsFunc(lower, func(r rune) bool {
		return !unicode.IsLetter(r)
	}) {
		if _, ok := toolContinuationActionWords[word]; ok {
			return true
		}
	}
	for _, action := range toolContinuationActionFragments {
		if strings.Contains(lower, action) {
			return true
		}
	}
	return false
}

// fallbackPreambleFromToolCalls recovers a short user-facing activity update
// from a local tool's required description argument when a model emits a
// silent tool batch. External schemas may use "description" for arbitrary
// business content, so MCP, gateway, and integration calls never qualify.
func fallbackPreambleFromToolCalls(toolCalls []client.FunctionCall, registry *ToolRegistry) string {
	if registry == nil {
		return ""
	}
	for i := range toolCalls {
		tool, ok := registry.Get(toolCalls[i].Name)
		if !ok || !hasLocalPreambleDescription(tool) {
			continue
		}
		var args map[string]json.RawMessage
		if err := json.Unmarshal([]byte(toolCalls[i].ArgumentsString()), &args); err != nil {
			continue
		}
		var value string
		if err := json.Unmarshal(args["description"], &value); err != nil {
			continue
		}
		if value = strings.TrimSpace(value); value != "" {
			return finishPreambleSentence(value)
		}
	}
	return ""
}

func hasLocalPreambleDescription(tool Tool) bool {
	if sourcer, ok := tool.(ToolSourcer); ok && sourcer.ToolSource() != SourceLocal {
		return false
	}
	info := tool.Info()
	required := false
	for _, field := range info.Required {
		if field == "description" {
			required = true
			break
		}
	}
	if !required {
		return false
	}
	properties, ok := info.Parameters["properties"].(map[string]any)
	if !ok {
		return false
	}
	_, declared := properties["description"]
	return declared
}

func finishPreambleSentence(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	runes := []rune(text)
	if len(runes) > maxFallbackPreambleRunes {
		return string(runes[:maxFallbackPreambleRunes-1]) + "…"
	}
	last, _ := utf8.DecodeLastRuneInString(text)
	if strings.ContainsRune(".!?。！？…", last) {
		return text
	}
	if len(runes) == maxFallbackPreambleRunes {
		return string(runes[:maxFallbackPreambleRunes-1]) + "…"
	}
	if unicode.In(last, unicode.Han, unicode.Hiragana, unicode.Katakana) {
		return text + "。"
	}
	return text + "."
}
