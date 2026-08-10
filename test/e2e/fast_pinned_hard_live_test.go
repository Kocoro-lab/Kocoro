package e2e

// Hard-tier extension of the Fast-pinned vs Full A/B: probes where Fast's
// ceiling actually is. The moderate tier (fast_pinned_ab_live_test.go) showed
// parity at 4-6 step tasks; this tier stresses long faithful execution —
// a 12-step token-dependent chain, a multi-rule config rewrite, a 3-hop
// cross-document trace with distractors, and a 12-operation stateful ledger.
// The answer decides how the failure-escalation path (Fast → Full retry)
// must be tuned once the front-end pins the mode.
//
// Run (after the moderate lane finishes — never concurrently, latency
// numbers would contaminate each other):
//
//	SHANNON_E2E_LIVE=1 KOCORO_FAST_PINNED_HARD=1 go test ./test/e2e/ -run TestLive_FastPinnedHardTier -timeout 120m -v
//
// Release qualification:
//
//	SHANNON_E2E_LIVE=1 KOCORO_FAST_PINNED_HARD=1 KOCORO_FAST_PINNED_HARD_SAMPLE=release KOCORO_FAST_PINNED_HARD_REPETITIONS=15 go test ./test/e2e/ -run TestLive_FastPinnedHardTier -timeout 120m -v

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
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
	fastPinnedHardGateEnv    = "KOCORO_FAST_PINNED_HARD"
	fastPinnedHardRepsEnv    = "KOCORO_FAST_PINNED_HARD_REPETITIONS"
	fastPinnedHardOutputEnv  = "KOCORO_FAST_PINNED_HARD_OUTPUT"
	fastPinnedHardSampleEnv  = "KOCORO_FAST_PINNED_HARD_SAMPLE"
	fastPinnedHardMaxCostUSD = 8.0
	fastPinnedHardSeed       = int64(20260811)
	hardChainSteps           = 12
)

// hardChainTool is the 12-step variant of the chained probe: every step
// returns the token the next step must present.
type hardChainTool struct {
	mu        sync.Mutex
	calls     int
	badTokens int
}

func hardChainToken(step int) string { return fmt.Sprintf("QZ%02d-3d", step) }

func (t *hardChainTool) Info() agent.ToolInfo {
	return agent.ToolInfo{
		Name:        "chain_probe",
		Description: fmt.Sprintf("Advance the %d-step chained task. Call with step=1 and token=INIT first; every response names the token the next step requires.", hardChainSteps),
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"step":  map[string]any{"type": "integer", "description": fmt.Sprintf("Step number 1-%d.", hardChainSteps)},
				"token": map[string]any{"type": "string", "description": "Token returned by the previous step (INIT for step 1)."},
			},
		},
		Required: []string{"step", "token"},
	}
}

func (t *hardChainTool) RequiresApproval() bool     { return false }
func (t *hardChainTool) IsReadOnlyCall(string) bool { return false }

func (t *hardChainTool) Run(_ context.Context, argsJSON string) (agent.ToolResult, error) {
	var args struct {
		Step  int    `json:"step"`
		Token string `json:"token"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil || args.Step < 1 || args.Step > hardChainSteps {
		return agent.ValidationError(fmt.Sprintf("step must be 1-%d and token is required", hardChainSteps)), nil
	}
	expected := "INIT"
	if args.Step > 1 {
		expected = hardChainToken(args.Step - 1)
	}
	t.mu.Lock()
	if strings.TrimSpace(args.Token) != expected {
		t.badTokens++
		t.mu.Unlock()
		return agent.ValidationError(fmt.Sprintf("wrong token for step %d", args.Step)), nil
	}
	t.calls++
	t.mu.Unlock()
	if args.Step < hardChainSteps {
		return agent.ToolResult{Content: fmt.Sprintf("STEP-%d OK. Next: call step=%d with token=%s.", args.Step, args.Step+1, hardChainToken(args.Step))}, nil
	}
	return agent.ToolResult{Content: "ALL STEPS DONE. Final code: LONGCHAIN-457."}, nil
}

// ledgerTool executes a scripted inventory-operation sequence. Operation acks
// carry no state, so the model must either track state itself or use the
// final query — faithful long-sequence execution is the thing under test.
type ledgerTool struct {
	mu    sync.Mutex
	state map[string]int
	ops   int
}

func (t *ledgerTool) Info() agent.ToolInfo {
	return agent.ToolInfo{
		Name:        "ledger_op",
		Description: "Apply one inventory operation. op is add, remove, set, or query. add/remove adjust qty for item; set replaces it; query returns the full current state as JSON.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"op":   map[string]any{"type": "string", "description": "add | remove | set | query"},
				"item": map[string]any{"type": "string", "description": "Item name (omit for query)."},
				"qty":  map[string]any{"type": "integer", "description": "Quantity (omit for query)."},
			},
		},
		Required: []string{"op"},
	}
}

func (t *ledgerTool) RequiresApproval() bool     { return false }
func (t *ledgerTool) IsReadOnlyCall(string) bool { return false }

func (t *ledgerTool) Run(_ context.Context, argsJSON string) (agent.ToolResult, error) {
	var args struct {
		Op   string `json:"op"`
		Item string `json:"item"`
		Qty  int    `json:"qty"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil || args.Op == "" {
		return agent.ValidationError("op is required"), nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.state == nil {
		t.state = map[string]int{}
	}
	switch args.Op {
	case "query":
		body, _ := json.Marshal(t.state)
		return agent.ToolResult{Content: string(body)}, nil
	case "add":
		t.state[args.Item] += args.Qty
	case "remove":
		t.state[args.Item] -= args.Qty
		if t.state[args.Item] <= 0 {
			delete(t.state, args.Item)
		}
	case "set":
		t.state[args.Item] = args.Qty
	default:
		return agent.ValidationError("op must be add, remove, set, or query"), nil
	}
	t.ops++
	return agent.ToolResult{Content: fmt.Sprintf("op %d applied", t.ops)}, nil
}

func fastPinnedHardCases() []fastABCase {
	return []fastABCase{
		{
			name:  "long_chain_12",
			probe: true, // repurposed flag: runner registers the hard chain tool
			setup: func(_ *testing.T, _ string) string {
				return fmt.Sprintf("使用 chain_probe 工具完成整个 %d 步链式任务：从 step=1、token=INIT 开始，每一步的返回会告诉你下一步需要的 token。全部完成后回复 final code。", hardChainSteps)
			},
			validate: nil, // hard runner validates via the hard chain tool
		},
		{
			name: "config_rewrite",
			setup: func(t *testing.T, dir string) string {
				writeFastABFile(t, dir, "services.conf", hardConfigInput())
				return fmt.Sprintf(
					"读取 %s/services.conf（12 个服务块，块之间空行分隔）。按以下全部规则改写并把完整结果写入 %s/services_out.conf，然后回复 REWRITE DONE：\n"+
						"1) 所有 timeout_secs 的值翻倍；\n"+
						"2) 所有 env: dev 改为 env: prod；\n"+
						"3) 整块删除含 deprecated: true 的服务块；\n"+
						"4) replicas 小于 3 的改为 3；\n"+
						"5) log_level: debug 改为 log_level: info；\n"+
						"6) 其余内容与块顺序保持不变。", dir, dir)
			},
			validate: func(_ string, dir string, _ *fastABHandler, _ *chainProbeTool) []string {
				return validateHardConfigRewrite(dir)
			},
		},
		{
			name: "cross_doc_3hop",
			setup: func(t *testing.T, dir string) string {
				writeFastABFile(t, dir, "report_main.txt", "Q3 audit report. The regional budget details were moved to ledger_west.txt.")
				writeFastABFile(t, dir, "ledger_west.txt", "West region ledger. Figures pending sign-off; the certifying auditor is named in signoff_sheet.txt.")
				writeFastABFile(t, dir, "signoff_sheet.txt", "Sign-off sheet. Certifying auditor: Priya Anand. Her final certified amount is recorded in cert_final.txt.")
				writeFastABFile(t, dir, "cert_final.txt", "Certified final amount for the West region: 318750 USD.")
				writeFastABFile(t, dir, "ledger_east.txt", "East region ledger. Auditor: Mark Olsen. Certified amount: 205400 USD.")
				writeFastABFile(t, dir, "old_report_2025.txt", "2025 archive. Auditor then: Julia Kern. Amount: 287300 USD. Superseded.")
				writeFastABFile(t, dir, "draft_notes.txt", "Draft only. Estimated West amount 300000 USD, unverified.")
				writeFastABFile(t, dir, "todo.txt", "Remember to file the audit paperwork by Friday.")
				return fmt.Sprintf("从 %s/report_main.txt 开始沿引用追查：West region 的认证审计人是谁、认证终值是多少？严格输出两行：\nAUDITOR: <姓名>\nAMOUNT: <数值> USD", dir)
			},
			validate: func(answer, _ string, _ *fastABHandler, _ *chainProbeTool) []string {
				var failures []string
				if !strings.Contains(answer, "Priya Anand") {
					failures = append(failures, "wrong_auditor")
				}
				if !strings.Contains(strings.ReplaceAll(answer, ",", ""), "318750") {
					failures = append(failures, "wrong_amount")
				}
				for _, distractor := range []string{"Mark Olsen", "Julia Kern", "300000", "287300", "205400"} {
					if strings.Contains(strings.ReplaceAll(answer, ",", ""), distractor) {
						failures = append(failures, "distractor_leak")
						break
					}
				}
				return failures
			},
		},
		{
			name: "stateful_ledger",
			setup: func(_ *testing.T, _ string) string {
				return "用 ledger_op 工具按顺序精确执行以下 12 个库存操作，全部完成后调用 query 获取终态，最后严格输出两行：\nWIDGETS: <widgets 数量>\nTOTAL: <所有条目数量总和>\n操作序列：\n" +
					"1. add widgets 40\n2. add gears 15\n3. add bolts 100\n4. remove widgets 12\n5. set gears 50\n6. add widgets 7\n" +
					"7. remove bolts 30\n8. add springs 22\n9. remove gears 50\n10. set bolts 64\n11. add widgets 5\n12. remove springs 2\n"
			},
			validate: nil, // hard runner validates via the ledger tool + answer
		},
	}
}

func hardConfigInput() string {
	var sb strings.Builder
	type svc struct {
		name       string
		timeout    int
		env        string
		replicas   int
		logLevel   string
		deprecated bool
	}
	services := []svc{
		{"auth", 30, "dev", 2, "debug", false},
		{"billing", 60, "prod", 4, "info", false},
		{"catalog", 45, "dev", 1, "debug", true},
		{"delivery", 120, "dev", 3, "warn", false},
		{"email", 15, "prod", 2, "debug", false},
		{"feed", 90, "dev", 5, "info", true},
		{"gateway", 20, "prod", 6, "info", false},
		{"history", 75, "dev", 1, "debug", false},
		{"invoice", 40, "prod", 2, "warn", true},
		{"jobs", 300, "dev", 4, "debug", false},
		{"kv", 10, "prod", 3, "info", false},
		{"ledger", 55, "dev", 2, "debug", false},
	}
	for i, s := range services {
		if i > 0 {
			sb.WriteString("\n")
		}
		sb.WriteString(fmt.Sprintf("service: %s\n", s.name))
		sb.WriteString(fmt.Sprintf("  timeout_secs: %d\n", s.timeout))
		sb.WriteString(fmt.Sprintf("  env: %s\n", s.env))
		sb.WriteString(fmt.Sprintf("  replicas: %d\n", s.replicas))
		sb.WriteString(fmt.Sprintf("  log_level: %s\n", s.logLevel))
		if s.deprecated {
			sb.WriteString("  deprecated: true\n")
		}
	}
	return sb.String()
}

func validateHardConfigRewrite(dir string) []string {
	var failures []string
	body, err := os.ReadFile(filepath.Join(dir, "services_out.conf"))
	if err != nil {
		return []string{"output_file_missing"}
	}
	out := string(body)
	for _, gone := range []string{"service: catalog", "service: feed", "service: invoice", "deprecated: true"} {
		if strings.Contains(out, gone) {
			failures = append(failures, "deprecated_block_kept:"+gone)
		}
	}
	// Doubled timeouts for the nine surviving services.
	for _, want := range []string{
		"service: auth", "timeout_secs: 60",
		"service: delivery", "timeout_secs: 240",
		"service: email", "timeout_secs: 30",
		"service: gateway", "timeout_secs: 40",
		"service: history", "timeout_secs: 150",
		"service: jobs", "timeout_secs: 600",
		"service: kv", "timeout_secs: 20",
		"service: ledger", "timeout_secs: 110",
		"timeout_secs: 120", // billing 60→120
	} {
		if !strings.Contains(out, want) {
			failures = append(failures, "missing:"+want)
		}
	}
	if strings.Contains(out, "env: dev") {
		failures = append(failures, "env_dev_left")
	}
	if strings.Contains(out, "log_level: debug") {
		failures = append(failures, "debug_left")
	}
	for _, bad := range []string{"replicas: 1", "replicas: 2"} {
		if strings.Contains(out, bad) {
			failures = append(failures, "low_replicas_left")
			break
		}
	}
	return failures
}

func TestLive_FastPinnedHardTier(t *testing.T) {
	if os.Getenv("SHANNON_E2E_LIVE") != "1" {
		t.Skip("set SHANNON_E2E_LIVE=1 to authorize real provider calls")
	}
	if os.Getenv(fastPinnedHardGateEnv) != "1" {
		t.Skipf("set %s=1 to run the paid hard-tier lane", fastPinnedHardGateEnv)
	}
	cfg := loadAgentLabQualityConfig(t)
	provider := client.NewGatewayClient(cfg.endpoint, cfg.apiKey)

	repetitions := 3
	if raw := strings.TrimSpace(os.Getenv(fastPinnedHardRepsEnv)); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > 30 {
			t.Fatalf("%s must be in [1,30]", fastPinnedHardRepsEnv)
		}
		repetitions = value
	}
	sample := fastPinnedSample(t, fastPinnedHardSampleEnv, repetitions)

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
		t.Fatalf("Fast profile resolve failed (resolveErr=%v validate=%v)", resolveErr, fastProfile.ValidateFast())
	}
	t.Logf("fast profile pinned: id=%s model=%s", fastProfile.ProfileID, fastProfile.Model)

	cases := fastPinnedHardCases()
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
	rand.New(rand.NewSource(fastPinnedHardSeed)).Shuffle(len(jobs), func(i, j int) { jobs[i], jobs[j] = jobs[j], jobs[i] })

	var results []fastABRun
	totalCost := 0.0
	for _, jb := range jobs {
		run := runFastHardJob(t, provider, fastProfile, cases[jb.caseIndex], jb.arm, jb.repetition)
		totalCost += run.CostUSD
		results = append(results, run)
		t.Logf("fast_hard case=%s arm=%s rep=%d correct=%v failures=%v latency_ms=%d llm_calls=%d cost=%.6f",
			run.Case, run.Arm, run.Repetition, run.Correct, run.Failures, run.LatencyMillis, run.LLMCalls, run.CostUSD)
		if totalCost > fastPinnedHardMaxCostUSD {
			t.Logf("cost ceiling %.2f USD exceeded at %.4f — stopping with an incomplete report", fastPinnedHardMaxCostUSD, totalCost)
			break
		}
	}
	summarizeFastHard(t, results, repetitions, totalCost, sample)
}

func runFastHardJob(
	t *testing.T,
	provider *client.GatewayClient,
	fastProfile executionprofile.Profile,
	tc fastABCase,
	arm string,
	repetition int,
) fastABRun {
	t.Helper()
	dir := t.TempDir()
	trialID := fastPinnedTrialID(fastPinnedHardSeed, tc.name, repetition)
	prompt := fastPinnedTrialPrompt(tc.setup(t, dir), trialID)

	registry := agent.NewToolRegistry()
	registry.Register(&tools.FileReadTool{})
	registry.Register(&tools.FileWriteTool{})
	registry.Register(&tools.FileEditTool{})
	registry.Register(&tools.DirectoryListTool{})
	registry.Register(&tools.GlobTool{})
	registry.Register(&tools.CalculateTool{})
	var hardChain *hardChainTool
	var ledger *ledgerTool
	switch tc.name {
	case "long_chain_12":
		hardChain = &hardChainTool{}
		registry.Register(hardChain)
	case "stateful_ledger":
		ledger = &ledgerTool{}
		registry.Register(ledger)
	}

	cacheOffProvider := &fastPinnedCacheOffClient{inner: provider}
	loop := agent.NewAgentLoop(cacheOffProvider, registry, "medium", dir, 30, 30_000, 200, nil, nil, nil)
	loop.SetCacheSource("fast_pinned_hard")
	loop.SetSkillDiscovery(false)
	loop.SetMaxTokens(4000)
	loop.SetTemperature(0)
	handler := &fastABHandler{}
	loop.SetHandler(handler)

	if arm == "fast" {
		loop.SetKoeExecutionProfile(fastProfile)
	} else {
		loop.SetSpecificModel(fastPinnedABFullModel)
		loop.SetThinking(&client.ThinkingConfig{Type: "adaptive"})
		loop.SetKoeExecutionProfile(executionprofile.FullProfile(executionprofile.ModeFull, "fast_hard_control"))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 420*time.Second)
	defer cancel()
	started := time.Now()
	answer, usage, err := loop.Run(ctx, prompt, nil, nil)
	run := fastABRun{
		Case: tc.name, Arm: arm, Repetition: repetition,
		TrialID:       trialID,
		LatencyMillis: time.Since(started).Milliseconds(),
		Answer:        strings.TrimSpace(answer),
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
		run.Failures = append(run.Failures, "usage_not_observed")
	}
	if !run.CostObserved {
		run.Failures = append(run.Failures, "cost_not_observed")
	}
	if run.Answer == "" {
		run.Failures = append(run.Failures, "terminal_answer_missing")
	}
	switch tc.name {
	case "long_chain_12":
		if !strings.Contains(run.Answer, "LONGCHAIN-457") {
			run.Failures = append(run.Failures, "missing_final_code")
		}
		hardChain.mu.Lock()
		calls, bad := hardChain.calls, hardChain.badTokens
		hardChain.mu.Unlock()
		if calls != hardChainSteps {
			run.Failures = append(run.Failures, fmt.Sprintf("chain_calls_%d_not_%d", calls, hardChainSteps))
		}
		if bad != 0 {
			run.Failures = append(run.Failures, fmt.Sprintf("bad_tokens_%d", bad))
		}
	case "stateful_ledger":
		// Expected final state: widgets 40, gears 0 (removed), bolts 64,
		// springs 20 → widgets=40, total=124.
		if !strings.Contains(run.Answer, "40") {
			run.Failures = append(run.Failures, "wrong_widgets")
		}
		if !strings.Contains(run.Answer, "124") {
			run.Failures = append(run.Failures, "wrong_total")
		}
		ledger.mu.Lock()
		ops := ledger.ops
		ledger.mu.Unlock()
		if ops != 12 {
			run.Failures = append(run.Failures, fmt.Sprintf("ledger_ops_%d_not_12", ops))
		}
	default:
		run.Failures = append(run.Failures, tc.validate(run.Answer, dir, handler, nil)...)
	}
	run.Correct = len(run.Failures) == 0
	return run
}

func summarizeFastHard(t *testing.T, results []fastABRun, repetitions int, totalCost float64, sample string) {
	t.Helper()
	caseNames := make([]string, 0, len(fastPinnedHardCases()))
	for _, tc := range fastPinnedHardCases() {
		caseNames = append(caseNames, tc.name)
	}
	report := newFastPinnedQualificationReport(
		"kocoro.fast_pinned_hard.v3", fastPinnedHardSeed, sample, repetitions,
		caseNames, results, totalCost, fastPinnedHardMaxCostUSD,
	)
	for _, cell := range report.Cells {
		t.Logf("hard %-28s correct=%d/%d median_ms=%d p90_ms=%d mean_llm_calls=%.1f total_cost=%.6f",
			cell.Case+"/"+cell.Arm, cell.CorrectRuns, cell.Runs,
			cell.MedianMillis, cell.P90Millis, cell.MeanLLMCalls, cell.CostUSD)
	}
	t.Logf("fast_pinned_hard complete=%t runs=%d repetitions=%d comparison_qualifying=%t release_qualifying=%t total_cost=%.6f",
		report.Complete, len(results), repetitions, report.ComparisonQualifying, report.ReleaseQualifying, totalCost)

	outputPath := strings.TrimSpace(os.Getenv(fastPinnedHardOutputEnv))
	if outputPath == "" {
		outputPath = filepath.Join(os.TempDir(), fmt.Sprintf("kocoro-fast-pinned-hard-%d.json", fastPinnedHardSeed))
	}
	if err := writeFastPinnedQualificationReport(outputPath, report); err != nil {
		t.Fatalf("write report: %v", err)
	}
	t.Logf("report=%s", outputPath)
	assertFastPinnedQualification(t, report, outputPath)
}
