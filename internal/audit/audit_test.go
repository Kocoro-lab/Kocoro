package audit

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNewAuditLogger(t *testing.T) {
	dir := t.TempDir()
	logDir := filepath.Join(dir, "logs")

	logger, err := NewAuditLogger(logDir)
	if err != nil {
		t.Fatalf("NewAuditLogger() error: %v", err)
	}
	defer logger.Close()

	// Directory should be created
	if _, err := os.Stat(logDir); os.IsNotExist(err) {
		t.Error("log directory was not created")
	}

	// File should exist
	logPath := filepath.Join(logDir, "audit.log")
	if _, err := os.Stat(logPath); os.IsNotExist(err) {
		t.Error("audit.log was not created")
	}
}

func TestNewAuditLogger_EmptyDir(t *testing.T) {
	_, err := NewAuditLogger("")
	if err == nil {
		t.Error("expected error for empty logDir")
	}
}

func TestAuditLogger_Log(t *testing.T) {
	dir := t.TempDir()
	logger, err := NewAuditLogger(dir)
	if err != nil {
		t.Fatalf("NewAuditLogger() error: %v", err)
	}

	entry := AuditEntry{
		Timestamp:     time.Date(2025, 1, 15, 10, 30, 0, 0, time.UTC),
		SessionID:     "test-session-123",
		ToolName:      "bash",
		InputSummary:  "ls -la /tmp",
		OutputSummary: "total 0\ndrwxrwxrwt  2 root root",
		Decision:      "allow",
		Approved:      true,
		DurationMs:    42,
	}

	logger.Log(entry)
	logger.Close()

	// Read back
	logPath := filepath.Join(dir, "audit.log")
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("failed to read audit.log: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d", len(lines))
	}

	var decoded AuditEntry
	if err := json.Unmarshal([]byte(lines[0]), &decoded); err != nil {
		t.Fatalf("failed to parse JSON line: %v", err)
	}

	if decoded.SessionID != "test-session-123" {
		t.Errorf("SessionID = %q, want %q", decoded.SessionID, "test-session-123")
	}
	if decoded.ToolName != "bash" {
		t.Errorf("ToolName = %q, want %q", decoded.ToolName, "bash")
	}
	if decoded.Decision != "allow" {
		t.Errorf("Decision = %q, want %q", decoded.Decision, "allow")
	}
	if decoded.DurationMs != 42 {
		t.Errorf("DurationMs = %d, want %d", decoded.DurationMs, 42)
	}
}

func TestAuditLogger_MultipleEntries(t *testing.T) {
	dir := t.TempDir()
	logger, err := NewAuditLogger(dir)
	if err != nil {
		t.Fatalf("NewAuditLogger() error: %v", err)
	}

	for i := range 5 {
		logger.Log(AuditEntry{
			Timestamp:  time.Now(),
			SessionID:  "session",
			ToolName:   "bash",
			Decision:   "allow",
			DurationMs: int64(i),
		})
	}
	logger.Close()

	data, err := os.ReadFile(filepath.Join(dir, "audit.log"))
	if err != nil {
		t.Fatalf("failed to read: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 5 {
		t.Errorf("expected 5 lines, got %d", len(lines))
	}
}

func TestAuditLogger_AppendMode(t *testing.T) {
	dir := t.TempDir()

	// Write first entry
	logger1, err := NewAuditLogger(dir)
	if err != nil {
		t.Fatalf("NewAuditLogger() error: %v", err)
	}
	logger1.Log(AuditEntry{SessionID: "first"})
	logger1.Close()

	// Open again and write second entry
	logger2, err := NewAuditLogger(dir)
	if err != nil {
		t.Fatalf("NewAuditLogger() error: %v", err)
	}
	logger2.Log(AuditEntry{SessionID: "second"})
	logger2.Close()

	data, err := os.ReadFile(filepath.Join(dir, "audit.log"))
	if err != nil {
		t.Fatalf("failed to read: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Errorf("expected 2 lines (append mode), got %d", len(lines))
	}
}

func TestAuditLogger_RedactsOnWrite(t *testing.T) {
	dir := t.TempDir()
	logger, err := NewAuditLogger(dir)
	if err != nil {
		t.Fatalf("NewAuditLogger() error: %v", err)
	}

	logger.Log(AuditEntry{
		InputSummary:  "curl -H 'Authorization: Bearer mytoken123' https://api.example.com",
		OutputSummary: "API_KEY=supersecretkey123",
	})
	logger.Close()

	data, err := os.ReadFile(filepath.Join(dir, "audit.log"))
	if err != nil {
		t.Fatalf("failed to read: %v", err)
	}

	content := string(data)
	if strings.Contains(content, "mytoken123") {
		t.Error("Bearer token was not redacted")
	}
	if strings.Contains(content, "supersecretkey123") {
		t.Error("API_KEY value was not redacted")
	}
	if !strings.Contains(content, "[REDACTED]") {
		t.Error("expected [REDACTED] placeholder in output")
	}
}

func TestAuditLogger_TruncatesLongSummaries(t *testing.T) {
	dir := t.TempDir()
	logger, err := NewAuditLogger(dir)
	if err != nil {
		t.Fatalf("NewAuditLogger() error: %v", err)
	}

	longInput := strings.Repeat("x", 1000)
	logger.Log(AuditEntry{
		InputSummary:  longInput,
		OutputSummary: longInput,
	})
	logger.Close()

	data, err := os.ReadFile(filepath.Join(dir, "audit.log"))
	if err != nil {
		t.Fatalf("failed to read: %v", err)
	}

	var decoded AuditEntry
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(data))), &decoded); err != nil {
		t.Fatalf("failed to parse: %v", err)
	}

	if len(decoded.InputSummary) > maxSummaryLen {
		t.Errorf("InputSummary len = %d, want <= %d", len(decoded.InputSummary), maxSummaryLen)
	}
	if len(decoded.OutputSummary) > maxSummaryLen {
		t.Errorf("OutputSummary len = %d, want <= %d", len(decoded.OutputSummary), maxSummaryLen)
	}
	if !strings.HasSuffix(decoded.InputSummary, "...") {
		t.Error("truncated summary should end with ...")
	}
}

func TestRedactSecrets_AWSAccessKey(t *testing.T) {
	input := "using key AKIAIOSFODNN7EXAMPLE for auth"
	got := RedactSecrets(input)
	if strings.Contains(got, "AKIAIOSFODNN7EXAMPLE") {
		t.Error("AWS access key was not redacted")
	}
	if !strings.Contains(got, "[REDACTED]") {
		t.Error("expected [REDACTED] placeholder")
	}
}

func TestRedactSecrets_JWT(t *testing.T) {
	jwt := "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIiwibmFtZSI6IkpvaG4gRG9lIiwiaWF0IjoxNTE2MjM5MDIyfQ.SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV_adQssw5c"
	input := "token: " + jwt
	got := RedactSecrets(input)
	if strings.Contains(got, "eyJhbGciOiJIUzI1NiI") {
		t.Error("JWT was not redacted")
	}
}

func TestRedactSecrets_SKKey(t *testing.T) {
	input := "key is sk-abcdefghijklmnopqrstuvwxyz"
	got := RedactSecrets(input)
	if strings.Contains(got, "sk-abcdefghijklmnopqrstuvwxyz") {
		t.Error("sk- key was not redacted")
	}
}

func TestRedactSecrets_KeyDashKey(t *testing.T) {
	input := "api key-abcdefghijklmnopqrstuvwxyz"
	got := RedactSecrets(input)
	if strings.Contains(got, "key-abcdefghijklmnopqrstuvwxyz") {
		t.Error("key- pattern was not redacted")
	}
}

func TestRedactSecrets_BearerToken(t *testing.T) {
	input := "Authorization: Bearer abc123xyz"
	got := RedactSecrets(input)
	if strings.Contains(got, "abc123xyz") {
		t.Error("Bearer token was not redacted")
	}
}

func TestRedactSecrets_PEMMarker(t *testing.T) {
	input := "-----BEGIN RSA PRIVATE KEY-----"
	got := RedactSecrets(input)
	if strings.Contains(got, "-----BEGIN") {
		t.Error("PEM marker was not redacted")
	}
}

// TestRedactSecrets_URLEmbeddedCredentials covers the case where git's
// auth-failure stderr echoes back a remote URL configured with embedded
// credentials (e.g. `git config url."https://user:token@github.com/".insteadOf`).
// That stderr lands in audit.log via auditHTTPOpError's output_summary on
// install failure, so we must scrub credentials before persisting.
func TestRedactSecrets_URLEmbeddedCredentials(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		mustHave  string // substring that must remain (proves scheme+host kept)
		mustHide  string // substring that must NOT appear (proves creds gone)
	}{
		{
			name:     "https with token",
			input:    "fatal: unable to access 'https://wayland:ghp_secrettoken123@github.com/foo/bar.git/': Forbidden",
			mustHave: "https://[REDACTED]@github.com/foo/bar.git",
			mustHide: "ghp_secrettoken123",
		},
		{
			name:     "http with password",
			input:    "remote: http://admin:hunter2@internal.corp/repo.git",
			mustHave: "http://[REDACTED]@internal.corp/repo.git",
			mustHide: "hunter2",
		},
		{
			name:     "username also hidden",
			input:    "url is https://secret_user:tok@example.com/path",
			mustHave: "[REDACTED]",
			mustHide: "secret_user",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := RedactSecrets(tt.input)
			if !strings.Contains(got, tt.mustHave) {
				t.Errorf("RedactSecrets(%q) = %q; expected substring %q", tt.input, got, tt.mustHave)
			}
			if strings.Contains(got, tt.mustHide) {
				t.Errorf("RedactSecrets(%q) = %q; must not contain %q", tt.input, got, tt.mustHide)
			}
		})
	}
}

// TestRedactSecrets_URLWithoutCredentials verifies bare URLs (no `user:pass@`
// portion) pass through untouched. Same regex shape happens to live in our
// installFromRepo source comment, so we must not silently rewrite plain URLs
// in log output.
func TestRedactSecrets_URLWithoutCredentials(t *testing.T) {
	cases := []string{
		"https://github.com/anthropics/skills.git",
		"http://localhost:7533/skills/install/docx",
		"https://example.com/path?query=value",
	}
	for _, in := range cases {
		t.Run(in, func(t *testing.T) {
			got := RedactSecrets(in)
			if got != in {
				t.Errorf("RedactSecrets(%q) = %q; want unchanged", in, got)
			}
		})
	}
}

func TestRedactSecrets_EnvVarAssignments(t *testing.T) {
	tests := []struct {
		input    string
		contains string
	}{
		{"API_KEY=mysecret123", "mysecret123"},
		{"DB_PASSWORD=hunter2", "hunter2"},
		{"AUTH_TOKEN=tok_abc123", "tok_abc123"},
		{"AWS_SECRET=very_secret", "very_secret"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := RedactSecrets(tt.input)
			if strings.Contains(got, tt.contains) {
				t.Errorf("RedactSecrets(%q) still contains %q", tt.input, tt.contains)
			}
		})
	}
}

// TestRedactSecrets_FeishuAppSecretInHTTPBody covers the exact production
// shape the JSON-secret pattern was added for: the `http` tool serializes its
// request body into the tool-args JSON, so a Feishu/Lark App Secret arrives
// backslash-escaped inside a JSON-string field (\"app_secret\":\"...\"). The
// original regex only matched UNescaped quotes and let this leak verbatim.
func TestRedactSecrets_FeishuAppSecretInHTTPBody(t *testing.T) {
	// As produced by fc.ArgumentsString() for an http POST whose body is JSON.
	input := `{"method":"POST","url":"http://localhost:7533/channels/feishu/app-installs","body":"{\"app_id\":\"cli_x\",\"app_secret\":\"SeCrEt0123456789abcdef\"}"}`
	got := RedactSecrets(input)
	if strings.Contains(got, "SeCrEt0123456789abcdef") {
		t.Errorf("Feishu app_secret leaked through redaction: %s", got)
	}
	if !strings.Contains(got, "[REDACTED]") {
		t.Errorf("expected [REDACTED] placeholder: %s", got)
	}
	// app_id is not secret-shaped and must stay visible for debuggability.
	if !strings.Contains(got, "cli_x") {
		t.Errorf("non-secret app_id should be preserved: %s", got)
	}
}

// TestRedactSecrets_EmbeddedQuoteInValue covers a JSON secret value that itself
// contains an escaped quote (e.g. a password). The value matcher must consume
// escaped characters so the WHOLE value is redacted — not just the head up to
// the first \", which would leak the tail.
func TestRedactSecrets_EmbeddedQuoteInValue(t *testing.T) {
	cases := []struct{ in, leak string }{
		{`{"app_secret":"HEAD\"TAILSECRET"}`, "TAILSECRET"},
		{`{"password":"P1\"P2\"P3"}`, "P3"},
		{`{"access_token":"x\"yz9"}`, "yz9"},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got := RedactSecrets(c.in)
			if strings.Contains(got, c.leak) {
				t.Errorf("RedactSecrets(%q) = %q; leaked %q after embedded quote", c.in, got, c.leak)
			}
			if !strings.Contains(got, "[REDACTED]") {
				t.Errorf("RedactSecrets(%q) = %q; expected [REDACTED]", c.in, got)
			}
		})
	}
}

// TestRedactSecrets_HyphenatedKey covers header-style secret keys whose name
// contains a hyphen (x-api-key, x-auth-token). The original [a-z0-9_] key
// class excluded '-', so these never matched.
func TestRedactSecrets_HyphenatedKey(t *testing.T) {
	cases := []string{
		`{"x-api-key":"abcdef0123456789secretval"}`,
		`{"x-auth-token":"abcdef0123456789secretval"}`,
		`{\"x-api-key\":\"abcdef0123456789secretval\"}`,
	}
	for _, in := range cases {
		t.Run(in, func(t *testing.T) {
			got := RedactSecrets(in)
			if strings.Contains(got, "abcdef0123456789secretval") {
				t.Errorf("RedactSecrets(%q) leaked secret: %s", in, got)
			}
		})
	}
}

// TestAuditLogger_RedactsBeforeTruncate locks the ordering: redaction must run
// over the FULL input before truncation, so a delimiter-anchored secret regex
// still sees the closing quote. A truncate-first ordering chops the delimiter
// of a secret whose value straddles the cap and leaks the value head.
func TestAuditLogger_RedactsBeforeTruncate(t *testing.T) {
	dir := t.TempDir()
	logger, err := NewAuditLogger(dir)
	if err != nil {
		t.Fatalf("NewAuditLogger() error: %v", err)
	}
	// Opening quote + value head land before the cap; the closing quote falls
	// past it (value is longer than maxSummaryLen).
	secret := "LEAKHEAD" + strings.Repeat("z", maxSummaryLen)
	logger.Log(AuditEntry{InputSummary: `{"app_secret":"` + secret + `"}`})
	logger.Close()

	data, err := os.ReadFile(filepath.Join(dir, "audit.log"))
	if err != nil {
		t.Fatalf("failed to read: %v", err)
	}
	if strings.Contains(string(data), "LEAKHEAD") {
		t.Errorf("secret head leaked — redaction must run before truncation: %s", data)
	}
}

func TestRedactSecrets_NoFalsePositives(t *testing.T) {
	safe := []string{
		"running ls -la",
		"file: readme.md",
		"go build ./...",
		"git status",
	}

	for _, input := range safe {
		t.Run(input, func(t *testing.T) {
			got := RedactSecrets(input)
			if got != input {
				t.Errorf("RedactSecrets(%q) = %q, should be unchanged", input, got)
			}
		})
	}
}

func TestRedactSecrets_EmptyString(t *testing.T) {
	got := RedactSecrets("")
	if got != "" {
		t.Errorf("RedactSecrets(\"\") = %q, want \"\"", got)
	}
}

func TestTruncate(t *testing.T) {
	tests := []struct {
		input  string
		maxLen int
		want   string
	}{
		{"hello", 10, "hello"},
		{"hello", 5, "hello"},
		{"hello world", 8, "hello..."},
		{"ab", 3, "ab"},
		{"abcdef", 6, "abcdef"},
		{"abcdefg", 6, "abc..."},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := truncate(tt.input, tt.maxLen)
			if got != tt.want {
				t.Errorf("truncate(%q, %d) = %q, want %q", tt.input, tt.maxLen, got, tt.want)
			}
		})
	}
}

// TestAuditEntry_ApprovedAlwaysPresent locks the invariant that
// `approved` is always present in the JSON output — including when the
// tool call was denied (Approved=false). Security tooling that greps
// the audit log distinguishes permitted from denied calls by this
// field; dropping it under omitempty would make denial entries
// indistinguishable from non-tool events (force_stop, etc.).
func TestAuditEntry_ApprovedAlwaysPresent(t *testing.T) {
	tests := []struct {
		name  string
		entry AuditEntry
	}{
		{
			"approved true",
			AuditEntry{
				Timestamp: time.Unix(0, 0).UTC(),
				SessionID: "s", ToolName: "bash", Approved: true,
			},
		},
		{
			"approved false (denied)",
			AuditEntry{
				Timestamp: time.Unix(0, 0).UTC(),
				SessionID: "s", ToolName: "bash", Approved: false,
			},
		},
		{
			"non-tool event (force_stop)",
			AuditEntry{
				Timestamp: time.Unix(0, 0).UTC(),
				SessionID: "s", Event: "force_stop", Approved: false,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data, err := json.Marshal(tt.entry)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if !strings.Contains(string(data), `"approved":`) {
				t.Errorf("approved field missing from JSON output: %s", data)
			}
		})
	}
}

// Cache-summary entries (event="cache_summary") carry per-Run cache health
// metrics. The JSON schema must round-trip without loss so audit-log
// consumers can build dashboards. See cache-action-plan §1.3.
func TestAuditLogger_CacheSummary_RoundTrips(t *testing.T) {
	dir := t.TempDir()
	logger, err := NewAuditLogger(dir)
	if err != nil {
		t.Fatalf("NewAuditLogger: %v", err)
	}

	entry := AuditEntry{
		Timestamp:           time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC),
		SessionID:           "session-xyz",
		Event:               "cache_summary",
		Source:              "oneshot_cli",
		Calls:               11,
		CacheCreationTokens: 82823,
		CacheReadTokens:     421719,
		CER:                 5.09,
		TailCERLast3:        63.1,
		WarmStart:           false,
	}
	logger.Log(entry)
	logger.Close()

	data, err := os.ReadFile(filepath.Join(dir, "audit.log"))
	if err != nil {
		t.Fatalf("read audit.log: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d", len(lines))
	}

	var decoded AuditEntry
	if err := json.Unmarshal([]byte(lines[0]), &decoded); err != nil {
		t.Fatalf("parse JSON: %v", err)
	}

	if decoded.Event != "cache_summary" {
		t.Errorf("Event = %q, want %q", decoded.Event, "cache_summary")
	}
	if decoded.Source != "oneshot_cli" {
		t.Errorf("Source = %q, want %q", decoded.Source, "oneshot_cli")
	}
	if decoded.Calls != 11 {
		t.Errorf("Calls = %d, want 11", decoded.Calls)
	}
	if decoded.CacheCreationTokens != 82823 {
		t.Errorf("cc = %d, want 82823", decoded.CacheCreationTokens)
	}
	if decoded.CacheReadTokens != 421719 {
		t.Errorf("cr = %d, want 421719", decoded.CacheReadTokens)
	}
	if decoded.CER != 5.09 {
		t.Errorf("CER = %f, want 5.09", decoded.CER)
	}
	if decoded.TailCERLast3 != 63.1 {
		t.Errorf("TailCERLast3 = %f, want 63.1", decoded.TailCERLast3)
	}
	if decoded.WarmStart {
		t.Error("WarmStart should be false")
	}

	// Sanity: the JSON must include the cache_summary discriminator so
	// `grep '"event":"cache_summary"' ~/.shannon/logs/audit.log` works.
	if !strings.Contains(lines[0], `"event":"cache_summary"`) {
		t.Error("audit line should contain event discriminator for grep filtering")
	}
}

// The CER cliff (zero cache reads) is the single most important diagnostic
// signal cache_summary exists to surface. omitempty on float64 would silently
// elide cer=0 / tail_cer_last3=0, hiding the failure mode from dashboards.
// MarshalJSON must force the field onto every cache_summary row.
func TestAuditLogger_CacheSummary_CERZero_StillEmitted(t *testing.T) {
	dir := t.TempDir()
	logger, err := NewAuditLogger(dir)
	if err != nil {
		t.Fatalf("NewAuditLogger: %v", err)
	}
	logger.Log(AuditEntry{
		Timestamp:           time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC),
		SessionID:           "session-cliff",
		Event:               "cache_summary",
		Source:              "tui",
		Calls:               5,
		CacheCreationTokens: 12000,
		CacheReadTokens:     0,
		CER:                 0,
		TailCERLast3:        0,
		WarmStart:           false,
	})
	logger.Close()

	data, err := os.ReadFile(filepath.Join(dir, "audit.log"))
	if err != nil {
		t.Fatalf("read audit.log: %v", err)
	}
	line := strings.TrimSpace(string(data))
	for _, k := range []string{`"cer":0`, `"tail_cer_last3":0`} {
		if !strings.Contains(line, k) {
			t.Errorf("cache_summary row must contain %s for cliff detection; got: %s", k, line)
		}
	}

	var decoded AuditEntry
	if err := json.Unmarshal([]byte(line), &decoded); err != nil {
		t.Fatalf("parse JSON: %v", err)
	}
	if decoded.CER != 0 || decoded.TailCERLast3 != 0 {
		t.Errorf("CER round-trip: got CER=%v TailCERLast3=%v, want both 0", decoded.CER, decoded.TailCERLast3)
	}
}

// WarmStart=true is the cross-session cache-reuse signal. Because
// `warm_start` is omitempty, a regression that always wrote false would
// look identical to a regression that dropped the field entirely. Lock
// the wire-level true case so the field actually appears in JSON.
func TestAuditLogger_CacheSummary_WarmStartTrue_RoundTrips(t *testing.T) {
	dir := t.TempDir()
	logger, err := NewAuditLogger(dir)
	if err != nil {
		t.Fatalf("NewAuditLogger: %v", err)
	}
	logger.Log(AuditEntry{
		Timestamp:           time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC),
		SessionID:           "session-warm",
		Event:               "cache_summary",
		Source:              "tui",
		Calls:               7,
		CacheCreationTokens: 0,
		CacheReadTokens:     31200,
		CER:                 4.2,
		TailCERLast3:        12.5,
		WarmStart:           true,
	})
	logger.Close()

	data, err := os.ReadFile(filepath.Join(dir, "audit.log"))
	if err != nil {
		t.Fatalf("read audit.log: %v", err)
	}
	line := strings.TrimSpace(string(data))
	if !strings.Contains(line, `"warm_start":true`) {
		t.Errorf("cache_summary row must contain warm_start:true; got: %s", line)
	}

	var decoded AuditEntry
	if err := json.Unmarshal([]byte(line), &decoded); err != nil {
		t.Fatalf("parse JSON: %v", err)
	}
	if !decoded.WarmStart {
		t.Errorf("WarmStart round-trip: got false, want true")
	}
}

// A regular tool-call entry must NOT serialize the cache-summary fields,
// so per-source dashboards don't get polluted with zero values.
func TestAuditLogger_ToolCallOmitsCacheSummaryFields(t *testing.T) {
	dir := t.TempDir()
	logger, err := NewAuditLogger(dir)
	if err != nil {
		t.Fatalf("NewAuditLogger: %v", err)
	}
	logger.Log(AuditEntry{
		Timestamp: time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC),
		SessionID: "s",
		ToolName:  "bash",
		Approved:  true,
	})
	logger.Close()

	data, err := os.ReadFile(filepath.Join(dir, "audit.log"))
	if err != nil {
		t.Fatalf("read audit.log: %v", err)
	}
	line := strings.TrimSpace(string(data))
	for _, k := range []string{`"calls"`, `"source"`, `"cer"`, `"tail_cer_last3"`, `"warm_start"`} {
		if strings.Contains(line, k) {
			t.Errorf("tool-call entry leaked cache-summary field %q in %s", k, line)
		}
	}
}
