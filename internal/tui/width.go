package tui

import "github.com/mattn/go-runewidth"

// Display-width helpers.
//
// Terminal layout is measured in CELLS, not runes: a CJK ideograph, a
// full-width punctuation mark, and most emoji each occupy 2 cells. The legacy
// truncate helpers counted len([]rune(s)), so "你好世界…" (each char = 2 cells)
// was treated as half its real width — every truncated Chinese tool line / title
// / status field overflowed the terminal and wrapped, garbling the layout. Since
// Kocoro's audience is primarily Chinese, that mis-count was the single most
// visible "显示错位" bug. These helpers count cells.

// displayWidth returns the terminal cell width of PLAIN text (no ANSI escapes).
// For already-styled strings use lipgloss.Width, which strips escapes first.
func displayWidth(s string) int {
	return runewidth.StringWidth(s)
}

// truncateCells truncates s so its display width is at most maxCells, appending
// tail (whose width is included in the budget) when truncation occurs. A
// double-width rune is never split across the boundary. Returns s unchanged when
// it already fits.
func truncateCells(s string, maxCells int, tail string) string {
	if maxCells <= 0 {
		return ""
	}
	if runewidth.StringWidth(s) <= maxCells {
		return s
	}
	return runewidth.Truncate(s, maxCells, tail)
}
