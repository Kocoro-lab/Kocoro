package claudecode

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// ApplyResult is what the daemon HTTP handler serializes to clients. All
// counts come from PlannedActions / actual atomic renames — never from raw
// scanner items — so conflicts (which are excluded from PlannedActions) can
// never be miscounted as imported.
type ApplyResult struct {
	Result               string         // "applied" | "partial_applied" | "staged_only"
	AppliedAt            time.Time
	Imported             map[string]CategoryCount
	Skipped              []Conflict
	MCPMissingEnvKeys    []ServerKeys
	MCPUnsupportedFields []ServerFields
	Failure              *FailureInfo
	ManifestID           string
}

// CategoryCount carries planned/completed as `any` so a JSON encoder can
// emit either an int (skills/agents/commands/mcp_servers) or a bool
// (global_rules). Boolean is the natural shape for the rules category
// since exactly one rules file is planned or none at all.
type CategoryCount struct {
	Completed any `json:"completed"`
	Planned   any `json:"planned"`
}

type ServerKeys struct {
	Server string   `json:"server"`
	Keys   []string `json:"keys"`
}

type ServerFields struct {
	Server string   `json:"server"`
	Fields []string `json:"fields"`
}

type FailureInfo struct {
	Category string `json:"category"`
	Item     string `json:"item"`
	Reason   string `json:"reason"`
	Detail   string `json:"detail"`
}

// Applier owns one migration end-to-end. Phase A re-validates source freshness,
// stages every output into a sibling directory, and writes the intent manifest.
// Phase B atomic-renames staging into target one item at a time; on first
// failure it stops, records the failure, finalizes the applied manifest, and
// returns result=partial_applied. There is no rollback of items that already
// renamed (spec §8.1 — partial-applied is a valid terminal state).
var migrateMu sync.Mutex

type Applier struct {
	target           string
	stopAfterStaging bool // test hook: stop after Phase A
	stagedActions    []stagedAction

	// testFailOnItem fires before each Phase B atomic rename. Returning a non-
	// nil error simulates a rename failure mid-flight. The index is per-category.
	testFailOnItem func(act PlannedAction, indexWithinCategory int) error
}

type stagedAction struct {
	action      PlannedAction
	stagingDir  string // for category dir items (skills, agents, commands-as-skills)
	stagingFile string // for global_rules single-file path
}

func NewApplier(target string) *Applier { return &Applier{target: target} }

const importedAtFormat = "2006-01-02T15:04:05Z07:00"

// Apply runs Phase A (stage + manifest) and Phase B (commit + applied manifest).
// Only one Apply may run process-wide at a time; concurrent attempts return
// "migration_in_progress" so the HTTP handler can map to 409.
func (a *Applier) Apply(p *Plan) (*ApplyResult, error) {
	if !migrateMu.TryLock() {
		return nil, fmt.Errorf("migration_in_progress")
	}
	defer migrateMu.Unlock()

	if time.Now().After(p.ExpiresAt) {
		return nil, fmt.Errorf("plan_expired")
	}

	if err := a.validateFreshness(p); err != nil {
		return nil, err
	}
	if err := a.stageAll(p); err != nil {
		a.cleanupStaging()
		return nil, fmt.Errorf("phase_a_failed: %w", err)
	}
	if err := WriteIntentManifest(a.target, p, a.stagingDirSet()); err != nil {
		a.cleanupStaging()
		return nil, err
	}

	if a.stopAfterStaging {
		return &ApplyResult{Result: "staged_only", ManifestID: p.ID}, nil
	}

	return a.commit(p)
}

// validateFreshness re-fingerprints every source path recorded in the plan
// and re-checks every planned destination. A mismatched fingerprint or a
// newly-existing target item aborts with plan_stale before any write.
func (a *Applier) validateFreshness(p *Plan) error {
	for path, want := range p.SourceHashes {
		ok, err := ValidateSourceFingerprint(path, want)
		if err != nil {
			return fmt.Errorf("plan_stale: cannot re-validate %s: %w", path, err)
		}
		if !ok {
			return fmt.Errorf("plan_stale: source_changed: %s", path)
		}
	}
	for _, act := range p.PlannedActions {
		if act.Category == "mcp_servers" || act.DstAbs == "" {
			continue
		}
		probe := act.DstAbs
		if s := dstSentinel(act.Category); s != "" {
			probe = filepath.Join(act.DstAbs, s)
		}
		if _, err := os.Stat(probe); err == nil {
			return fmt.Errorf("plan_stale: target_conflict_added: %s/%s", act.Category, act.Name)
		}
	}
	return nil
}

func dstSentinel(category string) string {
	switch category {
	case "skills", "commands":
		return "SKILL.md"
	case "agents":
		return "AGENT.md"
	default:
		return ""
	}
}

// stageAll runs every converter against a staging path. The staging suffix
// is keyed by (category, name) so concurrent migrations against different
// targets don't collide. Re-scans the source so converters see fresh
// metadata even if a long delay elapsed between Scan() and Apply().
func (a *Applier) stageAll(p *Plan) error {
	importedAt := time.Now().UTC().Format(importedAtFormat)
	scan, _ := Scan(p.SourcePaths)
	idx := indexScan(scan)

	for _, act := range p.PlannedActions {
		switch act.Category {
		case "skills":
			s, ok := idx.skills[act.Name]
			if !ok {
				return fmt.Errorf("skill %q missing in re-scan", act.Name)
			}
			staging := a.stagingDirFor(act)
			if err := ConvertSkill(s, staging); err != nil {
				return err
			}
			a.stagedActions = append(a.stagedActions, stagedAction{action: act, stagingDir: staging})

		case "agents":
			ag, ok := idx.agents[act.Name]
			if !ok {
				return fmt.Errorf("agent %q missing in re-scan", act.Name)
			}
			staging := a.stagingDirFor(act)
			if _, err := ConvertAgent(ag, staging, importedAt); err != nil {
				return err
			}
			a.stagedActions = append(a.stagedActions, stagedAction{action: act, stagingDir: staging})

		case "commands":
			c, ok := idx.commands[act.Name]
			if !ok {
				return fmt.Errorf("command %q missing in re-scan", act.Name)
			}
			staging := a.stagingDirFor(act)
			if err := ConvertCommand(c, staging, importedAt); err != nil {
				return err
			}
			a.stagedActions = append(a.stagedActions, stagedAction{action: act, stagingDir: staging})

		case "global_rules":
			if scan.GlobalRules == nil {
				return fmt.Errorf("global_rules missing in re-scan")
			}
			tmpFile := act.DstAbs + ".migrate.tmp"
			if err := ConvertRules(scan.GlobalRules, tmpFile, importedAt); err != nil {
				return err
			}
			a.stagedActions = append(a.stagedActions, stagedAction{action: act, stagingFile: tmpFile})

		case "mcp_servers":
			// MCP commits as a single config.yaml merge in Phase B — staging
			// is implicit in the planned action set; nothing to stage here.
			a.stagedActions = append(a.stagedActions, stagedAction{action: act})
		}
	}
	return nil
}

func (a *Applier) stagingDirFor(act PlannedAction) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s|%s", act.Category, act.Name)
	return act.DstAbs + ".staging-" + hex.EncodeToString(h.Sum(nil))[:8]
}

func (a *Applier) stagingDirSet() []string {
	out := make([]string, 0, len(a.stagedActions))
	for _, s := range a.stagedActions {
		if s.stagingDir != "" {
			out = append(out, s.stagingDir)
		}
		if s.stagingFile != "" {
			out = append(out, s.stagingFile)
		}
	}
	return out
}

func (a *Applier) cleanupStaging() {
	for _, s := range a.stagedActions {
		if s.stagingDir != "" {
			_ = os.RemoveAll(s.stagingDir)
		}
		if s.stagingFile != "" {
			_ = os.Remove(s.stagingFile)
		}
	}
}

// commit runs Phase B: per-item atomic rename, then a single MCP merge.
// Result counts are derived from PlannedActions per category (planned) and
// successful renames (completed). Raw scanner counts are not used here so
// MCP conflicts (excluded from PlannedActions) cannot leak into Imported.
func (a *Applier) commit(p *Plan) (*ApplyResult, error) {
	planned := countPlanned(p)
	completed := map[string]int{}
	completedRules := false
	completedMCP := false
	var applied []AppliedItem
	var failure *FailureInfo

	scan, _ := Scan(p.SourcePaths)

	indexByCat := map[string]int{}
	for _, s := range a.stagedActions {
		cat := s.action.Category
		idx := indexByCat[cat]
		indexByCat[cat]++

		if a.testFailOnItem != nil {
			if err := a.testFailOnItem(s.action, idx); err != nil {
				failure = &FailureInfo{Category: cat, Item: s.action.Name, Reason: "rename_failed", Detail: err.Error()}
				break
			}
		}

		switch cat {
		case "skills", "agents", "commands":
			if err := os.MkdirAll(filepath.Dir(s.action.DstAbs), 0o755); err != nil {
				failure = &FailureInfo{Category: cat, Item: s.action.Name, Reason: "mkdir_failed", Detail: err.Error()}
			} else if err := os.Rename(s.stagingDir, s.action.DstAbs); err != nil {
				failure = &FailureInfo{Category: cat, Item: s.action.Name, Reason: "rename_failed", Detail: err.Error()}
			} else {
				completed[cat]++
				applied = append(applied, AppliedItem{Category: cat, Name: s.action.Name, OutputPath: s.action.DstAbs})
			}

		case "global_rules":
			if err := os.Rename(s.stagingFile, s.action.DstAbs); err != nil {
				failure = &FailureInfo{Category: cat, Item: s.action.Name, Reason: "rename_failed", Detail: err.Error()}
			} else {
				completedRules = true
				applied = append(applied, AppliedItem{Category: cat, Name: "instructions.md", OutputPath: s.action.DstAbs})
			}

		case "mcp_servers":
			// One merge committed on the last MCP entry in the staged sequence.
			if idx == planned.MCP-1 {
				importedAt := time.Now().UTC().Format(importedAtFormat)
				mcpsToMerge := mcpServersFromPlan(scan.MCPServers, p)
				if err := MergeMCPIntoConfig(a.target, mcpsToMerge, p.MCPDisabled, importedAt); err != nil {
					failure = &FailureInfo{Category: cat, Item: s.action.Name, Reason: "config_merge_failed", Detail: err.Error()}
				} else {
					completedMCP = true
					for _, m := range mcpsToMerge {
						applied = append(applied, AppliedItem{Category: cat, Name: m.Name, OutputPath: filepath.Join(a.target, "config.yaml")})
					}
				}
			}
		}

		if failure != nil {
			break
		}
	}

	res := &ApplyResult{
		AppliedAt:  time.Now().UTC(),
		ManifestID: p.ID,
		Failure:    failure,
		Imported: map[string]CategoryCount{
			"skills":       intCount(completed["skills"], planned.Skills),
			"agents":       intCount(completed["agents"], planned.Agents),
			"commands":     intCount(completed["commands"], planned.Commands),
			"global_rules": boolCount(completedRules, planned.Rules),
			"mcp_servers":  intCount(mcpCompletedCount(completedMCP, p), planned.MCP),
		},
	}
	if failure != nil {
		res.Result = "partial_applied"
	} else {
		res.Result = "applied"
	}
	// Skipped and MCP warnings come from the plan, not the scan, so conflicts
	// stay visible to the user without being miscounted.
	res.Skipped = append(res.Skipped, p.Conflicts...)
	for _, w := range p.PlannedWarnings {
		switch w.Kind {
		case "missing_env_keys":
			res.MCPMissingEnvKeys = append(res.MCPMissingEnvKeys, ServerKeys{Server: w.Server, Keys: w.Keys})
		case "unsupported_fields":
			res.MCPUnsupportedFields = append(res.MCPUnsupportedFields, ServerFields{Server: w.Server, Fields: w.Fields})
		}
	}
	_ = WriteAppliedManifest(a.target, p, applied)
	return res, nil
}

type plannedCounts struct {
	Skills, Agents, Commands, MCP int
	Rules                         bool
}

func countPlanned(p *Plan) plannedCounts {
	c := plannedCounts{}
	for _, a := range p.PlannedActions {
		switch a.Category {
		case "skills":
			c.Skills++
		case "agents":
			c.Agents++
		case "commands":
			c.Commands++
		case "global_rules":
			c.Rules = true
		case "mcp_servers":
			c.MCP++
		}
	}
	return c
}

func intCount(completed, planned int) CategoryCount {
	return CategoryCount{Completed: completed, Planned: planned}
}

func boolCount(completed, planned bool) CategoryCount {
	return CategoryCount{Completed: completed, Planned: planned}
}

// mcpCompletedCount returns 0 when the MCP merge did not run, else the number
// of MCP servers in PlannedActions (since the merge is all-or-nothing).
func mcpCompletedCount(committed bool, p *Plan) int {
	if !committed {
		return 0
	}
	n := 0
	for _, a := range p.PlannedActions {
		if a.Category == "mcp_servers" {
			n++
		}
	}
	return n
}

// mcpServersFromPlan filters the fresh scan to only the servers that the
// plan actually intends to import (i.e. excluded by name match against
// PlannedActions). Conflicts in the plan are therefore not re-imported.
func mcpServersFromPlan(scanned []ScannedMCPServer, p *Plan) []ScannedMCPServer {
	want := map[string]bool{}
	for _, a := range p.PlannedActions {
		if a.Category == "mcp_servers" {
			want[a.Name] = true
		}
	}
	var out []ScannedMCPServer
	for _, s := range scanned {
		if want[s.Name] {
			out = append(out, s)
		}
	}
	return out
}

// indexScan converts the slice-of-X scan result into per-name lookup maps
// for the converters.
type scanIndex struct {
	skills   map[string]ScannedSkill
	agents   map[string]ScannedAgent
	commands map[string]ScannedCommand
}

func indexScan(s *ScanResult) scanIndex {
	idx := scanIndex{
		skills:   map[string]ScannedSkill{},
		agents:   map[string]ScannedAgent{},
		commands: map[string]ScannedCommand{},
	}
	for _, x := range s.Skills {
		idx.skills[x.Name] = x
	}
	for _, x := range s.Agents {
		idx.agents[x.Name] = x
	}
	for _, x := range s.Commands {
		idx.commands[x.Name] = x
	}
	return idx
}
