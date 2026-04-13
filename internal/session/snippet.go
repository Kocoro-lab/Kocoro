package session

import (
	"strings"
	"unicode/utf8"
)

// snippetWindow is the number of runes shown before and after the first match.
const snippetWindow = 40

// extractTerms pulls content terms out of a search query for snippet
// highlighting. FTS5 operators and punctuation are stripped; quoted phrases
// are preserved as single terms; CJK text is segmented so per-token
// highlighting aligns with how the index matched.
func extractTerms(query string) []string {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil
	}

	var terms []string
	var cur strings.Builder
	inQuote := false
	for _, r := range query {
		if r == '"' {
			if inQuote {
				if cur.Len() > 0 {
					terms = append(terms, cur.String())
					cur.Reset()
				}
				inQuote = false
			} else {
				inQuote = true
			}
			continue
		}
		if inQuote {
			cur.WriteRune(r)
			continue
		}
		// Outside quotes: split on whitespace and FTS operators.
		if r == ' ' || r == '\t' || r == '\n' ||
			r == '(' || r == ')' || r == '*' || r == '^' || r == ':' {
			if cur.Len() > 0 {
				terms = append(terms, cur.String())
				cur.Reset()
			}
			continue
		}
		cur.WriteRune(r)
	}
	if cur.Len() > 0 {
		terms = append(terms, cur.String())
	}

	// Expand each term: if it contains CJK, segment it so we highlight the
	// same units the index matched on. Drop boolean keywords.
	var expanded []string
	for _, t := range terms {
		upper := strings.ToUpper(t)
		if upper == "AND" || upper == "OR" || upper == "NOT" {
			continue
		}
		if containsCJK(t) {
			for _, seg := range strings.Fields(Tokenize(t)) {
				if seg != "" {
					expanded = append(expanded, seg)
				}
			}
		} else {
			expanded = append(expanded, t)
		}
	}
	return expanded
}

func containsCJK(s string) bool {
	for _, r := range s {
		c := classify(r)
		if c == runeChinese || c == runeJapanese {
			return true
		}
	}
	return false
}

// buildSnippet returns a slice of original text around the first term match,
// with matched substrings wrapped in >>>…<<<. Case-insensitive for ASCII,
// exact match for CJK.
func buildSnippet(original string, terms []string) string {
	if original == "" {
		return ""
	}
	if len(terms) == 0 {
		return truncate(original, snippetWindow*2)
	}

	matchStart, matchEnd := findFirstMatch(original, terms)
	if matchStart < 0 {
		return truncate(original, snippetWindow*2)
	}

	// Expand a rune-aware window around [matchStart, matchEnd).
	runeStart := runeOffset(original, matchStart)
	runeEnd := runeOffset(original, matchEnd)
	runes := []rune(original)

	left := runeStart - snippetWindow
	if left < 0 {
		left = 0
	}
	right := runeEnd + snippetWindow
	if right > len(runes) {
		right = len(runes)
	}

	var b strings.Builder
	if left > 0 {
		b.WriteString("...")
	}
	// Emit [left, matchStart) verbatim, highlight [matchStart, matchEnd),
	// then [matchEnd, right). Then walk forward highlighting subsequent
	// matches inside the window.
	b.WriteString(string(runes[left:runeStart]))
	b.WriteString(">>>")
	b.WriteString(string(runes[runeStart:runeEnd]))
	b.WriteString("<<<")

	// Highlight additional matches within the window.
	tailStart := runeEnd
	tailBytes := byteOffset(original, tailStart)
	windowEndBytes := byteOffset(original, right)
	remaining := original[tailBytes:windowEndBytes]
	b.WriteString(highlightAll(remaining, terms))

	if right < len(runes) {
		b.WriteString("...")
	}
	return b.String()
}

// findFirstMatch returns the byte range [start, end) of the earliest match of
// any term in original. Returns (-1, -1) if nothing matches.
func findFirstMatch(original string, terms []string) (int, int) {
	lower := strings.ToLower(original)
	bestStart, bestEnd := -1, -1
	for _, t := range terms {
		if t == "" {
			continue
		}
		var idx int
		if containsCJK(t) {
			idx = strings.Index(original, t)
		} else {
			idx = strings.Index(lower, strings.ToLower(t))
		}
		if idx < 0 {
			continue
		}
		if bestStart < 0 || idx < bestStart {
			bestStart = idx
			bestEnd = idx + len(t)
		}
	}
	return bestStart, bestEnd
}

// highlightAll wraps every occurrence of any term in s with >>>…<<<.
// Non-overlapping; earliest/longest term wins per position.
func highlightAll(s string, terms []string) string {
	if s == "" || len(terms) == 0 {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	lower := strings.ToLower(s)
	i := 0
	for i < len(s) {
		bestStart := -1
		bestLen := 0
		for _, t := range terms {
			if t == "" {
				continue
			}
			var idx int
			if containsCJK(t) {
				idx = strings.Index(s[i:], t)
			} else {
				idx = strings.Index(lower[i:], strings.ToLower(t))
			}
			if idx < 0 {
				continue
			}
			abs := i + idx
			if bestStart < 0 || abs < bestStart || (abs == bestStart && len(t) > bestLen) {
				bestStart = abs
				bestLen = len(t)
			}
		}
		if bestStart < 0 {
			b.WriteString(s[i:])
			break
		}
		b.WriteString(s[i:bestStart])
		b.WriteString(">>>")
		b.WriteString(s[bestStart : bestStart+bestLen])
		b.WriteString("<<<")
		i = bestStart + bestLen
	}
	return b.String()
}

func truncate(s string, maxRunes int) string {
	if utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	runes := []rune(s)
	return string(runes[:maxRunes]) + "..."
}

// runeOffset returns the rune index for the byte offset b in s.
func runeOffset(s string, b int) int {
	if b <= 0 {
		return 0
	}
	if b >= len(s) {
		return utf8.RuneCountInString(s)
	}
	return utf8.RuneCountInString(s[:b])
}

// byteOffset returns the byte position for rune index r in s.
func byteOffset(s string, r int) int {
	if r <= 0 {
		return 0
	}
	count := 0
	for i := range s {
		if count == r {
			return i
		}
		count++
	}
	return len(s)
}
