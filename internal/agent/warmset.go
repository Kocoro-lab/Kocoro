package agent

import (
	"encoding/json"
	"sync"
	"sync/atomic"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

// Warm-set limits bound how many deferred tool schemas a session keeps warmed
// after tool_search loads them. Workload: long sessions against large MCP
// catalogs where the model progressively loads many cold tools. Symptom when
// a limit binds: the least-recently-loaded schema is evicted, so the next call
// to that tool costs one extra tool_search round trip to re-warm it. Override:
// agent.warm_set_max_schemas / agent.warm_set_max_schema_tokens in config.
// Set once at startup (SetWorkingSetLimits) before any session runs.
const (
	defaultWorkingSetSchemaCountLimit = 16
	defaultWorkingSetSchemaTokenLimit = 8000
)

var (
	workingSetSchemaCountLimit atomic.Int64
	workingSetSchemaTokenLimit atomic.Int64
)

func init() {
	workingSetSchemaCountLimit.Store(defaultWorkingSetSchemaCountLimit)
	workingSetSchemaTokenLimit.Store(defaultWorkingSetSchemaTokenLimit)
}

// SetWorkingSetLimits overrides the warm-set caps from configuration. Values
// < 1 fall back to the defaults so a zero-valued config cannot disable the
// runaway defense entirely.
func SetWorkingSetLimits(maxSchemas, maxTokens int) {
	if maxSchemas < 1 {
		maxSchemas = defaultWorkingSetSchemaCountLimit
	}
	if maxTokens < 1 {
		maxTokens = defaultWorkingSetSchemaTokenLimit
	}
	workingSetSchemaCountLimit.Store(int64(maxSchemas))
	workingSetSchemaTokenLimit.Store(int64(maxTokens))
}

// WorkingSet is a session-scoped cache of deferred tool schemas that were
// previously loaded via tool_search. The cache is invalidated whenever the
// underlying effective toolset fingerprint changes.
type WorkingSet struct {
	mu          sync.RWMutex
	fingerprint string
	schemas     map[string]client.Tool
	order       map[string]uint64
	nextOrder   uint64
	searchKey   string
	searchIndex *toolSearchIndex
}

// NewWorkingSet creates an empty working set.
func NewWorkingSet() *WorkingSet {
	return &WorkingSet{
		schemas: make(map[string]client.Tool),
		order:   make(map[string]uint64),
	}
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
	if ws.order == nil {
		ws.order = make(map[string]uint64)
	}
	ws.nextOrder++
	ws.schemas[name] = schema
	ws.order[name] = ws.nextOrder
	ws.evictLocked()
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
	delete(ws.order, name)
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
	ws.order = make(map[string]uint64)
	ws.nextOrder = 0
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
	ws.order = make(map[string]uint64)
	ws.nextOrder = 0
	ws.searchKey = ""
	ws.searchIndex = nil
}

func workingSetSchemaTokens(schema client.Tool) int {
	body, err := json.Marshal(schema)
	if err != nil {
		// An unsizable schema counts as a full-budget entry, not a free one —
		// otherwise a schema that fails to marshal would pin unlimited
		// siblings in the warm set without ever contributing to the cap.
		return int(workingSetSchemaTokenLimit.Load())
	}
	return int((float64(len(body)) + charsPerTokenSchema - 1) / charsPerTokenSchema)
}

func (ws *WorkingSet) evictLocked() {
	countLimit := int(workingSetSchemaCountLimit.Load())
	tokenLimit := int(workingSetSchemaTokenLimit.Load())
	// len > 1 keeps the most recently added schema even when it alone exceeds
	// the token budget: evicting the entry the model just loaded would force a
	// tool_search → load → evict spin on every call to that tool. One
	// oversized schema over budget beats a tool that can never stay warm.
	for len(ws.schemas) > 1 && (len(ws.schemas) > countLimit || ws.schemaTokensLocked() > tokenLimit) {
		newestName := ""
		newestOrder := uint64(0)
		oldestName := ""
		oldestOrder := ^uint64(0)
		for name, order := range ws.order {
			if order < oldestOrder {
				oldestName = name
				oldestOrder = order
			}
			if order >= newestOrder {
				newestName = name
				newestOrder = order
			}
		}
		if oldestName == "" || oldestName == newestName {
			return
		}
		delete(ws.schemas, oldestName)
		delete(ws.order, oldestName)
	}
}

func (ws *WorkingSet) schemaTokensLocked() int {
	total := 0
	for _, schema := range ws.schemas {
		total += workingSetSchemaTokens(schema)
	}
	return total
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
