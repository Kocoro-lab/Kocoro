package session

import (
	"database/sql"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

func openRawDBImpl(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0644)
}

// seed populates an index with a controlled CJK / JP / EN / mixed corpus.
func seed(t *testing.T) *Index {
	t.Helper()
	dir := t.TempDir()
	idx, err := OpenIndex(dir)
	if err != nil {
		t.Fatalf("OpenIndex: %v", err)
	}
	t.Cleanup(func() { idx.Close() })

	now := time.Now().Truncate(time.Second)
	sessions := []*Session{
		{ID: "zh-1", Title: "登录", CreatedAt: now, UpdatedAt: now,
			Messages: []client.Message{
				{Role: "user", Content: client.NewTextContent("帮我实现登录接口，使用 OAuth2 协议完成授权流程")},
				{Role: "assistant", Content: client.NewTextContent("好的，JWT 签发 token")},
			}},
		{ID: "zh-2", Title: "修复", CreatedAt: now, UpdatedAt: now,
			Messages: []client.Message{
				{Role: "user", Content: client.NewTextContent("生产环境部署失败，需要修复 nginx 配置")},
			}},
		{ID: "zh-3", Title: "机器学习", CreatedAt: now, UpdatedAt: now,
			Messages: []client.Message{
				{Role: "user", Content: client.NewTextContent("机器学习的原理和应用，深度学习神经网络")},
			}},
		{ID: "ja-1", Title: "機械学習", CreatedAt: now, UpdatedAt: now,
			Messages: []client.Message{
				{Role: "user", Content: client.NewTextContent("機械学習のログイン機能を実装してください")},
				{Role: "assistant", Content: client.NewTextContent("実装が完了しました")},
			}},
		{ID: "en-1", Title: "server", CreatedAt: now, UpdatedAt: now,
			Messages: []client.Message{
				{Role: "user", Content: client.NewTextContent("the server is running on port 8080 with multiple connections")},
				{Role: "assistant", Content: client.NewTextContent("several programs deployed, all connections stable")},
			}},
		{ID: "mix-1", Title: "mixed", CreatedAt: now, UpdatedAt: now,
			Messages: []client.Message{
				{Role: "user", Content: client.NewTextContent("debug 登录接口 failed on port 8080")},
			}},
	}
	for _, s := range sessions {
		if err := idx.UpsertSession(s); err != nil {
			t.Fatal(err)
		}
	}
	return idx
}

func TestTrigram_2CharCJK_LikeFallback(t *testing.T) {
	idx := seed(t)
	cases := map[string][]string{
		"登录": {"zh-1", "mix-1"},
		"接口": {"zh-1", "mix-1"},
		"修复": {"zh-2"},
		"部署": {"zh-2"},
		"実装": {"ja-1"},
	}
	for q, wantSessions := range cases {
		t.Run(q, func(t *testing.T) {
			res, err := idx.Search(q, 10)
			if err != nil {
				t.Fatalf("Search(%q): %v", q, err)
			}
			got := make(map[string]bool)
			for _, r := range res {
				got[r.SessionID] = true
			}
			for _, want := range wantSessions {
				if !got[want] {
					t.Errorf("expected hit in %s for %q, got %v", want, q, keys(got))
				}
			}
			if len(res) > 0 && !strings.Contains(res[0].Snippet, ">>>") {
				t.Errorf("expected highlight in snippet for %q, got %q", q, res[0].Snippet)
			}
		})
	}
}

func TestTrigram_3CharCJK(t *testing.T) {
	idx := seed(t)
	cases := map[string]string{
		"机器学习": "zh-3",
		"登录接口": "zh-1",
		"機械学習": "ja-1",
	}
	for q, wantID := range cases {
		t.Run(q, func(t *testing.T) {
			res, err := idx.Search(q, 5)
			if err != nil {
				t.Fatalf("Search: %v", err)
			}
			if len(res) == 0 {
				t.Fatalf("expected hits for %q", q)
			}
			found := false
			for _, r := range res {
				if r.SessionID == wantID {
					found = true
				}
			}
			if !found {
				t.Errorf("expected %s in hits for %q, got results %+v", wantID, q, res)
			}
		})
	}
}

func TestTrigram_JapaneseMixed(t *testing.T) {
	idx := seed(t)
	// kana + kanji compound.
	res, err := idx.Search("ログイン機能", 5)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res) == 0 {
		t.Fatal("expected hit for ログイン機能")
	}
	if res[0].SessionID != "ja-1" {
		t.Errorf("expected ja-1, got %s", res[0].SessionID)
	}
}

func TestTrigram_QuotedPhrase(t *testing.T) {
	idx := seed(t)
	// Quoted CJK phrase should match adjacent occurrence.
	res, err := idx.Search(`"登录接口"`, 5)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res) == 0 {
		t.Fatal("expected hits for quoted CJK phrase")
	}
	// Quoted EN phrase.
	res, err = idx.Search(`"port 8080"`, 5)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res) == 0 {
		t.Fatal("expected hits for quoted EN phrase")
	}
}

func TestTrigram_EnglishSubstring(t *testing.T) {
	idx := seed(t)
	// Trigram provides substring match (not porter stemming). 'run' matches
	// 'running' because 'run' is a trigram of 'running'.
	cases := []string{"run", "program", "deploy", "connection"}
	for _, q := range cases {
		t.Run(q, func(t *testing.T) {
			res, err := idx.Search(q, 5)
			if err != nil {
				t.Fatalf("Search: %v", err)
			}
			if len(res) == 0 {
				t.Errorf("expected hits for %q", q)
			}
			// Native snippet() should highlight.
			if len(res) > 0 && !strings.Contains(res[0].Snippet, ">>>") {
				t.Errorf("expected highlight for %q, got %q", q, res[0].Snippet)
			}
		})
	}
}

func TestTrigram_MixedLatinCJK(t *testing.T) {
	idx := seed(t)
	res, err := idx.Search("OAuth2", 5)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res) == 0 {
		t.Fatal("expected hits for OAuth2")
	}
	if res[0].SessionID != "zh-1" {
		t.Errorf("expected zh-1, got %s", res[0].SessionID)
	}
}

func TestTrigram_VersionGateRebuild(t *testing.T) {
	dir := t.TempDir()
	idx1, err := OpenIndex(dir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Truncate(time.Second)
	if err := idx1.UpsertSession(&Session{
		ID: "v-1", Title: "v", CreatedAt: now, UpdatedAt: now,
		Messages: []client.Message{{Role: "user", Content: client.NewTextContent("登录接口 test")}},
	}); err != nil {
		t.Fatal(err)
	}
	idx1.Close()

	// Roll version back to simulate upgrade-from-porter.
	idx2raw, err := OpenIndex(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := idx2raw.db.Exec(`PRAGMA user_version = 1`); err != nil {
		t.Fatal(err)
	}
	idx2raw.Close()

	idx3, err := OpenIndex(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer idx3.Close()
	if !idx3.NeedsRebuild() {
		t.Error("expected NeedsRebuild true after version rollback")
	}
	// messages table was dropped and re-created empty — FTS should return nothing.
	var n int
	if err := idx3.db.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("expected empty messages table after version-gate drop, got %d rows", n)
	}
}

// TestTrigram_VersionGateThroughNewStore guards against a regression where
// OpenIndex drops stale FTS tables on version change but the NewStore
// auto-rebuild trigger misses the signal, leaving search permanently empty.
func TestTrigram_VersionGateThroughNewStore(t *testing.T) {
	dir := t.TempDir()

	s1 := NewStore(dir)
	now := time.Now().Truncate(time.Second)
	if err := s1.Save(&Session{
		ID: "s", Title: "t", CreatedAt: now, UpdatedAt: now,
		Messages: []client.Message{{Role: "user", Content: client.NewTextContent("登录接口 failed")}},
	}); err != nil {
		t.Fatal(err)
	}
	s1.Close()

	// Simulate an older tokenizer version on disk.
	idx, err := OpenIndex(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := idx.db.Exec(`PRAGMA user_version = 1`); err != nil {
		t.Fatal(err)
	}
	idx.Close()

	// Reopen via NewStore — the real application flow. Must rebuild.
	s2 := NewStore(dir)
	defer s2.Close()
	res, err := s2.Search("登录接口", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) == 0 {
		t.Error("expected rebuild to restore searchability after version change via NewStore")
	}
}

// TestTrigram_MixedShortAndLongTerms guards against silently dropping short
// CJK terms in mixed queries. FTS5 trigram ignores terms <3 chars, so without
// per-term fallback analysis, `登录 failed` would match rows that only contain
// "failed" with no 登录 at all.
func TestTrigram_MixedShortAndLongTerms(t *testing.T) {
	dir := t.TempDir()
	idx, err := OpenIndex(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	now := time.Now().Truncate(time.Second)
	if err := idx.UpsertSession(&Session{
		ID: "target", Title: "t", CreatedAt: now, UpdatedAt: now,
		Messages: []client.Message{{Role: "user", Content: client.NewTextContent("登录接口 failed on port 8080")}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := idx.UpsertSession(&Session{
		ID: "noise", Title: "n", CreatedAt: now, UpdatedAt: now,
		Messages: []client.Message{{Role: "user", Content: client.NewTextContent("the build failed with no CJK")}},
	}); err != nil {
		t.Fatal(err)
	}

	res, err := idx.Search("登录 failed", 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range res {
		if r.SessionID == "noise" {
			t.Errorf("mixed query `登录 failed` incorrectly matched 'noise' session with no 登录")
		}
	}
	if len(res) == 0 {
		t.Error("expected at least the 'target' session to match")
	}
}

// TestTrigram_QuotedShortCJK ensures `"登录"` works the same as `登录`.
func TestTrigram_QuotedShortCJK(t *testing.T) {
	dir := t.TempDir()
	idx, err := OpenIndex(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	now := time.Now().Truncate(time.Second)
	if err := idx.UpsertSession(&Session{
		ID: "s", Title: "t", CreatedAt: now, UpdatedAt: now,
		Messages: []client.Message{{Role: "user", Content: client.NewTextContent("帮我实现登录接口")}},
	}); err != nil {
		t.Fatal(err)
	}
	unquoted, _ := idx.Search("登录", 5)
	quoted, _ := idx.Search(`"登录"`, 5)
	if len(quoted) != len(unquoted) {
		t.Errorf("quoted short CJK should match like unquoted: quoted=%d unquoted=%d", len(quoted), len(unquoted))
	}
}

// TestTrigram_UpgradeFromMainStored0 covers the real upgrade path: existing
// users on current main have a porter-tokenized FTS and user_version=0 (main
// never stamped it). The migration must drop the stale FTS and trigger a
// rebuild so trigram semantics (substring match) take effect.
func TestTrigram_UpgradeFromMainStored0(t *testing.T) {
	dir := t.TempDir()

	// Construct a pre-trigram DB directly: porter+unicode61 FTS, no user_version.
	raw, err := openRawDBImpl(dir + "/sessions.db")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`
PRAGMA journal_mode=WAL;
CREATE TABLE sessions (id TEXT PRIMARY KEY, title TEXT NOT NULL DEFAULT '',
    cwd TEXT NOT NULL DEFAULT '', created_at DATETIME NOT NULL,
    updated_at DATETIME NOT NULL, msg_count INTEGER NOT NULL DEFAULT 0);
CREATE TABLE messages (rowid INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id TEXT NOT NULL, msg_index INTEGER NOT NULL, role TEXT NOT NULL,
    content TEXT NOT NULL, UNIQUE(session_id, msg_index));
CREATE VIRTUAL TABLE messages_fts USING fts5(content, content=messages,
    content_rowid=rowid, tokenize='porter unicode61');
`); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Truncate(time.Second).Format(time.RFC3339Nano)
	raw.Exec(`INSERT INTO sessions (id,title,created_at,updated_at,msg_count) VALUES ('s','t',?,?,1)`, now, now)
	raw.Exec(`INSERT INTO messages (session_id,msg_index,role,content) VALUES ('s',0,'user','the nginx configuration file')`)
	raw.Exec(`INSERT INTO messages_fts(rowid,content) VALUES (1, 'the nginx configuration file')`)
	raw.Close()

	// Matching JSON so the rebuild path can reseed.
	json := `{"id":"s","title":"t","created_at":"` + now + `","updated_at":"` + now +
		`","schema_version":1,"messages":[{"role":"user","content":"the nginx configuration file"}]}`
	if err := writeFile(dir+"/s.json", json); err != nil {
		t.Fatal(err)
	}

	// Reopen via NewStore — real upgrade flow.
	s := NewStore(dir)
	defer s.Close()

	// `figur` is a mid-word substring that only a trigram index matches.
	res, err := s.Search("figur", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) == 0 {
		t.Error("upgrade from main (user_version=0, porter FTS) did not migrate to trigram — substring query failed")
	}
}

// TestTrigram_OperatorQueryWithShortCJK: boolean operators combined with
// short CJK terms cannot be faithfully expressed via LIKE intersection, and
// silently degrading to trigram MATCH drops the short term. We reject the
// query instead of returning wrong results.
func TestTrigram_OperatorQueryWithShortCJK(t *testing.T) {
	dir := t.TempDir()
	idx, err := OpenIndex(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	now := time.Now().Truncate(time.Second)
	if err := idx.UpsertSession(&Session{
		ID: "s", Title: "t", CreatedAt: now, UpdatedAt: now,
		Messages: []client.Message{{Role: "user", Content: client.NewTextContent("登录接口 failed on port 8080")}},
	}); err != nil {
		t.Fatal(err)
	}
	_, err = idx.Search("登录 AND failed", 5)
	if err == nil {
		t.Error("expected error for operator query with short CJK term, got nil")
	}
}

// TestLikeSnippet_TurkishCaseExpansion guards against the latent bug where
// strings.ToLower("İ") becomes "i\u0307" (2 → 3 bytes), which would make the
// byte-offset-based snippet machinery slice mid-rune on surrounding text.
func TestLikeSnippet_TurkishCaseExpansion(t *testing.T) {
	// "İ" appears before the match; naive byte-offset code could mis-slice.
	content := "Proje İstanbul 登录 sonuç"
	snip := likeSnippet(content, []string{"登录"})
	if !strings.Contains(snip, ">>>登录<<<") {
		t.Errorf("expected >>>登录<<< highlight, got %q", snip)
	}
}

// TestLikeSnippet_EarliestMatch centres the snippet on whichever term matches
// earliest so multi-term queries surface the relevant context instead of
// always anchoring on the first term.
func TestLikeSnippet_EarliestMatch(t *testing.T) {
	content := "failed at startup before 登录 was ever tried"
	// Query terms: 登录 (later) and failed (earlier). Snippet should centre
	// on 'failed' since it appears first in content.
	snip := likeSnippet(content, []string{"登录", "failed"})
	if !strings.Contains(snip, ">>>failed<<<") {
		t.Errorf("expected snippet centred on earliest term 'failed', got %q", snip)
	}
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
