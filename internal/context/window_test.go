package context

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

func TestEstimateTokens(t *testing.T) {
	t.Run("empty messages", func(t *testing.T) {
		got := EstimateTokens(nil)
		if got != 0 {
			t.Errorf("EstimateTokens(nil) = %d, want 0", got)
		}
	})

	t.Run("single text message", func(t *testing.T) {
		msgs := []client.Message{
			{Role: "user", Content: client.NewTextContent("hello world")}, // 11 chars
		}
		got := EstimateTokens(msgs)
		// 11 chars / 3.5 ≈ 3.14, ceil = 4, + 4 overhead = 8
		want := 8
		if got != want {
			t.Errorf("EstimateTokens = %d, want %d", got, want)
		}
	})

	t.Run("multiple messages accumulate", func(t *testing.T) {
		msgs := []client.Message{
			{Role: "system", Content: client.NewTextContent("You are a helpful assistant.")}, // 28 chars
			{Role: "user", Content: client.NewTextContent("hello")},                          // 5 chars
			{Role: "assistant", Content: client.NewTextContent("Hi there!")},                  // 9 chars
		}
		got := EstimateTokens(msgs)
		// (28/3.5 + 4) + (5/3.5 + 4) + (9/3.5 + 4) = 12+4 + 2+4 + 3+4 = 29
		// ceil(28/3.5)=8, ceil(5/3.5)=2, ceil(9/3.5)=3 → 8+4 + 2+4 + 3+4 = 25
		want := 25
		if got != want {
			t.Errorf("EstimateTokens = %d, want %d", got, want)
		}
	})

	t.Run("block content with tool results", func(t *testing.T) {
		blocks := []client.ContentBlock{
			{Type: "text", Text: "result data here"}, // 16 chars
		}
		msgs := []client.Message{
			{Role: "user", Content: client.NewBlockContent(blocks)},
		}
		got := EstimateTokens(msgs)
		// ceil(16/3.5) + 4 = 5 + 4 = 9
		want := 9
		if got != want {
			t.Errorf("EstimateTokens = %d, want %d", got, want)
		}
	})
}

func TestShouldCompact(t *testing.T) {
	t.Run("below threshold returns false", func(t *testing.T) {
		// 90% of 128000 = 115200
		got := ShouldCompact(50000, 1000, 128000)
		if got {
			t.Error("ShouldCompact should be false when well below threshold")
		}
	})

	t.Run("input plus output above threshold returns true", func(t *testing.T) {
		// 105000 + 15000 = 120000 > 115200
		got := ShouldCompact(105000, 15000, 128000)
		if !got {
			t.Error("ShouldCompact should be true when input+output exceeds 90% of context window")
		}
	})

	t.Run("exactly at threshold returns true", func(t *testing.T) {
		// 90% of 128000 = 115200
		got := ShouldCompact(110000, 5200, 128000)
		if !got {
			t.Error("ShouldCompact should be true at exactly the threshold")
		}
	})

	t.Run("zero context window returns false", func(t *testing.T) {
		got := ShouldCompact(100000, 10000, 0)
		if got {
			t.Error("ShouldCompact should be false with zero context window")
		}
	})
}

func TestShapeHistory(t *testing.T) {
	// Helper to build a sequence of user/assistant turn pairs
	makeTurns := func(n int) []client.Message {
		var msgs []client.Message
		for i := 0; i < n; i++ {
			msgs = append(msgs,
				client.Message{Role: "user", Content: client.NewTextContent("user msg " + string(rune('A'+i)))},
				client.Message{Role: "assistant", Content: client.NewTextContent("assistant msg " + string(rune('A'+i)))},
			)
		}
		return msgs
	}

	t.Run("short history unchanged", func(t *testing.T) {
		system := client.Message{Role: "system", Content: client.NewTextContent("system prompt")}
		turns := makeTurns(3)
		all := append([]client.Message{system}, turns...)

		got := ShapeHistory(all, "summary text", 128000)

		// History is small, no shaping needed — should return original unchanged
		if len(got) != len(all) {
			t.Errorf("ShapeHistory should not shape short history, got %d messages, want %d", len(got), len(all))
		}
	})

	t.Run("long history is shaped with summary", func(t *testing.T) {
		system := client.Message{Role: "system", Content: client.NewTextContent("system prompt")}
		turns := makeTurns(30) // 60 messages
		all := append([]client.Message{system}, turns...)

		got := ShapeHistory(all, "summary of dropped turns", 128000)

		// Should have: system + first user msg + summary + last N turn pairs
		if len(got) < 5 {
			t.Fatalf("ShapeHistory returned too few messages: %d", len(got))
		}

		// First message is system
		if got[0].Role != "system" {
			t.Errorf("first message should be system, got %s", got[0].Role)
		}

		// Second message is the first user message (primer)
		if got[1].Role != "user" {
			t.Errorf("second message should be first user message, got %s", got[1].Role)
		}
		if got[1].Content.Text() != "user msg A" {
			t.Errorf("second message should be original first user msg, got %q", got[1].Content.Text())
		}

		// Third message is the summary injection
		if got[2].Role != "user" {
			t.Errorf("third message should be summary (user role), got %s", got[2].Role)
		}
		if got[2].Content.Text() == "" {
			t.Error("summary message should not be empty")
		}

		// Last message should be the last assistant message from original
		lastOrig := all[len(all)-1]
		lastShaped := got[len(got)-1]
		if lastShaped.Content.Text() != lastOrig.Content.Text() {
			t.Errorf("last message should be preserved, got %q want %q",
				lastShaped.Content.Text(), lastOrig.Content.Text())
		}

		// Should be shorter than original
		if len(got) >= len(all) {
			t.Errorf("shaped history should be shorter: got %d, original %d", len(got), len(all))
		}
	})

	t.Run("budget-aware shrinking", func(t *testing.T) {
		system := client.Message{Role: "system", Content: client.NewTextContent("system prompt")}
		// Create turns with large content to force budget-aware shrinking
		var turns []client.Message
		bigContent := make([]byte, 10000)
		for i := range bigContent {
			bigContent[i] = 'x'
		}
		for i := 0; i < 30; i++ {
			turns = append(turns,
				client.Message{Role: "user", Content: client.NewTextContent(string(bigContent))},
				client.Message{Role: "assistant", Content: client.NewTextContent(string(bigContent))},
			)
		}
		all := append([]client.Message{system}, turns...)

		// Small context window forces aggressive shrinking
		got := ShapeHistory(all, "summary", 5000)

		// Should keep minimum: system + first user + summary + at least 3 pairs (6 msgs)
		// Total minimum = 1 + 1 + 1 + 6 = 9
		if len(got) > len(all) {
			t.Errorf("shaped should not grow: got %d, original %d", len(got), len(all))
		}

		// Verify minimum floor of 3 recent pairs is maintained
		// Count non-system, non-primer, non-summary messages
		recentCount := len(got) - 3 // subtract system + primer + summary
		if recentCount < 6 {        // 3 pairs = 6 messages minimum
			t.Errorf("should keep at least 3 recent pairs (6 msgs), got %d recent msgs", recentCount)
		}
	})

	t.Run("few messages but over token budget still shapes", func(t *testing.T) {
		system := client.Message{Role: "system", Content: client.NewTextContent("system prompt")}
		// 10 turn pairs (20 msgs) — below old message-count gate of 43
		// but each message is huge, blowing the token budget
		bigContent := make([]byte, 50000)
		for i := range bigContent {
			bigContent[i] = 'x'
		}
		var turns []client.Message
		for i := 0; i < 10; i++ {
			turns = append(turns,
				client.Message{Role: "user", Content: client.NewTextContent(string(bigContent))},
				client.Message{Role: "assistant", Content: client.NewTextContent(string(bigContent))},
			)
		}
		all := append([]client.Message{system}, turns...)

		// Estimated tokens: ~10 * 2 * (50000/3.5 + 4) ≈ 285k, way over 5000
		got := ShapeHistory(all, "summary of prior work", 5000)

		// Should be shaped (shorter than original)
		if len(got) >= len(all) {
			t.Errorf("few-but-large messages should still be shaped: got %d, original %d", len(got), len(all))
		}

		// Should contain summary
		found := false
		for _, m := range got {
			if m.Content.Text() == "Previous context summary: summary of prior work" {
				found = true
				break
			}
		}
		if !found {
			t.Error("should contain summary message when token budget forces shaping")
		}
	})

	t.Run("empty summary skips injection", func(t *testing.T) {
		system := client.Message{Role: "system", Content: client.NewTextContent("system prompt")}
		turns := makeTurns(30)
		all := append([]client.Message{system}, turns...)

		got := ShapeHistory(all, "", 128000)

		// With empty summary and large window, should still shape but no summary message
		for _, m := range got {
			if m.Role == "user" && m.Content.Text() != "" {
				// Make sure no "Previous context summary" message exists
				text := m.Content.Text()
				if len(text) > 25 && text[:25] == "Previous context summary:" {
					t.Error("should not inject empty summary")
				}
			}
		}
	})

	// Boundary-orphan regression: ShapeHistory keeps the last keepLast*2 messages
	// from the post-firstUser tail. If the slice boundary lands between an
	// assistant tool_use and the matching user tool_result, the result contains
	// an orphaned tool_result that Anthropic's API rejects with 400. The fix
	// must strip orphaned tool blocks at the slice boundary WITHOUT merging
	// consecutive same-role messages (which would drop the original first
	// user prompt next to the summary-as-user message).
	t.Run("strips orphaned tool_result at slice boundary", func(t *testing.T) {
		system := client.Message{Role: "system", Content: client.NewTextContent("system prompt")}
		firstUser := client.Message{Role: "user", Content: client.NewTextContent("user msg A")}

		// Build rest of length 51. With defaultKeepLast=20, keepMsgs=40,
		// recent = rest[11:51]; rest[10] is dropped, rest[11] is recent[0].
		// Place tool_use at rest[10] (dropped) and matching tool_result at
		// rest[11] (kept) so the boundary cuts the pair.
		rest := make([]client.Message, 51)
		for i := 0; i < 51; i++ {
			if i%2 == 0 {
				rest[i] = client.Message{Role: "assistant", Content: client.NewTextContent("a" + string(rune('A'+i)))}
			} else {
				rest[i] = client.Message{Role: "user", Content: client.NewTextContent("u" + string(rune('A'+i)))}
			}
		}
		// Replace rest[10] (assistant, dropped) and rest[11] (user, recent[0]).
		rest[10] = client.Message{Role: "assistant", Content: client.NewBlockContent([]client.ContentBlock{
			{Type: "text", Text: "running tool"},
			client.NewToolUseBlock("toolu_boundary", "bash", nil),
		})}
		rest[11] = client.Message{Role: "user", Content: client.NewBlockContent([]client.ContentBlock{
			client.NewToolResultBlock("toolu_boundary", "ok", false),
		})}

		all := append([]client.Message{system, firstUser}, rest...)
		got := ShapeHistory(all, "summary text", 128000)

		// The original first user message must be preserved (regression guard
		// against a naive post-shape SanitizeHistory that would merge it into
		// the summary user message).
		if len(got) < 2 || got[1].Content.Text() != "user msg A" {
			t.Fatalf("first user message must be preserved verbatim, got %+v", got[1])
		}

		// No orphaned tool_result blocks anywhere in the output.
		toolUseIDs := make(map[string]bool)
		for _, m := range got {
			if !m.Content.HasBlocks() {
				continue
			}
			for _, b := range m.Content.Blocks() {
				if b.Type == "tool_use" && b.ID != "" {
					toolUseIDs[b.ID] = true
				}
			}
		}
		for i, m := range got {
			if !m.Content.HasBlocks() {
				continue
			}
			for _, b := range m.Content.Blocks() {
				if b.Type == "tool_result" && b.ToolUseID != "" && !toolUseIDs[b.ToolUseID] {
					t.Errorf("orphaned tool_result with id %q at position %d (role=%s)", b.ToolUseID, i, m.Role)
				}
			}
		}

		// Specifically: the orphaned tool_result for toolu_boundary must not
		// survive (its tool_use was dropped by the slice).
		for _, m := range got {
			if !m.Content.HasBlocks() {
				continue
			}
			for _, b := range m.Content.Blocks() {
				if b.Type == "tool_result" && b.ToolUseID == "toolu_boundary" {
					t.Error("orphaned tool_result for toolu_boundary should have been stripped at slice boundary")
				}
			}
		}
	})

	// Symmetric case: orphaned tool_use at the back of recent (slice boundary
	// drops the matching tool_result). Anthropic also rejects unpaired tool_use.
	t.Run("strips orphaned tool_use at slice boundary", func(t *testing.T) {
		system := client.Message{Role: "system", Content: client.NewTextContent("system prompt")}
		firstUser := client.Message{Role: "user", Content: client.NewTextContent("user msg A")}

		// Construct rest where the last assistant message has a tool_use whose
		// matching tool_result lives in a later position that does not exist
		// (i.e. tool_use is the final message). This exercises the back-end
		// orphan path that ShapeHistory does not currently sanitize.
		rest := make([]client.Message, 51)
		for i := 0; i < 50; i++ {
			if i%2 == 0 {
				rest[i] = client.Message{Role: "assistant", Content: client.NewTextContent("a" + string(rune('A'+i)))}
			} else {
				rest[i] = client.Message{Role: "user", Content: client.NewTextContent("u" + string(rune('A'+i)))}
			}
		}
		// Final message is an assistant whose only block is an unpaired tool_use.
		rest[50] = client.Message{Role: "assistant", Content: client.NewBlockContent([]client.ContentBlock{
			client.NewToolUseBlock("toolu_tail", "bash", nil),
		})}

		all := append([]client.Message{system, firstUser}, rest...)
		got := ShapeHistory(all, "summary text", 128000)

		// No orphaned tool_use should remain.
		toolResultIDs := make(map[string]bool)
		for _, m := range got {
			if !m.Content.HasBlocks() {
				continue
			}
			for _, b := range m.Content.Blocks() {
				if b.Type == "tool_result" && b.ToolUseID != "" {
					toolResultIDs[b.ToolUseID] = true
				}
			}
		}
		for i, m := range got {
			if !m.Content.HasBlocks() {
				continue
			}
			for _, b := range m.Content.Blocks() {
				if b.Type == "tool_use" && b.ID != "" && !toolResultIDs[b.ID] {
					t.Errorf("orphaned tool_use with id %q at position %d", b.ID, i)
				}
			}
		}
	})
}

// TestTruncateOversizedLastUserMessage covers the short-session single-input
// fallback added to guard P0-#1 from 2026-05-11 stress testing. Without this,
// a 191K-token single user message escapes every client-side defense because
// ShapeHistory and the preflight path are both gated by MinShapeable()=9.
func TestTruncateOversizedLastUserMessage(t *testing.T) {
	t.Run("plain-text oversized user message is truncated", func(t *testing.T) {
		// 200K context, 0.90 threshold = 180K tokens. At 3.5 chars/token that's
		// ~630K chars. Build a user message clearly above this so the function
		// fires.
		huge := strings.Repeat("padding ", 100000) // 800K chars
		msgs := []client.Message{
			{Role: "system", Content: client.NewTextContent("system prompt")},
			{Role: "user", Content: client.NewTextContent(huge)},
		}
		out, dropped := TruncateOversizedLastUserMessage(msgs, 200000)
		if dropped == 0 {
			t.Fatalf("expected truncation when input exceeds threshold, dropped=0")
		}
		if !strings.Contains(out[1].Content.Text(), "user message truncated") {
			t.Error("truncation marker absent from user message body")
		}
		if !utf8.ValidString(out[1].Content.Text()) {
			t.Error("output is not valid UTF-8")
		}
	})

	// Regression: a huge first user message followed by a small follow-up
	// (the daemon/TUI resume case) — must still truncate the huge one, not
	// the small one. Picking "most recent" would silently miss this and
	// the huge message escapes to the API. (See 2026-05-11 GPT review F1.)
	t.Run("huge first user message followed by small follow-up", func(t *testing.T) {
		huge := strings.Repeat("padding ", 100000) // 800K chars
		msgs := []client.Message{
			{Role: "system", Content: client.NewTextContent("system")},
			{Role: "user", Content: client.NewTextContent(huge)},
			{Role: "assistant", Content: client.NewTextContent("ok")},
			{Role: "user", Content: client.NewTextContent("继续")},
		}
		out, dropped := TruncateOversizedLastUserMessage(msgs, 200000)
		if dropped == 0 {
			t.Fatalf("expected truncation of huge first user message, dropped=0")
		}
		// Truncation must hit messages[1] (the huge one), not messages[3] (the small one).
		if !strings.Contains(out[1].Content.Text(), "user message truncated") {
			t.Errorf("huge first user message (msgs[1]) was not truncated — got body of length %d, head %q",
				len(out[1].Content.Text()), out[1].Content.Text()[:min(120, len(out[1].Content.Text()))])
		}
		if out[3].Content.Text() != "继续" {
			t.Errorf("small follow-up user message (msgs[3]) was mutated; got %q want \"继续\"", out[3].Content.Text())
		}
	})

	t.Run("under-threshold message is unchanged", func(t *testing.T) {
		small := strings.Repeat("hello ", 100) // 600 chars, ~170 tokens
		msgs := []client.Message{
			{Role: "user", Content: client.NewTextContent(small)},
		}
		out, dropped := TruncateOversizedLastUserMessage(msgs, 200000)
		if dropped != 0 {
			t.Errorf("expected dropped=0 under threshold, got %d", dropped)
		}
		if out[0].Content.Text() != small {
			t.Errorf("user message body mutated under threshold")
		}
	})

	// Regression for PR review #5: TruncateOversizedLastUserMessage used to
	// pass a rune-count budget into truncateMessageBody which slices by
	// bytes. For CJK content (~3 bytes/rune) the function silently
	// over-truncated to ~1/3 the intended size — safe direction but wasteful.
	// After the fix, the surviving content for CJK should be at least
	// half of what it would be for ASCII at the same budget (we don't
	// expect exact parity, but a 1/3 ratio is too aggressive).
	t.Run("CJK content not over-truncated to byte-budget interpretation", func(t *testing.T) {
		// 200K repeats of "你好世界 " = 1M runes, ~3.2M bytes. Default
		// 0.90 * 200K = 180K tokens × 3.5 chars/token = 630K rune budget.
		// Pre-fix: truncate to 630K *bytes* = ~210K runes (over-trunc 3x).
		// Post-fix: truncate to ~3.2M/1M × 630K bytes = ~2M bytes = ~660K
		// runes — much closer to the intended 630K runes.
		chinese := strings.Repeat("你好世界 ", 200000)
		msgs := []client.Message{
			{Role: "user", Content: client.NewTextContent(chinese)},
		}
		out, dropped := TruncateOversizedLastUserMessage(msgs, 200000)
		if dropped == 0 {
			t.Fatalf("expected truncation; this input is well over threshold")
		}
		body := out[0].Content.Text()
		if !utf8.ValidString(body) {
			t.Errorf("truncation produced invalid UTF-8")
		}
		surviveRunes := utf8.RuneCountInString(body)
		// Floor for "not over-truncated": at least 400K runes survive.
		// Pre-fix this was ~210K runes (over-trunc). Post-fix expected ~660K.
		const minExpectedRunes = 400_000
		if surviveRunes < minExpectedRunes {
			t.Errorf("CJK content over-truncated: %d runes survived, want >= %d (pre-fix bug)",
				surviveRunes, minExpectedRunes)
		}
	})

	t.Run("CJK content stays rune-aligned", func(t *testing.T) {
		// EstimateTokens counts runes (not bytes), so the input has to push
		// rune-count past 0.90 × 200K = 180K tokens × 3.5 chars/token = 630K
		// runes. 200K repeats × 5 runes ("你好世界 ") = 1M runes, well over.
		chinese := strings.Repeat("你好世界 ", 200000)
		if !utf8.ValidString(chinese) {
			t.Fatalf("test setup invariant: input must be valid UTF-8")
		}
		msgs := []client.Message{
			{Role: "user", Content: client.NewTextContent(chinese)},
		}
		out, dropped := TruncateOversizedLastUserMessage(msgs, 200000)
		if dropped == 0 {
			t.Fatalf("expected truncation on huge CJK input")
		}
		body := out[0].Content.Text()
		if !utf8.ValidString(body) {
			t.Errorf("truncation split a rune mid-sequence: invalid UTF-8")
		}
	})

	t.Run("multi-block user message is left alone", func(t *testing.T) {
		// Structured content (tool_result / image) needs different handling
		// — this fallback intentionally skips it.
		msgs := []client.Message{
			{Role: "user", Content: client.NewBlockContent([]client.ContentBlock{
				{Type: "text", Text: strings.Repeat("padding ", 100000)},
			})},
		}
		_, dropped := TruncateOversizedLastUserMessage(msgs, 200000)
		if dropped != 0 {
			t.Errorf("multi-block message should not be touched; dropped=%d", dropped)
		}
	})

	t.Run("zero context window is a no-op", func(t *testing.T) {
		msgs := []client.Message{
			{Role: "user", Content: client.NewTextContent(strings.Repeat("padding ", 100000))},
		}
		_, dropped := TruncateOversizedLastUserMessage(msgs, 0)
		if dropped != 0 {
			t.Errorf("zero contextWindow should be no-op; dropped=%d", dropped)
		}
	})

	t.Run("no user message is a no-op", func(t *testing.T) {
		msgs := []client.Message{
			{Role: "system", Content: client.NewTextContent("just system")},
			{Role: "assistant", Content: client.NewTextContent("just assistant")},
		}
		_, dropped := TruncateOversizedLastUserMessage(msgs, 200000)
		if dropped != 0 {
			t.Errorf("expected no-op when no user msg; dropped=%d", dropped)
		}
	})
}
