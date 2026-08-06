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

func TestStripThinkingFromSessionJSON_RemovesCheckpointThinkingBlocks(t *testing.T) {
	input := []byte(`{
		"messages": [{"role":"assistant","content":[{"type":"text","text":"archive"}]}],
		"compaction_checkpoint": {
			"schema_version": 1,
			"archive_through_index": 1,
			"messages": [{"role":"assistant","content":[
				{"type":"thinking","thinking":"PRIVATE_CHECKPOINT_REASONING","signature":"sig"},
				{"type":"text","text":"checkpoint reply"},
				{"type":"redacted_thinking","data":"PRIVATE_CHECKPOINT_BLOB"}
			]}]
		}
	}`)
	out, err := stripThinkingFromSessionJSON(input)
	if err != nil {
		t.Fatalf("strip failed: %v", err)
	}
	s := string(out)
	if strings.Contains(s, "PRIVATE_CHECKPOINT_REASONING") || strings.Contains(s, "PRIVATE_CHECKPOINT_BLOB") {
		t.Fatalf("checkpoint thinking leaked through sync strip: %s", s)
	}
	// The checkpoint is dropped wholesale rather than thinking-stripped, so
	// "checkpoint reply" goes with it — it is a duplicate of archive content
	// the cloud resume path does not read. Only the archive must survive.
	if strings.Contains(s, "compaction_checkpoint") {
		t.Fatalf("checkpoint survived the upload strip: %s", s)
	}
	if !strings.Contains(s, "archive") {
		t.Fatalf("lossless archive content was lost: %s", s)
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

// BuildBatches owns the externally visible size gate, so pin the checkpoint
// removal there as well as in the JSON helper. A derived checkpoint can push an
// otherwise-uploadable archive over SingleSessionMaxBytes; checking size first
// would mark the session permanently failed even though the cloud never reads
// the duplicate checkpoint.
func TestBuildBatches_DropsCompactionCheckpointBeforeSizeCheck(t *testing.T) {
	checkpointPad := strings.Repeat("DUPLICATE_CHECKPOINT_BLOAT_", 200)
	body := []byte(`{
		"id": "s1",
		"messages": [
			{"role": "user", "content": "hi"},
			{"role": "assistant", "content": "archive reply"}
		],
		"compaction_checkpoint": {
			"schema_version": 1,
			"archive_through_index": 2,
			"messages": [
				{"role": "user", "content": "` + checkpointPad + `"}
			]
		}
	}`)

	now := time.Now().UTC()
	cands := []Candidate{{SessionID: "s1", UpdatedAt: now}}
	loader := func(_, _ string) ([]byte, error) { return body, nil }
	cfg := DefaultConfig()
	cfg.BatchMaxSessions = 5
	cfg.BatchMaxBytes = 1 << 20
	cfg.SingleSessionMaxBytes = 1024
	marker := emptyMarker()

	batches, err := BuildBatches(context.Background(), cands, loader, cfg, &marker, now)
	if err != nil {
		t.Fatalf("BuildBatches: %v", err)
	}
	if len(batches) != 1 || len(batches[0].Sessions) != 1 {
		t.Fatalf("archive should fit after checkpoint removal: batches=%d failed=%+v", len(batches), marker.Failed)
	}
	payload := string(batches[0].Sessions[0].JSON)
	if strings.Contains(payload, "compaction_checkpoint") || strings.Contains(payload, "DUPLICATE_CHECKPOINT_BLOAT_") {
		t.Fatalf("derived checkpoint survived batch construction: %s", payload)
	}
	if !strings.Contains(payload, "archive reply") {
		t.Fatalf("lossless archive was damaged: %s", payload)
	}
	if _, failed := marker.Failed["s1"]; failed {
		t.Fatalf("session was permanently size-failed before checkpoint removal: %+v", marker.Failed["s1"])
	}
}

// The compaction checkpoint is derived state — a compacted view of messages
// already present in this payload. Uploading it roughly doubles a compacted
// session's bytes, and the size gate downstream marks an oversize session
// `size_limit_exceeded`, which backoff.go treats as PERMANENT. So it must be
// dropped, not merely thinking-stripped.
func TestStripThinkingFromSessionJSON_DropsCompactionCheckpoint(t *testing.T) {
	body := []byte(`{
	  "id": "s1",
	  "messages": [
	    {"role": "user", "content": "hi"},
	    {"role": "assistant", "content": [{"type": "text", "text": "hello"}]}
	  ],
	  "compaction_checkpoint": {
	    "schema_version": 1,
	    "archive_through_index": 2,
	    "messages": [
	      {"role": "user", "content": "Previous context summary: ..."},
	      {"role": "assistant", "content": [{"type": "text", "text": "hello"}]}
	    ]
	  }
	}`)

	out, err := stripThinkingFromSessionJSON(body)
	if err != nil {
		t.Fatalf("strip: %v", err)
	}
	if len(out) >= len(body) {
		t.Errorf("payload did not shrink: %d bytes in, %d out", len(body), len(out))
	}

	var top map[string]any
	if err := json.Unmarshal(out, &top); err != nil {
		t.Fatalf("result is not valid JSON: %v", err)
	}
	if _, present := top["compaction_checkpoint"]; present {
		t.Error("compaction_checkpoint survived the upload strip")
	}
	msgs, ok := top["messages"].([]any)
	if !ok || len(msgs) != 2 {
		t.Fatalf("lossless transcript was damaged: %v", top["messages"])
	}
	if top["id"] != "s1" {
		t.Errorf("unrelated field lost: %v", top["id"])
	}
}

// Dropping the checkpoint forces a re-marshal on sessions that carry no
// thinking blocks at all — a path that previously returned the original bytes
// untouched. Decoding into map[string]any turns JSON numbers into float64,
// whose 53-bit mantissa truncates the integers real tool inputs carry (unix
// nanosecond timestamps, 64-bit row IDs). Same hazard normalizeToolInput
// guards against; the fix is json.Decoder.UseNumber().
func TestStripThinkingFromSessionJSON_PreservesLargeIntegers(t *testing.T) {
	const nanoTS = "1785920867066123456" // 19 digits, well past 2^53
	const rowID = "9007199254740993"     // 2^53 + 1
	body := []byte(`{
	  "messages": [
	    {"role": "assistant", "content": [
	      {"type": "tool_use", "id": "t1", "name": "q",
	       "input": {"since": ` + nanoTS + `, "row": ` + rowID + `}}
	    ]}
	  ],
	  "compaction_checkpoint": {"schema_version": 1, "archive_through_index": 1, "messages": []}
	}`)

	out, err := stripThinkingFromSessionJSON(body)
	if err != nil {
		t.Fatalf("strip: %v", err)
	}
	if !strings.Contains(string(out), nanoTS) {
		t.Errorf("nanosecond timestamp was truncated by the float64 roundtrip: %s", out)
	}
	if !strings.Contains(string(out), rowID) {
		t.Errorf("64-bit row id was truncated by the float64 roundtrip: %s", out)
	}
}

// A streaming Decoder stops at the first complete JSON value. Without an
// explicit EOF check it would accept a concatenated/corrupt session file,
// re-marshal only the leading object, and silently discard the rest — a
// regression against json.Unmarshal, which rejects trailing data outright.
func TestStripThinkingFromSessionJSON_RejectsTrailingJSON(t *testing.T) {
	body := []byte(`{"messages":[],"compaction_checkpoint":{"schema_version":1}} {"unexpected":"tail"}`)

	out, err := stripThinkingFromSessionJSON(body)
	if err == nil {
		t.Fatalf("expected trailing JSON to be rejected, got %s", out)
	}
	if string(out) != string(body) {
		t.Errorf("original bytes must be returned untouched on error; got %s", out)
	}
}
