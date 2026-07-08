package koe

import (
	"os"
	"strings"
	"unicode"
)

// baseDismissPhrases is the curated closed-vocabulary of "end the conversation"
// control phrases, stored normalized (see normalizeDismissPhrase). A whole-utterance
// match here deterministically hangs the call up — the industry pattern for
// latency-critical control words (Alexa Follow-Up Mode exit = "stop / cancel / go to
// sleep / thank you"; Siri/Alexa wake+stop = on-device fixed-vocabulary KWS, NOT the
// cloud LLM). This is the RELIABLE half of a two-path design: the end_call TOOL
// (model judgment, reinforced in koePersona) catches open phrasings the list lacks
// ("今天就到这里吧"), and this fixed-vocabulary gate guarantees the common exact words
// regardless of the model. Both converge on the idempotent onEndCall, so a racing
// double-fire is harmless. Rationale for not trusting the tool alone: before the
// koePersona reinforcement, gpt-realtime-mini called end_call for only 1 of ~7
// explicit dismissals (it verbally acknowledged instead, e.g. "取消并且退出" → spoke
// "结束对话并退出，再见" but never hung up). Matching is whole-utterance exact (not
// substring) to keep false-hangups near zero. Extend at runtime with
// KOE_DISMISS_PHRASES; KOE_DISMISS_DETECT=0 is the kill switch.
var baseDismissPhrases = map[string]struct{}{
	// en — quit / dismiss / stop-talking
	"stop": {}, "stop it": {}, "shut up": {}, "be quiet": {}, "stop talking": {},
	"quiet": {}, "enough": {}, "that's enough": {}, "thats enough": {}, "goodbye": {},
	"bye": {}, "exit": {}, "quit": {}, "dismiss": {}, "that's all": {}, "thats all": {},
	"go to sleep": {}, "hush": {}, "silence": {},
	// en "stop"/"exit" is transcribed in other scripts in a non-en conversation
	// (observed: Cyrillic "Стоп." / "Питчу." in a zh call).
	"стоп": {}, "выход": {},
	// zh (simplified)
	"停": {}, "停止": {}, "停一下": {}, "停一停": {}, "停下": {}, "停下来": {}, "打住": {},
	"别说了": {}, "别讲了": {}, "别说话": {}, "别说话了": {}, "不要说了": {}, "别念了": {},
	"闭嘴": {}, "住口": {}, "住嘴": {}, "安静": {}, "安静点": {}, "够了": {},
	"退出": {}, "结束": {}, "结束对话": {}, "再见": {}, "拜拜": {}, "拜": {}, "就这样": {},
	"没事了": {}, "取消并退出": {}, "取消并且退出": {}, // bare "取消" is the cancel TOOL's job (cancel a task, not hang up)
	// zh (traditional; simple/trad-identical forms live in the simplified block)
	"停下來": {}, "別說了": {}, "別講了": {}, "別說話": {}, "別說話了": {}, "不要說了": {},
	"別念了": {}, "閉嘴": {}, "安靜": {}, "安靜點": {}, "夠了": {}, "結束": {},
	"結束對話": {}, "再見": {}, "退出對話": {},
	// ja — plain, rough-imperative, quiet, and dismiss forms
	"止まって": {}, "止まれ": {}, "止めて": {}, "やめて": {}, "やめろ": {}, "もうやめて": {},
	"黙って": {}, "黙れ": {}, "静かに": {}, "うるさい": {}, "ストップ": {}, "もういい": {},
	"終わり": {}, "終了": {}, "さようなら": {}, "バイバイ": {}, "終わって": {},
}

// normalizeDismissPhrase strips surrounding whitespace and punctuation (ASCII and CJK,
// e.g. "." "!" "。" "！" "，") and lowercases. Trimming is end-only, so internal
// spaces ("shut up") are preserved. ToLower is a no-op for CJK.
func normalizeDismissPhrase(s string) string {
	s = strings.TrimFunc(s, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsPunct(r)
	})
	s = strings.ToLower(s)
	// Strip trailing Chinese modal particles that colloquial dismissals tack on
	// ("退出吧" → "退出", "够了吧" → "够了"). None of the listed phrases end in these, so
	// this only widens matching. 了 is deliberately excluded — it is part of "别说了" /
	// "不要说了" — as is 下 ("停下", "停一下").
	s = strings.TrimRight(s, "吧啊呀嘛呢哦喽噢啦咯呗")
	return s
}

// isDismissPhrase reports whether a raw ASR transcript is a pure "end the
// conversation" control intent. Kill switch: KOE_DISMISS_DETECT=0. Extra phrases:
// KOE_DISMISS_PHRASES (comma-separated), each normalized before comparison.
func isDismissPhrase(transcript string) bool {
	if !koeEnvBool("KOE_DISMISS_DETECT", true) {
		return false
	}
	norm := normalizeDismissPhrase(transcript)
	if norm == "" {
		return false
	}
	if _, ok := baseDismissPhrases[norm]; ok {
		return true
	}
	for _, extra := range strings.Split(os.Getenv("KOE_DISMISS_PHRASES"), ",") {
		if e := normalizeDismissPhrase(extra); e != "" && e == norm {
			return true
		}
	}
	return false
}
