package agent

import (
	"encoding/json"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

// fallbackPreambleFromToolCalls recovers a short user-facing activity update
// from the standard description/purpose arguments when a model emits a silent
// tool batch. It intentionally ignores raw tool arguments and tool names.
func fallbackPreambleFromToolCalls(toolCalls []client.FunctionCall) string {
	for i := range toolCalls {
		var args map[string]json.RawMessage
		if err := json.Unmarshal([]byte(toolCalls[i].ArgumentsString()), &args); err != nil {
			continue
		}
		for _, field := range []string{"description", "purpose"} {
			var value string
			if err := json.Unmarshal(args[field], &value); err != nil {
				continue
			}
			if value = strings.TrimSpace(value); value != "" {
				return finishPreambleSentence(value)
			}
		}
	}
	return ""
}

func finishPreambleSentence(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	last, _ := utf8.DecodeLastRuneInString(text)
	if strings.ContainsRune(".!?。！？", last) {
		return text
	}
	for _, r := range text {
		if unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana) {
			return text + "。"
		}
	}
	return text + "."
}
