package e2e

import (
	"encoding/json"
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
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("Python harness self-test: %v\n%s", err, output)
	}
	t.Logf("Python harness self-test:\n%s", output)
}

func TestOffline_ProductReleaseRejectsMissingEvidenceBeforePaidWork(t *testing.T) {
	outputDir := t.TempDir()
	command := exec.Command(filepath.Join(repoRoot(), "scripts", "agent-lab.sh"), outputDir)
	for _, entry := range os.Environ() {
		if strings.HasPrefix(entry, "KOCORO_AUDIO_HIL_REPORT=") ||
			strings.HasPrefix(entry, "KOCORO_EXTERNAL_WRITE_RECOVERY_REPORT=") ||
			strings.HasPrefix(entry, "AGENT_LAB_LANE=") {
			continue
		}
		command.Env = append(command.Env, entry)
	}
	command.Env = append(
		command.Env,
		"AGENT_LAB_LANE=product_release",
		"KOCORO_PRODUCT_RELEASE_E2E=1",
		"PYTHONDONTWRITEBYTECODE=1",
	)
	output, err := command.CombinedOutput()
	if err == nil {
		t.Fatal("product release unexpectedly passed without physical evidence")
	}
	if !strings.Contains(string(output), "KOCORO_AUDIO_HIL_REPORT") {
		t.Fatalf("missing-evidence failure is not actionable: %s", output)
	}
	if strings.Contains(string(output), "Set KOE_PROVIDER_AGENTLOOP_E2E") {
		t.Fatalf("paid provider lane ran before evidence preflight: %s", output)
	}
	if _, statErr := os.Stat(filepath.Join(outputDir, "provider")); !os.IsNotExist(statErr) {
		t.Fatalf("provider artifacts exist after failed evidence preflight: %v", statErr)
	}

	raw, readErr := os.ReadFile(filepath.Join(outputDir, "manifest.json"))
	if readErr != nil {
		t.Fatalf("read failed product manifest: %v", readErr)
	}
	var manifest struct {
		QualificationScope string `json:"qualification_scope"`
		Passed             bool   `json:"passed"`
		Checks             []struct {
			Name       string `json:"name"`
			ExitStatus int    `json:"exit_status"`
		} `json:"checks"`
	}
	if unmarshalErr := json.Unmarshal(raw, &manifest); unmarshalErr != nil {
		t.Fatalf("decode failed product manifest: %v", unmarshalErr)
	}
	if manifest.QualificationScope != "whole_product" || manifest.Passed {
		t.Fatalf("unexpected failed product manifest: %+v", manifest)
	}
	if len(manifest.Checks) != 1 || manifest.Checks[0].Name != "product_release_evidence" || manifest.Checks[0].ExitStatus != 2 {
		t.Fatalf("unexpected evidence checks: %+v", manifest.Checks)
	}
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
