//go:build live

package client

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

// Live integration tests against a real Ollama instance.
//
// Run with:
//   OLLAMA_LIVE=1 go test -tags=live ./internal/client/ -run TestOllamaLive -v -timeout 300s
//
// Override defaults:
//   OLLAMA_ENDPOINT=http://localhost:11434  OLLAMA_MODEL=gemma4:31b  OLLAMA_LIVE=1 go test ...

func skipUnlessLive(t *testing.T) {
	t.Helper()
	if os.Getenv("OLLAMA_LIVE") == "" {
		t.Skip("set OLLAMA_LIVE=1 to run live Ollama tests")
	}
}

func liveEndpointFromEnv() string {
	if v := os.Getenv("OLLAMA_ENDPOINT"); v != "" {
		return v
	}
	return "http://localhost:11434"
}

func liveModelFromEnv() string {
	if v := os.Getenv("OLLAMA_MODEL"); v != "" {
		return v
	}
	return "qwen3:4b"
}

// ---------------------------------------------------------------------------
// Basic text completion
// ---------------------------------------------------------------------------

func TestOllamaLive_SimpleCompletion(t *testing.T) {
	skipUnlessLive(t)

	oc := NewOllamaClient(liveEndpointFromEnv(), liveModelFromEnv())
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	resp, err := oc.Complete(ctx, CompletionRequest{
		Messages: []Message{
			{Role: "user", Content: NewTextContent("Reply with exactly one word: hello")},
		},
		Temperature: 0,
		MaxTokens:   2000,
	})
	if err != nil {
		t.Fatalf("Complete failed: %v", err)
	}

	t.Logf("Model: %s", resp.Model)
	t.Logf("OutputText: %q", resp.OutputText)
	t.Logf("FinishReason: %s", resp.FinishReason)
	t.Logf("Usage: input=%d output=%d total=%d", resp.Usage.InputTokens, resp.Usage.OutputTokens, resp.Usage.TotalTokens)

	if resp.OutputText == "" {
		t.Error("expected non-empty output")
	}
	if resp.Provider != "ollama" {
		t.Errorf("Provider: got %q, want %q", resp.Provider, "ollama")
	}
	if resp.Usage.InputTokens == 0 {
		t.Error("expected non-zero input tokens")
	}
	if resp.Usage.OutputTokens == 0 {
		t.Error("expected non-zero output tokens")
	}
}

// ---------------------------------------------------------------------------
// Streaming text completion
// ---------------------------------------------------------------------------

func TestOllamaLive_StreamCompletion(t *testing.T) {
	skipUnlessLive(t)

	oc := NewOllamaClient(liveEndpointFromEnv(), liveModelFromEnv())
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	var deltas []string
	resp, err := oc.CompleteStream(ctx, CompletionRequest{
		Messages: []Message{
			{Role: "user", Content: NewTextContent("Count from 1 to 5, one number per line.")},
		},
		Temperature: 0,
		MaxTokens:   2000,
	}, func(d StreamDelta) {
		deltas = append(deltas, d.Text)
	})
	if err != nil {
		t.Fatalf("CompleteStream failed: %v", err)
	}

	joined := strings.Join(deltas, "")
	t.Logf("Deltas received: %d", len(deltas))
	t.Logf("Concatenated: %q", joined)
	t.Logf("OutputText: %q", resp.OutputText)
	t.Logf("FinishReason: %s", resp.FinishReason)

	if len(deltas) < 2 {
		t.Errorf("expected multiple stream deltas, got %d", len(deltas))
	}
	if resp.OutputText == "" {
		t.Error("expected non-empty OutputText")
	}
	if resp.OutputText != joined {
		t.Errorf("OutputText should equal concatenated deltas:\n  OutputText: %q\n  Joined:     %q", resp.OutputText, joined)
	}
}

// ---------------------------------------------------------------------------
// Single tool call
// ---------------------------------------------------------------------------

func TestOllamaLive_SingleToolCall(t *testing.T) {
	skipUnlessLive(t)

	oc := NewOllamaClient(liveEndpointFromEnv(), liveModelFromEnv())
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	resp, err := oc.Complete(ctx, CompletionRequest{
		Messages: []Message{
			{Role: "user", Content: NewTextContent("What is the current weather in Tokyo? Use the get_weather tool.")},
		},
		Tools: []Tool{
			{
				Type: "function",
				Function: FunctionDef{
					Name:        "get_weather",
					Description: "Get the current weather for a city",
					Parameters: map[string]any{
						"type": "object",
						"properties": map[string]any{
							"city": map[string]any{
								"type":        "string",
								"description": "City name",
							},
						},
						"required": []string{"city"},
					},
				},
			},
		},
		Temperature: 0,
		MaxTokens:   2000,
	})
	if err != nil {
		t.Fatalf("Complete failed: %v", err)
	}

	t.Logf("OutputText: %q", resp.OutputText)
	t.Logf("FinishReason: %s", resp.FinishReason)
	t.Logf("ToolCalls: %d", len(resp.ToolCalls))

	if len(resp.ToolCalls) == 0 {
		t.Fatal("expected at least 1 tool call, got 0 — model did not call the tool")
	}

	tc := resp.ToolCalls[0]
	t.Logf("ToolCall[0]: id=%q name=%q args=%s", tc.ID, tc.Name, string(tc.Arguments))

	if tc.Name != "get_weather" {
		t.Errorf("expected tool name 'get_weather', got %q", tc.Name)
	}
	if tc.ID == "" {
		t.Error("expected non-empty tool call ID")
	}

	// Verify arguments are valid JSON with a city field
	var args map[string]any
	if err := json.Unmarshal(tc.Arguments, &args); err != nil {
		t.Fatalf("tool call arguments not valid JSON: %v", err)
	}
	city, ok := args["city"]
	if !ok {
		t.Error("expected 'city' in tool call arguments")
	} else {
		t.Logf("City: %v", city)
	}

	if resp.FinishReason != "tool_use" {
		t.Errorf("FinishReason: got %q, want %q", resp.FinishReason, "tool_use")
	}
}

// ---------------------------------------------------------------------------
// Tool call → tool result → final response (full round-trip)
// ---------------------------------------------------------------------------

func TestOllamaLive_ToolRoundTrip(t *testing.T) {
	skipUnlessLive(t)

	oc := NewOllamaClient(liveEndpointFromEnv(), liveModelFromEnv())
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	tools := []Tool{
		{
			Type: "function",
			Function: FunctionDef{
				Name:        "calculate",
				Description: "Evaluate a math expression and return the result",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"expression": map[string]any{
							"type":        "string",
							"description": "The math expression to evaluate, e.g. '2+3'",
						},
					},
					"required": []string{"expression"},
				},
			},
		},
	}

	// Step 1: User asks, model should call calculate tool
	resp1, err := oc.Complete(ctx, CompletionRequest{
		Messages: []Message{
			{Role: "user", Content: NewTextContent("What is 42 * 17? Use the calculate tool.")},
		},
		Tools:       tools,
		Temperature: 0,
		MaxTokens:   2000,
	})
	if err != nil {
		t.Fatalf("Step 1 failed: %v", err)
	}

	t.Logf("Step 1 — ToolCalls: %d, FinishReason: %s", len(resp1.ToolCalls), resp1.FinishReason)

	if len(resp1.ToolCalls) == 0 {
		t.Fatalf("Step 1: expected tool call, got none. OutputText: %q", resp1.OutputText)
	}

	tc := resp1.ToolCalls[0]
	t.Logf("Step 1 — Tool: %s, Args: %s, ID: %s", tc.Name, string(tc.Arguments), tc.ID)

	// Step 2: Build conversation with tool result, send back for final answer
	// Reconstruct the conversation as ShanClaw would store it (Anthropic format)
	assistantBlocks := []ContentBlock{
		NewToolUseBlock(tc.ID, tc.Name, tc.Arguments),
	}
	if resp1.OutputText != "" {
		assistantBlocks = append([]ContentBlock{{Type: "text", Text: resp1.OutputText}}, assistantBlocks...)
	}

	resultBlocks := []ContentBlock{
		NewToolResultBlock(tc.ID, "714", false),
	}

	resp2, err := oc.Complete(ctx, CompletionRequest{
		Messages: []Message{
			{Role: "user", Content: NewTextContent("What is 42 * 17? Use the calculate tool.")},
			{Role: "assistant", Content: NewBlockContent(assistantBlocks)},
			{Role: "user", Content: NewBlockContent(resultBlocks)},
		},
		Tools:       tools,
		Temperature: 0,
		MaxTokens:   2000,
	})
	if err != nil {
		t.Fatalf("Step 2 failed: %v", err)
	}

	t.Logf("Step 2 — OutputText: %q", resp2.OutputText)
	t.Logf("Step 2 — FinishReason: %s", resp2.FinishReason)
	t.Logf("Step 2 — ToolCalls: %d", len(resp2.ToolCalls))

	if resp2.OutputText == "" {
		t.Error("Step 2: expected text response with the answer")
	}
	if !strings.Contains(resp2.OutputText, "714") {
		t.Errorf("Step 2: expected output to mention 714, got: %q", resp2.OutputText)
	}
}

// ---------------------------------------------------------------------------
// Multiple tools offered, model selects the right one
// ---------------------------------------------------------------------------

func TestOllamaLive_MultipleToolsSelectsCorrect(t *testing.T) {
	skipUnlessLive(t)

	oc := NewOllamaClient(liveEndpointFromEnv(), liveModelFromEnv())
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	resp, err := oc.Complete(ctx, CompletionRequest{
		Messages: []Message{
			{Role: "user", Content: NewTextContent("Read the file at /tmp/test.txt. Use the appropriate tool.")},
		},
		Tools: []Tool{
			{
				Type: "function",
				Function: FunctionDef{
					Name:        "bash",
					Description: "Execute a shell command",
					Parameters: map[string]any{
						"type": "object",
						"properties": map[string]any{
							"command": map[string]any{"type": "string", "description": "Shell command to execute"},
						},
						"required": []string{"command"},
					},
				},
			},
			{
				Type: "function",
				Function: FunctionDef{
					Name:        "file_read",
					Description: "Read contents of a file at the given path",
					Parameters: map[string]any{
						"type": "object",
						"properties": map[string]any{
							"path": map[string]any{"type": "string", "description": "Absolute file path to read"},
						},
						"required": []string{"path"},
					},
				},
			},
			{
				Type: "function",
				Function: FunctionDef{
					Name:        "file_write",
					Description: "Write content to a file",
					Parameters: map[string]any{
						"type": "object",
						"properties": map[string]any{
							"path":    map[string]any{"type": "string", "description": "File path"},
							"content": map[string]any{"type": "string", "description": "Content to write"},
						},
						"required": []string{"path", "content"},
					},
				},
			},
		},
		Temperature: 0,
		MaxTokens:   2000,
	})
	if err != nil {
		t.Fatalf("Complete failed: %v", err)
	}

	t.Logf("ToolCalls: %d, FinishReason: %s", len(resp.ToolCalls), resp.FinishReason)
	if len(resp.ToolCalls) > 0 {
		for i, tc := range resp.ToolCalls {
			t.Logf("ToolCall[%d]: name=%q args=%s", i, tc.Name, string(tc.Arguments))
		}
	}

	if len(resp.ToolCalls) == 0 {
		t.Fatalf("expected tool call, got none. OutputText: %q", resp.OutputText)
	}

	tc := resp.ToolCalls[0]
	// Should pick file_read (or bash with cat) — either is acceptable
	if tc.Name != "file_read" && tc.Name != "bash" {
		t.Errorf("expected file_read or bash, got %q", tc.Name)
	}
	if tc.Name == "file_read" {
		var args map[string]any
		json.Unmarshal(tc.Arguments, &args)
		if path, ok := args["path"].(string); !ok || !strings.Contains(path, "test.txt") {
			t.Errorf("expected path containing test.txt, got: %v", args["path"])
		}
	}
}

// ---------------------------------------------------------------------------
// No tools — pure text, verify clean response
// ---------------------------------------------------------------------------

func TestOllamaLive_NoTools(t *testing.T) {
	skipUnlessLive(t)

	oc := NewOllamaClient(liveEndpointFromEnv(), liveModelFromEnv())
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	resp, err := oc.Complete(ctx, CompletionRequest{
		Messages: []Message{
			{Role: "user", Content: NewTextContent("What is 2+2? Reply with just the number.")},
		},
		Temperature: 0,
		MaxTokens:   2000,
	})
	if err != nil {
		t.Fatalf("Complete failed: %v", err)
	}

	t.Logf("OutputText: %q", resp.OutputText)

	if !strings.Contains(resp.OutputText, "4") {
		t.Errorf("expected '4' in output, got: %q", resp.OutputText)
	}
	if len(resp.ToolCalls) > 0 {
		t.Errorf("expected no tool calls without tools, got %d", len(resp.ToolCalls))
	}
}
