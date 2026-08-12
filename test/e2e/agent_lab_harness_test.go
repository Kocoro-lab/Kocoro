package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestOffline_AgentLabPythonHarness(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 unavailable; comparison harness requires python3")
	}
	// The koe mode-classifier comparison harness and its tests were removed with
	// the routing lane. Skip rather than fail so this stays a working slot for
	// the next Python harness instead of a green run over an empty discovery.
	if _, err := os.Stat(filepath.Join(repoRoot(), "scripts", "tests")); os.IsNotExist(err) {
		t.Skip("no scripts/tests directory; no Python harness to self-test")
	}
	command := exec.Command(
		python,
		"-m",
		"unittest",
		"discover",
		"-s",
		filepath.Join(repoRoot(), "scripts", "tests"),
		"-p",
		"test_*.py",
		"-v",
	)
	command.Env = append(os.Environ(), "PYTHONDONTWRITEBYTECODE=1")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("Python harness self-test: %v\n%s", err, output)
	}
	t.Logf("Python harness self-test:\n%s", output)
}

func TestOffline_AgentLabScriptsParse(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash unavailable; agent-lab runners require bash")
	}
	// One invocation per script: `bash -n a.sh b.sh` syntax-checks only a.sh and
	// silently turns the rest into positional parameters, so a batched call
	// reports success for scripts it never read — including missing ones.
	for _, name := range []string{"agent-lab.sh", "koe-provider-qualification.sh"} {
		script := filepath.Join(repoRoot(), "scripts", name)
		if _, err := os.Stat(script); err != nil {
			t.Fatalf("agent-lab script %s: %v", name, err)
		}
		output, err := exec.Command(bash, "-n", script).CombinedOutput()
		if err != nil {
			t.Fatalf("agent-lab script parse %s: %v\n%s", name, err, output)
		}
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
