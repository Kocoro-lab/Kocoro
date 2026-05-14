package claudecode

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConvertRules_BannerAndBody(t *testing.T) {
	src := filepath.Join("testdata", "claude_home_basic", "CLAUDE.md")
	staging := t.TempDir()
	dst := filepath.Join(staging, "instructions.md")
	if err := ConvertRules(&ScannedRules{SrcAbsPath: src}, dst, "2026-05-14T11:22:00Z"); err != nil {
		t.Fatalf("ConvertRules: %v", err)
	}
	data, _ := os.ReadFile(dst)
	s := string(data)
	if !strings.Contains(s, "imported from ~/.claude/CLAUDE.md") {
		t.Errorf("banner missing: %s", s)
	}
	if !strings.Contains(s, "inherited by named agents") {
		t.Errorf("inheritance note missing: %s", s)
	}
	if !strings.Contains(s, "Be concise") {
		t.Errorf("body lost: %s", s)
	}
}
