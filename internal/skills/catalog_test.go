package skills

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testCatalogEntry(slug string) CatalogEntry {
	return CatalogEntry{
		ID:          "official:" + slug,
		Slug:        slug,
		Source:      "official",
		DisplayName: "Test " + slug,
		Description: "Reviewed capability",
		Version:     "1",
		Installable: true,
		Installation: CatalogInstallation{
			Provider:      "github_archive",
			Repository:    "https://github.com/example/skills",
			Ref:           strings.Repeat("a", 40),
			ArtifactPath:  "skills/" + slug,
			ArchiveSHA256: strings.Repeat("b", 64),
		},
		Recommendation: RecommendationMetadata{
			Eligible: true, IntentTags: []string{"new.task"}, Surfaces: []string{"desktop"}, MaxBundleSize: 3,
		},
	}
}

func registryProviderForBody(t *testing.T, body *atomic.Value) (CatalogProvider, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body.Load().([]byte))
	}))
	client := NewMarketplaceClient(server.URL, 0)
	return NewRegistryCatalogProvider(client, NewEmbeddedCatalogProvider(""), true), server
}

func TestRegistryCatalogProviderAcceptsNewReviewedMetadataWithoutDaemonRebuild(t *testing.T) {
	dir := t.TempDir()
	tarball := fakeTarball(t, "skills-"+strings.Repeat("a", 40), "new-capability")
	entry := testCatalogEntry("new-capability")
	entry.Installation = pinnedTestInstallation(tarball, entry.Slug)
	index, _ := json.Marshal(RegistryIndex{Version: 1, InstallableCapabilities: []CatalogEntry{entry}})
	var body atomic.Value
	body.Store(index)
	provider, server := registryProviderForBody(t, &body)
	defer server.Close()

	entries, revision, err := InstallableCatalogFrom(context.Background(), provider, dir, "desktop")
	if err != nil || !strings.HasPrefix(revision, "sha256:") || len(entries) != 1 || entries[0].ID != entry.ID {
		t.Fatalf("entries=%+v revision=%q err=%v", entries, revision, err)
	}
	original := openRepoTarball
	t.Cleanup(func() { openRepoTarball = original })
	openRepoTarball = func(_ context.Context, repository, ref string) (io.ReadCloser, error) {
		if repository != entry.Installation.Repository || ref != entry.Installation.Ref {
			t.Fatalf("installer requested repository=%q ref=%q", repository, ref)
		}
		return tarballReader(tarball), nil
	}
	if err := InstallSkillFromCatalog(context.Background(), dir, entry.Slug, provider); err != nil {
		t.Fatalf("install dynamic catalog entry: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "skills", entry.Slug, "SKILL.md"))
	if err != nil || !strings.Contains(string(data), "name: "+entry.Slug) {
		t.Fatalf("installed SKILL.md data=%q err=%v", data, err)
	}
	receipt, ok, err := ReadCatalogInstallReceipt(filepath.Join(dir, "skills", entry.Slug))
	if err != nil || !ok || receipt.DescriptorDigest == "" || receipt.TreeSHA256 == "" {
		t.Fatalf("catalog receipt=%+v ok=%v err=%v", receipt, ok, err)
	}
	unattached, _, err := RecommendationCatalogFrom(context.Background(), provider, dir, "desktop", map[string]bool{})
	if err != nil || len(unattached) != 1 || unattached[0].ID != entry.ID {
		t.Fatalf("installed but unattached capability not discoverable: entries=%+v err=%v", unattached, err)
	}
	visible, _, err := RecommendationCatalogFrom(context.Background(), provider, dir, "desktop", map[string]bool{entry.Slug: true})
	if err != nil || len(visible) != 0 {
		t.Fatalf("already-visible capability was recommended: entries=%+v err=%v", visible, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "skills", entry.Slug, "SKILL.md"), append(data, []byte("\ntampered")...), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := InstallCatalogEntry(context.Background(), dir, entry, provider); err == nil {
		t.Fatal("tampered installed tree was accepted as an idempotent catalog install")
	}
}

func TestRegistryCatalogProviderKeepsLastTrustedSnapshotOnBadRefresh(t *testing.T) {
	entry := testCatalogEntry("last-good")
	good, _ := json.Marshal(RegistryIndex{Version: 1, InstallableCapabilities: []CatalogEntry{entry}})
	var body atomic.Value
	body.Store(good)
	provider, server := registryProviderForBody(t, &body)
	defer server.Close()
	first, firstRevision, err := provider.Catalog(context.Background())
	if err != nil || len(first) != 1 {
		t.Fatalf("prime catalog=%+v err=%v", first, err)
	}
	badEntry := entry
	badEntry.Recommendation.Surfaces = []string{"unknown-surface"}
	bad, _ := json.Marshal(RegistryIndex{Version: 1, InstallableCapabilities: []CatalogEntry{badEntry}})
	body.Store(bad)
	second, secondRevision, err := provider.Catalog(context.Background())
	if err != nil || len(second) != 1 || second[0].ID != entry.ID || secondRevision != firstRevision {
		t.Fatalf("last-good lost entries=%+v revision=%q err=%v", second, secondRevision, err)
	}
}

func TestRegistryCatalogProviderDoesNotTrustArbitraryMarketplaceURL(t *testing.T) {
	entry := testCatalogEntry("untrusted")
	index, _ := json.Marshal(RegistryIndex{Version: 1, InstallableCapabilities: []CatalogEntry{entry}})
	var body atomic.Value
	body.Store(index)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(index) }))
	defer server.Close()
	provider := NewRegistryCatalogProvider(NewMarketplaceClient(server.URL, time.Hour), NewEmbeddedCatalogProvider(""), false)
	entries, _, err := provider.Catalog(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, got := range entries {
		if got.ID == entry.ID {
			t.Fatal("untrusted registry nominated an official recommendation")
		}
	}
}

func TestRegistryCatalogMarketplaceRecommendationRequiresExplicitEligibility(t *testing.T) {
	dir := t.TempDir()
	entry := testCatalogEntry("reviewed-marketplace")
	entry.ID = "marketplace:reviewed-marketplace"
	entry.Source = "marketplace"
	entry.Recommendation.Eligible = false // JSON default is fail-closed.
	index, _ := json.Marshal(RegistryIndex{Version: 1, InstallableCapabilities: []CatalogEntry{entry}})
	var body atomic.Value
	body.Store(index)
	provider, server := registryProviderForBody(t, &body)
	defer server.Close()
	entries, _, err := InstallableCatalogFrom(context.Background(), provider, dir, "desktop")
	if err != nil || len(entries) != 0 {
		t.Fatalf("default-ineligible marketplace entry leaked: entries=%+v err=%v", entries, err)
	}
	entry.Recommendation.Eligible = true // explicit controlled-review decision.
	index, _ = json.Marshal(RegistryIndex{Version: 1, InstallableCapabilities: []CatalogEntry{entry}})
	body.Store(index)
	entries, _, err = InstallableCatalogFrom(context.Background(), provider, dir, "desktop")
	if err != nil || len(entries) != 1 || entries[0].ID != entry.ID {
		t.Fatalf("reviewed marketplace entry unavailable: entries=%+v err=%v", entries, err)
	}
}

func TestValidateOfficialCatalogRejectsMalformedEntries(t *testing.T) {
	base := testCatalogEntry("valid-fixture")
	tests := map[string]func(*CatalogEntry){
		"duplicate id":       func(e *CatalogEntry) {},
		"duplicate slug":     func(e *CatalogEntry) {},
		"invalid slug":       func(e *CatalogEntry) { e.Slug = "../bad" },
		"unknown source":     func(e *CatalogEntry) { e.Source = "random" },
		"empty display":      func(e *CatalogEntry) { e.DisplayName = "" },
		"empty description":  func(e *CatalogEntry) { e.Description = "" },
		"empty version":      func(e *CatalogEntry) { e.Version = "" },
		"unknown surface":    func(e *CatalogEntry) { e.Recommendation.Surfaces = []string{"watch"} },
		"unknown provider":   func(e *CatalogEntry) { e.Installation.Provider = "magic" },
		"missing repository": func(e *CatalogEntry) { e.Installation.Repository = "" },
		"missing ref":        func(e *CatalogEntry) { e.Installation.Ref = "" },
		"missing artifact":   func(e *CatalogEntry) { e.Installation.ArtifactPath = "" },
		"absolute artifact":  func(e *CatalogEntry) { e.Installation.ArtifactPath = "/skills/valid-fixture" },
		"missing integrity":  func(e *CatalogEntry) { e.Installation.ArchiveSHA256 = "" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			entry := base
			mutate(&entry)
			entries := []CatalogEntry{entry}
			if name == "duplicate id" {
				duplicate := entry
				duplicate.Slug = "other-fixture"
				duplicate.Installation.ArtifactPath = "skills/other-fixture"
				entries = append(entries, duplicate)
			} else if name == "duplicate slug" {
				duplicate := entry
				duplicate.ID = "official:duplicate-id"
				entries = append(entries, duplicate)
			}
			if err := validateOfficialCatalog(entries); err == nil {
				t.Fatalf("malformed catalog admitted: %+v", entries)
			}
		})
	}
}

// An occupied destination is never the installer's to clear. Installs stage
// under skills/.staging and commit by rename, so an interrupted install leaves
// nothing at the destination — anything there is user content, whether or not it
// has a SKILL.md yet (a half-authored skill is indistinguishable from
// "leftovers"). Both shapes must survive untouched and surface as a recoverable
// conflict, so the daemon answers 409 with the path instead of a bare 500.
func TestInstallCatalogEntryPreservesOccupiedDestinationAsRecoverableConflict(t *testing.T) {
	dir := t.TempDir()
	tarball := fakeTarball(t, "skills-"+strings.Repeat("b", 40), "half-authored", "hand-made")
	original := openRepoTarball
	t.Cleanup(func() { openRepoTarball = original })
	openRepoTarball = func(context.Context, string, string) (io.ReadCloser, error) {
		return tarballReader(tarball), nil
	}

	for _, tc := range []struct {
		name, slug, file, body string
	}{
		{"no SKILL.md yet", "half-authored", "notes.md", "my draft"},
		{"manual install", "hand-made", "SKILL.md", "---\nname: hand-made\ndescription: mine\n---\nbody\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			entry := testCatalogEntry(tc.slug)
			entry.Installation = pinnedTestInstallation(tarball, entry.Slug)
			skillDir := filepath.Join(dir, "skills", entry.Slug)
			if err := os.MkdirAll(skillDir, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(skillDir, tc.file), []byte(tc.body), 0600); err != nil {
				t.Fatal(err)
			}
			_, err := InstallCatalogEntry(context.Background(), dir, entry, nil)
			if !errors.Is(err, ErrSkillInstallConflict) {
				t.Fatalf("want ErrSkillInstallConflict, got %v", err)
			}
			if !strings.Contains(err.Error(), skillDir) {
				t.Fatalf("conflict error does not name the path the user must clear: %v", err)
			}
			data, readErr := os.ReadFile(filepath.Join(skillDir, tc.file))
			if readErr != nil || string(data) != tc.body {
				t.Fatalf("installer modified user content: data=%q err=%v", data, readErr)
			}
		})
	}
}
