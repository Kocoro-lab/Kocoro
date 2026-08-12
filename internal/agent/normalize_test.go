package agent

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

func TestNormalizeJSON_IdenticalArgumentsAreCanonicalized(t *testing.T) {
	a := normalizeJSON(json.RawMessage(`{"command":"date","path":"/tmp"}`))
	b := normalizeJSON(json.RawMessage(`{ "path": "/tmp", "command": "date" }`))

	if a != b {
		t.Fatalf("expected canonical JSON to match, got %q and %q", a, b)
	}
	if a != `{"command":"date","path":"/tmp"}` {
		t.Fatalf("expected deterministic key order, got %q", a)
	}
}

func TestNormalizeJSON_EmptyAndWhitespaceInputs(t *testing.T) {
	tests := []json.RawMessage{
		nil,
		{},
		[]byte(""),
		[]byte("   \n\t"),
	}

	for i, tc := range tests {
		got := normalizeJSON(tc)
		if got != "{}" {
			t.Fatalf("case %d: expected {}, got %q", i, got)
		}
	}
}

// TestNormalizeJSON_NullBecomesEmptyObject verifies that literal `null`
// arguments (emitted by providers when a tool is called with no args) are
// canonicalized to `{}` so dedup/cache keys don't diverge between null and
// empty-object representations of the same semantic "no arguments". See
// issue #45.
func TestNormalizeJSON_NullBecomesEmptyObject(t *testing.T) {
	cases := []json.RawMessage{
		json.RawMessage("null"),
		json.RawMessage(" null "),
		json.RawMessage("\tnull\n"),
	}
	for i, tc := range cases {
		got := normalizeJSON(tc)
		if got != "{}" {
			t.Fatalf("case %d: expected {}, got %q", i, got)
		}
	}
}

func TestNormalizeJSON_InvalidJSONFallsBackToTrimmedRaw(t *testing.T) {
	raw := json.RawMessage(`{ "command": "date",`)
	expected := strings.TrimSpace(string(raw))
	got := normalizeJSON(raw)
	if got != expected {
		t.Fatalf("expected trimmed fallback %q, got %q", expected, got)
	}
}

func TestNormalizeWebQuery_BrowserURL(t *testing.T) {
	result := normalizeWebQuery(`{"action":"navigate","url":"https://jd.com/search?q=huawei"}`)
	if result == "" {
		t.Error("normalizeWebQuery should extract URL from browser args")
	}
}

func TestNormalizeStructuredToolCallPreamble_StripsDuplicateSerializedCalls(t *testing.T) {
	text := "Tool calls:\nTool: browser_click, Args: {\"ref\":\"e12\"}\nTool: browser_type, Args: {\"ref\":\"e13\",\"text\":\"hello\"}"
	toolCalls := []client.FunctionCall{
		{Name: "browser_click", Arguments: json.RawMessage(`{"ref":"e12"}`)},
		{Name: "browser_type", Arguments: json.RawMessage(`{"text":"hello","ref":"e13"}`)},
	}

	if got := normalizeStructuredToolCallPreamble(text, toolCalls); got != "" {
		t.Fatalf("expected duplicate serialized tool-call text to be stripped, got %q", got)
	}
}

func TestNormalizeStructuredToolCallPreamble_PreservesMeaningfulText(t *testing.T) {
	text := "Let me check that file."
	toolCalls := []client.FunctionCall{
		{Name: "mock_tool", Arguments: json.RawMessage(`{}`)},
	}

	if got := normalizeStructuredToolCallPreamble(text, toolCalls); got != text {
		t.Fatalf("expected meaningful text to be preserved, got %q", got)
	}
}

func TestToolOutcomeSignatureCanonicalizesJSONObjects(t *testing.T) {
	a := toolOutcomeSignature(`{"status":"running","progress":25}`)
	b := toolOutcomeSignature(`{ "progress": 25, "status": "running" }`)
	if a != b {
		t.Fatalf("semantically identical JSON outcomes must share a signature: %q != %q", a, b)
	}
	if a == toolOutcomeSignature(`{"status":"running","progress":60}`) {
		t.Fatal("changed progress must produce a different outcome signature")
	}
}
