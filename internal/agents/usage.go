package agents

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// AgentsAttachingSkill returns the names of agents whose _attached.yaml
// manifest references the given skill. The result is sorted alphabetically
// and is always a non-nil slice (empty slice when no agents attach the skill),
// so JSON responses render as "[]" rather than "null".
//
// Errors from reading a single agent's manifest are skipped — a corrupt
// manifest for agent A should not hide attachments in agent B. Only a
// filesystem error opening agentsDir itself is returned.
func AgentsAttachingSkill(agentsDir, skillName string) ([]string, error) {
	entries, err := os.ReadDir(agentsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return []string{}, nil
		}
		return nil, err
	}

	result := make([]string, 0)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		agentName := entry.Name()
		if len(agentName) == 0 || agentName[0] == '.' {
			continue
		}
		if err := ValidateAgentName(agentName); err != nil {
			continue
		}
		names, err := ReadAttachedSkills(agentsDir, agentName)
		if err != nil {
			// Corrupt or unreadable manifest — skip this agent, keep going.
			continue
		}
		for _, n := range names {
			if n == skillName {
				result = append(result, agentName)
				break
			}
		}
	}
	sort.Strings(result)
	return result, nil
}

type attachedSkillsChange struct {
	agentName string
	before    []string
	after     []string
}

// SkillAttachmentCleanup describes dangling skill references removed from one
// agent manifest.
type SkillAttachmentCleanup struct {
	Agent  string
	Skills []string
}

// DetachSkillAliasesFromAllAgents removes every matching slug or legacy display
// name from every agent manifest. All manifests are validated before any write,
// so a corrupt manifest cannot silently survive a successful global deletion.
func DetachSkillAliasesFromAllAgents(agentsDir string, identifiers ...string) ([]string, error) {
	targets := make(map[string]struct{}, len(identifiers))
	for _, identifier := range identifiers {
		if identifier != "" {
			targets[identifier] = struct{}{}
		}
	}
	if len(targets) == 0 {
		return []string{}, nil
	}

	changes, err := planAttachedSkillChanges(agentsDir, func(name string) bool {
		_, remove := targets[name]
		return remove
	})
	if err != nil {
		return nil, err
	}
	if err := applyAttachedSkillChanges(agentsDir, changes); err != nil {
		return nil, err
	}

	agents := make([]string, 0, len(changes))
	for _, change := range changes {
		agents = append(agents, change.agentName)
	}
	return agents, nil
}

// AgentsAttachingSkillAliasesStrict returns every agent whose manifest
// references one of the identifiers. Unlike AgentsAttachingSkill, a corrupt or
// unreadable manifest fails the scan so destructive callers can stop before
// changing any state.
func AgentsAttachingSkillAliasesStrict(agentsDir string, identifiers ...string) ([]string, error) {
	targets := make(map[string]struct{}, len(identifiers))
	for _, identifier := range identifiers {
		if identifier != "" {
			targets[identifier] = struct{}{}
		}
	}
	if len(targets) == 0 {
		return []string{}, nil
	}
	changes, err := planAttachedSkillChanges(agentsDir, func(name string) bool {
		_, attached := targets[name]
		return attached
	})
	if err != nil {
		return nil, err
	}
	result := make([]string, 0, len(changes))
	for _, change := range changes {
		result = append(result, change.agentName)
	}
	return result, nil
}

// PruneDanglingSkillAttachments removes manifest entries that cannot resolve to
// an installed global skill. A reference matching a directory slug remains
// present whenever that directory contains SKILL.md, even if its metadata is
// temporarily unparseable.
func PruneDanglingSkillAttachments(agentsDir, shannonDir string) ([]SkillAttachmentCleanup, error) {
	present, err := installedSkillIdentifiers(shannonDir)
	if err != nil {
		return nil, err
	}
	changes, err := planAttachedSkillChanges(agentsDir, func(name string) bool {
		_, exists := present[name]
		return !exists
	})
	if err != nil {
		return nil, err
	}
	if err := applyAttachedSkillChanges(agentsDir, changes); err != nil {
		return nil, err
	}

	result := make([]SkillAttachmentCleanup, 0, len(changes))
	for _, change := range changes {
		after := make(map[string]struct{}, len(change.after))
		for _, name := range change.after {
			after[name] = struct{}{}
		}
		removed := make([]string, 0, len(change.before)-len(change.after))
		for _, name := range change.before {
			if _, kept := after[name]; !kept {
				removed = append(removed, name)
			}
		}
		sort.Strings(removed)
		result = append(result, SkillAttachmentCleanup{
			Agent:  change.agentName,
			Skills: removed,
		})
	}
	return result, nil
}

func installedSkillIdentifiers(shannonDir string) (map[string]struct{}, error) {
	present := make(map[string]struct{})
	skillsDir := filepath.Join(shannonDir, "skills")
	entries, err := os.ReadDir(skillsDir)
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("read global skills: %w", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(skillsDir, entry.Name(), "SKILL.md")); err == nil {
			present[entry.Name()] = struct{}{}
		}
	}

	loaded, err := LoadGlobalSkills(shannonDir)
	if err != nil {
		return nil, fmt.Errorf("load global skills: %w", err)
	}
	for _, skill := range loaded {
		if skill.Slug != "" {
			present[skill.Slug] = struct{}{}
		}
		if skill.Name != "" {
			present[skill.Name] = struct{}{}
		}
	}
	return present, nil
}

func planAttachedSkillChanges(agentsDir string, remove func(string) bool) ([]attachedSkillsChange, error) {
	entries, err := os.ReadDir(agentsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return []attachedSkillsChange{}, nil
		}
		return nil, err
	}

	changes := make([]attachedSkillsChange, 0)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		agentName := entry.Name()
		if err := ValidateAgentName(agentName); err != nil {
			continue
		}
		names, err := ReadAttachedSkills(agentsDir, agentName)
		if err != nil {
			return nil, fmt.Errorf("read attached skills for agent %q: %w", agentName, err)
		}
		filtered := make([]string, 0, len(names))
		for _, name := range names {
			if !remove(name) {
				filtered = append(filtered, name)
			}
		}
		if len(filtered) == len(names) {
			continue
		}
		changes = append(changes, attachedSkillsChange{
			agentName: agentName,
			before:    append([]string(nil), names...),
			after:     filtered,
		})
	}
	return changes, nil
}

func applyAttachedSkillChanges(agentsDir string, changes []attachedSkillsChange) error {
	applied := make([]attachedSkillsChange, 0, len(changes))
	for _, change := range changes {
		if err := SetAttachedSkills(agentsDir, change.agentName, change.after); err != nil {
			rollbackErrs := make([]error, 0)
			for i := len(applied) - 1; i >= 0; i-- {
				previous := applied[i]
				if rollbackErr := SetAttachedSkills(agentsDir, previous.agentName, previous.before); rollbackErr != nil {
					rollbackErrs = append(rollbackErrs, fmt.Errorf("restore agent %q: %w", previous.agentName, rollbackErr))
				}
			}
			return errors.Join(
				fmt.Errorf("update attached skills for agent %q: %w", change.agentName, err),
				errors.Join(rollbackErrs...),
			)
		}
		applied = append(applied, change)
	}
	return nil
}
