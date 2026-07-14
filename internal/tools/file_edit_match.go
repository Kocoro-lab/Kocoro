package tools

import (
	"fmt"
	"strings"
)

// matchSpan is a half-open byte range in the original file content. Fuzzy
// matchers return positions instead of only the matched bytes: the same bytes
// may appear earlier inside a different line, and a global strings.Replace
// would then edit the wrong location.
type matchSpan struct {
	start int
	end   int
}

// normalizePunctRune folds common typographic punctuation to its ASCII
// equivalent, so an old_string typed in plain ASCII can still match file bytes
// containing smart quotes, en/em dashes, or exotic spaces -- the most common
// cause of a spurious "old_string not found". The mapping is strictly 1 rune to
// 1 rune (never inserts or deletes runes), which keeps rune indices aligned
// between the original and normalized forms; a normalized-space match therefore
// maps straight back to the exact original file bytes. Code points are written
// as hex literals so no invisible characters can hide in this table.
func normalizePunctRune(r rune) rune {
	switch r {
	case 0x2018, 0x2019, 0x201A, 0x201B: // fancy single quotes -> '
		return '\''
	case 0x201C, 0x201D, 0x201E, 0x201F: // fancy double quotes -> "
		return '"'
	case 0x2010, 0x2011, 0x2012, 0x2013, 0x2014, 0x2015, 0x2212: // hyphens / dashes / minus -> -
		return '-'
	case 0x00A0, 0x2002, 0x2003, 0x2004, 0x2005, 0x2006,
		0x2007, 0x2008, 0x2009, 0x200A, 0x202F, 0x205F, 0x3000: // exotic spaces -> space
		return ' '
	default:
		return r
	}
}

func normalizePunct(runes []rune) []rune {
	out := make([]rune, len(runes))
	for i, r := range runes {
		out[i] = normalizePunctRune(r)
	}
	return out
}

// normalizePunctWithByteOffsets normalizes s while retaining the byte offset of
// every rune in the original string. normalizePunctRune is one-rune-to-one-rune,
// so a normalized rune match maps directly back to an exact original byte span.
func normalizePunctWithByteOffsets(s string) (normalized []rune, byteOffsets []int) {
	normalized = make([]rune, 0, len(s))
	byteOffsets = make([]int, 0, len(s)+1)
	for i, r := range s {
		normalized = append(normalized, normalizePunctRune(r))
		byteOffsets = append(byteOffsets, i)
	}
	byteOffsets = append(byteOffsets, len(s))
	return normalized, byteOffsets
}

// nonOverlappingRuneMatches returns left-to-right, non-overlapping match starts
// using KMP. This keeps punctuation fallback O(len(content)+len(old)) instead of
// comparing the full pattern at every content rune.
func nonOverlappingRuneMatches(haystack, needle []rune) []int {
	if len(needle) == 0 || len(needle) > len(haystack) {
		return nil
	}
	lps := make([]int, len(needle))
	for i, length := 1, 0; i < len(needle); {
		if needle[i] == needle[length] {
			length++
			lps[i] = length
			i++
		} else if length > 0 {
			length = lps[length-1]
		} else {
			i++
		}
	}

	var starts []int
	for i, matched := 0, 0; i < len(haystack); i++ {
		for matched > 0 && haystack[i] != needle[matched] {
			matched = lps[matched-1]
		}
		if haystack[i] == needle[matched] {
			matched++
		}
		if matched == len(needle) {
			starts = append(starts, i-len(needle)+1)
			matched = 0 // strings.Count / ReplaceAll use non-overlapping matches.
		}
	}
	return starts
}

// fuzzyFindPunct locates every non-overlapping old occurrence after normalizing
// typographic punctuation on both sides. Returned spans always address the real
// original file bytes, even when different matches use different quote/dash
// code points.
func fuzzyFindPunct(content, old string) []matchSpan {
	normalizedContent, byteOffsets := normalizePunctWithByteOffsets(content)
	normalizedOld := normalizePunct([]rune(old))
	starts := nonOverlappingRuneMatches(normalizedContent, normalizedOld)
	spans := make([]matchSpan, 0, len(starts))
	for _, start := range starts {
		spans = append(spans, matchSpan{
			start: byteOffsets[start],
			end:   byteOffsets[start+len(normalizedOld)],
		})
	}
	return spans
}

// splitLinesKeepNL splits s into lines, each retaining its trailing "\n"
// (the final line has none unless s ends with "\n"). Concatenating the result
// reproduces s exactly, so a matched line range maps back to real bytes.
func splitLinesKeepNL(s string) []string {
	var out []string
	for len(s) > 0 {
		i := strings.IndexByte(s, '\n')
		if i < 0 {
			out = append(out, s)
			break
		}
		out = append(out, s[:i+1])
		s = s[i+1:]
	}
	return out
}

// splitLinesKeepNLLimit avoids building an unbounded slice only to discover
// that diagnostics should be skipped for a very high-line-count file.
func splitLinesKeepNLLimit(s string, maxLines int) (lines []string, overflow bool) {
	for len(s) > 0 {
		if len(lines) == maxLines {
			return nil, true
		}
		i := strings.IndexByte(s, '\n')
		if i < 0 {
			lines = append(lines, s)
			break
		}
		lines = append(lines, s[:i+1])
		s = s[i+1:]
	}
	return lines, false
}

// rstripNormLine is the per-line comparison key for the rstrip pass: drop the
// line terminator and trailing spaces/tabs, then normalize punctuation. This
// absorbs trailing-whitespace and CRLF (\r) differences.
func rstripNormLine(line string) string {
	line = strings.TrimRight(line, "\r\n")
	line = strings.TrimRight(line, " \t")
	return string(normalizePunct([]rune(line)))
}

// trimNormLine is the per-line key for the trim pass: additionally drop leading
// whitespace, so indentation differences (tabs vs spaces, depth) are ignored.
func trimNormLine(line string) string {
	line = strings.TrimRight(line, "\r\n")
	line = strings.Trim(line, " \t")
	return string(normalizePunct([]rune(line)))
}

// nonOverlappingLineMatches returns non-overlapping matches of a sequence of
// normalized lines using KMP.
func nonOverlappingLineMatches(haystack, needle []string) []int {
	if len(needle) == 0 || len(needle) > len(haystack) {
		return nil
	}
	lps := make([]int, len(needle))
	for i, length := 1, 0; i < len(needle); {
		if needle[i] == needle[length] {
			length++
			lps[i] = length
			i++
		} else if length > 0 {
			length = lps[length-1]
		} else {
			i++
		}
	}

	var starts []int
	for i, matched := 0, 0; i < len(haystack); i++ {
		for matched > 0 && haystack[i] != needle[matched] {
			matched = lps[matched-1]
		}
		if haystack[i] == needle[matched] {
			matched++
		}
		if matched == len(needle) {
			starts = append(starts, i-len(needle)+1)
			matched = 0
		}
	}
	return starts
}

// fuzzyFindLines matches old against content one line at a time, in two
// decreasing-strictness passes: first rstrip
// (ignore per-line trailing whitespace and CR), then trim (also ignore leading
// indentation). It returns exact byte spans for the original line blocks. When
// old carries no trailing newline, each span excludes the block's final line
// terminator so that terminator remains around new_string.
func fuzzyFindLines(content, old string) []matchSpan {
	clines := splitLinesKeepNL(content)
	olines := splitLinesKeepNL(old)
	m := len(olines)
	if m == 0 || m > len(clines) {
		return nil
	}
	lineOffsets := make([]int, len(clines)+1)
	for i, line := range clines {
		lineOffsets[i+1] = lineOffsets[i] + len(line)
	}
	for _, norm := range []func(string) string{rstripNormLine, trimNormLine} {
		normalizedContent := make([]string, len(clines))
		for i := range clines {
			normalizedContent[i] = norm(clines[i])
		}
		normalizedOld := make([]string, m)
		for i := range olines {
			normalizedOld[i] = norm(olines[i])
		}
		starts := nonOverlappingLineMatches(normalizedContent, normalizedOld)
		if len(starts) > 0 {
			spans := make([]matchSpan, 0, len(starts))
			for _, first := range starts {
				lastLine := clines[first+m-1]
				if strings.HasSuffix(old, "\n") && !strings.HasSuffix(lastLine, "\n") {
					// old explicitly includes a line terminator. CRLF and LF are
					// equivalent in this fallback, but no terminator is not.
					continue
				}
				end := lineOffsets[first+m]
				if !strings.HasSuffix(old, "\n") {
					withoutNL := strings.TrimSuffix(lastLine, "\n")
					withoutNL = strings.TrimSuffix(withoutNL, "\r")
					end -= len(lastLine) - len(withoutNL)
				}
				spans = append(spans, matchSpan{start: lineOffsets[first], end: end})
			}
			if len(spans) > 0 {
				return spans
			}
		}
	}
	return nil
}

// replaceMatchSpans applies sorted, non-overlapping byte spans in one pass.
func replaceMatchSpans(content string, spans []matchSpan, replacement string) string {
	var out strings.Builder
	last := 0
	for _, span := range spans {
		out.WriteString(content[last:span.start])
		out.WriteString(replacement)
		last = span.end
	}
	out.WriteString(content[last:])
	return out.String()
}

// firstLine returns s up to (not including) the first newline.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// longestCommonSubstringLen returns the length of the longest contiguous runes
// shared by a and b. The caller supplies reusable scratch buffers so scanning
// multiple candidate lines does not allocate a DP row for every old-string rune.
func longestCommonSubstringLen(a, b []rune, prev, cur []int) int {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	prev = prev[:len(b)+1]
	cur = cur[:len(b)+1]
	clear(prev)
	clear(cur)
	best := 0
	for i := 1; i <= len(a); i++ {
		clear(cur)
		for j := 1; j <= len(b); j++ {
			if a[i-1] == b[j-1] {
				cur[j] = prev[j-1] + 1
				if cur[j] > best {
					best = cur[j]
				}
			}
		}
		prev, cur = cur, prev
	}
	return best
}

// diagnoseNoMatch builds a short hint pointing at the file line most similar to
// old_string's first line, echoing its ACTUAL bytes so the model can spot the
// real difference (a typo, a changed value) without a re-read. Returns "" when nothing is similar
// enough (better silent than misleading): the shared run must be >= 4 runes and
// cover at least half of old_string's first line. Diagnostics are skipped on
// large or expensive inputs.
//
// Workload: source/config lines are normally well under 512 runes, and a useful
// typo hint needs far less than two million rune comparisons. Binding symptom:
// minified files, huge whitespace-padded lines, or a broad scan skip the optional
// closest-line hint while preserving the primary validation error. Override:
// raise these constants together if longer diagnostics become operationally
// necessary; they are intentionally hard limits because this is an error path.
const (
	diagMaxLines       = 2000
	diagMaxLineRunes   = 512
	diagMaxComparisons = 2_000_000
	diagMaxOutputRunes = 512
)

type diagnosticCandidate struct {
	lineIndex int
	runes     []rune
}

func normalizePunctBounded(s string, maxRunes int) ([]rune, bool) {
	normalized := make([]rune, 0, min(len(s), maxRunes))
	for _, r := range s {
		if len(normalized) == maxRunes {
			return nil, true
		}
		normalized = append(normalized, normalizePunctRune(r))
	}
	return normalized, false
}

func truncateDiagnosticRunes(s string, maxRunes int) (string, bool) {
	count := 0
	for byteIndex := range s {
		if count == maxRunes {
			return s[:byteIndex], true
		}
		count++
	}
	return s, false
}

func diagnoseNoMatch(content, old string) string {
	ofirst := strings.TrimSpace(firstLine(old))
	if ofirst == "" {
		return ""
	}
	onRunes, overflow := normalizePunctBounded(ofirst, diagMaxLineRunes)
	if overflow {
		return ""
	}
	clines, overflow := splitLinesKeepNLLimit(content, diagMaxLines)
	if overflow {
		return ""
	}
	candidates := make([]diagnosticCandidate, 0, len(clines))
	comparisons := 0
	for i, cl := range clines {
		cnRunes, overlong := normalizePunctBounded(
			strings.TrimSpace(strings.TrimRight(cl, "\r\n")),
			diagMaxLineRunes,
		)
		if overlong {
			continue
		}
		cost := len(onRunes) * len(cnRunes)
		if cost > diagMaxComparisons-comparisons {
			return ""
		}
		comparisons += cost
		candidates = append(candidates, diagnosticCandidate{lineIndex: i, runes: cnRunes})
	}

	prev := make([]int, diagMaxLineRunes+1)
	cur := make([]int, diagMaxLineRunes+1)
	bestIdx, bestScore := -1, 0
	for _, candidate := range candidates {
		if score := longestCommonSubstringLen(onRunes, candidate.runes, prev, cur); score > bestScore {
			bestScore, bestIdx = score, candidate.lineIndex
		}
	}
	if bestIdx < 0 || bestScore < 4 || bestScore*2 < len(onRunes) {
		return ""
	}
	actual := strings.TrimRight(clines[bestIdx], "\r\n")
	if truncated, ok := truncateDiagnosticRunes(actual, diagMaxOutputRunes); ok {
		actual = truncated + "… [truncated]"
	}
	return fmt.Sprintf(" Closest match is line %d: %q -- compare it against your old_string.", bestIdx+1, actual)
}
