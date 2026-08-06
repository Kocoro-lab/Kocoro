package e2e

import (
	"os/exec"
	"path/filepath"
	"testing"
)

func TestOffline_AgentLabComparisonHarness(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 unavailable; comparison harness requires python3")
	}
	command := exec.Command(
		python,
		"-m",
		"unittest",
		"discover",
		"-s",
		filepath.Join(repoRoot(), "scripts", "tests"),
		"-p",
		"test_compare_koe_mode_reports.py",
		"-v",
	)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("comparison harness self-test: %v\n%s", err, output)
	}
	t.Logf("comparison harness self-test:\n%s", output)
}
