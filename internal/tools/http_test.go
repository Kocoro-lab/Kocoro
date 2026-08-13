package tools

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestHTTP_Info(t *testing.T) {
	tool := &HTTPTool{}
	info := tool.Info()
	if info.Name != "http" {
		t.Errorf("expected name 'http', got %q", info.Name)
	}
	if !containsString(info.Required, "url") {
		t.Errorf("expected Required to contain 'url', got %v", info.Required)
	}
	if !containsString(info.Required, "description") {
		t.Errorf("expected Required to contain 'description' (PR 7), got %v", info.Required)
	}
}

func TestHTTP_InvalidArgs(t *testing.T) {
	tool := &HTTPTool{}
	result, err := tool.Run(context.Background(), `not valid json`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Error("expected error result for invalid JSON")
	}
}

func TestHTTP_GET(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Errorf("expected GET, got %s", r.Method)
		}
		w.Header().Set("X-Test", "hello")
		w.WriteHeader(200)
		w.Write([]byte("ok"))
	}))
	defer srv.Close()

	tool := &HTTPTool{}
	result, err := tool.Run(context.Background(), `{"url": "`+srv.URL+`","description":"test get"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error result: %s", result.Content)
	}
	if !contains(result.Content, "200") {
		t.Errorf("expected status 200 in output, got: %s", result.Content)
	}
	if !contains(result.Content, "ok") {
		t.Errorf("expected body 'ok' in output, got: %s", result.Content)
	}
}

func TestHTTP_POST(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("expected POST, got %s", r.Method)
		}
		w.WriteHeader(201)
		w.Write([]byte("created"))
	}))
	defer srv.Close()

	tool := &HTTPTool{}
	result, err := tool.Run(context.Background(), `{"url": "`+srv.URL+`", "method": "POST", "body": "test","description":"test post"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error result: %s", result.Content)
	}
	if !contains(result.Content, "201") {
		t.Errorf("expected status 201 in output, got: %s", result.Content)
	}
}

func TestHTTP_PostDispatchResponseLossIsOutcomeUnknownForMutation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		_ = conn.Close()
	}))
	defer srv.Close()

	result, err := (&HTTPTool{}).Run(context.Background(), `{"url":"`+srv.URL+`","method":"POST","body":"create","description":"test lost response"}`)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !result.IsError || !result.SideEffectOutcomeUnknown {
		t.Fatalf("result = %#v, want outcome unknown", result)
	}
	if result.IsRetryable || result.ErrorCategory != "" {
		t.Fatalf("outcome unknown must not be retryable: %#v", result)
	}
	if contains(result.Content, srv.URL) {
		t.Fatalf("outcome-unknown diagnostic leaked request URL: %q", result.Content)
	}
}

func TestHTTP_PostDispatchResponseLossForReadRemainsTransient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		_ = conn.Close()
	}))
	defer srv.Close()

	result, err := (&HTTPTool{}).Run(context.Background(), `{"url":"`+srv.URL+`","method":"GET","description":"test lost read"}`)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !result.IsError || result.SideEffectOutcomeUnknown {
		t.Fatalf("result = %#v, want ordinary read error", result)
	}
	if !result.IsRetryable || result.ErrorCategory != "transient" {
		t.Fatalf("read transport loss should remain transient: %#v", result)
	}
}

func TestHTTP_PreDispatchConnectFailureForMutationRemainsTransient(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	url := "http://" + listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}

	result, runErr := (&HTTPTool{}).Run(context.Background(), `{"url":"`+url+`","method":"POST","body":"create","description":"test connect failure"}`)
	if runErr != nil {
		t.Fatalf("Run: %v", runErr)
	}
	if !result.IsError || result.SideEffectOutcomeUnknown {
		t.Fatalf("result = %#v, want pre-dispatch tool error", result)
	}
	if !result.IsRetryable || result.ErrorCategory != "transient" {
		t.Fatalf("connect failure should remain transient: %#v", result)
	}
}

func TestHTTP_MutationWithKnownStatusAndTruncatedBodyIsNotOutcomeUnknown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("short"))
	}))
	defer srv.Close()

	result, err := (&HTTPTool{}).Run(context.Background(), `{"url":"`+srv.URL+`","method":"POST","body":"create","description":"test truncated body"}`)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !result.IsError || result.SideEffectOutcomeUnknown {
		t.Fatalf("result = %#v, want definitive response-read error", result)
	}
	if result.IsRetryable || result.ErrorCategory != "" {
		t.Fatalf("known mutation response must not encourage replay: %#v", result)
	}
	if !contains(result.Content, "returned status 201") {
		t.Fatalf("result must retain known status: %s", result.Content)
	}
}

func TestHTTP_StatusCodeErrorFlag(t *testing.T) {
	// Method-aware IsError semantics:
	//   5xx                       → IsError always (server failure)
	//   GET/HEAD/OPTIONS 4xx       → IsError EXCEPT 404 and 410 (polling-exempt)
	//                                401/403/429 etc. are real failures — auth,
	//                                rate-limit — the loop detector should see.
	//   POST/PUT/PATCH/DELETE 4xx  → IsError always (mutations don't 4xx legitimately)
	tests := []struct {
		name           string
		method         string
		status         int
		body           string
		wantIsError    bool
		wantStatusText string
	}{
		// Reads: only 404/410 are polling-exempt. Everything else 4xx+ IS error.
		{"GET 200", "GET", 200, "ok", false, "Status: 200"},
		{"GET 400", "GET", 400, "bad query", true, "Status: 400"},
		{"GET 401 auth fail", "GET", 401, "unauth", true, "Status: 401"},
		{"GET 403 forbidden", "GET", 403, "forbidden", true, "Status: 403"},
		{"GET 404 polling-exempt", "GET", 404, "not found", false, "Status: 404"},
		{"GET 410 polling-exempt", "GET", 410, "gone", false, "Status: 410"},
		{"GET 429 rate-limit", "GET", 429, "slow down", true, "Status: 429"},
		{"GET 500", "GET", 500, "boom", true, "Status: 500"},
		{"GET 502", "GET", 502, "bad gw", true, "Status: 502"},
		{"HEAD 404 polling-exempt", "HEAD", 404, "", false, "Status: 404"},
		{"HEAD 401", "HEAD", 401, "", true, "Status: 401"},
		// Mutations: all 4xx+ IS error (real validation / auth / routing bug).
		{"POST 201", "POST", 201, "created", false, "Status: 201"},
		{"POST 400 malformed body", "POST", 400, "bad request", true, "Status: 400"},
		{"POST 401", "POST", 401, "unauth", true, "Status: 401"},
		{"POST 404 wrong endpoint", "POST", 404, "not found", true, "Status: 404"},
		{"PUT 400", "PUT", 400, "bad", true, "Status: 400"},
		{"PATCH 404", "PATCH", 404, "not found", true, "Status: 404"},
		{"DELETE 400", "DELETE", 400, "bad", true, "Status: 400"},
		{"DELETE 404", "DELETE", 404, "not found", true, "Status: 404"},
		{"POST 500", "POST", 500, "boom", true, "Status: 500"},
		// Empty method → treated as GET (conservative default).
		{"empty method treated as GET 404 exempt", "", 404, "not found", false, "Status: 404"},
		{"empty method treated as GET 401 error", "", 401, "unauth", true, "Status: 401"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
				w.Write([]byte(tt.body))
			}))
			defer srv.Close()

			tool := &HTTPTool{}
			var argsJSON string
			if tt.method == "" {
				argsJSON = `{"url": "` + srv.URL + `","description":"test status"}`
			} else {
				argsJSON = `{"url": "` + srv.URL + `", "method": "` + tt.method + `", "body": "x","description":"test status"}`
			}
			result, err := tool.Run(context.Background(), argsJSON)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if result.IsError != tt.wantIsError {
				t.Errorf("IsError = %v, want %v (content: %s)", result.IsError, tt.wantIsError, result.Content)
			}
			if !contains(result.Content, tt.wantStatusText) {
				t.Errorf("expected %q in output, got: %s", tt.wantStatusText, result.Content)
			}
		})
	}
}

func TestHTTP_RedirectNotError(t *testing.T) {
	// Target server returns 200 after redirect.
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte("final"))
	}))
	defer target.Close()

	// Redirector issues 302 pointing at target. Go's default client follows the redirect,
	// so the final observed response is 200 (IsError false). If a future change disables
	// redirect following, this test documents that a bare 302 is < 400 and thus not an error.
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer redirector.Close()

	tool := &HTTPTool{}
	result, err := tool.Run(context.Background(), `{"url": "`+redirector.URL+`","description":"test redirect"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Errorf("expected IsError=false for 302→200 chain, got true. content: %s", result.Content)
	}
	if !contains(result.Content, "Status: 200") {
		t.Errorf("expected final Status: 200 in output (redirect followed), got: %s", result.Content)
	}
}

func TestHTTP_InvalidURL(t *testing.T) {
	tool := &HTTPTool{}
	result, err := tool.Run(context.Background(), `{"url": "http://invalid.localhost.test:99999/nope","description":"test network error"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Error("expected error result for invalid URL")
	}
}

func TestHTTP_IsSafeArgs(t *testing.T) {
	tool := &HTTPTool{}
	tests := []struct {
		argsJSON string
		safe     bool
	}{
		{`{"url": "http://localhost:8080/api"}`, true},
		{`{"url": "http://127.0.0.1:3000/test"}`, true},
		{`{"url": "http://localhost/path", "method": "GET"}`, true},
		{`{"url": "http://localhost/path", "method": "POST"}`, false},
		{`{"url": "https://example.com/api"}`, false},
		{`{"url": "https://example.com/api", "method": "GET"}`, false},
		// GET with any body (inline or file) is not safe — could exfiltrate data.
		{`{"url": "http://localhost/api", "body": "x"}`, false},
		{`{"url": "http://localhost/api", "body_from_file": "/tmp/x"}`, false},
		{`not valid json`, false},
	}
	for _, tt := range tests {
		if tool.IsSafeArgs(tt.argsJSON) != tt.safe {
			t.Errorf("IsSafeArgs(%q) = %v, want %v", tt.argsJSON, !tt.safe, tt.safe)
		}
	}
}

func TestHTTP_BodyFromFile(t *testing.T) {
	// Payload contains every character that's painful to JSON-escape:
	// double quotes, backslashes, newlines, tabs, unicode, backticks.
	payload := "line1 \"with quotes\"\nline2 \\backslash\\\nline3\ttab\nline4 中文 — `backtick`\n"

	dir := t.TempDir()
	bodyPath := filepath.Join(dir, "payload.txt")
	if err := os.WriteFile(bodyPath, []byte(payload), 0644); err != nil {
		t.Fatalf("write payload: %v", err)
	}

	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "PUT" {
			t.Errorf("expected PUT, got %s", r.Method)
		}
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		got = string(b)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	tool := &HTTPTool{}
	argsJSON := `{"url": "` + srv.URL + `", "method": "PUT", "body_from_file": "` + bodyPath + `","description":"test file body"}`
	result, err := tool.Run(context.Background(), argsJSON)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error result: %s", result.Content)
	}
	if got != payload {
		t.Errorf("server received body %q, want %q", got, payload)
	}
}

func TestHTTP_BodyFromFile_SetsContentLength(t *testing.T) {
	// Without an explicit ContentLength, *os.File bodies would fall back to
	// chunked transfer encoding (stdlib only auto-detects length for
	// *strings.Reader / *bytes.Reader / *bytes.Buffer). We set it explicitly
	// from f.Stat() so strict HTTP/1.0 servers and proxies see a normal
	// Content-Length header.
	dir := t.TempDir()
	bodyPath := filepath.Join(dir, "payload.txt")
	payload := []byte("exactly thirty bytes of content")
	if err := os.WriteFile(bodyPath, payload, 0644); err != nil {
		t.Fatalf("write payload: %v", err)
	}
	wantLen := int64(len(payload))

	var gotLen int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotLen = r.ContentLength
		w.WriteHeader(200)
	}))
	defer srv.Close()

	tool := &HTTPTool{}
	argsJSON := `{"url": "` + srv.URL + `", "method": "POST", "body_from_file": "` + bodyPath + `","description":"test content length"}`
	result, err := tool.Run(context.Background(), argsJSON)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error result: %s", result.Content)
	}
	if gotLen != wantLen {
		t.Errorf("Content-Length = %d, want %d (chunked encoding indicates -1)", gotLen, wantLen)
	}
}

func TestHTTP_BodyFromFile_MutuallyExclusive(t *testing.T) {
	dir := t.TempDir()
	bodyPath := filepath.Join(dir, "payload.txt")
	if err := os.WriteFile(bodyPath, []byte("x"), 0644); err != nil {
		t.Fatalf("write payload: %v", err)
	}

	tool := &HTTPTool{}
	argsJSON := `{"url": "http://localhost/x", "method": "POST", "body": "inline", "body_from_file": "` + bodyPath + `","description":"test exclusive bodies"}`
	result, err := tool.Run(context.Background(), argsJSON)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Error("expected error when both body and body_from_file are set")
	}
	if !contains(result.Content, "mutually exclusive") {
		t.Errorf("expected 'mutually exclusive' message, got: %s", result.Content)
	}
}

func TestHTTP_BodyFromFile_Missing(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "does-not-exist.txt")

	tool := &HTTPTool{}
	argsJSON := `{"url": "http://localhost/x", "method": "POST", "body_from_file": "` + missing + `","description":"test missing body file"}`
	result, err := tool.Run(context.Background(), argsJSON)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Error("expected error for missing body_from_file")
	}
	if !contains(result.Content, "does not exist") {
		t.Errorf("expected 'does not exist' in error, got: %s", result.Content)
	}
}

func TestHTTP_BodyFromFile_Directory(t *testing.T) {
	dir := t.TempDir()

	tool := &HTTPTool{}
	argsJSON := `{"url": "http://localhost/x", "method": "POST", "body_from_file": "` + dir + `","description":"test directory body"}`
	result, err := tool.Run(context.Background(), argsJSON)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Error("expected error when body_from_file points at a directory")
	}
	if !contains(result.Content, "is a directory") {
		t.Errorf("expected 'is a directory' in error, got: %s", result.Content)
	}
}

func TestHTTP_BodyFromFile_RelativePathWithoutCWD(t *testing.T) {
	tool := &HTTPTool{}
	argsJSON := `{"url": "http://localhost/x", "method": "POST", "body_from_file": "relative/path.txt","description":"test relative body file"}`
	result, err := tool.Run(context.Background(), argsJSON)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Error("expected error for relative path without session CWD")
	}
	if !contains(result.Content, "absolute path") {
		t.Errorf("expected 'absolute path' guidance in error, got: %s", result.Content)
	}
}
