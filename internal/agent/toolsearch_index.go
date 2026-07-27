package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"math"
	"sort"
	"strings"
	"unicode"
)

const (
	toolSearchDefaultLimit = 8
	toolSearchBM25K1       = 1.2
	toolSearchBM25B        = 0.75
)

// ToolSearchNamespaceProvider exposes structural source metadata such as an
// MCP server name. It is intentionally not a curated search hint: searchable
// text remains derived from runtime and schema metadata.
type ToolSearchNamespaceProvider interface {
	ToolSearchNamespace() string
}

type toolSearchCorpusDocument struct {
	name  string
	text  string
	order int
}

type toolSearchCorpus struct {
	fingerprint string
	documents   []toolSearchCorpusDocument
}

type toolSearchDocument struct {
	name      string
	order     int
	length    int
	frequency map[string]int
}

type toolSearchIndex struct {
	documents         []toolSearchDocument
	documentFrequency map[string]int
	averageLength     float64
	fingerprint       string
}

type toolSearchHit struct {
	name  string
	order int
	score float64
}

func collectToolSearchCorpus(reg *ToolRegistry, deferred map[string]bool) toolSearchCorpus {
	if reg == nil || len(deferred) == 0 {
		return toolSearchCorpus{}
	}

	h := sha256.New()
	documents := make([]toolSearchCorpusDocument, 0, len(deferred))
	for order, name := range reg.SortedNames() {
		if !deferred[name] {
			continue
		}
		tool, ok := reg.Get(name)
		if !ok {
			continue
		}
		text := buildToolSearchDocument(tool)
		documents = append(documents, toolSearchCorpusDocument{
			name:  name,
			text:  text,
			order: order,
		})
		_, _ = h.Write([]byte(name))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(text))
		_, _ = h.Write([]byte{'\n'})
	}
	if len(documents) == 0 {
		return toolSearchCorpus{}
	}
	return toolSearchCorpus{
		fingerprint: hex.EncodeToString(h.Sum(nil)),
		documents:   documents,
	}
}

func buildToolSearchIndex(corpus toolSearchCorpus) *toolSearchIndex {
	index := &toolSearchIndex{
		documentFrequency: make(map[string]int),
		fingerprint:       corpus.fingerprint,
	}
	if len(corpus.documents) == 0 {
		return index
	}

	index.documents = make([]toolSearchDocument, 0, len(corpus.documents))
	totalLength := 0
	for _, raw := range corpus.documents {
		tokens := tokenizeToolSearch(raw.text)
		frequency := make(map[string]int, len(tokens))
		for _, token := range tokens {
			frequency[token]++
		}
		for token := range frequency {
			index.documentFrequency[token]++
		}
		index.documents = append(index.documents, toolSearchDocument{
			name:      raw.name,
			order:     raw.order,
			length:    len(tokens),
			frequency: frequency,
		})
		totalLength += len(tokens)
	}
	index.averageLength = float64(totalLength) / float64(len(index.documents))
	return index
}

func newToolSearchIndex(reg *ToolRegistry, deferred map[string]bool) *toolSearchIndex {
	return buildToolSearchIndex(collectToolSearchCorpus(reg, deferred))
}

func (index *toolSearchIndex) Search(query string, limit int) []string {
	if index == nil || limit <= 0 || len(index.documents) == 0 {
		return nil
	}
	queryTokens := uniqueToolSearchTokens(tokenizeToolSearch(query))
	if len(queryTokens) == 0 {
		return nil
	}

	hits := make([]toolSearchHit, 0, len(index.documents))
	documentCount := float64(len(index.documents))
	averageLength := index.averageLength
	if averageLength <= 0 {
		averageLength = 1
	}
	for _, document := range index.documents {
		score := 0.0
		for _, token := range queryTokens {
			tf := document.frequency[token]
			if tf == 0 {
				continue
			}
			df := index.documentFrequency[token]
			idf := math.Log(1 + (documentCount-float64(df)+0.5)/(float64(df)+0.5))
			numerator := float64(tf) * (toolSearchBM25K1 + 1)
			denominator := float64(tf) + toolSearchBM25K1*
				(1-toolSearchBM25B+toolSearchBM25B*float64(document.length)/averageLength)
			score += idf * numerator / denominator
		}
		if score > 0 {
			hits = append(hits, toolSearchHit{
				name:  document.name,
				order: document.order,
				score: score,
			})
		}
	}

	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].score != hits[j].score {
			return hits[i].score > hits[j].score
		}
		if hits[i].order != hits[j].order {
			return hits[i].order < hits[j].order
		}
		return hits[i].name < hits[j].name
	})
	if len(hits) > limit {
		hits = hits[:limit]
	}
	names := make([]string, 0, len(hits))
	for _, hit := range hits {
		names = append(names, hit.name)
	}
	return names
}

// buildToolSearchDocument derives searchable text from the canonical tool
// metadata. Name terms appear twice (canonical and separator-normalized),
// matching Codex's default weighting without a SearchHint maintenance field.
func buildToolSearchDocument(tool Tool) string {
	if tool == nil {
		return ""
	}
	info := tool.Info()
	parts := make([]string, 0, 8)
	appendSearchPart := func(part string) {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			parts = append(parts, trimmed)
		}
	}
	appendSearchPart(info.Name)
	appendSearchPart(normalizeToolSearchSeparators(info.Name))
	appendSearchPart(info.Description)
	appendSchemaSearchText(info.Parameters, &parts)

	if provider, ok := tool.(ToolSearchNamespaceProvider); ok {
		namespace := provider.ToolSearchNamespace()
		appendSearchPart(namespace)
		appendSearchPart(normalizeToolSearchSeparators(namespace))
	}
	if sourcer, ok := tool.(ToolSourcer); ok {
		appendSearchPart(string(sourcer.ToolSource()))
	}
	return strings.Join(parts, " ")
}

func normalizeToolSearchSeparators(value string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '_', '-', '.', '/', ':':
			return ' '
		default:
			return r
		}
	}, value)
}

func appendSchemaSearchText(value any, parts *[]string) {
	schema, ok := value.(map[string]any)
	if !ok {
		return
	}
	if description, ok := schema["description"].(string); ok {
		if description = strings.TrimSpace(description); description != "" {
			*parts = append(*parts, description)
		}
	}
	if properties, ok := schema["properties"].(map[string]any); ok {
		names := make([]string, 0, len(properties))
		for name := range properties {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			*parts = append(*parts, name, normalizeToolSearchSeparators(name))
			appendSchemaSearchText(properties[name], parts)
		}
	}
	for _, key := range []string{"items", "additionalProperties"} {
		appendSchemaSearchText(schema[key], parts)
	}
	for _, key := range []string{"anyOf", "allOf", "oneOf"} {
		variants, ok := schema[key].([]any)
		if !ok {
			continue
		}
		for _, variant := range variants {
			appendSchemaSearchText(variant, parts)
		}
	}
	for _, key := range []string{"$defs", "definitions"} {
		definitions, ok := schema[key].(map[string]any)
		if !ok {
			continue
		}
		names := make([]string, 0, len(definitions))
		for name := range definitions {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			appendSchemaSearchText(definitions[name], parts)
		}
	}
}

func tokenizeToolSearch(text string) []string {
	var tokens []string
	var word []rune
	var cjk []rune

	flushWord := func() {
		if len(word) == 0 {
			return
		}
		tokens = append(tokens, strings.ToLower(string(word)))
		word = word[:0]
	}
	flushCJK := func() {
		if len(cjk) == 0 {
			return
		}
		seen := make(map[string]bool, len(cjk)*2+1)
		appendUnique := func(token string) {
			if token != "" && !seen[token] {
				seen[token] = true
				tokens = append(tokens, token)
			}
		}
		appendUnique(string(cjk))
		for _, r := range cjk {
			appendUnique(string(r))
		}
		for i := 0; i+1 < len(cjk); i++ {
			appendUnique(string(cjk[i : i+2]))
		}
		cjk = cjk[:0]
	}

	for _, r := range text {
		switch {
		case isCJKSearchRune(r):
			flushWord()
			cjk = append(cjk, r)
		case unicode.IsLetter(r) || unicode.IsNumber(r):
			flushCJK()
			word = append(word, unicode.ToLower(r))
		default:
			flushWord()
			flushCJK()
		}
	}
	flushWord()
	flushCJK()
	return tokens
}

func isCJKSearchRune(r rune) bool {
	return unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Hangul)
}

func uniqueToolSearchTokens(tokens []string) []string {
	seen := make(map[string]bool, len(tokens))
	unique := make([]string, 0, len(tokens))
	for _, token := range tokens {
		if token == "" || seen[token] {
			continue
		}
		seen[token] = true
		unique = append(unique, token)
	}
	return unique
}
