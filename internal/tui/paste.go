package tui

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// pastePlaceholder is the exact composer placeholder for a stashed paste. It
// includes the char count so (a) the user sees how big the paste was and (b) a
// user can't realistically type the placeholder verbatim — guarding expandPastes
// from clobbering literal "[Pasted text #N]" text the user happens to type.
func pastePlaceholder(n int, text string) string {
	return fmt.Sprintf("[Pasted text #%d (%d chars)]", n, utf8.RuneCountInString(text))
}

// pasteTruncateThreshold: a bracketed paste longer than this many runes is
// stashed and replaced in the composer with a [Pasted text #N] placeholder, so
// a pasted log/document doesn't flood the input or the prompt echo. The full
// text is expanded back for the model on submit. (CC truncates >10K, Codex >1K;
// 1000 keeps ordinary paragraph pastes inline.)
const pasteTruncateThreshold = 1000

// expandPastes replaces every [Pasted text #N] placeholder with its stashed
// full text. Pure; called at submit time.
func expandPastes(input string, pastes map[int]string) string {
	if len(pastes) == 0 {
		return input
	}
	out := input
	for n, text := range pastes {
		out = strings.ReplaceAll(out, pastePlaceholder(n, text), text)
	}
	return out
}

// stashPaste stores a large paste under the next number and inserts its
// placeholder into the composer in place of the raw text.
func (m *Model) stashPaste(text string) {
	if m.pastes == nil {
		m.pastes = map[int]string{}
	}
	m.pasteCounter++
	m.pastes[m.pasteCounter] = text
	m.textarea.InsertString(pastePlaceholder(m.pasteCounter, text))
}
