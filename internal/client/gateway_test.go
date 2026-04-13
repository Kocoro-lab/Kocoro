package client

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCompleteUsesCompletionsEndpoint(t *testing.T) {
	got := struct {
		Messages []Message `json:"messages"`
		Tools    []Tool    `json:"tools"`
	}{}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/completions" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(CompletionResponse{
			OutputText:   "hello",
			FinishReason: "end_turn",
			Usage: Usage{
				InputTokens:  3,
				OutputTokens: 4,
				TotalTokens:  7,
			},
		})
	}))
	defer server.Close()

	gw := NewGatewayClient(server.URL, "key")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resp, err := gw.Complete(ctx, CompletionRequest{
		Messages: []Message{{Role: "user", Content: NewTextContent("ping")}},
		Tools:    []Tool{{Type: "function"}},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.OutputText != "hello" {
		t.Fatalf("expected output hello, got %s", resp.OutputText)
	}
	if len(got.Messages) != 1 || got.Messages[0].Content.Text() != "ping" {
		t.Errorf("request body messages not preserved")
	}
	if len(got.Tools) != 1 || got.Tools[0].Type != "function" {
		t.Errorf("expected tool payload to include tools")
	}
}

func TestListTools(t *testing.T) {
	tools := []ServerToolSchema{
		{Name: "web_search", Description: "Search the web", Parameters: map[string]any{"type": "object"}},
		{Name: "getStockBars", Description: "Get stock bars"},
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/tools" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Method != http.MethodGet {
			t.Errorf("expected GET, got %s", r.Method)
		}
		if r.Header.Get("X-API-Key") != "test-key" {
			t.Errorf("expected X-API-Key=test-key, got %s", r.Header.Get("X-API-Key"))
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(tools)
	}))
	defer server.Close()

	gw := NewGatewayClient(server.URL, "test-key")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	got, err := gw.ListTools(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(got))
	}
	if got[0].Name != "web_search" {
		t.Errorf("expected web_search, got %s", got[0].Name)
	}
	if got[1].Name != "getStockBars" {
		t.Errorf("expected getStockBars, got %s", got[1].Name)
	}
}

func TestListTools_ServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("internal error"))
	}))
	defer server.Close()

	gw := NewGatewayClient(server.URL, "")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := gw.ListTools(ctx)
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestExecuteTool(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/tools/web_search/execute" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}

		var req ToolExecuteRequest
		json.NewDecoder(r.Body).Decode(&req)
		if req.Arguments["query"] != "golang testing" {
			t.Errorf("expected query=golang testing, got %v", req.Arguments["query"])
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(ToolExecuteResponse{
			Success: true,
			Output:  json.RawMessage(`{"results":["found 10 results"]}`),
		})
	}))
	defer server.Close()

	gw := NewGatewayClient(server.URL, "key")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resp, err := gw.ExecuteTool(ctx, "web_search", map[string]any{"query": "golang testing"}, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !resp.Success {
		t.Error("expected success=true")
	}
	if string(resp.Output) != `{"results":["found 10 results"]}` {
		t.Errorf("unexpected output: %s", string(resp.Output))
	}
}

func TestExecuteTool_UrlEscapesName(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// r.URL.RawPath preserves the percent-encoding; r.URL.Path is decoded
		want := "/api/v1/tools/my%2Ftool/execute"
		if r.URL.RawPath != want {
			t.Errorf("expected raw path %s, got %s", want, r.URL.RawPath)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(ToolExecuteResponse{Success: true, Output: json.RawMessage(`"ok"`)})
	}))
	defer server.Close()

	gw := NewGatewayClient(server.URL, "")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := gw.ExecuteTool(ctx, "my/tool", map[string]any{}, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestExecuteTool_403(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte("tool not allowed"))
	}))
	defer server.Close()

	gw := NewGatewayClient(server.URL, "")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := gw.ExecuteTool(ctx, "dangerous_tool", map[string]any{}, "")
	if err == nil {
		t.Fatal("expected error for 403")
	}
}

func TestMessageContent_MarshalString(t *testing.T) {
	msg := Message{Role: "user", Content: NewTextContent("hello")}
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal error: %v", err)
	}
	var raw map[string]json.RawMessage
	json.Unmarshal(data, &raw)
	var content string
	if err := json.Unmarshal(raw["content"], &content); err != nil {
		t.Fatalf("content should be a string, got: %s", string(raw["content"]))
	}
	if content != "hello" {
		t.Errorf("expected 'hello', got %q", content)
	}
}

func TestMessageContent_MarshalBlocks(t *testing.T) {
	msg := Message{
		Role: "user",
		Content: NewBlockContent([]ContentBlock{
			{Type: "text", Text: "describe this"},
			{Type: "image", Source: &ImageSource{Type: "base64", MediaType: "image/png", Data: "abc123"}},
		}),
	}
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal error: %v", err)
	}
	var raw map[string]json.RawMessage
	json.Unmarshal(data, &raw)
	var blocks []ContentBlock
	if err := json.Unmarshal(raw["content"], &blocks); err != nil {
		t.Fatalf("content should be an array, got: %s", string(raw["content"]))
	}
	if len(blocks) != 2 {
		t.Fatalf("expected 2 blocks, got %d", len(blocks))
	}
}

func TestMessageContent_UnmarshalString(t *testing.T) {
	raw := `{"role":"user","content":"hello"}`
	var msg Message
	if err := json.Unmarshal([]byte(raw), &msg); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if msg.Content.Text() != "hello" {
		t.Errorf("expected 'hello', got %q", msg.Content.Text())
	}
}

func TestMessageContent_UnmarshalBlocks(t *testing.T) {
	raw := `{"role":"user","content":[{"type":"text","text":"hi"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"xyz"}}]}`
	var msg Message
	if err := json.Unmarshal([]byte(raw), &msg); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if !msg.Content.HasBlocks() {
		t.Fatal("expected blocks")
	}
	blocks := msg.Content.Blocks()
	if len(blocks) != 2 {
		t.Fatalf("expected 2 blocks, got %d", len(blocks))
	}
}

func TestContentBlock_ToolUse_MarshalJSON(t *testing.T) {
	block := NewToolUseBlock("toolu_abc123", "bash", json.RawMessage(`{"command":"ls"}`))
	data, err := json.Marshal(block)
	if err != nil {
		t.Fatalf("marshal error: %v", err)
	}
	var m map[string]any
	json.Unmarshal(data, &m)
	if m["type"] != "tool_use" {
		t.Errorf("expected type=tool_use, got %v", m["type"])
	}
	if m["id"] != "toolu_abc123" {
		t.Errorf("expected id=toolu_abc123, got %v", m["id"])
	}
	if m["name"] != "bash" {
		t.Errorf("expected name=bash, got %v", m["name"])
	}
}

func TestContentBlock_ToolResult_MarshalJSON_StringContent(t *testing.T) {
	block := NewToolResultBlock("toolu_abc123", "file1.txt\nfile2.txt", false)
	data, err := json.Marshal(block)
	if err != nil {
		t.Fatalf("marshal error: %v", err)
	}
	var m map[string]any
	json.Unmarshal(data, &m)
	if m["type"] != "tool_result" {
		t.Errorf("expected type=tool_result, got %v", m["type"])
	}
	if m["tool_use_id"] != "toolu_abc123" {
		t.Errorf("expected tool_use_id=toolu_abc123, got %v", m["tool_use_id"])
	}
	if m["content"] != "file1.txt\nfile2.txt" {
		t.Errorf("unexpected content: %v", m["content"])
	}
	if _, ok := m["is_error"]; ok {
		t.Error("is_error should be omitted when false")
	}
}

func TestContentBlock_ToolResult_MarshalJSON_ArrayContent(t *testing.T) {
	block := NewToolResultBlockWithImages("toolu_xyz", "Screenshot captured", []ContentBlock{
		{Type: "image", Source: &ImageSource{Type: "base64", MediaType: "image/png", Data: "fakedata"}},
	}, false)
	data, err := json.Marshal(block)
	if err != nil {
		t.Fatalf("marshal error: %v", err)
	}
	var m map[string]any
	json.Unmarshal(data, &m)
	contentArr, ok := m["content"].([]any)
	if !ok {
		t.Fatalf("expected content to be array, got %T: %v", m["content"], m["content"])
	}
	if len(contentArr) != 2 {
		t.Fatalf("expected 2 content blocks (text+image), got %d", len(contentArr))
	}
}

func TestContentBlock_ToolResult_RoundTrip(t *testing.T) {
	// String content round-trip
	original := NewToolResultBlock("toolu_abc", "result text", true)
	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal error: %v", err)
	}
	var decoded ContentBlock
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if decoded.Type != "tool_result" {
		t.Errorf("type mismatch: %s", decoded.Type)
	}
	if decoded.ToolUseID != "toolu_abc" {
		t.Errorf("tool_use_id mismatch: %s", decoded.ToolUseID)
	}
	if !decoded.IsError {
		t.Error("is_error should be true")
	}
	text, ok := decoded.ToolContent.(string)
	if !ok {
		t.Fatalf("expected string content, got %T", decoded.ToolContent)
	}
	if text != "result text" {
		t.Errorf("content mismatch: %s", text)
	}

	// Array content round-trip
	original2 := NewToolResultBlockWithImages("toolu_xyz", "Screenshot", []ContentBlock{
		{Type: "image", Source: &ImageSource{Type: "base64", MediaType: "image/png", Data: "abc"}},
	}, false)
	data2, _ := json.Marshal(original2)
	var decoded2 ContentBlock
	if err := json.Unmarshal(data2, &decoded2); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	blocks, ok := decoded2.ToolContent.([]ContentBlock)
	if !ok {
		t.Fatalf("expected []ContentBlock, got %T", decoded2.ToolContent)
	}
	if len(blocks) != 2 {
		t.Fatalf("expected 2 nested blocks, got %d", len(blocks))
	}
}

func TestFunctionCall_ID(t *testing.T) {
	raw := `{"id":"toolu_abc","name":"bash","arguments":{"command":"ls"}}`
	var fc FunctionCall
	if err := json.Unmarshal([]byte(raw), &fc); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if fc.ID != "toolu_abc" {
		t.Errorf("expected ID=toolu_abc, got %q", fc.ID)
	}
	if fc.Name != "bash" {
		t.Errorf("expected Name=bash, got %q", fc.Name)
	}
}

func TestToolResultText_Extraction(t *testing.T) {
	// String content
	b1 := NewToolResultBlock("id1", "hello world", false)
	if got := ToolResultText(b1); got != "hello world" {
		t.Errorf("expected 'hello world', got %q", got)
	}
	// Array content
	b2 := NewToolResultBlockWithImages("id2", "screenshot taken", nil, false)
	if got := ToolResultText(b2); got != "screenshot taken" {
		t.Errorf("expected 'screenshot taken', got %q", got)
	}
	// Non-tool_result
	b3 := ContentBlock{Type: "text", Text: "plain"}
	if got := ToolResultText(b3); got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}

// TestNormalizeToolInput verifies the shared helper that coerces null/empty
// tool_use input to an empty object. See issue #45.
func TestNormalizeToolInput(t *testing.T) {
	cases := []struct {
		name string
		in   json.RawMessage
		want string
	}{
		{"nil", nil, "{}"},
		{"empty bytes", json.RawMessage(""), "{}"},
		{"literal null", json.RawMessage("null"), "{}"},
		{"null with leading whitespace", json.RawMessage("  null"), "{}"},
		{"null with trailing whitespace", json.RawMessage("null  "), "{}"},
		{"null with surrounding whitespace", json.RawMessage(" null "), "{}"},
		{"whitespace only", json.RawMessage("   "), "{}"},
		{"empty object preserved", json.RawMessage("{}"), "{}"},
		{"populated object preserved", json.RawMessage(`{"x":1}`), `{"x":1}`},
		{"nested object preserved", json.RawMessage(`{"a":{"b":2}}`), `{"a":{"b":2}}`},
		// Double-encoded string unwrap — OpenAI-shaped adapters sometimes
		// return tool arguments as a JSON-encoded string wrapping an object.
		// Anthropic's tool_use.input validator rejects these unless unwrapped.
		{"double-encoded simple", json.RawMessage(`"{\"command\":\"ls\"}"`), `{"command":"ls"}`},
		{"double-encoded nested", json.RawMessage(`"{\"a\":{\"b\":2}}"`), `{"a":{"b":2}}`},
		{"double-encoded empty object", json.RawMessage(`"{}"`), `{}`},
		{"double-encoded with whitespace inside", json.RawMessage(`"  {\"x\":1}  "`), `{"x":1}`},
		// Non-object scalars / strings still pass through untouched so
		// genuine provider bugs remain visible rather than silently masked.
		// See TestContentBlock_MarshalJSON_PreservesNonObjectToolUseInput.
		{"plain string passthrough", json.RawMessage(`"hello"`), `"hello"`},
		{"empty string passthrough", json.RawMessage(`""`), `""`},
		{"quoted null passthrough", json.RawMessage(`"null"`), `"null"`},
		{"encoded array passthrough", json.RawMessage(`"[1,2,3]"`), `"[1,2,3]"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := normalizeToolInput(tc.in)
			if string(got) != tc.want {
				t.Errorf("normalizeToolInput(%q) = %q, want %q", string(tc.in), string(got), tc.want)
			}
		})
	}
}

// TestNewToolUseBlock_NormalizesInput verifies that the constructor coerces
// null/empty input to {} so in-memory consumers (ollama.go, microcompact, etc.)
// never see a literal "null" when reading block.Input.
func TestNewToolUseBlock_NormalizesInput(t *testing.T) {
	cases := []struct {
		name string
		in   json.RawMessage
		want string
	}{
		{"nil input", nil, "{}"},
		{"literal null", json.RawMessage("null"), "{}"},
		{"empty bytes", json.RawMessage(""), "{}"},
		{"valid object passthrough", json.RawMessage(`{"url":"x"}`), `{"url":"x"}`},
		{"double-encoded string unwraps to object", json.RawMessage(`"{\"url\":\"x\"}"`), `{"url":"x"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := NewToolUseBlock("tu_1", "browser_snapshot", tc.in)
			if b.Type != "tool_use" {
				t.Errorf("Type = %q, want tool_use", b.Type)
			}
			if string(b.Input) != tc.want {
				t.Errorf("Input = %q, want %q", string(b.Input), tc.want)
			}
		})
	}
}

// TestContentBlock_MarshalJSON_ForcesToolUseInput is the load-bearing test for
// issue #45. Even if a tool_use block was constructed with nil/null Input
// (e.g. via a code path that bypasses NewToolUseBlock), MarshalJSON must
// always emit a concrete JSON object for tool_use.input. The serialized bytes
// must contain "input":{} and must never contain "input":null, and must never
// omit the input field entirely.
func TestContentBlock_MarshalJSON_ForcesToolUseInput(t *testing.T) {
	cases := []struct {
		name  string
		block ContentBlock
	}{
		{"nil input", ContentBlock{Type: "tool_use", ID: "tu_1", Name: "browser_snapshot"}},
		{"literal null input", ContentBlock{Type: "tool_use", ID: "tu_2", Name: "browser_close", Input: json.RawMessage("null")}},
		{"whitespace null input", ContentBlock{Type: "tool_use", ID: "tu_3", Name: "browser_snapshot", Input: json.RawMessage(" null ")}},
		{"empty bytes input", ContentBlock{Type: "tool_use", ID: "tu_4", Name: "noop", Input: json.RawMessage("")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data, err := json.Marshal(tc.block)
			if err != nil {
				t.Fatalf("marshal failed: %v", err)
			}
			s := string(data)
			if !strings.Contains(s, `"input":{}`) {
				t.Errorf("expected %q to contain %q", s, `"input":{}`)
			}
			if strings.Contains(s, `"input":null`) {
				t.Errorf("serialized output must not contain \"input\":null, got %s", s)
			}
			// Verify by round-trip that "input" key exists and is a JSON object.
			var m map[string]any
			if err := json.Unmarshal(data, &m); err != nil {
				t.Fatalf("round-trip unmarshal failed: %v", err)
			}
			input, ok := m["input"]
			if !ok {
				t.Errorf("input field is missing from serialized output: %s", s)
			}
			if _, isObj := input.(map[string]any); !isObj {
				t.Errorf("input field is not a JSON object, got %T: %v", input, input)
			}
		})
	}
}

// TestContentBlock_MarshalJSON_PreservesValidToolUseInput ensures the
// normalization only kicks in for null/empty inputs and leaves populated
// inputs untouched.
func TestContentBlock_MarshalJSON_PreservesValidToolUseInput(t *testing.T) {
	b := ContentBlock{
		Type:  "tool_use",
		ID:    "tu_valid",
		Name:  "browser_navigate",
		Input: json.RawMessage(`{"url":"https://example.com"}`),
	}
	data, err := json.Marshal(b)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}
	s := string(data)
	if !strings.Contains(s, `"input":{"url":"https://example.com"}`) {
		t.Errorf("expected populated input to be preserved, got %s", s)
	}
}

// TestContentBlock_MarshalJSON_PreservesNonObjectToolUseInput verifies that
// scalar/array/string/bool tool inputs are NOT silently coerced to {} even
// though they are not valid tool_use.input per Anthropic's schema. The
// normalization intentionally targets only null/empty — any other value is
// passed through so the provider bug stays visible instead of being masked.
func TestContentBlock_MarshalJSON_PreservesNonObjectToolUseInput(t *testing.T) {
	cases := []struct {
		name     string
		rawInput json.RawMessage
		expect   string
	}{
		{"number", json.RawMessage(`42`), `"input":42`},
		{"string", json.RawMessage(`"hello"`), `"input":"hello"`},
		{"array", json.RawMessage(`[1,2,3]`), `"input":[1,2,3]`},
		{"bool true", json.RawMessage(`true`), `"input":true`},
		{"bool false", json.RawMessage(`false`), `"input":false`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := ContentBlock{Type: "tool_use", ID: "tu_scalar", Name: "odd_tool", Input: tc.rawInput}
			data, err := json.Marshal(b)
			if err != nil {
				t.Fatalf("marshal failed: %v", err)
			}
			s := string(data)
			if !strings.Contains(s, tc.expect) {
				t.Errorf("expected %q in output, got %s", tc.expect, s)
			}
			if strings.Contains(s, `"input":{}`) {
				t.Errorf("non-null scalar must not be coerced to {}, got %s", s)
			}
		})
	}
}

// TestContentBlock_MarshalJSON_OtherBlocksNoInputField ensures the
// normalization only applies to tool_use blocks. Other block types
// (text, image, tool_result) must NOT have an "input" field injected.
func TestContentBlock_MarshalJSON_OtherBlocksNoInputField(t *testing.T) {
	cases := []struct {
		name  string
		block ContentBlock
	}{
		{"text block", ContentBlock{Type: "text", Text: "hello"}},
		{"tool_result block", NewToolResultBlock("tu_1", "ok", false)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data, err := json.Marshal(tc.block)
			if err != nil {
				t.Fatalf("marshal failed: %v", err)
			}
			s := string(data)
			if strings.Contains(s, `"input"`) {
				t.Errorf("non-tool_use block must not serialize an input field, got %s", s)
			}
		})
	}
}

// TestCompletionRequest_Serialization_NoNullToolInput is the full-payload
// regression test for issue #45. It constructs a CompletionRequest containing
// a tool_use block with nil/null Input (simulating the poisoned history from
// a previous gateway response) and asserts the final serialized JSON bytes
// would not be rejected by Anthropic's schema validator.
func TestCompletionRequest_Serialization_NoNullToolInput(t *testing.T) {
	req := CompletionRequest{
		Messages: []Message{
			{Role: "user", Content: NewTextContent("take a snapshot")},
			{Role: "assistant", Content: NewBlockContent([]ContentBlock{
				{Type: "tool_use", ID: "tu_a", Name: "browser_snapshot"},                              // nil Input
				{Type: "tool_use", ID: "tu_b", Name: "browser_close", Input: json.RawMessage("null")}, // poisoned
			})},
			{Role: "user", Content: NewBlockContent([]ContentBlock{
				NewToolResultBlock("tu_a", "ok", false),
				NewToolResultBlock("tu_b", "ok", false),
			})},
		},
	}
	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}
	s := string(data)
	if strings.Contains(s, `"input":null`) {
		t.Errorf("payload must not contain \"input\":null, got %s", s)
	}
	// Each tool_use block must have a concrete input object in the final JSON.
	// Count occurrences of "input":{} — we expect exactly 2.
	if strings.Count(s, `"input":{}`) != 2 {
		t.Errorf("expected 2 occurrences of \"input\":{}, got %d: %s", strings.Count(s, `"input":{}`), s)
	}
}

// TestArgumentsString_NullHandling verifies that FunctionCall.ArgumentsString
// also coerces literal "null" to "{}". This protects XML-fallback and any
// consumer reading argument strings for logging/audit.
func TestArgumentsString_NullHandling(t *testing.T) {
	cases := []struct {
		name string
		raw  json.RawMessage
		want string
	}{
		{"nil", nil, "{}"},
		{"empty", json.RawMessage(""), "{}"},
		{"literal null", json.RawMessage("null"), "{}"},
		{"whitespace null", json.RawMessage(" null "), "{}"},
		{"empty object", json.RawMessage("{}"), "{}"},
		{"populated object", json.RawMessage(`{"url":"x"}`), `{"url":"x"}`},
		{"json-encoded string", json.RawMessage(`"already a string"`), "already a string"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fc := FunctionCall{Name: "noop", Arguments: tc.raw}
			got := fc.ArgumentsString()
			if got != tc.want {
				t.Errorf("ArgumentsString() = %q, want %q", got, tc.want)
			}
		})
	}
}
