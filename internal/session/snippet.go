package session

import (
	"sort"
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
// exact match for CJK. Quoted ASCII phrases highlight literally; single ASCII
// terms also highlight stem-equivalent words (e.g. a query "programs" will
// highlight "program" that FTS5 matched via porter).
func buildSnippet(original string, terms []string) string {
	if original == "" {
		return ""
	}

	matches := findTermMatches(original, terms)
	if len(matches) == 0 {
		return truncate(original, snippetWindow*2)
	}

	first := matches[0]
	runeStart := runeOffset(original, first[0])
	runeEnd := runeOffset(original, first[1])
	runes := []rune(original)

	left := runeStart - snippetWindow
	if left < 0 {
		left = 0
	}
	right := runeEnd + snippetWindow
	if right > len(runes) {
		right = len(runes)
	}
	leftByte := byteOffset(original, left)
	rightByte := byteOffset(original, right)

	var b strings.Builder
	if left > 0 {
		b.WriteString("...")
	}
	pos := leftByte
	for _, m := range matches {
		if m[1] <= leftByte {
			continue
		}
		if m[0] >= rightByte {
			break
		}
		s, e := m[0], m[1]
		if s < pos {
			s = pos
		}
		if e > rightByte {
			e = rightByte
		}
		if s < pos {
			continue
		}
		b.WriteString(original[pos:s])
		b.WriteString(">>>")
		b.WriteString(original[s:e])
		b.WriteString("<<<")
		pos = e
	}
	if pos < rightByte {
		b.WriteString(original[pos:rightByte])
	}
	if right < len(runes) {
		b.WriteString("...")
	}
	return b.String()
}

// findTermMatches returns sorted, non-overlapping byte ranges [start, end)
// where any term matches in s. CJK terms match literally; quoted ASCII
// phrases match literally; single ASCII terms match word tokens whose light
// porter-style stem matches the term's stem.
func findTermMatches(s string, terms []string) [][2]int {
	if s == "" || len(terms) == 0 {
		return nil
	}

	var ranges [][2]int

	// CJK terms: literal, all occurrences.
	for _, t := range terms {
		if t == "" || !containsCJK(t) {
			continue
		}
		start := 0
		for start <= len(s)-len(t) {
			idx := strings.Index(s[start:], t)
			if idx < 0 {
				break
			}
			abs := start + idx
			ranges = append(ranges, [2]int{abs, abs + len(t)})
			start = abs + len(t)
		}
	}

	// Quoted ASCII phrases: literal, case-insensitive.
	var asciiPhrases []string

	// Single ASCII terms: collect stems, then scan s by ASCII word boundaries.
	asciiStems := make(map[string]struct{})
	for _, t := range terms {
		if t == "" || containsCJK(t) {
			continue
		}
		if strings.ContainsAny(t, " \t\n\r") {
			asciiPhrases = append(asciiPhrases, strings.ToLower(t))
			continue
		}
		st := asciiStem(t)
		if st == "" {
			continue
		}
		asciiStems[st] = struct{}{}
	}
	if len(asciiPhrases) > 0 {
		lower := strings.ToLower(s)
		for _, phrase := range asciiPhrases {
			start := 0
			for start <= len(lower)-len(phrase) {
				idx := strings.Index(lower[start:], phrase)
				if idx < 0 {
					break
				}
				abs := start + idx
				ranges = append(ranges, [2]int{abs, abs + len(phrase)})
				start = abs + len(phrase)
			}
		}
	}
	if len(asciiStems) > 0 {
		lower := strings.ToLower(s)
		i := 0
		for i < len(lower) {
			if !isAsciiWord(lower[i]) {
				i++
				continue
			}
			j := i + 1
			for j < len(lower) && isAsciiWord(lower[j]) {
				j++
			}
			word := lower[i:j]
			wst := asciiStem(word)
			for ts := range asciiStems {
				if wst == ts ||
					(len(wst) > 0 && strings.HasPrefix(wst, ts)) ||
					(len(ts) > 0 && strings.HasPrefix(ts, wst)) {
					ranges = append(ranges, [2]int{i, j})
					break
				}
			}
			i = j
		}
	}

	if len(ranges) == 0 {
		return nil
	}
	sort.Slice(ranges, func(i, j int) bool {
		if ranges[i][0] != ranges[j][0] {
			return ranges[i][0] < ranges[j][0]
		}
		return ranges[i][1] > ranges[j][1]
	})
	merged := ranges[:0:0]
	for _, r := range ranges {
		if len(merged) > 0 && r[0] < merged[len(merged)-1][1] {
			// Overlapping — extend if this range is longer.
			if r[1] > merged[len(merged)-1][1] {
				merged[len(merged)-1][1] = r[1]
			}
			continue
		}
		merged = append(merged, r)
	}
	return merged
}

func isAsciiWord(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9')
}

// asciiStem applies a small subset of porter-style suffix stripping so that
// query word and indexed word collapse to the same root. This is intentionally
// approximate — it only needs to cover common English inflections so that
// stemmed FTS5 matches still get highlighted in the snippet.
func asciiStem(s string) string {
	s = strings.ToLower(s)
	if len(s) <= 3 {
		return s
	}
	switch {
	case strings.HasSuffix(s, "sses"):
		return s[:len(s)-2] // "classes" -> "class"
	case strings.HasSuffix(s, "ies") && len(s) > 4:
		return s[:len(s)-3] + "i" // "flies" -> "fli"
	case strings.HasSuffix(s, "ing") && len(s) > 5:
		return s[:len(s)-3]
	case strings.HasSuffix(s, "ed") && len(s) > 4:
		return s[:len(s)-2]
	case strings.HasSuffix(s, "es") && len(s) > 4:
		return s[:len(s)-2]
	case strings.HasSuffix(s, "s") && !strings.HasSuffix(s, "ss") && len(s) > 3:
		return s[:len(s)-1]
	}
	return s
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
