package session

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Every persistence path must leave the durable-marker invariant intact:
// on-disk InProgress=true ⟺ a marker file exists. Store.Save maintains it;
// the Patch* read-modify-write paths must re-assert it for whatever
// InProgress value they persist, or a patch racing a completion save can
// strand a session as "interrupted forever with no marker" (never recovered,
// never cleaned) or leave a stale marker behind.
func TestPatchPathsReassertInterruptedMarkerInvariant(t *testing.T) {
	now := time.Now()

	patches := []struct {
		name  string
		apply func(s *Store, id string) error
	}{
		{"PatchTitle", func(s *Store, id string) error { return s.PatchTitle(id, "t") }},
		{"PatchAutoTitle", func(s *Store, id string) error {
			_, err := s.PatchAutoTitle(id, "t", 99)
			return err
		}},
		{"PatchFlags", func(s *Store, id string) error {
			pinned := true
			return s.PatchFlags(id, &pinned, nil)
		}},
		{"PatchPublishedShares", func(s *Store, id string) error {
			return s.PatchPublishedShares(id, func(e []PublishedShareEntry) []PublishedShareEntry { return e })
		}},
		{"PatchSummaryCache", func(s *Store, id string) error { return s.PatchSummaryCache(id, "sum", "key") }},
	}

	for _, tc := range patches {
		t.Run(tc.name+"_restores_missing_marker", func(t *testing.T) {
			dir := t.TempDir()
			id := "inprogress-no-marker"
			writeLegacySessionWithoutMarker(t, dir, &Session{
				SchemaVersion:   1,
				ID:              id,
				CreatedAt:       now,
				UpdatedAt:       now,
				TitleAuto:       true,
				InProgress:      true,
				InterruptedTurn: &InterruptedTurn{Source: "desktop", UpdatedAt: now},
			})
			store := NewStore(dir)
			defer store.Close()
			if err := tc.apply(store, id); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			marker := filepath.Join(dir, interruptedMarkerDirName, id+interruptedMarkerSuffix)
			if _, err := os.Stat(marker); err != nil {
				t.Fatalf("%s persisted InProgress=true but left no marker: %v", tc.name, err)
			}
		})

		t.Run(tc.name+"_clears_stale_marker", func(t *testing.T) {
			dir := t.TempDir()
			id := "complete-with-marker"
			writeLegacySessionWithoutMarker(t, dir, &Session{
				SchemaVersion: 1,
				ID:            id,
				CreatedAt:     now,
				UpdatedAt:     now,
				TitleAuto:     true,
			})
			store := NewStore(dir)
			defer store.Close()
			if err := store.writeInterruptedMarker(id); err != nil {
				t.Fatal(err)
			}
			if err := tc.apply(store, id); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			marker := filepath.Join(dir, interruptedMarkerDirName, id+interruptedMarkerSuffix)
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("%s persisted InProgress=false but left a stale marker (err=%v)", tc.name, err)
			}
		})
	}
}
