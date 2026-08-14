//go:build live

package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/config"
	"github.com/Kocoro-lab/ShanClaw/internal/executionprofile"
	"github.com/Kocoro-lab/ShanClaw/internal/tools"
)

const (
	generalOutcomeGateEnv        = "KOCORO_GENERAL_OUTCOME_LIVE"
	generalOutcomeSampleEnv      = "KOCORO_GENERAL_OUTCOME_SAMPLE"
	generalOutcomeRepetitionsEnv = "KOCORO_GENERAL_OUTCOME_REPETITIONS"
	generalOutcomeSeedEnv        = "KOCORO_GENERAL_OUTCOME_SEED"
	generalOutcomeMaxCostEnv     = "KOCORO_GENERAL_OUTCOME_MAX_COST_USD"
	generalOutcomeOutputEnv      = "KOCORO_GENERAL_OUTCOME_OUTPUT"
	generalOutcomeModelEnv       = "KOCORO_GENERAL_OUTCOME_MODEL"
	generalOutcomeEffortEnv      = "KOCORO_GENERAL_OUTCOME_EFFORT"

	generalOutcomeComparisonRepetitions = 1
	generalOutcomeReleaseRepetitions    = 5
	generalOutcomeDefaultSeed           = int64(20260813)
)

type generalOutcomeLiveConfig struct {
	endpoint      string
	apiKey        string
	modelTier     string
	specificModel string
	effortTier    string
	sample        string
	repetitions   int
	seed          int64
	maxCostUSD    float64
	outputPath    string
}

type generalOutcomeJob struct {
	TaskIndex  int
	Repetition int
}

type generalOutcomeRun struct {
	TaskID              string         `json:"task_id"`
	Category            string         `json:"category"`
	ExpectedStatus      string         `json:"expected_status"`
	ObservedStatus      string         `json:"observed_status"`
	Repetition          int            `json:"repetition"`
	ScheduleIndex       int            `json:"schedule_index"`
	Correct             bool           `json:"correct"`
	Failures            []string       `json:"failures"`
	Answer              string         `json:"answer"`
	ToolCalls           map[string]int `json:"tool_calls"`
	Receipts            []string       `json:"receipts"`
	LatencyMillis       int64          `json:"latency_millis"`
	LLMCalls            int            `json:"llm_calls"`
	InputTokens         int            `json:"input_tokens"`
	OutputTokens        int            `json:"output_tokens"`
	TotalTokens         int            `json:"total_tokens"`
	CostUSD             float64        `json:"cost_usd"`
	UsageObserved       bool           `json:"usage_observed"`
	CostObserved        bool           `json:"cost_observed"`
	ResponseCachePolicy string         `json:"response_cache_policy"`
}

type generalOutcomeReport struct {
	SchemaVersion                string              `json:"schema_version"`
	GeneratedAt                  string              `json:"generated_at"`
	Complete                     bool                `json:"complete"`
	Sample                       string              `json:"sample"`
	SpecificModel                string              `json:"specific_model,omitempty"`
	EffortTier                   string              `json:"effort_tier,omitempty"`
	RepetitionsPerTask           int                 `json:"repetitions_per_task"`
	MinimumComparisonRepetitions int                 `json:"minimum_comparison_repetitions"`
	MinimumReleaseRepetitions    int                 `json:"minimum_release_repetitions"`
	Seed                         int64               `json:"seed"`
	Randomized                   bool                `json:"randomized"`
	Scheduled                    int                 `json:"scheduled"`
	Completed                    int                 `json:"completed"`
	CorrectRuns                  int                 `json:"correct_runs"`
	ReportedCostUSD              float64             `json:"reported_cost_usd"`
	MaxCostUSD                   float64             `json:"max_cost_usd"`
	UsageObserved                bool                `json:"usage_observed"`
	CostObserved                 bool                `json:"cost_observed"`
	ComparisonQualifying         bool                `json:"comparison_qualifying"`
	ReleaseQualifying            bool                `json:"release_qualifying"`
	Runs                         []generalOutcomeRun `json:"runs"`
	CoverageBoundaries           []string            `json:"coverage_boundaries"`
}

type generalOutcomeCacheOffClient struct {
	inner    client.LLMClient
	mu       sync.Mutex
	policies []executionprofile.ResponseCachePolicy
}

func (c *generalOutcomeCacheOffClient) Complete(ctx context.Context, request client.CompletionRequest) (*client.CompletionResponse, error) {
	request.ResponseCachePolicy = executionprofile.ResponseCacheOff
	c.record(request.ResponseCachePolicy)
	return c.inner.Complete(ctx, request)
}

func (c *generalOutcomeCacheOffClient) CompleteStream(ctx context.Context, request client.CompletionRequest, onDelta func(client.StreamDelta)) (*client.CompletionResponse, error) {
	request.ResponseCachePolicy = executionprofile.ResponseCacheOff
	c.record(request.ResponseCachePolicy)
	return c.inner.CompleteStream(ctx, request, onDelta)
}

func (c *generalOutcomeCacheOffClient) record(policy executionprofile.ResponseCachePolicy) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.policies = append(c.policies, policy)
}

func (c *generalOutcomeCacheOffClient) allOff() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.policies) == 0 {
		return false
	}
	for _, policy := range c.policies {
		if policy != executionprofile.ResponseCacheOff {
			return false
		}
	}
	return true
}

type generalOutcomeSandbox struct {
	mu       sync.Mutex
	root     string
	initial  generalOutcomeState
	external map[string]generalOutcomeExternal
	effects  map[string]string
	calls    map[string]int
	receipts []string
}

func newGeneralOutcomeSandbox(t *testing.T, state generalOutcomeState) *generalOutcomeSandbox {
	t.Helper()
	root := neutralTempDir(t, "general-outcome-*")
	for relative, content := range state.Files {
		path, err := safeGeneralOutcomePath(root, relative)
		if err != nil {
			t.Fatalf("invalid fixture path %q: %v", relative, err)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("create fixture directory: %v", err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatalf("write fixture %s: %v", relative, err)
		}
	}
	return &generalOutcomeSandbox{
		root: root,
		initial: generalOutcomeState{
			Files:    cloneGeneralOutcomeMap(state.Files),
			External: cloneGeneralOutcomeExternalMap(state.External),
			Effects:  cloneGeneralOutcomeMap(state.Effects),
		},
		external: cloneGeneralOutcomeExternalMap(state.External),
		effects:  cloneGeneralOutcomeMap(state.Effects),
		calls:    map[string]int{},
	}
}

func (s *generalOutcomeSandbox) recordCall(name string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls[name]++
	return s.calls[name]
}

func (s *generalOutcomeSandbox) addReceipt(receipt string) {
	if receipt == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.receipts = append(s.receipts, receipt)
}

func (s *generalOutcomeSandbox) snapshot(t *testing.T) generalOutcomeObservation {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(s.root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(s.root, path)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files[filepath.ToSlash(relative)] = string(data)
		return nil
	})
	if err != nil {
		t.Fatalf("snapshot outcome sandbox: %v", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return generalOutcomeObservation{
		Calls:    cloneGeneralOutcomeIntMap(s.calls),
		Receipts: append([]string(nil), s.receipts...),
		State: generalOutcomeState{
			Files:    files,
			External: cloneGeneralOutcomeExternalMap(s.external),
			Effects:  cloneGeneralOutcomeMap(s.effects),
		},
	}
}

func (s *generalOutcomeSandbox) assertExternalUnchanged(t *testing.T) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if !reflectGeneralOutcomeExternalEqual(s.external, s.initial.External) {
		t.Fatal("synthetic tool mutated external fixture state")
	}
}

type generalOutcomeRecordedTool struct {
	inner   agent.Tool
	sandbox *generalOutcomeSandbox
}

func (t *generalOutcomeRecordedTool) Info() agent.ToolInfo   { return t.inner.Info() }
func (t *generalOutcomeRecordedTool) RequiresApproval() bool { return t.inner.RequiresApproval() }
func (t *generalOutcomeRecordedTool) Run(ctx context.Context, args string) (agent.ToolResult, error) {
	if result, valid := validateGeneralOutcomeToolPath(t.sandbox.root, args); !valid {
		return result, nil
	}
	t.sandbox.recordCall(t.inner.Info().Name)
	return t.inner.Run(ctx, args)
}

func (t *generalOutcomeRecordedTool) IsReadOnlyCall(args string) bool {
	if value, ok := t.inner.(agent.ReadOnlyChecker); ok {
		return value.IsReadOnlyCall(args)
	}
	return false
}

func (t *generalOutcomeRecordedTool) IsConcurrencySafeCall(args string) bool {
	if value, ok := t.inner.(agent.ConcurrencySafeChecker); ok {
		return value.IsConcurrencySafeCall(args)
	}
	return t.IsReadOnlyCall(args)
}

type generalOutcomeExternalTool struct {
	name        string
	description string
	sandbox     *generalOutcomeSandbox
}

func (t *generalOutcomeExternalTool) Info() agent.ToolInfo {
	properties := map[string]any{"description": agent.DescriptionFieldSpec}
	required := []string{"description"}
	switch t.name {
	case "web_fetch":
		properties["url"] = map[string]any{"type": "string"}
		required = append([]string{"url"}, required...)
	case "calendar_create":
		properties["id"] = map[string]any{"type": "string"}
		properties["start"] = map[string]any{"type": "string"}
		required = append([]string{"id", "start"}, required...)
	case "send_email":
		properties["to"] = map[string]any{"type": "string"}
		properties["body"] = map[string]any{"type": "string"}
		required = append([]string{"to", "body"}, required...)
	}
	return agent.ToolInfo{Name: t.name, Description: t.description + agent.DescriptionGuidance, Parameters: map[string]any{"type": "object", "properties": properties}, Required: required}
}

func (t *generalOutcomeExternalTool) RequiresApproval() bool     { return false }
func (t *generalOutcomeExternalTool) IsReadOnlyCall(string) bool { return t.name == "web_fetch" }
func (t *generalOutcomeExternalTool) IsConcurrencySafeCall(args string) bool {
	return t.IsReadOnlyCall(args)
}

func (t *generalOutcomeExternalTool) Run(_ context.Context, argsJSON string) (agent.ToolResult, error) {
	if result, valid := agent.ValidateToolArguments(t.Info(), argsJSON); !valid {
		return result, nil
	}
	var args struct {
		URL   string `json:"url"`
		ID    string `json:"id"`
		Start string `json:"start"`
		To    string `json:"to"`
		Body  string `json:"body"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return agent.ValidationError(fmt.Sprintf("invalid arguments: %v", err)), nil
	}
	callNumber := t.sandbox.recordCall(t.name)
	key := args.URL
	if t.name == "calendar_create" {
		key = "calendar:" + args.ID
	}
	if t.name == "send_email" {
		key = "email:" + args.To
	}
	t.sandbox.mu.Lock()
	record, exists := t.sandbox.external[key]
	t.sandbox.mu.Unlock()
	if !exists {
		return agent.BusinessError("synthetic resource not found"), nil
	}
	if record.Status == "transient" && callNumber < record.SucceedAfter {
		return agent.TransientError("transient upstream error; retry is safe"), nil
	}
	t.sandbox.addReceipt(record.Receipt)
	switch record.Status {
	case "ok", "transient":
		return agent.ToolResult{Content: record.Content + "\nsource=" + key + "\nreceipt=" + record.Receipt}, nil
	case "blocked", "not_found", "business_error":
		return agent.BusinessError(record.Content + "\nreceipt=" + record.Receipt), nil
	case "outcome_unknown":
		t.sandbox.mu.Lock()
		t.sandbox.effects[key] = "outcome_unknown"
		t.sandbox.mu.Unlock()
		return agent.ToolResult{
			Content: record.Content + "; outcome unknown; do not replay\nreceipt=" + record.Receipt,
			IsError: true, SideEffectOutcomeUnknown: true,
		}, nil
	default:
		return agent.BusinessError("invalid synthetic status"), nil
	}
}

func TestLive_GeneralAgentOutcomeDataset(t *testing.T) {
	if os.Getenv(generalOutcomeGateEnv) != "1" {
		t.Skip("set KOCORO_GENERAL_OUTCOME_LIVE=1 to authorize the paid general-agent outcome lane")
	}
	if os.Getenv("SHANNON_E2E_LIVE") != "1" {
		t.Fatal("also set SHANNON_E2E_LIVE=1 to authorize real provider calls")
	}
	dataset := loadGeneralOutcomeDataset(t)
	cfg := loadGeneralOutcomeLiveConfig(t)
	jobs := buildGeneralOutcomeJobs(len(dataset.Tasks), cfg.repetitions, cfg.seed)
	provider := client.NewGatewayClient(cfg.endpoint, cfg.apiKey)
	runs := make([]generalOutcomeRun, 0, len(jobs))
	for index, job := range jobs {
		run := runGeneralOutcomeTask(t, provider, cfg, dataset.Tasks[job.TaskIndex], job.Repetition, index+1)
		runs = append(runs, run)
		t.Logf("general_outcome task=%s rep=%d status=%s correct=%t failures=%v latency_ms=%d cost_usd=%.8f", run.TaskID, run.Repetition, run.ObservedStatus, run.Correct, run.Failures, run.LatencyMillis, run.CostUSD)
		if totalGeneralOutcomeCost(runs) > cfg.maxCostUSD {
			break
		}
	}
	report := newGeneralOutcomeReport(cfg, jobs, runs)
	if err := writeGeneralOutcomeReport(cfg.outputPath, report); err != nil {
		t.Fatalf("write general-agent outcome report: %v", err)
	}
	if !report.Complete {
		t.Fatalf("outcome lane incomplete: completed=%d scheduled=%d cost=%.8f cap=%.8f report=%s", report.Completed, report.Scheduled, report.ReportedCostUSD, report.MaxCostUSD, cfg.outputPath)
	}
	if report.CorrectRuns != report.Completed {
		t.Fatalf("outcome lane failed: correct=%d completed=%d report=%s", report.CorrectRuns, report.Completed, cfg.outputPath)
	}
	if !report.ComparisonQualifying {
		t.Fatalf("outcome comparison did not qualify: cost=%.8f cap=%.8f repetitions=%d report=%s",
			report.ReportedCostUSD, report.MaxCostUSD, report.RepetitionsPerTask, cfg.outputPath)
	}
	if cfg.sample == "release" && !report.ReleaseQualifying {
		t.Fatalf("outcome release did not qualify: usage=%t cost=%t reported_cost=%.8f max_cost=%.8f repetitions=%d report=%s",
			report.UsageObserved, report.CostObserved, report.ReportedCostUSD, report.MaxCostUSD, report.RepetitionsPerTask, cfg.outputPath)
	}
}

func runGeneralOutcomeTask(t *testing.T, provider client.LLMClient, cfg generalOutcomeLiveConfig, task generalOutcomeTask, repetition, scheduleIndex int) generalOutcomeRun {
	t.Helper()
	sandbox := newGeneralOutcomeSandbox(t, task.InitialState)
	registry := agent.NewToolRegistry()
	for _, name := range task.AllowedTools {
		switch name {
		case "file_read":
			registry.Register(&generalOutcomeRecordedTool{inner: &tools.FileReadTool{}, sandbox: sandbox})
		case "grep":
			registry.Register(&generalOutcomeRecordedTool{inner: &tools.GrepTool{}, sandbox: sandbox})
		case "file_write":
			registry.Register(&generalOutcomeRecordedTool{inner: &tools.FileWriteTool{}, sandbox: sandbox})
		case "file_edit":
			registry.Register(&generalOutcomeRecordedTool{inner: &tools.FileEditTool{}, sandbox: sandbox})
		case "web_fetch":
			registry.Register(&generalOutcomeExternalTool{name: name, description: "Read one synthetic web record by exact URL.", sandbox: sandbox})
		case "calendar_create":
			registry.Register(&generalOutcomeExternalTool{name: name, description: "Create one synthetic calendar event.", sandbox: sandbox})
		case "send_email":
			registry.Register(&generalOutcomeExternalTool{name: name, description: "Send one synthetic email.", sandbox: sandbox})
		default:
			t.Fatalf("task %s requested unsupported tool %s", task.ID, name)
		}
	}
	wrapper := &generalOutcomeCacheOffClient{inner: provider}
	loop := agent.NewAgentLoop(wrapper, registry, cfg.modelTier, t.TempDir(), 6, 30_000, 200, nil, nil, nil)
	loop.SetCacheSource("general_agent_outcome")
	loop.SetSkillDiscovery(false)
	loop.SetSessionCWD(sandbox.root)
	loop.SetBypassPermissions(true)
	loop.SetMaxTokens(700)
	loop.SetTemperature(0)
	if cfg.effortTier != "" {
		loop.SetEffortTier(cfg.effortTier)
	}
	if task.Source != "" {
		loop.SetSource(task.Source)
	}
	if cfg.specificModel != "" {
		loop.SetSpecificModel(cfg.specificModel)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	started := time.Now()
	answer, usage, err := loop.Run(ctx, task.Prompt, nil, nil)
	observed := sandbox.snapshot(t)
	observed.Answer = strings.TrimSpace(answer)
	observed.Status = deriveGeneralOutcomeStatus(task, observed)
	sandbox.assertExternalUnchanged(t)
	failures := evaluateGeneralOutcome(task, observed)
	if err != nil && !(task.Oracle.ExpectedStatus == "outcome_unknown" && errors.Is(err, agent.ErrSideEffectOutcomeUnknown)) {
		failures = append(failures, "provider_or_loop_error:"+err.Error())
	}
	if !wrapper.allOff() {
		failures = append(failures, "response_cache_policy_not_off")
	}
	run := generalOutcomeRun{
		TaskID: task.ID, Category: task.Category, ExpectedStatus: task.Oracle.ExpectedStatus,
		ObservedStatus: observed.Status, Repetition: repetition, ScheduleIndex: scheduleIndex,
		Correct: len(failures) == 0, Failures: uniqueGeneralOutcomeFailures(failures), Answer: observed.Answer,
		ToolCalls: observed.Calls, Receipts: observed.Receipts, LatencyMillis: time.Since(started).Milliseconds(),
		ResponseCachePolicy: string(executionprofile.ResponseCacheOff),
	}
	if usage != nil {
		run.LLMCalls = usage.LLMCalls
		run.InputTokens = usage.InputTokens
		run.OutputTokens = usage.OutputTokens
		run.TotalTokens = usage.TotalTokens
		run.CostUSD = usage.CostUSD
		run.UsageObserved = usage.LLMCalls > 0 && usage.TotalTokens > 0
		run.CostObserved = usage.LLMCalls > 0 && usage.CostUSD > 0
	}
	if !run.UsageObserved {
		run.Failures = uniqueGeneralOutcomeFailures(append(run.Failures, "usage_not_observed"))
		run.Correct = false
	}
	if cfg.sample == "release" && !run.CostObserved {
		run.Failures = uniqueGeneralOutcomeFailures(append(run.Failures, "cost_not_observed"))
		run.Correct = false
	}
	return run
}

func loadGeneralOutcomeLiveConfig(t *testing.T) generalOutcomeLiveConfig {
	t.Helper()
	endpoint := strings.TrimSpace(os.Getenv("SHANNON_E2E_ENDPOINT"))
	apiKey := strings.TrimSpace(os.Getenv("SHANNON_E2E_API_KEY"))
	modelTier := "medium"
	specificModel := ""
	loaded, err := config.Load()
	if err != nil && (endpoint == "" || apiKey == "") {
		t.Fatalf("load configured Cloud access: %v", err)
	}
	if loaded != nil {
		if endpoint == "" {
			endpoint = loaded.Endpoint
		}
		if apiKey == "" {
			apiKey = loaded.APIKey
		}
		if loaded.ModelTier != "" {
			modelTier = loaded.ModelTier
		}
		specificModel = loaded.Agent.Model
	}
	if modelOverride := strings.TrimSpace(os.Getenv(generalOutcomeModelEnv)); modelOverride != "" {
		specificModel = modelOverride
	}
	effortTier := strings.TrimSpace(os.Getenv(generalOutcomeEffortEnv))
	if effortTier != "" && effortTier != "low" && effortTier != "high" && effortTier != "xhigh" && effortTier != "max" {
		t.Fatalf("%s must be low, high, xhigh, or max", generalOutcomeEffortEnv)
	}
	if endpoint == "" || apiKey == "" {
		t.Fatal("general-agent outcome lane needs SHANNON_E2E_ENDPOINT/SHANNON_E2E_API_KEY or configured Cloud credentials; set KOCORO_FORCE_KEYCHAIN_HYDRATE=1 to authorize test-only credential hydration")
	}
	sample := strings.TrimSpace(os.Getenv(generalOutcomeSampleEnv))
	if sample == "" {
		sample = "comparison"
	}
	if sample != "comparison" && sample != "release" {
		t.Fatalf("%s must be comparison or release", generalOutcomeSampleEnv)
	}
	repetitions := generalOutcomeComparisonRepetitions
	if sample == "release" {
		repetitions = generalOutcomeReleaseRepetitions
	}
	if raw := strings.TrimSpace(os.Getenv(generalOutcomeRepetitionsEnv)); raw != "" {
		value, parseErr := strconv.Atoi(raw)
		if parseErr != nil || value < 1 || value > 20 {
			t.Fatalf("%s must be an integer in [1,20]", generalOutcomeRepetitionsEnv)
		}
		repetitions = value
	}
	if sample == "release" && repetitions < generalOutcomeReleaseRepetitions {
		t.Fatalf("release sample requires %s >= %d", generalOutcomeRepetitionsEnv, generalOutcomeReleaseRepetitions)
	}
	seed := generalOutcomeDefaultSeed
	if raw := strings.TrimSpace(os.Getenv(generalOutcomeSeedEnv)); raw != "" {
		value, parseErr := strconv.ParseInt(raw, 10, 64)
		if parseErr != nil {
			t.Fatalf("%s must be a signed 64-bit integer", generalOutcomeSeedEnv)
		}
		seed = value
	}
	maxCost := 5.0
	if raw := strings.TrimSpace(os.Getenv(generalOutcomeMaxCostEnv)); raw != "" {
		value, parseErr := strconv.ParseFloat(raw, 64)
		if parseErr != nil || math.IsNaN(value) || math.IsInf(value, 0) || value <= 0 || value > 50 {
			t.Fatalf("%s must be greater than 0 and at most 50", generalOutcomeMaxCostEnv)
		}
		maxCost = value
	}
	output := strings.TrimSpace(os.Getenv(generalOutcomeOutputEnv))
	if output == "" {
		output = filepath.Join(os.TempDir(), fmt.Sprintf("kocoro-general-outcomes-%d.json", seed))
	}
	return generalOutcomeLiveConfig{endpoint: endpoint, apiKey: apiKey, modelTier: modelTier, specificModel: specificModel, effortTier: effortTier, sample: sample, repetitions: repetitions, seed: seed, maxCostUSD: maxCost, outputPath: output}
}

func buildGeneralOutcomeJobs(taskCount, repetitions int, seed int64) []generalOutcomeJob {
	jobs := make([]generalOutcomeJob, 0, taskCount*repetitions)
	for repetition := 1; repetition <= repetitions; repetition++ {
		for taskIndex := 0; taskIndex < taskCount; taskIndex++ {
			jobs = append(jobs, generalOutcomeJob{TaskIndex: taskIndex, Repetition: repetition})
		}
	}
	rng := rand.New(rand.NewSource(seed))
	rng.Shuffle(len(jobs), func(i, j int) { jobs[i], jobs[j] = jobs[j], jobs[i] })
	return jobs
}

func newGeneralOutcomeReport(cfg generalOutcomeLiveConfig, jobs []generalOutcomeJob, runs []generalOutcomeRun) generalOutcomeReport {
	report := generalOutcomeReport{
		SchemaVersion: generalOutcomeSchemaVersion, GeneratedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Complete: len(runs) == len(jobs), Sample: cfg.sample, SpecificModel: cfg.specificModel, EffortTier: cfg.effortTier, RepetitionsPerTask: cfg.repetitions,
		MinimumComparisonRepetitions: generalOutcomeComparisonRepetitions, MinimumReleaseRepetitions: generalOutcomeReleaseRepetitions,
		Seed: cfg.seed, Randomized: true, Scheduled: len(jobs), Completed: len(runs), MaxCostUSD: cfg.maxCostUSD,
		UsageObserved: len(runs) > 0, CostObserved: len(runs) > 0, Runs: append([]generalOutcomeRun(nil), runs...),
		CoverageBoundaries: []string{
			"Uses deterministic answer, evidence, receipt, and final-state oracles; no LLM judge.",
			"Exercises the production AgentLoop and real configured provider with response_cache_policy=off.",
			"File and external effects are confined to a per-run synthetic sandbox; no real email, calendar, or web system is mutated.",
			"Comparison defaults to one run per task; release requires at least five complete all-pass runs per task with usage and cost evidence.",
		},
	}
	for _, run := range runs {
		if run.Correct {
			report.CorrectRuns++
		}
		if !run.UsageObserved {
			report.UsageObserved = false
		}
		if !run.CostObserved {
			report.CostObserved = false
		}
		report.ReportedCostUSD += run.CostUSD
	}
	qualificationRuns := make([]generalOutcomeQualificationRun, len(runs))
	for index, run := range runs {
		qualificationRuns[index] = generalOutcomeQualificationRun{
			Correct: run.Correct, UsageObserved: run.UsageObserved, CostObserved: run.CostObserved,
		}
	}
	report.ComparisonQualifying, report.ReleaseQualifying = generalOutcomeQualification(
		cfg.sample, cfg.repetitions, len(jobs), report.ReportedCostUSD, report.MaxCostUSD,
		generalOutcomeCoverageComplete(cfg.repetitions, len(jobs), runs), qualificationRuns,
	)
	return report
}

func generalOutcomeCoverageComplete(repetitions, scheduled int, runs []generalOutcomeRun) bool {
	if repetitions < 1 || scheduled < 1 || len(runs) != scheduled || scheduled%repetitions != 0 {
		return false
	}
	seen := make(map[string]bool, len(runs))
	tasks := make(map[string]bool, scheduled/repetitions)
	for _, run := range runs {
		if run.TaskID == "" || run.Repetition < 1 || run.Repetition > repetitions {
			return false
		}
		cell := fmt.Sprintf("%s/%d", run.TaskID, run.Repetition)
		if seen[cell] {
			return false
		}
		seen[cell] = true
		tasks[run.TaskID] = true
	}
	return len(tasks)*repetitions == scheduled && len(seen) == scheduled
}

func totalGeneralOutcomeCost(runs []generalOutcomeRun) float64 {
	total := 0.0
	for _, run := range runs {
		total += run.CostUSD
	}
	return total
}

func writeGeneralOutcomeReport(path string, report generalOutcomeReport) error {
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o600)
}

func safeGeneralOutcomePath(root, relative string) (string, error) {
	if filepath.IsAbs(relative) {
		return "", fmt.Errorf("absolute path")
	}
	clean := filepath.Clean(relative)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes sandbox")
	}
	return filepath.Join(root, clean), nil
}

func reflectGeneralOutcomeExternalEqual(a, b map[string]generalOutcomeExternal) bool {
	if len(a) != len(b) {
		return false
	}
	keys := make([]string, 0, len(a))
	for key := range a {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if a[key] != b[key] {
			return false
		}
	}
	return true
}
