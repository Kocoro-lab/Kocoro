package session

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

// searchableMessageText returns the clean, human-authored text of a message for
// the content-search index — the user's questions and the model's final
// answers — deliberately EXCLUDING tool_result output (PDF/doc/command dumps)
// and mid-turn tool_use narration. This mirrors the predicate
// internal/context.buildTitleTranscript uses, kept local here to avoid a
// package cycle. Excluding tool dumps both fixes search-result noise (a session
// that read a PDF no longer floods results) and keeps the FTS index small.
func searchableMessageText(m client.Message) string {
	switch m.Role {
	case "user":
		if !m.Content.HasBlocks() {
			return strings.TrimSpace(m.Content.Text())
		}
		var parts []string
		for _, b := range m.Content.Blocks() {
			switch b.Type {
			case "tool_result":
				// A tool-result carrier message (PDF/doc/command output) — skip
				// the whole message so its dump never enters search.
				return ""
			case "text":
				if t := strings.TrimSpace(b.Text); t != "" {
					parts = append(parts, t)
				}
			}
		}
		return strings.TrimSpace(strings.Join(parts, " "))
	case "assistant":
		for _, b := range m.Content.Blocks() {
			if b.Type == "tool_use" {
				// Mid-turn tool call, not a final reply — skip.
				return ""
			}
		}
		return strings.TrimSpace(m.Content.Text())
	}
	return ""
}

// SnippetSeg is one run of a highlighted snippet: consecutive characters that
// are either all matched (Hit) or all context. Returning pre-split segments
// (rather than byte/rune offsets) avoids any Go-UTF8 vs JS-UTF16 offset mismatch
// at the client boundary — the renderer just wraps Hit segments in <mark>.
type SnippetSeg struct {
	Text string `json:"text"`
	Hit  bool   `json:"hit"`
}

// SessionHit is one search result grouped at the SESSION level: the session's
// best-matching message plus a highlighted snippet and how many of its messages
// matched. Title matching is intentionally left to the client (it holds the full
// session list in memory and fuzzy-matches titles instantly); this endpoint
// covers full-text CONTENT search, which the client cannot do cheaply.
type SessionHit struct {
	SessionID  string       `json:"session_id"`
	Agent      string       `json:"agent"`
	Title      string       `json:"title"`
	CreatedAt  time.Time    `json:"created_at"`
	UpdatedAt  time.Time    `json:"updated_at"`
	MsgIndex   int          `json:"msg_index"`
	Role       string       `json:"role"`
	Snippet    []SnippetSeg `json:"snippet"`
	MatchCount int          `json:"match_count"`
}

// searchScanCap bounds how many matching message rows we pull before grouping.
// Generous for a single-user desktop corpus and cheap to group in Go; the FTS/
// LIKE query already orders best-first, so truncation only drops low-priority
// tail matches.
const searchScanCap = 400

type sessionMatchRow struct {
	sessionID, title, role, content string
	msgIndex                        int
	created, updated                time.Time
}

// SearchSessions runs a content search and returns results grouped by session
// (one hit per session: its best-matching message, a highlighted snippet, and
// its match count), best-first. Short/CJK (<3-rune) terms route through a LIKE
// scan; longer terms use the FTS5 trigram index; a colon-bearing query that
// FTS5 misreads as column syntax falls back to LIKE.
func (idx *Index) SearchSessions(query string) ([]SessionHit, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, nil
	}
	terms := splitQueryTerms(query)

	hasShort := false
	for _, t := range terms {
		if utf8.RuneCountInString(t) < 3 {
			hasShort = true
			break
		}
	}
	hasOperator := containsFTSOperator(query)

	var rows []sessionMatchRow
	var err error
	if hasShort {
		if hasOperator {
			return nil, fmt.Errorf("invalid search query: terms shorter than 3 characters cannot be combined with AND/OR/NOT/NEAR or wildcard operators; remove operators or use longer terms")
		}
		rows, err = idx.matchRowsLike(terms)
		if err != nil {
			return nil, err
		}
	} else {
		rows, err = idx.matchRowsFTS(query)
		if err != nil {
			if isFTSColumnError(err) {
				rows, err = idx.matchRowsLike(terms)
				if err != nil {
					return nil, err
				}
			} else if isFTSSyntaxError(err) {
				return nil, fmt.Errorf("invalid search query: %s", query)
			} else {
				return nil, err
			}
		}
	}

	// Terms to highlight in snippets: the query words minus FTS operators, so a
	// query like "cat OR dog" doesn't <mark> a literal "OR"/"and" in the body.
	hlTerms := highlightTerms(terms)

	// Group by session, preserving the query's best-first row order.
	order := make([]string, 0, len(rows))
	byID := make(map[string]*SessionHit, len(rows))
	counts := make(map[string]int, len(rows))
	for i := range rows {
		r := rows[i]
		counts[r.sessionID]++
		if _, ok := byID[r.sessionID]; ok {
			continue
		}
		order = append(order, r.sessionID)
		byID[r.sessionID] = &SessionHit{
			SessionID: r.sessionID,
			Title:     r.title,
			CreatedAt: r.created,
			UpdatedAt: r.updated,
			MsgIndex:  r.msgIndex,
			Role:      r.role,
			Snippet:   buildSnippetSegs(r.content, hlTerms),
		}
	}
	hits := make([]SessionHit, 0, len(order))
	for _, id := range order {
		h := byID[id]
		h.MatchCount = counts[id]
		hits = append(hits, *h)
	}
	return hits, nil
}

func (idx *Index) matchRowsFTS(query string) ([]sessionMatchRow, error) {
	rs, err := idx.db.Query(
		`SELECT m.session_id, s.title, m.role, m.msg_index, s.created_at, s.updated_at, m.content
		 FROM messages_fts
		 JOIN messages m ON m.rowid = messages_fts.rowid
		 JOIN sessions s ON s.id = m.session_id
		 WHERE messages_fts MATCH ?
		 ORDER BY rank
		 LIMIT ?`,
		query, searchScanCap,
	)
	if err != nil {
		return nil, err
	}
	return scanMatchRows(rs)
}

func (idx *Index) matchRowsLike(terms []string) ([]sessionMatchRow, error) {
	if len(terms) == 0 {
		return nil, nil
	}
	clauses := make([]string, 0, len(terms))
	args := make([]any, 0, len(terms)+1)
	for _, t := range terms {
		clauses = append(clauses, `m.content LIKE ? ESCAPE '\'`)
		args = append(args, "%"+escapeLike(t)+"%")
	}
	args = append(args, searchScanCap)
	rs, err := idx.db.Query(
		`SELECT m.session_id, s.title, m.role, m.msg_index, s.created_at, s.updated_at, m.content
		 FROM messages m
		 JOIN sessions s ON s.id = m.session_id
		 WHERE `+strings.Join(clauses, " AND ")+`
		 ORDER BY s.updated_at DESC
		 LIMIT ?`,
		args...,
	)
	if err != nil {
		return nil, fmt.Errorf("search sessions (like): %w", err)
	}
	return scanMatchRows(rs)
}

func scanMatchRows(rs interface {
	Next() bool
	Scan(...any) error
	Close() error
	Err() error
}) ([]sessionMatchRow, error) {
	defer rs.Close()
	var out []sessionMatchRow
	for rs.Next() {
		var r sessionMatchRow
		var createdStr, updatedStr string
		if err := rs.Scan(&r.sessionID, &r.title, &r.role, &r.msgIndex, &createdStr, &updatedStr, &r.content); err != nil {
			return nil, fmt.Errorf("scan match row: %w", err)
		}
		r.created = parseTime(createdStr)
		r.updated = parseTime(updatedStr)
		out = append(out, r)
	}
	return out, rs.Err()
}

// buildSnippetSegs produces a whitespace-collapsed window (± snippetRadius runes)
// centered on the earliest matching term, with every term occurrence inside the
// window flagged Hit. Case-insensitive; rune-based so CJK is safe. Leading/
// trailing ellipsis segments mark truncation.
const snippetRadius = 42

func buildSnippetSegs(content string, terms []string) []SnippetSeg {
	content = strings.Join(strings.Fields(content), " ")
	runes := []rune(content)
	lower := []rune(strings.ToLower(content))
	n := len(runes)

	termRunes := make([][]rune, 0, len(terms))
	best := -1
	for _, t := range terms {
		tr := []rune(strings.ToLower(strings.TrimSpace(t)))
		if len(tr) == 0 {
			continue
		}
		termRunes = append(termRunes, tr)
		if i := runeIndexOf(lower, tr, 0); i >= 0 && (best < 0 || i < best) {
			best = i
		}
	}
	if best < 0 {
		best = 0
	}
	from := best - snippetRadius
	if from < 0 {
		from = 0
	}
	to := best + snippetRadius
	if to > n {
		to = n
	}

	hit := make([]bool, to-from)
	for _, tr := range termRunes {
		for i := from; ; {
			j := runeIndexOf(lower, tr, i)
			if j < 0 || j >= to {
				break
			}
			for k := j; k < j+len(tr) && k < to; k++ {
				hit[k-from] = true
			}
			i = j + len(tr)
		}
	}

	segs := make([]SnippetSeg, 0, 8)
	if from > 0 {
		segs = append(segs, SnippetSeg{Text: "…"})
	}
	if to > from {
		start := from
		cur := hit[0]
		for pos := from + 1; pos < to; pos++ {
			if hit[pos-from] != cur {
				segs = append(segs, SnippetSeg{Text: string(runes[start:pos]), Hit: cur})
				start = pos
				cur = hit[pos-from]
			}
		}
		segs = append(segs, SnippetSeg{Text: string(runes[start:to]), Hit: cur})
	}
	if to < n {
		segs = append(segs, SnippetSeg{Text: "…"})
	}
	return segs
}

// highlightTerms strips FTS5 operator tokens (AND/OR/NOT/NEAR) and wildcard
// markers from the query terms so snippet highlighting doesn't <mark> a literal
// operator word that happens to appear in the body. Matching already ran on the
// full query; this only affects which substrings are visually highlighted.
func highlightTerms(terms []string) []string {
	out := make([]string, 0, len(terms))
	for _, t := range terms {
		if t == "AND" || t == "OR" || t == "NOT" || strings.HasPrefix(t, "NEAR(") {
			continue
		}
		if t = strings.Trim(t, "*"); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// runeIndexOf returns the index in hay of the first occurrence of needle at or
// after `from`, or -1. Both are already lowercased by the caller.
func runeIndexOf(hay, needle []rune, from int) int {
	if len(needle) == 0 || from < 0 {
		return -1
	}
	for i := from; i+len(needle) <= len(hay); i++ {
		match := true
		for j := 0; j < len(needle); j++ {
			if hay[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}
