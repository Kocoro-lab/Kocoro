package claudecode

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// IntentManifest is written before Phase B begins. It lists every staging
// directory and every planned output path. If apply crashes before
// WriteAppliedManifest runs, the next daemon startup uses these records to
// clean any staging trees and rename the intent file to .orphan for audit.
type IntentManifest struct {
	PlanID      string    `json:"plan_id"`
	CreatedAt   time.Time `json:"created_at"`
	StagingDirs []string  `json:"staging_dirs"`
	Outputs     []string  `json:"outputs"`
}

// AppliedItem records one item that successfully renamed into the target tree.
type AppliedItem struct {
	Category   string `json:"category"`
	Name       string `json:"name"`
	OutputPath string `json:"output_path"`
}

// AppliedManifest is written after Phase B succeeds (fully or partially).
// Its presence next to a same-ID intent manifest signals "no recovery needed".
type AppliedManifest struct {
	PlanID    string        `json:"plan_id"`
	AppliedAt time.Time     `json:"applied_at"`
	Items     []AppliedItem `json:"items"`
}

func manifestDir(target string) string {
	return filepath.Join(target, ".migrate-manifests")
}

// WriteIntentManifest declares Phase B intent. Called before any atomic rename.
func WriteIntentManifest(target string, p *Plan, stagingDirs []string) error {
	if err := os.MkdirAll(manifestDir(target), 0o755); err != nil {
		return err
	}
	outs := make([]string, 0, len(p.PlannedActions))
	for _, a := range p.PlannedActions {
		outs = append(outs, a.DstAbs)
	}
	m := IntentManifest{PlanID: p.ID, CreatedAt: p.CreatedAt, StagingDirs: stagingDirs, Outputs: outs}
	return writeJSON(filepath.Join(manifestDir(target), p.ID+".intent.json"), m)
}

// WriteAppliedManifest replaces the intent manifest with an applied record.
// Called after Phase B completes (regardless of full vs partial-applied result).
func WriteAppliedManifest(target string, p *Plan, items []AppliedItem) error {
	if err := os.MkdirAll(manifestDir(target), 0o755); err != nil {
		return err
	}
	m := AppliedManifest{PlanID: p.ID, AppliedAt: time.Now().UTC(), Items: items}
	if err := writeJSON(filepath.Join(manifestDir(target), p.ID+".applied.json"), m); err != nil {
		return err
	}
	_ = os.Remove(filepath.Join(manifestDir(target), p.ID+".intent.json"))
	return nil
}

// RecoverOrphans is called on daemon startup. For each *.intent.json without
// a matching *.applied.json, it removes the listed staging directories and
// renames the intent file to *.orphan.json for audit. Already-renamed target
// items are left in place (they're conservative writes — no conflict at apply
// time — and rolling them back would be destructive per §9.4 no-overwrite).
func RecoverOrphans(target string) error {
	dir := manifestDir(target)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var errs []string
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".intent.json") {
			continue
		}
		planID := strings.TrimSuffix(name, ".intent.json")
		appliedPath := filepath.Join(dir, planID+".applied.json")
		if _, err := os.Stat(appliedPath); err == nil {
			// Stale intent next to its own applied — remove the stale.
			_ = os.Remove(filepath.Join(dir, name))
			continue
		}
		intentPath := filepath.Join(dir, name)
		data, err := os.ReadFile(intentPath)
		if err != nil {
			errs = append(errs, fmt.Sprintf("read %s: %v", name, err))
			continue
		}
		var m IntentManifest
		if err := json.Unmarshal(data, &m); err == nil {
			for _, s := range m.StagingDirs {
				_ = os.RemoveAll(s)
			}
		}
		if err := os.Rename(intentPath, filepath.Join(dir, planID+".orphan.json")); err != nil {
			errs = append(errs, fmt.Sprintf("rename %s: %v", name, err))
		}
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
