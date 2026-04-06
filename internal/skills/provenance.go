package skills

import (
	"encoding/json"
	"os"
	"path/filepath"
)

const (
	InstallSourceMarketplace = "marketplace"
	InstallSourceLocal       = "local"
	InstallSourceBundled     = "bundled"

	marketplaceProvenanceFile = ".marketplace.json"
)

type marketplaceProvenance struct {
	MarketplaceSlug string `json:"marketplace_slug"`
}

func installProvenanceForSkill(source, skillDir string) (string, string) {
	switch source {
	case SourceBundled:
		return InstallSourceBundled, ""
	case SourceGlobal:
		if slug, ok := readMarketplaceProvenance(skillDir); ok {
			return InstallSourceMarketplace, slug
		}
		return InstallSourceLocal, ""
	default:
		return "", ""
	}
}

func readMarketplaceProvenance(skillDir string) (string, bool) {
	data, err := os.ReadFile(filepath.Join(skillDir, marketplaceProvenanceFile))
	if err != nil {
		return "", false
	}

	var provenance marketplaceProvenance
	if err := json.Unmarshal(data, &provenance); err != nil {
		return "", false
	}
	if err := ValidateSkillName(provenance.MarketplaceSlug); err != nil {
		return "", false
	}
	if provenance.MarketplaceSlug != filepath.Base(skillDir) {
		return "", false
	}
	return provenance.MarketplaceSlug, true
}

func writeMarketplaceProvenance(skillDir, slug string) error {
	data, err := json.Marshal(marketplaceProvenance{MarketplaceSlug: slug})
	if err != nil {
		return err
	}
	return atomicWrite(filepath.Join(skillDir, marketplaceProvenanceFile), data)
}

func clearMarketplaceProvenance(skillDir string) error {
	err := os.Remove(filepath.Join(skillDir, marketplaceProvenanceFile))
	if err == nil || os.IsNotExist(err) {
		return nil
	}
	return err
}
