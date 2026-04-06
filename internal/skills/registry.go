package skills

// Skill is a composable capability loaded from a SKILL.md file.
// Follows the Anthropic Agent Skills spec (agentskills.io/specification).
type Skill struct {
	Name            string            `json:"name"`
	Description     string            `json:"description"`
	Prompt          string            `json:"prompt,omitempty"`
	License         string            `json:"license,omitempty"`
	Compatibility   string            `json:"compatibility,omitempty"`
	Metadata        map[string]string `json:"metadata,omitempty"`
	AllowedTools    []string          `json:"allowed_tools,omitempty"`
	Source          string            `json:"-"`
	InstallSource   string            `json:"-"`
	MarketplaceSlug string            `json:"-"`
	Dir             string            `json:"-"`
}

// SkillMeta is the lightweight representation for API responses (no body/prompt).
type SkillMeta struct {
	Name            string `json:"name"`
	Description     string `json:"description"`
	Source          string `json:"source,omitempty"`
	InstallSource   string `json:"install_source"`
	MarketplaceSlug string `json:"marketplace_slug,omitempty"`
}

// ToMeta returns API-safe metadata without the full prompt body.
func (s *Skill) ToMeta() SkillMeta {
	return SkillMeta{
		Name:            s.Name,
		Description:     s.Description,
		Source:          s.Source,
		InstallSource:   s.InstallSource,
		MarketplaceSlug: s.MarketplaceSlug,
	}
}
