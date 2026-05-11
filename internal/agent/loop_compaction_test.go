package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

// TestAgentLoop_CompactionAndMemoryPersist verifies the full compaction chain:
//
//  1. Agent loop runs multiple tool-call iterations within a single Run()
//  2. Mock server reports growing input tokens each iteration
//  3. When tokens exceed 90% of context_window → compaction triggers
//  4. PersistLearnings fires (small tier) → writes to MEMORY.md
//  5. GenerateSummary fires (small tier) → creates summary
//  6. ShapeHistory reduces messages
//
// Uses context_window=2000 so 90% threshold = 1800 tokens.
// Needs ≥5 tool iterations so messages > MinShapeable (9).
func TestAgentLoop_CompactionAndMemoryPersist(t *testing.T) {
	memoryDir := t.TempDir()

	var mu sync.Mutex
	var calls []string // ordered log of all calls

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := readBody(r.Body)
		defer r.Body.Close()

		var req struct {
			ModelTier string `json:"model_tier"`
			Messages  []struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		json.Unmarshal(raw, &req)

		mu.Lock()
		callNum := len(calls) + 1

		// Identify small-tier calls
		if req.ModelTier == "small" {
			isPersist := false
			isSummary := false
			for _, m := range req.Messages {
				var text string
				json.Unmarshal(m.Content, &text)
				if strings.Contains(text, "extracting durable knowledge") {
					isPersist = true
				}
				if strings.Contains(text, "Compress the following conversation") {
					isSummary = true
				}
			}

			if isPersist {
				calls = append(calls, fmt.Sprintf("call %d: PERSIST", callNum))
				mu.Unlock()
				t.Logf("Call %d: [small] PersistLearnings (messages: %d)", callNum, len(req.Messages))
				json.NewEncoder(w).Encode(nativeResponse(
					"- Agent discussed system architecture\n- Testing compaction flow",
					"end_turn", nil, 50, 30))
				return
			}
			if isSummary {
				calls = append(calls, fmt.Sprintf("call %d: SUMMARY", callNum))
				mu.Unlock()
				t.Logf("Call %d: [small] GenerateSummary", callNum)
				json.NewEncoder(w).Encode(nativeResponse(
					"User asked about architecture. Agent reasoned through multiple steps.",
					"end_turn", nil, 50, 30))
				return
			}

			calls = append(calls, fmt.Sprintf("call %d: small-other", callNum))
			mu.Unlock()
			t.Logf("Call %d: [small] other", callNum)
			json.NewEncoder(w).Encode(nativeResponse("ok", "end_turn", nil, 50, 30))
			return
		}

		// Main-tier calls: use message count to decide behavior.
		// We need the loop to iterate 6+ times so messages exceed MinShapeable (9).
		// Report input tokens that grow to exceed the 1700 threshold.
		msgCount := len(req.Messages)
		// Scale input tokens based on message count to simulate realistic growth
		inputTokens := msgCount * 200

		if msgCount < 12 {
			// Keep looping with tool calls until we have enough messages
			calls = append(calls, fmt.Sprintf("call %d: TOOL (msgs=%d, input=%d)", callNum, msgCount, inputTokens))
			mu.Unlock()
			t.Logf("Call %d: [main] tool_use (msgs=%d, input_tokens=%d)", callNum, msgCount, inputTokens)
			json.NewEncoder(w).Encode(nativeResponse(
				"", "tool_use",
				toolCall("think", fmt.Sprintf(`{"thought":"Analyzing step with %d messages in context"}`, msgCount)),
				inputTokens, 100))
		} else {
			calls = append(calls, fmt.Sprintf("call %d: END_TURN (msgs=%d, input=%d)", callNum, msgCount, inputTokens))
			mu.Unlock()
			t.Logf("Call %d: [main] end_turn (msgs=%d, input_tokens=%d)", callNum, msgCount, inputTokens)
			json.NewEncoder(w).Encode(nativeResponse(
				"Here is the complete analysis based on my reasoning through all the steps.",
				"end_turn", nil, inputTokens, 100))
		}
	}))
	defer server.Close()

	gw := client.NewGatewayClient(server.URL, "")
	reg := NewToolRegistry()

	// Register think tool — no approval needed, keeps loop iterating
	reg.Register(&thinkTool{})

	handler := &mockHandler{approveResult: true}

	loop := NewAgentLoop(gw, reg, "medium", "", 20, 2000, 200, nil, nil, nil)
	loop.SetContextWindow(2000) // 90% = 1800 triggers compaction
	loop.SetMemoryDir(memoryDir)
	loop.SetHandler(handler)

	// Run with a big message
	result, usage, err := loop.Run(context.Background(),
		"Explain the complete system architecture. Think through each component step by step. Be thorough.",
		nil, nil)
	if err != nil {
		t.Logf("Run error (may be iteration limit): %v", err)
	}

	mu.Lock()
	t.Logf("\n=== Call sequence (%d total) ===", len(calls))
	for _, c := range calls {
		t.Logf("  %s", c)
	}

	hasPersist := false
	hasSummary := false
	for _, c := range calls {
		if strings.Contains(c, "PERSIST") {
			hasPersist = true
		}
		if strings.Contains(c, "SUMMARY") {
			hasSummary = true
		}
	}
	mu.Unlock()

	t.Logf("Result: %d chars", len(result))
	t.Logf("Usage: %d LLM calls, %d input+output tokens",
		usage.LLMCalls, usage.InputTokens+usage.OutputTokens)

	// Check compaction fired
	if !hasPersist {
		t.Error("PersistLearnings should have fired during compaction")
	}
	if !hasSummary {
		t.Error("GenerateSummary should have fired during compaction")
	}

	// Check MEMORY.md
	memPath := filepath.Join(memoryDir, "MEMORY.md")
	memData, err := os.ReadFile(memPath)
	if err != nil {
		if hasPersist {
			t.Fatalf("MEMORY.md should exist since PersistLearnings fired: %v", err)
		}
		t.Logf("MEMORY.md not created — compaction didn't trigger")
		return
	}

	memContent := string(memData)
	t.Logf("\n=== MEMORY.md ===\n%s", memContent)

	if !strings.Contains(memContent, "Auto-persisted") {
		t.Error("MEMORY.md should contain Auto-persisted section")
	}
}

// thinkTool is a minimal think tool for the compaction test.
type thinkTool struct{}

func (t *thinkTool) Info() ToolInfo {
	return ToolInfo{
		Name:        "think",
		Description: "Plan or reason through tasks",
		Parameters:  map[string]any{"type": "object", "properties": map[string]any{"thought": map[string]any{"type": "string"}}},
		Required:    []string{"thought"},
	}
}

func (t *thinkTool) Run(ctx context.Context, args string) (ToolResult, error) {
	return ToolResult{Content: "Thought recorded."}, nil
}

func (t *thinkTool) RequiresApproval() bool { return false }

func readBody(body interface{ Read([]byte) (int, error) }) ([]byte, error) {
	var buf []byte
	tmp := make([]byte, 4096)
	for {
		n, err := body.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			break
		}
	}
	return buf, nil
}

func TestTruncateHeadTail(t *testing.T) {
	t.Run("short string unchanged", func(t *testing.T) {
		s := "hello world"
		got := truncateHeadTail(s, 100)
		if got != s {
			t.Errorf("expected unchanged, got %q", got)
		}
	})

	t.Run("exact limit unchanged", func(t *testing.T) {
		s := "abcdefghij" // 10 runes
		got := truncateHeadTail(s, 10)
		if got != s {
			t.Errorf("expected unchanged, got %q", got)
		}
	})

	t.Run("long string gets head+tail", func(t *testing.T) {
		// 100 chars, truncate to 40
		s := strings.Repeat("a", 50) + strings.Repeat("z", 50)
		got := truncateHeadTail(s, 40)
		// keepHead=30, keepTail=10
		if !strings.HasPrefix(got, strings.Repeat("a", 30)) {
			t.Errorf("expected head of 30 'a's, got prefix: %q", got[:40])
		}
		if !strings.HasSuffix(got, strings.Repeat("z", 10)) {
			t.Errorf("expected tail of 10 'z's, got suffix: %q", got[len(got)-20:])
		}
		if !strings.Contains(got, "[... truncated 60 chars ...]") {
			t.Errorf("expected truncation marker with 60 dropped chars, got: %q", got)
		}
	})

	t.Run("rune-safe with multibyte", func(t *testing.T) {
		// 20 runes of 3 bytes each
		s := strings.Repeat("日", 20)
		got := truncateHeadTail(s, 10)
		// keepHead=7, keepTail=2
		runes := []rune(got)
		// Should start with 7 日 and end with 2 日
		if runes[0] != '日' || runes[len(runes)-1] != '日' {
			t.Errorf("expected rune-safe truncation, got: %q", got)
		}
		if !strings.Contains(got, "[... truncated 10 chars ...]") {
			t.Errorf("expected truncation marker, got: %q", got)
		}
	})
}

func TestBuildToolCallMap(t *testing.T) {
	messages := []client.Message{
		{
			Role: "assistant",
			Content: client.NewBlockContent([]client.ContentBlock{
				client.NewToolUseBlock("tu-1", "file_read", json.RawMessage(`{"path":"/tmp/foo.txt"}`)),
				client.NewToolUseBlock("tu-2", "bash", json.RawMessage(`{"command":"echo hello"}`)),
			}),
		},
		{
			Role: "user",
			Content: client.NewBlockContent([]client.ContentBlock{
				client.NewToolResultBlock("tu-1", "file contents here", false),
			}),
		},
	}

	m := buildToolCallMap(messages)
	if len(m) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(m))
	}
	if m["tu-1"].Name != "file_read" {
		t.Errorf("expected file_read, got %q", m["tu-1"].Name)
	}
	if m["tu-2"].Name != "bash" {
		t.Errorf("expected bash, got %q", m["tu-2"].Name)
	}
	if !strings.Contains(m["tu-1"].Args, "/tmp/foo.txt") {
		t.Errorf("expected args to contain path, got %q", m["tu-1"].Args)
	}
}

func TestBuildToolCallMap_LongArgsTruncated(t *testing.T) {
	longArgs := `{"content":"` + strings.Repeat("x", 200) + `"}`
	messages := []client.Message{
		{
			Role: "assistant",
			Content: client.NewBlockContent([]client.ContentBlock{
				client.NewToolUseBlock("tu-1", "file_write", json.RawMessage(longArgs)),
			}),
		},
	}

	m := buildToolCallMap(messages)
	if len(m["tu-1"].Args) > 104 { // 100 + "..."
		t.Errorf("expected args truncated to ~103 chars, got %d", len(m["tu-1"].Args))
	}
}

func TestCompressOldToolResults_TieredBehavior(t *testing.T) {
	// Create 25 tool result pairs to exercise all three tiers with current constants:
	// tier1Threshold=20, keepRecent passed as 8 to match compressAfter.
	const numTools = 25
	const keepRecent = 8

	var messages []client.Message
	messages = append(messages, client.Message{
		Role:    "user",
		Content: client.NewTextContent("Do some work"),
	})

	for i := 0; i < numTools; i++ {
		id := fmt.Sprintf("tu-%d", i)
		name := fmt.Sprintf("tool_%d", i)
		args := json.RawMessage(fmt.Sprintf(`{"arg":"value_%d"}`, i))
		content := fmt.Sprintf("Result content for tool %d: %s", i, strings.Repeat("x", 500))

		messages = append(messages, client.Message{
			Role: "assistant",
			Content: client.NewBlockContent([]client.ContentBlock{
				client.NewToolUseBlock(id, name, args),
			}),
		})
		messages = append(messages, client.Message{
			Role: "user",
			Content: client.NewBlockContent([]client.ContentBlock{
				client.NewToolResultBlock(id, content, false),
			}),
		})
	}

	compressOldToolResults(context.Background(), messages, keepRecent, 300, nil)

	for i := 0; i < numTools; i++ {
		msgIdx := 2 + i*2
		msg := messages[msgIdx]
		blocks := msg.Content.Blocks()
		if len(blocks) == 0 {
			t.Fatalf("tool result %d: no blocks", i)
		}
		resultContent := ""
		for _, b := range blocks {
			if b.Type == "tool_result" {
				if s, ok := b.ToolContent.(string); ok {
					resultContent = s
				}
			}
		}

		distFromEnd := (numTools - 1) - i

		if distFromEnd < keepRecent {
			// Tier 3: should be full (500+ chars)
			if len(resultContent) < 500 {
				t.Errorf("tool %d (dist=%d): expected tier 3 full content (%d chars), got %d chars",
					i, distFromEnd, 500, len(resultContent))
			}
		} else if distFromEnd >= 20 {
			// Tier 1: should contain "snipped"
			if !strings.Contains(resultContent, "snipped") {
				t.Errorf("tool %d (dist=%d): expected tier 1 metadata with 'snipped', got: %q",
					i, distFromEnd, resultContent)
			}
		} else {
			// Tier 2: should be truncated but not snipped (head+tail)
			if strings.Contains(resultContent, "snipped") {
				t.Errorf("tool %d (dist=%d): tier 2 should not contain 'snipped', got: %q",
					i, distFromEnd, resultContent)
			}
			if len(resultContent) > 400 {
				t.Errorf("tool %d (dist=%d): expected tier 2 truncated to ~300 chars, got %d",
					i, distFromEnd, len(resultContent))
			}
			if !strings.Contains(resultContent, "[... truncated") {
				t.Errorf("tool %d (dist=%d): expected head+tail truncation marker, got: %q",
					i, distFromEnd, resultContent)
			}
		}
	}
}

func TestCompressOldToolResults_Tier2FloorForReadTools(t *testing.T) {
	// Verify that file_read and grep results never degrade to Tier 1 metadata stubs,
	// even when they would normally be old enough for Tier 1.
	const numTools = 26
	var messages []client.Message
	messages = append(messages, client.Message{
		Role:    "user",
		Content: client.NewTextContent("Start"),
	})

	// Tools 0-4: floor tools, 5-25: normal tools.
	// With 26 total results, tool 5 sits exactly at distFromEnd=20, so it should
	// hit Tier 1 and serve as the non-floor control case.
	for i := 0; i < numTools; i++ {
		id := fmt.Sprintf("tu-%d", i)
		name := "tool_other"
		if i < 3 {
			name = "file_read"
		} else if i < 5 {
			name = "grep"
		}
		args := json.RawMessage(fmt.Sprintf(`{"arg":"value_%d"}`, i))
		content := fmt.Sprintf("Result %d: %s", i, strings.Repeat("x", 500))

		messages = append(messages, client.Message{
			Role: "assistant",
			Content: client.NewBlockContent([]client.ContentBlock{
				client.NewToolUseBlock(id, name, args),
			}),
		})
		messages = append(messages, client.Message{
			Role: "user",
			Content: client.NewBlockContent([]client.ContentBlock{
				client.NewToolResultBlock(id, content, false),
			}),
		})
	}

	compressOldToolResults(context.Background(), messages, 8, 300, nil)

	// Check the oldest file_read/grep results (tools 0-4, dist 25-21 from end)
	// These should be Tier 2 (truncated with head+tail), NOT Tier 1 (snipped).
	for i := 0; i < 5; i++ {
		msgIdx := 2 + i*2
		blocks := messages[msgIdx].Content.Blocks()
		resultContent := ""
		for _, b := range blocks {
			if b.Type == "tool_result" {
				if s, ok := b.ToolContent.(string); ok {
					resultContent = s
				}
			}
		}
		if strings.Contains(resultContent, "snipped") {
			t.Errorf("floor tool %d: should not be Tier 1 (snipped), got: %q", i, resultContent[:80])
		}
		if !strings.Contains(resultContent, "[... truncated") {
			t.Errorf("floor tool %d: should be Tier 2 (truncated), got: %q", i, resultContent[:80])
		}
	}

	// Non-floor control: tool 5 is old enough for Tier 1 and should become metadata-only.
	normalIdx := 2 + 5*2
	blocks := messages[normalIdx].Content.Blocks()
	resultContent := ""
	for _, b := range blocks {
		if b.Type == "tool_result" {
			if s, ok := b.ToolContent.(string); ok {
				resultContent = s
			}
		}
	}
	if !strings.Contains(resultContent, "snipped") {
		t.Fatalf("non-floor tool should be Tier 1 (snipped), got: %q", resultContent[:80])
	}
	if strings.Contains(resultContent, "[... truncated") {
		t.Fatalf("non-floor tool should not stay in Tier 2, got: %q", resultContent[:80])
	}
}

func TestCompressOldToolResults_EmergencyMode(t *testing.T) {
	// Simulate emergency compaction: keepRecent=1, maxChars=100
	var messages []client.Message
	messages = append(messages, client.Message{
		Role:    "user",
		Content: client.NewTextContent("Start"),
	})

	for i := 0; i < 5; i++ {
		id := fmt.Sprintf("tu-%d", i)
		content := strings.Repeat("y", 300)
		messages = append(messages, client.Message{
			Role: "assistant",
			Content: client.NewBlockContent([]client.ContentBlock{
				client.NewToolUseBlock(id, "bash", json.RawMessage(`{"command":"ls"}`)),
			}),
		})
		messages = append(messages, client.Message{
			Role: "user",
			Content: client.NewBlockContent([]client.ContentBlock{
				client.NewToolResultBlock(id, content, false),
			}),
		})
	}

	compressOldToolResults(context.Background(), messages, 1, 100, nil)

	// Only the last tool result should be full
	for i := 0; i < 5; i++ {
		msgIdx := 2 + i*2
		blocks := messages[msgIdx].Content.Blocks()
		for _, b := range blocks {
			if b.Type == "tool_result" {
				s, ok := b.ToolContent.(string)
				if !ok {
					continue
				}
				if i == 4 {
					// Last one: tier 3, full
					if len(s) < 300 {
						t.Errorf("last tool result should be full, got %d chars", len(s))
					}
				} else {
					// All others should be compressed
					if len(s) >= 300 {
						t.Errorf("tool %d should be compressed, got %d chars", i, len(s))
					}
				}
			}
		}
	}
}

// TestAgentLoop_ReactiveCompaction verifies the reactive compaction safety net:
//
//  1. Agent loop has enough messages to build context (6+ tool iterations)
//  2. Mock server returns HTTP 400 "prompt is too long" after sufficient iterations
//  3. Reactive compaction fires: PersistLearnings → compress → summary → ShapeHistory
//  4. Retry succeeds with compacted messages
//  5. compactionApplied flag prevents infinite retry loops
//
// The proactive compaction is bypassed by reporting low input tokens until the
// server triggers the 400 error, simulating the case where token counting
// underestimates and the API rejects the request.
func TestAgentLoop_ReactiveCompaction(t *testing.T) {
	memoryDir := t.TempDir()

	var mu sync.Mutex
	var calls []string

	// After 6 tool iterations (13+ messages), return a 400 context-length error
	// on the next main-tier call, then succeed on retry.
	contextErrorReturned := false
	retrySucceeded := false

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := readBody(r.Body)
		defer r.Body.Close()

		var req struct {
			ModelTier string `json:"model_tier"`
			Messages  []struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		json.Unmarshal(raw, &req)

		mu.Lock()
		callNum := len(calls) + 1

		// Small-tier calls (PersistLearnings, GenerateSummary)
		if req.ModelTier == "small" {
			isPersist := false
			isSummary := false
			for _, m := range req.Messages {
				var text string
				json.Unmarshal(m.Content, &text)
				if strings.Contains(text, "extracting durable knowledge") {
					isPersist = true
				}
				if strings.Contains(text, "Compress the following conversation") {
					isSummary = true
				}
			}

			if isPersist {
				calls = append(calls, fmt.Sprintf("call %d: PERSIST", callNum))
				mu.Unlock()
				t.Logf("Call %d: [small] PersistLearnings (messages: %d)", callNum, len(req.Messages))
				json.NewEncoder(w).Encode(nativeResponse(
					"- Agent was analyzing system architecture\n- Reactive compaction triggered",
					"end_turn", nil, 50, 30))
				return
			}
			if isSummary {
				calls = append(calls, fmt.Sprintf("call %d: SUMMARY", callNum))
				mu.Unlock()
				t.Logf("Call %d: [small] GenerateSummary", callNum)
				json.NewEncoder(w).Encode(nativeResponse(
					"User asked about architecture. Agent analyzed multiple components before context overflow.",
					"end_turn", nil, 50, 30))
				return
			}

			calls = append(calls, fmt.Sprintf("call %d: small-other", callNum))
			mu.Unlock()
			json.NewEncoder(w).Encode(nativeResponse("ok", "end_turn", nil, 50, 30))
			return
		}

		// Main-tier calls
		msgCount := len(req.Messages)

		if msgCount < 12 {
			// Keep looping with tool calls, report LOW tokens so proactive
			// compaction does NOT trigger (under 90% of 128000).
			calls = append(calls, fmt.Sprintf("call %d: TOOL (msgs=%d)", callNum, msgCount))
			mu.Unlock()
			t.Logf("Call %d: [main] tool_use (msgs=%d)", callNum, msgCount)
			json.NewEncoder(w).Encode(nativeResponse(
				"", "tool_use",
				toolCall("think", fmt.Sprintf(`{"thought":"Step %d analysis"}`, msgCount)),
				500, 100)) // Low tokens — proactive compaction won't trigger
			return
		}

		// At 12+ messages: return 400 context-length error (once)
		if !contextErrorReturned {
			contextErrorReturned = true
			calls = append(calls, fmt.Sprintf("call %d: CONTEXT_ERROR (msgs=%d)", callNum, msgCount))
			mu.Unlock()
			t.Logf("Call %d: [main] → 400 prompt is too long (msgs=%d)", callNum, msgCount)
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"error":{"type":"invalid_request_error","message":"prompt is too long"}}`))
			return
		}

		// After reactive compaction retries: succeed
		retrySucceeded = true
		calls = append(calls, fmt.Sprintf("call %d: RETRY_SUCCESS (msgs=%d)", callNum, msgCount))
		mu.Unlock()
		t.Logf("Call %d: [main] end_turn after reactive compaction (msgs=%d)", callNum, msgCount)
		json.NewEncoder(w).Encode(nativeResponse(
			"Analysis complete after reactive compaction.",
			"end_turn", nil, 800, 100))
	}))
	defer server.Close()

	gw := client.NewGatewayClient(server.URL, "")
	reg := NewToolRegistry()
	reg.Register(&thinkTool{})

	handler := &mockHandler{approveResult: true}

	loop := NewAgentLoop(gw, reg, "medium", "", 20, 2000, 200, nil, nil, nil)
	loop.SetContextWindow(128000) // High window so proactive compaction doesn't trigger
	loop.SetMemoryDir(memoryDir)
	loop.SetHandler(handler)

	result, usage, err := loop.Run(context.Background(),
		"Analyze each component of the system. Think through every step carefully.",
		nil, nil)
	if err != nil {
		t.Logf("Run error: %v", err)
	}

	mu.Lock()
	t.Logf("\n=== Call sequence (%d total) ===", len(calls))
	for _, c := range calls {
		t.Logf("  %s", c)
	}

	hasPersist := false
	hasSummary := false
	hasContextError := false
	hasRetrySuccess := false
	for _, c := range calls {
		if strings.Contains(c, "PERSIST") {
			hasPersist = true
		}
		if strings.Contains(c, "SUMMARY") {
			hasSummary = true
		}
		if strings.Contains(c, "CONTEXT_ERROR") {
			hasContextError = true
		}
		if strings.Contains(c, "RETRY_SUCCESS") {
			hasRetrySuccess = true
		}
	}
	mu.Unlock()

	t.Logf("Result: %d chars", len(result))
	t.Logf("Usage: %d LLM calls", usage.LLMCalls)

	// Verify reactive compaction chain
	if !hasContextError {
		t.Error("expected context-length 400 error to be returned by mock server")
	}
	if !hasPersist {
		t.Error("PersistLearnings should fire during reactive compaction")
	}
	if !hasSummary {
		t.Error("GenerateSummary should fire during reactive compaction")
	}
	if !hasRetrySuccess {
		t.Error("retry after reactive compaction should succeed")
	}
	if !retrySucceeded {
		t.Error("retrySucceeded flag should be true")
	}

	// Verify MEMORY.md was written
	memPath := filepath.Join(memoryDir, "MEMORY.md")
	memData, err := os.ReadFile(memPath)
	if err != nil {
		t.Fatalf("MEMORY.md should exist after reactive PersistLearnings: %v", err)
	}
	memContent := string(memData)
	t.Logf("\n=== MEMORY.md ===\n%s", memContent)
	if !strings.Contains(memContent, "Auto-persisted") {
		t.Error("MEMORY.md should contain Auto-persisted section")
	}

	// Verify result came through
	if result == "" {
		t.Error("expected non-empty result after successful retry")
	}
}

// TestAgentLoop_ReactiveCompactionNoDoubleRetry verifies the compactionApplied
// guard prevents infinite loops: if reactive compaction fires but the retry
// ALSO returns a context-length error, the loop should fail instead of retrying.
func TestAgentLoop_ReactiveCompactionNoDoubleRetry(t *testing.T) {
	contextErrors := 0

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := readBody(r.Body)
		defer r.Body.Close()

		var req struct {
			ModelTier string `json:"model_tier"`
			Messages  []struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		json.Unmarshal(raw, &req)

		// Small-tier: always succeed
		if req.ModelTier == "small" {
			for _, m := range req.Messages {
				var text string
				json.Unmarshal(m.Content, &text)
				if strings.Contains(text, "extracting durable knowledge") {
					json.NewEncoder(w).Encode(nativeResponse("learnings", "end_turn", nil, 50, 30))
					return
				}
				if strings.Contains(text, "Compress the following conversation") {
					json.NewEncoder(w).Encode(nativeResponse("summary", "end_turn", nil, 50, 30))
					return
				}
			}
			json.NewEncoder(w).Encode(nativeResponse("ok", "end_turn", nil, 50, 30))
			return
		}

		msgCount := len(req.Messages)
		t.Logf("Main-tier call: msgs=%d, contextErrors=%d", msgCount, contextErrors)

		if msgCount < 6 && contextErrors == 0 {
			// Build up messages with tool calls until we first trigger overflow.
			json.NewEncoder(w).Encode(nativeResponse(
				"", "tool_use",
				toolCall("think", `{"thought":"building context"}`),
				500, 100))
			return
		}

		// Always return context-length error once we've started — even after
		// compaction reduces message count. This forces the double-retry guard.
		contextErrors++
		t.Logf("Returning context-length error #%d (msgs=%d)", contextErrors, msgCount)
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":{"type":"invalid_request_error","message":"context_length_exceeded"}}`))
	}))
	defer server.Close()

	gw := client.NewGatewayClient(server.URL, "")
	reg := NewToolRegistry()
	reg.Register(&thinkTool{})

	handler := &mockHandler{approveResult: true}
	loop := NewAgentLoop(gw, reg, "medium", "", 20, 2000, 200, nil, nil, nil)
	loop.SetContextWindow(128000)
	loop.SetMemoryDir(t.TempDir())
	loop.SetHandler(handler)

	_, _, err := loop.Run(context.Background(), "Trigger reactive compaction that fails on retry too.", nil, nil)

	// Should get an error — NOT an infinite loop
	if err == nil {
		t.Fatal("expected error when retry after reactive compaction also fails")
	}
	t.Logf("Got expected error: %v", err)

	// Should have seen at most 2 context-length errors (original + one retry)
	if contextErrors > 2 {
		t.Errorf("expected at most 2 context-length errors (original + retry), got %d — infinite loop guard may be broken", contextErrors)
	}
}

func TestReactiveSummaryInput_InsertsPriorSummaryOnce(t *testing.T) {
	messages := []client.Message{
		{Role: "system", Content: client.NewTextContent("system")},
		{Role: "user", Content: client.NewTextContent("first user")},
		{Role: "assistant", Content: client.NewTextContent("recent reply")},
	}

	withSummary := reactiveSummaryInput(messages, "Earlier work happened")
	if len(withSummary) != len(messages)+1 {
		t.Fatalf("expected injected summary message, got %d messages", len(withSummary))
	}
	if got := withSummary[2].Content.Text(); got != "Previous context summary: Earlier work happened" {
		t.Fatalf("unexpected injected summary message: %q", got)
	}

	again := reactiveSummaryInput(withSummary, "Earlier work happened")
	if len(again) != len(withSummary) {
		t.Fatal("summary should not be injected twice")
	}
}

func TestAgentLoop_ReactiveCompaction_UsesEmergencyFallbackWhenSoftStillOverBudget(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	mainCalls := 0

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := readBody(r.Body)
		defer r.Body.Close()

		var req struct {
			ModelTier string `json:"model_tier"`
			Messages  []struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		json.Unmarshal(raw, &req)

		mu.Lock()
		defer mu.Unlock()

		if req.ModelTier == "small" {
			calls = append(calls, "summary")
			json.NewEncoder(w).Encode(nativeResponse(
				"condensed summary",
				"end_turn", nil, 50, 30))
			return
		}

		mainCalls++
		if mainCalls == 1 {
			calls = append(calls, "context_error")
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"error":{"type":"invalid_request_error","message":"prompt is too long"}}`))
			return
		}

		calls = append(calls, "retry_success")
		json.NewEncoder(w).Encode(nativeResponse(
			"Recovered after emergency fallback.",
			"end_turn", nil, 500, 100))
	}))
	defer server.Close()

	gw := client.NewGatewayClient(server.URL, "")
	reg := NewToolRegistry()
	reg.Register(&thinkTool{})

	loop := NewAgentLoop(gw, reg, "medium", "", 10, 2000, 200, nil, nil, nil)
	loop.SetContextWindow(100000)

	huge := strings.Repeat("x", 450000)
	// Wrap the huge first user message in a block-content envelope so the
	// shortSessionTruncate preflight (added 2026-05-11) skips it — this
	// test specifically exercises the reactive emergency fallback path,
	// which only fires when client-side defenses cannot recover the
	// prompt size pre-flight. A plain-text huge message would now get
	// truncated by TruncateOversizedLastUserMessage before reaching the
	// API and the soft compaction alone would suffice. Real-world
	// equivalent: a session whose bulk lives in tool_result / image
	// blocks, which our preflight intentionally leaves untouched.
	history := []client.Message{
		{Role: "user", Content: client.NewBlockContent([]client.ContentBlock{
			{Type: "text", Text: huge},
		})},
		{Role: "assistant", Content: client.NewTextContent("ack")},
		{Role: "user", Content: client.NewTextContent("second turn")},
		{Role: "assistant", Content: client.NewTextContent("second reply")},
		{Role: "user", Content: client.NewTextContent("third turn")},
		{Role: "assistant", Content: client.NewTextContent("third reply")},
	}

	result, _, err := loop.Run(context.Background(), "trigger reactive overflow", nil, history)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "Recovered after emergency fallback." {
		t.Fatalf("unexpected result: %q", result)
	}

	mu.Lock()
	gotCalls := append([]string(nil), calls...)
	mu.Unlock()

	summaryCalls := 0
	for _, call := range gotCalls {
		if call == "summary" {
			summaryCalls++
		}
	}
	if summaryCalls != 2 {
		t.Fatalf("expected soft + emergency summary calls, got %d (%v)", summaryCalls, gotCalls)
	}
	if len(gotCalls) != 4 || gotCalls[0] != "context_error" || gotCalls[1] != "summary" || gotCalls[2] != "summary" || gotCalls[3] != "retry_success" {
		t.Fatalf("unexpected call order: %v", gotCalls)
	}
}

// TestAgentLoop_CompactionTriggersOnWarmCache is a regression test for the
// compaction-gate fix that sums cached tokens into the gate's input.
//
// Before the fix, lastInputTokens was assigned normalizedUsage.InputTokens —
// which Anthropic defines as *excluding* cached tokens. A long warm-cache
// session would report input_tokens of a few hundred while cache_read_tokens
// carried the real 90K+ prompt, so ShouldCompact never tripped and compaction
// never fired until the cache went cold.
//
// After the fix, totalPromptTokens(u) = input + cache_read + cache_creation,
// which reflects the real context-window consumption.
//
// This test drives the loop against a mock that always reports a small
// InputTokens but a large CacheReadTokens. Once messages grow past
// MinShapeable (9), the gate must trigger — PersistLearnings + GenerateSummary
// must both fire. If the test fails, the gate has regressed to the pre-fix
// behaviour.
func TestAgentLoop_CompactionTriggersOnWarmCache(t *testing.T) {
	memoryDir := t.TempDir()

	var mu sync.Mutex
	var calls []string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := readBody(r.Body)
		defer r.Body.Close()

		var req struct {
			ModelTier string `json:"model_tier"`
			Messages  []struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		json.Unmarshal(raw, &req)

		mu.Lock()
		callNum := len(calls) + 1

		if req.ModelTier == "small" {
			isPersist := false
			isSummary := false
			for _, m := range req.Messages {
				var text string
				json.Unmarshal(m.Content, &text)
				if strings.Contains(text, "extracting durable knowledge") {
					isPersist = true
				}
				if strings.Contains(text, "Compress the following conversation") {
					isSummary = true
				}
			}
			if isPersist {
				calls = append(calls, fmt.Sprintf("call %d: PERSIST", callNum))
				mu.Unlock()
				json.NewEncoder(w).Encode(nativeResponse(
					"- Warm-cache compaction fired correctly",
					"end_turn", nil, 50, 30))
				return
			}
			if isSummary {
				calls = append(calls, fmt.Sprintf("call %d: SUMMARY", callNum))
				mu.Unlock()
				json.NewEncoder(w).Encode(nativeResponse(
					"Agent summarised cached history.", "end_turn", nil, 50, 30))
				return
			}
			calls = append(calls, fmt.Sprintf("call %d: small-other", callNum))
			mu.Unlock()
			json.NewEncoder(w).Encode(nativeResponse("ok", "end_turn", nil, 50, 30))
			return
		}

		// Main-tier: simulate a warm cache — small InputTokens, large CacheReadTokens.
		// context_window=2000 so threshold = 1700. InputTokens alone (200) is below
		// threshold; total prompt (200 + 1800 cache_read = 2000) is above. Pre-fix
		// code reads only InputTokens and would NOT compact; post-fix reads
		// totalPromptTokens and SHOULD compact once msgCount > MinShapeable (9).
		msgCount := len(req.Messages)
		resp := client.CompletionResponse{
			Model:        "test-model",
			FinishReason: "tool_use",
			FunctionCall: nil,
			ToolCalls: []client.FunctionCall{{
				Name:      "think",
				Arguments: json.RawMessage(fmt.Sprintf(`{"thought":"step with %d msgs"}`, msgCount)),
			}},
			Usage: client.Usage{
				InputTokens:     200,
				OutputTokens:    50,
				TotalTokens:     250,
				CacheReadTokens: 1800,
			},
			RequestID: "req-test",
		}
		if msgCount >= 12 {
			// Emit end_turn so the run can terminate after compaction fires.
			resp.FinishReason = "end_turn"
			resp.ToolCalls = nil
			resp.OutputText = "Analysis complete after warm-cache compaction."
		}
		calls = append(calls, fmt.Sprintf("call %d: MAIN (msgs=%d, input=200, cache_read=1800)", callNum, msgCount))
		mu.Unlock()
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	gw := client.NewGatewayClient(server.URL, "")
	reg := NewToolRegistry()
	reg.Register(&thinkTool{})

	handler := &mockHandler{approveResult: true}

	loop := NewAgentLoop(gw, reg, "medium", "", 20, 2000, 200, nil, nil, nil)
	loop.SetContextWindow(2000)
	loop.SetMemoryDir(memoryDir)
	loop.SetHandler(handler)

	_, _, err := loop.Run(context.Background(),
		"Run through several reasoning steps so message count grows past MinShapeable.",
		nil, nil)
	if err != nil {
		t.Logf("Run error (iteration limit is acceptable): %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	t.Logf("\n=== Call sequence (%d total) ===", len(calls))
	for _, c := range calls {
		t.Logf("  %s", c)
	}

	hasPersist := false
	hasSummary := false
	for _, c := range calls {
		if strings.Contains(c, "PERSIST") {
			hasPersist = true
		}
		if strings.Contains(c, "SUMMARY") {
			hasSummary = true
		}
	}

	if !hasPersist {
		t.Error("PersistLearnings must fire once warm-cache total prompt exceeds 90% — gate regressed to pre-fix behavior")
	}
	if !hasSummary {
		t.Error("GenerateSummary must fire once warm-cache total prompt exceeds 90% — gate regressed to pre-fix behavior")
	}
}

// TestAgentLoop_EmptySummaryTriggersBackoff verifies two related fixes:
//
//  1. When GenerateSummary returns a non-error empty string (e.g. LLM produced
//     <analysis> only, extractSummary filtered to ""), the compaction gate
//     treats it as a failure and increments summaryFailures.
//  2. After 3 consecutive failures, the cool-off window of 5 iterations
//     really skips 5 iterations of SUMMARY attempts — regardless of when
//     the failures happen in the run. The pre-fix `(i - summaryFailures) < 5`
//     expression only yields a full 5-iter window when failures start at
//     i=0; a middle cluster at e.g. i=4,5,6 collapsed the window to 1 iter,
//     a late cluster at i=9,10,11 produced zero backoff at all.
//
// Post-fix assertions:
//   - Total SUMMARY count is ≤ 4 across the whole run (3 initial failures
//     plus at most one post-cool-off retry before the iter cap)
//   - At least 3 SUMMARY calls fire, so the breaker actually trips
//   - Between the 3rd and 4th SUMMARY there are ≥ 5 MAIN completion calls.
//     Every iteration emits exactly one MAIN call regardless of compaction
//     gating, so MAIN count between SUMMARYs is a direct measure of
//     iterations skipped by backoff. This is the key assertion: measuring
//     call-stream index differences (e.g. "4th SUMMARY ≥ call 3rdIndex+6")
//     would silently accept a 3-iter backoff as if it were 5, because the
//     iter that retries also contributes MAIN+PERSIST calls to the stream.
func TestAgentLoop_EmptySummaryTriggersBackoff(t *testing.T) {
	memoryDir := t.TempDir()

	var mu sync.Mutex
	var calls []string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := readBody(r.Body)
		defer r.Body.Close()

		var req struct {
			ModelTier string `json:"model_tier"`
			Messages  []struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		json.Unmarshal(raw, &req)

		mu.Lock()
		callNum := len(calls) + 1

		if req.ModelTier == "small" {
			isPersist := false
			isSummary := false
			for _, m := range req.Messages {
				var text string
				json.Unmarshal(m.Content, &text)
				if strings.Contains(text, "extracting durable knowledge") {
					isPersist = true
				}
				if strings.Contains(text, "Compress the following conversation") {
					isSummary = true
				}
			}
			if isPersist {
				calls = append(calls, fmt.Sprintf("call %d: PERSIST", callNum))
				mu.Unlock()
				json.NewEncoder(w).Encode(nativeResponse(
					"- simulated persist", "end_turn", nil, 50, 30))
				return
			}
			if isSummary {
				calls = append(calls, fmt.Sprintf("call %d: SUMMARY(empty)", callNum))
				mu.Unlock()
				// LLM returned <analysis> only — extractSummary strips it and returns "".
				// sumErr is nil; summary is "".
				json.NewEncoder(w).Encode(nativeResponse(
					"<analysis>scratch work, no summary block produced</analysis>",
					"end_turn", nil, 50, 30))
				return
			}
			calls = append(calls, fmt.Sprintf("call %d: small-other", callNum))
			mu.Unlock()
			json.NewEncoder(w).Encode(nativeResponse("ok", "end_turn", nil, 50, 30))
			return
		}

		// Main-tier: push messages past MinShapeable (9) and keep total prompt above
		// context_window*0.85. With context_window=2000 threshold=1700, small input
		// + large cache_read (1800) makes totalPromptTokens cross every turn.
		msgCount := len(req.Messages)
		resp := client.CompletionResponse{
			Model:        "test-model",
			FinishReason: "tool_use",
			ToolCalls: []client.FunctionCall{{
				Name:      "think",
				Arguments: json.RawMessage(fmt.Sprintf(`{"thought":"iter with %d msgs"}`, msgCount)),
			}},
			Usage: client.Usage{
				InputTokens:     200,
				OutputTokens:    50,
				TotalTokens:     250,
				CacheReadTokens: 1800, // total = 2000 > 1700 threshold
			},
			RequestID: "req-test",
		}
		if msgCount >= 30 {
			// Hard stop after 15 rounds so the test can't loop forever.
			resp.FinishReason = "end_turn"
			resp.ToolCalls = nil
			resp.OutputText = "done"
		}
		calls = append(calls, fmt.Sprintf("call %d: MAIN (msgs=%d)", callNum, msgCount))
		mu.Unlock()
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	gw := client.NewGatewayClient(server.URL, "")
	reg := NewToolRegistry()
	reg.Register(&thinkTool{})

	handler := &mockHandler{approveResult: true}

	loop := NewAgentLoop(gw, reg, "medium", "", 20, 2000, 200, nil, nil, nil)
	loop.SetContextWindow(2000)
	loop.SetMemoryDir(memoryDir)
	loop.SetHandler(handler)

	_, _, err := loop.Run(context.Background(),
		"Drive the loop past MinShapeable while reporting warm-cache tokens.",
		nil, nil)
	if err != nil {
		t.Logf("Run error (iteration cap is acceptable): %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	t.Logf("\n=== Call sequence (%d total) ===", len(calls))
	for _, c := range calls {
		t.Logf("  %s", c)
	}

	// Extract iteration numbers of SUMMARY calls. The `calls` slice records
	// every /v1/completions hit with "call N: …"; the call index is our
	// proxy for iteration ordering since MAIN + SUMMARY + PERSIST are
	// serialized per iter.
	summaryIndices := []int{}
	for idx, c := range calls {
		if strings.Contains(c, "SUMMARY") {
			summaryIndices = append(summaryIndices, idx)
		}
	}

	// Assertion 1 — empty is treated as failure, so backoff engages after 3.
	// Pre-fix: no backoff on empty → ≥8 SUMMARY in a 15-iter run.
	// Post-fix: fails on 3 then cool-off → at most 4 across the whole run
	// (3 initial failures + at most 1 retry after the 5-iter window closes
	// if the run has not yet hit the 15-iter cap).
	if len(summaryIndices) > 4 {
		t.Errorf("empty-summary backoff did not engage: saw %d SUMMARY calls (expected ≤4)\n"+
			"pre-fix behaviour resets summaryFailures when sumErr==nil && summary==\"\", "+
			"defeating the backoff circuit breaker",
			len(summaryIndices))
	}

	// Assertion 2 — the first 3 SUMMARY calls land before the run's midpoint.
	// If they straddle too wide an interval it means SUMMARY was silently
	// skipping (shouldCompact gate closed) rather than genuinely firing.
	if len(summaryIndices) < 3 {
		t.Fatalf("expected at least 3 SUMMARY calls to trip the breaker; got %d.\n"+
			"call sequence:\n  %s",
			len(summaryIndices), strings.Join(calls, "\n  "))
	}

	// Stress-adequacy soft guard — when the breaker holds to end of run
	// (len(summaryIndices) == 3), Assertion 3 below is skipped entirely via
	// its len >= 4 guard. That is a valid GREEN state only if the run was
	// long enough that a 4th SUMMARY could have fired had the cool-off
	// window been too narrow. If too few MAIN iters actually completed, the
	// test is passing vacuously — a future bump to MinShapeable() or the
	// hard-stop condition could silently hollow out Assertion 3 without any
	// real behavior regression. Count total MAIN iters and flag the gap.
	mainIterCount := 0
	for _, c := range calls {
		if strings.Contains(c, "MAIN") {
			mainIterCount++
		}
	}
	t.Logf("ran %d MAIN iterations total", mainIterCount)
	if len(summaryIndices) == 3 && mainIterCount < 12 {
		t.Errorf("test under-stressed: only %d MAIN iters completed; the breaker holding with "+
			"exactly 3 SUMMARY may be because the run ended, not because the cool-off is 5 iters. "+
			"Raise the hard-stop condition (msgCount>=30) or maxIter so the run reaches ≥ 12 MAIN "+
			"iterations — then Assertion 3 can actually measure the cool-off window.",
			mainIterCount)
	}

	// Assertion 3 — iteration-level cool-off window. Measured by counting
	// MAIN calls between the 3rd and 4th SUMMARY.
	//
	// Within one iteration the call order is PERSIST → SUMMARY → MAIN. So for
	// a correct summaryBackoffIters=5 cool-off, calls[thirdIdx+1 : fourthIdx]
	// contains:
	//   • 1 MAIN from iter F itself (same iter as the 3rd SUMMARY — MAIN
	//     fires after SUMMARY within the iter and is not gated by backoff)
	//   • 5 MAINs from iters F+1…F+5 (fully backed off — only MAIN fires)
	//   • 1 PERSIST from iter F+6 (gate re-opens, right before the 4th SUMMARY)
	// So the expected mainBetween is 6, not 5. The threshold `< 6` strictly
	// rejects any regression down to summaryBackoffIters=4 (mainBetween=5).
	//
	// A previous version of this assertion used call-stream index arithmetic
	// (`windowEnd := thirdFailureAt + 6`). That is wrong because the iter
	// which emits the 4th SUMMARY also contributes MAIN+PERSIST calls to the
	// stream, so a 3-iter backoff and a 5-iter backoff both place the 4th
	// SUMMARY at roughly the same call index, hiding the regression. Counting
	// MAIN calls is the iter-native measure.
	//
	// This is also the assertion that fails when Task 2's three-way switch is
	// applied WITHOUT the `(i - summaryFailures)` → `(i - lastSummaryFailureIter)`
	// formula fix: mid-run failures collapse the window so only 0–2 MAIN
	// calls separate the 3rd and 4th SUMMARY.
	const expectedMainBetween = 6 // 1 same-iter MAIN + summaryBackoffIters backed-off MAINs
	if len(summaryIndices) >= 4 {
		thirdIdx := summaryIndices[2]
		fourthIdx := summaryIndices[3]
		mainBetween := 0
		for _, c := range calls[thirdIdx+1 : fourthIdx] {
			if strings.Contains(c, "MAIN") {
				mainBetween++
			}
		}
		if mainBetween < expectedMainBetween {
			t.Errorf("backoff cool-off window too narrow: only %d MAIN iterations "+
				"between 3rd SUMMARY (call %d) and 4th SUMMARY (call %d); expected ≥ %d "+
				"(1 same-iter MAIN + 5 backed-off MAINs).\n"+
				"This is the signature of a broken cool-off window — the 4th retry "+
				"fired too soon.\ncall sequence:\n  %s",
				mainBetween, thirdIdx, fourthIdx, expectedMainBetween, strings.Join(calls, "\n  "))
		}
	}
	// If there is no 4th SUMMARY at all (len(summaryIndices) == 3), the
	// breaker held for the entire remaining run — that is also a valid
	// GREEN state and intentionally passes without additional checks.
}

// statusRecorder captures OnRunStatus calls for assertions. It also satisfies
// EventHandler with no-op stubs so it can be assigned to AgentLoop.handler in
// tests that exercise recordCompactionFailure end-to-end.
type statusRecorder struct {
	events []recordedStatus
}

type recordedStatus struct{ code, detail string }

func (s *statusRecorder) OnRunStatus(code, detail string) {
	s.events = append(s.events, recordedStatus{code, detail})
}

// EventHandler stubs — no behavior, just to satisfy the interface.
func (s *statusRecorder) OnToolCall(name string, args string)                        {}
func (s *statusRecorder) OnToolResult(string, string, ToolResult, time.Duration)     {}
func (s *statusRecorder) OnText(string)                                              {}
func (s *statusRecorder) OnPreamble(string)                                          {}
func (s *statusRecorder) OnStreamDelta(string)                                       {}
func (s *statusRecorder) OnApprovalNeeded(string, string) bool                       { return false }
func (s *statusRecorder) OnUsage(TurnUsage)                                          {}
func (s *statusRecorder) OnCloudAgent(agentID string, status string, message string) {}
func (s *statusRecorder) OnCloudProgress(int, int)                                   {}
func (s *statusRecorder) OnCloudPlan(planType string, content string, needsReview bool) {
}

func TestEmitCompactionFailureStatus_PostsToHandler(t *testing.T) {
	rec := &statusRecorder{}
	emitCompactionFailureStatus(rec, "reactive_summary_with_prior", errors.New("synthetic 500"))

	if len(rec.events) != 1 {
		t.Fatalf("expected 1 OnRunStatus event, got %d", len(rec.events))
	}
	if rec.events[0].code != "compaction_failed" {
		t.Errorf("code = %q, want compaction_failed", rec.events[0].code)
	}
	if !strings.Contains(rec.events[0].detail, "reactive_summary_with_prior") ||
		!strings.Contains(rec.events[0].detail, "synthetic 500") {
		t.Errorf("detail missing context: %q", rec.events[0].detail)
	}
}

func TestEmitCompactionFailureStatus_HandlesNilHandler(t *testing.T) {
	// nil handler must not panic.
	emitCompactionFailureStatus(nil, "any_phase", errors.New("err"))
}

func TestEmitCompactionFailureStatus_HandlesNonRunStatusHandler(t *testing.T) {
	// Handler that doesn't implement RunStatusHandler must be silently skipped.
	bareHandler := struct{}{}
	emitCompactionFailureStatus(bareHandler, "any_phase", errors.New("err"))
}

func TestRecordCompactionFailure_EmitsBothChannels(t *testing.T) {
	rec := &statusRecorder{}
	a := &AgentLoop{handler: rec, auditor: nil} // auditor=nil exercises the nil guard
	a.recordCompactionFailure("test_phase", errors.New("boom"))

	if len(rec.events) != 1 {
		t.Fatalf("expected 1 OnRunStatus event, got %d", len(rec.events))
	}
	if rec.events[0].code != "compaction_failed" {
		t.Errorf("code = %q, want compaction_failed", rec.events[0].code)
	}
	if !strings.Contains(rec.events[0].detail, "test_phase") ||
		!strings.Contains(rec.events[0].detail, "boom") {
		t.Errorf("detail missing context: %q", rec.events[0].detail)
	}
}

func TestPreflightCompaction_TriggersBeforeOversizedCall(t *testing.T) {
	// Build a message slice that estimates above 0.95 * contextWindow.
	// Use 1.5M chars ≈ 428K tokens; with contextWindow = 200_000, that's
	// way over the 95% threshold (190K).
	bigText := strings.Repeat("x", 1_500_000)
	messages := []client.Message{
		{Role: "system", Content: client.NewTextContent("sys")},
		{Role: "user", Content: client.NewTextContent("first")},
		{Role: "user", Content: client.NewTextContent(bigText)},
	}

	// shouldPreflightCompact is the decision function under test.
	got := shouldPreflightCompact(messages, 200_000)
	if !got {
		t.Errorf("shouldPreflightCompact = false, want true (msg estimate exceeds 95%% of 200K)")
	}

	// Negative case: well-under-budget should not trigger.
	smallMsgs := []client.Message{
		{Role: "system", Content: client.NewTextContent("sys")},
		{Role: "user", Content: client.NewTextContent("hi")},
	}
	if shouldPreflightCompact(smallMsgs, 200_000) {
		t.Errorf("shouldPreflightCompact = true on under-budget messages")
	}

	// Edge: contextWindow=0 (disabled) → never trigger.
	if shouldPreflightCompact(messages, 0) {
		t.Errorf("shouldPreflightCompact = true with contextWindow=0; should be disabled")
	}
}

func TestShortSessionTruncate_RepeatsUntilUnderPreflightThreshold(t *testing.T) {
	loop := &AgentLoop{contextWindow: 200_000}
	hugeA := strings.Repeat("a", 700_000)
	hugeB := strings.Repeat("b", 700_000)
	messages := []client.Message{
		{Role: "system", Content: client.NewTextContent("sys")},
		{Role: "user", Content: client.NewTextContent(hugeA)},
		{Role: "assistant", Content: client.NewTextContent("ack")},
		{Role: "user", Content: client.NewTextContent(hugeB)},
	}
	if !shouldPreflightCompact(messages, loop.contextWindow) {
		t.Fatal("test setup invariant: input must exceed preflight threshold")
	}

	out := loop.shortSessionTruncate(messages, "test")
	if shouldPreflightCompact(out, loop.contextWindow) {
		t.Fatalf("shortSessionTruncate left prompt over preflight threshold")
	}
	if !strings.Contains(out[1].Content.Text(), "user message truncated") {
		t.Errorf("first oversized user message was not truncated")
	}
	if !strings.Contains(out[3].Content.Text(), "user message truncated") {
		t.Errorf("second oversized user message was not truncated")
	}
}

func TestShortSessionTruncate_CurrentTurnRunMessagesStripScaffold(t *testing.T) {
	var captured []client.CompletionRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req client.CompletionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		captured = append(captured, req)
		json.NewEncoder(w).Encode(nativeResponse("ok", "end_turn", nil, 500, 50))
	}))
	defer server.Close()

	gw := client.NewGatewayClient(server.URL, "")
	loop := NewAgentLoop(gw, NewToolRegistry(), "medium", "", 5, 2000, 200, nil, nil, nil)
	loop.SetContextWindow(200_000)

	huge := strings.Repeat("x", 800_000)
	result, _, err := loop.Run(context.Background(), huge, nil, nil)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result != "ok" {
		t.Fatalf("result = %q, want ok", result)
	}
	if len(captured) != 1 {
		t.Fatalf("expected one main request, got %d", len(captured))
	}
	if !strings.Contains(captured[0].Messages[1].Content.Text(), "user message truncated") {
		t.Fatalf("test setup invariant: outgoing user prompt was not truncated")
	}

	runMsgs := loop.RunMessages()
	if len(runMsgs) == 0 {
		t.Fatal("RunMessages empty")
	}
	first := runMsgs[0].Content.Text()
	if strings.Contains(first, "<!-- cache_break -->") ||
		strings.Contains(first, "Active agent context") {
		t.Fatalf("RunMessages leaked scaffolded user prompt after truncation; head=%q", first[:min(200, len(first))])
	}
	if !strings.Contains(first, "user message truncated") {
		t.Fatalf("RunMessages should persist truncated raw user content, got head=%q", first[:min(200, len(first))])
	}
}

// TestAgentLoop_ReactiveCompaction_RecoversFromOversizedTranscript is the E2E
// regression for the 2026-05-07 production cascade:
//
//  1. Session arrives with a multi-megabyte transcript (huge plain-text user
//     and assistant turns from previous interactions).
//  2. First main-tier completion call returns HTTP 400 "prompt is too long".
//  3. Reactive recovery fires: PersistLearnings → compressOldToolResults
//     → GenerateSummary (small-tier) → ShapeHistory → retry.
//  4. Pre-fix bug: GenerateSummary received the same ~1M-char transcript and
//     itself returned 400 with "prompt is too long", surfacing a hard error
//     to the user.
//  5. Post-fix (Task 3): capTranscriptForSummarize truncates the transcript
//     head+tail to summarizeInputCapChars (540K) before the small-tier call,
//     keeping the summarizer comfortably under Haiku 4.5's 200K context window.
//
// We use plain text messages (not tool_result blocks) so compressOldToolResults
// is a no-op — the only line of defense that should kick in here is Task 3's
// cap. We also keep the message count below ctxwin.MinShapeable() (9) so the
// preflight compaction path does not fire and steal the test scenario; the 400
// response is the only path into reactive recovery.
func TestAgentLoop_ReactiveCompaction_RecoversFromOversizedTranscript(t *testing.T) {
	// Mirrors internal/context/summarize.go's summarizeInputCapChars constant;
	// kept as a local literal because the source-of-truth constant is unexported.
	// If you bump the cap there, bump this assertion too.
	const summarizeInputCapCharsLocal = 540_000

	memoryDir := t.TempDir()

	var mu sync.Mutex
	var calls []string
	var maxSummaryUserChars int // peak observed body length on small-tier SUMMARY calls

	contextErrorReturned := false
	retrySucceeded := false

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := readBody(r.Body)
		defer r.Body.Close()

		var req struct {
			ModelTier string `json:"model_tier"`
			Messages  []struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		json.Unmarshal(raw, &req)

		mu.Lock()
		callNum := len(calls) + 1

		if req.ModelTier == "small" {
			isPersist := false
			isSummary := false
			summaryUserChars := 0
			for _, m := range req.Messages {
				var text string
				json.Unmarshal(m.Content, &text)
				if strings.Contains(text, "extracting durable knowledge") {
					isPersist = true
				}
				if strings.Contains(text, "Compress the following conversation") {
					isSummary = true
				}
				if m.Role == "user" {
					// Track length of the user-content portion — this is the
					// transcript that capTranscriptForSummarize is supposed to
					// cap. The system message (the summary prompt) is fixed and
					// not the load-bearing variable.
					if len(text) > summaryUserChars {
						summaryUserChars = len(text)
					}
				}
			}

			if isPersist {
				calls = append(calls, fmt.Sprintf("call %d: PERSIST", callNum))
				mu.Unlock()
				t.Logf("Call %d: [small] PersistLearnings (messages: %d)", callNum, len(req.Messages))
				json.NewEncoder(w).Encode(nativeResponse(
					"- Reactive recovery exercised cap on oversized transcript",
					"end_turn", nil, 50, 30))
				return
			}
			if isSummary {
				if summaryUserChars > maxSummaryUserChars {
					maxSummaryUserChars = summaryUserChars
				}
				calls = append(calls, fmt.Sprintf("call %d: SUMMARY (user_chars=%d)", callNum, summaryUserChars))
				mu.Unlock()
				t.Logf("Call %d: [small] GenerateSummary (user_content_chars=%d)", callNum, summaryUserChars)
				// Return a valid <summary> wrapper so extractSummary yields non-empty content;
				// otherwise ShapeHistory falls back to sliding-window without a summary, which
				// is a separate code path from what we want to exercise here.
				json.NewEncoder(w).Encode(nativeResponse(
					"<summary>Earlier session covered analysis of multiple components before the context overflowed.</summary>",
					"end_turn", nil, 50, 30))
				return
			}

			calls = append(calls, fmt.Sprintf("call %d: small-other", callNum))
			mu.Unlock()
			json.NewEncoder(w).Encode(nativeResponse("ok", "end_turn", nil, 50, 30))
			return
		}

		// Main-tier call: first one returns 400, subsequent calls succeed.
		msgCount := len(req.Messages)
		if !contextErrorReturned {
			contextErrorReturned = true
			calls = append(calls, fmt.Sprintf("call %d: CONTEXT_ERROR (msgs=%d)", callNum, msgCount))
			mu.Unlock()
			t.Logf("Call %d: [main] → 400 prompt is too long (msgs=%d)", callNum, msgCount)
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"error":{"type":"invalid_request_error","message":"prompt is too long"}}`))
			return
		}

		retrySucceeded = true
		calls = append(calls, fmt.Sprintf("call %d: RETRY_SUCCESS (msgs=%d)", callNum, msgCount))
		mu.Unlock()
		t.Logf("Call %d: [main] end_turn after reactive compaction (msgs=%d)", callNum, msgCount)
		json.NewEncoder(w).Encode(nativeResponse(
			"Recovered after reactive compaction with size-capped summary.",
			"end_turn", nil, 800, 100))
	}))
	defer server.Close()

	gw := client.NewGatewayClient(server.URL, "")
	reg := NewToolRegistry()
	reg.Register(&thinkTool{})

	handler := &mockHandler{approveResult: true}

	// Six pre-existing messages (3 user/assistant turns) of ~200K chars each
	// → ~1.2M chars total transcript. After buildTranscript prefixes each
	// message with "[role]: ", that's still well over summarizeInputCapChars
	// (540K). Without Task 3's cap, the small-tier SUMMARY call would receive
	// the full ~1.2M-char transcript and 400 with "prompt is too long",
	// reproducing the production cascade.
	//
	// Plain text (not tool_result blocks) is intentional:
	//   - tool_result is truncated to 450 chars per block by summarizeToolResult,
	//     which would short-circuit the cap path and make this test prove
	//     nothing about Task 3.
	//   - Plain text in user/assistant messages flows through messageText
	//     untouched, so the only line of defense between buildTranscript and
	//     the small-tier model is capTranscriptForSummarize.
	//
	// Six messages keeps the count below ctxwin.MinShapeable() (9), which gates
	// the preflight compaction path. We want the 400 response to be the ONLY
	// trigger of reactive recovery; preflight firing first would shape the
	// transcript before we ever see the 400 and the test would prove nothing
	// about the reactive→summary cap interaction.
	const perMsgChars = 200_000
	hugeUser := strings.Repeat("u", perMsgChars)
	hugeAssistant := strings.Repeat("a", perMsgChars)
	history := []client.Message{
		{Role: "user", Content: client.NewTextContent(hugeUser)},
		{Role: "assistant", Content: client.NewTextContent(hugeAssistant)},
		{Role: "user", Content: client.NewTextContent(hugeUser)},
		{Role: "assistant", Content: client.NewTextContent(hugeAssistant)},
		{Role: "user", Content: client.NewTextContent(hugeUser)},
		{Role: "assistant", Content: client.NewTextContent(hugeAssistant)},
	}
	totalHistoryChars := 6 * perMsgChars
	t.Logf("Seeded history: 6 messages × %d chars = %d chars total (over the %d-char small-tier cap)",
		perMsgChars, totalHistoryChars, summarizeInputCapCharsLocal)

	loop := NewAgentLoop(gw, reg, "medium", "", 20, 2000, 200, nil, nil, nil)
	// Big context window so the proactive 0.85 gate doesn't fire on
	// lastInputTokens (which our mock reports as 800/100, well under any cap).
	// The 400 response is the only trigger we want for reactive recovery.
	loop.SetContextWindow(2_000_000)
	loop.SetMemoryDir(memoryDir)
	loop.SetHandler(handler)

	result, usage, err := loop.Run(context.Background(),
		"Continue the analysis after the prior session.",
		nil, history)
	if err != nil {
		t.Fatalf("expected reactive recovery to succeed, got error: %v", err)
	}

	mu.Lock()
	t.Logf("\n=== Call sequence (%d total) ===", len(calls))
	for _, c := range calls {
		t.Logf("  %s", c)
	}

	hasPersist := false
	hasSummary := false
	hasContextError := false
	hasRetrySuccess := false
	for _, c := range calls {
		if strings.Contains(c, "PERSIST") {
			hasPersist = true
		}
		if strings.Contains(c, "SUMMARY") {
			hasSummary = true
		}
		if strings.Contains(c, "CONTEXT_ERROR") {
			hasContextError = true
		}
		if strings.Contains(c, "RETRY_SUCCESS") {
			hasRetrySuccess = true
		}
	}
	peakSummaryChars := maxSummaryUserChars
	mu.Unlock()

	t.Logf("Result: %d chars", len(result))
	t.Logf("Usage: %d LLM calls", usage.LLMCalls)
	t.Logf("Peak SUMMARY user-content body length: %d chars (cap=%d, raw transcript would be ~%d)",
		peakSummaryChars, summarizeInputCapCharsLocal, totalHistoryChars)

	if !hasContextError {
		t.Error("expected the mock's main-tier 400 to fire (reactive recovery entry point)")
	}
	if !hasPersist {
		t.Error("expected PersistLearnings during reactive compaction")
	}
	if !hasSummary {
		t.Error("expected GenerateSummary during reactive compaction")
	}
	if !hasRetrySuccess {
		t.Error("expected the post-recovery main-tier retry to succeed")
	}
	if !retrySucceeded {
		t.Error("retrySucceeded flag should be true")
	}
	if result == "" {
		t.Error("expected non-empty result after successful retry")
	}

	// The Task 3 assertion. Without capTranscriptForSummarize, peakSummaryChars
	// would be ~1.2M (the raw buildTranscript output) and the small-tier call
	// would 400 just like the main-tier, ending the cascade in a hard error
	// to the user. With the cap, peakSummaryChars should sit at or just under
	// summarizeInputCapChars (540K) — never above.
	if peakSummaryChars == 0 {
		t.Fatalf("peak SUMMARY body length is 0; the small-tier SUMMARY call was never observed")
	}
	if peakSummaryChars > summarizeInputCapCharsLocal {
		t.Errorf("peak SUMMARY user-content body length = %d, want ≤ %d (summarizeInputCapChars).\n"+
			"This is the regression for the 2026-05-07 cascade: capTranscriptForSummarize\n"+
			"failed to apply, so the small-tier summarizer received the full oversized\n"+
			"transcript and would 400 in production.",
			peakSummaryChars, summarizeInputCapCharsLocal)
	}
	// Also assert the cap was actually exercised (binding) — if peakSummaryChars
	// is way under 540K, the test scenario didn't actually push hard enough to
	// prove anything about the cap.
	if peakSummaryChars < summarizeInputCapCharsLocal/2 {
		t.Errorf("peak SUMMARY user-content body length = %d, expected the cap to be binding (≥ %d).\n"+
			"The seeded transcript may not be large enough to actually exercise capTranscriptForSummarize.",
			peakSummaryChars, summarizeInputCapCharsLocal/2)
	}
}
