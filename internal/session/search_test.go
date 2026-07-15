package session

import (
	"strings"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

func toolResultMsg(text string) client.Message {
	return client.Message{
		Role:    "user",
		Content: client.NewBlockContent([]client.ContentBlock{{Type: "tool_result", ToolContent: text}}),
	}
}

func toolUseMsg(narration string) client.Message {
	return client.Message{
		Role: "assistant",
		Content: client.NewBlockContent([]client.ContentBlock{
			{Type: "text", Text: narration},
			{Type: "tool_use", ToolUseID: "t1"},
		}),
	}
}

func TestSearchableMessageText_ExcludesToolDumps(t *testing.T) {
	// User tool_result carrier → skipped entirely.
	if got := searchableMessageText(toolResultMsg("%PDF-1.7 endobj binary dump")); got != "" {
		t.Errorf("tool_result user message should be skipped, got %q", got)
	}
	// Assistant message carrying a tool_use → skipped (mid-turn, not a reply).
	if got := searchableMessageText(toolUseMsg("let me search")); got != "" {
		t.Errorf("assistant tool_use message should be skipped, got %q", got)
	}
	// Plain user + plain assistant text → kept.
	u := client.Message{Role: "user", Content: client.NewTextContent("how is the weather")}
	if got := searchableMessageText(u); got != "how is the weather" {
		t.Errorf("user text = %q", got)
	}
	a := client.Message{Role: "assistant", Content: client.NewTextContent("it is sunny")}
	if got := searchableMessageText(a); got != "it is sunny" {
		t.Errorf("assistant text = %q", got)
	}
}

func TestBuildSnippetSegs(t *testing.T) {
	// No match → best falls to 0, whole (short) content, no hit segment.
	segs := buildSnippetSegs("nothing here", []string{"zzz"})
	for _, s := range segs {
		if s.Hit {
			t.Fatalf("no term should be highlighted: %+v", segs)
		}
	}

	// Single hit is segmented and flagged.
	segs = buildSnippetSegs("the quick brown fox jumps", []string{"fox"})
	var hit string
	for _, s := range segs {
		if s.Hit {
			hit += s.Text
		}
	}
	if hit != "fox" {
		t.Errorf("expected the hit segment to be 'fox', got %q (segs=%+v)", hit, segs)
	}

	// Long body → leading/trailing ellipsis segments around the window.
	long := strings.Repeat("a", 200) + " needle " + strings.Repeat("b", 200)
	segs = buildSnippetSegs(long, []string{"needle"})
	if segs[0].Text != "…" || segs[len(segs)-1].Text != "…" {
		t.Errorf("expected ellipsis on both ends, got first=%q last=%q", segs[0].Text, segs[len(segs)-1].Text)
	}
	joined := ""
	for _, s := range segs {
		if s.Hit {
			joined = s.Text
		}
	}
	if joined != "needle" {
		t.Errorf("expected hit 'needle', got %q", joined)
	}

	// CJK content highlights correctly (rune-based, not byte-based).
	segs = buildSnippetSegs("这是一段关于天气的对话", []string{"天气"})
	joined = ""
	for _, s := range segs {
		if s.Hit {
			joined += s.Text
		}
	}
	if joined != "天气" {
		t.Errorf("expected CJK hit '天气', got %q", joined)
	}
}

func TestHighlightTerms_StripsOperators(t *testing.T) {
	got := highlightTerms([]string{"cat", "OR", "dog", "NEAR(x", "foo*"})
	want := []string{"cat", "dog", "foo"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("highlightTerms = %v, want %v", got, want)
	}
}

func openTestIndex(t *testing.T) *Index {
	t.Helper()
	idx, err := OpenIndex(t.TempDir())
	if err != nil {
		t.Fatalf("OpenIndex: %v", err)
	}
	t.Cleanup(func() { idx.Close() })
	return idx
}

func msgSession(id, title string, at time.Time, msgs ...client.Message) *Session {
	return &Session{ID: id, Title: title, CreatedAt: at, UpdatedAt: at, Messages: msgs}
}

func TestSearchSessions_GroupsBySessionWithMatchCount(t *testing.T) {
	idx := openTestIndex(t)
	now := time.Now().Truncate(time.Second)
	mustUpsert(t, idx, msgSession("s-banana", "Fruit chat", now,
		client.Message{Role: "user", Content: client.NewTextContent("I love banana bread")},
		client.Message{Role: "assistant", Content: client.NewTextContent("banana is great")},
	))
	mustUpsert(t, idx, msgSession("s-apple", "Other", now.Add(-time.Hour),
		client.Message{Role: "user", Content: client.NewTextContent("apple pie recipe")},
	))

	hits, err := idx.SearchSessions("banana")
	if err != nil {
		t.Fatalf("SearchSessions: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("expected 1 grouped session hit, got %d (%+v)", len(hits), hits)
	}
	if hits[0].SessionID != "s-banana" {
		t.Errorf("wrong session: %q", hits[0].SessionID)
	}
	if hits[0].MatchCount != 2 {
		t.Errorf("expected match_count 2 (both messages), got %d", hits[0].MatchCount)
	}
	if hits[0].Title != "Fruit chat" {
		t.Errorf("title = %q", hits[0].Title)
	}
}

func TestSearchSessions_CJKShortTermLikeFallback(t *testing.T) {
	idx := openTestIndex(t)
	now := time.Now().Truncate(time.Second)
	mustUpsert(t, idx, msgSession("s-cjk", "登录问题", now,
		client.Message{Role: "user", Content: client.NewTextContent("我遇到了登录失败的问题")},
	))
	// "登录" is 2 runes → routes through the LIKE fallback (trigram needs >=3).
	hits, err := idx.SearchSessions("登录")
	if err != nil {
		t.Fatalf("SearchSessions: %v", err)
	}
	if len(hits) != 1 || hits[0].SessionID != "s-cjk" {
		t.Fatalf("expected the CJK session, got %+v", hits)
	}
}

func TestSearchSessions_ToolDumpNotSearchable(t *testing.T) {
	idx := openTestIndex(t)
	now := time.Now().Truncate(time.Second)
	mustUpsert(t, idx, msgSession("s-pdf", "PDF read", now,
		client.Message{Role: "user", Content: client.NewTextContent("summarize this")},
		toolResultMsg("%PDF-1.7 ... endobj ... binary blob"),
	))
	// The tool_result body is excluded from the index, so its tokens don't match.
	hits, err := idx.SearchSessions("endobj")
	if err != nil {
		t.Fatalf("SearchSessions: %v", err)
	}
	if len(hits) != 0 {
		t.Errorf("tool-dump content must not be searchable, got %+v", hits)
	}
}

func TestSearchSessions_ShortTermWithOperatorErrors(t *testing.T) {
	idx := openTestIndex(t)
	if _, err := idx.SearchSessions("ab AND cd"); err == nil {
		t.Error("expected an error for a <3-rune term combined with a boolean operator")
	}
}
