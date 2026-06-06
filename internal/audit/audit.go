package audit

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"
)

// AuditEntry represents a single audited agent event. Most entries
// describe tool calls (ToolName populated); non-tool entries (e.g.
// "force_stop" stops, run-level boundaries) set Event instead and leave
// ToolName empty.
//
// NOTE: Approved intentionally does NOT use omitempty. `approved:false`
// is the explicit marker for a tool call that was denied or rejected —
// security tooling that greps the audit log distinguishes permitted vs
// denied calls by that field, and omitempty would drop the denial signal.
// Non-tool events (Event != "") serialize with approved:false too, which
// is cosmetic noise only — the Event tag disambiguates them.
type AuditEntry struct {
	Timestamp     time.Time `json:"timestamp"`
	SessionID     string    `json:"session_id"`
	Event         string    `json:"event,omitempty"`
	ToolName      string    `json:"tool_name,omitempty"`
	InputSummary  string    `json:"input_summary,omitempty"`
	OutputSummary string    `json:"output_summary,omitempty"`
	Decision      string    `json:"decision,omitempty"`
	Approved      bool      `json:"approved"`
	DurationMs    int64     `json:"duration_ms,omitempty"`
	// Cost fields (populated when tool reports usage, e.g. gateway tools that
	// call xAI Grok / SerpAPI). Omitted when the tool does not return usage.
	// TotalTokens is the aggregate; for tools that only report a flat count
	// (SERP APIs, current Shannon Cloud schema) only TotalTokens is populated.
	// LLM-backed tools that expose input/output split also fill InputTokens/OutputTokens.
	InputTokens         int     `json:"input_tokens,omitempty"`
	OutputTokens        int     `json:"output_tokens,omitempty"`
	TotalTokens         int     `json:"total_tokens,omitempty"`
	CostUSD             float64 `json:"cost_usd,omitempty"`
	Model               string  `json:"model,omitempty"`
	CacheReadTokens     int     `json:"cache_read_tokens,omitempty"`
	CacheCreationTokens int     `json:"cache_creation_tokens,omitempty"`

	// Cache-summary fields. Populated only when Event=="cache_summary"
	// (one entry per AgentLoop.Run). All other entries leave these zero
	// (omitempty keeps the schema clean for tool-call rows).
	// Source carries the cache_source tag (oneshot_cli/tui/slack/...) for
	// per-source dashboards. CER and TailCERLast3 are doubles; the Cache*
	// fields above hold the absolute totals.
	//
	// CER and TailCERLast3 keep omitempty here for tool-call rows, but
	// MarshalJSON below forces them onto cache_summary rows even when 0.0
	// — that zero IS the worst-case cliff signal we want dashboards to
	// catch (jq 'select(.cer < 1)' / WHERE cer IS NOT NULL would otherwise
	// miss the most important row).
	Calls        int     `json:"calls,omitempty"`
	Source       string  `json:"source,omitempty"`
	CER          float64 `json:"cer,omitempty"`
	TailCERLast3 float64 `json:"tail_cer_last3,omitempty"`
	WarmStart    bool    `json:"warm_start,omitempty"`

	// SystemStableHash is a 16-char hex sha256-prefix of the system prompt
	// (BP #1 / system_stable). Populated on cache_summary entries so cross-user
	// cache-share rates can be measured: distinct hash counts within a TTL
	// window indicate residual per-user drift in BP #1. See issue #107.
	SystemStableHash string `json:"system_stable_hash,omitempty"`
}

// MarshalJSON forces CER and TailCERLast3 to appear in cache_summary rows
// even when their value is exactly 0.0. Tool-call rows (Event != "cache_summary")
// fall through to the default tag-driven marshaling, which keeps them out of
// the JSON via omitempty. See AuditEntry comment above for rationale.
func (e AuditEntry) MarshalJSON() ([]byte, error) {
	type alias AuditEntry
	if e.Event == "cache_summary" {
		return json.Marshal(struct {
			alias
			CER          float64 `json:"cer"`
			TailCERLast3 float64 `json:"tail_cer_last3"`
		}{alias: alias(e), CER: e.CER, TailCERLast3: e.TailCERLast3})
	}
	return json.Marshal(alias(e))
}

// AuditLogger writes audit entries as JSON lines to a log file.
type AuditLogger struct {
	mu   sync.Mutex
	file *os.File
}

const maxSummaryLen = 500

// NewAuditLogger creates a logger that writes to the given logDir/audit.log.
// Creates the directory if it does not exist.
func NewAuditLogger(logDir string) (*AuditLogger, error) {
	if logDir == "" {
		return nil, fmt.Errorf("logDir must not be empty")
	}

	if err := os.MkdirAll(logDir, 0700); err != nil {
		return nil, fmt.Errorf("failed to create log directory %s: %w", logDir, err)
	}

	logPath := filepath.Join(logDir, "audit.log")
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return nil, fmt.Errorf("failed to open audit log %s: %w", logPath, err)
	}

	return &AuditLogger{file: f}, nil
}

// Log records a tool call event. Input and output summaries are truncated
// and have secrets redacted before writing.
func (a *AuditLogger) Log(entry AuditEntry) {
	entry.InputSummary = RedactSecrets(truncate(entry.InputSummary, maxSummaryLen))
	entry.OutputSummary = RedactSecrets(truncate(entry.OutputSummary, maxSummaryLen))

	data, err := json.Marshal(entry)
	if err != nil {
		return
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	a.file.Write(data)
	a.file.Write([]byte("\n"))
}

// Close closes the underlying log file.
func (a *AuditLogger) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.file.Close()
}

// redaction patterns compiled once at package init
var redactPatterns []*regexp.Regexp

// urlCredRE matches URL-embedded credentials of the form
// https://user:token@host/... and is handled separately from redactPatterns
// because the replacement preserves the scheme and host (only the
// credentials are scrubbed). Defends against git stderr echoing back a
// remote URL configured with embedded creds — that stderr lands in
// audit.log via auditHTTPOpError's output_summary on install failure.
var urlCredRE = regexp.MustCompile(`(https?://)[^/\s:@]+:[^/\s@]+@`)

// jsonSecretRE redacts the VALUE of a JSON field whose key ends in a
// secret-like word ("app_secret", "access_token", "api_key", "password", ...),
// keeping the key name visible. The env-var pattern in init() only catches
// `NAME=value` shapes; tool-call inputs routed through the `http` tool carry
// secrets as JSON (`{"app_secret":"<32-char generic>"}`) that no other pattern
// matches — e.g. a Feishu/Lark App Secret. The key word must be immediately
// followed by the closing quote so "token_count" / "secret_id" don't match.
var jsonSecretRE = regexp.MustCompile(`(?i)("(?:[a-z0-9_]*(?:secret|token|password|passwd|api[_-]?key))"\s*:\s*)"[^"]*"`)

func init() {
	patterns := []string{
		// AWS access key IDs
		`AKIA[0-9A-Z]{16}`,
		// JWT tokens
		`eyJ[A-Za-z0-9_-]+\.eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+`,
		// sk- style API keys (OpenAI, Stripe, etc.)
		`sk-[a-zA-Z0-9]{20,}`,
		// key- style API keys
		`key-[a-zA-Z0-9]{20,}`,
		// Bearer tokens
		`Bearer [A-Za-z0-9_-]+`,
		// PEM content markers
		`-----BEGIN[A-Z \-]*-----`,
		// Env var assignments with secret-like names
		`(?i)[A-Z_]*(?:KEY|SECRET|TOKEN|PASSWORD)\s*=\s*\S+`,
	}

	for _, p := range patterns {
		redactPatterns = append(redactPatterns, regexp.MustCompile(p))
	}
}

// RedactSecrets replaces known secret patterns with [REDACTED]. URL-embedded
// credentials are redacted in-place ("https://[REDACTED]@host/...") so the
// scheme and host stay visible for debugging.
func RedactSecrets(text string) string {
	result := text
	for _, re := range redactPatterns {
		result = re.ReplaceAllString(result, "[REDACTED]")
	}
	result = urlCredRE.ReplaceAllString(result, "${1}[REDACTED]@")
	result = jsonSecretRE.ReplaceAllString(result, `${1}"[REDACTED]"`)
	return result
}

// truncate shortens text to maxLen, appending "..." if truncated.
func truncate(s string, maxLen int) string {
	r := []rune(s)
	if len(r) <= maxLen {
		return s
	}
	return string(r[:maxLen-3]) + "..."
}
