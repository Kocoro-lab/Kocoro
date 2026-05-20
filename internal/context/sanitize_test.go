package context

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/runstatus"
)

func TestSanitizeHistory_Empty(t *testing.T) {
	result := SanitizeHistory(nil)
	if result != nil {
		t.Errorf("expected nil, got %v", result)
	}
	result = SanitizeHistory([]client.Message{})
	if len(result) != 0 {
		t.Errorf("expected empty, got %d", len(result))
	}
}

func TestSanitizeHistory_CleanPassthrough(t *testing.T) {
	msgs := []client.Message{
		{Role: "user", Content: client.NewTextContent("hello")},
		{Role: "assistant", Content: client.NewTextContent("hi there")},
		{Role: "user", Content: client.NewTextContent("how are you")},
		{Role: "assistant", Content: client.NewTextContent("doing well")},
	}
	result := SanitizeHistory(msgs)
	if len(result) != 4 {
		t.Fatalf("expected 4, got %d", len(result))
	}
	for i, m := range result {
		if m.Role != msgs[i].Role || m.Content.Text() != msgs[i].Content.Text() {
			t.Errorf("msg %d mismatch", i)
		}
	}
}

func TestSanitizeHistory_DropsToolCallPlaceholders(t *testing.T) {
	msgs := []client.Message{
		{Role: "user", Content: client.NewTextContent("search for X")},
		{Role: "assistant", Content: client.NewTextContent("[tool_call: web_search]")},
		{Role: "assistant", Content: client.NewTextContent("[tool_call: web_search]")},
		{Role: "assistant", Content: client.NewTextContent("here are the results")},
	}
	result := SanitizeHistory(msgs)
	if len(result) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(result))
	}
	if result[0].Role != "user" {
		t.Errorf("expected user, got %s", result[0].Role)
	}
	if result[1].Content.Text() != "here are the results" {
		t.Errorf("expected final assistant text, got %q", result[1].Content.Text())
	}
}

func TestSanitizeHistory_DropsPlainTextToolMessages(t *testing.T) {
	msgs := []client.Message{
		{Role: "user", Content: client.NewTextContent("hello")},
		{Role: "assistant", Content: client.NewTextContent("let me search")},
		{Role: "tool", Content: client.NewTextContent("Search results for: shoes")},
		{Role: "assistant", Content: client.NewTextContent("found some shoes")},
	}
	result := SanitizeHistory(msgs)
	// tool msg dropped, consecutive assistants merged → keep last
	if len(result) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(result))
	}
	if result[1].Content.Text() != "found some shoes" {
		t.Errorf("expected merged assistant, got %q", result[1].Content.Text())
	}
}

func TestSanitizeHistory_DropsErrorMessages(t *testing.T) {
	friendlyErr := runstatus.FriendlyMessage(runstatus.CodeServiceTemporaryError)
	msgs := []client.Message{
		{Role: "user", Content: client.NewTextContent("hello")},
		{Role: "assistant", Content: client.NewTextContent(friendlyErr)},
		{Role: "user", Content: client.NewTextContent("try again")},
		{Role: "assistant", Content: client.NewTextContent(friendlyErr)},
	}
	result := SanitizeHistory(msgs)
	// Both error assistants dropped, consecutive users merged → keep last
	if len(result) != 1 {
		t.Fatalf("expected 1 message, got %d", len(result))
	}
	if result[0].Content.Text() != "try again" {
		t.Errorf("expected last user, got %q", result[0].Content.Text())
	}
}

func TestSanitizeHistory_DropsAgentFailedError(t *testing.T) {
	msgs := []client.Message{
		{Role: "user", Content: client.NewTextContent("Say hi")},
		{Role: "assistant", Content: client.NewTextContent("[error: agent failed to respond]")},
		{Role: "user", Content: client.NewTextContent("hello")},
		{Role: "assistant", Content: client.NewTextContent("hi!")},
	}
	result := SanitizeHistory(msgs)
	// error dropped, consecutive users merged → keep last
	if len(result) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(result))
	}
	if result[0].Content.Text() != "hello" {
		t.Errorf("expected second user msg, got %q", result[0].Content.Text())
	}
}

func TestSanitizeHistory_MergesConsecutiveAssistant(t *testing.T) {
	msgs := []client.Message{
		{Role: "user", Content: client.NewTextContent("hello")},
		{Role: "assistant", Content: client.NewTextContent("first response")},
		{Role: "assistant", Content: client.NewTextContent("second response")},
		{Role: "assistant", Content: client.NewTextContent("third response")},
	}
	result := SanitizeHistory(msgs)
	if len(result) != 2 {
		t.Fatalf("expected 2, got %d", len(result))
	}
	if result[1].Content.Text() != "third response" {
		t.Errorf("expected last assistant kept, got %q", result[1].Content.Text())
	}
}

func TestSanitizeHistory_MergesConsecutiveUser(t *testing.T) {
	msgs := []client.Message{
		{Role: "user", Content: client.NewTextContent("first")},
		{Role: "user", Content: client.NewTextContent("second")},
		{Role: "user", Content: client.NewTextContent("third")},
		{Role: "assistant", Content: client.NewTextContent("got it")},
	}
	result := SanitizeHistory(msgs)
	if len(result) != 2 {
		t.Fatalf("expected 2, got %d", len(result))
	}
	if result[0].Content.Text() != "third" {
		t.Errorf("expected last user kept, got %q", result[0].Content.Text())
	}
}

func TestSanitizeHistory_FullCorruptionScenario(t *testing.T) {
	// Reproduce the exact little-v corruption pattern
	msgs := []client.Message{
		{Role: "user", Content: client.NewTextContent("check rankings")},
		{Role: "assistant", Content: client.NewTextContent(runstatus.FriendlyMessage(runstatus.CodeDeadlineExceeded))},
		{Role: "user", Content: client.NewTextContent("heartbeat prompt")},
		{Role: "assistant", Content: client.NewTextContent("I noticed the timeout")},
		{Role: "assistant", Content: client.NewTextContent("urgent confirmation")},
		{Role: "assistant", Content: client.NewTextContent("[tool_call: web_search]")},
		{Role: "assistant", Content: client.NewTextContent("[tool_call: web_search]")},
		{Role: "assistant", Content: client.NewTextContent("[tool_call: web_search]")},
		{Role: "tool", Content: client.NewTextContent("Search results 1")},
		{Role: "tool", Content: client.NewTextContent("Search results 2")},
		{Role: "tool", Content: client.NewTextContent("Search results 3")},
		{Role: "assistant", Content: client.NewTextContent("Here are the rankings")},
		{Role: "user", Content: client.NewTextContent("你好呀！")},
		{Role: "assistant", Content: client.NewTextContent(runstatus.FriendlyMessage(runstatus.CodeServiceTemporaryError))},
		{Role: "user", Content: client.NewTextContent("今天有没有什么更新")},
		{Role: "assistant", Content: client.NewTextContent(runstatus.FriendlyMessage(runstatus.CodeServiceTemporaryError))},
	}

	result := SanitizeHistory(msgs)

	// Verify alternation
	for i := 1; i < len(result); i++ {
		if result[i].Role == result[i-1].Role {
			t.Errorf("consecutive same role at %d: %s", i, result[i].Role)
		}
	}

	// Should not contain any error or tool_call messages
	for i, m := range result {
		text := m.Content.Text()
		if m.Role == "tool" {
			t.Errorf("tool message at %d should be dropped", i)
		}
		if text == runstatus.FriendlyMessage(runstatus.CodeServiceTemporaryError) {
			t.Errorf("error message at %d should be dropped", i)
		}
		if text == "[tool_call: web_search]" {
			t.Errorf("tool_call placeholder at %d should be dropped", i)
		}
	}

	// Verify we kept meaningful content
	found := false
	for _, m := range result {
		if m.Content.Text() == "Here are the rankings" {
			found = true
		}
	}
	if !found {
		t.Error("expected 'Here are the rankings' to survive")
	}

	t.Logf("sanitized %d → %d messages", len(msgs), len(result))
	for i, m := range result {
		t.Logf("  [%d] %s: %s", i, m.Role, truncStr(m.Content.Text(), 50))
	}
}

func TestSanitizeHistory_PreservesSystemMessages(t *testing.T) {
	msgs := []client.Message{
		{Role: "system", Content: client.NewTextContent("you are helpful")},
		{Role: "user", Content: client.NewTextContent("hello")},
		{Role: "assistant", Content: client.NewTextContent("hi")},
	}
	result := SanitizeHistory(msgs)
	if len(result) != 3 {
		t.Fatalf("expected 3, got %d", len(result))
	}
	if result[0].Role != "system" {
		t.Errorf("system message should be preserved")
	}
}

func TestSanitizeHistory_DropsOrphanedToolUse(t *testing.T) {
	// Assistant has a tool_use block but the matching tool_result was
	// stripped (e.g. it was an error marker). The tool_use must be removed
	// to prevent API rejection.
	msgs := []client.Message{
		{Role: "user", Content: client.NewTextContent("fetch the page")},
		{Role: "assistant", Content: client.NewBlockContent([]client.ContentBlock{
			{Type: "text", Text: "Let me fetch that."},
			client.NewToolUseBlock("toolu_orphan1", "web_fetch", nil),
		})},
		// No tool_result for toolu_orphan1 follows (it was dropped).
		{Role: "user", Content: client.NewTextContent("try again")},
		{Role: "assistant", Content: client.NewTextContent("OK")},
	}
	result := SanitizeHistory(msgs)

	// The orphaned tool_use should be stripped; the text should survive.
	for _, m := range result {
		if m.Content.HasBlocks() {
			for _, b := range m.Content.Blocks() {
				if b.Type == "tool_use" && b.ID == "toolu_orphan1" {
					t.Error("orphaned tool_use should be stripped")
				}
			}
		}
	}

	// The text "Let me fetch that." should survive.
	found := false
	for _, m := range result {
		if m.Content.HasBlocks() {
			for _, b := range m.Content.Blocks() {
				if b.Type == "text" && b.Text == "Let me fetch that." {
					found = true
				}
			}
		}
	}
	if !found {
		t.Error("text content should survive after stripping orphaned tool_use")
	}
}

func TestSanitizeHistory_DropsAssistantWithOnlyOrphanedToolUse(t *testing.T) {
	// Assistant message has ONLY a tool_use block (no text).
	// After stripping the orphan, the message is empty → dropped entirely.
	msgs := []client.Message{
		{Role: "user", Content: client.NewTextContent("do something")},
		{Role: "assistant", Content: client.NewBlockContent([]client.ContentBlock{
			client.NewToolUseBlock("toolu_orphan2", "bash", nil),
		})},
		{Role: "user", Content: client.NewTextContent("hello")},
		{Role: "assistant", Content: client.NewTextContent("hi")},
	}
	result := SanitizeHistory(msgs)

	// Should be: user("hello") → assistant("hi")
	// The first user + empty assistant are dropped/merged.
	if len(result) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(result))
	}
	if result[0].Role != "user" {
		t.Errorf("expected user, got %s", result[0].Role)
	}
	if result[1].Content.Text() != "hi" {
		t.Errorf("expected 'hi', got %q", result[1].Content.Text())
	}
}

func TestSanitizeHistory_PreservesMatchedToolUse(t *testing.T) {
	// Normal case: tool_use has a matching tool_result → preserved.
	msgs := []client.Message{
		{Role: "user", Content: client.NewTextContent("search")},
		{Role: "assistant", Content: client.NewBlockContent([]client.ContentBlock{
			{Type: "text", Text: "Searching..."},
			client.NewToolUseBlock("toolu_matched", "web_search", nil),
		})},
		{Role: "user", Content: client.NewBlockContent([]client.ContentBlock{
			client.NewToolResultBlock("toolu_matched", "results here", false),
		})},
		{Role: "assistant", Content: client.NewTextContent("found it")},
	}
	result := SanitizeHistory(msgs)

	if len(result) != 4 {
		t.Fatalf("expected 4 messages, got %d", len(result))
	}

	// Verify the tool_use block is preserved.
	assistantBlocks := result[1].Content.Blocks()
	hasToolUse := false
	for _, b := range assistantBlocks {
		if b.Type == "tool_use" && b.ID == "toolu_matched" {
			hasToolUse = true
		}
	}
	if !hasToolUse {
		t.Error("matched tool_use should be preserved")
	}
}

func TestSanitizeHistory_NonAdjacentToolResultDoesNotMatch(t *testing.T) {
	// tool_result exists later in conversation but not adjacent to the tool_use.
	// The tool_use should still be stripped (adjacency required).
	msgs := []client.Message{
		{Role: "user", Content: client.NewTextContent("fetch page")},
		{Role: "assistant", Content: client.NewBlockContent([]client.ContentBlock{
			{Type: "text", Text: "Fetching..."},
			client.NewToolUseBlock("toolu_nonadj", "web_fetch", nil),
		})},
		{Role: "user", Content: client.NewTextContent("actually never mind")},
		{Role: "assistant", Content: client.NewTextContent("OK, cancelled")},
		// tool_result appears later but not adjacent to the tool_use
		{Role: "user", Content: client.NewBlockContent([]client.ContentBlock{
			client.NewToolResultBlock("toolu_nonadj", "late result", false),
		})},
		{Role: "assistant", Content: client.NewTextContent("done")},
	}
	result := SanitizeHistory(msgs)

	// The tool_use in the first assistant should be stripped.
	for _, m := range result {
		if m.Content.HasBlocks() {
			for _, b := range m.Content.Blocks() {
				if b.Type == "tool_use" && b.ID == "toolu_nonadj" {
					t.Error("non-adjacent tool_use should be stripped")
				}
			}
		}
	}

	// The non-adjacent tool_result should also be stripped (its tool_use is gone).
	for _, m := range result {
		if m.Content.HasBlocks() {
			for _, b := range m.Content.Blocks() {
				if b.Type == "tool_result" && b.ToolUseID == "toolu_nonadj" {
					t.Error("orphaned tool_result should be stripped")
				}
			}
		}
	}
}

func TestSanitizeHistory_MergeDeletesToolUseLeavingOrphanResult(t *testing.T) {
	// Consecutive assistant merge deletes the one with tool_use,
	// orphaning the tool_result in the following user message.
	msgs := []client.Message{
		{Role: "user", Content: client.NewTextContent("do it")},
		{Role: "assistant", Content: client.NewBlockContent([]client.ContentBlock{
			client.NewToolUseBlock("toolu_merged", "bash", nil),
		})},
		{Role: "assistant", Content: client.NewTextContent("noise")},
		{Role: "user", Content: client.NewBlockContent([]client.ContentBlock{
			client.NewToolResultBlock("toolu_merged", "output", false),
		})},
		{Role: "assistant", Content: client.NewTextContent("done")},
	}
	result := SanitizeHistory(msgs)

	// After merge: assistant("noise") replaces assistant(tool_use).
	// The tool_result should then be stripped as orphaned.
	for _, m := range result {
		if m.Content.HasBlocks() {
			for _, b := range m.Content.Blocks() {
				if b.Type == "tool_result" && b.ToolUseID == "toolu_merged" {
					t.Error("tool_result orphaned by merge should be stripped")
				}
			}
		}
	}

	// Verify alternation is maintained.
	for i := 1; i < len(result); i++ {
		if result[i].Role == result[i-1].Role {
			t.Errorf("consecutive same role at %d: %s", i, result[i].Role)
		}
	}
}

func TestSanitizeHistory_DuplicateIDNonAdjacentStripped(t *testing.T) {
	// A valid pair uses ID "toolu_dup", then a stale tool_result reuses
	// the same ID later. The stale one must be stripped.
	msgs := []client.Message{
		{Role: "user", Content: client.NewTextContent("search")},
		{Role: "assistant", Content: client.NewBlockContent([]client.ContentBlock{
			client.NewToolUseBlock("toolu_dup", "web_search", nil),
		})},
		{Role: "user", Content: client.NewBlockContent([]client.ContentBlock{
			client.NewToolResultBlock("toolu_dup", "valid result", false),
		})},
		{Role: "assistant", Content: client.NewTextContent("got it")},
		// Stale duplicate — same ID but not adjacent to any tool_use.
		{Role: "user", Content: client.NewBlockContent([]client.ContentBlock{
			client.NewToolResultBlock("toolu_dup", "stale duplicate", false),
		})},
		{Role: "assistant", Content: client.NewTextContent("done")},
	}
	result := SanitizeHistory(msgs)

	// The valid pair should survive.
	if !result[1].Content.HasBlocks() {
		t.Fatal("valid assistant with tool_use should be preserved")
	}
	foundValidUse := false
	for _, b := range result[1].Content.Blocks() {
		if b.Type == "tool_use" && b.ID == "toolu_dup" {
			foundValidUse = true
		}
	}
	if !foundValidUse {
		t.Error("adjacent tool_use should be preserved")
	}

	// The stale duplicate tool_result should be stripped.
	for i, m := range result {
		if i <= 2 {
			continue // skip the valid pair
		}
		if m.Content.HasBlocks() {
			for _, b := range m.Content.Blocks() {
				if b.Type == "tool_result" && b.ToolUseID == "toolu_dup" {
					t.Errorf("stale duplicate tool_result at position %d should be stripped", i)
				}
			}
		}
	}

	// Verify alternation.
	for i := 1; i < len(result); i++ {
		if result[i].Role == result[i-1].Role {
			t.Errorf("consecutive same role at %d: %s", i, result[i].Role)
		}
	}
}

func TestSanitizeHistory_LegacyToolRoleDropped(t *testing.T) {
	// Legacy tool-role messages (even with proper tool_result blocks) should
	// be dropped since the pairing pass only recognizes user-role tool_results.
	msgs := []client.Message{
		{Role: "user", Content: client.NewTextContent("hello")},
		{Role: "assistant", Content: client.NewBlockContent([]client.ContentBlock{
			client.NewToolUseBlock("toolu_legacy", "bash", nil),
		})},
		{Role: "tool", Content: client.NewBlockContent([]client.ContentBlock{
			client.NewToolResultBlock("toolu_legacy", "output", false),
		})},
		{Role: "assistant", Content: client.NewTextContent("done")},
	}
	result := SanitizeHistory(msgs)

	for _, m := range result {
		if m.Role == "tool" {
			t.Error("legacy tool-role message should be dropped")
		}
	}

	// Verify alternation.
	for i := 1; i < len(result); i++ {
		if result[i].Role == result[i-1].Role {
			t.Errorf("consecutive same role at %d: %s", i, result[i].Role)
		}
	}
}

func truncStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// TestSanitizeHistory_NormalizesStaleToolUseInput verifies that SanitizeHistory
// rewrites any stale tool_use block whose Input is null/empty to "{}" so that
// sessions persisted before the issue #45 fix can resume without replaying a
// poisoned history that would trigger Anthropic's schema validator.
func TestSanitizeHistory_NormalizesStaleToolUseInput(t *testing.T) {
	msgs := []client.Message{
		{Role: "user", Content: client.NewTextContent("take a snapshot")},
		{Role: "assistant", Content: client.NewBlockContent([]client.ContentBlock{
			// These represent poisoned history loaded from a session file
			// written by a pre-fix version of ShanClaw.
			{Type: "tool_use", ID: "tu_null", Name: "browser_snapshot", Input: json.RawMessage("null")},
			{Type: "tool_use", ID: "tu_nil", Name: "browser_close"},
			{Type: "tool_use", ID: "tu_ok", Name: "browser_navigate", Input: json.RawMessage(`{"url":"https://example.com"}`)},
		})},
		{Role: "user", Content: client.NewBlockContent([]client.ContentBlock{
			client.NewToolResultBlock("tu_null", "ok", false),
			client.NewToolResultBlock("tu_nil", "ok", false),
			client.NewToolResultBlock("tu_ok", "ok", false),
		})},
	}

	result := SanitizeHistory(msgs)

	// Find the assistant message and inspect its tool_use blocks.
	var asstBlocks []client.ContentBlock
	for _, m := range result {
		if m.Role == "assistant" && m.Content.HasBlocks() {
			asstBlocks = m.Content.Blocks()
			break
		}
	}
	if len(asstBlocks) != 3 {
		t.Fatalf("expected 3 assistant blocks, got %d", len(asstBlocks))
	}
	for _, b := range asstBlocks {
		if b.Type != "tool_use" {
			continue
		}
		switch b.ID {
		case "tu_null", "tu_nil":
			if string(b.Input) != "{}" {
				t.Errorf("block %s: expected Input=\"{}\", got %q", b.ID, string(b.Input))
			}
		case "tu_ok":
			if string(b.Input) != `{"url":"https://example.com"}` {
				t.Errorf("block tu_ok: populated Input must be preserved, got %q", string(b.Input))
			}
		}
	}
}

// TestSanitizeHistory_DoesNotMutateInput verifies the long-standing contract
// of SanitizeHistory: it returns a new slice and must not mutate the caller's
// messages or any nested block slices. This guards against a naive in-place
// rewrite of the tool_use normalization pass.
func TestSanitizeHistory_DoesNotMutateInput(t *testing.T) {
	origInput := json.RawMessage("null")
	origBlocks := []client.ContentBlock{
		{Type: "tool_use", ID: "tu_1", Name: "browser_snapshot", Input: origInput},
	}
	origUserBlocks := []client.ContentBlock{
		client.NewToolResultBlock("tu_1", "ok", false),
	}
	msgs := []client.Message{
		{Role: "user", Content: client.NewTextContent("snap")},
		{Role: "assistant", Content: client.NewBlockContent(origBlocks)},
		{Role: "user", Content: client.NewBlockContent(origUserBlocks)},
	}

	_ = SanitizeHistory(msgs)

	// The original tool_use block's Input must still be the stale "null"
	// value — SanitizeHistory must have rebuilt a new slice, not mutated
	// the caller's.
	if string(origBlocks[0].Input) != "null" {
		t.Errorf("SanitizeHistory mutated caller's block Input: got %q, want %q", string(origBlocks[0].Input), "null")
	}
	if string(origInput) != "null" {
		t.Errorf("SanitizeHistory mutated caller's RawMessage: got %q, want %q", string(origInput), "null")
	}
}

// --- Empty assistant repair (docs/empty-assistant-content-400.md) ---
//
// Bug: ShanClaw's final response path used to persist
// `assistant content: ""` when the LLM returned err==nil with no tool
// calls AND no visible text. Replaying that history triggers Anthropic's
// "cache_control cannot be set for empty text blocks" 400.
// SanitizeHistory must repair such poisoned sessions on the way to the
// wire AND on resume.

// Plain string content with empty text on assistant must be dropped.
// Surrounding user content must survive — content-preserving merge for
// the newly-adjacent users that result from the drop.
func TestSanitizeHistory_DropsPlainEmptyAssistant(t *testing.T) {
	msgs := []client.Message{
		{Role: "user", Content: client.NewTextContent("first")},
		{Role: "assistant", Content: client.NewTextContent("")},
		{Role: "user", Content: client.NewTextContent("我的图呢")},
	}
	result := SanitizeHistory(msgs)

	for i, m := range result {
		if m.Role == "assistant" && !m.Content.HasBlocks() && m.Content.Text() == "" {
			t.Errorf("empty assistant survived at %d: %+v", i, m)
		}
	}

	var userTexts []string
	for _, m := range result {
		if m.Role == "user" {
			userTexts = append(userTexts, m.Content.Text())
		}
	}
	combined := strings.Join(userTexts, " | ")
	if !strings.Contains(combined, "first") {
		t.Errorf("first user content dropped after empty-assistant repair; got %q", combined)
	}
	if !strings.Contains(combined, "我的图呢") {
		t.Errorf("second user content dropped after empty-assistant repair; got %q", combined)
	}
}

// Block content with NO blocks AND empty text on assistant — same as bare empty.
// `MessageContent{blocks: nil, text: ""}` and `MessageContent{blocks: []}` both
// serialize to "" via MessageContent.MarshalJSON.
func TestSanitizeHistory_DropsEmptyBlockListAssistant(t *testing.T) {
	msgs := []client.Message{
		{Role: "user", Content: client.NewTextContent("hi")},
		{Role: "assistant", Content: client.NewBlockContent(nil)},
		{Role: "user", Content: client.NewTextContent("there")},
	}
	result := SanitizeHistory(msgs)
	for i, m := range result {
		if m.Role == "assistant" && !m.Content.HasBlocks() && m.Content.Text() == "" {
			t.Errorf("empty-block-list assistant survived at %d", i)
		}
	}
}

// Mixed blocks: thinking + empty text. The empty text block is the one that
// triggers the 400; strip it but keep the thinking block (which preserves
// the assistant turn's reasoning trail across resume).
func TestSanitizeHistory_StripsEmptyTextBlockFromAssistant(t *testing.T) {
	msgs := []client.Message{
		{Role: "user", Content: client.NewTextContent("hi")},
		{Role: "assistant", Content: client.NewBlockContent([]client.ContentBlock{
			{Type: "thinking", Thinking: "plan", Signature: "sig"},
			{Type: "text", Text: ""},
		})},
		{Role: "user", Content: client.NewTextContent("ok")},
	}
	result := SanitizeHistory(msgs)

	var asst *client.Message
	for i := range result {
		if result[i].Role == "assistant" {
			asst = &result[i]
		}
	}
	if asst == nil {
		t.Fatal("assistant message dropped — thinking block should have survived")
	}
	blocks := asst.Content.Blocks()
	for _, b := range blocks {
		if b.Type == "text" && b.Text == "" {
			t.Errorf("empty text block survived in assistant blocks")
		}
	}
	hasThinking := false
	for _, b := range blocks {
		if b.Type == "thinking" && b.Thinking == "plan" {
			hasThinking = true
		}
	}
	if !hasThinking {
		t.Error("thinking block must be preserved when stripping empty text blocks")
	}
}

// All-empty-text blocks → effectively empty assistant → drop entirely.
func TestSanitizeHistory_DropsAllEmptyTextBlockAssistant(t *testing.T) {
	msgs := []client.Message{
		{Role: "user", Content: client.NewTextContent("hi")},
		{Role: "assistant", Content: client.NewBlockContent([]client.ContentBlock{
			{Type: "text", Text: ""},
			{Type: "text", Text: ""},
		})},
		{Role: "user", Content: client.NewTextContent("there")},
	}
	result := SanitizeHistory(msgs)
	for i, m := range result {
		if m.Role == "assistant" {
			t.Errorf("all-empty-text-block assistant survived at %d: %+v", i, m)
		}
	}
}

// Replicates the exact 2026-05-19 session shape:
// user → asst(tool_use) → user(tool_result) → asst("") → user("我的图呢").
// Dropping the empty assistant produces user(tool_result) + user(text)
// adjacency. A keep-later merge would discard the tool_result and orphan
// the prior assistant's tool_use, replacing one 400 with another. The
// repair must content-merge instead so the tool_use/tool_result pair
// stays intact.
func TestSanitizeHistory_EmptyAssistantDropPreservesToolResultPair(t *testing.T) {
	msgs := []client.Message{
		{Role: "user", Content: client.NewTextContent("read this")},
		{Role: "assistant", Content: client.NewBlockContent([]client.ContentBlock{
			{Type: "tool_use", ID: "tu_1", Name: "file_read", Input: json.RawMessage(`{"path":"/x"}`)},
		})},
		{Role: "user", Content: client.NewBlockContent([]client.ContentBlock{
			client.NewToolResultBlock("tu_1", "file contents", false),
		})},
		{Role: "assistant", Content: client.NewTextContent("")},
		{Role: "user", Content: client.NewTextContent("我的图呢")},
	}
	result := SanitizeHistory(msgs)

	for i, m := range result {
		if m.Role == "assistant" && !m.Content.HasBlocks() && m.Content.Text() == "" {
			t.Errorf("empty assistant survived at %d", i)
		}
	}

	var foundToolUse, foundToolResult bool
	for _, m := range result {
		if !m.Content.HasBlocks() {
			continue
		}
		for _, b := range m.Content.Blocks() {
			if b.Type == "tool_use" && b.ID == "tu_1" {
				foundToolUse = true
			}
			if b.Type == "tool_result" && b.ToolUseID == "tu_1" {
				foundToolResult = true
			}
		}
	}
	if !foundToolUse {
		t.Error("tool_use dropped — would orphan and 400")
	}
	if !foundToolResult {
		t.Error("tool_result dropped — would orphan and 400")
	}

	var allText strings.Builder
	for _, m := range result {
		allText.WriteString(m.Content.Text())
	}
	if !strings.Contains(allText.String(), "我的图呢") {
		t.Errorf("user text 我的图呢 lost during empty-assistant repair; got %q", allText.String())
	}
}

// RepairEmptyAssistantContent is also called directly from agent.messagesForLLM
// as a wire-time guard. Verify the exported helper handles the same shapes.
func TestRepairEmptyAssistantContent_DropsBareEmptyAndMergesUsers(t *testing.T) {
	in := []client.Message{
		{Role: "user", Content: client.NewTextContent("first")},
		{Role: "assistant", Content: client.NewTextContent("")},
		{Role: "user", Content: client.NewTextContent("我的图呢")},
	}
	out := RepairEmptyAssistantContent(in)
	for i, m := range out {
		if m.Role == "assistant" && !m.Content.HasBlocks() && m.Content.Text() == "" {
			t.Errorf("empty assistant survived at %d", i)
		}
	}
	var combined strings.Builder
	for _, m := range out {
		combined.WriteString(m.Content.Text())
	}
	if !strings.Contains(combined.String(), "first") || !strings.Contains(combined.String(), "我的图呢") {
		t.Errorf("user content lost in RepairEmptyAssistantContent; got %q", combined.String())
	}
}

// Idempotence: running the repair on already-clean history must be a no-op.
func TestRepairEmptyAssistantContent_Idempotent(t *testing.T) {
	in := []client.Message{
		{Role: "user", Content: client.NewTextContent("hi")},
		{Role: "assistant", Content: client.NewTextContent("hello")},
	}
	out1 := RepairEmptyAssistantContent(in)
	out2 := RepairEmptyAssistantContent(out1)
	if len(out1) != len(out2) {
		t.Fatalf("non-idempotent: %d vs %d", len(out1), len(out2))
	}
	for i := range out1 {
		if out1[i].Content.Text() != out2[i].Content.Text() {
			t.Errorf("idempotence broken at %d: %q vs %q", i, out1[i].Content.Text(), out2[i].Content.Text())
		}
	}
}

// Bot review #4: inverse-shape regression. RepairEmptyAssistantContent must
// NOT touch user adjacency that was already present in the input (no empty-
// assistant drop between them). That case stays mergeConsecutiveRoles' job
// with its established keep-later semantics — tested by
// TestSanitizeHistory_MergesConsecutiveUser. This explicit, separately-
// scoped test pins the contract so a future refactor of the merge logic
// can't silently move the responsibility.
func TestRepairEmptyAssistantContent_DoesNotMergePreExistingAdjacentUsers(t *testing.T) {
	in := []client.Message{
		{Role: "user", Content: client.NewTextContent("first")},
		{Role: "user", Content: client.NewTextContent("second")},
		{Role: "user", Content: client.NewTextContent("third")},
		{Role: "assistant", Content: client.NewTextContent("ack")},
	}
	out := RepairEmptyAssistantContent(in)
	if len(out) != len(in) {
		t.Fatalf("repair must not merge already-adjacent users (no drop happened); got %d, want %d", len(out), len(in))
	}
	for i := range in {
		if out[i].Role != in[i].Role || out[i].Content.Text() != in[i].Content.Text() {
			t.Errorf("msg %d mutated: got {%s,%q}, want {%s,%q}",
				i, out[i].Role, out[i].Content.Text(),
				in[i].Role, in[i].Content.Text())
		}
	}
}

// Fast-path: clean history must return the input slice unchanged (same
// header) — no allocation, no copy. This both proves the optimization
// works and guards against an accidental re-introduction of unconditional
// `make([]client.Message, 0, ...)` that would defeat it.
func TestRepairEmptyAssistantContent_FastPathReturnsInputUnchanged(t *testing.T) {
	in := []client.Message{
		{Role: "user", Content: client.NewTextContent("hi")},
		{Role: "assistant", Content: client.NewTextContent("hello")},
		{Role: "user", Content: client.NewTextContent("bye")},
		{Role: "assistant", Content: client.NewTextContent("see you")},
	}
	out := RepairEmptyAssistantContent(in)
	if &out[0] != &in[0] {
		t.Error("clean history should return the input slice header unchanged (fast path)")
	}
}

// Cache-debug instrumentation: with SHANNON_CACHE_DEBUG=1, each drop / strip /
// merge emits a row in ~/.shannon/logs/cache-debug.log so the analyst can
// attribute next-request msg_hashes drift to this repair pass. Per CLAUDE.md
// "Prompt Cache": all wire-shape mutators MUST log.
//
// We trigger all three actions in one fixture:
//   user("first") → assistant("") → assistant([thinking,""]) → user("我的图呢")
// After repair: drop the bare-empty assistant, strip the empty text block from
// the next assistant, then merge the two users (the second drop creates
// user/user adjacency between user("first") and user("我的图呢")). Wait — that
// fixture doesn't actually produce a merge because the second assistant is
// kept. We use a focused shape instead.
func TestRepairEmptyAssistantContent_EmitsCacheCompactEvents(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("SHANNON_CACHE_DEBUG", "1")

	in := []client.Message{
		// Will be kept; empty text block is the only repair here → strip event.
		{Role: "user", Content: client.NewTextContent("u0")},
		{Role: "assistant", Content: client.NewBlockContent([]client.ContentBlock{
			{Type: "thinking", Thinking: "plan", Signature: "sig"},
			{Type: "text", Text: ""},
		})},
		// Drop + merge: bare-empty assistant between two users.
		{Role: "user", Content: client.NewTextContent("u2")},
		{Role: "assistant", Content: client.NewTextContent("")},
		{Role: "user", Content: client.NewTextContent("u4")},
	}
	out := RepairEmptyAssistantContent(in)

	// Verify the wire-side effect first (independent of the log):
	// - assistant at idx 1 in input survives with thinking-only blocks
	// - assistant at idx 3 is gone
	// - users at idx 2 and 4 are merged
	if len(out) != 3 {
		t.Fatalf("expected 3 messages (u0, asst-thinking, u2+u4), got %d", len(out))
	}

	logPath := filepath.Join(tmp, ".shannon", "logs", "cache-debug.log")
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read cache-debug.log: %v", err)
	}

	type event struct {
		Dir    string `json:"dir"`
		Action string `json:"action"`
		MsgIdx int    `json:"msg_idx"`
	}
	var events []event
	for line := range strings.SplitSeq(string(data), "\n") {
		if line == "" {
			continue
		}
		var ev event
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue
		}
		if ev.Dir == "compact" {
			events = append(events, ev)
		}
	}

	wantActions := map[string]bool{
		"empty_text_block_strip": false,
		"empty_assistant_drop":   false,
		"repair_user_merge":      false,
	}
	for _, ev := range events {
		if _, ok := wantActions[ev.Action]; ok {
			wantActions[ev.Action] = true
		}
	}
	for action, seen := range wantActions {
		if !seen {
			t.Errorf("expected cache-compact action %q to be emitted; events=%+v", action, events)
		}
	}
}
