package daemon

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
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

func TestMaterializeInlineImageBlocks_Success(t *testing.T) {
	dir := t.TempDir()
	raw := []byte("fake-png-data")
	blocks, cleanup := materializeInlineImageBlocks(dir, []RequestContentBlock{
		{Type: "text", Text: "describe this"},
		{Type: "image", Source: &client.ImageSource{
			Type:      "base64",
			MediaType: "image/png",
			Data:      base64.StdEncoding.EncodeToString(raw),
		}},
	})
	defer func() {
		if cleanup != nil {
			cleanup()
		}
	}()

	if len(blocks) != 2 {
		t.Fatalf("expected 2 blocks, got %d", len(blocks))
	}
	if blocks[0].Type != "text" || blocks[0].Text != "describe this" {
		t.Fatalf("block 0 mismatch: %+v", blocks[0])
	}
	if blocks[1].Type != "file_ref" {
		t.Fatalf("block 1: expected file_ref, got %s", blocks[1].Type)
	}
	if blocks[1].Filename != "attachment_0.png" {
		t.Fatalf("expected generated filename attachment_0.png, got %q", blocks[1].Filename)
	}
	if blocks[1].ByteSize != int64(len(raw)) {
		t.Fatalf("expected byte size %d, got %d", len(raw), blocks[1].ByteSize)
	}
	if !strings.HasPrefix(blocks[1].FilePath, filepath.Join(dir, "tmp", "attachments")) {
		t.Fatalf("path %s escapes attachment dir", blocks[1].FilePath)
	}

	data, err := os.ReadFile(blocks[1].FilePath)
	if err != nil {
		t.Fatalf("failed to read materialized image: %v", err)
	}
	if string(data) != string(raw) {
		t.Fatalf("materialized bytes mismatch: got %q want %q", string(data), string(raw))
	}

	attachmentDir := filepath.Dir(blocks[1].FilePath)
	if cleanup == nil {
		t.Fatal("expected cleanup function")
	}
	cleanup()
	cleanup = nil
	if _, err := os.Stat(attachmentDir); !os.IsNotExist(err) {
		t.Fatalf("attachment dir should be removed after cleanup, got err: %v", err)
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

// TestMaterializeInlineImageBlocks_PreDecodeSizeGuard ensures an inline
// image block whose base64 payload exceeds the guard never reaches
// base64.DecodeString (which would allocate a decoded buffer larger than
// the downstream resolveFileRef cap). The oversized block is replaced
// with a user-visible text error so the LLM sees a clear reason instead
// of the Anthropic API returning an opaque 400 on the downstream call.
func TestMaterializeInlineImageBlocks_PreDecodeSizeGuard(t *testing.T) {
	dir := t.TempDir()
	oversize := strings.Repeat("A", maxInlineImageBase64Bytes+1)
	blocks, cleanup := materializeInlineImageBlocks(dir, []RequestContentBlock{
		{Type: "image", Source: &client.ImageSource{
			Type:      "base64",
			MediaType: "image/png",
			Data:      oversize,
		}},
	})
	if cleanup != nil {
		defer cleanup()
	}
	if len(blocks) != 1 || blocks[0].Type != "text" {
		t.Fatalf("oversize block should be replaced by a text error, got %+v", blocks)
	}
	if !strings.Contains(blocks[0].Text, "rejected") {
		t.Errorf("text error should mention rejection: %q", blocks[0].Text)
	}
	if cleanup != nil {
		t.Error("no attachment dir should be created when every block is rejected by the size guard")
	}
}

// TestMaterializeInlineImageBlocks_FilenameSanitized verifies that a
// caller-supplied filename containing path separators is reduced to its
// basename before being stored on the file_ref block. The Filename field
// is echoed into the model-visible attachment hint, so we can't let a
// value like "../etc/passwd" surface there even though the disk path is
// safe.
func TestMaterializeInlineImageBlocks_FilenameSanitized(t *testing.T) {
	dir := t.TempDir()
	raw := []byte("fake-png-data")
	blocks, cleanup := materializeInlineImageBlocks(dir, []RequestContentBlock{
		{Type: "image", Filename: "../etc/passwd.png", Source: &client.ImageSource{
			Type:      "base64",
			MediaType: "image/png",
			Data:      base64.StdEncoding.EncodeToString(raw),
		}},
	})
	if cleanup != nil {
		defer cleanup()
	}
	if len(blocks) != 1 || blocks[0].Type != "file_ref" {
		t.Fatalf("expected one file_ref block, got %+v", blocks)
	}
	if blocks[0].Filename != "passwd.png" {
		t.Errorf("Filename should be reduced to basename; got %q", blocks[0].Filename)
	}
	if strings.Contains(blocks[0].FilePath, "..") {
		t.Errorf("FilePath must not contain traversal segments; got %q", blocks[0].FilePath)
	}
}

// ---- Phase-1 cloud-extension protocol tests (plan §4.3) ----

// TestDownloadRemoteFiles_DocumentB64 verifies the DocumentB64 priority
// branch: cloud-supplied PDF bytes are decoded to a temp file and surfaced
// as a `document` content block (base64 source) followed by a `text` hint
// pointing at the local path.
func TestDownloadRemoteFiles_DocumentB64(t *testing.T) {
	dir := t.TempDir()
	pdfBytes := []byte("%PDF-1.4 fake pdf payload for testing")
	encoded := base64.StdEncoding.EncodeToString(pdfBytes)
	blocks, cleanup := downloadRemoteFiles(dir, []RemoteFile{
		{
			Name:           "spec.pdf",
			MimeType:       "application/pdf",
			DocumentB64: encoded,
		},
	})
	defer cleanup()

	if len(blocks) != 2 {
		t.Fatalf("expected 2 blocks (document + hint), got %d: %+v", len(blocks), blocks)
	}
	if blocks[0].Type != "document" {
		t.Fatalf("block 0 should be document, got %q", blocks[0].Type)
	}
	if blocks[0].Source == nil || blocks[0].Source.MediaType != "application/pdf" {
		t.Fatalf("document block missing PDF source: %+v", blocks[0])
	}
	if blocks[0].Source.Data != encoded {
		t.Errorf("document base64 should match cloud input exactly; got %d bytes vs %d", len(blocks[0].Source.Data), len(encoded))
	}
	if blocks[1].Type != "text" {
		t.Fatalf("block 1 should be text hint, got %q", blocks[1].Type)
	}
	if !strings.Contains(blocks[1].Text, "Attached PDF: spec.pdf") {
		t.Errorf("hint should mention filename; got %q", blocks[1].Text)
	}
	if !strings.Contains(blocks[1].Text, "use file_read") {
		t.Errorf("hint should mention file_read; got %q", blocks[1].Text)
	}
}

// TestDownloadRemoteFiles_DocumentB64Whitespace ensures whitespace in
// cloud-provided base64 (newlines from chunked encoders) is stripped so the
// outbound prompt-cache prefix stays byte-stable across re-sends.
func TestDownloadRemoteFiles_DocumentB64Whitespace(t *testing.T) {
	dir := t.TempDir()
	raw := []byte("hello-pdf-bytes")
	encoded := base64.StdEncoding.EncodeToString(raw)
	// Splice newlines like older base64 encoders.
	chunked := ""
	for i, r := range encoded {
		chunked += string(r)
		if i%4 == 0 {
			chunked += "\n"
		}
	}
	blocks, cleanup := downloadRemoteFiles(dir, []RemoteFile{
		{Name: "a.pdf", MimeType: "application/pdf", DocumentB64: chunked},
	})
	defer cleanup()
	if len(blocks) < 1 || blocks[0].Type != "document" {
		t.Fatalf("expected document block, got %+v", blocks)
	}
	if strings.ContainsAny(blocks[0].Source.Data, "\r\n\t ") {
		t.Errorf("document base64 should be whitespace-stripped; got %q", blocks[0].Source.Data)
	}
}

// TestDownloadRemoteFiles_DocumentB64SizeGuard ensures oversized base64
// payloads are rejected before decoding (and fall back to URL if available).
func TestDownloadRemoteFiles_DocumentB64SizeGuard(t *testing.T) {
	dir := t.TempDir()
	oversize := strings.Repeat("A", maxInlineDocumentB64Bytes+1)
	blocks, cleanup := downloadRemoteFiles(dir, []RemoteFile{
		{Name: "huge.pdf", MimeType: "application/pdf", DocumentB64: oversize},
	})
	defer cleanup()
	if len(blocks) != 1 || blocks[0].Type != "text" {
		t.Fatalf("oversize doc should yield single text error block, got %+v", blocks)
	}
	if !strings.Contains(blocks[0].Text, "unable to process") {
		t.Errorf("error block should mention failure; got %q", blocks[0].Text)
	}
}

// TestDownloadRemoteFiles_ExtractedText verifies the ExtractedText priority
// branch: cloud's pre-extracted text becomes a single text content block
// prefixed with the filename/mimetype.
func TestDownloadRemoteFiles_ExtractedText(t *testing.T) {
	dir := t.TempDir()
	blocks, cleanup := downloadRemoteFiles(dir, []RemoteFile{
		{
			Name:          "report.docx",
			MimeType:      "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
			ExtractedText: "# Q4 Report\n\nRevenue is up 12%.",
		},
	})
	defer cleanup()
	if len(blocks) != 1 || blocks[0].Type != "text" {
		t.Fatalf("expected one text block, got %+v", blocks)
	}
	if !strings.HasPrefix(blocks[0].Text, "[Attached: report.docx (application/vnd.openxmlformats-") {
		t.Errorf("text should start with attachment header; got %q", blocks[0].Text)
	}
	if !strings.Contains(blocks[0].Text, "Q4 Report") {
		t.Errorf("text should carry the extracted body; got %q", blocks[0].Text)
	}
}

// TestDownloadRemoteFiles_ExtractedTextDaemonTruncation ensures the daemon
// enforces MaxExtractedTextChars as defense-in-depth even when cloud sends an
// oversized payload (plan §4.5.1).
func TestDownloadRemoteFiles_ExtractedTextDaemonTruncation(t *testing.T) {
	dir := t.TempDir()
	huge := strings.Repeat("a", MaxExtractedTextChars+1000)
	blocks, cleanup := downloadRemoteFiles(dir, []RemoteFile{
		{Name: "huge.txt", MimeType: "text/plain", ExtractedText: huge},
	})
	defer cleanup()
	if len(blocks) != 1 {
		t.Fatalf("expected 1 block, got %d", len(blocks))
	}
	// The text body (after header) must be no larger than MaxExtractedTextChars
	// plus the truncation footer.
	if !strings.Contains(blocks[0].Text, "Daemon truncated extracted text") {
		t.Errorf("daemon should append a truncation note; got tail %q", blocks[0].Text[len(blocks[0].Text)-200:])
	}
}

// TestDownloadRemoteFiles_BackwardCompat_URLPath ensures the legacy URL
// download path still works when neither DocumentB64 nor ExtractedText
// is set (older cloud sending only URL + auth_header).
func TestDownloadRemoteFiles_BackwardCompat_URLPath(t *testing.T) {
	skipURLValidation(t)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		w.Write([]byte("downloaded-pdf-bytes"))
	}))
	defer ts.Close()

	dir := t.TempDir()
	blocks, cleanup := downloadRemoteFiles(dir, []RemoteFile{
		{Name: "legacy.pdf", URL: ts.URL + "/legacy.pdf"},
	})
	defer cleanup()
	if len(blocks) != 1 || blocks[0].Type != "file_ref" {
		t.Fatalf("expected file_ref block for URL-only payload, got %+v", blocks)
	}
	if blocks[0].Filename != "legacy.pdf" {
		t.Errorf("filename mismatch; got %q", blocks[0].Filename)
	}
}

// TestRemoteFile_UnmarshalProtocol checks that the JSON tags match plan §4.3
// exactly. The protocol field names (extracted_text, document_b64,
// extraction_note) are part of the contract with shannon-cloud — drifting
// from them silently breaks WS interop.
func TestRemoteFile_UnmarshalProtocol(t *testing.T) {
	raw := `{
		"name": "x.docx",
		"mimetype": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
		"size": 1234,
		"url": "https://example.com/x.docx",
		"auth_header": "Bearer xyz",
		"extracted_text": "body text",
		"document_b64": "ZmFrZQ==",
		"extraction_note": "via python-docx"
	}`
	var got RemoteFile
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.ExtractedText != "body text" {
		t.Errorf("ExtractedText tag mismatch; got %q", got.ExtractedText)
	}
	if got.DocumentB64 != "ZmFrZQ==" {
		t.Errorf("DocumentB64 should map to JSON tag document_b64; got %q", got.DocumentB64)
	}
	if got.ExtractionNote != "via python-docx" {
		t.Errorf("ExtractionNote tag mismatch; got %q", got.ExtractionNote)
	}
}

// TestRemoteFile_BackwardCompat ensures an older cloud payload that lacks the
// new fields unmarshals cleanly into a legacy-shaped RemoteFile. This guards
// the "new daemon, old cloud" interop matrix.
func TestRemoteFile_BackwardCompat(t *testing.T) {
	raw := `{"name":"old.bin","mimetype":"application/octet-stream","size":42,"url":"https://example.com/old.bin"}`
	var got RemoteFile
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.ExtractedText != "" || got.DocumentB64 != "" || got.ExtractionNote != "" {
		t.Errorf("legacy payload should leave new fields zero; got %+v", got)
	}
	if got.URL != "https://example.com/old.bin" {
		t.Errorf("legacy URL should round-trip; got %q", got.URL)
	}
}

// TestCapabilities_AdvertisesNewTokens guards that daemon advertises the new
// inline_document_b64 and inline_extracted_text capability tokens.
func TestCapabilities_AdvertisesNewTokens(t *testing.T) {
	want := map[string]bool{
		CapInlineDocumentB64:   false,
		CapInlineExtractedText: false,
	}
	for _, c := range Capabilities {
		if _, ok := want[c]; ok {
			want[c] = true
		}
	}
	for token, found := range want {
		if !found {
			t.Errorf("default Capabilities = %v, missing %q", Capabilities, token)
		}
	}
}

// TestDownloadRemoteFiles_DocumentB64_EmptyMIME verifies that DocumentB64
// payloads without a MimeType are rejected (instead of silently defaulting
// to application/pdf) and the caller falls back to URL download. Without
// this guard a future cloud bug shipping non-PDF bytes + empty MIME would
// be mis-forwarded to Anthropic as a PDF and 400.
func TestDownloadRemoteFiles_DocumentB64_EmptyMIME(t *testing.T) {
	skipURLValidation(t)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		w.Write([]byte("fallback PDF bytes from URL"))
	}))
	defer ts.Close()

	dir := t.TempDir()
	blocks, cleanup := downloadRemoteFiles(dir, []RemoteFile{{
		Name:        "bad.pdf",
		MimeType:    "", // empty — should NOT default to PDF
		URL:         ts.URL + "/file.pdf",
		DocumentB64: "ZmFrZQ==", // non-empty so the document branch is exercised
	}})
	defer cleanup()

	if len(blocks) != 1 {
		t.Fatalf("expected 1 block (URL fallback), got %d: %+v", len(blocks), blocks)
	}
	if blocks[0].Type != "file_ref" {
		t.Fatalf("expected file_ref from URL fallback (not document block), got %s: %+v", blocks[0].Type, blocks[0])
	}
	if blocks[0].ByteSize != int64(len("fallback PDF bytes from URL")) {
		t.Errorf("URL fallback bytes not downloaded; got byte_size=%d", blocks[0].ByteSize)
	}
}

