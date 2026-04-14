package session

import (
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/go-ego/gse"
	"github.com/ikawaha/kagome-dict/ipa"
	kagome "github.com/ikawaha/kagome/v2/tokenizer"

	"github.com/Kocoro-lab/ShanClaw/internal/session/dictdata"
)

// TokenizerVersion is bumped whenever the tokenization pipeline or the
// messages-table schema changes in a way that invalidates the on-disk index.
// A mismatch between this value and the version stored in the DB triggers a
// full index rebuild.
//
// Version history:
//
//	2 — intended to introduce CJK tokenization (gse + kagome) and the
//	    `original` column. Never released to users; an early build
//	    silently skipped the migration when stored==0, leaving pre-CJK
//	    schema in place while stamping user_version=2.
//	3 — current. Forces re-migration for any DB stamped as v2 by that
//	    earlier build.
const TokenizerVersion = 3

var (
	gseSegOnce sync.Once
	gseSeg     *gse.Segmenter
	gseErr     error

	kagomeOnce sync.Once
	kagomeTnz  *kagome.Tokenizer
	kagomeErr  error
)

func getGse() *gse.Segmenter {
	gseSegOnce.Do(func() {
		var s gse.Segmenter
		// Load Chinese dict from our own compressed embed (dictdata package)
		// instead of gse's built-in NewEmbed, which unconditionally embeds a
		// 23 MB Japanese dictionary we don't use. Build with -tags ne to
		// exclude gse's embed; our dictdata provides only the zh data (~3 MB
		// compressed, ~8 MB decompressed).
		if err := s.LoadDictStr(dictdata.ZhDict()); err != nil {
			gseErr = err
			return
		}
		gseSeg = &s
	})
	if gseErr != nil {
		return nil
	}
	return gseSeg
}

func getKagome() *kagome.Tokenizer {
	kagomeOnce.Do(func() {
		t, err := kagome.New(ipa.Dict(), kagome.OmitBosEos())
		if err != nil {
			kagomeErr = err
			return
		}
		kagomeTnz = t
	})
	if kagomeErr != nil {
		return nil
	}
	return kagomeTnz
}

// Tokenize converts free text into a space-separated token stream suitable for
// FTS5 unicode61 indexing. CJK segments are split with gse/kagome; Latin/digit
// segments are passed through unchanged (porter+unicode61 handles them). The
// result is only stored in the FTS column — original text is preserved in a
// separate column for snippet rendering.
func Tokenize(text string) string {
	if text == "" {
		return ""
	}

	var b strings.Builder
	b.Grow(len(text) + len(text)/4)

	runes := []rune(text)
	i := 0
	for i < len(runes) {
		r := runes[i]
		switch classify(r) {
		case runeJapanese, runeChinese:
			// Greedy CJK run: absorb both Han ideographs and kana so
			// kanji compounds inside Japanese sentences (e.g. 機械学習 in
			// 機械学習の原理) stay with kagome instead of being handed to
			// the Chinese segmenter. The presence of any kana in the run
			// flips routing to Japanese; pure-Han runs go to Chinese.
			j := i + 1
			hasKana := classify(r) == runeJapanese
			for j < len(runes) {
				c := classify(runes[j])
				if c != runeChinese && c != runeJapanese {
					break
				}
				if c == runeJapanese {
					hasKana = true
				}
				j++
			}
			seg := string(runes[i:j])
			if hasKana {
				appendTokens(&b, tokenizeJapanese(seg))
			} else {
				appendTokens(&b, tokenizeChinese(seg))
			}
			i = j
		case runeSpace:
			b.WriteRune(' ')
			i++
		default:
			// Latin, digits, punctuation — emit verbatim, unicode61 handles boundaries.
			j := i + 1
			for j < len(runes) {
				c := classify(runes[j])
				if c == runeChinese || c == runeJapanese || c == runeSpace {
					break
				}
				j++
			}
			b.WriteString(string(runes[i:j]))
			i = j
		}
	}
	return strings.TrimRight(b.String(), " ")
}

type runeClass int

const (
	runeOther runeClass = iota
	runeChinese
	runeJapanese
	runeSpace
)

func classify(r rune) runeClass {
	if unicode.IsSpace(r) {
		return runeSpace
	}
	// Hiragana or Katakana → definitely Japanese.
	if (r >= 0x3040 && r <= 0x309F) || (r >= 0x30A0 && r <= 0x30FF) {
		return runeJapanese
	}
	// CJK Unified Ideographs (Han). Shared between Chinese and Japanese;
	// Tokenize() greedily absorbs adjacent kana into the same segment and
	// routes the whole thing through kagome when any kana is present.
	if r >= 0x4E00 && r <= 0x9FFF {
		return runeChinese
	}
	// CJK Extension A
	if r >= 0x3400 && r <= 0x4DBF {
		return runeChinese
	}
	// CJK Compatibility Ideographs — common in real text (e.g. some
	// proper nouns), so route them through Han segmentation.
	if r >= 0xF900 && r <= 0xFAFF {
		return runeChinese
	}
	// Halfwidth Katakana (legacy Japanese encodings).
	if r >= 0xFF65 && r <= 0xFF9F {
		return runeJapanese
	}
	return runeOther
}

func tokenizeChinese(s string) []string {
	seg := getGse()
	if seg == nil {
		// Fallback: per-character tokens so search still works, just less precise.
		return runeTokens(s)
	}
	return seg.Cut(s, true)
}

func tokenizeJapanese(s string) []string {
	t := getKagome()
	if t == nil {
		return runeTokens(s)
	}
	toks := t.Tokenize(s)
	out := make([]string, 0, len(toks))
	for _, tok := range toks {
		if tok.Surface == "" {
			continue
		}
		out = append(out, tok.Surface)
	}
	return out
}

func runeTokens(s string) []string {
	out := make([]string, 0, utf8.RuneCountInString(s))
	for _, r := range s {
		out = append(out, string(r))
	}
	return out
}

func appendTokens(b *strings.Builder, tokens []string) {
	for _, t := range tokens {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		if b.Len() > 0 {
			last := b.String()[b.Len()-1]
			if last != ' ' {
				b.WriteByte(' ')
			}
		}
		b.WriteString(t)
		b.WriteByte(' ')
	}
}
