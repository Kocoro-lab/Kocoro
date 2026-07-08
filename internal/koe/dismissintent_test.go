package koe

import "testing"

func TestNormalizeDismissPhrase(t *testing.T) {
	cases := []struct{ in, want string }{
		{"退出。", "退出"},
		{"Exit.", "exit"},
		{"  BYE  ", "bye"},
		{"闭嘴！", "闭嘴"},
		{"やめて。", "やめて"},
		{"", ""},
		{"！！！", ""},
		// trailing modal particles stripped (colloquial dismissals)
		{"退出吧", "退出"},
		{"够了吧。", "够了"},
		{"停止吧！", "停止"},
		// 了 and 下 are NOT particles here — they belong to listed phrases
		{"别说了", "别说了"},
		{"停一下", "停一下"},
	}
	for _, c := range cases {
		if got := normalizeDismissPhrase(c.in); got != c.want {
			t.Errorf("normalizeDismissPhrase(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestIsDismissPhraseHits(t *testing.T) {
	hits := []string{
		// en quit / stop-talking / goodbye
		"stop", "shut up", "quiet", "enough", "goodbye", "bye", "exit", "quit", "that's all",
		// zh
		"停", "停止", "闭嘴", "别说了", "够了", "退出", "结束", "结束对话", "再见", "拜拜", "就这样",
		"取消并且退出", "住口", "打住",
		// zh traditional
		"閉嘴", "夠了", "結束對話", "再見",
		// ja
		"やめて", "黙れ", "ストップ", "もういい", "終わり", "さようなら",
		// real ASR outputs observed live 2026-07-08
		"退出", "闭嘴。", "Стоп.",
		// colloquial particle variants (live gap: "退出吧" missed both paths)
		"退出吧", "够了吧", "停止吧", "闭嘴吧", "再见啦",
		// trailing punctuation
		"再见。", "黙れ！",
	}
	for _, h := range hits {
		if !isDismissPhrase(h) {
			t.Errorf("isDismissPhrase(%q) = false, want true", h)
		}
	}
}

func TestIsDismissPhraseMisses(t *testing.T) {
	misses := []string{
		"",
		"取消",             // bare cancel = the cancel TOOL's job (stop a task), NOT hang up
		"继续",             // keep going
		"别停",             // don't stop
		"don't stop",
		"帮我查一下天气",       // a real request
		"解释一下量子纠缠",     // a real request
		"stop talking about the weather",
		"止まらないで",
	}
	for _, m := range misses {
		if isDismissPhrase(m) {
			t.Errorf("isDismissPhrase(%q) = true, want false", m)
		}
	}
}

func TestIsDismissPhraseKillSwitch(t *testing.T) {
	t.Setenv("KOE_DISMISS_DETECT", "0")
	if isDismissPhrase("退出") {
		t.Error("KOE_DISMISS_DETECT=0 must disable dismiss detection")
	}
}

func TestIsDismissPhraseEnvExtra(t *testing.T) {
	t.Setenv("KOE_DISMISS_PHRASES", "收工, おしまい")
	if !isDismissPhrase("收工") {
		t.Error("KOE_DISMISS_PHRASES entry '收工' must match")
	}
	if !isDismissPhrase("おしまい") {
		t.Error("KOE_DISMISS_PHRASES entry 'おしまい' must match")
	}
	if isDismissPhrase("继续说") {
		t.Error("'继续说' must not match")
	}
}
