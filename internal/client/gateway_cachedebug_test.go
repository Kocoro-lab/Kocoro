package client

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCacheDebug_DefaultOff(t *testing.T) {
	os.Unsetenv("SHANNON_CACHE_DEBUG")
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	req := CompletionRequest{Messages: []Message{{Role: "user", Content: NewTextContent("hi")}}}
	_ = logCacheDebug(req, "complete")

	path := filepath.Join(tmp, ".shannon", "logs", "cache-debug.log")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expected no cache-debug.log without env flag, got err=%v", err)
	}
}

func TestCacheDebug_EnabledByEnv(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("SHANNON_CACHE_DEBUG", "1")

	req := CompletionRequest{Messages: []Message{{Role: "user", Content: NewTextContent("hi")}}}
	reqID := logCacheDebug(req, "complete")
	if reqID == "" {
		t.Fatalf("expected non-empty reqID")
	}
	path := filepath.Join(tmp, ".shannon", "logs", "cache-debug.log")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected cache-debug.log to be written, got err=%v", err)
	}
}

func TestCacheDebug_WritesSessionID(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("SHANNON_CACHE_DEBUG", "1")

	req := CompletionRequest{
		Messages:  []Message{{Role: "user", Content: NewTextContent("hi")}},
		SessionID: "sess-abc123",
	}
	reqID := logCacheDebug(req, "complete")
	if reqID == "" {
		t.Fatal("expected non-empty reqID")
	}

	data, err := os.ReadFile(filepath.Join(tmp, ".shannon", "logs", "cache-debug.log"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"session_id":"sess-abc123"`) {
		t.Fatalf("session_id not written in log entry: %s", data)
	}
}

func TestCacheDebug_ResponseCarriesSessionID(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("SHANNON_CACHE_DEBUG", "1")

	resp := &CompletionResponse{
		RequestID: "gw-req-1",
		Usage:     Usage{InputTokens: 10, CacheReadTokens: 5, CacheCreationTokens: 0},
	}
	logCacheResponse("abc123", "sess-xyz789", resp)

	data, _ := os.ReadFile(filepath.Join(tmp, ".shannon", "logs", "cache-debug.log"))
	if !strings.Contains(string(data), `"session_id":"sess-xyz789"`) {
		t.Fatalf("session_id missing from resp entry: %s", data)
	}
}

// TestCacheDebug_ForceTTLEnvCaptured guards visibility of SHANNON_FORCE_TTL.
// Without this field, after-the-fact analysis cannot tell whether a TTL
// override was active during the run or whether the gateway used its default
// operator TTL override logging; cache_source itself is attribution.
func TestCacheDebug_ForceTTLEnvCaptured(t *testing.T) {
	cases := []struct {
		val      string
		wantSubs string
	}{
		{"5m", `"force_ttl":"5m"`},
		{"1h", `"force_ttl":"1h"`},
		{"off", `"force_ttl":"off"`},
	}
	for _, tc := range cases {
		t.Run(tc.val, func(t *testing.T) {
			tmp := t.TempDir()
			t.Setenv("HOME", tmp)
			t.Setenv("SHANNON_CACHE_DEBUG", "1")
			t.Setenv("SHANNON_FORCE_TTL", tc.val)

			req := CompletionRequest{Messages: []Message{{Role: "user", Content: NewTextContent("hi")}}}
			if id := logCacheDebug(req, "complete"); id == "" {
				t.Fatal("expected reqID")
			}
			data, _ := os.ReadFile(filepath.Join(tmp, ".shannon", "logs", "cache-debug.log"))
			if !strings.Contains(string(data), tc.wantSubs) {
				t.Fatalf("missing %s in log entry: %s", tc.wantSubs, data)
			}
		})
	}
}

// TestLogCacheCompactEvent_DefaultOff confirms compact events are gated by
// SHANNON_CACHE_DEBUG just like the req/resp lines.
func TestLogCacheCompactEvent_DefaultOff(t *testing.T) {
	os.Unsetenv("SHANNON_CACHE_DEBUG")
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	old := NewBlockContent([]ContentBlock{NewToolResultBlock("t1", "raw", false)})
	new_ := NewBlockContent([]ContentBlock{NewToolResultBlock("t1", "summary", false)})
	LogCacheCompactEvent("tier1", 5, old, new_)

	path := filepath.Join(tmp, ".shannon", "logs", "cache-debug.log")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expected no log without env: %v", err)
	}
}

// TestLogCacheCompactEvent_EmitsHashAndAction is the core observability
// guarantee: when an in-place rewrite changes a tool_result block's wire
// bytes, the analyst sees (action, msg_idx, old_hash, new_hash) — exactly
// the four fields needed to map a compaction event to the next req's
// msg_hashes ladder divergence.
func TestLogCacheCompactEvent_EmitsHashAndAction(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("SHANNON_CACHE_DEBUG", "1")

	old := NewBlockContent([]ContentBlock{NewToolResultBlock("t1", "lots of raw output bytes here", false)})
	new_ := NewBlockContent([]ContentBlock{NewToolResultBlock("t1", "[micro-compact] summary", false)})
	LogCacheCompactEvent("tier2", 7, old, new_)

	data, _ := os.ReadFile(filepath.Join(tmp, ".shannon", "logs", "cache-debug.log"))
	var entry map[string]any
	if err := json.Unmarshal(data, &entry); err != nil {
		t.Fatalf("parse log: %v\nraw=%s", err, data)
	}
	if entry["dir"] != "compact" {
		t.Errorf("dir=%v want compact", entry["dir"])
	}
	if entry["action"] != "tier2" {
		t.Errorf("action=%v want tier2", entry["action"])
	}
	if v, _ := entry["msg_idx"].(float64); int(v) != 7 {
		t.Errorf("msg_idx=%v want 7", entry["msg_idx"])
	}
	oh, _ := entry["old_hash"].(string)
	nh, _ := entry["new_hash"].(string)
	if len(oh) != 12 || len(nh) != 12 {
		t.Errorf("hash lengths: old=%q new=%q want 12 each", oh, nh)
	}
	if oh == nh {
		t.Errorf("old_hash should differ from new_hash on real rewrite")
	}
}

// TestLogCacheCompactEvent_SkipsNoOpRewrite avoids polluting the log when a
// "compaction" pass leaves the bytes unchanged (idempotent re-visit, e.g.
// CompressedTier already set, which the cache-idempotence fix relies on).
func TestLogCacheCompactEvent_SkipsNoOpRewrite(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("SHANNON_CACHE_DEBUG", "1")

	same := NewBlockContent([]ContentBlock{NewToolResultBlock("t1", "[micro-compact] frozen", false)})
	LogCacheCompactEvent("tier1", 3, same, same)

	path := filepath.Join(tmp, ".shannon", "logs", "cache-debug.log")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		data, _ := os.ReadFile(path)
		t.Fatalf("expected no log entry for no-op rewrite, got: %s", data)
	}
}

// TestCacheDebug_RawDumpRotationEvictsOldest caps the raw dump dir to N
// entries via SHANNON_CACHE_DEBUG_RAW_MAX. A long session writing one dir
// per LLM call previously filled disk indefinitely (one repro = 31 dirs;
// production sessions can produce 1000+). This test pushes 5 dumps with
// cap=3 and asserts only the 3 most recent survive.
func TestCacheDebug_RawDumpRotationEvictsOldest(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("SHANNON_CACHE_DEBUG", "1")
	t.Setenv("SHANNON_CACHE_DEBUG_RAW", "1")
	t.Setenv("SHANNON_CACHE_DEBUG_RAW_MAX", "3")

	rawDir := filepath.Join(tmp, ".shannon", "logs", "cache-debug-raw")
	for range 5 {
		req := CompletionRequest{Messages: []Message{{Role: "user", Content: NewTextContent("msg")}}}
		if id := logCacheDebug(req, "complete"); id == "" {
			t.Fatal("expected reqID")
		}
		// Ensure deterministic mtime ordering on coarse filesystems.
		time.Sleep(20 * time.Millisecond)
	}

	entries, err := os.ReadDir(rawDir)
	if err != nil {
		t.Fatalf("read raw dir: %v", err)
	}
	if len(entries) != 3 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("expected 3 dirs after rotation with cap=3, got %d: %v", len(entries), names)
	}
}

// TestCacheDebug_RawDumpRotationKeepsAllUnderCap confirms dumps below the
// cap are untouched.
func TestCacheDebug_RawDumpRotationKeepsAllUnderCap(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("SHANNON_CACHE_DEBUG", "1")
	t.Setenv("SHANNON_CACHE_DEBUG_RAW", "1")
	t.Setenv("SHANNON_CACHE_DEBUG_RAW_MAX", "10")

	for range 4 {
		req := CompletionRequest{Messages: []Message{{Role: "user", Content: NewTextContent("msg")}}}
		_ = logCacheDebug(req, "complete")
	}
	rawDir := filepath.Join(tmp, ".shannon", "logs", "cache-debug-raw")
	entries, _ := os.ReadDir(rawDir)
	if len(entries) != 4 {
		t.Fatalf("expected 4 dirs (under cap), got %d", len(entries))
	}
}

// TestCacheDebug_PerBlockHashesEmittedForBlockMessages exposes structure
// inside multi-block user messages so the analyst can pinpoint *which*
// tool_result block in a message drifted, not just that the message did.
// Without this, when compressOldToolResults rewrites one of N tool_result
// blocks inside a single user turn, the rolled-up msg hash flips with no
// indication of which block changed.
func TestCacheDebug_PerBlockHashesEmittedForBlockMessages(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("SHANNON_CACHE_DEBUG", "1")

	// One assistant message carrying 2 tool_use, then one user message
	// carrying 2 tool_result blocks (the typical bash + glob batch shape).
	req := CompletionRequest{
		Messages: []Message{
			{Role: "user", Content: NewBlockContent([]ContentBlock{
				NewToolResultBlock("tu1", "ok one", false),
				NewToolResultBlock("tu2", "ok two", false),
			})},
		},
	}
	if id := logCacheDebug(req, "complete"); id == "" {
		t.Fatal("expected reqID")
	}
	data, _ := os.ReadFile(filepath.Join(tmp, ".shannon", "logs", "cache-debug.log"))
	var entry map[string]any
	if err := json.Unmarshal(data, &entry); err != nil {
		t.Fatalf("parse log: %v\nraw=%s", err, data)
	}
	mh, ok := entry["msg_hashes"].([]any)
	if !ok || len(mh) != 1 {
		t.Fatalf("expected 1 msg_hashes entry: %v", entry["msg_hashes"])
	}
	m0, _ := mh[0].(map[string]any)
	blocks, ok := m0["blocks"].([]any)
	if !ok {
		t.Fatalf("expected blocks array on user msg with structured content: %v", m0)
	}
	if len(blocks) != 2 {
		t.Fatalf("expected 2 blocks, got %d: %v", len(blocks), blocks)
	}
	for i, b := range blocks {
		bm, _ := b.(map[string]any)
		if bm["type"] != "tool_result" {
			t.Errorf("block %d: type=%v want tool_result", i, bm["type"])
		}
		if h, _ := bm["hash"].(string); len(h) != 12 {
			t.Errorf("block %d: hash=%q want 12 hex chars", i, h)
		}
	}
}

// TestCacheDebug_PerBlockHashCapturesTier verifies that the per-block ladder
// surfaces CompressedTier so analysts can read drift × tier together (e.g.
// distinguish a fresh write from a re-write of an already-compacted block).
func TestCacheDebug_PerBlockHashCapturesTier(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("SHANNON_CACHE_DEBUG", "1")

	tier2 := NewToolResultBlock("tu1", "[micro-compact] summary", false)
	tier2.CompressedTier = 2
	plain := NewToolResultBlock("tu2", "raw", false)

	req := CompletionRequest{Messages: []Message{
		{Role: "user", Content: NewBlockContent([]ContentBlock{tier2, plain})},
	}}
	_ = logCacheDebug(req, "complete")
	data, _ := os.ReadFile(filepath.Join(tmp, ".shannon", "logs", "cache-debug.log"))
	var entry map[string]any
	_ = json.Unmarshal(data, &entry)
	mh, _ := entry["msg_hashes"].([]any)
	m0, _ := mh[0].(map[string]any)
	blocks, _ := m0["blocks"].([]any)
	if len(blocks) != 2 {
		t.Fatalf("expected 2 blocks, got %d", len(blocks))
	}
	b0, _ := blocks[0].(map[string]any)
	if v, _ := b0["tier"].(float64); int(v) != 2 {
		t.Errorf("block 0 tier=%v want 2", b0["tier"])
	}
	b1, _ := blocks[1].(map[string]any)
	// tier 0 (uncompressed) must be omitted to keep logs compact — the field
	// only matters when non-zero.
	if _, present := b1["tier"]; present {
		t.Errorf("block 1 tier should be omitted (tier=0), got %v", b1["tier"])
	}
}

// TestCacheDebug_PerBlockHashOmittedForTextOnly confirms text-only messages
// (the common assistant-text and first-user-prompt case) don't get an empty
// blocks array — keeps logs lean.
func TestCacheDebug_PerBlockHashOmittedForTextOnly(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("SHANNON_CACHE_DEBUG", "1")

	req := CompletionRequest{Messages: []Message{
		{Role: "user", Content: NewTextContent("plain text prompt")},
	}}
	_ = logCacheDebug(req, "complete")
	data, _ := os.ReadFile(filepath.Join(tmp, ".shannon", "logs", "cache-debug.log"))
	var entry map[string]any
	_ = json.Unmarshal(data, &entry)
	mh, _ := entry["msg_hashes"].([]any)
	m0, _ := mh[0].(map[string]any)
	if _, present := m0["blocks"]; present {
		t.Errorf("text-only message must not carry blocks field: %v", m0)
	}
}

// TestCacheDebug_ForceTTLOmittedWhenUnset confirms we don't pollute the log
// with empty force_ttl in normal operation.
func TestCacheDebug_ForceTTLOmittedWhenUnset(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("SHANNON_CACHE_DEBUG", "1")
	os.Unsetenv("SHANNON_FORCE_TTL")

	req := CompletionRequest{Messages: []Message{{Role: "user", Content: NewTextContent("hi")}}}
	_ = logCacheDebug(req, "complete")
	data, _ := os.ReadFile(filepath.Join(tmp, ".shannon", "logs", "cache-debug.log"))
	if strings.Contains(string(data), `"force_ttl"`) {
		t.Fatalf("force_ttl should be absent when env unset: %s", data)
	}
}
