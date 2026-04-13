package session

import (
	"strings"
	"sync"
	"unicode"

	"github.com/go-ego/gse"
	"github.com/ikawaha/kagome-dict/ipa"
	kagome "github.com/ikawaha/kagome/v2/tokenizer"
)

// TokenizerVersion is bumped whenever the tokenization pipeline or the
// messages-table schema changes in a way that invalidates the on-disk index.
// A mismatch between this value and the version stored in the DB triggers a
// full index rebuild.
//
// Version history:
//
//	2 — introduced CJK tokenization (gse + kagome) and the `original` column.
//	3 — forces re-migration for DBs that were incorrectly stamped as v2 by a
//	    build where the migration branch silently skipped when stored==0,
//	    leaving the pre-CJK messages schema in place.
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
		s, err := gse.NewEmbed("zh")
		if err != nil {
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
		case runeJapanese:
			j := i + 1
			for j < len(runes) && classify(runes[j]) == runeJapanese {
				j++
			}
			appendTokens(&b, tokenizeJapanese(string(runes[i:j])))
			i = j
		case runeChinese:
			j := i + 1
			for j < len(runes) && classify(runes[j]) == runeChinese {
				j++
			}
			appendTokens(&b, tokenizeChinese(string(runes[i:j])))
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
	return b.String()
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
	// we route pure-Han runs through gse. Mixed runs with kana already got
	// pulled into the Japanese segment above because kana neighbors them.
	if r >= 0x4E00 && r <= 0x9FFF {
		return runeChinese
	}
	// CJK Extension A
	if r >= 0x3400 && r <= 0x4DBF {
		return runeChinese
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
	out := make([]string, 0, len([]rune(s)))
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
