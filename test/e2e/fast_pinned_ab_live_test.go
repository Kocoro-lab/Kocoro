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
//	SHANNON_E2E_LIVE=1 KOCORO_FAST_PINNED_AB=1 go test ./test/e2e/ -run TestLive_FastPinnedVsFullAB -v

import (
	"context"
	"fmt"
	"encoding/json"
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
	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/executionprofile"
	"github.com/Kocoro-lab/ShanClaw/internal/tools"
)

const (
	fastPinnedABGateEnv    = "KOCORO_FAST_PINNED_AB"
	fastPinnedABRepsEnv    = "KOCORO_FAST_PINNED_AB_REPETITIONS"
	fastPinnedABOutputEnv  = "KOCORO_FAST_PINNED_AB_OUTPUT"
	// Runtime model id for the Full control arm — the same control the
	// 420-run qualification lane uses (koeQualificationSonnetModel). This is
	// request configuration, not a tooling reference.
	fastPinnedABFullModel  = "claude-sonnet-5"
	fastPinnedABMaxCostUSD = 5.0
	fastPinnedABSeed       = int64(20260810)
)

// fastABHandler auto-approves tool calls (file tools all require approval)
// and counts per-tool invocations for the validators.
type fastABHandler struct {
	mu        sync.Mutex
	toolCalls map[string]int
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
func (h *fastABHandler) OnText(string)                                                       {}
func (h *fastABHandler) OnPreamble(string)                                                   {}
func (h *fastABHandler) OnStreamDelta(string)                                                {}
func (h *fastABHandler) OnApprovalNeeded(string, string) bool                                { return true }
func (h *fastABHandler) OnUsage(agent.TurnUsage)                                             {}
func (h *fastABHandler) OnCloudAgent(string, string, string)                                 {}
func (h *fastABHandler) OnCloudProgress(int, int)                                            {}
func (h *fastABHandler) OnCloudPlan(string, string, bool)                                    {}

func (h *fastABHandler) count(name string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.toolCalls[name]
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
	Case          string   `json:"case"`
	Arm           string   `json:"arm"`
	Repetition    int      `json:"repetition"`
	Correct       bool     `json:"correct"`
	Failures      []string `json:"failures,omitempty"`
	LatencyMillis int64    `json:"latency_millis"`
	LLMCalls      int      `json:"llm_calls"`
	InputTokens   int      `json:"input_tokens"`
	OutputTokens  int      `json:"output_tokens"`
	CostUSD       float64  `json:"cost_usd"`
	Answer        string   `json:"answer"`
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
		if totalCost > fastPinnedABMaxCostUSD {
			t.Fatalf("cost ceiling %.2f USD exceeded at %.4f — aborting", fastPinnedABMaxCostUSD, totalCost)
		}
		tc := cases[jb.caseIndex]
		run := runFastABJob(t, provider, fastProfile, tc, jb.arm, jb.repetition)
		totalCost += run.CostUSD
		results = append(results, run)
		t.Logf("fast_ab case=%s arm=%s rep=%d correct=%v failures=%v latency_ms=%d llm_calls=%d cost=%.6f",
			run.Case, run.Arm, run.Repetition, run.Correct, run.Failures, run.LatencyMillis, run.LLMCalls, run.CostUSD)
	}

	summarizeFastAB(t, results, repetitions, totalCost)
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
	prompt := tc.setup(t, dir)

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

	loop := agent.NewAgentLoop(provider, registry, "medium", dir, 12, 30_000, 200, nil, nil, nil)
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
	answer, usage, err := loop.Run(ctx, prompt, nil, nil)
	run := fastABRun{
		Case: tc.name, Arm: arm, Repetition: repetition,
		LatencyMillis: time.Since(started).Milliseconds(),
		Answer:        strings.TrimSpace(answer),
	}
	if err != nil {
		run.Failures = append(run.Failures, "loop_error:"+err.Error())
	}
	if usage != nil {
		run.LLMCalls = usage.LLMCalls
		run.InputTokens = usage.InputTokens
		run.OutputTokens = usage.OutputTokens
		run.CostUSD = usage.CostUSD
	}
	run.Failures = append(run.Failures, tc.validate(run.Answer, dir, handler, probe)...)
	run.Correct = len(run.Failures) == 0
	return run
}

func summarizeFastAB(t *testing.T, results []fastABRun, repetitions int, totalCost float64) {
	t.Helper()
	type agg struct {
		correct, total int
		latencies      []int64
		cost           float64
		llmCalls       int
	}
	byArm := map[string]*agg{}
	byCell := map[string]*agg{}
	for _, r := range results {
		for _, key := range []string{r.Arm, r.Case + "/" + r.Arm} {
			m := byArm
			if strings.Contains(key, "/") {
				m = byCell
			}
			a := m[key]
			if a == nil {
				a = &agg{}
				m[key] = a
			}
			a.total++
			if r.Correct {
				a.correct++
			}
			a.latencies = append(a.latencies, r.LatencyMillis)
			a.cost += r.CostUSD
			a.llmCalls += r.LLMCalls
		}
	}
	median := func(v []int64) int64 {
		if len(v) == 0 {
			return 0
		}
		sorted := append([]int64(nil), v...)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
		return sorted[len(sorted)/2]
	}
	var cellKeys []string
	for k := range byCell {
		cellKeys = append(cellKeys, k)
	}
	sort.Strings(cellKeys)
	for _, k := range cellKeys {
		a := byCell[k]
		t.Logf("cell %-32s correct=%d/%d median_ms=%d mean_cost=%.6f", k, a.correct, a.total, median(a.latencies), a.cost/float64(a.total))
	}
	for _, armName := range []string{"fast", "full"} {
		a := byArm[armName]
		if a == nil {
			continue
		}
		t.Logf("ARM %-4s correct=%d/%d median_ms=%d total_cost=%.6f mean_llm_calls=%.1f",
			armName, a.correct, a.total, median(a.latencies), a.cost, float64(a.llmCalls)/float64(a.total))
	}
	t.Logf("fast_pinned_ab complete runs=%d repetitions=%d total_cost=%.6f", len(results), repetitions, totalCost)

	outputPath := strings.TrimSpace(os.Getenv(fastPinnedABOutputEnv))
	if outputPath == "" {
		outputPath = filepath.Join(os.TempDir(), fmt.Sprintf("kocoro-fast-pinned-ab-%d.json", fastPinnedABSeed))
	}
	body, err := json.MarshalIndent(map[string]any{
		"schema_version": "kocoro.fast_pinned_ab.v1",
		"seed":           fastPinnedABSeed,
		"repetitions":    repetitions,
		"total_cost_usd": totalCost,
		"runs":           results,
	}, "", "  ")
	if err != nil {
		t.Fatalf("marshal report: %v", err)
	}
	if err := os.WriteFile(outputPath, append(body, '\n'), 0o644); err != nil {
		t.Fatalf("write report: %v", err)
	}
	t.Logf("report=%s", outputPath)
}
