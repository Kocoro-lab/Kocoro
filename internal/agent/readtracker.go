package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/cwdctx"
)

// readTrackerKey is the context key for ReadTracker.
type readTrackerKey struct{}

// memoryDirKey is the context key for the agent's memory directory path.
type memoryDirKey struct{}

// conversationSnapshotKey 是获取当前对话快照的 context key。
type conversationSnapshotKey struct{}

// ConversationSnapshotFunc 返回当前对话消息的快照副本。
type ConversationSnapshotFunc func() []client.Message

// WithConversationSnapshot 注入对话快照提供函数到 context。
func WithConversationSnapshot(ctx context.Context, fn ConversationSnapshotFunc) context.Context {
	return context.WithValue(ctx, conversationSnapshotKey{}, fn)
}

// ConversationSnapshotFromContext 从 context 获取对话快照提供函数。
// 调用返回的函数可获取当前对话消息的副本。无 provider 时返回 nil。
func ConversationSnapshotFromContext(ctx context.Context) ConversationSnapshotFunc {
	fn, _ := ctx.Value(conversationSnapshotKey{}).(ConversationSnapshotFunc)
	return fn
}

// WithMemoryDir returns a new context with the memory directory set.
func WithMemoryDir(ctx context.Context, dir string) context.Context {
	return context.WithValue(ctx, memoryDirKey{}, dir)
}

// MemoryDirFromContext returns the memory directory from context, or "".
func MemoryDirFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(memoryDirKey{}).(string); ok {
		return v
	}
	return ""
}

// agentNameKey is the context key for the current agent's name.
// Empty string is meaningful — it represents the default agent.
type agentNameKey struct{}

// WithAgentName returns a new context with the agent name set. Tools that
// need "who is calling me" (e.g. schedule_create wanting to default to the
// caller's agent) read it via AgentNameFromContext.
func WithAgentName(ctx context.Context, name string) context.Context {
	return context.WithValue(ctx, agentNameKey{}, name)
}

// AgentNameFromContext returns the current agent's name from context plus
// an "ok" boolean. ok=false means no agent identity was injected (tool
// called outside the agent loop); ok=true with empty string means the
// caller is explicitly the default agent.
func AgentNameFromContext(ctx context.Context) (string, bool) {
	v, ok := ctx.Value(agentNameKey{}).(string)
	return v, ok
}

// sourceKey is the context key for the per-call originating source
// (e.g. "slack", "feishu", "webview", "tui", "cli", "one-shot").
type sourceKey struct{}

// WithSource returns a new context with the originating source set. Tools
// that need to capture where the call came from (currently schedule_create,
// to feed the broadcast smart-default gate downstream) read it via
// SourceFromContext. Empty string means "unknown / pre-feature caller".
func WithSource(ctx context.Context, source string) context.Context {
	return context.WithValue(ctx, sourceKey{}, source)
}

// SourceFromContext returns the originating source from context plus an
// "ok" boolean. ok=false means no source was injected (tool called outside
// the agent loop); ok=true with empty string means the loop explicitly
// recorded an unknown source.
func SourceFromContext(ctx context.Context) (string, bool) {
	v, ok := ctx.Value(sourceKey{}).(string)
	return v, ok
}

// imStatusContextKey is the context key for the run's inbound IMStatusContext
// (the opaque platform routing blob Cloud sent with the triggering message).
type imStatusContextKey struct{}

// WithIMStatusContext returns a new context carrying the run's inbound
// IMStatusContext. schedule_create reads it via IMStatusContextFromContext to
// snapshot a proactive-delivery target onto the new Schedule (so a later
// scheduled run can be routed back to the originating IM thread). Empty/nil for
// non-IM runs (TUI/CLI/Desktop/cron) — the downstream falls back to broadcast.
func WithIMStatusContext(ctx context.Context, blob json.RawMessage) context.Context {
	return context.WithValue(ctx, imStatusContextKey{}, blob)
}

// IMStatusContextFromContext returns the run's inbound IMStatusContext (nil
// when none was injected or the run was non-IM).
func IMStatusContextFromContext(ctx context.Context) (json.RawMessage, bool) {
	v, ok := ctx.Value(imStatusContextKey{}).(json.RawMessage)
	return v, ok
}

// IsMemoryFile returns true if path resolves to the MEMORY.md inside the
// agent's configured memory directory. Returns false when no memory dir
// is set in context (e.g. tool called outside agent loop).
func IsMemoryFile(ctx context.Context, path string) bool {
	dir, ok := ctx.Value(memoryDirKey{}).(string)
	if !ok || dir == "" {
		return false
	}
	resolvedPath := cwdctx.ResolvePath(ctx, path)
	memPath := filepath.Clean(filepath.Join(dir, "MEMORY.md"))
	return strings.EqualFold(resolvedPath, memPath)
}

// ReadTrackerKey returns the context key used to store a ReadTracker.
// Exported for use in tests that need to inject a tracker into context.
func ReadTrackerKey() any { return readTrackerKey{} }

// fileReadEntry records the (mtime, size, offset, limit) tuple of a prior
// file_read so a repeat call with the same tuple can return a short stub
// instead of replaying the same content. when is the wallclock time of
// the original read.
type fileReadEntry struct {
	mtime  time.Time
	size   int64
	offset int
	limit  int
	when   time.Time
}

type fileReadKey struct {
	path   string
	offset int
	limit  int
}

// ReadTracker tracks which files have been read during the current agent turn.
// Used to enforce read-before-edit: file_edit and file_write on existing files
// must be preceded by a file_read of that file. Also tracks per-range read
// history so file_read can dedup repeat calls with the same (path, mtime,
// size, offset, limit) tuple. mu serializes access because file_read can
// run inside parallel read-only batches (executeBatches), while MarkRead
// from the post-loop runs on the main goroutine.
type ReadTracker struct {
	mu        sync.Mutex
	read      map[string]bool
	lastReads map[fileReadKey]fileReadEntry
	cwd       string
}

// NewReadTracker creates a new ReadTracker.
func NewReadTracker() *ReadTracker {
	return &ReadTracker{
		read:      make(map[string]bool),
		lastReads: make(map[fileReadKey]fileReadEntry),
	}
}

// SetCWD sets the session CWD used for relative path resolution.
func (rt *ReadTracker) SetCWD(cwd string) {
	rt.cwd = cwd
}

// ResetTurnReads clears per-turn read-before-write state while preserving
// session-scoped file_read dedup history.
func (rt *ReadTracker) ResetTurnReads() {
	rt.mu.Lock()
	rt.read = make(map[string]bool)
	rt.mu.Unlock()
}

// MarkRead records that a file has been read.
func (rt *ReadTracker) MarkRead(path string) {
	norm := normalizePathWithCWD(path, rt.cwd)
	if norm == "" {
		return
	}
	rt.mu.Lock()
	rt.read[norm] = true
	rt.mu.Unlock()
}

// HasRead returns true if the file has been read in this turn.
func (rt *ReadTracker) HasRead(path string) bool {
	norm := normalizePathWithCWD(path, rt.cwd)
	if norm == "" {
		return false
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.read[norm]
}

// CheckFileReadDedup returns (true, stub) when the same path was already read
// in this session at the same (offset, limit) AND the file's mtime+size are
// unchanged. Returns (false, "") otherwise — caller should perform the read
// and call RecordFileRead afterwards. Returns (false, "") when no tracker
// is in context (e.g. tool called outside the agent loop).
func CheckFileReadDedup(ctx context.Context, path string, offset, limit int, mtime time.Time, size int64) (bool, string) {
	rt, ok := ctx.Value(readTrackerKey{}).(*ReadTracker)
	if !ok || rt == nil {
		return false, ""
	}
	norm := normalizePathWithCWD(path, rt.cwd)
	if norm == "" {
		return false, ""
	}
	key := fileReadKey{path: norm, offset: offset, limit: limit}
	rt.mu.Lock()
	entry, exists := rt.lastReads[key]
	rt.mu.Unlock()
	if !exists {
		return false, ""
	}
	if entry.mtime.Equal(mtime) && entry.size == size {
		stub := fmt.Sprintf(
			"[file unchanged since last read at %s — to force re-read, modify the file or use a different offset/limit range]",
			entry.when.Format("15:04:05"),
		)
		return true, stub
	}
	return false, ""
}

// RecordFileRead stores a per-range read entry for later dedup checks.
// No-op when no tracker is in context.
func RecordFileRead(ctx context.Context, path string, offset, limit int, mtime time.Time, size int64) {
	rt, ok := ctx.Value(readTrackerKey{}).(*ReadTracker)
	if !ok || rt == nil {
		return
	}
	norm := normalizePathWithCWD(path, rt.cwd)
	if norm == "" {
		return
	}
	key := fileReadKey{path: norm, offset: offset, limit: limit}
	rt.mu.Lock()
	rt.lastReads[key] = fileReadEntry{
		mtime:  mtime,
		size:   size,
		offset: offset,
		limit:  limit,
		when:   time.Now(),
	}
	rt.mu.Unlock()
}

// CheckReadBeforeWrite extracts the ReadTracker from context and returns an error
// if the given path has not been read. Returns nil if the tracker is absent (e.g.,
// tool called outside the agent loop) or the file has been read.
func CheckReadBeforeWrite(ctx context.Context, path string) error {
	rt, ok := ctx.Value(readTrackerKey{}).(*ReadTracker)
	if !ok || rt == nil {
		return nil
	}
	if !rt.HasRead(path) {
		return fmt.Errorf("You must read this file with file_read before editing it. Path: %s", path)
	}
	return nil
}

// normalizePathWithCWD resolves a path to an absolute, clean, symlink-resolved
// form using the given cwd for relative path resolution. When cwd is empty
// (scopeless daemon tasks that arrive without a CWD) a relative input is
// returned cleaned but unresolved; callers must not fall back to the daemon
// process cwd, which is what the wider CWD-hardening work was meant to
// eliminate.
func normalizePathWithCWD(path, cwd string) string {
	if path == "" {
		return ""
	}
	if !filepath.IsAbs(path) {
		if cwd == "" {
			return filepath.Clean(path)
		}
		path = filepath.Join(cwd, path)
	}
	path = filepath.Clean(path)
	// Try to resolve symlinks; if it fails (file doesn't exist yet), use the clean path.
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	return path
}
