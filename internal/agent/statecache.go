package agent

import (
	"encoding/json"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

type StateDomain string

const (
	StateDomainBrowser    StateDomain = "browser"
	StateDomainFilesystem StateDomain = "filesystem"
	StateDomainProcess    StateDomain = "process"
)

type StateRef struct {
	Domain StateDomain
	Scope  string
}

type CallStateTraits struct {
	Reads        []StateRef
	Writes       []StateRef
	UnknownWrite bool
	Cacheable    bool
}

// CrossIterationCacheable is an explicit referential-transparency contract.
// Read-only and safe-to-run are insufficient: job status, browser snapshots,
// files, calendars, and remote records can change without an in-loop write.
// Tools must opt in only when identical arguments are guaranteed to return an
// equivalent result for the entire AgentLoop run.
type CrossIterationCacheable interface {
	CacheAcrossIterations(argsJSON string) bool
}

type stateVersionTracker struct {
	versions map[string]int
}

func newStateVersionTracker() *stateVersionTracker {
	return &stateVersionTracker{versions: make(map[string]int)}
}

func (t *stateVersionTracker) fingerprint(refs []StateRef) string {
	if len(refs) == 0 {
		return ""
	}
	seen := make(map[string]bool, len(refs))
	parts := make([]string, 0, len(refs))
	for _, ref := range refs {
		key := stateRefKey(ref)
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		parts = append(parts, key+"="+strconv.Itoa(t.versions[key]))
	}
	sort.Strings(parts)
	return strings.Join(parts, "|")
}

func (t *stateVersionTracker) bump(refs []StateRef) {
	for _, ref := range refs {
		key := stateRefKey(ref)
		if key == "" {
			continue
		}
		t.versions[key]++
	}
}

func stateRefKey(ref StateRef) string {
	scope := strings.TrimSpace(ref.Scope)
	if scope == "" {
		scope = "*"
	}
	return string(ref.Domain) + "\x00" + scope
}

func browserStateRef() StateRef {
	return StateRef{Domain: StateDomainBrowser, Scope: "active"}
}

func filesystemStateRef(path string) StateRef {
	path = strings.TrimSpace(path)
	if path == "" {
		return StateRef{}
	}
	return StateRef{Domain: StateDomainFilesystem, Scope: filepath.Clean(path)}
}

func processSessionStateRef() StateRef {
	return StateRef{Domain: StateDomainProcess, Scope: "session"}
}

func resolveCallStateTraits(toolName, argsJSON string) CallStateTraits {
	switch toolName {
	case "browser_snapshot", "browser_take_screenshot", "browser_tabs":
		return CallStateTraits{
			Reads:     []StateRef{browserStateRef()},
			Cacheable: true,
		}
	case "browser_navigate", "browser_click", "browser_type", "browser_press_key", "browser_drag", "browser_select_option":
		return CallStateTraits{
			Writes: []StateRef{browserStateRef()},
		}
	case "file_read":
		if ref := filesystemStateRef(extractPathArg(argsJSON)); ref != (StateRef{}) {
			return CallStateTraits{
				Reads:     []StateRef{ref},
				Cacheable: true,
			}
		}
	case "file_write", "file_edit":
		if ref := filesystemStateRef(extractPathArg(argsJSON)); ref != (StateRef{}) {
			return CallStateTraits{
				Writes: []StateRef{ref},
			}
		}
	case "bash":
		return CallStateTraits{
			Writes:       []StateRef{processSessionStateRef()},
			UnknownWrite: true,
		}
	}

	if strings.HasPrefix(toolName, "browser_") {
		return CallStateTraits{
			Writes: []StateRef{browserStateRef()},
		}
	}

	return CallStateTraits{}
}

func resolveFallbackReadStateTraits(tool Tool, argsJSON string) CallStateTraits {
	if tool == nil {
		return CallStateTraits{}
	}
	// Read-only classification controls approval and parallel scheduling, but it
	// does not imply referential transparency. These GUI tools keep observation
	// state (refs, state IDs, and coordinate image authority) on the tool
	// instance, so serving a cross-iteration cached result would bypass the Run
	// call that installs that state. Every such observation must execute.
	switch tool.Info().Name {
	case "computer_use", "accessibility":
		return CallStateTraits{}
	}
	readOnly, ok := tool.(ReadOnlyChecker)
	if !ok || !readOnly.IsReadOnlyCall(argsJSON) {
		return CallStateTraits{}
	}
	cacheable, ok := tool.(CrossIterationCacheable)
	if !ok || !cacheable.CacheAcrossIterations(argsJSON) {
		return CallStateTraits{}
	}
	return CallStateTraits{
		Reads:     []StateRef{processSessionStateRef()},
		Cacheable: true,
	}
}

func enforceCrossIterationCacheContract(tool Tool, argsJSON string, traits CallStateTraits) CallStateTraits {
	if !traits.Cacheable {
		return traits
	}
	cacheable, ok := tool.(CrossIterationCacheable)
	if !ok || !cacheable.CacheAcrossIterations(argsJSON) {
		traits.Cacheable = false
	}
	return traits
}

func buildStateAwareCacheKey(toolName string, args json.RawMessage, traits CallStateTraits, tracker *stateVersionTracker) string {
	if !traits.Cacheable || tracker == nil {
		return ""
	}
	base := toolName + "\x00" + normalizeJSON(args)
	fingerprint := tracker.fingerprint(traits.Reads)
	if fingerprint == "" {
		return ""
	}
	return base + "\x00" + fingerprint
}
