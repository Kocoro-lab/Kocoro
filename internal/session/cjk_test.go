package session

import (
	"strings"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

func TestTokenize_Chinese(t *testing.T) {
	out := Tokenize("机器学习的原理")
	// gse should split this into multiple tokens, space-separated.
	if !strings.Contains(out, " ") {
		t.Fatalf("expected space-separated tokens, got %q", out)
	}
	// The original characters must all survive.
	for _, r := range "机器学习的原理" {
		if !strings.ContainsRune(out, r) {
			t.Errorf("rune %q missing from tokenized output %q", r, out)
		}
	}
}

func TestTokenize_Japanese(t *testing.T) {
	out := Tokenize("機械学習の原理")
	if !strings.Contains(out, " ") {
		t.Fatalf("expected space-separated tokens, got %q", out)
	}
	for _, r := range "機械学習の原理" {
		if !strings.ContainsRune(out, r) {
			t.Errorf("rune %q missing from tokenized output %q", r, out)
		}
	}
}

func TestTokenize_MixedLanguages(t *testing.T) {
	out := Tokenize("debug 登录接口 failed")
	// Latin portions stay verbatim.
	if !strings.Contains(out, "debug") {
		t.Errorf("expected 'debug' preserved, got %q", out)
	}
	if !strings.Contains(out, "failed") {
		t.Errorf("expected 'failed' preserved, got %q", out)
	}
}

func TestTokenize_Empty(t *testing.T) {
	if got := Tokenize(""); got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}

func TestIndex_ChineseSearch(t *testing.T) {
	dir := t.TempDir()
	idx, err := OpenIndex(dir)
	if err != nil {
		t.Fatalf("OpenIndex: %v", err)
	}
	defer idx.Close()

	now := time.Now().Truncate(time.Second)
	if err := idx.UpsertSession(&Session{
		ID: "zh-1", Title: "中文测试", CreatedAt: now, UpdatedAt: now,
		Messages: []client.Message{
			{Role: "user", Content: client.NewTextContent("帮我实现登录接口")},
			{Role: "assistant", Content: client.NewTextContent("好的，使用 OAuth2 协议")},
		},
	}); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		query     string
		wantMatch bool
	}{
		{"登录", true},     // two-character Chinese word
		{"接口", true},     // another two-character word
		{"OAuth2", true}, // mixed English in Chinese context
		{"不存在的词", false}, // no match
	}
	for _, c := range cases {
		t.Run(c.query, func(t *testing.T) {
			results, err := idx.Search(c.query, 20)
			if err != nil {
				t.Fatalf("Search(%q): %v", c.query, err)
			}
			if c.wantMatch && len(results) == 0 {
				t.Errorf("expected at least one result for %q", c.query)
			}
			if !c.wantMatch && len(results) != 0 {
				t.Errorf("expected no results for %q, got %d", c.query, len(results))
			}
		})
	}
}

func TestIndex_JapaneseSearch(t *testing.T) {
	dir := t.TempDir()
	idx, err := OpenIndex(dir)
	if err != nil {
		t.Fatalf("OpenIndex: %v", err)
	}
	defer idx.Close()

	now := time.Now().Truncate(time.Second)
	if err := idx.UpsertSession(&Session{
		ID: "ja-1", Title: "日本語テスト", CreatedAt: now, UpdatedAt: now,
		Messages: []client.Message{
			{Role: "user", Content: client.NewTextContent("機械学習のログイン機能を実装してください")},
		},
	}); err != nil {
		t.Fatal(err)
	}

	cases := []string{"機械学習", "ログイン", "実装"}
	for _, q := range cases {
		t.Run(q, func(t *testing.T) {
			results, err := idx.Search(q, 20)
			if err != nil {
				t.Fatalf("Search(%q): %v", q, err)
			}
			if len(results) == 0 {
				t.Errorf("expected match for %q", q)
			}
		})
	}
}

func TestIndex_SnippetPreservesOriginal(t *testing.T) {
	dir := t.TempDir()
	idx, err := OpenIndex(dir)
	if err != nil {
		t.Fatalf("OpenIndex: %v", err)
	}
	defer idx.Close()

	now := time.Now().Truncate(time.Second)
	original := "帮我实现登录接口，使用 OAuth2 协议完成授权流程"
	if err := idx.UpsertSession(&Session{
		ID: "snip-1", Title: "snippet", CreatedAt: now, UpdatedAt: now,
		Messages: []client.Message{
			{Role: "user", Content: client.NewTextContent(original)},
		},
	}); err != nil {
		t.Fatal(err)
	}

	results, err := idx.Search("登录", 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 {
		t.Fatal("expected result")
	}
	snip := results[0].Snippet
	// Snippet must contain the highlight markers around "登录".
	if !strings.Contains(snip, ">>>登录<<<") {
		t.Errorf("expected >>>登录<<< in snippet, got %q", snip)
	}
	// Snippet must not contain space-separated CJK (i.e. tokenized form leaking through).
	if strings.Contains(snip, "帮 我") || strings.Contains(snip, "登 录") {
		t.Errorf("snippet leaked tokenized form: %q", snip)
	}
	// Stripping the highlight markers should yield a substring of the original.
	stripped := strings.ReplaceAll(strings.ReplaceAll(snip, ">>>", ""), "<<<", "")
	stripped = strings.TrimPrefix(stripped, "...")
	stripped = strings.TrimSuffix(stripped, "...")
	if !strings.Contains(original, stripped) {
		t.Errorf("snippet %q (stripped: %q) is not a substring of original %q", snip, stripped, original)
	}
}

func TestIndex_EnglishStillWorks(t *testing.T) {
	dir := t.TempDir()
	idx, err := OpenIndex(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	now := time.Now().Truncate(time.Second)
	if err := idx.UpsertSession(&Session{
		ID: "en-1", Title: "english", CreatedAt: now, UpdatedAt: now,
		Messages: []client.Message{
			{Role: "user", Content: client.NewTextContent("the server is running on port 8080")},
		},
	}); err != nil {
		t.Fatal(err)
	}

	// Porter stemming: searching "run" should still match "running".
	results, err := idx.Search("run", 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 {
		t.Error("expected stemmed match 'run' -> 'running'")
	}
}

func TestIndex_VersionGateTriggersRebuild(t *testing.T) {
	dir := t.TempDir()

	// Open index with current version and insert a session.
	idx1, err := OpenIndex(dir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Truncate(time.Second)
	if err := idx1.UpsertSession(&Session{
		ID: "v-1", Title: "v", CreatedAt: now, UpdatedAt: now,
		Messages: []client.Message{
			{Role: "user", Content: client.NewTextContent("alpha content")},
		},
	}); err != nil {
		t.Fatal(err)
	}
	idx1.Close()

	// Simulate an older tokenizer version stamp by flipping user_version.
	// We reopen, roll the version back, and close.
	if _, err := openRaw(dir).Exec(`PRAGMA user_version = 1`); err != nil {
		t.Fatal(err)
	}

	idx2, err := OpenIndex(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer idx2.Close()

	if !idx2.NeedsRebuild() {
		t.Error("expected NeedsRebuild true after version rollback")
	}
	// After DROP TABLE, the sessions table should be empty.
	empty, _ := idx2.IsEmpty()
	if !empty {
		t.Error("expected index to be empty after version gate dropped tables")
	}

	// End-to-end: Rebuild from JSON should restore searchability.
	store := &Store{dir: dir, index: idx2}
	if err := store.Save(&Session{
		ID: "v-1", Title: "v", CreatedAt: now, UpdatedAt: now,
		Messages: []client.Message{
			{Role: "user", Content: client.NewTextContent("alpha content")},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := idx2.Rebuild(store); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	results, err := idx2.Search("alpha", 20)
	if err != nil {
		t.Fatalf("Search after rebuild: %v", err)
	}
	if len(results) == 0 || results[0].SessionID != "v-1" {
		t.Errorf("expected to find session v-1 after rebuild, got %+v", results)
	}
	if idx2.NeedsRebuild() {
		t.Error("expected NeedsRebuild false after Rebuild completes")
	}
}

// openRaw opens the sessions.db without the version gate logic (for tests).
func openRaw(dir string) *rawDB {
	idx, err := OpenIndex(dir)
	if err != nil {
		panic(err)
	}
	return &rawDB{idx: idx}
}

type rawDB struct{ idx *Index }

func (r *rawDB) Exec(q string, args ...any) (any, error) {
	_, err := r.idx.db.Exec(q, args...)
	r.idx.Close()
	return nil, err
}
