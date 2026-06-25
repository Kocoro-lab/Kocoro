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

// truncateCellsSafe truncates s for a line that lives in Bubbletea's animated
// inline live region, where any physical wrap is catastrophic: a single wrapped
// row desyncs the renderer's linesRendered count, and every subsequent CursorUp
// then lands the cursor on the wrong row/column — surfacing as the "ghost
// spinner stranded in scrollback" + "CJK preamble offset right" bugs.
//
// runewidth.Truncate measures with runewidth's own width tables, which can
// DISAGREE with what the terminal actually paints (East-Asian ambiguous / some
// full-width punctuation / emoji depend on locale + terminal config). When
// runewidth undercounts, a "truncated to width" line still overflows and wraps.
//
// So we measure PESSIMISTICALLY: every non-ASCII rune is budgeted as 2 cells.
// That is an upper bound on real terminal width (no printable rune renders wider
// than 2 cells in a monospace terminal), so the result can NEVER wrap, even when
// runewidth would have allowed one more character. Worst case we truncate a few
// columns early — invisible in a dimmed "being typed" preview, and far cheaper
// than a corrupted scrollback. ASCII-art bars/borders must NOT use this (their
// box-drawing glyphs are reliably 1 cell); reserve it for free-form text.
func truncateCellsSafe(s string, maxCells int) string {
	if maxCells <= 0 {
		return ""
	}
	used := 0
	for i, r := range s {
		w := 1
		if r >= 0x80 {
			w = 2
		}
		if used+w > maxCells {
			return s[:i]
		}
		used += w
	}
	return s
}
