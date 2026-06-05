package session

import "testing"

func TestTitle(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"hello world", "hello world"},
		{"", "New session"},
		{"   ", "New session"},
		{"line1\nline2", "line1"},
		{"a]very long input that exceeds fifty characters and should be truncated at word boundary", "a]very long input that exceeds fifty characters..."},
		{"short\r\nwith crlf", "short"},
		// CJK: 3 bytes per rune, must not truncate mid-rune
		{"这是一个很长的中文标题，需要被截断到五十个字符以内，否则会显示不全的问题需要修复", "这是一个很长的中文标题，需要被截断到五十个字符以内，否则会显示不全的问题需要修复"},
		// Emoji: 4 bytes per rune, must not corrupt (52 runes → truncate to 50)
		{"你截图看看我现在这个窗口，是不是🤔🤔都有这种表情符号，测试一下是否正常显示出来了呢朋友们大家好好好好好", "你截图看看我现在这个窗口，是不是🤔🤔都有这种表情符号，测试一下是否正常显示出来了呢朋友们大家好好好好..."},
	}
	for _, tt := range tests {
		got := Title(tt.input)
		if got != tt.want {
			t.Errorf("Title(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}
