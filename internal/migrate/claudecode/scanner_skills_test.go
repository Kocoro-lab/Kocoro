package claudecode

import (
	"path/filepath"
	"testing"
)

func TestScanSkills_BasicLayouts(t *testing.T) {
	src := filepath.Join("testdata", "claude_home_basic")
	got, warnings, err := scanSkills(src)
	if err != nil {
		t.Fatalf("scanSkills: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %+v", warnings)
	}
	names := map[string]string{}
	for _, s := range got {
		names[s.Name] = s.Layout
	}
	if names["aws-claude"] != "flat" {
		t.Errorf("aws-claude: want flat, got %q", names["aws-claude"])
	}
	if names["dir-style"] != "dir" {
		t.Errorf("dir-style: want dir, got %q", names["dir-style"])
	}
	if names["flat-one"] != "" {
		t.Errorf("empty dir 'flat-one' should not be scanned, got %q", names["flat-one"])
	}
}

func TestScanSkills_MissingDir(t *testing.T) {
	_, warnings, err := scanSkills(filepath.Join("testdata", "does-not-exist"))
	if err != nil {
		t.Fatalf("missing dir should not error, got %v", err)
	}
	if len(warnings) == 0 {
		t.Error("expected source_unavailable warning")
	}
}
