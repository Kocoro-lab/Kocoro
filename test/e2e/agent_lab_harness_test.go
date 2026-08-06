package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

func TestOffline_AgentLabScriptsParse(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash unavailable; agent-lab runners require bash")
	}
	command := exec.Command(
		bash,
		"-n",
		filepath.Join(repoRoot(), "scripts", "agent-lab.sh"),
		filepath.Join(repoRoot(), "scripts", "koe-provider-qualification.sh"),
	)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("agent-lab script parse: %v\n%s", err, output)
	}
}

func TestOffline_ProviderQualificationRejectsUndersizedReleaseSample(t *testing.T) {
	script := filepath.Join(repoRoot(), "scripts", "koe-provider-qualification.sh")
	command := exec.Command(script, t.TempDir())
	command.Env = append(
		os.Environ(),
		"KOE_PROVIDER_AGENTLOOP_E2E=1",
		"KOE_PROVIDER_SAMPLE=release",
		"KOE_PROVIDER_REPETITIONS=1",
	)
	output, err := command.CombinedOutput()
	if err == nil {
		t.Fatal("undersized release provider sample unexpectedly succeeded")
	}
	exitError, ok := err.(*exec.ExitError)
	if !ok || exitError.ExitCode() != 2 {
		t.Fatalf("undersized release exit=%v, want status 2; output=%s", err, output)
	}
	if !strings.Contains(string(output), "requires KOE_PROVIDER_REPETITIONS >= 30") {
		t.Fatalf("undersized release error is not actionable: %s", output)
	}
}
