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
	// toolSearchDefaultLimit bounds ranked seed schemas returned for one
	// discovery request. Eight covers the normal single-family workload before
	// core-family expansion; a result capped here means the query should be
	// refined or exact names loaded with select: rather than raising prompt cost
	// for every search.
	toolSearchDefaultLimit = 8

	// Standard BM25 defaults: K1 controls term-frequency saturation and B
	// controls document-length normalization.
	toolSearchBM25K1 = 1.2
	toolSearchBM25B  = 0.75

	// Prefix fallback targets normal partial-name queries, not one- or
	// two-character fan-out across a large catalog. Users can use the complete
	// short token or select: for exact lookup; lower this only with ranking and
	// result-volume coverage for the intended catalog.
	toolSearchPrefixMinRunes = 3

	// JSON Schema is recursive. Sixteen levels covers practical generated tool
	// schemas while bounding pathological nesting that would otherwise grow
	// search-document work without limit. Flatten the schema or deliberately
	// raise this cap with a focused traversal test when a legitimate schema
	// needs more depth.
	toolSearchSchemaMaxDepth = 16
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
	documents []toolSearchCorpusDocument
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
	}
	return toolSearchCorpus{documents: documents}
}

func buildToolSearchIndex(corpus toolSearchCorpus) *toolSearchIndex {
	index := &toolSearchIndex{
		documentFrequency: make(map[string]int),
	}
	if len(corpus.documents) == 0 {
		return index
	}

	index.documents = make([]toolSearchDocument, 0, len(corpus.documents))
	totalLength := 0
	for _, raw := range corpus.documents {
		tokens, normalizedLength := tokenizeToolSearchDocument(raw.text)
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
			length:    normalizedLength,
			frequency: frequency,
		})
		totalLength += normalizedLength
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
	if len(names) == 0 {
		return index.searchTokenPrefixes(queryTokens, limit)
	}
	return names
}

// searchTokenPrefixes is a bounded compatibility fallback for partial tool
// names such as "sched" → "schedule_create". Exact-token BM25 remains the
// primary ranker; prefix matching runs only when it produces no results, so
// broad description substrings cannot displace relevant BM25 hits.
func (index *toolSearchIndex) searchTokenPrefixes(queryTokens []string, limit int) []string {
	hits := make([]toolSearchHit, 0, len(index.documents))
	for _, document := range index.documents {
		score := 0.0
		for _, queryToken := range queryTokens {
			if len([]rune(queryToken)) < toolSearchPrefixMinRunes {
				continue
			}
			for token := range document.frequency {
				if strings.HasPrefix(token, queryToken) {
					score++
					break
				}
			}
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
// metadata. The canonical and separator-normalized names intentionally repeat
// name tokens, giving names more weight than incidental description matches
// without a separate SearchHint maintenance field.
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
	appendSchemaSearchTextAtDepth(value, parts, 0)
}

func appendSchemaSearchTextAtDepth(value any, parts *[]string, depth int) {
	schema, ok := value.(map[string]any)
	if !ok {
		return
	}
	if description, ok := schema["description"].(string); ok {
		if description = strings.TrimSpace(description); description != "" {
			*parts = append(*parts, description)
		}
	}
	if depth >= toolSearchSchemaMaxDepth {
		return
	}
	if properties, ok := schema["properties"].(map[string]any); ok {
		names := make([]string, 0, len(properties))
		for name := range properties {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			*parts = append(*parts, name, normalizeToolSearchSeparators(name))
			appendSchemaSearchTextAtDepth(properties[name], parts, depth+1)
		}
	}
	for _, key := range []string{"items", "additionalProperties"} {
		appendSchemaSearchTextAtDepth(schema[key], parts, depth+1)
	}
	for _, key := range []string{"anyOf", "allOf", "oneOf"} {
		variants, ok := schema[key].([]any)
		if !ok {
			continue
		}
		for _, variant := range variants {
			appendSchemaSearchTextAtDepth(variant, parts, depth+1)
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
			appendSchemaSearchTextAtDepth(definitions[name], parts, depth+1)
		}
	}
}

func tokenizeToolSearch(text string) []string {
	tokens, _ := tokenizeToolSearchDocument(text)
	return tokens
}

// tokenizeToolSearchDocument returns search features plus a normalized source
// length for BM25. CJK unigrams and bigrams retain repeated term frequency, but
// the length counts source runes rather than generated n-gram features.
func tokenizeToolSearchDocument(text string) ([]string, int) {
	var tokens []string
	var word []rune
	var cjk []rune
	normalizedLength := 0

	flushWord := func() {
		if len(word) == 0 {
			return
		}
		tokens = append(tokens, strings.ToLower(string(word)))
		normalizedLength++
		word = word[:0]
	}
	flushCJK := func() {
		if len(cjk) == 0 {
			return
		}
		normalizedLength += len(cjk)
		for _, r := range cjk {
			tokens = append(tokens, string(r))
		}
		for i := 0; i+1 < len(cjk); i++ {
			tokens = append(tokens, string(cjk[i:i+2]))
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
	return tokens, normalizedLength
}

func deferredToolSearchKey(deferred map[string]bool) string {
	if len(deferred) == 0 {
		return ""
	}
	names := make([]string, 0, len(deferred))
	for name, isDeferred := range deferred {
		if isDeferred {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	h := sha256.New()
	for _, name := range names {
		_, _ = h.Write([]byte(name))
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
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
