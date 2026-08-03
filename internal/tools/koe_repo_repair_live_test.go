package tools

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/executionprofile"
)

// This E2E makes paid provider calls. Normal test runs stop at the first gate
// before reading endpoint or credential environment variables.
//
// Fast:
//
//	KOE_REPO_REPAIR_LIVE=1 \
//	KOE_REPO_REPAIR_LANE=luna_fast \
//	KOE_FAST_QUALIFICATION_ENDPOINT=http://127.0.0.1:18080 \
//	KOE_FAST_QUALIFICATION_API_KEY=... \
//	go test ./internal/tools -run TestKoeRepoRepairLive_AgentLoop -v -count=1
//
// The controlled Sonnet reference uses the same command with
// KOE_REPO_REPAIR_LANE=sonnet_reference. This is a benchmark pin only; product
// Full mode preserves the selected Agent's normal model, tier, and effort.
//
// The test never logs the task prompt, model reply, tool arguments, endpoint,
// execution profile id, or credential. Failure messages contain only
// content-free oracle classes and counts.
const (
	koeRepoRepairGateEnv = "KOE_REPO_REPAIR_LIVE"
	koeRepoRepairLaneEnv = "KOE_REPO_REPAIR_LANE"

	koeRepoRepairTargetFile    = "retry_policy.go"
	koeRepoRepairRunTimeout    = 8 * time.Minute
	koeRepoRepairBashTimeout   = 2 * time.Minute
	koeRepoRepairMaxIterations = 12
)

func TestKoeRepoRepairLive_AgentLoop(t *testing.T) {
	if os.Getenv(koeRepoRepairGateEnv) != "1" {
		t.Skip("set KOE_REPO_REPAIR_LIVE=1 to run the paid repository-repair E2E")
	}

	cfg := loadKoeQualificationRuntimeConfig(t)
	lane := strings.TrimSpace(os.Getenv(koeRepoRepairLaneEnv))
	if lane == "" {
		lane = koeQualificationFastLane
	}
	if lane != koeQualificationFastLane &&
		lane != koeQualificationSonnetReferenceLane {
		t.Fatal("KOE_REPO_REPAIR_LANE must be luna_fast or sonnet_reference")
	}

	repoDir := t.TempDir()
	writeKoeRepoRepairFixture(t, repoDir)
	before := snapshotKoeRepoRepairFiles(t, repoDir)
	assertKoeRepoRepairFixtureFails(t, repoDir)

	gateway := client.NewGatewayClient(cfg.endpoint, cfg.apiKey)
	profile, profileExact := resolveKoeRepoRepairProfile(t, gateway, lane)

	audit := newKoeRepoRepairAudit(repoDir)
	registry := agent.NewToolRegistry()
	registry.Register(&FileReadTool{})
	registry.Register(&GrepTool{})
	registry.Register(&koeRepoRepairRecordingFileEdit{
		inner: &FileEditTool{},
		audit: audit,
	})
	registry.Register(&koeRepoRepairRecordingBash{
		inner: &BashTool{
			CWD:                repoDir,
			DefaultTimeoutSecs: int(koeRepoRepairBashTimeout.Seconds()),
			MaxTimeoutSecs:     int(koeRepoRepairBashTimeout.Seconds()),
		},
		audit: audit,
	})

	start := time.Now()
	recordingClient := &koeQualificationLLMClient{
		inner: gateway,
		start: start,
	}
	loop := agent.NewAgentLoop(
		recordingClient,
		registry,
		"",
		"",
		koeRepoRepairMaxIterations,
		4000,
		200,
		nil,
		nil,
		nil,
	)
	loop.SetMaxTokens(koeQualificationMaxTokens)
	loop.SetSessionCWD(repoDir)
	loop.SetEnableStreaming(true)
	loop.SetSkillDiscovery(false)
	loop.SetBypassPermissions(true)
	loop.SetCacheSource("koe_repo_repair_live")
	loop.SetSessionID(fmt.Sprintf("koe-repo-repair-%s-%d", lane, time.Now().UnixNano()))
	loop.SetHandler(&koeQualificationEventHandler{})
	if lane == koeQualificationFastLane {
		loop.SetKoeExecutionProfile(profile)
	} else {
		loop.SetSpecificModel(koeQualificationSonnetModel)
		loop.SetThinking(&client.ThinkingConfig{Type: "adaptive"})
		loop.SetReasoningEffort("")
		loop.SetEffortTier("")
		loop.SetKoeExecutionProfile(profile)
	}

	runCtx, cancel := context.WithTimeout(context.Background(), koeRepoRepairRunTimeout)
	defer cancel()
	result, usage, runErr := loop.Run(runCtx, koeRepoRepairTaskPrompt(), nil, nil)
	elapsed := time.Since(start)

	requests, responses, _, _, _, _ := recordingClient.snapshot()
	if runErr != nil {
		class, status := koeQualificationRuntimeErrorClass(runErr)
		t.Fatalf("repository-repair AgentLoop failed: class=%s status=%d", class, status)
	}
	if strings.TrimSpace(result) == "" {
		t.Fatal("repository-repair AgentLoop returned an empty final answer")
	}

	after := snapshotKoeRepoRepairFiles(t, repoDir)
	assertKoeRepoRepairFileOracle(t, before, after)
	assertKoeRepoRepairTestsPass(t, repoDir)
	costUSD := 0.0
	if usage != nil {
		costUSD = usage.CostUSD
	}
	t.Logf(
		"content-free repository-repair artifact: pass=true lane=%s completion_calls=%d total_millis=%d cost_usd=%.6f",
		lane,
		len(requests),
		elapsed.Milliseconds(),
		costUSD,
	)
	assertKoeRepoRepairToolOracle(t, audit, responses)

	expectedProvider, expectedModel := koeQualificationExpectedRoute(lane)
	providerExact, _ := koeQualificationObservedExact(
		responses,
		expectedProvider,
		func(response client.CompletionResponse) string { return response.Provider },
	)
	modelExact, _ := koeQualificationObservedExact(
		responses,
		expectedModel,
		func(response client.CompletionResponse) string { return response.Model },
	)
	routeExact := koeQualificationRouteExact(lane, profile.ProfileID, requests)
	cacheExact, cacheHits := koeQualificationCacheExact(responses)
	if !profileExact || !providerExact || !modelExact || !routeExact || !cacheExact {
		t.Fatalf(
			"repository-repair route oracle failed: profile=%t provider=%t model=%t route=%t cache=%t cache_hits=%d",
			profileExact,
			providerExact,
			modelExact,
			routeExact,
			cacheExact,
			cacheHits,
		)
	}

	toolCalls, toolIterations := koeQualificationToolCalls(responses)
	t.Logf(
		"content-free repository-repair verdict: pass=true lane=%s completion_calls=%d tool_calls=%d tool_iterations=%d edit_effects=%d duplicate_edit_effects=%d total_millis=%d",
		lane,
		len(requests),
		len(toolCalls),
		toolIterations,
		audit.successfulEditCount(),
		audit.duplicateEditEffectCount(),
		elapsed.Milliseconds(),
	)
}

func koeRepoRepairTaskPrompt() string {
	return "Repair the disposable Go repository in the current working directory. " +
		"Its existing `go test ./...` deterministically fails with a panic. " +
		"Diagnose the root cause using the available production tools, make one " +
		"minimal source-only repair with file_edit, and do not modify tests or " +
		"go.mod or create/delete files. Do not use bash to edit files. Run " +
		"`go test ./...` yourself before and after the repair, and do not claim " +
		"completion until the final test run passes. Finish with a concise, " +
		"non-empty summary."
}

func resolveKoeRepoRepairProfile(
	t *testing.T,
	gateway *client.GatewayClient,
	lane string,
) (executionprofile.Profile, bool) {
	t.Helper()
	if lane == koeQualificationSonnetReferenceLane {
		return executionprofile.FullProfile(
			executionprofile.ModeFull,
			"koe_repo_repair_live",
		), true
	}

	ctx, cancel := context.WithTimeout(
		context.Background(),
		koeQualificationResolverTimeout,
	)
	defer cancel()
	cloudProfile, err := gateway.ResolveKoeExecutionProfile(ctx)
	if err != nil {
		class, status := koeQualificationRuntimeErrorClass(err)
		t.Fatalf("repository-repair profile resolution failed: class=%s status=%d", class, status)
	}
	profile := executionprofile.Resolve(executionprofile.ResolutionInput{
		RequestedMode: executionprofile.ModeFast,
		FastEnabled:   true,
		CloudProfile:  &cloudProfile,
	})
	exact := cloudProfile == profile && profile.ValidateFast() == nil
	if !exact {
		t.Fatal("repository-repair fast profile failed exact validation")
	}
	return profile, true
}

func writeKoeRepoRepairFixture(t *testing.T, repoDir string) {
	t.Helper()
	files := map[string]string{
		"go.mod": "module example.com/retrypolicy\n\ngo 1.25.7\n",
		koeRepoRepairTargetFile: `package retrypolicy

import "strings"

func ParseRetryHeader(raw string) (string, string) {
	parts := strings.SplitN(raw, "=", 2)
	return parts[0], parts[1]
}
`,
		"retry_policy_test.go": `package retrypolicy

import "testing"

func TestParseRetryHeaderWithValue(t *testing.T) {
	key, value := ParseRetryHeader("retry-after=3")
	if key != "retry-after" || value != "3" {
		t.Fatalf("ParseRetryHeader() = (%q, %q), want (retry-after, 3)", key, value)
	}
}

func TestParseRetryHeaderWithoutValueUsesDefault(t *testing.T) {
	key, value := ParseRetryHeader("retry-after")
	if key != "retry-after" || value != "default" {
		t.Fatalf("ParseRetryHeader() = (%q, %q), want (retry-after, default)", key, value)
	}
}
`,
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(repoDir, name), []byte(content), 0o644); err != nil {
			t.Fatalf("create repository-repair fixture: %v", err)
		}
	}
}

func assertKoeRepoRepairFixtureFails(t *testing.T, repoDir string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "test", "./...")
	cmd.Dir = repoDir
	output, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("repository-repair fixture unexpectedly passed before AgentLoop")
	}
	if ctx.Err() != nil {
		t.Fatal("repository-repair fixture precondition timed out")
	}
	if !strings.Contains(string(output), "index out of range") {
		t.Fatal("repository-repair fixture did not fail with the expected deterministic panic")
	}
}

func assertKoeRepoRepairTestsPass(t *testing.T, repoDir string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "test", "./...")
	cmd.Dir = repoDir
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			t.Fatal("repository-repair postcondition test timed out")
		}
		t.Fatal("repository-repair postcondition test failed")
	}
}

func snapshotKoeRepoRepairFiles(t *testing.T, repoDir string) map[string][32]byte {
	t.Helper()
	files := make(map[string][32]byte)
	err := filepath.WalkDir(repoDir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(repoDir, path)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files[filepath.ToSlash(relative)] = sha256.Sum256(data)
		return nil
	})
	if err != nil {
		t.Fatalf("snapshot repository-repair fixture: %v", err)
	}
	return files
}

func assertKoeRepoRepairFileOracle(
	t *testing.T,
	before map[string][32]byte,
	after map[string][32]byte,
) {
	t.Helper()
	if len(before) != len(after) {
		t.Fatalf(
			"repository-repair file-set changed: before=%d after=%d",
			len(before),
			len(after),
		)
	}
	names := make([]string, 0, len(before))
	for name := range before {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		afterHash, ok := after[name]
		if !ok {
			t.Fatal("repository-repair deleted a fixture file")
		}
		if name == koeRepoRepairTargetFile {
			if afterHash == before[name] {
				t.Fatal("repository-repair target source was not changed")
			}
			continue
		}
		if afterHash != before[name] {
			t.Fatal("repository-repair changed a non-target file")
		}
	}
	targetPath := koeRepoRepairTargetFile
	targetHash, ok := after[targetPath]
	if !ok || targetHash == sha256.Sum256(nil) {
		t.Fatal("repository-repair target source is missing or empty")
	}
}

func assertKoeRepoRepairToolOracle(
	t *testing.T,
	audit *koeRepoRepairAudit,
	responses []client.CompletionResponse,
) {
	t.Helper()
	observed := make(map[string]int)
	for _, response := range responses {
		for _, call := range response.AllToolCalls() {
			observed[call.Name]++
		}
	}
	if observed["file_read"]+observed["grep"] == 0 {
		t.Fatal("repository-repair did not inspect repository source")
	}
	if observed["file_edit"] == 0 {
		t.Fatal("repository-repair did not call the production file_edit tool")
	}
	if observed["bash"] < 2 {
		t.Fatal("repository-repair did not run both diagnostic and verification tests")
	}

	snapshot := audit.snapshot()
	if snapshot.successfulEdits != 1 {
		t.Fatalf(
			"repository-repair successful edit effects=%d, want exactly 1",
			snapshot.successfulEdits,
		)
	}
	if snapshot.duplicateEditEffects != 0 {
		t.Fatalf(
			"repository-repair duplicate edit effects=%d, want 0",
			snapshot.duplicateEditEffects,
		)
	}
	if snapshot.offTargetEditEffects != 0 {
		t.Fatalf(
			"repository-repair off-target edit effects=%d, want 0",
			snapshot.offTargetEditEffects,
		)
	}
	if snapshot.bashSourceMutations != 0 {
		t.Fatalf(
			"repository-repair bash source mutations=%d, want 0",
			snapshot.bashSourceMutations,
		)
	}
	if !snapshot.sawFailingTestBeforeEdit {
		t.Fatal("repository-repair did not observe a failing go test before editing")
	}
	if !snapshot.sawPassingTestAfterEdit {
		t.Fatal("repository-repair did not run a passing go test after editing")
	}
}

type koeRepoRepairAuditSnapshot struct {
	successfulEdits          int
	duplicateEditEffects     int
	offTargetEditEffects     int
	bashSourceMutations      int
	sawFailingTestBeforeEdit bool
	sawPassingTestAfterEdit  bool
}

type koeRepoRepairAudit struct {
	repoDir    string
	targetPath string

	mu                        sync.Mutex
	sequence                  int
	firstSuccessfulEdit       int
	successfulEdits           int
	duplicateEditEffects      int
	offTargetEditEffects      int
	bashSourceMutations       int
	successfulEffectDigests   map[[32]byte]int
	failingTestCompletions    []int
	successfulTestCompletions []int
}

func newKoeRepoRepairAudit(repoDir string) *koeRepoRepairAudit {
	return &koeRepoRepairAudit{
		repoDir:                 repoDir,
		targetPath:              filepath.Join(repoDir, koeRepoRepairTargetFile),
		successfulEffectDigests: make(map[[32]byte]int),
	}
}

func (a *koeRepoRepairAudit) recordEdit(
	args fileEditArgs,
	before [32]byte,
	after [32]byte,
	result agent.ToolResult,
	runErr error,
) {
	if runErr != nil || result.IsError || before == after {
		return
	}
	path := args.Path
	if !filepath.IsAbs(path) {
		path = filepath.Join(a.repoDir, path)
	}
	path = filepath.Clean(path)
	digest := sha256.Sum256([]byte(strings.Join([]string{
		path,
		args.OldString,
		args.NewString,
		fmt.Sprintf("%t", args.ReplaceAll),
	}, "\x00")))

	a.mu.Lock()
	defer a.mu.Unlock()
	a.sequence++
	a.successfulEdits++
	if a.firstSuccessfulEdit == 0 {
		a.firstSuccessfulEdit = a.sequence
	}
	a.successfulEffectDigests[digest]++
	if a.successfulEffectDigests[digest] > 1 {
		a.duplicateEditEffects++
	}
	if path != filepath.Clean(a.targetPath) {
		a.offTargetEditEffects++
	}
}

func (a *koeRepoRepairAudit) recordBash(
	command string,
	sourceBefore [32]byte,
	sourceAfter [32]byte,
	result agent.ToolResult,
	runErr error,
) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if sourceBefore != sourceAfter {
		a.bashSourceMutations++
	}
	if !koeRepoRepairIsGoTestAll(command) {
		return
	}
	a.sequence++
	if runErr != nil || result.IsError {
		a.failingTestCompletions = append(a.failingTestCompletions, a.sequence)
		return
	}
	a.successfulTestCompletions = append(a.successfulTestCompletions, a.sequence)
}

func (a *koeRepoRepairAudit) snapshot() koeRepoRepairAuditSnapshot {
	a.mu.Lock()
	defer a.mu.Unlock()
	snapshot := koeRepoRepairAuditSnapshot{
		successfulEdits:      a.successfulEdits,
		duplicateEditEffects: a.duplicateEditEffects,
		offTargetEditEffects: a.offTargetEditEffects,
		bashSourceMutations:  a.bashSourceMutations,
	}
	for _, sequence := range a.failingTestCompletions {
		if a.firstSuccessfulEdit > 0 && sequence < a.firstSuccessfulEdit {
			snapshot.sawFailingTestBeforeEdit = true
			break
		}
	}
	for _, sequence := range a.successfulTestCompletions {
		if a.firstSuccessfulEdit > 0 && sequence > a.firstSuccessfulEdit {
			snapshot.sawPassingTestAfterEdit = true
			break
		}
	}
	return snapshot
}

func (a *koeRepoRepairAudit) successfulEditCount() int {
	return a.snapshot().successfulEdits
}

func (a *koeRepoRepairAudit) duplicateEditEffectCount() int {
	return a.snapshot().duplicateEditEffects
}

func koeRepoRepairIsGoTestAll(command string) bool {
	return strings.TrimSpace(command) == "go test ./..."
}

func koeRepoRepairFileHash(path string) [32]byte {
	data, err := os.ReadFile(path)
	if err != nil {
		return [32]byte{}
	}
	return sha256.Sum256(data)
}

type koeRepoRepairRecordingFileEdit struct {
	inner *FileEditTool
	audit *koeRepoRepairAudit
}

func (t *koeRepoRepairRecordingFileEdit) Info() agent.ToolInfo {
	return t.inner.Info()
}

func (t *koeRepoRepairRecordingFileEdit) Run(
	ctx context.Context,
	argsJSON string,
) (agent.ToolResult, error) {
	var args fileEditArgs
	_ = json.Unmarshal([]byte(argsJSON), &args)
	path := args.Path
	if !filepath.IsAbs(path) {
		path = filepath.Join(t.audit.repoDir, path)
	}
	before := koeRepoRepairFileHash(filepath.Clean(path))
	result, err := t.inner.Run(ctx, argsJSON)
	after := koeRepoRepairFileHash(filepath.Clean(path))
	t.audit.recordEdit(args, before, after, result, err)
	return result, err
}

func (t *koeRepoRepairRecordingFileEdit) RequiresApproval() bool {
	return t.inner.RequiresApproval()
}

func (t *koeRepoRepairRecordingFileEdit) IsReadOnlyCall(args string) bool {
	return t.inner.IsReadOnlyCall(args)
}

type koeRepoRepairRecordingBash struct {
	inner *BashTool
	audit *koeRepoRepairAudit
}

func (t *koeRepoRepairRecordingBash) Info() agent.ToolInfo {
	return t.inner.Info()
}

func (t *koeRepoRepairRecordingBash) Run(
	ctx context.Context,
	argsJSON string,
) (agent.ToolResult, error) {
	var args bashArgs
	_ = json.Unmarshal([]byte(argsJSON), &args)
	sourceBefore := koeRepoRepairFileHash(t.audit.targetPath)
	if !koeRepoRepairIsGoTestAll(args.Command) {
		result := agent.ValidationError(
			"repository-repair live test permits bash only for exact `go test ./...`",
		)
		t.audit.recordBash(args.Command, sourceBefore, sourceBefore, result, nil)
		return result, nil
	}
	result, err := t.inner.Run(ctx, argsJSON)
	sourceAfter := koeRepoRepairFileHash(t.audit.targetPath)
	t.audit.recordBash(args.Command, sourceBefore, sourceAfter, result, err)
	return result, err
}

func (t *koeRepoRepairRecordingBash) RequiresApproval() bool {
	return t.inner.RequiresApproval()
}

func (t *koeRepoRepairRecordingBash) IsReadOnlyCall(args string) bool {
	return t.inner.IsReadOnlyCall(args)
}

func (t *koeRepoRepairRecordingBash) IsConcurrencySafeCall(args string) bool {
	return t.inner.IsConcurrencySafeCall(args)
}

func (t *koeRepoRepairRecordingBash) IsSafeArgs(args string) bool {
	return t.inner.IsSafeArgs(args)
}
