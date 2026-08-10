package e2e

// Fast-pinned vs Full A/B on moderately complex tasks.
//
// Product question: if the voice front-end stops routing (no Realtime
// execution_mode classifier) and pins the Fast profile, can Fast still finish
// moderately complex work — and is it actually faster/cheaper? If not, Fast
// should be deleted rather than routed around.
//
// Both arms run the production AgentLoop with real local tools against the
// real provider. The Fast arm resolves and pins the Cloud kfp1 profile
// exactly like the daemon does; the Full arm mirrors the qualification
// harness control (the qualification control model + adaptive thinking +
// Full profile). Every case has
// a deterministic validator; no LLM judging.
//
// Run:
//
//	SHANNON_E2E_LIVE=1 KOCORO_FAST_PINNED_AB=1 go test ./test/e2e/ -run TestLive_FastPinnedVsFullAB -timeout 60m -v
//
// Release qualification:
//
//	SHANNON_E2E_LIVE=1 KOCORO_FAST_PINNED_AB=1 KOCORO_FAST_PINNED_AB_SAMPLE=release KOCORO_FAST_PINNED_AB_REPETITIONS=15 go test ./test/e2e/ -run TestLive_FastPinnedVsFullAB -timeout 60m -v

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	agentsapi "github.com/Kocoro-lab/ShanClaw/internal/agents"
	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/executionprofile"
	"github.com/Kocoro-lab/ShanClaw/internal/tools"
)

const (
	fastPinnedABGateEnv   = "KOCORO_FAST_PINNED_AB"
	fastPinnedABRepsEnv   = "KOCORO_FAST_PINNED_AB_REPETITIONS"
	fastPinnedABOutputEnv = "KOCORO_FAST_PINNED_AB_OUTPUT"
	fastPinnedABSampleEnv = "KOCORO_FAST_PINNED_AB_SAMPLE"
	// Runtime model id for the Full control arm — the same control the
	// 420-run qualification lane uses (koeQualificationSonnetModel). This is
	// request configuration, not a tooling reference.
	fastPinnedABFullModel  = "claude-sonnet-5"
	fastPinnedABMaxCostUSD = 5.0
	fastPinnedABSeed       = int64(20260810)
)

type fastPinnedCacheOffClient struct {
	inner client.LLMClient

	mu       sync.Mutex
	requests int
	cached   bool
}

func (c *fastPinnedCacheOffClient) Complete(
	ctx context.Context,
	req client.CompletionRequest,
) (*client.CompletionResponse, error) {
	req.ResponseCachePolicy = executionprofile.ResponseCacheOff
	resp, err := c.inner.Complete(ctx, req)
	c.record(resp)
	return resp, err
}

func (c *fastPinnedCacheOffClient) CompleteStream(
	ctx context.Context,
	req client.CompletionRequest,
	onDelta func(client.StreamDelta),
) (*client.CompletionResponse, error) {
	req.ResponseCachePolicy = executionprofile.ResponseCacheOff
	resp, err := c.inner.CompleteStream(ctx, req, onDelta)
	c.record(resp)
	return resp, err
}

func (c *fastPinnedCacheOffClient) record(resp *client.CompletionResponse) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.requests++
	if resp != nil && resp.Cached {
		c.cached = true
	}
}

func (c *fastPinnedCacheOffClient) observations() (isolated bool, cached bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.requests > 0 && !c.cached, c.cached
}

func fastPinnedTrialID(seed int64, caseName string, repetition int) string {
	return fmt.Sprintf("%d/%s/%02d", seed, caseName, repetition)
}

func fastPinnedTrialPrompt(prompt, trialID string) string {
	return prompt + "\n\nEvaluation trial ID: " + trialID +
		". This marker only separates independent evaluation samples; do not mention it in the answer."
}

// fastABHandler auto-approves tool calls (file tools all require approval)
// and counts per-tool invocations for the validators.
type fastABHandler struct {
	mu         sync.Mutex
	toolCalls  map[string]int
	trajectory []agent.RunTraceEvent
	usage      agent.UsageAccumulator
}

func (h *fastABHandler) OnToolCall(name, _ string, _ string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.toolCalls == nil {
		h.toolCalls = map[string]int{}
	}
	h.toolCalls[name]++
}
func (h *fastABHandler) OnToolResult(string, string, string, agent.ToolResult, time.Duration) {}
func (h *fastABHandler) OnText(string)                                                        {}
func (h *fastABHandler) OnPreamble(string)                                                    {}
func (h *fastABHandler) OnStreamDelta(string)                                                 {}
func (h *fastABHandler) OnApprovalNeeded(string, string) bool                                 { return true }
func (h *fastABHandler) OnUsage(usage agent.TurnUsage)                                        { h.usage.Add(usage) }
func (h *fastABHandler) OnCloudAgent(string, string, string)                                  {}
func (h *fastABHandler) OnCloudProgress(int, int)                                             {}
func (h *fastABHandler) OnCloudPlan(string, string, bool)                                     {}

func (h *fastABHandler) OnRunTrace(event agent.RunTraceEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.trajectory = append(h.trajectory, event)
}

func (h *fastABHandler) count(name string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.toolCalls[name]
}

func (h *fastABHandler) trace() []agent.RunTraceEvent {
	h.mu.Lock()
	defer h.mu.Unlock()
	var trace []agent.RunTraceEvent
	for _, event := range h.trajectory {
		if event.Type == agent.RunTraceEventToolOutcome {
			trace = append(trace, event)
		}
	}
	return trace
}

func (h *fastABHandler) loopEvents() []agent.RunTraceEvent {
	h.mu.Lock()
	defer h.mu.Unlock()
	var events []agent.RunTraceEvent
	for _, event := range h.trajectory {
		if event.Type != agent.RunTraceEventToolOutcome {
			events = append(events, event)
		}
	}
	return events
}

func (h *fastABHandler) accumulatedUsage() agent.AccumulatedUsage {
	return h.usage.Snapshot()
}

// chainProbeTool is a five-step task with真实 step-to-step data dependence:
// each step returns the token the next step must present, so the chain cannot
// be parallelized, skipped, or guessed.
type chainProbeTool struct {
	mu        sync.Mutex
	calls     int
	badTokens int
}

func chainProbeToken(step int) string { return fmt.Sprintf("KX%d-7f", step) }

func (t *chainProbeTool) Info() agent.ToolInfo {
	return agent.ToolInfo{
		Name:        "chain_probe",
		Description: "Advance the five-step chained task. Call with step=1 and token=INIT first; every response names the token the next step requires.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"step":  map[string]any{"type": "integer", "description": "Step number 1-5."},
				"token": map[string]any{"type": "string", "description": "Token returned by the previous step (INIT for step 1)."},
			},
		},
		Required: []string{"step", "token"},
	}
}

func (t *chainProbeTool) RequiresApproval() bool     { return false }
func (t *chainProbeTool) IsReadOnlyCall(string) bool { return false }

func (t *chainProbeTool) Run(_ context.Context, argsJSON string) (agent.ToolResult, error) {
	var args struct {
		Step  int    `json:"step"`
		Token string `json:"token"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil || args.Step < 1 || args.Step > 5 {
		return agent.ValidationError("step must be 1-5 and token is required"), nil
	}
	expected := "INIT"
	if args.Step > 1 {
		expected = chainProbeToken(args.Step - 1)
	}
	t.mu.Lock()
	if strings.TrimSpace(args.Token) != expected {
		t.badTokens++
		t.mu.Unlock()
		return agent.ValidationError(fmt.Sprintf("wrong token for step %d", args.Step)), nil
	}
	t.calls++
	t.mu.Unlock()
	if args.Step < 5 {
		return agent.ToolResult{Content: fmt.Sprintf("STEP-%d OK. Next: call step=%d with token=%s.", args.Step, args.Step+1, chainProbeToken(args.Step))}, nil
	}
	return agent.ToolResult{Content: "STEP-5 OK. ALL DONE. Final code: CHAIN-931."}, nil
}

type fastABCase struct {
	name     string
	setup    func(t *testing.T, dir string) string // writes fixtures, returns prompt
	probe    bool
	validate func(answer, dir string, h *fastABHandler, probe *chainProbeTool) []string
}

func fastPinnedABCases() []fastABCase {
	return []fastABCase{
		{
			name: "multi_file_synthesis",
			setup: func(t *testing.T, dir string) string {
				writeFastABFile(t, dir, "budget_engineering.txt", "Engineering department budget: 84500 USD. Lead: Chen Wei.")
				writeFastABFile(t, dir, "budget_marketing.txt", "Marketing department budget: 62300 USD. Lead: Sara Lim.")
				writeFastABFile(t, dir, "budget_ops.txt", "Operations department budget: 47200 USD. Lead: Diego Ruiz.")
				return fmt.Sprintf("目录 %s 下有三个部门的预算文件。读取全部三个文件，然后严格输出两行：\nTOTAL: <三个部门预算总和> USD\nLEAD: <预算最高的部门的负责人姓名>", dir)
			},
			validate: func(answer, _ string, h *fastABHandler, _ *chainProbeTool) []string {
				var failures []string
				normalized := strings.ReplaceAll(answer, ",", "")
				if !strings.Contains(normalized, "194000") {
					failures = append(failures, "wrong_total")
				}
				if !strings.Contains(answer, "Chen Wei") {
					failures = append(failures, "wrong_lead")
				}
				if h.count("file_read") < 3 {
					failures = append(failures, fmt.Sprintf("file_read_%d_lt_3", h.count("file_read")))
				}
				return failures
			},
		},
		{
			name: "constrained_schedule",
			setup: func(_ *testing.T, _ string) string {
				return "安排任务计划。任务与时长：Draft 2小时、Prep 1小时、Review 1小时、Fix 1小时、Final 30分钟。依赖：Review 必须在 Draft 与 Prep 都完成之后；Fix 在 Review 之后；Final 在 Fix 之后。可用时段仅限：Wed 09:00-12:00、Thu 14:00-17:00、Fri 09:00-11:00。最终截止 Fri 11:00。严格输出 5 行，每行格式 `Task | Day HH:MM-HH:MM`，Task 名用上述英文名。"
			},
			validate: func(answer, _ string, _ *fastABHandler, _ *chainProbeTool) []string {
				return validateFastABSchedule(answer)
			},
		},
		{
			name: "error_recovery",
			setup: func(t *testing.T, dir string) string {
				writeFastABFile(t, dir, "actual_notes.txt", "Meeting summary. The launch code name is AURORA-9. Next sync on Friday.")
				return fmt.Sprintf("读取 %s/notes.txt。如果该文件不存在，列出该目录找到真实的笔记文件并读取它。最后只回答 launch code name。", dir)
			},
			validate: func(answer, _ string, h *fastABHandler, _ *chainProbeTool) []string {
				var failures []string
				if !strings.Contains(answer, "AURORA-9") {
					failures = append(failures, "missing_code_name")
				}
				recovered := h.count("file_read") >= 2 ||
					(h.count("file_read") >= 1 && h.count("directory_list")+h.count("glob") >= 1)
				if !recovered {
					failures = append(failures, "no_recovery_trajectory")
				}
				return failures
			},
		},
		{
			name: "data_pipeline",
			setup: func(t *testing.T, dir string) string {
				writeFastABFile(t, dir, "metrics.csv", "day,value\n1,100\n2,110\n3,120\n4,130\n5,125\n6,135\n7,140\n8,160\n")
				return fmt.Sprintf("读取 %s/metrics.csv，计算 value 列的平均值，把这个数值（只写数值）写入 %s/result.txt，然后回复一行 `MEAN: <数值>`。", dir, dir)
			},
			validate: func(answer, dir string, h *fastABHandler, _ *chainProbeTool) []string {
				var failures []string
				if !strings.Contains(answer, "127.5") {
					failures = append(failures, "wrong_mean_in_answer")
				}
				body, err := os.ReadFile(filepath.Join(dir, "result.txt"))
				if err != nil || !strings.Contains(string(body), "127.5") {
					failures = append(failures, "result_file_missing_or_wrong")
				}
				if h.count("file_write") < 1 {
					failures = append(failures, "no_file_write")
				}
				return failures
			},
		},
		{
			name:  "state_chain",
			probe: true,
			setup: func(_ *testing.T, _ string) string {
				return "使用 chain_probe 工具完成整个五步链式任务：从 step=1、token=INIT 开始，每一步的返回会告诉你下一步需要的 token。全部完成后回复 final code。"
			},
			validate: func(answer, _ string, _ *fastABHandler, probe *chainProbeTool) []string {
				var failures []string
				if !strings.Contains(answer, "CHAIN-931") {
					failures = append(failures, "missing_final_code")
				}
				probe.mu.Lock()
				calls, bad := probe.calls, probe.badTokens
				probe.mu.Unlock()
				if calls != 5 {
					failures = append(failures, fmt.Sprintf("chain_calls_%d_not_5", calls))
				}
				if bad != 0 {
					failures = append(failures, fmt.Sprintf("bad_tokens_%d", bad))
				}
				return failures
			},
		},
		{
			name: "doc_reason",
			setup: func(t *testing.T, dir string) string {
				writeFastABFile(t, dir, "meeting.txt",
					"Q3 portfolio review.\n"+
						"Project Atlas lead: Marcus Ford. Delay risk: 3 days. Budget on track.\n"+
						"Project Boreas lead: Elena Voss. Delay risk: 11 days. Vendor issue open.\n"+
						"Project Cirrus lead: Tom Nakai. Delay risk: 6 days. Hiring complete.\n"+
						"Note: Marcus also mentors the intern program. Tom presented last week.\n")
				return fmt.Sprintf("读取 %s/meeting.txt，回答：延期风险最高的项目由谁负责？只回答姓名。", dir)
			},
			validate: func(answer, _ string, _ *fastABHandler, _ *chainProbeTool) []string {
				var failures []string
				if !strings.Contains(answer, "Elena Voss") {
					failures = append(failures, "wrong_person")
				}
				if strings.Contains(answer, "Marcus") || strings.Contains(answer, "Nakai") {
					failures = append(failures, "listed_distractors")
				}
				return failures
			},
		},
	}
}

func writeFastABFile(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatalf("write fixture %s: %v", name, err)
	}
}

// validateFastABSchedule checks the 5-line plan against the stated windows,
// durations, dependencies, and deadline — constraint-based, so any valid
// schedule passes rather than one blessed answer.
func validateFastABSchedule(answer string) []string {
	var failures []string
	linePattern := regexp.MustCompile(`(?m)^\s*(Draft|Prep|Review|Fix|Final)\s*\|\s*(Wed|Thu|Fri)\s+(\d{2}):(\d{2})-(\d{2}):(\d{2})\s*$`)
	matches := linePattern.FindAllStringSubmatch(answer, -1)
	if len(matches) != 5 {
		return append(failures, fmt.Sprintf("schedule_lines_%d_not_5", len(matches)))
	}
	type slot struct{ start, end int } // minutes on a global axis: Wed=0, Thu=10000, Fri=20000
	dayBase := map[string]int{"Wed": 0, "Thu": 10000, "Fri": 20000}
	windows := map[string][2]int{"Wed": {9 * 60, 12 * 60}, "Thu": {14 * 60, 17 * 60}, "Fri": {9 * 60, 11 * 60}}
	durations := map[string]int{"Draft": 120, "Prep": 60, "Review": 60, "Fix": 60, "Final": 30}
	placed := map[string]slot{}
	var intervals []slot
	for _, m := range matches {
		task, day := m[1], m[2]
		sh, _ := strconv.Atoi(m[3])
		sm, _ := strconv.Atoi(m[4])
		eh, _ := strconv.Atoi(m[5])
		em, _ := strconv.Atoi(m[6])
		start, end := sh*60+sm, eh*60+em
		if _, dup := placed[task]; dup {
			failures = append(failures, "duplicate_task_"+task)
			continue
		}
		w := windows[day]
		if start < w[0] || end > w[1] || end <= start {
			failures = append(failures, "outside_window_"+task)
		}
		if end-start != durations[task] {
			failures = append(failures, "wrong_duration_"+task)
		}
		g := slot{dayBase[day] + start, dayBase[day] + end}
		placed[task] = g
		intervals = append(intervals, g)
	}
	if len(placed) != 5 {
		return append(failures, "missing_tasks")
	}
	sort.Slice(intervals, func(i, j int) bool { return intervals[i].start < intervals[j].start })
	for i := 1; i < len(intervals); i++ {
		if intervals[i].start < intervals[i-1].end {
			failures = append(failures, "overlap")
			break
		}
	}
	for _, dep := range [][2]string{{"Draft", "Review"}, {"Prep", "Review"}, {"Review", "Fix"}, {"Fix", "Final"}} {
		if placed[dep[1]].start < placed[dep[0]].end {
			failures = append(failures, "dependency_"+dep[0]+"_before_"+dep[1])
		}
	}
	if placed["Final"].end > dayBase["Fri"]+11*60 {
		failures = append(failures, "missed_deadline")
	}
	return failures
}

type fastABRun struct {
	Case               string                `json:"case"`
	Arm                string                `json:"arm"`
	Repetition         int                   `json:"repetition"`
	TrialID            string                `json:"trial_id"`
	CachePolicy        string                `json:"response_cache_policy"`
	Cached             bool                  `json:"cached"`
	Correct            bool                  `json:"correct"`
	Failures           []string              `json:"failures,omitempty"`
	LatencyMillis      int64                 `json:"latency_millis"`
	LLMCalls           int                   `json:"llm_calls"`
	InputTokens        int                   `json:"input_tokens"`
	OutputTokens       int                   `json:"output_tokens"`
	TotalTokens        int                   `json:"total_tokens"`
	CostUSD            float64               `json:"cost_usd"`
	UsageObserved      bool                  `json:"usage_observed"`
	CostObserved       bool                  `json:"cost_observed"`
	Answer             string                `json:"answer"`
	TrajectoryObserved bool                  `json:"trajectory_observed"`
	ToolTrajectory     []agent.RunTraceEvent `json:"tool_trajectory"`
	LoopEvents         []agent.RunTraceEvent `json:"loop_events"`
	Status             fastPinnedRunStatus   `json:"status"`
	Efficiency         fastPinnedEfficiency  `json:"efficiency"`
}

type fastPinnedCellSummary struct {
	Case         string  `json:"case"`
	Arm          string  `json:"arm"`
	Runs         int     `json:"runs"`
	CorrectRuns  int     `json:"correct_runs"`
	MedianMillis int64   `json:"median_ms"`
	P90Millis    int64   `json:"p90_ms"`
	MeanLLMCalls float64 `json:"mean_llm_calls"`
	CostUSD      float64 `json:"cost_usd"`
}

type fastPinnedArmSummary struct {
	Arm                           string   `json:"arm"`
	Complete                      bool     `json:"complete"`
	Scheduled                     int      `json:"scheduled"`
	Completed                     int      `json:"completed"`
	Correct                       bool     `json:"correct"`
	CorrectRuns                   int      `json:"correct_runs"`
	UsageObserved                 bool     `json:"usage_observed"`
	CostObserved                  bool     `json:"cost_observed"`
	CacheIsolationObserved        bool     `json:"cache_isolation_observed"`
	TerminalAnswersObserved       bool     `json:"terminal_answers_observed"`
	ObservabilityObserved         bool     `json:"observability_observed"`
	TrajectoryObserved            bool     `json:"trajectory_observed"`
	EfficiencyObserved            bool     `json:"efficiency_observed"`
	EfficiencyQualifying          bool     `json:"efficiency_qualifying"`
	RelativePerformanceQualifying bool     `json:"relative_performance_qualifying"`
	TotalCostUSD                  float64  `json:"total_cost_usd"`
	ComparisonQualifying          bool     `json:"comparison_qualifying"`
	ReleaseQualifying             bool     `json:"release_qualifying"`
	QualificationFailures         []string `json:"qualification_failures"`
}

type fastPinnedQualificationReport struct {
	SchemaVersion                string                  `json:"schema_version"`
	GeneratedAt                  string                  `json:"generated_at"`
	Complete                     bool                    `json:"complete"`
	Sample                       string                  `json:"sample"`
	RepetitionsPerCell           int                     `json:"repetitions_per_cell"`
	MinimumComparisonRepetitions int                     `json:"minimum_comparison_repetitions"`
	MinimumReleaseRepetitions    int                     `json:"minimum_release_repetitions"`
	Seed                         int64                   `json:"seed"`
	Scheduled                    int                     `json:"scheduled"`
	Completed                    int                     `json:"completed"`
	CorrectRuns                  int                     `json:"correct_runs"`
	UsageObserved                bool                    `json:"usage_observed"`
	CostObserved                 bool                    `json:"cost_observed"`
	CacheIsolationObserved       bool                    `json:"cache_isolation_observed"`
	TerminalAnswersObserved      bool                    `json:"terminal_answers_observed"`
	TrajectoryObserved           bool                    `json:"trajectory_observed"`
	EfficiencyObserved           bool                    `json:"efficiency_observed"`
	EfficiencyQualifying         bool                    `json:"efficiency_qualifying"`
	FastPerformanceObserved      bool                    `json:"fast_performance_observed"`
	FastPerformanceQualifying    bool                    `json:"fast_performance_qualifying"`
	FastLatencyRatio             float64                 `json:"fast_latency_ratio"`
	FastCostRatio                float64                 `json:"fast_cost_ratio"`
	MaximumNonInferiorityRatio   float64                 `json:"maximum_noninferiority_ratio"`
	ComparisonQualifying         bool                    `json:"comparison_qualifying"`
	ReleaseQualifying            bool                    `json:"release_qualifying"`
	FastReleaseQualifying        bool                    `json:"fast_release_qualifying"`
	FullReleaseQualifying        bool                    `json:"full_release_qualifying"`
	TotalCostUSD                 float64                 `json:"total_cost_usd"`
	MaxCostUSD                   float64                 `json:"max_cost_usd"`
	Arms                         []fastPinnedArmSummary  `json:"arms"`
	Cells                        []fastPinnedCellSummary `json:"cells"`
	QualificationFailures        []string                `json:"qualification_failures"`
	Runs                         []fastABRun             `json:"runs"`
}

func TestLive_FastPinnedVsFullAB(t *testing.T) {
	if os.Getenv("SHANNON_E2E_LIVE") != "1" {
		t.Skip("set SHANNON_E2E_LIVE=1 to authorize real provider calls")
	}
	if os.Getenv(fastPinnedABGateEnv) != "1" {
		t.Skipf("set %s=1 to run the paid Fast-pinned A/B lane", fastPinnedABGateEnv)
	}
	cfg := loadAgentLabQualityConfig(t)
	provider := client.NewGatewayClient(cfg.endpoint, cfg.apiKey)

	repetitions := 3
	if raw := strings.TrimSpace(os.Getenv(fastPinnedABRepsEnv)); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > 30 {
			t.Fatalf("%s must be in [1,30]", fastPinnedABRepsEnv)
		}
		repetitions = value
	}
	sample := fastPinnedSample(t, fastPinnedABSampleEnv, repetitions)

	// Resolve the Fast profile once, exactly like the daemon admission path.
	resolveCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	cloudProfile, resolveErr := provider.ResolveKoeExecutionProfile(resolveCtx)
	cancel()
	fastProfile := executionprofile.Resolve(executionprofile.ResolutionInput{
		RequestedMode: executionprofile.ModeFast,
		FastEnabled:   true,
		CloudProfile:  &cloudProfile,
		CloudError:    resolveErr,
	})
	if resolveErr != nil || fastProfile.ValidateFast() != nil {
		t.Fatalf("Fast profile resolve failed (resolveErr=%v validate=%v) — cannot pin Fast", resolveErr, fastProfile.ValidateFast())
	}
	t.Logf("fast profile pinned: id=%s model=%s", fastProfile.ProfileID, fastProfile.Model)

	cases := fastPinnedABCases()
	type job struct {
		caseIndex  int
		arm        string
		repetition int
	}
	var jobs []job
	for rep := 1; rep <= repetitions; rep++ {
		for i := range cases {
			for _, arm := range []string{"fast", "full"} {
				jobs = append(jobs, job{caseIndex: i, arm: arm, repetition: rep})
			}
		}
	}
	rand.New(rand.NewSource(fastPinnedABSeed)).Shuffle(len(jobs), func(i, j int) { jobs[i], jobs[j] = jobs[j], jobs[i] })

	var results []fastABRun
	totalCost := 0.0
	for _, jb := range jobs {
		tc := cases[jb.caseIndex]
		run := runFastABJob(t, provider, fastProfile, tc, jb.arm, jb.repetition)
		totalCost += run.CostUSD
		results = append(results, run)
		t.Logf("fast_ab case=%s arm=%s rep=%d correct=%v failures=%v efficient=%v efficiency_violations=%v latency_ms=%d llm_calls=%d cost=%.6f",
			run.Case, run.Arm, run.Repetition, run.Correct, run.Failures,
			run.Efficiency.Qualifying, run.Efficiency.Violations,
			run.LatencyMillis, run.LLMCalls, run.CostUSD)
		if totalCost > fastPinnedABMaxCostUSD {
			t.Logf("cost ceiling %.2f USD exceeded at %.4f — stopping with an incomplete report", fastPinnedABMaxCostUSD, totalCost)
			break
		}
	}

	summarizeFastAB(t, results, repetitions, totalCost, sample)
}

func runFastABJob(
	t *testing.T,
	provider *client.GatewayClient,
	fastProfile executionprofile.Profile,
	tc fastABCase,
	arm string,
	repetition int,
) fastABRun {
	t.Helper()
	dir := t.TempDir()
	trialID := fastPinnedTrialID(fastPinnedABSeed, tc.name, repetition)
	prompt := fastPinnedTrialPrompt(tc.setup(t, dir), trialID)

	registry := agent.NewToolRegistry()
	registry.Register(&tools.FileReadTool{})
	registry.Register(&tools.FileWriteTool{})
	registry.Register(&tools.DirectoryListTool{})
	registry.Register(&tools.GlobTool{})
	registry.Register(&tools.CalculateTool{})
	var probe *chainProbeTool
	if tc.probe {
		probe = &chainProbeTool{}
		registry.Register(probe)
	}

	cacheOffProvider := &fastPinnedCacheOffClient{inner: provider}
	loop := agent.NewAgentLoop(cacheOffProvider, registry, "medium", dir, 12, 30_000, 200, nil, nil, nil)
	loop.SetCacheSource("fast_pinned_ab")
	loop.SetSkillDiscovery(false)
	loop.SetMaxTokens(1200)
	loop.SetTemperature(0)
	handler := &fastABHandler{}
	loop.SetHandler(handler)

	if arm == "fast" {
		loop.SetKoeExecutionProfile(fastProfile)
	} else {
		loop.SetSpecificModel(fastPinnedABFullModel)
		loop.SetThinking(&client.ThinkingConfig{Type: "adaptive"})
		loop.SetKoeExecutionProfile(executionprofile.FullProfile(executionprofile.ModeFull, "fast_ab_control"))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	started := time.Now()
	answer, _, err := loop.Run(ctx, prompt, nil, nil)
	status := loop.LastRunStatus()
	run := fastABRun{
		Case: tc.name, Arm: arm, Repetition: repetition,
		TrialID:            trialID,
		LatencyMillis:      time.Since(started).Milliseconds(),
		Answer:             strings.TrimSpace(answer),
		TrajectoryObserved: true,
		ToolTrajectory:     handler.trace(),
		LoopEvents:         handler.loopEvents(),
		Status: fastPinnedRunStatus{
			Partial: status.Partial, FailureCode: string(status.FailureCode),
			LastTool: status.LastTool, RetryCount: status.RetryCount,
			IterationCount: status.IterationCount,
		},
	}
	cacheIsolated, cached := cacheOffProvider.observations()
	run.Cached = cached
	if cacheIsolated {
		run.CachePolicy = string(executionprofile.ResponseCacheOff)
	} else {
		run.Failures = append(run.Failures, "response_cache_not_isolated")
	}
	if err != nil {
		run.Failures = append(run.Failures, "loop_error:"+err.Error())
	}
	usage := handler.accumulatedUsage()
	run.LLMCalls = usage.LLM.LLMCalls
	run.InputTokens = usage.LLM.InputTokens
	run.OutputTokens = usage.LLM.OutputTokens
	run.TotalTokens = usage.LLM.TotalTokens
	run.CostUSD = usage.TotalCostUSD()
	run.UsageObserved = usage.LLM.LLMCalls > 0 && usage.LLM.TotalTokens > 0
	run.CostObserved = usage.LLM.LLMCalls > 0 && run.CostUSD > 0
	if !run.UsageObserved {
		run.Failures = append(run.Failures, "usage_not_observed")
	}
	if !run.CostObserved {
		run.Failures = append(run.Failures, "cost_not_observed")
	}
	if run.Answer == "" {
		run.Failures = append(run.Failures, "terminal_answer_missing")
	}
	run.Efficiency = evaluateFastPinnedEfficiency(run.Case, run.LLMCalls, run.Status, run.ToolTrajectory)
	run.Failures = append(run.Failures, tc.validate(run.Answer, dir, handler, probe)...)
	run.Correct = len(run.Failures) == 0
	return run
}

func summarizeFastAB(t *testing.T, results []fastABRun, repetitions int, totalCost float64, sample string) {
	t.Helper()
	caseNames := make([]string, 0, len(fastPinnedABCases()))
	for _, tc := range fastPinnedABCases() {
		caseNames = append(caseNames, tc.name)
	}
	report := newFastPinnedQualificationReport(
		"kocoro.fast_pinned_ab.v4", fastPinnedABSeed, sample, repetitions,
		caseNames, results, totalCost, fastPinnedABMaxCostUSD,
	)
	for _, cell := range report.Cells {
		t.Logf("cell %-32s correct=%d/%d median_ms=%d p90_ms=%d mean_llm_calls=%.1f total_cost=%.6f",
			cell.Case+"/"+cell.Arm, cell.CorrectRuns, cell.Runs,
			cell.MedianMillis, cell.P90Millis, cell.MeanLLMCalls, cell.CostUSD)
	}
	t.Logf("fast_pinned_ab complete=%t runs=%d repetitions=%d comparison_qualifying=%t release_qualifying=%t total_cost=%.6f",
		report.Complete, len(results), repetitions, report.ComparisonQualifying, report.ReleaseQualifying, totalCost)

	outputPath := strings.TrimSpace(os.Getenv(fastPinnedABOutputEnv))
	if outputPath == "" {
		outputPath = filepath.Join(os.TempDir(), fmt.Sprintf("kocoro-fast-pinned-ab-%d-%s.json",
			fastPinnedABSeed, time.Now().UTC().Format("20060102T150405.000000000Z")))
	}
	if err := writeFastPinnedQualificationReport(outputPath, report); err != nil {
		t.Fatalf("write report: %v", err)
	}
	t.Logf("report=%s", outputPath)
	assertFastPinnedQualification(t, report, outputPath)
}

func fastPinnedSample(t *testing.T, envName string, repetitions int) string {
	t.Helper()
	sample := strings.TrimSpace(os.Getenv(envName))
	if sample == "" {
		sample = "smoke"
	}
	if sample != "smoke" && sample != "release" {
		t.Fatalf("%s must be smoke or release", envName)
	}
	minimum := agentLabQualityComparisonRepetitions
	if sample == "release" {
		minimum = agentLabQualityReleaseRepetitions
	}
	if repetitions < minimum {
		t.Fatalf("%s=%s requires repetitions >= %d", envName, sample, minimum)
	}
	return sample
}

func newFastPinnedQualificationReport(
	schemaVersion string,
	seed int64,
	sample string,
	repetitions int,
	caseNames []string,
	results []fastABRun,
	totalCost float64,
	maxCost float64,
) fastPinnedQualificationReport {
	requiresTrajectory := strings.HasSuffix(schemaVersion, ".v4")
	const maximumNonInferiorityRatio = 1.20
	type armState struct {
		summary            fastPinnedArmSummary
		structuralFailures []string
	}
	report := fastPinnedQualificationReport{
		SchemaVersion:                schemaVersion,
		GeneratedAt:                  time.Now().UTC().Format(time.RFC3339Nano),
		Sample:                       sample,
		RepetitionsPerCell:           repetitions,
		MinimumComparisonRepetitions: agentLabQualityComparisonRepetitions,
		MinimumReleaseRepetitions:    agentLabQualityReleaseRepetitions,
		Seed:                         seed,
		Scheduled:                    len(caseNames) * 2 * repetitions,
		Completed:                    len(results),
		UsageObserved:                len(results) > 0,
		CostObserved:                 len(results) > 0,
		CacheIsolationObserved:       len(results) > 0,
		TerminalAnswersObserved:      len(results) > 0,
		TrajectoryObserved:           !requiresTrajectory || len(results) > 0,
		EfficiencyObserved:           !requiresTrajectory || len(results) > 0,
		EfficiencyQualifying:         !requiresTrajectory || len(results) > 0,
		MaximumNonInferiorityRatio:   maximumNonInferiorityRatio,
		TotalCostUSD:                 totalCost,
		MaxCostUSD:                   maxCost,
		Runs:                         append([]fastABRun(nil), results...),
	}
	armStates := map[string]*armState{}
	for _, arm := range []string{"fast", "full"} {
		armStates[arm] = &armState{summary: fastPinnedArmSummary{
			Arm: arm, Scheduled: len(caseNames) * repetitions,
			UsageObserved: true, CostObserved: true,
			CacheIsolationObserved: true, TerminalAnswersObserved: true,
			TrajectoryObserved: true, EfficiencyObserved: true,
			EfficiencyQualifying: true,
		}}
	}
	type cellState struct {
		summary     fastPinnedCellSummary
		repetitions map[int]bool
		latencies   []int64
		llmCalls    int
	}
	expected := make(map[string]*cellState, len(caseNames)*2)
	for _, caseName := range caseNames {
		for _, arm := range []string{"fast", "full"} {
			key := caseName + "/" + arm
			expected[key] = &cellState{
				summary:     fastPinnedCellSummary{Case: caseName, Arm: arm},
				repetitions: make(map[int]bool, repetitions),
			}
		}
	}
	for _, run := range results {
		computedEfficiency := evaluateFastPinnedEfficiency(run.Case, run.LLMCalls, run.Status, run.ToolTrajectory)
		trajectoryObserved := fastPinnedProcessObserved(run)
		efficiencyObserved := run.Efficiency.PolicyID == fastPinnedEfficiencyPolicyID
		efficiencyQualifying := efficiencyObserved && run.Efficiency.Qualifying && computedEfficiency.Qualifying
		arm := armStates[run.Arm]
		if arm != nil {
			arm.summary.Completed++
			arm.summary.TotalCostUSD += run.CostUSD
			if run.Correct {
				arm.summary.CorrectRuns++
			}
			if !run.UsageObserved {
				arm.summary.UsageObserved = false
			}
			if !run.CostObserved {
				arm.summary.CostObserved = false
			}
			if run.CachePolicy != string(executionprofile.ResponseCacheOff) || run.Cached {
				arm.summary.CacheIsolationObserved = false
			}
			if strings.TrimSpace(run.Answer) == "" {
				arm.summary.TerminalAnswersObserved = false
			}
			if requiresTrajectory && !trajectoryObserved {
				arm.summary.TrajectoryObserved = false
			}
			if requiresTrajectory && !efficiencyObserved {
				arm.summary.EfficiencyObserved = false
			}
			if requiresTrajectory && !efficiencyQualifying {
				arm.summary.EfficiencyQualifying = false
			}
		}
		if run.Correct {
			report.CorrectRuns++
		}
		if !run.UsageObserved {
			report.UsageObserved = false
		}
		if !run.CostObserved {
			report.CostObserved = false
		}
		if run.CachePolicy != string(executionprofile.ResponseCacheOff) || run.Cached {
			report.CacheIsolationObserved = false
		}
		wantTrialID := fastPinnedTrialID(seed, run.Case, run.Repetition)
		if run.TrialID != wantTrialID {
			failure := fmt.Sprintf("trial_id_mismatch:%s/%s:%d", run.Case, run.Arm, run.Repetition)
			report.QualificationFailures = append(report.QualificationFailures, failure)
			if arm != nil {
				arm.structuralFailures = append(arm.structuralFailures, failure)
			}
		}
		if strings.TrimSpace(run.Answer) == "" {
			report.TerminalAnswersObserved = false
		}
		if requiresTrajectory && !trajectoryObserved {
			report.TrajectoryObserved = false
		}
		if requiresTrajectory && !efficiencyObserved {
			report.EfficiencyObserved = false
		}
		if requiresTrajectory && !efficiencyQualifying {
			report.EfficiencyQualifying = false
		}
		key := run.Case + "/" + run.Arm
		cell := expected[key]
		if cell == nil {
			failure := "unexpected_cell:" + key
			report.QualificationFailures = append(report.QualificationFailures, failure)
			if arm != nil {
				arm.structuralFailures = append(arm.structuralFailures, failure)
			}
			continue
		}
		cell.summary.Runs++
		cell.summary.CostUSD += run.CostUSD
		cell.latencies = append(cell.latencies, run.LatencyMillis)
		cell.llmCalls += run.LLMCalls
		if run.Correct {
			cell.summary.CorrectRuns++
		}
		if run.Repetition < 1 || run.Repetition > repetitions {
			failure := fmt.Sprintf("invalid_repetition:%s:%d", key, run.Repetition)
			report.QualificationFailures = append(report.QualificationFailures, failure)
			if arm != nil {
				arm.structuralFailures = append(arm.structuralFailures, failure)
			}
		} else if cell.repetitions[run.Repetition] {
			failure := fmt.Sprintf("duplicate_repetition:%s:%d", key, run.Repetition)
			report.QualificationFailures = append(report.QualificationFailures, failure)
			if arm != nil {
				arm.structuralFailures = append(arm.structuralFailures, failure)
			}
		} else {
			cell.repetitions[run.Repetition] = true
		}
	}
	cellKeys := make([]string, 0, len(expected))
	for key := range expected {
		cellKeys = append(cellKeys, key)
	}
	sort.Strings(cellKeys)
	for _, key := range cellKeys {
		cell := expected[key]
		cell.summary.MedianMillis = fastPinnedMedian(cell.latencies)
		cell.summary.P90Millis = fastPinnedPercentile(cell.latencies, 0.90)
		if cell.summary.Runs > 0 {
			cell.summary.MeanLLMCalls = float64(cell.llmCalls) / float64(cell.summary.Runs)
		}
		report.Cells = append(report.Cells, cell.summary)
		if cell.summary.Runs != repetitions || len(cell.repetitions) != repetitions {
			failure := fmt.Sprintf("incomplete_cell:%s:%d/%d", key, len(cell.repetitions), repetitions)
			report.QualificationFailures = append(report.QualificationFailures, failure)
			armStates[cell.summary.Arm].structuralFailures = append(
				armStates[cell.summary.Arm].structuralFailures, failure)
		}
	}
	if requiresTrajectory {
		var fastMedianSum, fullMedianSum int64
		var fastCost, fullCost float64
		performanceObserved := len(caseNames) > 0
		for _, caseName := range caseNames {
			fastCell := expected[caseName+"/fast"]
			fullCell := expected[caseName+"/full"]
			if fastCell == nil || fullCell == nil ||
				fastCell.summary.Runs != repetitions || fullCell.summary.Runs != repetitions ||
				fastCell.summary.MedianMillis <= 0 || fullCell.summary.MedianMillis <= 0 ||
				fastCell.summary.CostUSD <= 0 || fullCell.summary.CostUSD <= 0 {
				performanceObserved = false
				continue
			}
			fastMedianSum += fastCell.summary.MedianMillis
			fullMedianSum += fullCell.summary.MedianMillis
			fastCost += fastCell.summary.CostUSD
			fullCost += fullCell.summary.CostUSD
		}
		report.FastPerformanceObserved = performanceObserved && fullMedianSum > 0 && fullCost > 0
		if report.FastPerformanceObserved {
			report.FastLatencyRatio = float64(fastMedianSum) / float64(fullMedianSum)
			report.FastCostRatio = fastCost / fullCost
			report.FastPerformanceQualifying = report.FastLatencyRatio <= maximumNonInferiorityRatio &&
				report.FastCostRatio <= maximumNonInferiorityRatio
		}
	}
	if report.Completed != report.Scheduled {
		report.QualificationFailures = append(report.QualificationFailures,
			fmt.Sprintf("incomplete_schedule:%d/%d", report.Completed, report.Scheduled))
	}
	report.Complete = len(report.QualificationFailures) == 0
	if requiresTrajectory && !report.FastPerformanceObserved {
		report.QualificationFailures = append(report.QualificationFailures, "fast_relative_performance_not_observed")
	} else if requiresTrajectory && !report.FastPerformanceQualifying {
		report.QualificationFailures = append(report.QualificationFailures,
			fmt.Sprintf("fast_relative_performance_regressed:latency=%.4f:cost=%.4f:max=%.2f",
				report.FastLatencyRatio, report.FastCostRatio, maximumNonInferiorityRatio))
	}
	if report.CorrectRuns != report.Completed {
		report.QualificationFailures = append(report.QualificationFailures,
			fmt.Sprintf("incorrect_runs:%d/%d", report.CorrectRuns, report.Completed))
	}
	if !report.UsageObserved {
		report.QualificationFailures = append(report.QualificationFailures, "usage_not_observed")
	}
	if !report.CostObserved {
		report.QualificationFailures = append(report.QualificationFailures, "cost_not_observed")
	}
	if !report.CacheIsolationObserved {
		report.QualificationFailures = append(report.QualificationFailures, "response_cache_not_isolated")
	}
	if !report.TerminalAnswersObserved {
		report.QualificationFailures = append(report.QualificationFailures, "terminal_answer_missing")
	}
	if requiresTrajectory && !report.TrajectoryObserved {
		report.QualificationFailures = append(report.QualificationFailures, "trajectory_not_observed")
	}
	if requiresTrajectory && !report.EfficiencyObserved {
		report.QualificationFailures = append(report.QualificationFailures, "efficiency_not_observed")
	}
	if requiresTrajectory && !report.EfficiencyQualifying {
		report.QualificationFailures = append(report.QualificationFailures, "efficiency_not_qualifying")
	}
	if report.TotalCostUSD > report.MaxCostUSD {
		report.QualificationFailures = append(report.QualificationFailures, "cost_ceiling_exceeded")
	}
	for _, armName := range []string{"fast", "full"} {
		state := armStates[armName]
		arm := &state.summary
		if arm.Completed == 0 {
			arm.UsageObserved = false
			arm.CostObserved = false
			arm.CacheIsolationObserved = false
			arm.TerminalAnswersObserved = false
			if requiresTrajectory {
				arm.TrajectoryObserved = false
				arm.EfficiencyObserved = false
				arm.EfficiencyQualifying = false
			}
		}
		if arm.Completed != arm.Scheduled {
			state.structuralFailures = append(state.structuralFailures,
				fmt.Sprintf("incomplete_schedule:%s:%d/%d", armName, arm.Completed, arm.Scheduled))
		}
		arm.Complete = len(state.structuralFailures) == 0
		arm.QualificationFailures = append(arm.QualificationFailures, state.structuralFailures...)
		arm.Correct = arm.Complete && arm.Completed > 0 && arm.CorrectRuns == arm.Completed
		if arm.CorrectRuns != arm.Completed {
			arm.QualificationFailures = append(arm.QualificationFailures,
				fmt.Sprintf("incorrect_runs:%s:%d/%d", armName, arm.CorrectRuns, arm.Completed))
		}
		if !arm.UsageObserved {
			arm.QualificationFailures = append(arm.QualificationFailures, "usage_not_observed:"+armName)
		}
		if !arm.CostObserved {
			arm.QualificationFailures = append(arm.QualificationFailures, "cost_not_observed:"+armName)
		}
		if !arm.CacheIsolationObserved {
			arm.QualificationFailures = append(arm.QualificationFailures, "response_cache_not_isolated:"+armName)
		}
		if !arm.TerminalAnswersObserved {
			arm.QualificationFailures = append(arm.QualificationFailures, "terminal_answer_missing:"+armName)
		}
		if requiresTrajectory && !arm.TrajectoryObserved {
			arm.QualificationFailures = append(arm.QualificationFailures, "trajectory_not_observed:"+armName)
		}
		if requiresTrajectory && !arm.EfficiencyObserved {
			arm.QualificationFailures = append(arm.QualificationFailures, "efficiency_not_observed:"+armName)
		}
		if requiresTrajectory && !arm.EfficiencyQualifying {
			arm.QualificationFailures = append(arm.QualificationFailures, "efficiency_not_qualifying:"+armName)
		}
		arm.RelativePerformanceQualifying = !requiresTrajectory || armName != "fast" || report.FastPerformanceQualifying
		if !arm.RelativePerformanceQualifying {
			arm.QualificationFailures = append(arm.QualificationFailures, "fast_relative_performance_not_qualifying")
		}
		arm.ObservabilityObserved = arm.UsageObserved && arm.CostObserved &&
			arm.CacheIsolationObserved && arm.TerminalAnswersObserved &&
			(!requiresTrajectory || (arm.TrajectoryObserved && arm.EfficiencyObserved))
		allArmValid := arm.Complete && arm.Correct && arm.ObservabilityObserved &&
			report.TotalCostUSD <= report.MaxCostUSD &&
			(!requiresTrajectory || (arm.EfficiencyQualifying && arm.RelativePerformanceQualifying))
		arm.ComparisonQualifying = allArmValid && repetitions >= report.MinimumComparisonRepetitions
		arm.ReleaseQualifying = sample == "release" && allArmValid &&
			repetitions >= report.MinimumReleaseRepetitions
		report.Arms = append(report.Arms, *arm)
		switch armName {
		case "fast":
			report.FastReleaseQualifying = arm.ReleaseQualifying
		case "full":
			report.FullReleaseQualifying = arm.ReleaseQualifying
		}
	}
	allValid := report.Complete && report.Completed > 0 &&
		report.CorrectRuns == report.Completed && report.UsageObserved && report.CostObserved &&
		report.CacheIsolationObserved && report.TerminalAnswersObserved && report.TotalCostUSD <= report.MaxCostUSD &&
		(!requiresTrajectory || (report.TrajectoryObserved && report.EfficiencyObserved && report.EfficiencyQualifying &&
			report.FastPerformanceObserved && report.FastPerformanceQualifying))
	report.ComparisonQualifying = allValid && repetitions >= report.MinimumComparisonRepetitions
	report.ReleaseQualifying = sample == "release" && allValid && repetitions >= report.MinimumReleaseRepetitions
	return report
}

func fastPinnedMedian(values []int64) int64 {
	if len(values) == 0 {
		return 0
	}
	ordered := append([]int64(nil), values...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i] < ordered[j] })
	return ordered[len(ordered)/2]
}

func fastPinnedPercentile(values []int64, percentile float64) int64 {
	if len(values) == 0 {
		return 0
	}
	ordered := append([]int64(nil), values...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i] < ordered[j] })
	index := int(math.Ceil(float64(len(ordered))*percentile)) - 1
	if index < 0 {
		index = 0
	}
	if index >= len(ordered) {
		index = len(ordered) - 1
	}
	return ordered[index]
}

func writeFastPinnedQualificationReport(path string, report fastPinnedQualificationReport) error {
	body, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return agentsapi.AtomicWrite(path, append(body, '\n'))
}

func assertFastPinnedQualification(t *testing.T, report fastPinnedQualificationReport, outputPath string) {
	t.Helper()
	if report.Sample == "release" {
		if !report.Complete || !report.FastReleaseQualifying {
			t.Fatalf("Fast-pinned release gate did not qualify the Fast product arm: failures=%v report=%s",
				report.QualificationFailures, outputPath)
		}
		if !report.FullReleaseQualifying {
			t.Logf("Full control arm did not release-qualify; global comparison remains non-qualifying: report=%s", outputPath)
		}
		return
	}
	if !report.Complete || !report.ComparisonQualifying {
		t.Fatalf("Fast-pinned gate failed closed: complete=%t comparison_qualifying=%t failures=%v report=%s",
			report.Complete, report.ComparisonQualifying, report.QualificationFailures, outputPath)
	}
}
