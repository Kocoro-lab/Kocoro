package tools

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeTempFile(t *testing.T, name, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		t.Fatalf("write temp: %v", err)
	}
	return path
}

func fetch(t *testing.T, u string) (*http.Response, string) {
	t.Helper()
	resp, err := http.Get(u)
	if err != nil {
		t.Fatalf("GET %s: %v", u, err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(body)
}

func TestFilePreviewBridge_RewriteAndServe(t *testing.T) {
	path := writeTempFile(t, "hello.html", "<h1>hi</h1>")
	b := NewFilePreviewBridge()
	b.AllowFile(path)
	t.Cleanup(func() { _ = b.Close() })

	rewritten, err := b.RewriteFileURL("file://" + path)
	if err != nil {
		t.Fatalf("RewriteFileURL: %v", err)
	}
	if !strings.HasPrefix(rewritten, "http://127.0.0.1:") {
		t.Fatalf("expected loopback URL, got %q", rewritten)
	}
	u, _ := url.Parse(rewritten)
	if u.Host == "" || u.Path == "" {
		t.Fatalf("bad URL: %q", rewritten)
	}

	resp, body := fetch(t, rewritten)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if body != "<h1>hi</h1>" {
		t.Fatalf("body mismatch: %q", body)
	}
}

func TestFilePreviewBridge_ReusesTokenForSamePath(t *testing.T) {
	path := writeTempFile(t, "x.txt", "ok")
	b := NewFilePreviewBridge()
	b.AllowFile(path)
	t.Cleanup(func() { _ = b.Close() })

	u1, err := b.RewriteFileURL("file://" + path)
	if err != nil {
		t.Fatalf("rewrite 1: %v", err)
	}
	u2, err := b.RewriteFileURL("file://" + path)
	if err != nil {
		t.Fatalf("rewrite 2: %v", err)
	}
	if u1 != u2 {
		t.Fatalf("token should be stable per-path, got %q vs %q", u1, u2)
	}
}

func TestFilePreviewBridge_DistinctTokensPerFile(t *testing.T) {
	p1 := writeTempFile(t, "a.txt", "A")
	p2 := writeTempFile(t, "b.txt", "B")
	b := NewFilePreviewBridge()
	b.AllowFile(p1)
	b.AllowFile(p2)
	t.Cleanup(func() { _ = b.Close() })
	u1, err := b.RewriteFileURL("file://" + p1)
	if err != nil {
		t.Fatalf("rewrite 1: %v", err)
	}
	u2, err := b.RewriteFileURL("file://" + p2)
	if err != nil {
		t.Fatalf("rewrite 2: %v", err)
	}
	if u1 == u2 {
		t.Fatalf("tokens must differ for different files, got %q", u1)
	}
	// Each URL must return its own file.
	if _, body := fetch(t, u1); body != "A" {
		t.Fatalf("u1 body: %q", body)
	}
	if _, body := fetch(t, u2); body != "B" {
		t.Fatalf("u2 body: %q", body)
	}
}

func TestFilePreviewBridge_UnknownToken404(t *testing.T) {
	path := writeTempFile(t, "x.txt", "ok")
	b := NewFilePreviewBridge()
	b.AllowFile(path)
	t.Cleanup(func() { _ = b.Close() })

	u, err := b.RewriteFileURL("file://" + path)
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	parsed, _ := url.Parse(u)
	bogus := "http://" + parsed.Host + "/deadbeef/whatever"
	resp, _ := fetch(t, bogus)
	if resp.StatusCode != 404 {
		t.Fatalf("unknown token should 404, got %d", resp.StatusCode)
	}
}

func TestFilePreviewBridge_NoDirectoryListing(t *testing.T) {
	// Register a file so the server starts, then probe the root.
	path := writeTempFile(t, "x.txt", "ok")
	b := NewFilePreviewBridge()
	b.AllowFile(path)
	t.Cleanup(func() { _ = b.Close() })
	u, err := b.RewriteFileURL("file://" + path)
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	parsed, _ := url.Parse(u)
	resp, _ := fetch(t, "http://"+parsed.Host+"/")
	if resp.StatusCode == 200 {
		t.Fatalf("root should not list — got 200")
	}
}

func TestFilePreviewBridge_TraversalRefused(t *testing.T) {
	path := writeTempFile(t, "x.txt", "ok")
	b := NewFilePreviewBridge()
	b.AllowFile(path)
	t.Cleanup(func() { _ = b.Close() })

	u, err := b.RewriteFileURL("file://" + path)
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	parsed, _ := url.Parse(u)
	// Known token but with a traversal name segment — we ignore the name
	// for disk access (pin to registered abs path), so it still returns
	// the original file. The critical invariant is that we do NOT leak
	// any other file.
	segments := strings.SplitN(strings.TrimPrefix(parsed.Path, "/"), "/", 2)
	if len(segments) < 1 {
		t.Fatal("parsed URL lacks token segment")
	}
	token := segments[0]
	bad := "http://" + parsed.Host + "/" + token + "/../../../etc/passwd"
	resp, body := fetch(t, bad)
	// Go's http.ServeMux normalizes the path, so we typically see the
	// same served file. What matters: no /etc/passwd content, no 200
	// for a non-registered path. We accept either: pinned file bytes,
	// OR a 404 — we just never want 200 leaking foreign content.
	if resp.StatusCode == 200 && strings.Contains(body, "root:") {
		t.Fatalf("traversal leaked /etc/passwd")
	}
}

func TestFilePreviewBridge_RejectsNonFileScheme(t *testing.T) {
	b := NewFilePreviewBridge()
	t.Cleanup(func() { _ = b.Close() })
	_, err := b.RewriteFileURL("http://example.com/")
	if err == nil {
		t.Fatal("should reject non-file URL")
	}
	if b.Active() {
		t.Fatal("server should not have started for a rejected URL")
	}
}

func TestFilePreviewBridge_RejectsDirectory(t *testing.T) {
	b := NewFilePreviewBridge()
	t.Cleanup(func() { _ = b.Close() })
	dir := t.TempDir()
	_, err := b.RewriteFileURL("file://" + dir)
	if err == nil {
		t.Fatal("should reject directory")
	}
}

func TestFilePreviewBridge_RejectsMissingFile(t *testing.T) {
	b := NewFilePreviewBridge()
	t.Cleanup(func() { _ = b.Close() })
	_, err := b.RewriteFileURL("file:///no/such/path/ever.html")
	if err == nil {
		t.Fatal("should reject missing file")
	}
}

// TestFilePreviewBridge_IndexHTML_ServedNotRedirected is a regression for
// the http.ServeFile behavior where URLs ending in "/index.html" are
// rewritten to "./" via an internal redirect. Because the handler ignores
// the URL name segment for disk access, such a redirect would land on a
// path we do not serve and return 404 — silently breaking preview of any
// file named index.html. Switching to http.ServeContent avoids this.
func TestFilePreviewBridge_IndexHTML_ServedNotRedirected(t *testing.T) {
	path := writeTempFile(t, "index.html", "<h1>hi</h1>")
	b := NewFilePreviewBridge()
	b.AllowFile(path)
	t.Cleanup(func() { _ = b.Close() })

	rewritten, err := b.RewriteFileURL("file://" + path)
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	// Disable redirect following — we want to see the handler's raw response.
	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Timeout: 2 * time.Second,
	}
	resp, err := client.Get(rewritten)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d — index.html redirect regression: %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	if string(body) != "<h1>hi</h1>" {
		t.Fatalf("body: %q", string(body))
	}
}

func TestFilePreviewBridge_PercentEncodedPath(t *testing.T) {
	dir := t.TempDir()
	// Space + non-ASCII.
	name := "hello world 日本.html"
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("ok"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	b := NewFilePreviewBridge()
	b.AllowFile(path)
	t.Cleanup(func() { _ = b.Close() })

	encodedPath := strings.ReplaceAll(path, " ", "%20")
	fileURL := "file://" + encodedPath
	rewritten, err := b.RewriteFileURL(fileURL)
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	resp, body := fetch(t, rewritten)
	if resp.StatusCode != 200 || body != "ok" {
		t.Fatalf("status=%d body=%q", resp.StatusCode, body)
	}
}

func TestFilePreviewBridge_CloseTearsDownServer(t *testing.T) {
	path := writeTempFile(t, "x.txt", "ok")
	b := NewFilePreviewBridge()
	b.AllowFile(path)
	u, err := b.RewriteFileURL("file://" + path)
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if !b.Active() {
		t.Fatal("bridge should be active after registration")
	}

	if err := b.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if b.Active() {
		t.Fatal("bridge should not be active after Close")
	}

	// Subsequent fetches must fail (connection refused or similar).
	client := &http.Client{Timeout: 500 * time.Millisecond}
	_, err = client.Get(u)
	if err == nil {
		t.Fatal("expected request to fail after Close")
	}

	// Close is idempotent.
	if err := b.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestFilePreviewBridge_CloseBeforeStart_NoOp(t *testing.T) {
	b := NewFilePreviewBridge()
	// Never registered a file → never started. Close must be safe.
	if err := b.Close(); err != nil {
		t.Fatalf("Close on unstarted bridge: %v", err)
	}
}

func TestFilePreview_CtxPlumbing(t *testing.T) {
	b := NewFilePreviewBridge()
	t.Cleanup(func() { _ = b.Close() })

	ctx := context.Background()
	if got := FilePreviewFrom(ctx); got != nil {
		t.Fatal("bare ctx should have no bridge")
	}
	ctx = WithFilePreview(ctx, b)
	if got := FilePreviewFrom(ctx); got != b {
		t.Fatal("ctx should carry the bridge")
	}
	// nil bridge → ctx unchanged.
	same := WithFilePreview(context.Background(), nil)
	if FilePreviewFrom(same) != nil {
		t.Fatal("WithFilePreview(nil) should not install a bridge")
	}
}

func TestMaybeRewriteFileURL_Intercepts(t *testing.T) {
	path := writeTempFile(t, "page.html", "<html></html>")
	b := NewFilePreviewBridge()
	b.AllowFile(path)
	t.Cleanup(func() { _ = b.Close() })

	ctx := WithFilePreview(context.Background(), b)
	args := map[string]any{"url": "file://" + path}
	got, ok := maybeRewriteFileURL(ctx, args)
	if !ok {
		t.Fatal("expected interception")
	}
	if !strings.HasPrefix(got, "http://127.0.0.1:") {
		t.Fatalf("expected loopback URL, got %q", got)
	}
}

func TestMaybeRewriteFileURL_NonFileURL_Skipped(t *testing.T) {
	b := NewFilePreviewBridge()
	t.Cleanup(func() { _ = b.Close() })
	ctx := WithFilePreview(context.Background(), b)

	_, ok := maybeRewriteFileURL(ctx, map[string]any{"url": "https://example.com"})
	if ok {
		t.Fatal("http URLs must not be intercepted")
	}
	if b.Active() {
		t.Fatal("bridge must not start for http URLs")
	}
}

func TestMaybeRewriteFileURL_NoBridge_Skipped(t *testing.T) {
	// bare ctx, no bridge.
	_, ok := maybeRewriteFileURL(context.Background(),
		map[string]any{"url": "file:///tmp/x.html"})
	if ok {
		t.Fatal("without a bridge on ctx, no rewrite should occur")
	}
}

// === Allowlist regressions (security-critical) =========================

func TestFilePreviewBridge_RejectsPathOutsideAllowlist(t *testing.T) {
	// Bridge with NO allowlist entries → fail-closed on everything.
	b := NewFilePreviewBridge()
	t.Cleanup(func() { _ = b.Close() })
	path := writeTempFile(t, "x.txt", "ok")
	_, err := b.RewriteFileURL("file://" + path)
	if err == nil {
		t.Fatal("empty allowlist must reject all files")
	}
	if !strings.Contains(err.Error(), "allowlist") {
		t.Fatalf("expected allowlist error, got: %v", err)
	}
	// Confirm server never started — no listener leak on rejected requests.
	if b.Active() {
		t.Fatal("allowlist rejection must not start the HTTP server")
	}
}

func TestFilePreviewBridge_AllowRoot_PermitsSubtree(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "sub")
	if err := os.MkdirAll(sub, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	deep := filepath.Join(sub, "deep.html")
	if err := os.WriteFile(deep, []byte("ok"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	b := NewFilePreviewBridge()
	b.AllowRoot(dir) // only the top dir; subtree is implicit
	t.Cleanup(func() { _ = b.Close() })

	if _, err := b.RewriteFileURL("file://" + deep); err != nil {
		t.Fatalf("subtree file must be allowed: %v", err)
	}
}

func TestFilePreviewBridge_AllowRoot_RejectsOutsideSubtree(t *testing.T) {
	allowed := t.TempDir()
	forbidden := writeTempFile(t, "secret.txt", "never") // different temp dir

	b := NewFilePreviewBridge()
	b.AllowRoot(allowed)
	t.Cleanup(func() { _ = b.Close() })

	_, err := b.RewriteFileURL("file://" + forbidden)
	if err == nil {
		t.Fatal("file outside allowlist must be rejected")
	}
}

func TestFilePreviewBridge_AllowRoot_DoesNotMatchSiblingPrefix(t *testing.T) {
	// Regression for the classic "/foo" matching "/foobar" bug. Must use
	// a path separator boundary, not a raw string prefix.
	base := t.TempDir()
	allowed := filepath.Join(base, "proj")
	sibling := filepath.Join(base, "proj-leak")
	if err := os.MkdirAll(allowed, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(sibling, 0755); err != nil {
		t.Fatal(err)
	}
	siblingFile := filepath.Join(sibling, "x.html")
	if err := os.WriteFile(siblingFile, []byte("leak"), 0644); err != nil {
		t.Fatal(err)
	}

	b := NewFilePreviewBridge()
	b.AllowRoot(allowed)
	t.Cleanup(func() { _ = b.Close() })

	if _, err := b.RewriteFileURL("file://" + siblingFile); err == nil {
		t.Fatal("sibling dir with shared prefix must NOT match allowlist root")
	}
}

func TestFilePreviewBridge_AllowFile_ExactOnly(t *testing.T) {
	allowed := writeTempFile(t, "ok.html", "yes")
	// Different file in a different temp dir — parent not whitelisted.
	neighbor := writeTempFile(t, "other.html", "no")

	b := NewFilePreviewBridge()
	b.AllowFile(allowed)
	t.Cleanup(func() { _ = b.Close() })

	if _, err := b.RewriteFileURL("file://" + allowed); err != nil {
		t.Fatalf("allowed file must pass: %v", err)
	}
	if _, err := b.RewriteFileURL("file://" + neighbor); err == nil {
		t.Fatal("neighbor file must be rejected when only exact file is allowed")
	}
}

// TestFilePreviewBridge_Symlink_EscapeFromAllowedRoot is the
// security-critical regression for the "allowlist is lexical, symlinks
// still escape" finding. A symlink placed INSIDE an allowed root that
// points to a target OUTSIDE the root must be rejected — the bridge
// compares symlink-resolved real paths on both sides, matching the
// file-access contract of permissions.CheckFilePath.
func TestFilePreviewBridge_Symlink_EscapeFromAllowedRoot(t *testing.T) {
	// Forbidden area.
	outsideDir := t.TempDir()
	secret := filepath.Join(outsideDir, "secret.txt")
	if err := os.WriteFile(secret, []byte("TOP SECRET"), 0644); err != nil {
		t.Fatalf("write secret: %v", err)
	}

	// Allowed area contains a symlink pointing to the secret.
	allowedDir := t.TempDir()
	trojan := filepath.Join(allowedDir, "looks-innocent.txt")
	if err := os.Symlink(secret, trojan); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	b := NewFilePreviewBridge()
	b.AllowRoot(allowedDir)
	t.Cleanup(func() { _ = b.Close() })

	_, err := b.RewriteFileURL("file://" + trojan)
	if err == nil {
		t.Fatal("symlink escape via allowed root must be rejected")
	}
	if !strings.Contains(err.Error(), "allowlist") {
		t.Fatalf("expected allowlist rejection, got: %v", err)
	}
}

// TestFilePreviewBridge_Symlink_InsideAllowedRoot confirms the converse:
// a symlink whose target stays inside the allowed subtree still works
// (realpath on both sides keeps intra-subtree symlinks usable).
func TestFilePreviewBridge_Symlink_InsideAllowedRoot(t *testing.T) {
	allowedDir := t.TempDir()
	target := filepath.Join(allowedDir, "real.html")
	if err := os.WriteFile(target, []byte("<h1>ok</h1>"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	link := filepath.Join(allowedDir, "alias.html")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	b := NewFilePreviewBridge()
	b.AllowRoot(allowedDir)
	t.Cleanup(func() { _ = b.Close() })

	if _, err := b.RewriteFileURL("file://" + link); err != nil {
		t.Fatalf("intra-subtree symlink should be allowed: %v", err)
	}
}

// TestFilePreviewBridge_AllowRoot_SymlinkedRoot_ResolvesCorrectly: if the
// configured root is itself a symlink, we resolve it on registration so
// the allowlist matches real-paths consistently on both sides.
func TestFilePreviewBridge_AllowRoot_SymlinkedRoot_ResolvesCorrectly(t *testing.T) {
	realBase := t.TempDir()
	realRoot := filepath.Join(realBase, "real")
	if err := os.MkdirAll(realRoot, 0755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(realRoot, "x.html")
	if err := os.WriteFile(file, []byte("ok"), 0644); err != nil {
		t.Fatal(err)
	}

	linkRoot := filepath.Join(t.TempDir(), "linked-root")
	if err := os.Symlink(realRoot, linkRoot); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	b := NewFilePreviewBridge()
	b.AllowRoot(linkRoot) // configured via symlink
	t.Cleanup(func() { _ = b.Close() })

	// Requesting the file via its real path must succeed (same realpath
	// on both sides).
	if _, err := b.RewriteFileURL("file://" + file); err != nil {
		t.Fatalf("file reached via real path should be allowed when root is a symlink: %v", err)
	}
}

func TestFilePreviewBridge_AllowRoot_IgnoresInvalidPaths(t *testing.T) {
	b := NewFilePreviewBridge()
	t.Cleanup(func() { _ = b.Close() })

	// Non-existent, non-directory, empty — each should be a silent no-op.
	b.AllowRoot("")
	b.AllowRoot("/definitely/not/a/real/path/ever")
	b.AllowRoot(writeTempFile(t, "not-a-dir.txt", "x")) // passed a file, not dir

	// Bridge is still fail-closed.
	path := writeTempFile(t, "x.txt", "ok")
	if _, err := b.RewriteFileURL("file://" + path); err == nil {
		t.Fatal("bad AllowRoot inputs must not widen allowlist")
	}
}

func TestMaybeRewriteFileURL_PreservesOriginalOnError(t *testing.T) {
	b := NewFilePreviewBridge()
	t.Cleanup(func() { _ = b.Close() })
	ctx := WithFilePreview(context.Background(), b)
	// Nonexistent file → rewrite fails → original preserved (ok=false).
	args := map[string]any{"url": "file:///absolutely/not/there.html"}
	if _, ok := maybeRewriteFileURL(ctx, args); ok {
		t.Fatal("rewrite must fail for missing file")
	}
	if args["url"] != "file:///absolutely/not/there.html" {
		t.Fatal("args[url] must be unchanged on rewrite failure")
	}
}
