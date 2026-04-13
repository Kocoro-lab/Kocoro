package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// APIError represents an HTTP error from the LLM API with a status code.
type APIError struct {
	StatusCode int
	Body       string
}

func (e *APIError) Error() string {
	if e.Body != "" {
		return fmt.Sprintf("API returned %d: %s", e.StatusCode, e.Body)
	}
	return fmt.Sprintf("API returned %d", e.StatusCode)
}

// --- Public types (used by agent loop) ---

// ContentBlock represents a polymorphic content block.
// Supported types: "text", "image", "tool_use", "tool_result".
type ContentBlock struct {
	Type   string       `json:"type"`
	Text   string       `json:"text,omitempty"`
	Source *ImageSource `json:"source,omitempty"`
	// tool_use fields
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
	// tool_result fields
	ToolUseID   string `json:"tool_use_id,omitempty"`
	IsError     bool   `json:"is_error,omitempty"`
	ToolContent any    `json:"-"` // string or []ContentBlock; serialized as "content" for tool_result
}

// MarshalJSON handles the polymorphic "content" field for tool_result blocks
// and guarantees tool_use.input is always a concrete JSON object (never null
// or missing), which is required by Anthropic's schema validator. See issue #45.
func (cb ContentBlock) MarshalJSON() ([]byte, error) {
	type plain ContentBlock // avoid infinite recursion
	m := make(map[string]any)

	// Marshal the base fields via the plain type
	base, err := json.Marshal(plain(cb))
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(base, &m); err != nil {
		return nil, err
	}

	// Add ToolContent as "content" for tool_result blocks
	if cb.Type == "tool_result" && cb.ToolContent != nil {
		m["content"] = cb.ToolContent
	}

	// For tool_use blocks, force-write a concrete input object. The base
	// field has `omitempty` so a nil/empty RawMessage is dropped by the
	// initial marshal; we reinject a normalized value here so every tool_use
	// block always carries a valid `input` field on the wire.
	if cb.Type == "tool_use" {
		normalized := normalizeToolInput(cb.Input)
		var inputVal any
		if err := json.Unmarshal(normalized, &inputVal); err != nil {
			// Fallback: preserve raw bytes as a JSON Number/string via RawMessage.
			inputVal = normalized
		}
		m["input"] = inputVal
	}

	return json.Marshal(m)
}

// normalizeToolInput coerces a tool_use input RawMessage into a shape
// Anthropic's tool_use.input validator ("Input should be a valid
// dictionary") will accept when we can do so unambiguously.
//
// Two normalizations are applied:
//  1. null / empty / whitespace → "{}" (issue #45).
//  2. JSON-encoded string wrapping a JSON object → unwrap once. Some
//     providers (OpenAI-shaped Chat Completions adapters) return tool
//     arguments as a JSON string whose decoded value is itself a JSON
//     object, e.g. `"{\"command\":\"ls\"}"`. The tool executes fine via
//     FunctionCall.ArgumentsString, but the double-encoded bytes used to
//     be persisted verbatim in the assistant turn, causing the next call
//     to Anthropic to drop that turn with a 400 — which llm-service then
//     silently sanitized out, losing tool history from the model.
//
// Non-object inputs (numbers, arrays, plain strings, bools) are passed
// through unchanged on purpose: those are provider bugs, and masking
// them with "{}" would hide the anomaly. Only the unambiguous
// null/empty and double-encoded-object cases are rewritten.
func normalizeToolInput(raw json.RawMessage) json.RawMessage {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return json.RawMessage("{}")
	}
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(trimmed, &s); err == nil {
			inner := bytes.TrimSpace([]byte(s))
			if len(inner) > 0 && inner[0] == '{' && json.Valid(inner) {
				return json.RawMessage(inner)
			}
		}
	}
	return raw
}

// UnmarshalJSON handles the polymorphic "content" field for tool_result blocks.
func (cb *ContentBlock) UnmarshalJSON(data []byte) error {
	type plain ContentBlock
	var p plain
	if err := json.Unmarshal(data, &p); err != nil {
		return err
	}
	*cb = ContentBlock(p)

	if cb.Type == "tool_result" {
		// Parse the "content" field which can be a string or array of blocks
		var raw struct {
			Content json.RawMessage `json:"content"`
		}
		if err := json.Unmarshal(data, &raw); err == nil && len(raw.Content) > 0 {
			var s string
			if err := json.Unmarshal(raw.Content, &s); err == nil {
				cb.ToolContent = s
			} else {
				var blocks []ContentBlock
				if err := json.Unmarshal(raw.Content, &blocks); err == nil {
					cb.ToolContent = blocks
				}
			}
		}
	}
	return nil
}

// NewToolUseBlock creates a tool_use content block. Input is normalized via
// normalizeToolInput so in-memory consumers (e.g. ollama.go's string(b.Input))
// never observe a literal "null" or empty bytes.
func NewToolUseBlock(id, name string, input json.RawMessage) ContentBlock {
	return ContentBlock{Type: "tool_use", ID: id, Name: name, Input: normalizeToolInput(input)}
}

// NewToolResultBlock creates a tool_result content block with string content.
func NewToolResultBlock(toolUseID, content string, isError bool) ContentBlock {
	return ContentBlock{Type: "tool_result", ToolUseID: toolUseID, IsError: isError, ToolContent: content}
}

// NewToolResultBlockWithImages creates a tool_result with nested text + image blocks.
func NewToolResultBlockWithImages(toolUseID, text string, images []ContentBlock, isError bool) ContentBlock {
	nested := []ContentBlock{{Type: "text", Text: text}}
	nested = append(nested, images...)
	return ContentBlock{Type: "tool_result", ToolUseID: toolUseID, IsError: isError, ToolContent: nested}
}

// ToolResultText extracts the text from a tool_result's content.
func ToolResultText(cb ContentBlock) string {
	if cb.Type != "tool_result" {
		return ""
	}
	switch v := cb.ToolContent.(type) {
	case string:
		return v
	case []ContentBlock:
		var sb strings.Builder
		for _, b := range v {
			if b.Type == "text" {
				sb.WriteString(b.Text)
			}
		}
		return sb.String()
	}
	return ""
}

// ImageSource holds base64-encoded image data for image content blocks.
type ImageSource struct {
	Type      string `json:"type"`       // "base64"
	MediaType string `json:"media_type"` // "image/png"
	Data      string `json:"data"`
}

// MessageContent holds message content as either a plain string or content blocks.
type MessageContent struct {
	text   string
	blocks []ContentBlock
}

// NewTextContent creates a MessageContent from a plain string.
func NewTextContent(text string) MessageContent {
	return MessageContent{text: text}
}

// NewBlockContent creates a MessageContent from content blocks.
func NewBlockContent(blocks []ContentBlock) MessageContent {
	return MessageContent{blocks: blocks}
}

// Text returns the text content. For block content, concatenates text from
// text blocks and tool_result blocks.
func (mc MessageContent) Text() string {
	if mc.text != "" {
		return mc.text
	}
	var sb strings.Builder
	for _, b := range mc.blocks {
		switch b.Type {
		case "text":
			sb.WriteString(b.Text)
		case "tool_result":
			if t := ToolResultText(b); t != "" {
				sb.WriteString(t)
			}
		}
	}
	return sb.String()
}

// HasBlocks returns true if the content contains content blocks.
func (mc MessageContent) HasBlocks() bool {
	return len(mc.blocks) > 0
}

// Blocks returns the content blocks.
func (mc MessageContent) Blocks() []ContentBlock {
	return mc.blocks
}

// MarshalJSON serializes as a string if plain text, or as an array if blocks.
func (mc MessageContent) MarshalJSON() ([]byte, error) {
	if len(mc.blocks) > 0 {
		return json.Marshal(mc.blocks)
	}
	return json.Marshal(mc.text)
}

// UnmarshalJSON deserializes from either a JSON string or an array of content blocks.
func (mc *MessageContent) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		mc.text = s
		return nil
	}
	var blocks []ContentBlock
	if err := json.Unmarshal(data, &blocks); err == nil {
		mc.blocks = blocks
		return nil
	}
	return fmt.Errorf("content must be string or array of content blocks")
}

type Message struct {
	Role       string         `json:"role"`
	Content    MessageContent `json:"content"`
	Name       string         `json:"name,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

type FunctionDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

// NativeToolDef represents a provider-native tool definition (e.g., Anthropic computer use).
type NativeToolDef struct {
	Type            string `json:"type"`
	Name            string `json:"name"`
	DisplayWidthPx  int    `json:"display_width_px,omitempty"`
	DisplayHeightPx int    `json:"display_height_px,omitempty"`
}

type Tool struct {
	Type            string      `json:"type"`
	Function        FunctionDef `json:"function,omitempty"`
	Name            string      `json:"name,omitempty"`
	DisplayWidthPx  int         `json:"display_width_px,omitempty"`
	DisplayHeightPx int         `json:"display_height_px,omitempty"`
}

type CompletionRequest struct {
	Messages      []Message `json:"messages"`
	ModelTier     string    `json:"model_tier,omitempty"`
	SpecificModel string    `json:"specific_model,omitempty"`
	Temperature   float64   `json:"temperature,omitempty"`
	MaxTokens     int       `json:"max_tokens,omitempty"`
	Tools         []Tool    `json:"tools,omitempty"`
	Stream        bool      `json:"stream,omitempty"`

	// Provider-specific parameters (passed through to gateway)
	Thinking        *ThinkingConfig `json:"thinking,omitempty"`
	ReasoningEffort string          `json:"reasoning_effort,omitempty"` // OpenAI o-models: minimal/low/medium/high
	ToolChoice      any             `json:"tool_choice,omitempty"`      // nil=auto, "any", or {"type":"tool","name":"..."}
}

// ThinkingConfig for Anthropic extended thinking.
// Sent as-is to the gateway which passes it to the Anthropic provider.
type ThinkingConfig struct {
	Type         string `json:"type"`                    // "adaptive", "enabled", or "disabled"
	BudgetTokens int    `json:"budget_tokens,omitempty"` // thinking token budget (only for "enabled" mode)
}

type FunctionCall struct {
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// ArgumentsString returns arguments as a JSON string regardless of
// whether the server sent a string or an object. Null/empty arguments are
// coerced to "{}" via normalizeToolInput (see issue #45) so downstream
// consumers (logging, XML fallback, audit) never see literal "null".
func (fc *FunctionCall) ArgumentsString() string {
	normalized := normalizeToolInput(fc.Arguments)
	var s string
	if err := json.Unmarshal(normalized, &s); err == nil {
		return s
	}
	return string(normalized)
}

type Usage struct {
	InputTokens        int     `json:"input_tokens"`
	OutputTokens       int     `json:"output_tokens"`
	TotalTokens        int     `json:"total_tokens"`
	CostUSD            float64 `json:"cost_usd"`
	CacheReadTokens    int     `json:"cache_read_tokens,omitempty"`
	CacheCreationTokens int    `json:"cache_creation_tokens,omitempty"`
}

type CompletionResponse struct {
	Provider     string         `json:"provider"`
	Model        string         `json:"model"`
	OutputText   string         `json:"output_text"`
	FinishReason string         `json:"finish_reason"`
	FunctionCall *FunctionCall  `json:"function_call,omitempty"`
	ToolCalls    []FunctionCall `json:"tool_calls,omitempty"`
	Usage        Usage          `json:"usage"`
	RequestID    string         `json:"request_id,omitempty"`
	LatencyMs    int            `json:"latency_ms,omitempty"`
	Cached       bool           `json:"cached"`
}

// AllToolCalls returns all tool calls from the response, preferring ToolCalls
// array if present, falling back to single FunctionCall for backward compat.
func (r *CompletionResponse) AllToolCalls() []FunctionCall {
	if len(r.ToolCalls) > 0 {
		return r.ToolCalls
	}
	if r.FunctionCall != nil {
		return []FunctionCall{*r.FunctionCall}
	}
	return nil
}

// HasToolCalls returns true if the response contains any tool calls.
func (r *CompletionResponse) HasToolCalls() bool {
	return len(r.ToolCalls) > 0 || r.FunctionCall != nil
}


// --- Task/workflow types (used by /research, /swarm) ---

type TaskRequest struct {
	Query            string         `json:"query"`
	SessionID        string         `json:"session_id,omitempty"`
	Context          map[string]any `json:"context,omitempty"`
	ResearchStrategy string         `json:"research_strategy,omitempty"`
}

type TaskStreamResponse struct {
	WorkflowID string `json:"workflow_id"`
	TaskID     string `json:"task_id"`
	StreamURL  string `json:"stream_url"`
}

type TaskStatusResponse struct {
	TaskID     string         `json:"task_id"`
	WorkflowID string         `json:"workflow_id"`
	Status     string         `json:"status"`
	Result     string         `json:"result"`
	Query      string         `json:"query"`
	Usage      map[string]any `json:"usage,omitempty"`
}

// --- Client ---

type GatewayClient struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client
}

func NewGatewayClient(baseURL, apiKey string) *GatewayClient {
	return &GatewayClient{
		baseURL: baseURL,
		apiKey:  apiKey,
		httpClient: &http.Client{
			Timeout: 600 * time.Second,
		},
	}
}

// Complete sends a completion request to the gateway's /v1/completions endpoint.
// This endpoint is a thin proxy to the LLM service that returns raw function_call
// responses for client-side tool execution.
func (c *GatewayClient) Complete(ctx context.Context, req CompletionRequest) (*CompletionResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/completions", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		httpReq.Header.Set("X-API-Key", c.apiKey)
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, &APIError{StatusCode: resp.StatusCode, Body: readResponseBody(resp)}
	}

	var result CompletionResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	return &result, nil
}

// StreamDelta represents an incremental text chunk from streaming completion.
type StreamDelta struct {
	Text string
}

// CompleteStream sends a streaming completion request. It calls onDelta for each
// text chunk and returns the final CompletionResponse when done.
func (c *GatewayClient) CompleteStream(ctx context.Context, req CompletionRequest, onDelta func(StreamDelta)) (*CompletionResponse, error) {
	req.Stream = true
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/completions", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	if c.apiKey != "" {
		httpReq.Header.Set("X-API-Key", c.apiKey)
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, &APIError{StatusCode: resp.StatusCode, Body: readResponseBody(resp)}
	}

	// Parse SSE stream
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	var result *CompletionResponse
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := line[6:]
		if payload == "[DONE]" {
			break
		}

		var event map[string]json.RawMessage
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			continue
		}

		typeBytes, ok := event["type"]
		if !ok {
			continue
		}
		var eventType string
		json.Unmarshal(typeBytes, &eventType)

		switch eventType {
		case "content_delta":
			var text string
			if textBytes, ok := event["text"]; ok {
				json.Unmarshal(textBytes, &text)
			}
			if text != "" && onDelta != nil {
				onDelta(StreamDelta{Text: text})
			}
		case "done":
			// The done event has the full response shape
			var final CompletionResponse
			if err := json.Unmarshal([]byte(payload), &final); err == nil {
				result = &final
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("stream read error: %w", err)
	}

	if result == nil {
		return nil, fmt.Errorf("stream ended without done event")
	}

	return result, nil
}

func (c *GatewayClient) SubmitTaskStream(ctx context.Context, req TaskRequest) (*TaskStreamResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/v1/tasks/stream", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		httpReq.Header.Set("X-API-Key", c.apiKey)
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("gateway request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		body := readResponseBody(resp)
		if body != "" {
			return nil, fmt.Errorf("gateway returned %d (expected 201): %s", resp.StatusCode, body)
		}
		return nil, fmt.Errorf("gateway returned %d (expected 201)", resp.StatusCode)
	}

	var result TaskStreamResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return &result, nil
}

// GetTask fetches the full task result from the REST API.
// Unlike SSE events which truncate at 10K chars, the REST response contains
// the complete untruncated result.
func (c *GatewayClient) GetTask(ctx context.Context, taskID string) (*TaskStatusResponse, error) {
	endpoint := fmt.Sprintf("%s/api/v1/tasks/%s", c.baseURL, url.PathEscape(taskID))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	if c.apiKey != "" {
		req.Header.Set("X-API-Key", c.apiKey)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		errBody := readResponseBody(resp)
		if errBody != "" {
			return nil, fmt.Errorf("get task returned %d: %s", resp.StatusCode, errBody)
		}
		return nil, fmt.Errorf("get task returned %d", resp.StatusCode)
	}

	var result TaskStatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return &result, nil
}

// ApproveReviewPlan approves a HITL research plan so the workflow continues.
func (c *GatewayClient) ApproveReviewPlan(ctx context.Context, workflowID string) error {
	body, _ := json.Marshal(map[string]string{"action": "approve"})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("%s/api/v1/tasks/%s/review", c.baseURL, url.PathEscape(workflowID)), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("X-API-Key", c.apiKey)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("approve request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("approve returned %d", resp.StatusCode)
	}
	return nil
}

func (c *GatewayClient) StreamURL(workflowID string) string {
	return fmt.Sprintf("%s/api/v1/stream/sse?workflow_id=%s", c.baseURL, workflowID)
}

// ResolveURL prepends the base URL if the given URL is relative (starts with /).
func (c *GatewayClient) ResolveURL(u string) string {
	if len(u) > 0 && u[0] == '/' {
		return c.baseURL + u
	}
	return u
}

func (c *GatewayClient) Health(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/health", nil)
	if err != nil {
		return err
	}
	if c.apiKey != "" {
		req.Header.Set("X-API-Key", c.apiKey)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body := readResponseBody(resp)
		if body != "" {
			return fmt.Errorf("health check returned %d: %s", resp.StatusCode, body)
		}
		return fmt.Errorf("health check returned %d", resp.StatusCode)
	}
	return nil
}

// --- Server tool types (used by tools.RegisterAll) ---

type ServerToolSchema struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

type ToolExecuteRequest struct {
	Arguments map[string]any `json:"arguments"`
	SessionID string         `json:"session_id,omitempty"`
}

type ToolExecuteResponse struct {
	Success         bool            `json:"success"`
	Output          json.RawMessage `json:"output"`
	Text            *string         `json:"text"`
	Error           *string         `json:"error"`
	ExecutionTimeMs int             `json:"execution_time_ms,omitempty"`
}

// ListTools fetches available server-side tool schemas from the gateway.
func (c *GatewayClient) ListTools(ctx context.Context) ([]ServerToolSchema, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/api/v1/tools", nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	if c.apiKey != "" {
		req.Header.Set("X-API-Key", c.apiKey)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, &APIError{StatusCode: resp.StatusCode, Body: readResponseBody(resp)}
	}

	var tools []ServerToolSchema
	if err := json.NewDecoder(resp.Body).Decode(&tools); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return tools, nil
}

// ExecuteTool calls a server-side tool by name with the given arguments.
func (c *GatewayClient) ExecuteTool(ctx context.Context, name string, arguments map[string]any, sessionID string) (*ToolExecuteResponse, error) {
	reqBody := ToolExecuteRequest{
		Arguments: arguments,
		SessionID: sessionID,
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	endpoint := c.baseURL + "/api/v1/tools/" + url.PathEscape(name) + "/execute"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("X-API-Key", c.apiKey)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		errBody := readResponseBody(resp)
		if errBody != "" {
			return nil, fmt.Errorf("tool %s returned %d: %s", name, resp.StatusCode, errBody)
		}
		return nil, fmt.Errorf("tool %s returned %d", name, resp.StatusCode)
	}

	var result ToolExecuteResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return &result, nil
}

func readResponseBody(resp *http.Response) string {
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(body))
}
