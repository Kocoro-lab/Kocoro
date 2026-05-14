package daemon

import (
	"errors"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/migrate/claudecode"
)

type claudeMigratePreviewRequest struct {
	SourcePath string `json:"source_path,omitempty"`
	Verbose    bool   `json:"verbose,omitempty"`
}

type claudeMigrateApplyRequest struct {
	PlanID string `json:"plan_id"`
}

type migratePathSet struct {
	ClaudeHome       string `json:"claude_home"`
	ClaudeUserConfig string `json:"claude_user_config"`
	Target           string `json:"target"`
}

type migrateCategorySummary struct {
	ToImport       int  `json:"to_import"`
	ToSkipConflict int  `json:"to_skip_conflict"`
	Present        bool `json:"present,omitempty"`
}

type migrateMCPSummary struct {
	ToImport       int      `json:"to_import"`
	ToSkipConflict int      `json:"to_skip_conflict"`
	MissingEnvKeys []string `json:"missing_env_keys,omitempty"`
}

type claudeMigratePreviewResponse struct {
	PlanID              string                          `json:"plan_id"`
	PlanHash            string                          `json:"plan_hash"`
	ExpiresAt           time.Time                       `json:"expires_at"`
	SourcePathsSymbolic migratePathSet                  `json:"source_paths_symbolic"`
	TargetPathSymbolic  string                          `json:"target_path_symbolic"`
	SourcePaths         *migratePathSet                 `json:"source_paths,omitempty"`
	TargetPath          string                          `json:"target_path,omitempty"`
	Summary             map[string]any                  `json:"summary"`
	Items               map[string][]migratePreviewItem `json:"items"`
	Conflicts           []claudecode.Conflict           `json:"conflicts"`
	Warnings            []migratePreviewWarning         `json:"warnings,omitempty"`
	MCPMissingEnvKeys   []claudecode.ServerKeys         `json:"mcp_missing_env_keys,omitempty"`
	MCPUnsupported      []claudecode.ServerFields       `json:"mcp_unsupported_fields,omitempty"`
	SourceErrors        map[string]string               `json:"source_errors,omitempty"`
}

type migratePreviewItem struct {
	Name              string   `json:"name"`
	Status            string   `json:"status"`
	SrcRelPath        string   `json:"src_relpath,omitempty"`
	Layout            string   `json:"layout,omitempty"`
	EnvKeys           []string `json:"env_keys,omitempty"`
	Command           string   `json:"command,omitempty"`
	ArgsSummary       string   `json:"args_summary,omitempty"`
	URL               string   `json:"url,omitempty"`
	ErrorReason       string   `json:"error_reason,omitempty"`
	UnsupportedFields []string `json:"unsupported_fields,omitempty"`
	Disabled          bool     `json:"disabled,omitempty"`
}

type migratePreviewWarning struct {
	Kind   string   `json:"kind"`
	Server string   `json:"server,omitempty"`
	Keys   []string `json:"keys,omitempty"`
	Fields []string `json:"fields,omitempty"`
	Path   string   `json:"path,omitempty"`
}

func (s *Server) handleClaudeMigratePreview(w http.ResponseWriter, r *http.Request) {
	if !s.requireDeps(w) {
		return
	}
	var req claudeMigratePreviewRequest
	if ok, _ := decodeOptionalBody(w, r, &req); !ok {
		return
	}
	home, _ := os.UserHomeDir()
	src, err := migrateSourcePaths(req.SourcePath, home)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	target := s.deps.ShannonDir
	if target == "" {
		writeError(w, http.StatusInternalServerError, "daemon shannon dir not configured")
		return
	}

	scan, err := claudecode.Scan(src)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if scan.TotalImportable() == 0 && len(scan.SourceErrors) > 0 {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"error":         "claude_not_found",
			"source_errors": scan.SourceErrors,
		})
		return
	}
	plan, err := claudecode.BuildPlan(scan, src, target, home, time.Now().UTC())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	migratePlanStore(s).SweepExpired()
	migratePlanStore(s).Put(plan)

	resp := buildClaudePreviewResponse(scan, plan, req.Verbose)
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleClaudeMigrateApply(w http.ResponseWriter, r *http.Request) {
	if !s.requireDeps(w) {
		return
	}
	var req claudeMigrateApplyRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.PlanID) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "plan_id_required"})
		return
	}
	store := migratePlanStore(s)
	plan, err := store.Get(req.PlanID)
	if err != nil {
		switch {
		case errors.Is(err, claudecode.ErrPlanExpired):
			writeJSON(w, http.StatusGone, map[string]string{"error": "plan_expired"})
		case errors.Is(err, claudecode.ErrPlanNotFound):
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "plan_not_found"})
		default:
			writeError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	result, err := claudecode.NewApplier(s.deps.ShannonDir).Apply(plan)
	if err != nil {
		if strings.Contains(err.Error(), "plan_stale") {
			store.Delete(plan.ID)
			writeJSON(w, http.StatusConflict, map[string]string{"error": "plan_stale", "message": err.Error()})
			return
		}
		if strings.Contains(err.Error(), "plan_expired") {
			store.Delete(plan.ID)
			writeJSON(w, http.StatusGone, map[string]string{"error": "plan_expired"})
			return
		}
		if strings.Contains(err.Error(), "migration_in_progress") {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "migration_in_progress"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "apply_failed", "message": err.Error()})
		return
	}
	store.Delete(plan.ID)
	writeJSON(w, http.StatusOK, result)
}

func migratePlanStore(s *Server) *claudecode.PlanStore {
	if s.migratePlans == nil {
		s.migratePlans = claudecode.NewPlanStore()
	}
	return s.migratePlans
}

func migrateSourcePaths(sourcePath, home string) (claudecode.SourcePaths, error) {
	if strings.TrimSpace(sourcePath) == "" {
		return claudecode.DefaultSources(home), nil
	}
	p := strings.TrimSpace(sourcePath)
	if strings.HasPrefix(p, "~/") && home != "" {
		p = filepath.Join(home, strings.TrimPrefix(p, "~/"))
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return claudecode.SourcePaths{}, err
	}
	return claudecode.SourcePaths{
		ClaudeHome:       abs,
		ClaudeUserConfig: filepath.Join(filepath.Dir(abs), ".claude.json"),
	}, nil
}

func buildClaudePreviewResponse(scan *claudecode.ScanResult, plan *claudecode.Plan, verbose bool) claudeMigratePreviewResponse {
	conflictCounts := map[string]int{}
	for _, c := range plan.Conflicts {
		conflictCounts[c.Category]++
	}
	plannedCounts := map[string]int{}
	for _, a := range plan.PlannedActions {
		plannedCounts[a.Category]++
	}
	missingKeys := uniqueMissingEnvKeys(plan.PlannedWarnings)
	resp := claudeMigratePreviewResponse{
		PlanID:    plan.ID,
		PlanHash:  plan.Hash,
		ExpiresAt: plan.ExpiresAt,
		SourcePathsSymbolic: migratePathSet{
			ClaudeHome:       responseSymbolicPath(plan.Symbolic.ClaudeHome, "<source_path>"),
			ClaudeUserConfig: responseSymbolicPath(plan.Symbolic.ClaudeUserConfig, "<source_config>"),
			Target:           responseSymbolicPath(plan.Symbolic.Target, "<target_path>"),
		},
		TargetPathSymbolic: responseSymbolicPath(plan.Symbolic.Target, "<target_path>"),
		Summary: map[string]any{
			"skills":       migrateCategorySummary{ToImport: plannedCounts["skills"], ToSkipConflict: conflictCounts["skills"]},
			"agents":       migrateCategorySummary{ToImport: plannedCounts["agents"], ToSkipConflict: conflictCounts["agents"]},
			"commands":     migrateCategorySummary{ToImport: plannedCounts["commands"], ToSkipConflict: conflictCounts["commands"]},
			"global_rules": migrateCategorySummary{ToImport: plannedCounts["global_rules"], ToSkipConflict: conflictCounts["global_rules"], Present: scan.GlobalRules != nil},
			"mcp_servers":  migrateMCPSummary{ToImport: plannedCounts["mcp_servers"], ToSkipConflict: conflictCounts["mcp_servers"], MissingEnvKeys: missingKeys},
		},
		Items: map[string][]migratePreviewItem{
			"skills":      previewSkillItems(scan.Skills),
			"agents":      previewAgentItems(scan.Agents),
			"commands":    previewCommandItems(scan.Commands),
			"mcp_servers": previewMCPItems(scan.MCPServers),
		},
		Conflicts:    plan.Conflicts,
		Warnings:     previewWarnings(append(scan.Warnings, plan.PlannedWarnings...)),
		SourceErrors: scan.SourceErrors,
	}
	for _, w := range plan.PlannedWarnings {
		switch w.Kind {
		case "missing_env_keys":
			resp.MCPMissingEnvKeys = append(resp.MCPMissingEnvKeys, claudecode.ServerKeys{Server: w.Server, Keys: w.Keys})
		case "unsupported_fields":
			resp.MCPUnsupported = append(resp.MCPUnsupported, claudecode.ServerFields{Server: w.Server, Fields: w.Fields})
		}
	}
	if verbose {
		resp.SourcePaths = &migratePathSet{
			ClaudeHome:       plan.SourcePaths.ClaudeHome,
			ClaudeUserConfig: plan.SourcePaths.ClaudeUserConfig,
			Target:           plan.TargetPath,
		}
		resp.TargetPath = plan.TargetPath
	}
	return resp
}

func responseSymbolicPath(symbolic, fallback string) string {
	if symbolic == "" {
		return fallback
	}
	if symbolic == "~" || strings.HasPrefix(symbolic, "~/") {
		return symbolic
	}
	if filepath.IsAbs(symbolic) {
		return fallback
	}
	return symbolic
}

func previewSkillItems(items []claudecode.ScannedSkill) []migratePreviewItem {
	out := make([]migratePreviewItem, 0, len(items))
	for _, s := range items {
		out = append(out, migratePreviewItem{Name: s.Name, Status: s.Status, SrcRelPath: s.SrcRelPath, Layout: s.Layout, ErrorReason: s.ErrorReason})
	}
	return out
}

func previewAgentItems(items []claudecode.ScannedAgent) []migratePreviewItem {
	out := make([]migratePreviewItem, 0, len(items))
	for _, a := range items {
		out = append(out, migratePreviewItem{Name: a.Name, Status: a.Status, SrcRelPath: a.SrcRelPath, ErrorReason: a.ErrorReason})
	}
	return out
}

func previewCommandItems(items []claudecode.ScannedCommand) []migratePreviewItem {
	out := make([]migratePreviewItem, 0, len(items))
	for _, c := range items {
		out = append(out, migratePreviewItem{Name: c.Name, Status: c.Status, SrcRelPath: c.SrcRelPath, ErrorReason: c.ErrorReason})
	}
	return out
}

func previewMCPItems(items []claudecode.ScannedMCPServer) []migratePreviewItem {
	out := make([]migratePreviewItem, 0, len(items))
	for _, m := range items {
		out = append(out, migratePreviewItem{
			Name:              m.Name,
			Status:            m.Status,
			EnvKeys:           m.EnvKeys,
			Command:           m.Command,
			ArgsSummary:       strings.Join(m.Args, " "),
			URL:               m.URL,
			ErrorReason:       m.ErrorReason,
			UnsupportedFields: m.UnsupportedFields,
			Disabled:          m.Disabled,
		})
	}
	return out
}

func previewWarnings(warnings []claudecode.Warning) []migratePreviewWarning {
	out := make([]migratePreviewWarning, 0, len(warnings))
	for _, w := range warnings {
		out = append(out, migratePreviewWarning{Kind: w.Kind, Server: w.Server, Keys: w.Keys, Fields: w.Fields, Path: w.Path})
	}
	return out
}

func uniqueMissingEnvKeys(warnings []claudecode.Warning) []string {
	seen := map[string]bool{}
	var out []string
	for _, w := range warnings {
		if w.Kind != "missing_env_keys" {
			continue
		}
		for _, k := range w.Keys {
			if !seen[k] {
				seen[k] = true
				out = append(out, k)
			}
		}
	}
	return out
}

func (s *Server) recoverMigrationOrphans() {
	if s == nil || s.deps == nil || s.deps.ShannonDir == "" {
		return
	}
	if err := claudecode.RecoverOrphans(s.deps.ShannonDir); err != nil {
		log.Printf("daemon migrate: recover orphan manifests: %v", err)
	}
}
