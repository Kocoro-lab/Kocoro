package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDownloadRemoteFiles_Success(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte("hello world"))
	}))
	defer ts.Close()

	dir := t.TempDir()
	blocks := downloadRemoteFiles(dir, []RemoteFile{
		{Name: "test.txt", URL: ts.URL + "/test.txt"},
	})

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
	var gotAuth string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Write([]byte("ok"))
	}))
	defer ts.Close()

	dir := t.TempDir()
	downloadRemoteFiles(dir, []RemoteFile{
		{Name: "doc.pdf", URL: ts.URL + "/doc.pdf", AuthHeader: "Bearer token123"},
	})

	if gotAuth != "Bearer token123" {
		t.Errorf("expected 'Bearer token123', got %q", gotAuth)
	}
}

func TestDownloadRemoteFiles_AuthPreservedOnRedirect(t *testing.T) {
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
	blocks := downloadRemoteFiles(dir, []RemoteFile{
		{Name: "photo.png", URL: origin.URL + "/photo.png", AuthHeader: "Bearer xoxb-slack-token"},
	})

	if redirectAuth != "Bearer xoxb-slack-token" {
		t.Errorf("auth header lost on redirect: got %q", redirectAuth)
	}
	if len(blocks) != 1 || blocks[0].Type != "file_ref" {
		t.Fatalf("expected 1 file_ref block, got %v", blocks)
	}
}

func TestDownloadRemoteFiles_HTMLResponseRejected(t *testing.T) {
	// Slack returns HTML login page when auth fails.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte("<html><body>Sign in to Slack</body></html>"))
	}))
	defer ts.Close()

	dir := t.TempDir()
	blocks := downloadRemoteFiles(dir, []RemoteFile{
		{Name: "image.png", URL: ts.URL + "/image.png", AuthHeader: "Bearer bad-token"},
	})

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
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ts.Close()

	dir := t.TempDir()
	blocks := downloadRemoteFiles(dir, []RemoteFile{
		{Name: "missing.txt", URL: ts.URL + "/missing.txt"},
	})

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
	blocks := downloadRemoteFiles(dir, []RemoteFile{
		{Name: "a.txt", URL: ts.URL + "/a.txt"},
		{Name: "b.png", URL: ts.URL + "/b.png"},
		{Name: "c.txt", URL: ts.URL + "/c.txt"},
	})

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
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("x"))
	}))
	defer ts.Close()

	dir := t.TempDir()
	blocks := downloadRemoteFiles(dir, []RemoteFile{
		{Name: "../../../etc/passwd", URL: ts.URL + "/a"},
		{Name: "", URL: ts.URL + "/b"},
		{Name: ".", URL: ts.URL + "/c"},
		{Name: "normal.png", URL: ts.URL + "/d"},
	})

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
	blocks := downloadRemoteFiles(dir, nil)
	if blocks != nil {
		t.Errorf("expected nil blocks for empty input, got %v", blocks)
	}

	blocks = downloadRemoteFiles(dir, []RemoteFile{})
	if blocks != nil {
		t.Errorf("expected nil blocks for empty slice, got %v", blocks)
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
	}
	for _, tt := range tests {
		got := slackDownloadURL(tt.input)
		if got != tt.want {
			t.Errorf("slackDownloadURL(%q)\n  got  %q\n  want %q", tt.input, got, tt.want)
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
