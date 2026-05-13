package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// OpenAI-compatible message types (used for Ollama request/response)
// ---------------------------------------------------------------------------

type openAIMessage struct {
	Role       string           `json:"role"`
	Content    string           `json:"content"`
	ToolCalls  []openAIToolCall `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
}

type openAIToolCall struct {
	ID       string             `json:"id"`
	Type     string             `json:"type"`
	Function openAIFunctionCall `json:"function"`
}

type openAIFunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ---------------------------------------------------------------------------
// Format conversion: Kocoro (Anthropic) → OpenAI
// ---------------------------------------------------------------------------

// convertMessagesToOpenAI converts Kocoro's Anthropic-format messages to
// OpenAI-compatible format for Ollama.
func convertMessagesToOpenAI(msgs []Message) []openAIMessage {
	var result []openAIMessage
	for _, msg := range msgs {
		if !msg.Content.HasBlocks() {
			result = append(result, openAIMessage{
				Role:    msg.Role,
				Content: msg.Content.Text(),
			})
			continue
		}

		blocks := msg.Content.Blocks()

		var toolUseBlocks []ContentBlock
		var toolResultBlocks []ContentBlock
		var textParts []string

		for _, b := range blocks {
			switch b.Type {
			case "tool_use":
				toolUseBlocks = append(toolUseBlocks, b)
			case "tool_result":
				toolResultBlocks = append(toolResultBlocks, b)
			case "text":
				if b.Text != "" {
					textParts = append(textParts, b.Text)
				}
			}
		}

		if msg.Role == "assistant" && len(toolUseBlocks) > 0 {
			// Assistant message with tool calls → OpenAI tool_calls format
			m := openAIMessage{
				Role:    "assistant",
				Content: strings.Join(textParts, "\n"),
			}
			for _, b := range toolUseBlocks {
				m.ToolCalls = append(m.ToolCalls, openAIToolCall{
					ID:   b.ID,
					Type: "function",
					Function: openAIFunctionCall{
						Name:      b.Name,
						Arguments: string(b.Input),
					},
				})
			}
			result = append(result, m)
		} else if len(toolResultBlocks) > 0 {
			// Tool results → one role:tool message per result
			for _, b := range toolResultBlocks {
				result = append(result, openAIMessage{
					Role:       "tool",
					Content:    ToolResultText(b),
					ToolCallID: b.ToolUseID,
				})
			}
		} else {
			// Text-only blocks → concatenate
			result = append(result, openAIMessage{
				Role:    msg.Role,
				Content: strings.Join(textParts, "\n"),
			})
		}
	}
	return result
}

// ---------------------------------------------------------------------------
// Format conversion: OpenAI → Kocoro
// ---------------------------------------------------------------------------

// mapFinishReason converts OpenAI finish reasons to Kocoro's internal values.
func mapFinishReason(reason string) string {
	switch reason {
	case "stop":
		return "end_turn"
	case "length":
		return "max_tokens"
	case "tool_calls":
		return "tool_use"
	case "content_filter":
		return "content_filter"
	default:
		return "end_turn"
	}
}

// convertOpenAIResponse parses an OpenAI-format JSON response into CompletionResponse.
func convertOpenAIResponse(data []byte) (*CompletionResponse, error) {
	var raw struct {
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Role      string  `json:"role"`
				Content   *string `json:"content"`
				Reasoning *string `json:"reasoning"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct {
						Name      string          `json:"name"`
						Arguments json.RawMessage `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}

	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("decode OpenAI response: %w", err)
	}
	if len(raw.Choices) == 0 {
		return nil, fmt.Errorf("empty choices in response")
	}

	choice := raw.Choices[0]
	resp := &CompletionResponse{
		Provider:     "ollama",
		Model:        raw.Model,
		FinishReason: mapFinishReason(choice.FinishReason),
		Usage: Usage{
			InputTokens:  raw.Usage.PromptTokens,
			OutputTokens: raw.Usage.CompletionTokens,
			TotalTokens:  raw.Usage.TotalTokens,
		},
	}

	if choice.Message.Content != nil && *choice.Message.Content != "" {
		resp.OutputText = *choice.Message.Content
	} else if choice.Message.Reasoning != nil && *choice.Message.Reasoning != "" {
		// Thinking models (e.g. Qwen3) may exhaust max_tokens during reasoning,
		// leaving content empty. Surface the reasoning so the user sees something.
		resp.OutputText = "[thinking] " + *choice.Message.Reasoning
	}

	for _, tc := range choice.Message.ToolCalls {
		args := tc.Function.Arguments
		// Arguments can be a JSON string (OpenAI compat) or JSON object (Ollama native).
		// Normalize to raw JSON object for Kocoro's FunctionCall.Arguments.
		var argsStr string
		if err := json.Unmarshal(args, &argsStr); err == nil {
			args = json.RawMessage(argsStr)
		}

		resp.ToolCalls = append(resp.ToolCalls, FunctionCall{
			ID:        tc.ID,
			Name:      tc.Function.Name,
			Arguments: args,
		})
	}

	return resp, nil
}

// ---------------------------------------------------------------------------
// Tool schema filtering
// ---------------------------------------------------------------------------

// filterToolsForOpenAI removes native tool definitions (e.g. Anthropic computer use)
// that Ollama doesn't support. Only standard function tools pass through.
func filterToolsForOpenAI(tools []Tool) []Tool {
	if tools == nil {
		return nil
	}
	var filtered []Tool
	for _, t := range tools {
		if t.Type == "function" {
			filtered = append(filtered, t)
		}
	}
	return filtered
}

// ---------------------------------------------------------------------------
// OllamaModel — model metadata from /api/tags
// ---------------------------------------------------------------------------

// OllamaModel represents a model available in the local Ollama instance.
type OllamaModel struct {
	Name    string `json:"name"`
	Size    int64  `json:"size"`
	Details struct {
		ParameterSize string `json:"parameter_size"`
	} `json:"details"`
}

// ---------------------------------------------------------------------------
// OllamaClient
// ---------------------------------------------------------------------------

// OllamaClient implements LLMClient for local Ollama instances via the
// OpenAI-compatible /v1/chat/completions endpoint.
type OllamaClient struct {
	endpoint   string
	model      string
	httpClient *http.Client
}

// NewOllamaClient creates a client for a local Ollama instance.
// endpoint is the base URL (e.g. "http://localhost:11434").
// model is the default model name (e.g. "llama3.1").
func NewOllamaClient(endpoint, model string) *OllamaClient {
	return &OllamaClient{
		endpoint: endpoint,
		model:    model,
		httpClient: &http.Client{
			Timeout: 600 * time.Second,
		},
	}
}

func (c *OllamaClient) resolveModel(req CompletionRequest) string {
	if req.SpecificModel != "" {
		return req.SpecificModel
	}
	return c.model
}

type ollamaRequestBody struct {
	Model       string          `json:"model"`
	Messages    []openAIMessage `json:"messages"`
	Tools       []Tool          `json:"tools,omitempty"`
	Temperature float64         `json:"temperature,omitempty"`
	MaxTokens   int             `json:"max_tokens,omitempty"`
	Stream      bool            `json:"stream"`
}

func (c *OllamaClient) buildRequestBody(req CompletionRequest, stream bool) ollamaRequestBody {
	return ollamaRequestBody{
		Model:       c.resolveModel(req),
		Messages:    convertMessagesToOpenAI(req.Messages),
		Tools:       filterToolsForOpenAI(req.Tools),
		Temperature: req.Temperature,
		MaxTokens:   req.MaxTokens,
		Stream:      stream,
	}
}

// Complete sends a non-streaming completion request to Ollama.
func (c *OllamaClient) Complete(ctx context.Context, req CompletionRequest) (*CompletionResponse, error) {
	body, err := json.Marshal(c.buildRequestBody(req, false))
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.endpoint+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, &APIError{StatusCode: resp.StatusCode, Body: readResponseBody(resp)}
	}

	respData, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	return convertOpenAIResponse(respData)
}

// CompleteStream sends a streaming completion request to Ollama.
// It calls onDelta for each text chunk and returns the final response.
func (c *OllamaClient) CompleteStream(ctx context.Context, req CompletionRequest, onDelta func(StreamDelta)) (*CompletionResponse, error) {
	body, err := json.Marshal(c.buildRequestBody(req, true))
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.endpoint+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, &APIError{StatusCode: resp.StatusCode, Body: readResponseBody(resp)}
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	var fullText strings.Builder
	var reasoningText strings.Builder
	var finishReason string
	var model string
	var usage Usage
	var toolCalls []FunctionCall

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

		var chunk struct {
			Model   string `json:"model"`
			Choices []struct {
				Delta struct {
					Content   string `json:"content"`
					Reasoning string `json:"reasoning"`
					ToolCalls []struct {
						ID       string `json:"id"`
						Type     string `json:"type"`
						Function struct {
							Name      string          `json:"name"`
							Arguments json.RawMessage `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
			Usage *struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
				TotalTokens      int `json:"total_tokens"`
			} `json:"usage"`
		}

		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue
		}

		if chunk.Model != "" {
			model = chunk.Model
		}

		if len(chunk.Choices) > 0 {
			delta := chunk.Choices[0].Delta
			if delta.Content != "" {
				fullText.WriteString(delta.Content)
				if onDelta != nil {
					onDelta(StreamDelta{Text: delta.Content})
				}
			}
			if delta.Reasoning != "" {
				reasoningText.WriteString(delta.Reasoning)
			}
			// Ollama sends tool calls as a complete chunk (not delta-split like OpenAI)
			for _, tc := range delta.ToolCalls {
				args := tc.Function.Arguments
				var argsStr string
				if err := json.Unmarshal(args, &argsStr); err == nil {
					args = json.RawMessage(argsStr)
				}
				toolCalls = append(toolCalls, FunctionCall{
					ID:        tc.ID,
					Name:      tc.Function.Name,
					Arguments: args,
				})
			}
			if chunk.Choices[0].FinishReason != nil {
				finishReason = *chunk.Choices[0].FinishReason
			}
		}

		if chunk.Usage != nil {
			usage = Usage{
				InputTokens:  chunk.Usage.PromptTokens,
				OutputTokens: chunk.Usage.CompletionTokens,
				TotalTokens:  chunk.Usage.TotalTokens,
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("stream read error: %w", err)
	}

	output := fullText.String()
	if output == "" && reasoningText.Len() > 0 {
		output = "[thinking] " + reasoningText.String()
	}

	return &CompletionResponse{
		Provider:     "ollama",
		Model:        model,
		OutputText:   output,
		FinishReason: mapFinishReason(finishReason),
		ToolCalls:    toolCalls,
		Usage:        usage,
	}, nil
}

// ListModels queries the Ollama instance for available models via /api/tags.
func (c *OllamaClient) ListModels(ctx context.Context) ([]OllamaModel, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint+"/api/tags", nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ollama returned %d", resp.StatusCode)
	}
	var result struct {
		Models []OllamaModel `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return result.Models, nil
}

// CheckHealth checks if the Ollama instance is reachable.
func (c *OllamaClient) CheckHealth(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint+"/", nil)
	if err != nil {
		return err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("ollama not reachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ollama returned %d", resp.StatusCode)
	}
	return nil
}
