package agent

import (
	"context"
	"encoding/json"
	"testing"
)

func TestResolveCallStateTraits(t *testing.T) {
	t.Run("browser read is cacheable", func(t *testing.T) {
		traits := resolveCallStateTraits("browser_snapshot", `{}`)
		if !traits.Cacheable {
			t.Fatal("expected browser_snapshot to be cacheable")
		}
		if len(traits.Reads) != 1 || traits.Reads[0].Domain != StateDomainBrowser {
			t.Fatalf("expected browser read traits, got %+v", traits)
		}
	})

	t.Run("file write tracks filesystem scope", func(t *testing.T) {
		traits := resolveCallStateTraits("file_write", `{"path":"/tmp/example.txt","content":"x"}`)
		if len(traits.Writes) != 1 {
			t.Fatalf("expected one filesystem write ref, got %+v", traits)
		}
		if traits.Writes[0].Domain != StateDomainFilesystem || traits.Writes[0].Scope != "/tmp/example.txt" {
			t.Fatalf("unexpected file write traits: %+v", traits)
		}
	})

	t.Run("bash is unknown write", func(t *testing.T) {
		traits := resolveCallStateTraits("bash", `{"command":"pwd"}`)
		if !traits.UnknownWrite {
			t.Fatal("expected bash to be treated as an unknown write")
		}
		if len(traits.Writes) != 1 || traits.Writes[0].Domain != StateDomainProcess {
			t.Fatalf("unexpected bash traits: %+v", traits)
		}
	})
}

func TestBuildStateAwareCacheKeyChangesAfterVersionBump(t *testing.T) {
	tracker := newStateVersionTracker()
	traits := resolveCallStateTraits("file_read", `{"path":"/tmp/example.txt"}`)

	before := buildStateAwareCacheKey("file_read", json.RawMessage(`{"path":"/tmp/example.txt"}`), traits, tracker)
	if before == "" {
		t.Fatal("expected initial cache key")
	}

	tracker.bump([]StateRef{{Domain: StateDomainFilesystem, Scope: "/tmp/example.txt"}})
	after := buildStateAwareCacheKey("file_read", json.RawMessage(`{"path":"/tmp/example.txt"}`), traits, tracker)
	if after == "" {
		t.Fatal("expected post-write cache key")
	}
	if before == after {
		t.Fatalf("expected cache key to change after version bump, got %q", before)
	}
}

func TestStatefulGUIObservationsBypassFallbackReadCache(t *testing.T) {
	for _, name := range []string{"computer_use", "accessibility"} {
		t.Run(name, func(t *testing.T) {
			tool := statefulReadCacheProbe{name: name}
			traits := resolveFallbackReadStateTraits(tool, `{"action":"get_app_state"}`)
			if traits.Cacheable || len(traits.Reads) != 0 {
				t.Fatalf("stateful GUI observation entered fallback read cache: %+v", traits)
			}
			if !tool.IsReadOnlyCall(`{"action":"get_app_state"}`) {
				t.Fatal("cache bypass must not remove read-only/safe classification")
			}
		})
	}

	ordinary := statefulReadCacheProbe{name: "ordinary_read"}
	traits := resolveFallbackReadStateTraits(ordinary, `{}`)
	if traits.Cacheable || len(traits.Reads) != 0 {
		t.Fatalf("dynamic read-only fallback entered cross-iteration cache: %+v", traits)
	}

	stable := stableReadCacheProbe{statefulReadCacheProbe{name: "stable_read"}}
	traits = resolveFallbackReadStateTraits(stable, `{}`)
	if !traits.Cacheable || len(traits.Reads) != 1 {
		t.Fatalf("explicitly stable read did not enter cross-iteration cache: %+v", traits)
	}
}

func TestEnforceCrossIterationCacheContractRejectsDynamicNamedReads(t *testing.T) {
	for _, name := range []string{"browser_snapshot", "file_read"} {
		traits := enforceCrossIterationCacheContract(
			statefulReadCacheProbe{name: name},
			`{}`,
			resolveCallStateTraits(name, `{}`),
		)
		if traits.Cacheable {
			t.Fatalf("%s remained cacheable without explicit stability contract", name)
		}
	}
}

type statefulReadCacheProbe struct {
	name string
}

func (tool statefulReadCacheProbe) Info() ToolInfo {
	return ToolInfo{Name: tool.name}
}

func (statefulReadCacheProbe) Run(context.Context, string) (ToolResult, error) {
	return ToolResult{}, nil
}

func (statefulReadCacheProbe) RequiresApproval() bool { return false }

func (statefulReadCacheProbe) IsReadOnlyCall(string) bool { return true }

type stableReadCacheProbe struct {
	statefulReadCacheProbe
}

func (stableReadCacheProbe) CacheAcrossIterations(string) bool { return true }
