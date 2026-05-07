package tools

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/cwdctx"
	"github.com/Kocoro-lab/ShanClaw/internal/uploads"
)

// fakeUploader records the most recent call and returns canned responses.
// Tests that expect the upload to be blocked by a guard set Called=true to
// verify the uploader was NEVER reached.
type fakeUploader struct {
	called    bool
	filename  string
	mimeType  string
	bodyBytes []byte
	resp      *uploads.UploadResponse
	err       error
}

func (f *fakeUploader) Upload(ctx context.Context, filename, contentType string,
	openBody func() (io.ReadCloser, error)) (*uploads.UploadResponse, error) {
	f.called = true
	f.filename = filename
	f.mimeType = contentType
	if openBody != nil {
		rc, err := openBody()
		if err == nil {
			f.bodyBytes, _ = io.ReadAll(rc)
			rc.Close()
		}
	}
	return f.resp, f.err
}

// runTool sets a session CWD on ctx so ResolveFilesystemPath accepts relative
// paths, then invokes the tool. Returns the ToolResult for assertions.
func runTool(t *testing.T, tool *PublishToWebTool, args map[string]any, sessionCWD string) agent.ToolResult {
	t.Helper()
	argsJSON, err := jsonMarshal(args)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	ctx := context.Background()
	if sessionCWD != "" {
		ctx = cwdctx.WithSessionCWD(ctx, sessionCWD)
	}
	res, err := tool.Run(ctx, argsJSON)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	return res
}

func jsonMarshal(v any) (string, error) {
	b, err := json.Marshal(v)
	return string(b), err
}

func TestPublishMissingPath(t *testing.T) {
	fu := &fakeUploader{}
	tool := NewPublishToWebTool(fu, nil)
	res := runTool(t, tool, map[string]any{
		"path":    "",
		"purpose": "send chart png to user via slack",
	}, "")
	if !res.IsError || res.ErrorCategory != agent.ErrCategoryValidation {
		t.Fatalf("expected validation error, got %+v", res)
	}
	if fu.called {
		t.Fatalf("uploader must not be called when path is missing")
	}
}

func TestPublishMissingPurpose(t *testing.T) {
	fu := &fakeUploader{}
	tool := NewPublishToWebTool(fu, nil)
	res := runTool(t, tool, map[string]any{
		"path":    "/tmp/x.html",
		"purpose": "",
	}, "")
	if !res.IsError || res.ErrorCategory != agent.ErrCategoryValidation {
		t.Fatalf("expected validation error, got %+v", res)
	}
}

func TestPublishPurposeTooShort(t *testing.T) {
	fu := &fakeUploader{}
	tool := NewPublishToWebTool(fu, nil)
	res := runTool(t, tool, map[string]any{
		"path":    "/tmp/x.html",
		"purpose": "x",
	}, "")
	if !res.IsError || res.ErrorCategory != agent.ErrCategoryValidation {
		t.Fatalf("expected validation error, got %+v", res)
	}
}

func TestPublishPurposeVaguePlaceholder(t *testing.T) {
	fu := &fakeUploader{}
	tool := NewPublishToWebTool(fu, nil)
	for _, vague := range []string{"share", "test", "todo", "asdf", "  Send It  ", "for testing", "share   with team", "send to user"} {
		res := runTool(t, tool, map[string]any{
			"path":    "/tmp/x.html",
			"purpose": vague,
		}, "")
		if !res.IsError || res.ErrorCategory != agent.ErrCategoryValidation {
			t.Errorf("purpose %q: expected validation error, got %+v", vague, res)
		}
	}
}

func TestPublishPathBlacklistComponent(t *testing.T) {
	for _, p := range []string{
		"/Users/me/.ssh/id_rsa.html", // blocked by both segment and id_rsa segment
		"/Users/me/.aws/config.txt",
		"/var/log/credentials/list.csv",
		"/srv/secrets/vault.md",
		"/work/.env",
	} {
		t.Run(p, func(t *testing.T) {
			fu := &fakeUploader{}
			tool := NewPublishToWebTool(fu, nil)
			res := runTool(t, tool, map[string]any{
				"path":    p,
				"purpose": "send to user via slack reply",
			}, "")
			if !res.IsError || res.ErrorCategory != agent.ErrCategoryBusiness {
				t.Fatalf("expected business error, got %+v", res)
			}
			if fu.called {
				t.Fatalf("uploader must not be called for blocked path %q", p)
			}
		})
	}
}

func TestPublishPathBlacklistSuffix(t *testing.T) {
	for _, p := range []string{
		"/tmp/cert.pem",
		"/tmp/server.key",
		"/tmp/store.p12",
		"/tmp/sig.asc",
	} {
		t.Run(p, func(t *testing.T) {
			fu := &fakeUploader{}
			tool := NewPublishToWebTool(fu, nil)
			res := runTool(t, tool, map[string]any{
				"path":    p,
				"purpose": "send to user via slack reply",
			}, "")
			if !res.IsError || res.ErrorCategory != agent.ErrCategoryBusiness {
				t.Fatalf("expected business error, got %+v", res)
			}
		})
	}
}

func TestPublishSensitiveDisguisedFilenames(t *testing.T) {
	for _, p := range []string{
		"/tmp/id_rsa.key.txt",
		"/tmp/server.key.txt",
		"/tmp/credentials.json",
		"/tmp/.env.local.txt",
	} {
		t.Run(p, func(t *testing.T) {
			fu := &fakeUploader{}
			tool := NewPublishToWebTool(fu, nil)
			res := runTool(t, tool, map[string]any{
				"path":    p,
				"purpose": "send to user via slack reply",
			}, "")
			if !res.IsError || res.ErrorCategory != agent.ErrCategoryBusiness {
				t.Fatalf("expected business error, got %+v", res)
			}
			if fu.called {
				t.Fatalf("uploader must not be called for disguised sensitive path %q", p)
			}
		})
	}
}

func TestPublishExtensionNotInAllowlist(t *testing.T) {
	for _, p := range []string{
		"/tmp/source.go",
		"/tmp/script.py",
		"/tmp/archive.zip",
		"/tmp/binary.exe",
		"/tmp/config.yaml",
		"/tmp/noext",
	} {
		t.Run(p, func(t *testing.T) {
			fu := &fakeUploader{}
			tool := NewPublishToWebTool(fu, nil)
			res := runTool(t, tool, map[string]any{
				"path":    p,
				"purpose": "send to user via slack reply",
			}, "")
			if !res.IsError || res.ErrorCategory != agent.ErrCategoryBusiness {
				t.Fatalf("expected business error, got %+v", res)
			}
			if fu.called {
				t.Fatalf("uploader must not be called for disallowed extension %q", p)
			}
		})
	}
}

func TestPublishConfigExtendsAllowlist(t *testing.T) {
	dir := t.TempDir()
	goFile := filepath.Join(dir, "main.go")
	if err := os.WriteFile(goFile, []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	allow := map[string]bool{".go": true} // only .go, simulate user override
	fu := &fakeUploader{
		resp: &uploads.UploadResponse{
			URL: "https://x/y/main.go", Key: "k", Size: 14, ContentType: "text/x-go",
		},
	}
	tool := NewPublishToWebTool(fu, allow)
	res := runTool(t, tool, map[string]any{
		"path":    goFile,
		"purpose": "share generated example with reviewer via gist",
	}, "")
	if res.IsError {
		t.Fatalf("expected success, got %+v", res)
	}
	if !fu.called {
		t.Fatalf("uploader should have been called when extension is on user allowlist")
	}
}

func TestPublishPathTraversalDeniedByCWD(t *testing.T) {
	fu := &fakeUploader{}
	tool := NewPublishToWebTool(fu, nil)
	// Relative path with no session CWD set → ResolveFilesystemPath returns ErrNoSessionCWD.
	res := runTool(t, tool, map[string]any{
		"path":    "relative/path.html",
		"purpose": "send to user via slack reply",
	}, "")
	if !res.IsError || res.ErrorCategory != agent.ErrCategoryValidation {
		t.Fatalf("expected validation error, got %+v", res)
	}
}

func TestPublishPathIsDirectory(t *testing.T) {
	dir := t.TempDir()
	// Need a .html-ish path to pass the extension check first; use a directory
	// whose name happens to end in an allowed extension.
	pseudo := filepath.Join(dir, "thing.html")
	if err := os.Mkdir(pseudo, 0o755); err != nil {
		t.Fatal(err)
	}
	fu := &fakeUploader{}
	tool := NewPublishToWebTool(fu, nil)
	res := runTool(t, tool, map[string]any{
		"path":    pseudo,
		"purpose": "share with user via slack reply",
	}, "")
	if !res.IsError || res.ErrorCategory != agent.ErrCategoryValidation {
		t.Fatalf("expected validation error, got %+v", res)
	}
	if !strings.Contains(res.Content, "directory") {
		t.Errorf("error message should mention directory: %q", res.Content)
	}
}

func TestPublishFileTooLarge(t *testing.T) {
	dir := t.TempDir()
	bigFile := filepath.Join(dir, "big.html")
	f, err := os.Create(bigFile)
	if err != nil {
		t.Fatal(err)
	}
	// Sparse file: truncate to publishMaxBytes+1 without writing all the bytes.
	if err := f.Truncate(publishMaxBytes + 1); err != nil {
		t.Fatal(err)
	}
	f.Close()

	fu := &fakeUploader{}
	tool := NewPublishToWebTool(fu, nil)
	res := runTool(t, tool, map[string]any{
		"path":    bigFile,
		"purpose": "publish landing page to user via slack reply",
	}, "")
	if !res.IsError || res.ErrorCategory != agent.ErrCategoryValidation {
		t.Fatalf("expected validation error, got %+v", res)
	}
	if fu.called {
		t.Fatalf("uploader must not be called for oversize file")
	}
}

func TestPublishHappyPath(t *testing.T) {
	dir := t.TempDir()
	htmlFile := filepath.Join(dir, "landing.html")
	if err := os.WriteFile(htmlFile, []byte("<h1>hi</h1>"), 0o644); err != nil {
		t.Fatal(err)
	}
	fu := &fakeUploader{
		resp: &uploads.UploadResponse{
			URL: "https://shannon-lp/x/y/landing.html", Key: "x/y/landing.html",
			Size: 11, ContentType: "text/html",
		},
	}
	tool := NewPublishToWebTool(fu, nil)
	res := runTool(t, tool, map[string]any{
		"path":         htmlFile,
		"purpose":      "send landing page draft to user via slack reply",
		"content_type": "text/html",
	}, "")
	if res.IsError {
		t.Fatalf("expected success, got %+v", res)
	}
	if !fu.called {
		t.Fatalf("uploader should have been called")
	}
	if fu.filename != "landing.html" {
		t.Errorf("filename = %q, want landing.html", fu.filename)
	}
	if fu.mimeType != "text/html" {
		t.Errorf("content-type = %q, want text/html", fu.mimeType)
	}
	if string(fu.bodyBytes) != "<h1>hi</h1>" {
		t.Errorf("body = %q", fu.bodyBytes)
	}
	if !strings.Contains(res.Content, "https://shannon-lp/x/y/landing.html") {
		t.Errorf("result should contain URL, got %q", res.Content)
	}
	if !strings.Contains(res.Content, "send landing page draft") {
		t.Errorf("result should echo purpose for audit, got %q", res.Content)
	}
}

func TestPublishUploadErrorClassification(t *testing.T) {
	dir := t.TempDir()
	htmlFile := filepath.Join(dir, "x.html")
	if err := os.WriteFile(htmlFile, []byte("<h1>hi</h1>"), 0o644); err != nil {
		t.Fatal(err)
	}
	subCases := []struct {
		name    string
		wrap    error
		wantCat agent.ErrorCategory
	}{
		{"unauthorized", uploads.ErrUnauthorized, agent.ErrCategoryPermission},
		{"file too large", uploads.ErrFileTooLarge, agent.ErrCategoryValidation},
		{"bad request", uploads.ErrBadRequest, agent.ErrCategoryValidation},
		{"endpoint not found", uploads.ErrEndpointNotFound, agent.ErrCategoryBusiness},
		{"server config", uploads.ErrServerConfig, agent.ErrCategoryBusiness},
		{"transient", uploads.ErrTransient, agent.ErrCategoryTransient},
	}
	for _, tc := range subCases {
		t.Run(tc.name, func(t *testing.T) {
			fu := &fakeUploader{err: wrapErr(tc.wrap)}
			tool := NewPublishToWebTool(fu, nil)
			res := runTool(t, tool, map[string]any{
				"path":    htmlFile,
				"purpose": "send to user via slack reply",
			}, "")
			if !res.IsError || res.ErrorCategory != tc.wantCat {
				t.Fatalf("got cat=%q, want %q (full result: %+v)", res.ErrorCategory, tc.wantCat, res)
			}
		})
	}
}

func wrapErr(sentinel error) error {
	return errorsW{sentinel}
}

type errorsW struct{ sentinel error }

func (e errorsW) Error() string { return "wrapped: " + e.sentinel.Error() }
func (e errorsW) Unwrap() error { return e.sentinel }

func TestPublishRequiresApprovalAndIsSafeArgs(t *testing.T) {
	tool := NewPublishToWebTool(&fakeUploader{}, nil)
	if !tool.RequiresApproval() {
		t.Errorf("RequiresApproval should return true")
	}
	if tool.IsSafeArgs(`{"path":"/tmp/x.html","purpose":"send to user via slack"}`) {
		t.Errorf("IsSafeArgs should always return false")
	}
}

// TestPublishSymlinkBypassRejected guards against a real escape vector: if the
// blocklist only inspects the user-supplied path string, a caller can stash a
// symlink with an innocent-looking name and exfiltrate the target. The tool
// must EvalSymlinks before running blocklist checks.
func TestPublishSymlinkBypassRejected(t *testing.T) {
	dir := t.TempDir()
	// Create a "secret" file under a denied path segment.
	secretsDir := filepath.Join(dir, "secrets")
	if err := os.Mkdir(secretsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	secretFile := filepath.Join(secretsDir, "vault.html")
	if err := os.WriteFile(secretFile, []byte("api_key=sk-...\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Symlink with an innocuous name pointing at the secret. Path-string check
	// alone would let this through; EvalSymlinks should catch it.
	innocent := filepath.Join(dir, "innocent.html")
	if err := os.Symlink(secretFile, innocent); err != nil {
		t.Fatal(err)
	}
	fu := &fakeUploader{}
	tool := NewPublishToWebTool(fu, nil)
	res := runTool(t, tool, map[string]any{
		"path":    innocent,
		"purpose": "send the page to user via slack reply",
	}, "")
	if !res.IsError || res.ErrorCategory != agent.ErrCategoryBusiness {
		t.Fatalf("expected business error from symlink-resolved blocklist, got %+v", res)
	}
	if fu.called {
		t.Fatalf("uploader must not be called when symlink target is blocklisted")
	}
}

// TestPublishSymlinkToAllowedFileWorks confirms that legitimate symlinks
// (target is in an allowed location) still publish correctly. We don't want
// the symlink defense to over-reject.
func TestPublishSymlinkToAllowedFileWorks(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real.html")
	if err := os.WriteFile(target, []byte("<h1>hi</h1>"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "alias.html")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	fu := &fakeUploader{
		resp: &uploads.UploadResponse{
			URL: "https://x/y/real.html", Key: "k", Size: 11, ContentType: "text/html",
		},
	}
	tool := NewPublishToWebTool(fu, nil)
	res := runTool(t, tool, map[string]any{
		"path":    link,
		"purpose": "send html preview to user via slack reply",
	}, "")
	if res.IsError {
		t.Fatalf("expected success, got %+v", res)
	}
	if !fu.called {
		t.Fatalf("uploader should have been called for legitimate symlink")
	}
	if string(fu.bodyBytes) != "<h1>hi</h1>" {
		t.Errorf("body = %q (should follow symlink to real file)", fu.bodyBytes)
	}
}

func TestPublishToolInfoSchema(t *testing.T) {
	tool := NewPublishToWebTool(&fakeUploader{}, nil)
	info := tool.Info()
	if info.Name != "publish_to_web" {
		t.Errorf("name = %q, want publish_to_web", info.Name)
	}
	required := info.Required
	if len(required) != 2 || required[0] != "path" || required[1] != "purpose" {
		t.Errorf("required = %v, want [path purpose]", required)
	}
}
