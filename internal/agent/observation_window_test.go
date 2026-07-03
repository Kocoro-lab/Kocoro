package agent

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

// browserResultContent extracts the string content of the tool_result block
// carrying the given tool_use ID. Fails the test if it is absent or not a
// string (screenshot image results use nested blocks and are out of scope for
// the text observation window).
func browserResultContent(t *testing.T, msgs []client.Message, toolID string) string {
	t.Helper()
	for _, m := range msgs {
		if m.Role != "user" || !m.Content.HasBlocks() {
			continue
		}
		for _, b := range m.Content.Blocks() {
			if b.Type == "tool_result" && b.ToolUseID == toolID {
				s, ok := b.ToolContent.(string)
				if !ok {
					t.Fatalf("tool_result %s content is %T, want string", toolID, b.ToolContent)
				}
				return s
			}
		}
	}
	t.Fatalf("tool_result %s not found", toolID)
	return ""
}

func isObservationStub(s string) bool {
	return strings.HasPrefix(s, "[elided browser observation:")
}

// --- truncateObservation (Feature 2: per-observation cap with explicit marker) ---

func TestTruncateObservation_UnderCapUnchanged(t *testing.T) {
	s := strings.Repeat("a", 100)
	if got := truncateObservation(s, 24000); got != s {
		t.Fatalf("under-cap content was modified: len=%d", len(got))
	}
}

func TestTruncateObservation_OverCapTruncatedWithMarker(t *testing.T) {
	s := strings.Repeat("a", 50000)
	got := truncateObservation(s, 24000)
	// Inclusive cap: the whole result (kept text + marker) stays within max.
	if n := len([]rune(got)); n > 24000 {
		t.Fatalf("truncated result must stay within the cap; got %d runes", n)
	}
	// Leading page text is preserved (23000 < kept portion, marker is small).
	if !strings.HasPrefix(got, strings.Repeat("a", 23000)) {
		t.Fatalf("kept prefix is not the leading page text")
	}
	if !strings.Contains(got, "truncated") {
		t.Fatalf("truncation marker missing: %q", got[len(got)-80:])
	}
	// Must disclose the original size so the model/audit knows how much was cut.
	if !strings.Contains(got, "50000") {
		t.Fatalf("marker should disclose original char count; got tail %q", got[len(got)-80:])
	}
}

func TestTruncateObservation_TinyCapHonorsContract(t *testing.T) {
	// When max is smaller than the truncation marker itself, the result must
	// STILL never exceed max runes (the function's documented contract).
	s := strings.Repeat("a", 5000)
	for _, max := range []int{1, 5, 10, 20} {
		got := truncateObservation(s, max)
		if n := utf8.RuneCountInString(got); n > max {
			t.Fatalf("max=%d: result is %d runes, exceeds cap", max, n)
		}
	}
}

func TestTruncateObservation_RuneSafe(t *testing.T) {
	// Multi-byte runes must not be split.
	s := strings.Repeat("世", 30000)
	got := truncateObservation(s, 24000)
	if !utf8.ValidString(got) {
		t.Fatalf("result not valid UTF-8 after truncation")
	}
	if n := len([]rune(got)); n > 24000 {
		t.Fatalf("cap exceeded: %d runes", n)
	}
	if !strings.HasPrefix(got, strings.Repeat("世", 23000)) {
		t.Fatalf("multi-byte prefix corrupted")
	}
}

// --- filterOldObservations (Feature 1: sliding window over browser observations) ---

func TestFilterOldObservations_KeepsLastNStubsOlder(t *testing.T) {
	names := make([]string, 10)
	for i := range names {
		names[i] = "browser"
	}
	msgs := makeTestMessages(names)

	stubbed := filterOldObservations(msgs, 3)
	if stubbed != 7 {
		t.Fatalf("stubbed %d observations, want 7", stubbed)
	}

	// tc00..tc06 stubbed; tc07..tc09 full.
	for i := 0; i < 10; i++ {
		id := fmt.Sprintf("tc%02d", i)
		content := browserResultContent(t, msgs, id)
		if i < 7 {
			if !isObservationStub(content) {
				t.Fatalf("observation %d should be stubbed, got %q", i, content)
			}
			if !strings.Contains(content, "browser") {
				t.Fatalf("stub %d should name the tool: %q", i, content)
			}
		} else {
			if isObservationStub(content) {
				t.Fatalf("observation %d should be full, got stub", i)
			}
		}
	}
}

func TestFilterOldObservations_PreservesBlockStructureAndToolUseID(t *testing.T) {
	names := []string{"browser", "browser", "browser", "browser", "browser"}
	msgs := makeTestMessages(names)

	filterOldObservations(msgs, 2)

	// Every tool_use must still have a paired tool_result with the same ID —
	// the API contract requires this. We must replace content, never delete blocks.
	toolUseIDs := map[string]bool{}
	toolResultIDs := map[string]bool{}
	for _, m := range msgs {
		if !m.Content.HasBlocks() {
			continue
		}
		for _, b := range m.Content.Blocks() {
			if b.Type == "tool_use" {
				toolUseIDs[b.ID] = true
			}
			if b.Type == "tool_result" {
				toolResultIDs[b.ToolUseID] = true
			}
		}
	}
	if len(toolUseIDs) != 5 || len(toolResultIDs) != 5 {
		t.Fatalf("block count changed: tool_use=%d tool_result=%d, want 5/5", len(toolUseIDs), len(toolResultIDs))
	}
	for id := range toolUseIDs {
		if !toolResultIDs[id] {
			t.Fatalf("tool_use %s lost its paired tool_result", id)
		}
	}
}

func TestFilterOldObservations_OnlyTouchesBrowserGUITools(t *testing.T) {
	// Interleave non-GUI (bash, file_read) with browser. Only browser
	// observations should ever be stubbed; conversational/other tool turns
	// must be left byte-identical.
	names := []string{"bash", "file_read", "browser", "bash", "file_read", "browser", "bash", "browser"}
	msgs := makeTestMessages(names)

	// Snapshot the non-browser results before.
	before := map[string]string{}
	for i, n := range names {
		if !isGUIToolName(n) {
			id := fmt.Sprintf("tc%02d", i)
			before[id] = browserResultContent(t, msgs, id)
		}
	}

	stubbed := filterOldObservations(msgs, 1) // keep only the last browser observation
	// 3 browser observations, keep 1 → 2 stubbed.
	if stubbed != 2 {
		t.Fatalf("stubbed %d, want 2 (browser only)", stubbed)
	}
	for id, want := range before {
		if got := browserResultContent(t, msgs, id); got != want {
			t.Fatalf("non-browser result %s was modified: %q != %q", id, got, want)
		}
	}
}

func TestFilterOldObservations_DisabledWhenKeepNonPositive(t *testing.T) {
	names := []string{"browser", "browser", "browser", "browser"}
	msgs := makeTestMessages(names)
	before, _ := json.Marshal(msgs)

	if n := filterOldObservations(msgs, 0); n != 0 {
		t.Fatalf("keep=0 stubbed %d, want 0 (disabled)", n)
	}
	after, _ := json.Marshal(msgs)
	if string(before) != string(after) {
		t.Fatalf("keep=0 mutated bytes")
	}

	if n := filterOldObservations(msgs, -5); n != 0 {
		t.Fatalf("keep<0 stubbed %d, want 0", n)
	}
}

func TestFilterOldObservations_NoOpWhenWithinWindow(t *testing.T) {
	names := []string{"browser", "browser"}
	msgs := makeTestMessages(names)
	before, _ := json.Marshal(msgs)

	if n := filterOldObservations(msgs, 3); n != 0 {
		t.Fatalf("count<=keep stubbed %d, want 0", n)
	}
	after, _ := json.Marshal(msgs)
	if string(before) != string(after) {
		t.Fatalf("within-window mutated bytes")
	}
}

func TestFilterOldObservations_Idempotent(t *testing.T) {
	names := make([]string, 8)
	for i := range names {
		names[i] = "browser_navigate" // MCP playwright name (browser_ prefix)
	}
	msgs := makeTestMessages(names)

	first := filterOldObservations(msgs, 3)
	if first != 5 {
		t.Fatalf("first pass stubbed %d, want 5", first)
	}
	snapshot, _ := json.Marshal(msgs)

	second := filterOldObservations(msgs, 3)
	if second != 0 {
		t.Fatalf("second pass stubbed %d, want 0 (idempotent)", second)
	}
	after, _ := json.Marshal(msgs)
	if string(snapshot) != string(after) {
		t.Fatalf("second pass mutated bytes (not idempotent)")
	}
}

// --- Minor boundary coverage for the observation window ---

func TestFilterOldObservations_ExactlyNBoundary(t *testing.T) {
	// keep == count: nothing stubbed (the len(ids) <= keep guard).
	msgs := makeTestMessages([]string{"browser", "browser", "browser"})
	if n := filterOldObservations(msgs, 3); n != 0 {
		t.Fatalf("exactly-N stubbed %d, want 0", n)
	}
	// keep == count-1: exactly one (the oldest) stubbed.
	msgs = makeTestMessages([]string{"browser", "browser", "browser", "browser"})
	if n := filterOldObservations(msgs, 3); n != 1 {
		t.Fatalf("N+1 stubbed %d, want 1", n)
	}
	if got := browserResultContent(t, msgs, "tc00"); !isObservationStub(got) {
		t.Fatalf("oldest observation should be the one stubbed at the boundary")
	}
}

func TestFilterOldObservations_AllNonGUINoOp(t *testing.T) {
	// A session with zero browser/GUI observations must be a byte-for-byte no-op.
	msgs := makeTestMessages([]string{"bash", "file_read", "grep", "http", "bash"})
	before, _ := json.Marshal(msgs)
	if n := filterOldObservations(msgs, 1); n != 0 {
		t.Fatalf("all-non-GUI stubbed %d, want 0", n)
	}
	after, _ := json.Marshal(msgs)
	if string(before) != string(after) {
		t.Fatalf("all-non-GUI session mutated bytes")
	}
}

// --- filterOldBrowserImages (Item 4: keep only the most recent browser screenshot) ---

// makeBrowserImageMessages builds n (assistant browser tool_use, user
// tool_result-with-image) pairs — the native-mode shape browser screenshots
// take in history.
func makeBrowserImageMessages(n int) []client.Message {
	msgs := []client.Message{{Role: "system", Content: client.NewTextContent("system")}}
	for i := 0; i < n; i++ {
		toolID := fmt.Sprintf("bi%02d", i)
		msgs = append(msgs, client.Message{
			Role: "assistant",
			Content: client.NewBlockContent([]client.ContentBlock{
				{Type: "tool_use", ID: toolID, Name: "browser", Input: json.RawMessage(`{"action":"screenshot"}`)},
			}),
		})
		img := client.ContentBlock{Type: "image", Source: &client.ImageSource{Type: "base64", MediaType: "image/png", Data: fmt.Sprintf("IMGDATA%02d", i)}}
		msgs = append(msgs, client.Message{
			Role: "user",
			Content: client.NewBlockContent([]client.ContentBlock{
				client.NewToolResultBlockWithImages(toolID, fmt.Sprintf("screenshot %d", i), []client.ContentBlock{img}, false),
			}),
		})
	}
	return msgs
}

// toolResultImageCount counts image blocks nested inside tool_result blocks.
func toolResultImageCount(msgs []client.Message) int {
	n := 0
	for _, m := range msgs {
		if !m.Content.HasBlocks() {
			continue
		}
		for _, b := range m.Content.Blocks() {
			if b.Type != "tool_result" {
				continue
			}
			if nested, ok := b.ToolContent.([]client.ContentBlock); ok {
				for _, nb := range nested {
					if nb.Type == "image" {
						n++
					}
				}
			}
		}
	}
	return n
}

func topLevelImageCount(msgs []client.Message) int {
	n := 0
	for _, m := range msgs {
		if !m.Content.HasBlocks() {
			continue
		}
		for _, b := range m.Content.Blocks() {
			if b.Type == "image" {
				n++
			}
		}
	}
	return n
}

func TestFilterOldBrowserImages_KeepsOnlyMostRecent(t *testing.T) {
	msgs := makeBrowserImageMessages(5)
	stripped := filterOldBrowserImages(msgs, 1)
	if stripped != 4 {
		t.Fatalf("stripped %d browser screenshots, want 4", stripped)
	}
	if got := toolResultImageCount(msgs); got != 1 {
		t.Fatalf("retained %d browser images, want exactly 1 (most recent)", got)
	}
	// The retained image must be the newest one (bi04).
	found := false
	for _, m := range msgs {
		if !m.Content.HasBlocks() {
			continue
		}
		for _, b := range m.Content.Blocks() {
			if b.Type == "tool_result" && b.ToolUseID == "bi04" {
				if nested, ok := b.ToolContent.([]client.ContentBlock); ok {
					for _, nb := range nested {
						if nb.Type == "image" {
							found = true
						}
					}
				}
			}
		}
	}
	if !found {
		t.Fatalf("most-recent browser screenshot (bi04) was not retained")
	}
}

func TestFilterOldBrowserImages_LeavesNonBrowserImagesUntouched(t *testing.T) {
	// Browser screenshots (GUI tool_result images).
	msgs := makeBrowserImageMessages(3)
	// A user-uploaded image (top-level image block, NOT a tool_result).
	uploadImg := client.ContentBlock{Type: "image", Source: &client.ImageSource{Type: "base64", MediaType: "image/png", Data: "UPLOAD"}}
	msgs = append(msgs, client.Message{Role: "user", Content: client.NewBlockContent([]client.ContentBlock{
		{Type: "text", Text: "compare this"}, uploadImg,
	})})
	// A non-GUI tool image (file_read returning an image nested in a tool_result).
	msgs = append(msgs, client.Message{Role: "assistant", Content: client.NewBlockContent([]client.ContentBlock{
		{Type: "tool_use", ID: "fr00", Name: "file_read", Input: json.RawMessage(`{"path":"a.png"}`)},
	})})
	frImg := client.ContentBlock{Type: "image", Source: &client.ImageSource{Type: "base64", MediaType: "image/png", Data: "FILEIMG"}}
	msgs = append(msgs, client.Message{Role: "user", Content: client.NewBlockContent([]client.ContentBlock{
		client.NewToolResultBlockWithImages("fr00", "a.png", []client.ContentBlock{frImg}, false),
	})})

	uploadsBefore := topLevelImageCount(msgs)

	stripped := filterOldBrowserImages(msgs, 1)
	if stripped != 2 {
		t.Fatalf("stripped %d, want 2 (browser only)", stripped)
	}
	// Non-browser top-level upload image untouched.
	if got := topLevelImageCount(msgs); got != uploadsBefore {
		t.Fatalf("top-level (upload) images changed: %d != %d", got, uploadsBefore)
	}
	// Browser tool_result images: 1 kept. file_read image also survives => total 2.
	if got := toolResultImageCount(msgs); got != 2 {
		t.Fatalf("tool_result images = %d, want 2 (1 browser kept + 1 file_read untouched)", got)
	}
	// The file_read (non-GUI) image specifically must survive.
	frImages := 0
	for _, m := range msgs {
		if !m.Content.HasBlocks() {
			continue
		}
		for _, b := range m.Content.Blocks() {
			if b.Type == "tool_result" && b.ToolUseID == "fr00" {
				if nested, ok := b.ToolContent.([]client.ContentBlock); ok {
					for _, nb := range nested {
						if nb.Type == "image" {
							frImages++
						}
					}
				}
			}
		}
	}
	if frImages != 1 {
		t.Fatalf("file_read image count = %d, want 1 (must not be touched by browser filter)", frImages)
	}
}

func TestFilterOldBrowserImages_DisabledWhenKeepNonPositive(t *testing.T) {
	msgs := makeBrowserImageMessages(4)
	before, _ := json.Marshal(msgs)
	if n := filterOldBrowserImages(msgs, 0); n != 0 {
		t.Fatalf("keep=0 stripped %d, want 0 (disabled)", n)
	}
	after, _ := json.Marshal(msgs)
	if string(before) != string(after) {
		t.Fatalf("keep=0 mutated bytes")
	}
}

// --- Acceptance: synthetic 10-iteration browser session (brief acceptance) ---

func TestAcceptance_TenIterationBrowserSession(t *testing.T) {
	const (
		keepWindow = 3
		obsCap     = 24000
		rawSize    = 57000 // a "fresh page snapshot" spike, larger than the cap
	)

	// Build 10 browser observations exactly as the loop would: each raw result
	// is capped at capture (truncateObservation, Feature 2), then the assembled
	// history is windowed (filterOldObservations, Feature 1).
	names := make([]string, 10)
	for i := range names {
		names[i] = "browser_snapshot"
	}
	msgs := makeTestMessages(names)

	// Simulate capture-time cap: rewrite each observation with a 57K raw page
	// snapshot truncated to the 24K cap.
	for i := range msgs {
		if msgs[i].Role != "user" || !msgs[i].Content.HasBlocks() {
			continue
		}
		blocks := msgs[i].Content.Blocks()
		nb := make([]client.ContentBlock, len(blocks))
		for j, b := range blocks {
			if b.Type == "tool_result" {
				b.ToolContent = truncateObservation(strings.Repeat("x", rawSize), obsCap)
			}
			nb[j] = b
		}
		msgs[i].Content = client.NewBlockContent(nb)
	}

	// Window the assembled context.
	filterOldObservations(msgs, keepWindow)

	// Assert: at most keepWindow full observations, the rest stubs, and every
	// full observation is <= obsCap chars.
	fullCount, stubCount := 0, 0
	for i := 0; i < 10; i++ {
		id := fmt.Sprintf("tc%02d", i)
		content := browserResultContent(t, msgs, id)
		if isObservationStub(content) {
			stubCount++
			continue
		}
		fullCount++
		if n := len([]rune(content)); n > obsCap {
			t.Fatalf("full observation %d is %d chars, exceeds cap %d", i, n, obsCap)
		}
	}
	if fullCount > keepWindow {
		t.Fatalf("got %d full observations, want <= %d", fullCount, keepWindow)
	}
	if fullCount != keepWindow || stubCount != 10-keepWindow {
		t.Fatalf("full=%d stub=%d, want full=%d stub=%d", fullCount, stubCount, keepWindow, 10-keepWindow)
	}

	// Context-size proof: unwindowed would carry 10 * 24000 = 240000 chars of
	// observation; windowed carries ~3 * 24000 + 7 small stubs.
	total := 0
	for i := 0; i < 10; i++ {
		total += len([]rune(browserResultContent(t, msgs, fmt.Sprintf("tc%02d", i))))
	}
	if total > keepWindow*obsCap+7*200 {
		t.Fatalf("windowed observation bytes = %d, expected ~%d", total, keepWindow*obsCap)
	}
}
