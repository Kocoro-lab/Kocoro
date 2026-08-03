package skills

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

// RecommendationMetadata is reviewed catalog data. It is intentionally not
// populated from SKILL.md frontmatter or marketplace text.
type RecommendationMetadata struct {
	Eligible      bool     `json:"eligible"`
	IntentTags    []string `json:"intent_tags"`
	Surfaces      []string `json:"surfaces"`
	MaxBundleSize int      `json:"max_bundle_size"`
}

// CatalogEntry is the stable installable-capability contract. ID is opaque to
// callers; slug remains the legacy installer key.
type CatalogEntry struct {
	ID             string                 `json:"id"`
	Slug           string                 `json:"slug"`
	Source         string                 `json:"source"`
	DisplayName    string                 `json:"display_name"`
	Description    string                 `json:"description"`
	Version        string                 `json:"version"`
	Installable    bool                   `json:"installable"`
	Installation   CatalogInstallation    `json:"installation,omitempty"`
	Recommendation RecommendationMetadata `json:"recommendation"`
}

type CatalogInstallation struct {
	Provider      string `json:"provider"`
	Repository    string `json:"repository,omitempty"`
	Ref           string `json:"ref,omitempty"`
	ArtifactPath  string `json:"artifact_path,omitempty"`
	ArchiveSHA256 string `json:"archive_sha256,omitempty"`
}

// CatalogInstallDescriptorDigest binds a recommendation and its eventual
// install receipt to the exact immutable catalog artifact. Display text is
// deliberately excluded: publishers may improve copy without changing the
// bytes the user authorized the daemon to install.
func CatalogInstallDescriptorDigest(entry CatalogEntry) (string, error) {
	canonical := struct {
		ID           string              `json:"id"`
		Slug         string              `json:"slug"`
		Source       string              `json:"source"`
		Version      string              `json:"version"`
		Installation CatalogInstallation `json:"installation"`
	}{entry.ID, entry.Slug, entry.Source, entry.Version, entry.Installation}
	b, err := json.Marshal(canonical)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("sha256:%x", sha256.Sum256(b)), nil
}

//go:embed official_catalog.json
var officialCatalogJSON []byte

var (
	officialCatalogOnce sync.Once
	officialCatalog     []CatalogEntry
	officialCatalogErr  error
)

func OfficialCatalog() ([]CatalogEntry, error) {
	entries, _, err := OfficialCatalogAt("")
	return entries, err
}

// CatalogProvider is the controlled source of installable-capability metadata.
// Providers never infer recommendation eligibility from SKILL.md text.
type CatalogProvider interface {
	Catalog(context.Context) ([]CatalogEntry, string, error)
}

// CatalogArtifactProvider is an optional provider seam used by controlled
// transports that already own artifact delivery. Production registry entries
// normally use the built-in HTTPS GitHub archive fetcher; tests and future
// signed transports can supply the exact verified bytes without mutating a
// process-global downloader.
type CatalogArtifactProvider interface {
	OpenCatalogArtifact(context.Context, CatalogInstallation) (io.ReadCloser, error)
}

type officialCatalogProvider struct{ shannonDir string }

// OfficialCatalogAt retains the legacy embedded-catalog API. Live production
// refreshes are owned by NewRegistryCatalogProvider; this function is the
// binary-pinned fallback and intentionally does not trust local JSON files.
func OfficialCatalogAt(shannonDir string) ([]CatalogEntry, string, error) {
	return officialCatalogProvider{shannonDir: shannonDir}.Catalog(context.Background())
}

func (p officialCatalogProvider) Catalog(context.Context) ([]CatalogEntry, string, error) {
	data := officialCatalogJSON
	entries, err := parseOfficialCatalog(data)
	if err != nil {
		return nil, "", err
	}
	revision := fmt.Sprintf("sha256:%x", sha256.Sum256(data))
	return entries, revision, nil
}

// NewEmbeddedCatalogProvider returns the binary-pinned fallback catalog. It is
// used when the controlled registry is unavailable or has not published the
// optional installable_capabilities field yet.
func NewEmbeddedCatalogProvider(shannonDir string) CatalogProvider {
	return officialCatalogProvider{shannonDir: shannonDir}
}

// registryCatalogProvider reuses the existing shanclaw-skill-registry index
// transport, TTL cache, retry, and stale-on-error behavior. Only a caller that
// has authenticated the registry URL as controlled may set trusted=true;
// arbitrary operator marketplace URLs remain browse/install-only and cannot
// nominate recommendation candidates.
type registryCatalogProvider struct {
	registry *MarketplaceClient
	fallback CatalogProvider
	trusted  bool

	mu           sync.RWMutex
	lastEntries  []CatalogEntry
	lastRevision string
}

func NewRegistryCatalogProvider(registry *MarketplaceClient, fallback CatalogProvider, trusted bool) CatalogProvider {
	if fallback == nil {
		fallback = NewEmbeddedCatalogProvider("")
	}
	return &registryCatalogProvider{registry: registry, fallback: fallback, trusted: trusted}
}

func (p *registryCatalogProvider) Catalog(ctx context.Context) ([]CatalogEntry, string, error) {
	if p == nil {
		return nil, "", fmt.Errorf("catalog provider is nil")
	}
	if !p.trusted || p.registry == nil {
		return p.fallback.Catalog(ctx)
	}
	idx, err := p.registry.Load(ctx)
	if err == nil && idx != nil && len(idx.InstallableCapabilities) > 0 {
		entries := cloneCatalogEntries(idx.InstallableCapabilities)
		if validateErr := validateOfficialCatalog(entries); validateErr == nil {
			data, marshalErr := json.Marshal(entries)
			if marshalErr == nil {
				revision := fmt.Sprintf("sha256:%x", sha256.Sum256(data))
				p.mu.Lock()
				p.lastEntries = cloneCatalogEntries(entries)
				p.lastRevision = revision
				p.mu.Unlock()
				return entries, revision, nil
			}
		}
	}
	// Invalid or unavailable updates never replace the last trusted snapshot.
	// The cache swap is atomic under p.mu; on a cold start the embedded catalog
	// remains the fail-closed fallback.
	p.mu.RLock()
	lastEntries := cloneCatalogEntries(p.lastEntries)
	lastRevision := p.lastRevision
	p.mu.RUnlock()
	if len(lastEntries) > 0 {
		return lastEntries, lastRevision, nil
	}
	return p.fallback.Catalog(ctx)
}

func cloneCatalogEntries(entries []CatalogEntry) []CatalogEntry {
	out := make([]CatalogEntry, len(entries))
	copy(out, entries)
	for i := range out {
		out[i].Recommendation.IntentTags = append([]string(nil), entries[i].Recommendation.IntentTags...)
		out[i].Recommendation.Surfaces = append([]string(nil), entries[i].Recommendation.Surfaces...)
	}
	return out
}

func parseOfficialCatalog(data []byte) ([]CatalogEntry, error) {
	// Retain the once-parsed embedded fallback for normal startup. Registry
	// snapshots are validated before reaching this helper and remain provider-
	// owned so an arbitrary local JSON file cannot become official metadata.
	if bytes.Equal(data, officialCatalogJSON) {
		officialCatalogOnce.Do(func() {
			officialCatalogErr = json.Unmarshal(officialCatalogJSON, &officialCatalog)
			if officialCatalogErr != nil {
				return
			}
			officialCatalogErr = validateOfficialCatalog(officialCatalog)
		})
		if officialCatalogErr != nil {
			return nil, officialCatalogErr
		}
		return append([]CatalogEntry(nil), officialCatalog...), nil
	}
	var entries []CatalogEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, err
	}
	if err := validateOfficialCatalog(entries); err != nil {
		return nil, err
	}
	return entries, nil
}

func validateOfficialCatalog(entries []CatalogEntry) error {
	seenIDs := map[string]bool{}
	seenSlugs := map[string]bool{}
	for _, entry := range entries {
		if !validCatalogSource(entry.Source) || !validCatalogID(entry.ID, entry.Source) || ValidateSkillName(entry.Slug) != nil || !entry.Installable || seenIDs[entry.ID] || seenSlugs[entry.Slug] || IsBuiltinSkill(entry.Slug) ||
			!validCatalogText(entry.DisplayName, 80) || !validCatalogText(entry.Description, 280) || !validCatalogText(entry.Version, 64) {
			return fmt.Errorf("invalid official skill catalog entry %q", entry.ID)
		}
		for _, surface := range entry.Recommendation.Surfaces {
			if surface != "desktop" {
				return fmt.Errorf("unknown recommendation surface %q for %q", surface, entry.ID)
			}
		}
		if entry.Recommendation.Eligible {
			if entry.Recommendation.MaxBundleSize < 1 || entry.Recommendation.MaxBundleSize > 5 || len(entry.Recommendation.IntentTags) == 0 || len(entry.Recommendation.IntentTags) > 32 || len(entry.Recommendation.Surfaces) == 0 || len(entry.Recommendation.Surfaces) > 8 {
				return fmt.Errorf("invalid recommendation metadata for %q", entry.ID)
			}
			for _, value := range append(append([]string(nil), entry.Recommendation.IntentTags...), entry.Recommendation.Surfaces...) {
				if !validCatalogText(value, 80) {
					return fmt.Errorf("invalid recommendation metadata for %q", entry.ID)
				}
			}
		}
		switch entry.Installation.Provider {
		case "bundled":
			if entry.Installation.ArtifactPath != "skills/"+entry.Slug || entry.Installation.Repository != "" || entry.Installation.Ref != "" || entry.Installation.ArchiveSHA256 != "" {
				return fmt.Errorf("invalid bundled installation metadata for %q", entry.ID)
			}
		case "github_archive":
			if !validPinnedGitHubInstallation(entry) {
				return fmt.Errorf("invalid pinned installation metadata for %q", entry.ID)
			}
		default:
			return fmt.Errorf("unsupported installation provider %q for %q", entry.Installation.Provider, entry.ID)
		}
		seenIDs[entry.ID] = true
		seenSlugs[entry.Slug] = true
	}
	return nil
}

func validCatalogSource(source string) bool {
	return source == "official" || source == "marketplace"
}

func validCatalogID(id, source string) bool {
	prefix := source + ":"
	if source == "" || !strings.HasPrefix(id, prefix) {
		return false
	}
	return ValidateSkillName(strings.TrimPrefix(id, prefix)) == nil
}

func validCatalogText(value string, maxRunes int) bool {
	if value == "" || strings.TrimSpace(value) != value || !utf8.ValidString(value) || utf8.RuneCountInString(value) > maxRunes {
		return false
	}
	for _, r := range value {
		if unicode.Is(unicode.Cc, r) || unicode.Is(unicode.Cf, r) {
			return false
		}
	}
	return true
}

func validPinnedGitHubInstallation(entry CatalogEntry) bool {
	u, err := url.Parse(entry.Installation.Repository)
	if err != nil || u.Scheme != "https" || u.Host != "github.com" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	repoPath := strings.TrimSuffix(strings.TrimPrefix(u.EscapedPath(), "/"), ".git")
	parts := strings.Split(repoPath, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" || strings.Contains(entry.Installation.Repository, "%") {
		return false
	}
	artifact := path.Clean(entry.Installation.ArtifactPath)
	return validLowerHex(entry.Installation.Ref, 40) && validLowerHex(entry.Installation.ArchiveSHA256, 64) &&
		artifact == entry.Installation.ArtifactPath && artifact != "." && !strings.HasPrefix(artifact, "/") && !strings.HasPrefix(artifact, "../") && path.Base(artifact) == entry.Slug
}

func validLowerHex(value string, size int) bool {
	if len(value) != size || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

// InstallableCatalog is the single runtime read path for recommendation.
// Marketplace entries are deliberately absent until a separately trusted
// provider supplies reviewed metadata with Eligible=true. V1's explicit
// publisher trust decision is the compiled-in official GitHub registry URL
// over TLS; there is no separate catalog-signing key in this protocol version.
func InstallableCatalog(shannonDir string, surface string) ([]CatalogEntry, error) {
	entries, _, err := InstallableCatalogWithRevision(shannonDir, surface)
	return entries, err
}

func InstallableCatalogWithRevision(shannonDir string, surface string) ([]CatalogEntry, string, error) {
	return InstallableCatalogFrom(context.Background(), NewEmbeddedCatalogProvider(shannonDir), shannonDir, surface)
}

func InstallableCatalogFrom(ctx context.Context, provider CatalogProvider, shannonDir string, surface string) ([]CatalogEntry, string, error) {
	return RecommendationCatalogFrom(ctx, provider, shannonDir, surface, nil)
}

// RecommendationCatalogFrom applies catalog eligibility to the Skills visible
// to the current Agent. A catalog-installed Skill that is global but not
// attached/enabled remains a valid candidate; a legacy or modified install
// without the matching catalog receipt is skipped rather than overwritten.
// A nil visible set preserves the legacy global-uninstalled behavior.
func RecommendationCatalogFrom(ctx context.Context, provider CatalogProvider, shannonDir string, surface string, visible map[string]bool) ([]CatalogEntry, string, error) {
	if provider == nil {
		provider = NewEmbeddedCatalogProvider(shannonDir)
	}
	entries, revision, err := provider.Catalog(ctx)
	if err != nil {
		return nil, "", err
	}
	out := make([]CatalogEntry, 0, len(entries))
	for _, entry := range entries {
		if IsBuiltinSkill(entry.Slug) || !entry.Installable || !entry.Recommendation.Eligible || !contains(entry.Recommendation.Surfaces, surface) {
			continue
		}
		skillDir := filepath.Join(shannonDir, "skills", entry.Slug)
		if _, err := os.Stat(filepath.Join(skillDir, "SKILL.md")); err == nil {
			if visible == nil || visible[entry.Slug] {
				continue
			}
			receipt, ok, receiptErr := ReadCatalogInstallReceipt(skillDir)
			digest, digestErr := CatalogInstallDescriptorDigest(entry)
			if receiptErr != nil || digestErr != nil || !ok || receipt.DescriptorDigest != digest {
				continue
			}
		} else if !os.IsNotExist(err) {
			return nil, "", fmt.Errorf("read installed skill %q: %w", entry.Slug, err)
		}
		out = append(out, entry)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, revision, nil
}

// CatalogEntryForSlug is the installation admission lookup. It deliberately
// reads catalog metadata rather than a parallel allowlist, so adding a reviewed
// official entry does not require a daemon code change.
func CatalogEntryForSlug(slug string) (CatalogEntry, bool) {
	return CatalogEntryForSlugAt("", slug)
}
func CatalogEntryForSlugAt(shannonDir, slug string) (CatalogEntry, bool) {
	entry, ok, _ := CatalogEntryForSlugFrom(context.Background(), NewEmbeddedCatalogProvider(shannonDir), slug)
	return entry, ok
}

func CatalogEntryForSlugFrom(ctx context.Context, provider CatalogProvider, slug string) (CatalogEntry, bool, error) {
	if provider == nil {
		provider = NewEmbeddedCatalogProvider("")
	}
	entries, _, err := provider.Catalog(ctx)
	if err != nil {
		return CatalogEntry{}, false, err
	}
	for _, entry := range entries {
		if entry.Slug == slug && entry.Installable && validCatalogSource(entry.Source) {
			return entry, true, nil
		}
	}
	return CatalogEntry{}, false, nil
}

func contains(values []string, want string) bool {
	for _, v := range values {
		if strings.EqualFold(v, want) {
			return true
		}
	}
	return false
}
