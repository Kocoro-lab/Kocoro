package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

type changeImpactManifest struct {
	SchemaVersion string              `json:"schema_version"`
	Policy        map[string]string   `json:"policy"`
	Entries       []changeImpactEntry `json:"entries"`
}

type changeImpactEntry struct {
	ID            string   `json:"id"`
	ChangeGlobs   []string `json:"change_globs"`
	Rationale     string   `json:"rationale"`
	Deterministic []string `json:"deterministic"`
	Comparison    []string `json:"comparison"`
	ReleaseStatus string   `json:"release_status"`
	Release       []string `json:"release"`
}

func TestChangeImpactManifest(t *testing.T) {
	root := repoRoot()
	body, err := os.ReadFile(filepath.Join(root, "test", "e2e", "testdata", "change_impact_manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest changeImpactManifest
	if err := json.Unmarshal(body, &manifest); err != nil {
		t.Fatalf("decode change-impact manifest: %v", err)
	}
	if manifest.SchemaVersion != "1" {
		t.Fatalf("schema_version=%q, want 1", manifest.SchemaVersion)
	}
	for _, lane := range []string{"deterministic", "comparison", "release"} {
		if strings.TrimSpace(manifest.Policy[lane]) == "" {
			t.Errorf("policy.%s is empty", lane)
		}
	}
	allGo := readGoSources(t, root)
	seen := make(map[string]bool)
	for _, entry := range manifest.Entries {
		if entry.ID == "" || seen[entry.ID] {
			t.Errorf("entry id is empty or duplicate: %q", entry.ID)
		}
		seen[entry.ID] = true
		if len(entry.ChangeGlobs) == 0 || len(entry.Deterministic) == 0 || strings.TrimSpace(entry.Rationale) == "" {
			t.Errorf("entry %s needs change_globs, deterministic commands, and rationale", entry.ID)
		}
		for _, pattern := range entry.ChangeGlobs {
			assertChangeGlobRootExists(t, root, entry.ID, pattern)
		}
		for _, command := range append(append(append([]string{}, entry.Deterministic...), entry.Comparison...), entry.Release...) {
			assertImpactCommand(t, entry.ID, command, allGo)
		}
		switch entry.ReleaseStatus {
		case "covered":
			if len(entry.Release) == 0 {
				t.Errorf("entry %s says release is covered but has no release command", entry.ID)
			}
		case "gap", "not_applicable":
			if len(entry.Release) != 0 {
				t.Errorf("entry %s has release_status=%s but also release commands", entry.ID, entry.ReleaseStatus)
			}
		default:
			t.Errorf("entry %s has invalid release_status %q", entry.ID, entry.ReleaseStatus)
		}
	}
	if len(manifest.Entries) < 8 {
		t.Fatalf("manifest has only %d entries; broad effect areas are missing", len(manifest.Entries))
	}
}

var runTargetPattern = regexp.MustCompile(`-run ['"]?\^([A-Za-z0-9_]+)\$`)

func assertImpactCommand(t *testing.T, entryID, command, allGo string) {
	t.Helper()
	if strings.TrimSpace(command) == "" {
		t.Errorf("entry %s contains an empty command", entryID)
		return
	}
	if strings.Contains(command, "SHANNON_E2E_LIVE=1") && strings.Contains(command, "go test") && !strings.Contains(command, "-tags=live") {
		t.Errorf("entry %s live command omits -tags=live: %s", entryID, command)
	}
	match := runTargetPattern.FindStringSubmatch(command)
	if len(match) == 2 && !strings.Contains(allGo, "func "+match[1]+"(") {
		t.Errorf("entry %s references unknown test %s", entryID, match[1])
	}
}

func assertChangeGlobRootExists(t *testing.T, root, entryID, pattern string) {
	t.Helper()
	prefix := pattern
	if index := strings.IndexAny(prefix, "*?[{"); index >= 0 {
		prefix = prefix[:index]
	}
	if prefix != pattern && !strings.HasSuffix(prefix, string(filepath.Separator)) {
		prefix = filepath.Dir(prefix)
	}
	prefix = strings.TrimSuffix(prefix, string(filepath.Separator))
	if prefix == "" {
		prefix = "."
	}
	if _, err := os.Stat(filepath.Join(root, prefix)); err != nil {
		t.Errorf("entry %s glob %q has missing stable root %q: %v", entryID, pattern, prefix, err)
	}
}

func readGoSources(t *testing.T, root string) string {
	t.Helper()
	var sources strings.Builder
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() && (entry.Name() == ".git" || entry.Name() == "vendor") {
			return filepath.SkipDir
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		body, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		sources.Write(body)
		return nil
	})
	if err != nil {
		t.Fatalf("read Go sources: %v", err)
	}
	return sources.String()
}
