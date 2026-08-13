package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

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
