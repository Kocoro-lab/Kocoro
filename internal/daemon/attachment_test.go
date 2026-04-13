package daemon

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// skipURLValidation disables SSRF checks for httptest (loopback) URLs.
func skipURLValidation(t *testing.T) {
	t.Helper()
	orig := urlValidator
	urlValidator = func(string) error { return nil }
	t.Cleanup(func() { urlValidator = orig })
}

func TestDownloadRemoteFiles_Success(t *testing.T) {
	skipURLValidation(t)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte("hello world"))
	}))
	defer ts.Close()

	dir := t.TempDir()
	blocks, cleanup := downloadRemoteFiles(dir, []RemoteFile{
		{Name: "test.txt", URL: ts.URL + "/test.txt"},
	})
	defer cleanup()

	if len(blocks) != 1 {
		t.Fatalf("expected 1 block, got %d", len(blocks))
	}
	b := blocks[0]
	if b.Type != "file_ref" {
		t.Fatalf("expected file_ref, got %s", b.Type)
	}
	if b.Filename != "test.txt" {
		t.Errorf("expected filename test.txt, got %s", b.Filename)
	}
	if b.ByteSize != 11 {
		t.Errorf("expected 11 bytes, got %d", b.ByteSize)
	}

	data, err := os.ReadFile(b.FilePath)
	if err != nil {
		t.Fatalf("failed to read downloaded file: %v", err)
	}
	if string(data) != "hello world" {
		t.Errorf("expected 'hello world', got %q", string(data))
	}
}

func TestDownloadRemoteFiles_WithAuth(t *testing.T) {
	skipURLValidation(t)
	var gotAuth string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Write([]byte("ok"))
	}))
	defer ts.Close()

	dir := t.TempDir()
	_, cleanup := downloadRemoteFiles(dir, []RemoteFile{
		{Name: "doc.pdf", URL: ts.URL + "/doc.pdf", AuthHeader: "Bearer token123"},
	})
	defer cleanup()

	if gotAuth != "Bearer token123" {
		t.Errorf("expected 'Bearer token123', got %q", gotAuth)
	}
}

func TestDownloadRemoteFiles_AuthPreservedOnRedirect(t *testing.T) {
	skipURLValidation(t)
	// Simulates Slack redirecting to a CDN — Authorization must survive.
	var redirectAuth string
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirectAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "image/png")
		w.Write([]byte("fake-png-data"))
	}))
	defer cdn.Close()

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, cdn.URL+"/file.png", http.StatusFound)
	}))
	defer origin.Close()

	dir := t.TempDir()
	blocks, cleanup := downloadRemoteFiles(dir, []RemoteFile{
		{Name: "photo.png", URL: origin.URL + "/photo.png", AuthHeader: "Bearer xoxb-slack-token"},
	})
	defer cleanup()

	if redirectAuth != "Bearer xoxb-slack-token" {
		t.Errorf("auth header lost on redirect: got %q", redirectAuth)
	}
	if len(blocks) != 1 || blocks[0].Type != "file_ref" {
		t.Fatalf("expected 1 file_ref block, got %v", blocks)
	}
}

func TestDownloadRemoteFiles_HTMLResponseRejected(t *testing.T) {
	skipURLValidation(t)
	// Slack returns HTML login page when auth fails.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte("<html><body>Sign in to Slack</body></html>"))
	}))
	defer ts.Close()

	dir := t.TempDir()
	blocks, cleanup := downloadRemoteFiles(dir, []RemoteFile{
		{Name: "image.png", URL: ts.URL + "/image.png", AuthHeader: "Bearer bad-token"},
	})
	defer cleanup()

	if len(blocks) != 1 {
		t.Fatalf("expected 1 block, got %d", len(blocks))
	}
	if blocks[0].Type != "text" {
		t.Fatalf("expected text error block, got %s", blocks[0].Type)
	}
	if !strings.Contains(blocks[0].Text, "Error") {
		t.Errorf("expected error message, got %q", blocks[0].Text)
	}
}

func TestDownloadRemoteFiles_Failure(t *testing.T) {
	skipURLValidation(t)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ts.Close()

	dir := t.TempDir()
	blocks, cleanup := downloadRemoteFiles(dir, []RemoteFile{
		{Name: "missing.txt", URL: ts.URL + "/missing.txt"},
	})
	defer cleanup()

	if len(blocks) != 1 {
		t.Fatalf("expected 1 block, got %d", len(blocks))
	}
	if blocks[0].Type != "text" {
		t.Fatalf("expected text error block, got %s", blocks[0].Type)
	}
	if !strings.Contains(blocks[0].Text, "Error") {
		t.Errorf("expected error text, got %q", blocks[0].Text)
	}
}

func TestDownloadRemoteFiles_MultipleFiles(t *testing.T) {
	skipURLValidation(t)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/a.txt":
			w.Write([]byte("aaa"))
		case "/b.png":
			w.Header().Set("Content-Type", "image/png")
			w.Write([]byte("png"))
		case "/c.txt":
			w.WriteHeader(http.StatusForbidden)
		}
	}))
	defer ts.Close()

	dir := t.TempDir()
	blocks, cleanup := downloadRemoteFiles(dir, []RemoteFile{
		{Name: "a.txt", URL: ts.URL + "/a.txt"},
		{Name: "b.png", URL: ts.URL + "/b.png"},
		{Name: "c.txt", URL: ts.URL + "/c.txt"},
	})
	defer cleanup()

	if len(blocks) != 3 {
		t.Fatalf("expected 3 blocks, got %d", len(blocks))
	}
	// First two succeed, third fails.
	if blocks[0].Type != "file_ref" {
		t.Errorf("block 0: expected file_ref, got %s", blocks[0].Type)
	}
	if blocks[1].Type != "file_ref" {
		t.Errorf("block 1: expected file_ref, got %s", blocks[1].Type)
	}
	if blocks[2].Type != "text" {
		t.Errorf("block 2: expected text error, got %s", blocks[2].Type)
	}
}

func TestDownloadRemoteFiles_FilenameSanitization(t *testing.T) {
	skipURLValidation(t)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("x"))
	}))
	defer ts.Close()

	dir := t.TempDir()
	blocks, cleanup := downloadRemoteFiles(dir, []RemoteFile{
		{Name: "../../../etc/passwd", URL: ts.URL + "/a"},
		{Name: "", URL: ts.URL + "/b"},
		{Name: ".", URL: ts.URL + "/c"},
		{Name: "normal.png", URL: ts.URL + "/d"},
	})
	defer cleanup()

	if len(blocks) != 4 {
		t.Fatalf("expected 4 blocks, got %d", len(blocks))
	}

	// All should be file_ref (downloads succeed).
	for i, b := range blocks {
		if b.Type != "file_ref" {
			t.Errorf("block %d: expected file_ref, got %s", i, b.Type)
			continue
		}
		// No file should escape the attachment directory.
		if !strings.HasPrefix(b.FilePath, filepath.Join(dir, "tmp", "attachments")) {
			t.Errorf("block %d: path %s escapes attachment dir", i, b.FilePath)
		}
	}

	// Display names use original filenames (or sanitized fallback for empty names).
	expected := []string{"../../../etc/passwd", "1_file", "2_file", "normal.png"}
	for i, b := range blocks {
		if b.Filename != expected[i] {
			t.Errorf("block %d: expected filename %q, got %q", i, expected[i], b.Filename)
		}
	}
}

func TestDownloadRemoteFiles_Empty(t *testing.T) {
	dir := t.TempDir()
	blocks, _ := downloadRemoteFiles(dir, nil)
	if blocks != nil {
		t.Errorf("expected nil blocks for empty input, got %v", blocks)
	}

	blocks, _ = downloadRemoteFiles(dir, []RemoteFile{})
	if blocks != nil {
		t.Errorf("expected nil blocks for empty slice, got %v", blocks)
	}
}

func TestDownloadRemoteFiles_Cleanup(t *testing.T) {
	skipURLValidation(t)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("data"))
	}))
	defer ts.Close()

	dir := t.TempDir()
	blocks, cleanup := downloadRemoteFiles(dir, []RemoteFile{
		{Name: "file.txt", URL: ts.URL + "/file.txt"},
	})

	if len(blocks) != 1 || blocks[0].Type != "file_ref" {
		t.Fatalf("expected 1 file_ref block, got %v", blocks)
	}
	filePath := blocks[0].FilePath

	// File should exist before cleanup.
	if _, err := os.Stat(filePath); err != nil {
		t.Fatalf("downloaded file should exist: %v", err)
	}

	// After cleanup, the entire attachment directory should be gone.
	cleanup()
	if _, err := os.Stat(filePath); !os.IsNotExist(err) {
		t.Errorf("downloaded file should be removed after cleanup, got err: %v", err)
	}
	attachDir := filepath.Dir(filePath)
	if _, err := os.Stat(attachDir); !os.IsNotExist(err) {
		t.Errorf("attachment dir should be removed after cleanup, got err: %v", err)
	}
}

func TestDownloadRemoteFiles_FileCountCap(t *testing.T) {
	skipURLValidation(t)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("x"))
	}))
	defer ts.Close()

	// Create more files than maxFiles.
	files := make([]RemoteFile, maxFiles+3)
	for i := range files {
		files[i] = RemoteFile{Name: fmt.Sprintf("file%d.txt", i), URL: ts.URL + fmt.Sprintf("/%d", i)}
	}

	dir := t.TempDir()
	blocks, cleanup := downloadRemoteFiles(dir, files)
	defer cleanup()

	// Should have maxFiles file_ref blocks + 1 warning text block.
	fileRefCount := 0
	warningCount := 0
	for _, b := range blocks {
		switch b.Type {
		case "file_ref":
			fileRefCount++
		case "text":
			if strings.Contains(b.Text, "Warning") {
				warningCount++
			}
		}
	}
	if fileRefCount != maxFiles {
		t.Errorf("expected %d file_ref blocks, got %d", maxFiles, fileRefCount)
	}
	if warningCount != 1 {
		t.Errorf("expected 1 warning block, got %d", warningCount)
	}
}

func TestValidateDownloadURL(t *testing.T) {
	tests := []struct {
		url     string
		wantErr bool
		errMsg  string
	}{
		{"https://files.slack.com/files-pri/T123/image.png", false, ""},
		{"http://example.com/file.txt", false, ""},
		{"ftp://example.com/file.txt", true, "unsupported URL scheme"},
		{"file:///etc/passwd", true, "unsupported URL scheme"},
		{"http://127.0.0.1/secret", true, "private/loopback"},
		{"http://[::1]/secret", true, "private/loopback"},
		{"http://169.254.169.254/latest/meta-data/", true, "private/loopback"},
		{"http://10.0.0.1/internal", true, "private/loopback"},
		{"http://192.168.1.1/internal", true, "private/loopback"},
		{"http://localhost/secret", true, "localhost"},
		{"http://localhost:7533/api/config", true, "localhost"},
		{"http://LOCALHOST/secret", true, "localhost"},
		{"http://metadata.google.internal/v1/", true, "metadata.google.internal"},
	}
	for _, tt := range tests {
		err := validateDownloadURL(tt.url)
		if tt.wantErr {
			if err == nil {
				t.Errorf("validateDownloadURL(%q): expected error containing %q, got nil", tt.url, tt.errMsg)
			} else if !strings.Contains(err.Error(), tt.errMsg) {
				t.Errorf("validateDownloadURL(%q): expected error containing %q, got %q", tt.url, tt.errMsg, err.Error())
			}
		} else {
			if err != nil {
				t.Errorf("validateDownloadURL(%q): unexpected error: %v", tt.url, err)
			}
		}
	}
}

func TestDownloadRemoteFiles_SSRFBlocked(t *testing.T) {
	// Use the real validator (don't skip).
	dir := t.TempDir()
	blocks, cleanup := downloadRemoteFiles(dir, []RemoteFile{
		{Name: "secret.txt", URL: "http://169.254.169.254/latest/meta-data/"},
		{Name: "local.txt", URL: "http://127.0.0.1:7533/api/config"},
		{Name: "localhost.txt", URL: "http://localhost:7533/api/config"},
	})
	defer cleanup()

	// All should produce error blocks.
	if len(blocks) != 3 {
		t.Fatalf("expected 3 blocks, got %d", len(blocks))
	}
	for i, b := range blocks {
		if b.Type != "text" {
			t.Errorf("block %d: expected text error, got %s", i, b.Type)
		}
		if !strings.Contains(b.Text, "Error") {
			t.Errorf("block %d: expected error text, got %q", i, b.Text)
		}
	}
}

func TestDownloadRemoteFiles_RedirectToLoopbackBlocked(t *testing.T) {
	skipURLValidation(t)
	// Restore real validator only for redirect checks — the initial URL
	// validation is skipped (httptest is loopback), but redirect validation
	// in CheckRedirect uses urlValidator which we override here.
	orig := urlValidator
	urlValidator = func(rawURL string) error { return nil }
	t.Cleanup(func() { urlValidator = orig })

	// Server that redirects to a loopback address.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://127.0.0.1:9999/secret", http.StatusFound)
	}))
	defer ts.Close()

	// Now set the validator to the real one so CheckRedirect catches the redirect.
	urlValidator = validateDownloadURL

	dir := t.TempDir()
	blocks, cleanup := downloadRemoteFiles(dir, []RemoteFile{
		{Name: "redirected.txt", URL: ts.URL + "/start"},
	})
	defer cleanup()

	if len(blocks) != 1 {
		t.Fatalf("expected 1 block, got %d", len(blocks))
	}
	if blocks[0].Type != "text" {
		t.Errorf("expected text error block, got %s", blocks[0].Type)
	}
}

func TestSanitizeFilename(t *testing.T) {
	tests := []struct {
		index int
		name  string
		want  string
	}{
		{0, "report.pdf", "0_report.pdf"},
		{1, "../../../evil.txt", "1_evil.txt"},
		{2, "/absolute/path.go", "2_path.go"},
		{3, "", "3_file"},
		{4, ".", "4_file"},
		{5, "..", "5_file"},
		{6, "hello world.txt", "6_hello world.txt"},
	}

	for _, tt := range tests {
		got := sanitizeFilename(tt.index, tt.name)
		if got != tt.want {
			t.Errorf("sanitizeFilename(%d, %q) = %q, want %q", tt.index, tt.name, got, tt.want)
		}
	}
}

func TestSlackDownloadURL(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{
			"https://files.slack.com/files-pri/T06CD61PYPR-F0ASF7TR410/image.png",
			"https://files.slack.com/files-pri/T06CD61PYPR-F0ASF7TR410/download/image.png",
		},
		{
			// Already has /download/ — no change
			"https://files.slack.com/files-pri/T06CD61PYPR-F0ASF7TR410/download/image.png",
			"https://files.slack.com/files-pri/T06CD61PYPR-F0ASF7TR410/download/image.png",
		},
		{
			// Non-Slack URL — no change
			"https://example.com/file.png",
			"https://example.com/file.png",
		},
		{
			// Feishu URL — no change
			"https://open.feishu.cn/open-apis/drive/v1/files/xxx",
			"https://open.feishu.cn/open-apis/drive/v1/files/xxx",
		},
		{
			// /files-pri/ in query string on non-Slack host — should NOT rewrite
			"https://example.com/download?path=/files-pri/T123/file.png",
			"https://example.com/download?path=/files-pri/T123/file.png",
		},
	}
	for _, tt := range tests {
		got := slackDownloadURL(tt.input)
		if got != tt.want {
			t.Errorf("slackDownloadURL(%q)\n  got  %q\n  want %q", tt.input, got, tt.want)
		}
	}
}

func TestSanitizeError(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{
			`Get "https://files.slack.com/files-pri/T123/file?token=xoxb-secret": dial tcp`,
			`Get "<redacted-url>": dial tcp`,
		},
		{
			"simple error without urls",
			"simple error without urls",
		},
		{
			`failed: http://169.254.169.254/meta blocked`,
			`failed: <redacted-url> blocked`,
		},
	}
	for _, tt := range tests {
		got := sanitizeError(fmt.Errorf("%s", tt.input))
		if got != tt.want {
			t.Errorf("sanitizeError(%q)\n  got  %q\n  want %q", tt.input, got, tt.want)
		}
	}
}

func TestRemoteFile_JSONUnmarshal(t *testing.T) {
	// Verify JSON tags match what Cloud actually sends.
	raw := `{"name":"img.png","mimetype":"image/png","size":1234,"url":"https://example.com/f","auth_header":"Bearer tok"}`
	var f RemoteFile
	if err := json.Unmarshal([]byte(raw), &f); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if f.Name != "img.png" {
		t.Errorf("Name: got %q", f.Name)
	}
	if f.MimeType != "image/png" {
		t.Errorf("MimeType: got %q (json tag mismatch?)", f.MimeType)
	}
	if f.Size != 1234 {
		t.Errorf("Size: got %d", f.Size)
	}
	if f.URL != "https://example.com/f" {
		t.Errorf("URL: got %q", f.URL)
	}
	if f.AuthHeader != "Bearer tok" {
		t.Errorf("AuthHeader: got %q", f.AuthHeader)
	}
}
