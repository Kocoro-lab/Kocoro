package agents

import (
	"errors"
	"fmt"
	"os"
	"slices"
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

// SkillAttachmentPlan is an immutable snapshot of every manifest change needed
// to detach one or more skill identifiers. Destructive callers can inspect the
// affected agents, take their route locks, and then apply this exact snapshot
// without a second filesystem scan.
type SkillAttachmentPlan struct {
	changes []attachedSkillsChange
}

// DetachSkillAliases removes every matching slug or legacy display name from a
// single agent manifest.
func DetachSkillAliases(agentsDir, agentName string, identifiers ...string) error {
	targets := make(map[string]struct{}, len(identifiers))
	for _, identifier := range identifiers {
		if identifier != "" {
			targets[identifier] = struct{}{}
		}
	}
	names, err := ReadAttachedSkills(agentsDir, agentName)
	if err != nil {
		return err
	}
	filtered := make([]string, 0, len(names))
	for _, name := range names {
		if _, remove := targets[name]; !remove {
			filtered = append(filtered, name)
		}
	}
	return SetAttachedSkills(agentsDir, agentName, filtered)
}

// DetachSkillAliasesFromAllAgents removes every matching slug or legacy display
// name from every agent manifest. All manifests are validated before any write,
// so a corrupt manifest cannot silently survive a successful global deletion.
func DetachSkillAliasesFromAllAgents(agentsDir string, identifiers ...string) ([]string, error) {
	plan, err := PlanDetachSkillAliases(agentsDir, identifiers...)
	if err != nil {
		return nil, err
	}
	return plan.Apply(agentsDir)
}

// AgentsAttachingSkillAliasesStrict returns every agent whose manifest
// references one of the identifiers. Unlike AgentsAttachingSkill, a corrupt or
// unreadable manifest fails the scan so destructive callers can stop before
// changing any state.
func AgentsAttachingSkillAliasesStrict(agentsDir string, identifiers ...string) ([]string, error) {
	plan, err := PlanDetachSkillAliases(agentsDir, identifiers...)
	if err != nil {
		return nil, err
	}
	return plan.AgentNames(), nil
}

// PlanDetachSkillAliases validates every agent attachment manifest and records
// the exact before/after state for manifests containing any identifier.
func PlanDetachSkillAliases(agentsDir string, identifiers ...string) (*SkillAttachmentPlan, error) {
	targets := make(map[string]struct{}, len(identifiers))
	for _, identifier := range identifiers {
		if identifier != "" {
			targets[identifier] = struct{}{}
		}
	}
	if len(targets) == 0 {
		return &SkillAttachmentPlan{}, nil
	}
	changes, err := planAttachedSkillChanges(agentsDir, func(name string) bool {
		_, attached := targets[name]
		return attached
	})
	if err != nil {
		return nil, err
	}
	return &SkillAttachmentPlan{changes: changes}, nil
}

// AgentNames returns the sorted agent names captured by the plan.
func (p *SkillAttachmentPlan) AgentNames() []string {
	if p == nil {
		return []string{}
	}
	result := make([]string, 0, len(p.changes))
	for _, change := range p.changes {
		result = append(result, change.agentName)
	}
	return result
}

// Apply writes the plan's captured after-state transactionally and returns the
// affected agent names.
func (p *SkillAttachmentPlan) Apply(agentsDir string) ([]string, error) {
	if p == nil {
		return []string{}, nil
	}
	// The destructive caller plans while holding every target skill lock, then
	// acquires the affected route locks. An unrelated attachment update may have
	// completed in that interval; reject the stale plan instead of overwriting
	// that update with the captured after-state.
	for _, change := range p.changes {
		current, err := ReadAttachedSkills(agentsDir, change.agentName)
		if err != nil {
			return nil, fmt.Errorf("revalidate attached skills for agent %q: %w", change.agentName, err)
		}
		if !slices.Equal(current, change.before) {
			return nil, fmt.Errorf("attached skills for agent %q changed while deletion was being prepared; retry", change.agentName)
		}
	}
	if err := applyAttachedSkillChanges(agentsDir, p.changes); err != nil {
		return nil, err
	}
	return p.AgentNames(), nil
}

// Restore returns every manifest in an applied plan to its exact captured
// identifier set, including legacy display-name aliases.
func (p *SkillAttachmentPlan) Restore(agentsDir string) error {
	if p == nil || len(p.changes) == 0 {
		return nil
	}
	restore := make([]attachedSkillsChange, 0, len(p.changes))
	for _, change := range p.changes {
		restore = append(restore, attachedSkillsChange{
			agentName: change.agentName,
			before:    append([]string(nil), change.after...),
			after:     append([]string(nil), change.before...),
		})
	}
	return applyAttachedSkillChanges(agentsDir, restore)
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
