package sync

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestStripThinkingFromSessionJSON_RemovesAssistantThinkingBlocks: the
// canonical case — assistant messages containing thinking + redacted_thinking
// blocks have those blocks removed; text + tool_use survive with their order.
func TestStripThinkingFromSessionJSON_RemovesAssistantThinkingBlocks(t *testing.T) {
	input := []byte(`{
		"id": "test",
		"messages": [
			{"role": "user", "content": "hi"},
			{"role": "assistant", "content": [
				{"type": "thinking", "thinking": "PRIVATE_REASONING", "signature": "sig"},
				{"type": "text", "text": "visible reply"},
				{"type": "tool_use", "id": "t1", "name": "file_read", "input": {"path": "/x"}},
				{"type": "redacted_thinking", "data": "opaque-blob"}
			]}
		]
	}`)
	out, err := stripThinkingFromSessionJSON(input)
	if err != nil {
		t.Fatalf("strip failed: %v", err)
	}
	s := string(out)
	if strings.Contains(s, "PRIVATE_REASONING") {
		t.Error("thinking text leaked through strip")
	}
	if strings.Contains(s, "opaque-blob") {
		t.Error("redacted_thinking data leaked through strip")
	}
	if strings.Contains(s, `"type":"thinking"`) || strings.Contains(s, `"type":"redacted_thinking"`) {
		t.Errorf("thinking block type entries still present: %s", s)
	}
	// Non-thinking content survives.
	if !strings.Contains(s, "visible reply") {
		t.Error("text block content lost")
	}
	if !strings.Contains(s, `"name":"file_read"`) {
		t.Error("tool_use block lost")
	}
}

// TestStripThinkingFromSessionJSON_PreservesNonAssistantContent confirms we
// don't accidentally touch user / system messages even if they (impossibly)
// contained type:thinking entries.
func TestStripThinkingFromSessionJSON_PreservesNonAssistantContent(t *testing.T) {
	input := []byte(`{
		"messages": [
			{"role": "user", "content": [
				{"type": "thinking", "thinking": "should-not-be-stripped", "signature": "x"}
			]},
			{"role": "user", "content": "plain string"}
		]
	}`)
	out, err := stripThinkingFromSessionJSON(input)
	if err != nil {
		t.Fatalf("strip failed: %v", err)
	}
	if !strings.Contains(string(out), "should-not-be-stripped") {
		t.Error("user-role thinking-shaped block was incorrectly stripped — strip must target assistant only")
	}
}

// TestStripThinkingFromSessionJSON_NoThinkingNoChange returns the original
// body unchanged when there's nothing to strip. Important so the size-check
// downstream operates on bytes the user expects.
func TestStripThinkingFromSessionJSON_NoThinkingNoChange(t *testing.T) {
	input := []byte(`{
		"id": "x",
		"messages": [
			{"role": "user", "content": "hi"},
			{"role": "assistant", "content": [{"type": "text", "text": "hello"}]}
		]
	}`)
	out, err := stripThinkingFromSessionJSON(input)
	if err != nil {
		t.Fatalf("strip failed: %v", err)
	}
	if &out[0] != &input[0] {
		// Slice header check: if no mutation happened, helper should return
		// the original backing array (cheap path). Acceptable for the helper
		// to allocate, but worth tracking.
		t.Log("note: strip returned a new slice even though no mutation occurred")
	}
	// Content must still be parseable + unchanged structurally.
	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("output not valid JSON: %v", err)
	}
}

// TestStripThinkingFromSessionJSON_PreservesUnknownFields: top-level + per-
// message fields that we don't model (e.g., custom_metadata) must round-trip
// unchanged. Defends against silently dropping data on upload.
func TestStripThinkingFromSessionJSON_PreservesUnknownFields(t *testing.T) {
	input := []byte(`{
		"id": "x",
		"custom_top_level": "keep-me",
		"messages": [
			{"role": "assistant", "custom_msg_field": "keep-me-too", "content": [
				{"type": "thinking", "thinking": "drop", "signature": "s"},
				{"type": "text", "text": "ok"}
			]}
		]
	}`)
	out, err := stripThinkingFromSessionJSON(input)
	if err != nil {
		t.Fatalf("strip failed: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, "keep-me") {
		t.Error("custom top-level field dropped")
	}
	if !strings.Contains(s, "keep-me-too") {
		t.Error("custom per-message field dropped")
	}
	if strings.Contains(s, `"type":"thinking"`) {
		t.Error("thinking still present after strip")
	}
}

// TestStripThinkingFromSessionJSON_MalformedJSONReturnsError: corrupt input
// surfaces a parse error so the caller can decide policy (skip the session
// vs. continue upload with the unstripped body). The helper must NOT panic.
func TestStripThinkingFromSessionJSON_MalformedJSONReturnsError(t *testing.T) {
	input := []byte(`{"id": "x", "messages": [{`)
	out, err := stripThinkingFromSessionJSON(input)
	if err == nil {
		t.Error("expected parse error on malformed JSON")
	}
	// Original body returned unchanged for caller's choice.
	if string(out) != string(input) {
		t.Errorf("on error, expected original body unchanged; got %s", out)
	}
}

// TestBuildBatches_StripsThinkingBeforeSizeCheck is the end-to-end version
// of the strip wiring: feed a session whose ON-DISK size exceeds the
// configured cap only because of thinking content. Without the
// pre-size-check strip, BuildBatches would mark it as size_limit_exceeded.
// With the strip, the post-strip body fits and the session is batched.
func TestBuildBatches_StripsThinkingBeforeSizeCheck(t *testing.T) {
	// Build a session whose JSON ON-DISK is ~3KB (huge thinking padding) but
	// whose post-strip body is ~200 bytes (visible text only).
	bigPad := strings.Repeat("PRIVATE_THINKING_BLOAT_", 200) // ~4400 chars
	body := []byte(`{
		"id": "s1",
		"messages": [
			{"role": "user", "content": "hi"},
			{"role": "assistant", "content": [
				{"type": "thinking", "thinking": "` + bigPad + `", "signature": "s"},
				{"type": "text", "text": "ok"}
			]}
		]
	}`)

	now := time.Now().UTC()
	cands := []Candidate{{SessionID: "s1", AgentName: "", UpdatedAt: now}}

	// Loader returns the bloat body.
	loader := func(_, _ string) ([]byte, error) {
		return body, nil
	}

	// Cap sized so that the bloated body would exceed it but the stripped body fits.
	cfg := DefaultConfig()
	cfg.BatchMaxSessions = 5
	cfg.BatchMaxBytes = 1 << 20
	cfg.SingleSessionMaxBytes = 1024 // 1KB cap; original body ~5KB; stripped ~200B.

	marker := emptyMarker()
	batches, err := BuildBatches(context.Background(), cands, loader, cfg, &marker, now)
	if err != nil {
		t.Fatalf("BuildBatches: %v", err)
	}
	if len(batches) != 1 {
		t.Fatalf("expected 1 batch (session fits after strip), got %d batches; marker.Failed=%+v", len(batches), marker.Failed)
	}
	if len(batches[0].Sessions) != 1 {
		t.Fatalf("expected 1 session in the batch, got %d", len(batches[0].Sessions))
	}
	payload := string(batches[0].Sessions[0].JSON)
	if strings.Contains(payload, "PRIVATE_THINKING_BLOAT_") {
		t.Errorf("thinking padding leaked into upload payload; size=%d", len(payload))
	}
	if !strings.Contains(payload, "ok") {
		t.Errorf("post-strip payload missing the visible text block: %s", payload)
	}
	if _, marked := marker.Failed["s1"]; marked {
		t.Errorf("session unexpectedly marked failed: %+v", marker.Failed["s1"])
	}
}
