package daemon

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestConvertFilesToInjected_PrioritizesExtractedText(t *testing.T) {
	// When ExtractedText and DocumentB64 are both set on the inject path,
	// ExtractedText wins. Note this is intentionally the REVERSE of
	// downloadRemoteFiles (which prefers DocumentB64 for PDF vision fidelity);
	// see ConvertFilesToInjected doc-comment for the rationale.
	files := []RemoteFile{{
		Name:          "report.pdf",
		MimeType:      "application/pdf",
		URL:           "https://example.com/report.pdf",
		ExtractedText: "extracted plain text",
		DocumentB64:   "aGVsbG8=", // base64("hello")
	}}
	out := ConvertFilesToInjected(context.Background(), files)
	if len(out) != 1 {
		t.Fatalf("expected 1 InjectedFile, got %d", len(out))
	}
	if out[0].Type != "text" {
		t.Errorf("Type: got %q want text", out[0].Type)
	}
	if out[0].Data != "extracted plain text" {
		t.Errorf("Data: got %q want extracted plain text", out[0].Data)
	}
}

func TestConvertFilesToInjected_PrioritizesDocumentB64(t *testing.T) {
	// Empty ExtractedText, populated DocumentB64 — emit a "document" block.
	files := []RemoteFile{{
		Name:        "report.pdf",
		MimeType:    "application/pdf",
		URL:         "https://example.com/report.pdf",
		DocumentB64: "JVBERi0=", // base64-ish PDF magic
	}}
	out := ConvertFilesToInjected(context.Background(), files)
	if len(out) != 1 {
		t.Fatalf("expected 1 InjectedFile, got %d", len(out))
	}
	if out[0].Type != "document" {
		t.Errorf("Type: got %q want document", out[0].Type)
	}
	if out[0].MediaType != "application/pdf" {
		t.Errorf("MediaType: got %q want application/pdf", out[0].MediaType)
	}
	if out[0].Data != "JVBERi0=" {
		t.Errorf("Data: got %q want JVBERi0=", out[0].Data)
	}
}

func TestConvertFilesToInjected_SkipsNonImageURLOnly(t *testing.T) {
	// URL-only with non-image MIME — the inject path has no disk-cleanup hook
	// for the file_ref/disk-download flow, so these are intentionally skipped.
	files := []RemoteFile{{
		Name:     "data.bin",
		MimeType: "application/octet-stream",
		URL:      "https://example.com/data.bin",
	}}
	out := ConvertFilesToInjected(context.Background(), files)
	if len(out) != 0 {
		t.Errorf("expected 0 entries (non-image URL-only should be skipped), got %d: %+v", len(out), out)
	}
}

func TestDownloadInjectedImageBase64_Oversize(t *testing.T) {
	skipURLValidation(t)
	// Stream maxInlineImageDecodedBytes+100 bytes of fake image data.
	// http.ResponseWriter streams so this doesn't peak server memory.
	oversize := maxInlineImageDecodedBytes + 100
	chunk := make([]byte, 64*1024)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.WriteHeader(http.StatusOK)
		remaining := oversize
		for remaining > 0 {
			n := len(chunk)
			if n > remaining {
				n = remaining
			}
			if _, err := w.Write(chunk[:n]); err != nil {
				return
			}
			remaining -= n
		}
	}))
	defer ts.Close()

	out, err := downloadInjectedImageBase64(context.Background(), ts.URL+"/big.png", "")
	if err == nil {
		t.Fatalf("expected oversize error, got nil; out len=%d", len(out))
	}
	if !strings.Contains(err.Error(), "exceeds") && !strings.Contains(err.Error(), "cap") {
		t.Errorf("error should mention exceeds/cap; got %q", err.Error())
	}
	if out != "" {
		t.Errorf("expected empty base64 on error, got %d chars", len(out))
	}
}

func TestDownloadInjectedImageBase64_SSRFBlocked(t *testing.T) {
	// Real validator: rejects loopback/link-local. AWS metadata endpoint is
	// the canonical SSRF target.
	out, err := downloadInjectedImageBase64(context.Background(), "http://169.254.169.254/latest/meta-data/", "")
	if err == nil {
		t.Fatalf("expected SSRF block, got success: out=%q", out)
	}
	// validateDownloadURL phrasing: "download from private/loopback IP is not allowed"
	if !strings.Contains(err.Error(), "not allowed") {
		t.Errorf("error should mention 'not allowed'; got %q", err.Error())
	}
	if out != "" {
		t.Errorf("expected empty base64 on SSRF block, got %d chars", len(out))
	}
}

func TestDownloadInjectedImageBase64_RedirectCapped(t *testing.T) {
	skipURLValidation(t)
	// Server that always redirects to itself — drives the >=10 redirect cap.
	var ts *httptest.Server
	ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, ts.URL+"/again", http.StatusFound)
	}))
	defer ts.Close()

	out, err := downloadInjectedImageBase64(context.Background(), ts.URL+"/start", "")
	if err == nil {
		t.Fatalf("expected redirect-cap error, got success: out=%q", out)
	}
	if !strings.Contains(err.Error(), "redirect") {
		t.Errorf("error should mention 'redirect'; got %q", err.Error())
	}
	if out != "" {
		t.Errorf("expected empty base64, got %d chars", len(out))
	}
}

func TestDownloadInjectedImageBase64_StripsAuthOnCrossHostRedirect(t *testing.T) {
	skipURLValidation(t)
	// Second server records whether the Authorization header arrives on the
	// redirected request. Two distinct httptest.Servers share 127.0.0.1 but
	// listen on different ports — using URL.Host (with port) for the same-
	// host check makes this register as a cross-host redirect.
	var gotAuthOnSecond string
	var secondHit bool
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondHit = true
		gotAuthOnSecond = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "image/png")
		w.Write([]byte("img"))
	}))
	defer second.Close()
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, second.URL, http.StatusFound)
	}))
	defer first.Close()
	_, err := downloadInjectedImageBase64(context.Background(), first.URL, "Bearer secret-token")
	if err != nil {
		t.Fatalf("download failed: %v", err)
	}
	if !secondHit {
		t.Fatal("expected second server to be hit after redirect")
	}
	if gotAuthOnSecond != "" {
		t.Errorf("Authorization header leaked across hosts: got %q, want \"\"", gotAuthOnSecond)
	}
}

func TestDownloadInjectedImageBase64_KeepsAuthOnSameHostRedirect(t *testing.T) {
	skipURLValidation(t)
	// Single httptest server with two paths so the redirect stays on the
	// same host:port — exercises the auth-preserved branch of the same-host
	// check.
	var gotAuthOnRedirect string
	var redirected bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/start":
			http.Redirect(w, r, "/final", http.StatusFound)
		case "/final":
			redirected = true
			gotAuthOnRedirect = r.Header.Get("Authorization")
			w.Header().Set("Content-Type", "image/png")
			w.Write([]byte("img"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	_, err := downloadInjectedImageBase64(context.Background(), srv.URL+"/start", "Bearer secret-token")
	if err != nil {
		t.Fatalf("download failed: %v", err)
	}
	if !redirected {
		t.Fatal("expected /final to be hit after same-host redirect")
	}
	if gotAuthOnRedirect != "Bearer secret-token" {
		t.Errorf("Authorization should pass through on same-host redirect; got %q", gotAuthOnRedirect)
	}
}

func TestDownloadInjectedImageBase64_HTMLBodyRejected(t *testing.T) {
	skipURLValidation(t)
	// Slack/Feishu auth-failure pattern: 200 OK with text/html login page.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte("<html><body>Sign in</body></html>"))
	}))
	defer ts.Close()

	out, err := downloadInjectedImageBase64(context.Background(), ts.URL+"/image.png", "Bearer bad-token")
	if err == nil {
		t.Fatalf("expected HTML rejection, got success: out=%q", out)
	}
	if !strings.Contains(err.Error(), "html") && !strings.Contains(err.Error(), "auth") {
		t.Errorf("error should mention html/auth; got %q", err.Error())
	}
	if out != "" {
		t.Errorf("expected empty base64, got %d chars", len(out))
	}
}
