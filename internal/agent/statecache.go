package agent

// State-version tracking for result shaping. shapeContextKey (resultshape.go)
// fingerprints a call's read domains so tree-result shaping starts a new
// generation whenever a tracked write advanced the underlying state.
//
// A cross-iteration tool-result cache used to live here behind a
// CrossIterationCacheable opt-in. No tool ever implemented it — read-only and
// safe-to-run do not imply referential transparency (job status, browser
// snapshots, files, calendars, and remote records change without an in-loop
// write; GUI observation tools additionally keep refs/state IDs on the tool
// instance, so serving a cached result would bypass the Run call that
// installs that state). With the contract permanently unclaimed the cache was
// dead weight and was removed; duplicate-read protection is owned by the
// ReadTracker file_read dedup independently (pinned by
// TestFileReadDedupHoldsWithoutStateCache).

import (
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
	Reads  []StateRef
	Writes []StateRef
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
			Reads: []StateRef{browserStateRef()},
		}
	case "browser_navigate", "browser_click", "browser_type", "browser_press_key", "browser_drag", "browser_select_option":
		return CallStateTraits{
			Writes: []StateRef{browserStateRef()},
		}
	case "file_read":
		if ref := filesystemStateRef(extractPathArg(argsJSON)); ref != (StateRef{}) {
			return CallStateTraits{
				Reads: []StateRef{ref},
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
			Writes: []StateRef{processSessionStateRef()},
		}
	}

	if strings.HasPrefix(toolName, "browser_") {
		return CallStateTraits{
			Writes: []StateRef{browserStateRef()},
		}
	}

	return CallStateTraits{}
}
