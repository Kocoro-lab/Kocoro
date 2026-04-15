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
//   - ALLOWLISTED paths only. RewriteFileURL rejects paths outside an
//     explicitly configured set of allowed roots or allowed files. A
//     bridge with no allowlist rejects everything — fail-closed default.
//     This matches the file-access boundary of other agent tools: the
//     model can't escalate its reach by routing through the browser.
//   - Random 16-byte hex token per file; URL path is /<token>/<name>.
//     Requests for unknown tokens → 404.
//   - Lazy server start on first registration; idempotent for the same
//     file path. Server is torn down via Close(), wired to session close.
//
// The model never sees the file:// URL after interception; the rewritten
// http://127.0.0.1:<port>/<token>/<name> URL is opaque to it, preventing
// the model from constructing unauthorized paths. Combined with the
// allowlist, this means the worst a compromised/misused browser_navigate
// call can do is re-read a file the agent was already authorized to
// access via normal tools.
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

	// Allowlist. A path is accepted for rewrite if it is either:
	//   - an exact match of an entry in allowedFiles (cleaned abs path), OR
	//   - under (or equal to) an entry in allowedRoots.
	// Empty allowlist → everything is rejected (fail-closed).
	allowedRoots []string // cleaned absolute directory paths
	allowedFiles map[string]bool
}

// NewFilePreviewBridge creates an unstarted bridge with an empty
// allowlist. Callers MUST configure it via AllowRoot / AllowFile before
// any rewrite will succeed. The daemon runner does this per-session from
// sessionCWD + user-attached paths.
func NewFilePreviewBridge() *FilePreviewBridge {
	return &FilePreviewBridge{
		tokens:       make(map[string]string),
		byPath:       make(map[string]string),
		allowedFiles: make(map[string]bool),
	}
}

// resolveReal returns the absolute, symlink-resolved form of path. If the
// path or any intermediate component cannot be resolved (missing file,
// platform edge case), it falls back to the lexical filepath.Clean(abs).
// Matches the best-effort realpath pattern used in permissions.CheckFilePath.
func resolveReal(path string) (string, bool) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", false
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		return filepath.Clean(real), true
	}
	return filepath.Clean(abs), true
}

// AllowRoot whitelists a directory subtree. All regular files at or below
// this directory (after symlink resolution on BOTH sides) become eligible
// for rewrite. Non-absolute or unresolvable paths are silently ignored
// (defense-in-depth — never widen the set on bad input).
func (b *FilePreviewBridge) AllowRoot(dir string) {
	if b == nil || dir == "" {
		return
	}
	real, ok := resolveReal(dir)
	if !ok {
		return
	}
	info, err := os.Stat(real)
	if err != nil || !info.IsDir() {
		return
	}
	b.mu.Lock()
	b.allowedRoots = append(b.allowedRoots, real)
	b.mu.Unlock()
}

// AllowFile whitelists an exact file path (after symlink resolution).
func (b *FilePreviewBridge) AllowFile(path string) {
	if b == nil || path == "" {
		return
	}
	real, ok := resolveReal(path)
	if !ok {
		return
	}
	b.mu.Lock()
	b.allowedFiles[real] = true
	b.mu.Unlock()
}

// isAllowedLocked checks whether abs is within the allowlist. Caller must
// hold b.mu.
func (b *FilePreviewBridge) isAllowedLocked(abs string) bool {
	cleaned := filepath.Clean(abs)
	if b.allowedFiles[cleaned] {
		return true
	}
	for _, root := range b.allowedRoots {
		// Proper prefix check: abs must be root itself or under it.
		if cleaned == root {
			return true
		}
		rootWithSep := root + string(filepath.Separator)
		if strings.HasPrefix(cleaned, rootWithSep) {
			return true
		}
	}
	return false
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
	// Resolve symlinks: a link inside an allowed root that points OUTSIDE
	// the root must be rejected. We compare real paths on both sides.
	// EvalSymlinks requires the target to exist, which is also our
	// regular-file check below.
	abs, err := filepath.Abs(decoded)
	if err != nil {
		return "", fmt.Errorf("resolve file path: %w", err)
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("resolve symlinks for %s: %w", abs, err)
	}
	real = filepath.Clean(real)
	info, err := os.Stat(real)
	if err != nil {
		return "", fmt.Errorf("stat %s: %w", real, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("not a regular file: %s", real)
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return "", errors.New("file preview bridge closed")
	}

	// Allowlist enforcement against the REAL path, so symlinks cannot
	// escape the allowed subtree. The model must not gain broader local-
	// file reach through the browser than the normal filesystem tools.
	if !b.isAllowedLocked(real) {
		return "", fmt.Errorf("file not in preview allowlist: %s", real)
	}

	// Reuse the existing token for this real path in the same session.
	if token, ok := b.byPath[real]; ok {
		return b.urlFor(token, real), nil
	}

	// Lazy server start on first registration. Bind outside the lock —
	// net.Listen is a blocking syscall we don't want holding b.mu while
	// AllowRoot/AllowFile callers wait on the mutex.
	if b.srv == nil {
		b.mu.Unlock()
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		b.mu.Lock()
		if err != nil {
			return "", fmt.Errorf("start preview server: %w", err)
		}
		// Another goroutine may have started the server while we had the
		// lock released. If so, close our spare listener and reuse theirs.
		if b.srv != nil {
			_ = ln.Close()
		} else if b.closed {
			_ = ln.Close()
			return "", errors.New("file preview bridge closed")
		} else {
			b.assignServerLocked(ln)
		}
	}

	token, err := randomToken()
	if err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	b.tokens[token] = real
	b.byPath[real] = token
	return b.urlFor(token, real), nil
}

// urlFor builds the public URL. Caller must hold b.mu.
func (b *FilePreviewBridge) urlFor(token, abs string) string {
	name := filepath.Base(abs)
	return fmt.Sprintf("http://127.0.0.1:%d/%s/%s", b.port, token, url.PathEscape(name))
}

// assignServerLocked wires an already-bound listener into the bridge's
// state and starts the serve goroutine. Caller must hold b.mu and have
// confirmed b.srv == nil and !b.closed.
func (b *FilePreviewBridge) assignServerLocked(ln net.Listener) {
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
	//
	// We deliberately use http.ServeContent (not http.ServeFile). ServeFile
	// issues a built-in redirect for URL paths ending in "/index.html" →
	// "./"; because our handler ignores the URL name segment for disk
	// access, that redirect would land on a path we do not serve and
	// return 404, silently breaking preview of any file named index.html.
	// ServeContent takes an already-open ReadSeeker and has no such
	// path-based redirect, so it respects the real file on disk.
	f, err := os.Open(abs)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		http.NotFound(w, r)
		return
	}
	http.ServeContent(w, r, filepath.Base(abs), info.ModTime(), f)
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
	// IP-based check only — deliberately does NOT special-case the literal
	// string "localhost". An adversarial /etc/hosts or similar resolver
	// manipulation could route "localhost" to a non-loopback address; in
	// practice net.(*TCPListener).RemoteAddr always gives us a numeric
	// address, so the IP parse path below covers every real case
	// (including the IPv4-mapped form "::ffff:127.0.0.1").
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
