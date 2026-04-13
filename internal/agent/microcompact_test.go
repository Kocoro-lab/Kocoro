package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
	ctxwin "github.com/Kocoro-lab/ShanClaw/internal/context"
)

// mockCompleter returns a fixed response for micro-compact tests.
type mockCompleter struct {
	output string
	calls  int
}

func (m *mockCompleter) Complete(ctx context.Context, req client.CompletionRequest) (*client.CompletionResponse, error) {
	m.calls++
	return &client.CompletionResponse{OutputText: m.output}, nil
}

func TestMicroCompact_LargeResultGetsSummarized(t *testing.T) {
	mc := &mockCompleter{output: "PostgreSQL unreachable on localhost:5432"}
	content := strings.Repeat("log line\n", 300) + "connection refused on localhost:5432"

	summary, ok := microCompactResult(context.Background(), mc, "bash", content)
	if !ok {
		t.Fatal("expected micro-compact to succeed")
	}
	if !strings.HasPrefix(summary, microCompactMarker) {
		t.Errorf("expected marker prefix, got: %q", summary[:50])
	}
	if mc.calls != 1 {
		t.Errorf("expected 1 LLM call, got %d", mc.calls)
	}
}

func TestMicroCompact_NilCompleterReturnsFalse(t *testing.T) {
	_, ok := microCompactResult(context.Background(), nil, "bash", "content")
	if ok {
		t.Error("expected false with nil completer")
	}
}

func TestMicroCompact_MarkerPreventsReSummarization(t *testing.T) {
	content := microCompactMarker + "already summarized"
	if !isMicroCompacted(content) {
		t.Error("should detect micro-compact marker")
	}
}

func TestMicroCompact_Tier2Integration_WithCompleter(t *testing.T) {
	mc := &mockCompleter{output: "Summary: found config at /etc/app.conf"}

	// Build messages with native tool_result blocks
	var messages []client.Message
	messages = append(messages, client.Message{Role: "system", Content: client.NewTextContent("system")})
	messages = append(messages, client.Message{Role: "user", Content: client.NewTextContent("start")})

	// Add 8 tool results (keepRecent=3 means 5 are eligible for compression)
	for i := 0; i < 8; i++ {
		// Large content (>2000 chars) to trigger micro-compact
		content := strings.Repeat("log output line\n", 200)
		messages = append(messages, client.Message{
			Role: "user",
			Content: client.NewBlockContent([]client.ContentBlock{
				client.NewToolResultBlock("tc"+string(rune('a'+i)), content, false),
			}),
		})
		messages = append(messages, client.Message{
			Role:    "assistant",
			Content: client.NewTextContent("noted"),
		})
	}

	compressOldToolResults(context.Background(), messages, 3, 300, mc)

	// Should have made at most microCompactMaxPerPass LLM calls
	if mc.calls > microCompactMaxPerPass {
		t.Errorf("expected <= %d LLM calls, got %d", microCompactMaxPerPass, mc.calls)
	}
	if mc.calls == 0 {
		t.Error("expected at least 1 LLM call for large Tier 2 results")
	}

	// Check that summarized results have the marker
	summarized := 0
	for _, msg := range messages {
		if !msg.Content.HasBlocks() {
			continue
		}
		for _, b := range msg.Content.Blocks() {
			if b.Type == "tool_result" {
				text := client.ToolResultText(b)
				if strings.HasPrefix(text, microCompactMarker) {
					summarized++
				}
			}
		}
	}
	if summarized != mc.calls {
		t.Errorf("expected %d summarized results, got %d", mc.calls, summarized)
	}
}

func TestMicroCompact_Tier2Integration_NilCompleter(t *testing.T) {
	// Without completer, should fall back to head+tail truncation
	var messages []client.Message
	messages = append(messages, client.Message{Role: "system", Content: client.NewTextContent("system")})
	messages = append(messages, client.Message{Role: "user", Content: client.NewTextContent("start")})

	for i := 0; i < 6; i++ {
		content := strings.Repeat("data line\n", 300)
		messages = append(messages, client.Message{
			Role: "user",
			Content: client.NewBlockContent([]client.ContentBlock{
				client.NewToolResultBlock("tc"+string(rune('a'+i)), content, false),
			}),
		})
		messages = append(messages, client.Message{
			Role:    "assistant",
			Content: client.NewTextContent("ok"),
		})
	}

	compressOldToolResults(context.Background(), messages, 3, 300, nil)

	// No micro-compact markers should exist
	for _, msg := range messages {
		if !msg.Content.HasBlocks() {
			continue
		}
		for _, b := range msg.Content.Blocks() {
			if b.Type == "tool_result" {
				text := client.ToolResultText(b)
				if strings.HasPrefix(text, microCompactMarker) {
					t.Error("should not have micro-compact marker with nil completer")
				}
			}
		}
	}
}

func TestMicroCompact_SkipsAlreadySummarized(t *testing.T) {
	mc := &mockCompleter{output: "new summary"}

	var messages []client.Message
	messages = append(messages, client.Message{Role: "system", Content: client.NewTextContent("system")})
	messages = append(messages, client.Message{Role: "user", Content: client.NewTextContent("start")})

	// Add a result that's already micro-compacted (but pad to >2000 chars to be eligible by size)
	alreadySummarized := microCompactMarker + strings.Repeat("x", 2100)
	messages = append(messages, client.Message{
		Role: "user",
		Content: client.NewBlockContent([]client.ContentBlock{
			client.NewToolResultBlock("tc1", alreadySummarized, false),
		}),
	})
	messages = append(messages, client.Message{Role: "assistant", Content: client.NewTextContent("ok")})
	// Add more to make this one fall in Tier 2
	for i := 0; i < 5; i++ {
		messages = append(messages, client.Message{
			Role: "user",
			Content: client.NewBlockContent([]client.ContentBlock{
				client.NewToolResultBlock("tc"+string(rune('2'+i)), "short", false),
			}),
		})
		messages = append(messages, client.Message{Role: "assistant", Content: client.NewTextContent("ok")})
	}

	compressOldToolResults(context.Background(), messages, 3, 300, mc)

	// Should NOT have called the completer for the already-summarized result
	if mc.calls != 0 {
		t.Errorf("expected 0 LLM calls for already-summarized content, got %d", mc.calls)
	}
}

func TestMicroCompact_SmallResultSkipsLLM(t *testing.T) {
	mc := &mockCompleter{output: "should not be called"}

	var messages []client.Message
	messages = append(messages, client.Message{Role: "system", Content: client.NewTextContent("system")})
	messages = append(messages, client.Message{Role: "user", Content: client.NewTextContent("start")})

	// Small content (<2000 chars) — should use head+tail, not LLM
	for i := 0; i < 6; i++ {
		messages = append(messages, client.Message{
			Role: "user",
			Content: client.NewBlockContent([]client.ContentBlock{
				client.NewToolResultBlock("tc"+string(rune('a'+i)), "short result", false),
			}),
		})
		messages = append(messages, client.Message{Role: "assistant", Content: client.NewTextContent("ok")})
	}

	compressOldToolResults(context.Background(), messages, 3, 300, mc)

	if mc.calls != 0 {
		t.Errorf("expected 0 LLM calls for small results, got %d", mc.calls)
	}
}

// failingCompleter always returns an error to test attempt-cap behavior.
type failingCompleter struct {
	calls int
}

func (f *failingCompleter) Complete(ctx context.Context, req client.CompletionRequest) (*client.CompletionResponse, error) {
	f.calls++
	return nil, fmt.Errorf("LLM unavailable")
}

func TestMicroCompact_FailingCompleterCapsAttempts(t *testing.T) {
	fc := &failingCompleter{}

	var messages []client.Message
	messages = append(messages, client.Message{Role: "system", Content: client.NewTextContent("system")})
	messages = append(messages, client.Message{Role: "user", Content: client.NewTextContent("start")})

	// 8 large tool results — all eligible for micro-compact
	for i := 0; i < 8; i++ {
		content := strings.Repeat("log output line\n", 200)
		messages = append(messages, client.Message{
			Role: "user",
			Content: client.NewBlockContent([]client.ContentBlock{
				client.NewToolResultBlock("tc"+string(rune('a'+i)), content, false),
			}),
		})
		messages = append(messages, client.Message{Role: "assistant", Content: client.NewTextContent("ok")})
	}

	compressOldToolResults(context.Background(), messages, 3, 300, fc)

	// Should cap at microCompactMaxPerPass attempts, even though all failed
	if fc.calls > microCompactMaxPerPass {
		t.Errorf("expected <= %d LLM attempts, got %d (should cap attempts, not just successes)", microCompactMaxPerPass, fc.calls)
	}
	if fc.calls == 0 {
		t.Error("expected at least 1 LLM attempt")
	}
}

func TestMicroCompact_SkipsThinkTool(t *testing.T) {
	mc := &mockCompleter{output: "summarized think"}

	var messages []client.Message
	messages = append(messages, client.Message{Role: "system", Content: client.NewTextContent("system")})
	messages = append(messages, client.Message{Role: "user", Content: client.NewTextContent("start")})

	// Large think result — should NOT be micro-compacted
	content := strings.Repeat("reasoning step\n", 200)
	messages = append(messages, client.Message{
		Role: "user",
		Content: client.NewBlockContent([]client.ContentBlock{
			client.NewToolResultBlock("tc_think", content, false),
		}),
	})
	messages = append(messages, client.Message{Role: "assistant", Content: client.NewTextContent("ok")})
	// Pad with more results to push think into Tier 2
	for i := 0; i < 5; i++ {
		messages = append(messages, client.Message{
			Role: "user",
			Content: client.NewBlockContent([]client.ContentBlock{
				client.NewToolResultBlock("tc"+string(rune('a'+i)), "short", false),
			}),
		})
		messages = append(messages, client.Message{Role: "assistant", Content: client.NewTextContent("ok")})
	}

	// Build toolCallMap with think tool
	toolCallMap := map[string]toolCallInfo{"tc_think": {Name: "think", Args: `{"thought":"..."}`}}
	// Inject into messages so buildToolCallMap finds it
	messages = append([]client.Message{messages[0], {
		Role: "assistant",
		Content: client.NewBlockContent([]client.ContentBlock{
			{Type: "tool_use", ID: "tc_think", Name: "think", Input: json.RawMessage(`{"thought":"..."}`)},
		}),
	}}, messages[1:]...)
	_ = toolCallMap // buildToolCallMap will find it from the injected message

	compressOldToolResults(context.Background(), messages, 3, 300, mc)

	if mc.calls != 0 {
		t.Errorf("expected 0 LLM calls for think tool, got %d", mc.calls)
	}
}

func TestMicroCompact_SkipsCloudDelegate(t *testing.T) {
	mc := &mockCompleter{output: "summarized cloud"}

	var messages []client.Message
	messages = append(messages, client.Message{Role: "system", Content: client.NewTextContent("system")})
	messages = append(messages, client.Message{Role: "user", Content: client.NewTextContent("start")})

	content := strings.Repeat("cloud deliverable content\n", 200)
	// Inject tool_use block for cloud_delegate
	messages = append(messages, client.Message{
		Role: "assistant",
		Content: client.NewBlockContent([]client.ContentBlock{
			{Type: "tool_use", ID: "tc_cloud", Name: "cloud_delegate", Input: json.RawMessage(`{"task":"analyze"}`)},
		}),
	})
	messages = append(messages, client.Message{
		Role: "user",
		Content: client.NewBlockContent([]client.ContentBlock{
			client.NewToolResultBlock("tc_cloud", content, false),
		}),
	})
	messages = append(messages, client.Message{Role: "assistant", Content: client.NewTextContent("ok")})
	for i := 0; i < 5; i++ {
		messages = append(messages, client.Message{
			Role: "user",
			Content: client.NewBlockContent([]client.ContentBlock{
				client.NewToolResultBlock("tc"+string(rune('a'+i)), "short", false),
			}),
		})
		messages = append(messages, client.Message{Role: "assistant", Content: client.NewTextContent("ok")})
	}

	compressOldToolResults(context.Background(), messages, 3, 300, mc)

	if mc.calls != 0 {
		t.Errorf("expected 0 LLM calls for cloud_delegate, got %d", mc.calls)
	}
}

// Ensure the interface is satisfied
var _ ctxwin.Completer = (*mockCompleter)(nil)
var _ ctxwin.Completer = (*failingCompleter)(nil)

// TestMicroCompact_SkipsBrowserSnapshot: browser_snapshot returns the DOM
// tree — the model's "eyes" for web tasks. Summarizing it destroys the
// structured page content the model needs to extract data. This regression
// was observed in a real x.com search task where DOM snapshots were replaced
// with meta-descriptions like "The browser navigated to X", blinding the
// model mid-extraction.
func TestMicroCompact_SkipsBrowserSnapshot(t *testing.T) {
	mc := &mockCompleter{output: "The browser navigated to X and displayed results"}

	var messages []client.Message
	messages = append(messages, client.Message{Role: "system", Content: client.NewTextContent("system")})

	// Inject tool_use for browser_snapshot with a large DOM payload.
	messages = append(messages, client.Message{
		Role: "assistant",
		Content: client.NewBlockContent([]client.ContentBlock{
			{Type: "tool_use", ID: "tc_snap", Name: "browser_snapshot", Input: json.RawMessage(`{}`)},
		}),
	})
	content := strings.Repeat("<div class=\"tweet\">large DOM payload</div>\n", 200)
	messages = append(messages, client.Message{
		Role: "user",
		Content: client.NewBlockContent([]client.ContentBlock{
			client.NewToolResultBlock("tc_snap", content, false),
		}),
	})
	messages = append(messages, client.Message{Role: "assistant", Content: client.NewTextContent("ok")})

	// Pad to push the snapshot into Tier 2.
	for i := 0; i < 5; i++ {
		messages = append(messages, client.Message{
			Role: "user",
			Content: client.NewBlockContent([]client.ContentBlock{
				client.NewToolResultBlock("tc"+string(rune('a'+i)), "short", false),
			}),
		})
		messages = append(messages, client.Message{Role: "assistant", Content: client.NewTextContent("ok")})
	}

	compressOldToolResults(context.Background(), messages, 3, 300, mc)

	if mc.calls != 0 {
		t.Errorf("expected 0 LLM calls for browser_snapshot, got %d", mc.calls)
	}
}

// TestIsMicroCompactSkipTool_BrowserPrefix locks in the prefix-based
// matching for browser_* tools. Before the fix, the skip list was an
// enumerated map that missed browser_drag and browser_take_screenshot —
// two tools that were already referenced elsewhere in the repo
// (internal/agent/normalize.go) but absent from this map. Now all browser_*
// names should match via strings.HasPrefix.
func TestIsMicroCompactSkipTool_BrowserPrefix(t *testing.T) {
	// Previously-missed names — regression guard.
	for _, name := range []string{"browser_drag", "browser_take_screenshot"} {
		if !isMicroCompactSkipTool(name) {
			t.Errorf("%s must be skipped (was missing from the old enumerated map)", name)
		}
	}
	// Still covered after the refactor.
	for _, name := range []string{"browser_navigate", "browser_snapshot", "browser_click"} {
		if !isMicroCompactSkipTool(name) {
			t.Errorf("%s must still be skipped after the prefix refactor", name)
		}
	}
	// Non-browser tools keep their original behavior.
	for _, name := range []string{"think", "file_read", "grep"} {
		if !isMicroCompactSkipTool(name) {
			t.Errorf("%s must still be skipped", name)
		}
	}
	// Completely unrelated tools must NOT be skipped.
	for _, name := range []string{"bash", "http", "memory_append"} {
		if isMicroCompactSkipTool(name) {
			t.Errorf("%s must not be skipped", name)
		}
	}
}

func TestIsTier2FloorTool_BrowserPrefix(t *testing.T) {
	for _, name := range []string{"browser_drag", "browser_take_screenshot", "browser_snapshot"} {
		if !isTier2FloorTool(name) {
			t.Errorf("%s must be a tier-2 floor tool", name)
		}
	}
	for _, name := range []string{"file_read", "grep", "glob", "directory_list"} {
		if !isTier2FloorTool(name) {
			t.Errorf("%s must remain a tier-2 floor tool", name)
		}
	}
	if isTier2FloorTool("bash") {
		t.Error("bash must not be a tier-2 floor tool")
	}
}

func TestMicroCompact_SkipsBrowserNavigate(t *testing.T) {
	mc := &mockCompleter{output: "navigated to page"}

	var messages []client.Message
	messages = append(messages, client.Message{Role: "system", Content: client.NewTextContent("system")})

	messages = append(messages, client.Message{
		Role: "assistant",
		Content: client.NewBlockContent([]client.ContentBlock{
			{Type: "tool_use", ID: "tc_nav", Name: "browser_navigate", Input: json.RawMessage(`{"url":"https://x.com"}`)},
		}),
	})
	content := strings.Repeat("<nav>page content</nav>\n", 200)
	messages = append(messages, client.Message{
		Role: "user",
		Content: client.NewBlockContent([]client.ContentBlock{
			client.NewToolResultBlock("tc_nav", content, false),
		}),
	})
	messages = append(messages, client.Message{Role: "assistant", Content: client.NewTextContent("ok")})

	for i := 0; i < 5; i++ {
		messages = append(messages, client.Message{
			Role: "user",
			Content: client.NewBlockContent([]client.ContentBlock{
				client.NewToolResultBlock("tc"+string(rune('a'+i)), "short", false),
			}),
		})
		messages = append(messages, client.Message{Role: "assistant", Content: client.NewTextContent("ok")})
	}

	compressOldToolResults(context.Background(), messages, 3, 300, mc)

	if mc.calls != 0 {
		t.Errorf("expected 0 LLM calls for browser_navigate, got %d", mc.calls)
	}
}
