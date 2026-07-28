package agent

import (
	"encoding/json"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

const maxFallbackPreambleRunes = 200

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
