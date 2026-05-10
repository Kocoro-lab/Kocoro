//go:build localsmoke

// Run with:
//   LOCAL_TLM_BUNDLE=/tmp/tlm-cap-root \
//     go test -tags localsmoke -v -timeout 120s -run TestMemoryBlockLocalSmoke ./internal/memory/
//
// LOCAL_TLM_BUNDLE must point at a bundle root directory that contains a
// `current` symlink → `bundles/<ts>/` with a `.commit` marker inside the
// active bundle. See internal/memory/testdata/README.md for setup.
//
// LOCAL_TLM_PATH overrides the tlm binary path (default: PATH lookup).
//
// Build-tag inventory for this package:
//   default     — pure unit tests, no live sidecar, no Cloud.
//   localsmoke  — this file. Spawns the real tlm sidecar against a local
//                 bundle. No Cloud dependency.
//   dogfood     — see dogfood_live_test.go. Spawns the sidecar AND pulls
//                 a bundle from Shannon Cloud; requires API key + endpoint.

package memory

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestMemoryBlockLocalSmoke(t *testing.T) {
	bundleRoot := os.Getenv("LOCAL_TLM_BUNDLE")
	if bundleRoot == "" {
		t.Skip("LOCAL_TLM_BUNDLE unset")
	}
	if _, err := os.Lstat(filepath.Join(bundleRoot, "current")); err != nil {
		t.Fatalf("LOCAL_TLM_BUNDLE/current must exist (see testdata/README.md): %v", err)
	}

	socket := filepath.Join(t.TempDir(), "smoke.sock")
	cfg := Config{
		SocketPath: socket,
		BundleRoot: bundleRoot,
		TLMPath:    os.Getenv("LOCAL_TLM_PATH"), // empty → PATH lookup
	}

	sc := NewSidecar(cfg, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	if err := sc.Spawn(ctx); err != nil {
		t.Fatalf("spawn: %v", err)
	}
	defer func() { _ = sc.Shutdown(5 * time.Second) }()

	if err := sc.WaitReady(ctx, 60*time.Second); err != nil {
		t.Fatalf("waitReady: %v", err)
	}

	client := NewClient(socket, 10*time.Second)

	t.Run("direct_relation_has_via_relations", func(t *testing.T) {
		env, _, err := client.Query(ctx, QueryIntent{
			Mode:                ModeDirectRelation,
			AnchorMentions:      []string{"Alice Nakamura"},
			RelationConstraints: []string{"created"},
			TargetSlot:          "tail",
			EvidenceBudget:      4,
			ResultLimit:         5,
		})
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		if env.MemoryBlock == nil || len(env.MemoryBlock.Groups) == 0 {
			t.Fatalf("expected non-empty memory_block.groups, got %+v", env.MemoryBlock)
		}
		if len(env.MemoryBlock.Groups[0].ViaRelations) == 0 {
			t.Fatalf("expected via_relations populated for direct_relation, got group=%+v", env.MemoryBlock.Groups[0])
		}
	})

	t.Run("path_query_has_observed_path", func(t *testing.T) {
		env, _, err := client.Query(ctx, QueryIntent{
			Mode:                ModePathQuery,
			AnchorMentions:      []string{"Jordan Sato"},
			RelationConstraints: []string{"collaborated_with^-1", "created"},
			EvidenceBudget:      4,
			ResultLimit:         5,
		})
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		if env.MemoryBlock == nil || len(env.MemoryBlock.Groups) == 0 {
			t.Fatalf("expected non-empty memory_block.groups for path query, got %+v", env.MemoryBlock)
		}
		op := env.MemoryBlock.Groups[0].ObservedPath
		if len(op) == 0 {
			t.Fatalf("expected observed_path populated, got group=%+v", env.MemoryBlock.Groups[0])
		}
		for i, h := range op {
			if h.Relation == "" {
				t.Fatalf("hop[%d] missing relation: %+v", i, h)
			}
			if h.Direction != "forward" && h.Direction != "inverse" {
				t.Fatalf("hop[%d] direction must be forward|inverse, got %q", i, h.Direction)
			}
			if h.FromLabel == "" || h.ToLabel == "" {
				t.Fatalf("hop[%d] missing labels: %+v", i, h)
			}
		}
	})

	t.Run("no_data_sets_no_data_reason", func(t *testing.T) {
		env, _, err := client.Query(ctx, QueryIntent{
			Mode:                ModeDirectRelation,
			AnchorMentions:      []string{"NonExistentEntity-zzzzz"},
			RelationConstraints: []string{"created"},
			TargetSlot:          "tail",
			EvidenceBudget:      4,
			ResultLimit:         5,
		})
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		if env.MemoryBlock == nil {
			t.Fatal("expected memory_block present even on no-data, got nil")
		}
		if env.MemoryBlock.NoDataReason == nil {
			t.Fatal("expected no_data_reason set on empty result")
		}
		if len(env.MemoryBlock.Groups) != 0 {
			t.Fatalf("expected empty groups on no-data, got %d", len(env.MemoryBlock.Groups))
		}
	})
}
