package agent

import "testing"

func TestResolveCallStateTraits(t *testing.T) {
	t.Run("browser observation reads browser state", func(t *testing.T) {
		traits := resolveCallStateTraits("browser_snapshot", `{}`)
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

	t.Run("bash writes process session state", func(t *testing.T) {
		traits := resolveCallStateTraits("bash", `{"command":"pwd"}`)
		if len(traits.Writes) != 1 || traits.Writes[0].Domain != StateDomainProcess {
			t.Fatalf("unexpected bash traits: %+v", traits)
		}
	})
}

// A tracked write must move the shape-context fingerprint to a new generation
// so tree-result shaping never treats a post-write observation as a repeat of
// the pre-write one.
func TestShapeContextKeyChangesAfterVersionBump(t *testing.T) {
	tracker := newStateVersionTracker()
	traits := resolveCallStateTraits("file_read", `{"path":"/tmp/example.txt"}`)

	before := shapeContextKey("file_read", traits, tracker)
	tracker.bump([]StateRef{{Domain: StateDomainFilesystem, Scope: "/tmp/example.txt"}})
	after := shapeContextKey("file_read", traits, tracker)
	if before == after {
		t.Fatalf("expected shape-context key to change after version bump, got %q", before)
	}
}
