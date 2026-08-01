package agent

import (
	"sync"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

// WorkingSet is a session-scoped cache of deferred tool schemas that were
// previously loaded via tool_search. The cache is invalidated whenever the
// underlying effective toolset fingerprint changes.
type WorkingSet struct {
	mu          sync.RWMutex
	fingerprint string
	schemas     map[string]client.Tool
	searchKey   string
	searchIndex *toolSearchIndex
}

// NewWorkingSet creates an empty working set.
func NewWorkingSet() *WorkingSet {
	return &WorkingSet{schemas: make(map[string]client.Tool)}
}

// Add inserts or replaces a schema in the working set.
func (ws *WorkingSet) Add(name string, schema client.Tool) {
	if ws == nil || name == "" {
		return
	}
	ws.mu.Lock()
	defer ws.mu.Unlock()
	if ws.schemas == nil {
		ws.schemas = make(map[string]client.Tool)
	}
	ws.schemas[name] = schema
}

// Remove forgets one warmed schema without invalidating the remaining
// session-scoped cache. Run-scoped, profile-bound tools use this during setup
// so a prior run can never advertise them without resolving a fresh profile.
func (ws *WorkingSet) Remove(name string) {
	if ws == nil || name == "" {
		return
	}
	ws.mu.Lock()
	defer ws.mu.Unlock()
	delete(ws.schemas, name)
}

// Contains reports whether the working set contains the named schema.
func (ws *WorkingSet) Contains(name string) bool {
	if ws == nil || name == "" {
		return false
	}
	ws.mu.RLock()
	defer ws.mu.RUnlock()
	_, ok := ws.schemas[name]
	return ok
}

// Get returns the named schema when present.
func (ws *WorkingSet) Get(name string) (client.Tool, bool) {
	if ws == nil || name == "" {
		return client.Tool{}, false
	}
	ws.mu.RLock()
	defer ws.mu.RUnlock()
	schema, ok := ws.schemas[name]
	return schema, ok
}

// Schemas returns a copy of the cached schema map.
func (ws *WorkingSet) Schemas() map[string]client.Tool {
	if ws == nil {
		return nil
	}
	ws.mu.RLock()
	defer ws.mu.RUnlock()
	out := make(map[string]client.Tool, len(ws.schemas))
	for name, schema := range ws.schemas {
		out[name] = schema
	}
	return out
}

// Len returns the number of warmed schemas.
func (ws *WorkingSet) Len() int {
	if ws == nil {
		return 0
	}
	ws.mu.RLock()
	defer ws.mu.RUnlock()
	return len(ws.schemas)
}

// Fingerprint returns the current toolset fingerprint tracked by this cache.
func (ws *WorkingSet) Fingerprint() string {
	if ws == nil {
		return ""
	}
	ws.mu.RLock()
	defer ws.mu.RUnlock()
	return ws.fingerprint
}

// EnsureFingerprint updates the tracked fingerprint and clears the warmed
// schemas whenever the fingerprint changes.
func (ws *WorkingSet) EnsureFingerprint(fingerprint string) bool {
	if ws == nil {
		return false
	}
	ws.mu.Lock()
	defer ws.mu.Unlock()
	if ws.schemas == nil {
		ws.schemas = make(map[string]client.Tool)
	}
	if ws.fingerprint == fingerprint {
		return false
	}
	ws.fingerprint = fingerprint
	ws.schemas = make(map[string]client.Tool)
	ws.searchKey = ""
	ws.searchIndex = nil
	return true
}

// SyncToolset invalidates the cache when the effective toolset fingerprint
// changes. Returns true when invalidation occurred.
func (ws *WorkingSet) SyncToolset(reg *ToolRegistry) bool {
	return ws.EnsureFingerprint(toolSchemaFingerprint(reg))
}

// Invalidate clears the cache and forgets the tracked fingerprint.
func (ws *WorkingSet) Invalidate() {
	if ws == nil {
		return
	}
	ws.mu.Lock()
	defer ws.mu.Unlock()
	ws.fingerprint = ""
	ws.schemas = make(map[string]client.Tool)
	ws.searchKey = ""
	ws.searchIndex = nil
}

// toolSearchIndex reuses the tokenized BM25 index while the effective toolset
// fingerprint and Deferred name set are unchanged. AgentLoop synchronizes the
// full metadata fingerprint before discovery; cache hits therefore hash only
// the small Deferred name set instead of rebuilding every search document.
func (ws *WorkingSet) toolSearchIndex(reg *ToolRegistry, deferred map[string]bool) *toolSearchIndex {
	if ws == nil {
		return newToolSearchIndex(reg, deferred)
	}
	if ws.Fingerprint() == "" {
		ws.SyncToolset(reg)
	}
	searchKey := ws.Fingerprint() + "\x00" + deferredToolSearchKey(deferred)

	ws.mu.RLock()
	if ws.searchIndex != nil && ws.searchKey == searchKey {
		index := ws.searchIndex
		ws.mu.RUnlock()
		return index
	}
	ws.mu.RUnlock()

	index := newToolSearchIndex(reg, deferred)
	ws.mu.Lock()
	if ws.searchIndex != nil && ws.searchKey == searchKey {
		cached := ws.searchIndex
		ws.mu.Unlock()
		return cached
	}
	ws.searchKey = searchKey
	ws.searchIndex = index
	ws.mu.Unlock()
	return index
}
