package client

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// cacheDebugLogPath returns the absolute path of the cache-debug log file,
// or "" when the home dir is unavailable.
func cacheDebugLogPath() string {
	home, _ := os.UserHomeDir()
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".shannon", "logs", "cache-debug.log")
}

const cacheDebugMaxBytes = 10 * 1024 * 1024 // 10 MB

var cacheDebugMu sync.Mutex

func appendCacheDebug(entry map[string]any) {
	path := cacheDebugLogPath()
	if path == "" {
		return
	}
	// Ensure parent dir exists; silent on failure (never block request).
	// 0700/0600 matches dumpRawForDebug and audit.go — even though this log
	// holds only hashes/lengths (no message bytes), keeping the same posture
	// avoids accidental loosening on multi-user machines.
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return
	}
	data, _ := json.Marshal(entry)

	cacheDebugMu.Lock()
	defer cacheDebugMu.Unlock()

	// Simple single-file rotation: if the log exceeds the cap, truncate it
	// to the most recent half. Best-effort — never block the request path.
	if info, err := os.Stat(path); err == nil && info.Size() > cacheDebugMaxBytes {
		_ = rotateCacheDebugLog(path)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(data, '\n'))
}

// rotateCacheDebugLog keeps the last half of the log file. Best-effort, no
// error propagation — a failed rotation just means the file stays large until
// the next append retries.
func rotateCacheDebugLog(path string) error {
	content, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	// Keep the second half (most recent entries). Find the first newline
	// after the midpoint so we don't split a JSON line.
	mid := len(content) / 2
	idx := bytes.IndexByte(content[mid:], '\n')
	if idx < 0 {
		return nil // single giant line — don't touch
	}
	return os.WriteFile(path, content[mid+idx+1:], 0600)
}

// logCacheDebug appends a "dir":"req" JSON line containing content hashes
// for the system / tools / first-user / last-user parts of the outgoing
// request, plus a freshly generated req_id. The returned req_id is passed
// to logCacheResponse so the response's cache_creation / cache_read tokens
// can be joined to the same request line. Silent on any error; never
// affects the request.
//
// Two diagnostic depths:
//   - SHANNON_CACHE_DEBUG=1    → JSON line with rollup hashes + per-tool +
//     per-message hash ladders (cheap, default for triage).
//   - SHANNON_CACHE_DEBUG_RAW=1 → in addition, full tools[]/messages[]/system
//     bytes are dumped to ~/.shannon/logs/cache-debug-raw/<req_id>/ so two
//     adjacent requests can be byte-diffed (heavy, opt-in).
func logCacheDebug(req CompletionRequest, tag string) string {
	if os.Getenv("SHANNON_CACHE_DEBUG") != "1" {
		return ""
	}
	h := func(b []byte) string {
		sum := sha256.Sum256(b)
		return hex.EncodeToString(sum[:6])
	}
	var systemBytes, firstUserBytes, lastUserBytes []byte

	// Per-message hash ladder. Hash the WHOLE marshalled message (role+content
	// +name+tool_call_id) — this is what the wire body actually contains, and
	// what Anthropic's prefix matcher sees. Tracking position-by-position lets
	// us spot middle-of-history byte drift (e.g. Tier compression rewriting an
	// older tool_result) that the rollup first_user_h / last_user_h hashes hide.
	//
	// For messages with structured content (multi-block user turns containing
	// tool_result blocks), additionally emit a per-block ladder so the analyst
	// can pinpoint which block inside a multi-block message drifted — the
	// minimum unit compressOldToolResults / micro-compact actually mutate.
	msgHashes := make([]map[string]any, 0, len(req.Messages))
	for i, m := range req.Messages {
		mb, _ := json.Marshal(m)
		entry := map[string]any{
			"i":    i,
			"role": m.Role,
			"hash": h(mb),
			"len":  len(mb),
		}
		if m.Content.HasBlocks() {
			blocks := m.Content.Blocks()
			ladder := make([]map[string]any, 0, len(blocks))
			for _, b := range blocks {
				bb, _ := json.Marshal(b)
				bEntry := map[string]any{
					"type": b.Type,
					"hash": h(bb),
					"len":  len(bb),
				}
				if b.CompressedTier != 0 {
					bEntry["tier"] = b.CompressedTier
				}
				ladder = append(ladder, bEntry)
			}
			entry["blocks"] = ladder
		}
		msgHashes = append(msgHashes, entry)
		cb, _ := json.Marshal(m.Content)
		switch m.Role {
		case "system":
			systemBytes = cb
		case "user":
			if firstUserBytes == nil {
				firstUserBytes = cb
			}
			lastUserBytes = cb
		}
	}

	// Per-tool hash list. Anthropic's prefix matcher hashes the tools[] array
	// in source order; if any single tool's marshalled bytes drift (e.g.
	// defer_loading flag flips after tool_search warms it), we need to know
	// WHICH tool changed, not just the rolled-up tools_h.
	toolHashes := make([]map[string]any, 0, len(req.Tools))
	for _, t := range req.Tools {
		tb, _ := json.Marshal(t)
		name := t.Name
		if name == "" {
			name = t.Function.Name
		}
		entry := map[string]any{
			"name": name,
			"hash": h(tb),
			"len":  len(tb),
		}
		if t.DeferLoading {
			entry["defer"] = true
		}
		toolHashes = append(toolHashes, entry)
	}

	toolsJSON, _ := json.Marshal(req.Tools)
	var idBuf [6]byte
	_, _ = rand.Read(idBuf[:])
	reqID := hex.EncodeToString(idBuf[:])
	entry := map[string]any{
		"ts":             time.Now().Format(time.RFC3339Nano),
		"dir":            "req",
		"req_id":         reqID,
		"session_id":     req.SessionID,
		"cache_source":   req.CacheSource,
		"tag":            tag,
		"msgs":           len(req.Messages),
		"model":          req.SpecificModel + "/" + req.ModelTier,
		"system_h":       h(systemBytes),
		"system_len":     len(systemBytes),
		"tools_h":        h(toolsJSON),
		"tools_count":    len(req.Tools),
		"first_user_h":   h(firstUserBytes),
		"first_user_len": len(firstUserBytes),
		"last_user_h":    h(lastUserBytes),
		"last_user_len":  len(lastUserBytes),
		"tool_hashes":    toolHashes,
		"msg_hashes":     msgHashes,
	}
	// SHANNON_FORCE_TTL surfaces operator overrides ({off,5m,1h}). Absent →
	// gateway uses cache_source → TTL routing (see docs/cache-strategy.md).
	// Logged only when set so normal-operation logs stay tidy.
	if v := os.Getenv("SHANNON_FORCE_TTL"); v != "" {
		entry["force_ttl"] = v
	}
	appendCacheDebug(entry)

	if os.Getenv("SHANNON_CACHE_DEBUG_RAW") == "1" {
		dumpRawForDebug(reqID, req)
	}
	return reqID
}

// dumpRawForDebug writes the request's tools[], messages[] and system content
// as pretty-printed JSON files keyed by req_id under
// ~/.shannon/logs/cache-debug-raw/<req_id>/. Caller verifies the env flag.
// All errors silent — diagnostic dump must never affect the request path.
//
// After writing, the parent directory is rotated to keep only the N most
// recent dumps (LRU by ModTime). N defaults to 100; override via
// SHANNON_CACHE_DEBUG_RAW_MAX. Long sessions previously accumulated 1000+
// dirs uncapped.
func dumpRawForDebug(reqID string, req CompletionRequest) {
	home, err := os.UserHomeDir()
	if home == "" || err != nil {
		return
	}
	parent := filepath.Join(home, ".shannon", "logs", "cache-debug-raw")
	dir := filepath.Join(parent, reqID)
	// 0700/0600: dumps contain the full LLM request (user messages, tool inputs,
	// and any secrets injected into bash results). Restrict to the owning user.
	if err := os.MkdirAll(dir, 0700); err != nil {
		return
	}
	if b, err := json.MarshalIndent(req.Tools, "", "  "); err == nil {
		_ = os.WriteFile(filepath.Join(dir, "tools.json"), b, 0600)
	}
	if b, err := json.MarshalIndent(req.Messages, "", "  "); err == nil {
		_ = os.WriteFile(filepath.Join(dir, "messages.json"), b, 0600)
	}
	// Pull system content out separately for quick inspection.
	for _, m := range req.Messages {
		if m.Role == "system" {
			if b, err := json.MarshalIndent(m.Content, "", "  "); err == nil {
				_ = os.WriteFile(filepath.Join(dir, "system.json"), b, 0600)
			}
			break
		}
	}
	rotateRawDumpDir(parent, rawDumpMaxEntries())
}

const cacheDebugRawDefaultMax = 100

// rawDumpMaxEntries returns the configured cap. SHANNON_CACHE_DEBUG_RAW_MAX
// must parse as a positive int; anything else falls back to the default.
func rawDumpMaxEntries() int {
	v := strings.TrimSpace(os.Getenv("SHANNON_CACHE_DEBUG_RAW_MAX"))
	if v == "" {
		return cacheDebugRawDefaultMax
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return cacheDebugRawDefaultMax
	}
	return n
}

// rotateRawDumpDir keeps the N most recent immediate subdirectories of dir
// (sorted by ModTime descending) and RemoveAll's the rest. Best-effort —
// silent on any error so a transient FS hiccup never poisons the request
// path.
func rotateRawDumpDir(dir string, max int) {
	if max <= 0 {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	type rec struct {
		name string
		mod  time.Time
	}
	recs := make([]rec, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		recs = append(recs, rec{name: e.Name(), mod: info.ModTime()})
	}
	if len(recs) <= max {
		return
	}
	// Newest first, then trim the tail.
	sort.Slice(recs, func(i, j int) bool { return recs[i].mod.After(recs[j].mod) })
	for _, r := range recs[max:] {
		_ = os.RemoveAll(filepath.Join(dir, r.name))
	}
}

// LogCacheCompactEvent records that an in-place message-content rewrite
// occurred, changing the wire bytes of a position in the prefix. The agent
// loop's compaction passes (Tier 1 strip-to-metadata, Tier 2 micro-compact
// or head+tail truncation, time-based microcompact) all mutate
// `messages[idx].Content` and are the ONLY legitimate sources of mid-history
// byte drift. Without these events, a `system_h` or `msg_hashes[k]` flip in
// the next req log line gives no clue WHY the bytes changed.
//
// Joins to neighboring req/resp lines by timestamp ordering — events fire
// just before the next LLM call, so a `dir:"compact"` line followed by a
// `dir:"req"` line on the same session is the standard correlation.
//
// Action vocabulary (free-form, but use these for analyzability):
//   - "tier1"     — stripToMetadata (Anthropic native blocks)
//   - "tier1_xml" — XML-format truncation
//   - "tier2"     — head+tail truncation or micro-compact summary
//   - "tbcompact" — time-based microcompact (timebasedcompact.go)
//   - "tbclear"   — time-based result clear (replaces with sentinel)
//   - "budget"    — query-time tool_result budget replacement
//
// Silent when SHANNON_CACHE_DEBUG != "1". Skips no-op rewrites where bytes
// are unchanged (idempotent re-visit on already-compacted blocks).
func LogCacheCompactEvent(action string, msgIndex int, oldContent, newContent MessageContent) {
	if os.Getenv("SHANNON_CACHE_DEBUG") != "1" {
		return
	}
	ob, _ := json.Marshal(oldContent)
	nb, _ := json.Marshal(newContent)
	if bytes.Equal(ob, nb) {
		return
	}
	h := func(b []byte) string {
		sum := sha256.Sum256(b)
		return hex.EncodeToString(sum[:6])
	}
	appendCacheDebug(map[string]any{
		"ts":       time.Now().Format(time.RFC3339Nano),
		"dir":      "compact",
		"action":   action,
		"msg_idx":  msgIndex,
		"old_hash": h(ob),
		"new_hash": h(nb),
		"old_len":  len(ob),
		"new_len":  len(nb),
	})
}

// logCacheResponse appends a "dir":"resp" JSON line joined to the request
// via req_id, recording the cache_creation / cache_read / input / output
// token counts returned by the gateway. Silent on any error.
func logCacheResponse(reqID, sessionID string, resp *CompletionResponse) {
	if reqID == "" || resp == nil {
		return
	}
	if os.Getenv("SHANNON_CACHE_DEBUG") != "1" {
		return
	}
	appendCacheDebug(map[string]any{
		"ts":            time.Now().Format(time.RFC3339Nano),
		"dir":           "resp",
		"req_id":        reqID,
		"session_id":    sessionID,
		"gateway_reqid": resp.RequestID,
		"in":            resp.Usage.InputTokens,
		"out":           resp.Usage.OutputTokens,
		"cc":            resp.Usage.CacheCreationTokens,
		"cc_5m":         resp.Usage.CacheCreation5mTokens,
		"cc_1h":         resp.Usage.CacheCreation1hTokens,
		"cr":            resp.Usage.CacheReadTokens,
	})
}

// APIError represents an HTTP error from the LLM API with a status code.
type APIError struct {
	StatusCode int
	Body       string
}

func (e *APIError) Error() string {
	if e.Body != "" {
		return fmt.Sprintf("API returned %d: %s", e.StatusCode, e.Body)
	}
	return fmt.Sprintf("API returned %d", e.StatusCode)
}

// --- Public types (used by agent loop) ---

// ContentBlock represents a polymorphic content block.
// Supported types: "text", "image", "tool_use", "tool_result".
type ContentBlock struct {
	Type   string       `json:"type"`
	Text   string       `json:"text,omitempty"`
	Source *ImageSource `json:"source,omitempty"`
	// tool_use fields
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
	// tool_result fields
	ToolUseID   string `json:"tool_use_id,omitempty"`
	IsError     bool   `json:"is_error,omitempty"`
	ToolContent any    `json:"-"` // string or []ContentBlock; serialized as "content" for tool_result
	// CompressedTier records that an agent-loop compaction pass has visited
	// this tool_result block. Once non-zero, further passes leave ToolContent
	// alone so the wire bytes stay byte-stable across iterations — Anthropic's
	// prompt-cache prefix matcher diverges at the first changed byte. Values:
	//   0 = uncompressed (eligible for first pass)
	//   1 = stripped to Tier 1 metadata (the most aggressive form)
	//   2 = visited at Tier 2 (LLM micro-compact summary or head+tail truncation)
	// The numbering is "recency tier" (lower number = older message = more
	// compressed), not "compression order" — the loop sets Tier 2 first when
	// micro-compact runs, then Tier 1 strips deeper on later passes.
	// Never serialized; agent-internal state only.
	CompressedTier int `json:"-"`
	// ToolName is set when Type == "tool_reference" (deferred-tool expansion hint
	// returned by tool_search). Anthropic expands the full schema server-side
	// for deferred tools referenced by name. Only populated for tool_reference blocks.
	ToolName string `json:"tool_name,omitempty"`
}

// MarshalJSON handles the polymorphic "content" field for tool_result blocks
// and guarantees tool_use.input is always a concrete JSON object (never null
// or missing), which is required by Anthropic's schema validator. See issue #45.
func (cb ContentBlock) MarshalJSON() ([]byte, error) {
	type plain ContentBlock // avoid infinite recursion
	m := make(map[string]any)

	// Marshal the base fields via the plain type
	base, err := json.Marshal(plain(cb))
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(base, &m); err != nil {
		return nil, err
	}

	// Add ToolContent as "content" for tool_result blocks
	if cb.Type == "tool_result" && cb.ToolContent != nil {
		m["content"] = cb.ToolContent
	}

	// For tool_use blocks, force-write a concrete input object. The base
	// field has `omitempty` so a nil/empty RawMessage is dropped by the
	// initial marshal; we reinject a normalized value here so every tool_use
	// block always carries a valid `input` field on the wire.
	if cb.Type == "tool_use" {
		normalized := normalizeToolInput(cb.Input)
		var inputVal any
		if err := json.Unmarshal(normalized, &inputVal); err != nil {
			// Fallback: preserve raw bytes as a JSON Number/string via RawMessage.
			inputVal = normalized
		}
		m["input"] = inputVal
	}

	return json.Marshal(m)
}

// normalizeToolInput coerces a tool_use input RawMessage into a shape
// Anthropic's tool_use.input validator ("Input should be a valid
// dictionary") will accept when we can do so unambiguously.
//
// Three normalizations are applied:
//  1. null / empty / whitespace → "{}" (issue #45).
//  2. JSON-encoded string wrapping a JSON object → unwrap once. Some
//     providers (OpenAI-shaped Chat Completions adapters) return tool
//     arguments as a JSON string whose decoded value is itself a JSON
//     object, e.g. `"{\"command\":\"ls\"}"`. The tool executes fine via
//     FunctionCall.ArgumentsString, but the double-encoded bytes used to
//     be persisted verbatim in the assistant turn, causing the next call
//     to Anthropic to drop that turn with a 400 — which llm-service then
//     silently sanitized out, losing tool history from the model.
//  3. **Canonical key ordering** — if the input is a JSON object, roundtrip
//     through Unmarshal→Marshal so nested map keys are lexicographic. Without
//     this, two conceptually-identical tool_use calls can serialize with
//     different key orders on different goroutines → cross-turn prompt-cache
//     miss (observed in session 2026-04-15-69f601dc1c98: same system_len,
//     17 system_h variants → −13pp CHR). Roundtrip is a no-op for already-
//     canonical inputs, so it's safe to apply unconditionally.
//
// Non-object inputs (numbers, arrays, plain strings, bools) are passed
// through unchanged on purpose: those are provider bugs, and masking
// them with "{}" would hide the anomaly.
func normalizeToolInput(raw json.RawMessage) json.RawMessage {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return json.RawMessage("{}")
	}
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(trimmed, &s); err == nil {
			inner := bytes.TrimSpace([]byte(s))
			if len(inner) > 0 && inner[0] == '{' && json.Valid(inner) {
				trimmed = inner
			}
		}
	}
	// Canonicalize JSON objects so nested map-key ordering is deterministic.
	// encoding/json sorts map keys since Go 1.12 for top-level map[string]X,
	// but `any`-wrapped maps (which is what Unmarshal produces for object
	// inputs) also sort on re-marshal. This closes the door on producer-side
	// non-determinism (e.g. Go map iteration order in tool wrappers).
	//
	// Use Decoder.UseNumber() — plain Unmarshal into any decodes every JSON
	// number as float64, which truncates integers > 2^53 (Unix nanos, 64-bit
	// IDs, byte sizes). json.Number preserves the exact source digits so the
	// roundtrip is lossless for numeric inputs while still canonicalizing key
	// order.
	if len(trimmed) > 0 && trimmed[0] == '{' {
		dec := json.NewDecoder(bytes.NewReader(trimmed))
		dec.UseNumber()
		var v any
		if err := dec.Decode(&v); err == nil {
			if canonical, err := json.Marshal(v); err == nil {
				return json.RawMessage(canonical)
			}
		}
	}
	return raw
}

// UnmarshalJSON handles the polymorphic "content" field for tool_result blocks.
func (cb *ContentBlock) UnmarshalJSON(data []byte) error {
	type plain ContentBlock
	var p plain
	if err := json.Unmarshal(data, &p); err != nil {
		return err
	}
	*cb = ContentBlock(p)

	if cb.Type == "tool_result" {
		// Parse the "content" field which can be a string or array of blocks
		var raw struct {
			Content json.RawMessage `json:"content"`
		}
		if err := json.Unmarshal(data, &raw); err == nil && len(raw.Content) > 0 {
			var s string
			if err := json.Unmarshal(raw.Content, &s); err == nil {
				cb.ToolContent = s
			} else {
				var blocks []ContentBlock
				if err := json.Unmarshal(raw.Content, &blocks); err == nil {
					cb.ToolContent = blocks
				}
			}
		}
	}
	return nil
}

// NewToolUseBlock creates a tool_use content block. Input is normalized via
// normalizeToolInput so in-memory consumers (e.g. ollama.go's string(b.Input))
// never observe a literal "null" or empty bytes.
func NewToolUseBlock(id, name string, input json.RawMessage) ContentBlock {
	return ContentBlock{Type: "tool_use", ID: id, Name: name, Input: normalizeToolInput(input)}
}

// NewToolResultBlock creates a tool_result content block with string content.
func NewToolResultBlock(toolUseID, content string, isError bool) ContentBlock {
	return ContentBlock{Type: "tool_result", ToolUseID: toolUseID, IsError: isError, ToolContent: content}
}

// NewToolResultBlockWithImages creates a tool_result with nested text + image blocks.
func NewToolResultBlockWithImages(toolUseID, text string, images []ContentBlock, isError bool) ContentBlock {
	nested := []ContentBlock{{Type: "text", Text: text}}
	nested = append(nested, images...)
	return ContentBlock{Type: "tool_result", ToolUseID: toolUseID, IsError: isError, ToolContent: nested}
}

// NewToolResultBlockWithBlocks creates a tool_result whose content is a list
// of structured blocks (currently tool_reference blocks emitted by
// tool_search when the defer_loading/tool_reference protocol is active).
// Anthropic expects list-shaped content for tool_result when you want to
// preserve block type (text, image, tool_reference).
func NewToolResultBlockWithBlocks(toolUseID string, blocks []ContentBlock, isError bool) ContentBlock {
	return ContentBlock{Type: "tool_result", ToolUseID: toolUseID, IsError: isError, ToolContent: blocks}
}

// ToolResultText extracts the text from a tool_result's content.
func ToolResultText(cb ContentBlock) string {
	if cb.Type != "tool_result" {
		return ""
	}
	switch v := cb.ToolContent.(type) {
	case string:
		return v
	case []ContentBlock:
		var sb strings.Builder
		for _, b := range v {
			if b.Type == "text" {
				sb.WriteString(b.Text)
			}
		}
		return sb.String()
	}
	return ""
}

// ImageSource holds base64-encoded image data for image content blocks.
type ImageSource struct {
	Type      string `json:"type"`       // "base64"
	MediaType string `json:"media_type"` // "image/png"
	Data      string `json:"data"`
}

// MessageContent holds message content as either a plain string or content blocks.
type MessageContent struct {
	text   string
	blocks []ContentBlock
}

// NewTextContent creates a MessageContent from a plain string.
func NewTextContent(text string) MessageContent {
	return MessageContent{text: text}
}

// NewBlockContent creates a MessageContent from content blocks.
func NewBlockContent(blocks []ContentBlock) MessageContent {
	return MessageContent{blocks: blocks}
}

// Text returns the text content. For block content, concatenates text from
// text blocks and tool_result blocks.
func (mc MessageContent) Text() string {
	if mc.text != "" {
		return mc.text
	}
	var sb strings.Builder
	for _, b := range mc.blocks {
		switch b.Type {
		case "text":
			sb.WriteString(b.Text)
		case "tool_result":
			if t := ToolResultText(b); t != "" {
				sb.WriteString(t)
			}
		}
	}
	return sb.String()
}

// HasBlocks returns true if the content contains content blocks.
func (mc MessageContent) HasBlocks() bool {
	return len(mc.blocks) > 0
}

// Blocks returns the content blocks.
func (mc MessageContent) Blocks() []ContentBlock {
	return mc.blocks
}

// MarshalJSON serializes as a string if plain text, or as an array if blocks.
func (mc MessageContent) MarshalJSON() ([]byte, error) {
	if len(mc.blocks) > 0 {
		return json.Marshal(mc.blocks)
	}
	return json.Marshal(mc.text)
}

// UnmarshalJSON deserializes from either a JSON string or an array of content blocks.
func (mc *MessageContent) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		mc.text = s
		return nil
	}
	var blocks []ContentBlock
	if err := json.Unmarshal(data, &blocks); err == nil {
		mc.blocks = blocks
		return nil
	}
	return fmt.Errorf("content must be string or array of content blocks")
}

type Message struct {
	Role       string         `json:"role"`
	Content    MessageContent `json:"content"`
	Name       string         `json:"name,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

type FunctionDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

// NativeToolDef represents a provider-native tool definition (e.g., Anthropic computer use).
type NativeToolDef struct {
	Type            string `json:"type"`
	Name            string `json:"name"`
	DisplayWidthPx  int    `json:"display_width_px,omitempty"`
	DisplayHeightPx int    `json:"display_height_px,omitempty"`
}

type Tool struct {
	Type            string      `json:"type"`
	Function        FunctionDef `json:"function,omitempty"`
	Name            string      `json:"name,omitempty"`
	DisplayWidthPx  int         `json:"display_width_px,omitempty"`
	DisplayHeightPx int         `json:"display_height_px,omitempty"`
	// DeferLoading signals Anthropic to strip this tool from the cache-key prefix.
	// Server expands the schema inline when tool_search returns a tool_reference
	// block naming it. Only use on tools the model can discover via tool_search.
	DeferLoading bool `json:"defer_loading,omitempty"`
}

type CompletionRequest struct {
	Messages      []Message `json:"messages"`
	ModelTier     string    `json:"model_tier,omitempty"`
	SpecificModel string    `json:"specific_model,omitempty"`
	Temperature   float64   `json:"temperature,omitempty"`
	MaxTokens     int       `json:"max_tokens,omitempty"`
	Tools         []Tool    `json:"tools,omitempty"`
	Stream        bool      `json:"stream,omitempty"`

	// Provider-specific parameters (passed through to gateway)
	Thinking        *ThinkingConfig `json:"thinking,omitempty"`
	ReasoningEffort string          `json:"reasoning_effort,omitempty"` // OpenAI o-models: minimal/low/medium/high
	ToolChoice      any             `json:"tool_choice,omitempty"`      // nil=auto, "any", or {"type":"tool","name":"..."}

	// SessionID is sent to the gateway so the Anthropic provider can preserve
	// the previous turn's rolling cache_control marker across turns (long-session
	// CER recovery — without this, gateway sees session_id=None and falls back
	// to single rolling marker per request).
	SessionID string `json:"session_id,omitempty"`

	// CacheSource tags the call-site origin so the gateway can route prompt-cache
	// TTL by logical source (channels/TUI → 1h; cron/webhook/mcp/oneshot/
	// subagent → 5m). See docs/cache-strategy.md for the authoritative table.
	// Unset → gateway treats as "unknown" and falls back to 5m (fail cheap).
	CacheSource string `json:"cache_source,omitempty"`
}

// ThinkingConfig for Anthropic extended thinking.
// Sent as-is to the gateway which passes it to the Anthropic provider.
type ThinkingConfig struct {
	Type         string `json:"type"`                    // "adaptive", "enabled", or "disabled"
	BudgetTokens int    `json:"budget_tokens,omitempty"` // thinking token budget (only for "enabled" mode)
}

type FunctionCall struct {
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// ArgumentsString returns arguments as a JSON string regardless of
// whether the server sent a string or an object. Null/empty arguments are
// coerced to "{}" via normalizeToolInput (see issue #45) so downstream
// consumers (logging, XML fallback, audit) never see literal "null".
func (fc *FunctionCall) ArgumentsString() string {
	normalized := normalizeToolInput(fc.Arguments)
	var s string
	if err := json.Unmarshal(normalized, &s); err == nil {
		return s
	}
	return string(normalized)
}

type Usage struct {
	InputTokens           int     `json:"input_tokens"`
	OutputTokens          int     `json:"output_tokens"`
	TotalTokens           int     `json:"total_tokens"`
	CostUSD               float64 `json:"cost_usd"`
	CacheReadTokens       int     `json:"cache_read_tokens,omitempty"`
	CacheCreationTokens   int     `json:"cache_creation_tokens,omitempty"`
	CacheCreation5mTokens int     `json:"cache_creation_5m_tokens,omitempty"`
	CacheCreation1hTokens int     `json:"cache_creation_1h_tokens,omitempty"`
}

// Normalized fills backward-compatible derived fields for callers that need a
// stable usage shape across legacy and split cache-token schemas.
func (u Usage) Normalized() Usage {
	if u.TotalTokens == 0 && (u.InputTokens > 0 || u.OutputTokens > 0) {
		u.TotalTokens = u.InputTokens + u.OutputTokens
	}
	if u.CacheCreationTokens == 0 && (u.CacheCreation5mTokens > 0 || u.CacheCreation1hTokens > 0) {
		u.CacheCreationTokens = u.CacheCreation5mTokens + u.CacheCreation1hTokens
	}
	return u
}

type CompletionResponse struct {
	Provider     string         `json:"provider"`
	Model        string         `json:"model"`
	OutputText   string         `json:"output_text"`
	FinishReason string         `json:"finish_reason"`
	FunctionCall *FunctionCall  `json:"function_call,omitempty"`
	ToolCalls    []FunctionCall `json:"tool_calls,omitempty"`
	Usage        Usage          `json:"usage"`
	RequestID    string         `json:"request_id,omitempty"`
	LatencyMs    int            `json:"latency_ms,omitempty"`
	Cached       bool           `json:"cached"`
}

// AllToolCalls returns all tool calls from the response, preferring ToolCalls
// array if present, falling back to single FunctionCall for backward compat.
func (r *CompletionResponse) AllToolCalls() []FunctionCall {
	if len(r.ToolCalls) > 0 {
		return r.ToolCalls
	}
	if r.FunctionCall != nil {
		return []FunctionCall{*r.FunctionCall}
	}
	return nil
}

// HasToolCalls returns true if the response contains any tool calls.
func (r *CompletionResponse) HasToolCalls() bool {
	return len(r.ToolCalls) > 0 || r.FunctionCall != nil
}

// --- Task/workflow types (used by /research, /swarm) ---

type TaskRequest struct {
	Query            string         `json:"query"`
	SessionID        string         `json:"session_id,omitempty"`
	Context          map[string]any `json:"context,omitempty"`
	ResearchStrategy string         `json:"research_strategy,omitempty"`
}

type TaskStreamResponse struct {
	WorkflowID string `json:"workflow_id"`
	TaskID     string `json:"task_id"`
	StreamURL  string `json:"stream_url"`
}

type TaskStatusResponse struct {
	TaskID     string         `json:"task_id"`
	WorkflowID string         `json:"workflow_id"`
	Status     string         `json:"status"`
	Result     string         `json:"result"`
	Query      string         `json:"query"`
	Usage      map[string]any `json:"usage,omitempty"`
}

// --- Client ---

type GatewayClient struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client
}

func NewGatewayClient(baseURL, apiKey string) *GatewayClient {
	return &GatewayClient{
		baseURL: baseURL,
		apiKey:  apiKey,
		httpClient: &http.Client{
			Timeout: 600 * time.Second,
		},
	}
}

// HTTPClient exposes the underlying *http.Client so sibling subsystems
// (e.g. internal/uploads) can reuse the same transport, timeout, and any
// future tracing wiring without standing up their own client.
func (c *GatewayClient) HTTPClient() *http.Client { return c.httpClient }

// BaseURL exposes the gateway base URL for sibling subsystems that need to
// build their own request paths against the same host.
func (c *GatewayClient) BaseURL() string { return c.baseURL }

// Complete sends a completion request to the gateway's /v1/completions endpoint.
// This endpoint is a thin proxy to the LLM service that returns raw function_call
// responses for client-side tool execution.
func (c *GatewayClient) Complete(ctx context.Context, req CompletionRequest) (*CompletionResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	reqID := logCacheDebug(req, "complete")

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/completions", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		httpReq.Header.Set("X-API-Key", c.apiKey)
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, &APIError{StatusCode: resp.StatusCode, Body: readResponseBody(resp)}
	}

	var result CompletionResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	logCacheResponse(reqID, req.SessionID, &result)
	return &result, nil
}

// StreamDelta represents an incremental text chunk from streaming completion.
type StreamDelta struct {
	Text string
}

// CompleteStream sends a streaming completion request. It calls onDelta for each
// text chunk and returns the final CompletionResponse when done.
func (c *GatewayClient) CompleteStream(ctx context.Context, req CompletionRequest, onDelta func(StreamDelta)) (*CompletionResponse, error) {
	req.Stream = true
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	reqID := logCacheDebug(req, "stream")

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/completions", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	if c.apiKey != "" {
		httpReq.Header.Set("X-API-Key", c.apiKey)
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, &APIError{StatusCode: resp.StatusCode, Body: readResponseBody(resp)}
	}

	// Parse SSE stream
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	var result *CompletionResponse
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

		var event map[string]json.RawMessage
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			continue
		}

		typeBytes, ok := event["type"]
		if !ok {
			continue
		}
		var eventType string
		json.Unmarshal(typeBytes, &eventType)

		switch eventType {
		case "content_delta":
			var text string
			if textBytes, ok := event["text"]; ok {
				json.Unmarshal(textBytes, &text)
			}
			if text != "" && onDelta != nil {
				onDelta(StreamDelta{Text: text})
			}
		case "done":
			// The done event has the full response shape
			var final CompletionResponse
			if err := json.Unmarshal([]byte(payload), &final); err == nil {
				result = &final
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("stream read error: %w", err)
	}

	if result == nil {
		return nil, fmt.Errorf("stream ended without done event")
	}

	logCacheResponse(reqID, req.SessionID, result)
	return result, nil
}

func (c *GatewayClient) SubmitTaskStream(ctx context.Context, req TaskRequest) (*TaskStreamResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/v1/tasks/stream", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		httpReq.Header.Set("X-API-Key", c.apiKey)
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("gateway request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		body := readResponseBody(resp)
		if body != "" {
			return nil, fmt.Errorf("gateway returned %d (expected 201): %s", resp.StatusCode, body)
		}
		return nil, fmt.Errorf("gateway returned %d (expected 201)", resp.StatusCode)
	}

	var result TaskStreamResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return &result, nil
}

// GetTask fetches the full task result from the REST API.
// Unlike SSE events which truncate at 10K chars, the REST response contains
// the complete untruncated result.
func (c *GatewayClient) GetTask(ctx context.Context, taskID string) (*TaskStatusResponse, error) {
	endpoint := fmt.Sprintf("%s/api/v1/tasks/%s", c.baseURL, url.PathEscape(taskID))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	if c.apiKey != "" {
		req.Header.Set("X-API-Key", c.apiKey)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		errBody := readResponseBody(resp)
		if errBody != "" {
			return nil, fmt.Errorf("get task returned %d: %s", resp.StatusCode, errBody)
		}
		return nil, fmt.Errorf("get task returned %d", resp.StatusCode)
	}

	var result TaskStatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return &result, nil
}

// ApproveReviewPlan approves a HITL research plan so the workflow continues.
func (c *GatewayClient) ApproveReviewPlan(ctx context.Context, workflowID string) error {
	body, _ := json.Marshal(map[string]string{"action": "approve"})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("%s/api/v1/tasks/%s/review", c.baseURL, url.PathEscape(workflowID)), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("X-API-Key", c.apiKey)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("approve request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("approve returned %d", resp.StatusCode)
	}
	return nil
}

func (c *GatewayClient) StreamURL(workflowID string) string {
	return fmt.Sprintf("%s/api/v1/stream/sse?workflow_id=%s", c.baseURL, workflowID)
}

// ResolveURL prepends the base URL if the given URL is relative (starts with /).
func (c *GatewayClient) ResolveURL(u string) string {
	if len(u) > 0 && u[0] == '/' {
		return c.baseURL + u
	}
	return u
}

func (c *GatewayClient) Health(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/health", nil)
	if err != nil {
		return err
	}
	if c.apiKey != "" {
		req.Header.Set("X-API-Key", c.apiKey)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body := readResponseBody(resp)
		if body != "" {
			return fmt.Errorf("health check returned %d: %s", resp.StatusCode, body)
		}
		return fmt.Errorf("health check returned %d", resp.StatusCode)
	}
	return nil
}

// --- Server tool types (used by tools.RegisterAll) ---

type ServerToolSchema struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

type ToolExecuteRequest struct {
	Arguments map[string]any `json:"arguments"`
	SessionID string         `json:"session_id,omitempty"`
}

type ToolExecuteResponse struct {
	Success         bool            `json:"success"`
	Output          json.RawMessage `json:"output"`
	Text            *string         `json:"text"`
	Error           *string         `json:"error"`
	ExecutionTimeMs int             `json:"execution_time_ms,omitempty"`
	// Usage reports resource consumption from the underlying provider (e.g.
	// xAI Grok tokens for x_search, SerpAPI query count for web_search).
	// Server-populated when available; nil when the tool does not bill per call.
	Usage *ToolUsage `json:"usage,omitempty"`
}

// ToolUsage captures cost information reported by a gateway tool execution.
// Shannon Cloud's current schema is flat: a single `tokens` count and
// `cost_usd`. For SERP tools (web_search), `tokens` is a synthetic billing
// count (e.g. 7500 per query at $2/1M rate). For LLM-backed tools (x_search
// → xAI Responses API), `tokens` is the real input+output token total and
// `cost_usd` includes both the per-call fee and token cost.
//
// Expanded fields (Input/OutputTokens, Provider, Model, Units) are reserved
// for future gateway schema upgrades; today only Tokens+CostUSD are populated.
type ToolUsage struct {
	Tokens    int     `json:"tokens,omitempty"` // gateway's current flat token count
	CostUSD   float64 `json:"cost_usd,omitempty"`
	CostModel string  `json:"cost_model,omitempty"` // synthetic model tag (e.g. "shannon_web_search", "grok-3")

	// Forward-compat fields — populated only if/when the gateway schema adds them.
	Provider     string `json:"provider,omitempty"`
	Model        string `json:"model,omitempty"`
	InputTokens  int    `json:"input_tokens,omitempty"`
	OutputTokens int    `json:"output_tokens,omitempty"`
	TotalTokens  int    `json:"total_tokens,omitempty"`
	Units        int    `json:"units,omitempty"`
	UnitType     string `json:"unit_type,omitempty"`
}

// ListTools fetches available server-side tool schemas from the gateway.
func (c *GatewayClient) ListTools(ctx context.Context) ([]ServerToolSchema, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/api/v1/tools", nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	if c.apiKey != "" {
		req.Header.Set("X-API-Key", c.apiKey)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, &APIError{StatusCode: resp.StatusCode, Body: readResponseBody(resp)}
	}

	var tools []ServerToolSchema
	if err := json.NewDecoder(resp.Body).Decode(&tools); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return tools, nil
}

// ExecuteTool calls a server-side tool by name with the given arguments.
func (c *GatewayClient) ExecuteTool(ctx context.Context, name string, arguments map[string]any, sessionID string) (*ToolExecuteResponse, error) {
	reqBody := ToolExecuteRequest{
		Arguments: arguments,
		SessionID: sessionID,
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	endpoint := c.baseURL + "/api/v1/tools/" + url.PathEscape(name) + "/execute"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("X-API-Key", c.apiKey)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		errBody := readResponseBody(resp)
		if errBody != "" {
			return nil, fmt.Errorf("tool %s returned %d: %s", name, resp.StatusCode, errBody)
		}
		return nil, fmt.Errorf("tool %s returned %d", name, resp.StatusCode)
	}

	var result ToolExecuteResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return &result, nil
}

func readResponseBody(resp *http.Response) string {
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(body))
}

// SessionEnvelope wraps one session JSON with its routing hint.
// agent_name is a sub-partition hint only — tenancy is anchored by API key auth.
type SessionEnvelope struct {
	AgentName string          `json:"agent_name"`
	Session   json.RawMessage `json:"session"`
}

type SyncBatchRequest struct {
	ClientVersion string            `json:"client_version"`
	SyncAt        time.Time         `json:"sync_at"`
	Sessions      []SessionEnvelope `json:"sessions"`
}

type RejectedEntry struct {
	ID     string `json:"id"`
	Reason string `json:"reason"`
}

type SyncBatchResponse struct {
	Accepted []string        `json:"accepted"`
	Rejected []RejectedEntry `json:"rejected"`
}

// SyncSessions POSTs a batch to /api/v1/sessions/sync. Non-2xx responses,
// network errors, and malformed bodies are returned as errors. Per-session
// accepted/rejected partitioning is the caller's responsibility.
func (c *GatewayClient) SyncSessions(ctx context.Context, batch SyncBatchRequest) (SyncBatchResponse, error) {
	body, err := json.Marshal(batch)
	if err != nil {
		return SyncBatchResponse{}, fmt.Errorf("marshal sync batch: %w", err)
	}
	endpoint := c.baseURL + "/api/v1/sessions/sync"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return SyncBatchResponse{}, fmt.Errorf("build sync request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		httpReq.Header.Set("X-API-Key", c.apiKey)
	}
	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return SyncBatchResponse{}, fmt.Errorf("sync request: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return SyncBatchResponse{}, fmt.Errorf("sync returned %d: %s", resp.StatusCode, string(respBody))
	}
	var out SyncBatchResponse
	if err := json.Unmarshal(respBody, &out); err != nil {
		return SyncBatchResponse{}, fmt.Errorf("parse sync response: %w", err)
	}
	return out, nil
}
