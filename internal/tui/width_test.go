package tui

import "testing"

func TestDisplayWidth(t *testing.T) {
	cases := []struct {
		s    string
		want int
	}{
		{"hello", 5},
		{"你好", 4},  // two CJK ideographs = 4 cells
		{"a你b", 4}, // 1 + 2 + 1
		{"", 0},
	}
	for _, c := range cases {
		if got := displayWidth(c.s); got != c.want {
			t.Errorf("displayWidth(%q) = %d, want %d", c.s, got, c.want)
		}
	}
}

func TestTruncateCellsCJK(t *testing.T) {
	// 10 CJK chars = 20 cells. The old rune-count truncate returned the full
	// 20-cell string for a 10-"char" budget, overflowing the terminal. The
	// width-aware version must keep the result within the cell budget.
	s := "一二三四五六七八九十"
	got := truncateCells(s, 10, "…")
	if w := displayWidth(got); w > 10 {
		t.Fatalf("truncateCells(CJK,10) width = %d > 10 (got %q)", w, got)
	}

	if got := truncateCells("abc", 10, "…"); got != "abc" {
		t.Errorf("truncateCells(abc,10) = %q, want abc", got)
	}

	// ASCII parity: truncate() must still match the historical "..." behavior
	// that header_test.go and callers depend on.
	if got := truncate("hello world", 8); got != "hello..." {
		t.Errorf("truncate(hello world,8) = %q, want hello...", got)
	}
}
