package tools

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// FilePreviewBridge serves local files over a short-lived loopback HTTP
// endpoint so browser-automation tools (notably Playwright) can preview
// them even when file:// is on their protocol deny-list.
//
// Design invariants:
//   - Bound to 127.0.0.1 only. Never publicly reachable.
//   - Random 16-byte hex token per file; URL path is /<token>/<name>.
//   - Each entry scopes exactly one file path — no directory listings, no
//     glob access, no traversal. Requests for unknown tokens → 404.
//   - Lazy server start on first registration; idempotent for the same
//     file path. Server is torn down via Close(), wired to session close.
//
// The model never sees the file:// URL after interception; the rewritten
// http://127.0.0.1:<port>/<token>/<name> URL is opaque to it, preventing
// the model from constructing unauthorized paths.
type FilePreviewBridge struct {
	mu       sync.Mutex
	srv      *http.Server
	listener net.Listener
	port     int
	// tokens maps token → absolute file path served under /<token>/<name>.
	tokens map[string]string
	// byPath lets us reuse the same token for the same file across
	// repeated rewrites of the same file:// URL in one session.
	byPath map[string]string
	closed bool
}

// NewFilePreviewBridge creates an unstarted bridge.
func NewFilePreviewBridge() *FilePreviewBridge {
	return &FilePreviewBridge{
		tokens: make(map[string]string),
		byPath: make(map[string]string),
	}
}

// RewriteFileURL takes a file:// URL, registers its target on the bridge
// (starting the HTTP server on first call), and returns the rewritten
// http://127.0.0.1:<port>/<token>/<name> URL. Percent-decodes UTF-8
// paths (so file:///path/with%20space or non-ASCII paths work).
//
// Returns an error for: non-file scheme, empty path, path that cannot be
// resolved to a regular file, or listener startup failure. The original
// URL should be left intact on error so the downstream MCP call surfaces
// the original "file:// blocked" error as before.
func (b *FilePreviewBridge) RewriteFileURL(fileURL string) (string, error) {
	if b == nil {
		return "", errors.New("file preview bridge not configured")
	}
	u, err := url.Parse(fileURL)
	if err != nil {
		return "", fmt.Errorf("parse file URL: %w", err)
	}
	if u.Scheme != "file" {
		return "", fmt.Errorf("not a file URL: %s", u.Scheme)
	}
	// file:///abs/path → u.Path is /abs/path after parsing. url.QueryUnescape
	// over u.Path handles %-encoded UTF-8 segments.
	decoded, err := url.QueryUnescape(u.Path)
	if err != nil {
		decoded = u.Path
	}
	abs, err := filepath.Abs(decoded)
	if err != nil {
		return "", fmt.Errorf("resolve file path: %w", err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("stat %s: %w", abs, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("not a regular file: %s", abs)
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return "", errors.New("file preview bridge closed")
	}

	// Reuse the existing token for this path in the same session.
	if token, ok := b.byPath[abs]; ok {
		return b.urlFor(token, abs), nil
	}

	// Lazy server start on first registration.
	if b.srv == nil {
		if err := b.startLocked(); err != nil {
			return "", fmt.Errorf("start preview server: %w", err)
		}
	}

	token, err := randomToken()
	if err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	b.tokens[token] = abs
	b.byPath[abs] = token
	return b.urlFor(token, abs), nil
}

// urlFor builds the public URL. Caller must hold b.mu.
func (b *FilePreviewBridge) urlFor(token, abs string) string {
	name := filepath.Base(abs)
	return fmt.Sprintf("http://127.0.0.1:%d/%s/%s", b.port, token, url.PathEscape(name))
}

// startLocked boots the loopback HTTP server. Caller must hold b.mu.
func (b *FilePreviewBridge) startLocked() error {
	ln, err := net.Listen("tcp", "127.0.0.1:0") // random port, loopback only
	if err != nil {
		return err
	}
	b.listener = ln
	b.port = ln.Addr().(*net.TCPAddr).Port

	mux := http.NewServeMux()
	mux.HandleFunc("/", b.serveToken)
	b.srv = &http.Server{
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 60 * time.Second,
	}
	go func() {
		_ = b.srv.Serve(ln) // returns http.ErrServerClosed on Close()
	}()
	return nil
}

// serveToken handles /<token>/<name>. Only exact registered tokens are
// served; no directory listing, no fallback, no traversal.
func (b *FilePreviewBridge) serveToken(w http.ResponseWriter, r *http.Request) {
	// Defense-in-depth: although the listener is bound to 127.0.0.1, reject
	// requests whose remote addr is not loopback. (Corp proxies, WSL port
	// forwarders, etc. can sometimes cross this boundary.)
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil || !isLoopbackHost(host) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	// Expect /<token>/<name>. Anything else → 404.
	parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/"), "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		http.NotFound(w, r)
		return
	}
	token := parts[0]

	b.mu.Lock()
	abs, ok := b.tokens[token]
	closed := b.closed
	b.mu.Unlock()
	if !ok || closed {
		http.NotFound(w, r)
		return
	}

	// Pin to the exact file — ignore the name segment for disk access.
	// The name is only in the URL so browsers pick a sensible download
	// name and so relative-link heuristics inside the page see the right
	// basename.
	http.ServeFile(w, r, abs)
}

// Close tears down the HTTP server and clears the token map. Safe to
// call multiple times. Intended to be wired to session close.
func (b *FilePreviewBridge) Close() error {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil
	}
	b.closed = true
	b.tokens = nil
	b.byPath = nil
	if b.srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		return b.srv.Shutdown(ctx)
	}
	return nil
}

// Active reports whether the bridge has a running server. Used by tests
// to distinguish "never started" (no file:// ever passed through) from
// "started then closed".
func (b *FilePreviewBridge) Active() bool {
	if b == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.srv != nil && !b.closed
}

func randomToken() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func isLoopbackHost(host string) bool {
	if host == "127.0.0.1" || host == "::1" || host == "localhost" {
		return true
	}
	// Parse IP for edge cases like "::ffff:127.0.0.1".
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return true
	}
	return false
}

// ---- ctx plumbing ----

type filePreviewCtxKey struct{}

// WithFilePreview attaches a bridge to ctx so MCPTool (and any other
// file://-capable interceptor) can locate it per-run without global state.
func WithFilePreview(ctx context.Context, b *FilePreviewBridge) context.Context {
	if b == nil {
		return ctx
	}
	return context.WithValue(ctx, filePreviewCtxKey{}, b)
}

// FilePreviewFrom retrieves the per-run bridge, or nil if none attached.
func FilePreviewFrom(ctx context.Context) *FilePreviewBridge {
	if b, ok := ctx.Value(filePreviewCtxKey{}).(*FilePreviewBridge); ok {
		return b
	}
	return nil
}
