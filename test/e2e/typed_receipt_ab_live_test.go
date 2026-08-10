package e2e

// Typed receipt vs free text fidelity A/B.
//
// Prior serial-tool experiments showed the model corrupting a 12-hex receipt
// when repeating it as free answer text (duplicated suffix, e.g.
// 29B388EA171B -> ...171B171B); extra prompt wording did not help and the
// stream/final texts were byte-identical, so the corruption happens at
// generation time. Hypothesis under test: the same value survives verbatim
// when the model writes it as a JSON tool argument instead of free text.
//
// Both arms pin the Cloud kfp1 Fast profile (the context where the historical
// failures occurred). Every run uses a fresh seeded receipt so memorization
// cannot help.
//
// Run:
//
//	SHANNON_E2E_LIVE=1 KOCORO_TYPED_RECEIPT_AB=1 go test ./test/e2e/ -run TestLive_TypedReceiptAB -v

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/executionprofile"
)

const (
	typedReceiptGateEnv    = "KOCORO_TYPED_RECEIPT_AB"
	typedReceiptOutputEnv  = "KOCORO_TYPED_RECEIPT_AB_OUTPUT"
	typedReceiptSeed       = int64(20260810)
	typedReceiptReps       = 10
	typedReceiptMaxCostUSD = 1.0
)

func typedReceiptNonce(step int) string { return fmt.Sprintf("NX%d-4c", step) }

// typedReceiptStepTool is a strict three-step serial chain whose final step
// returns the receipt. Steps are token-gated so the chain cannot be skipped.
type typedReceiptStepTool struct {
	mu      sync.Mutex
	calls   int
	receipt string
}

func (t *typedReceiptStepTool) Info() agent.ToolInfo {
	return agent.ToolInfo{
		Name:        "receipt_step",
		Description: "Advance the three-step serial task. Call with step=1 and nonce=START first; each response names the nonce the next step requires. Step 3 returns the final receipt.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"step":  map[string]any{"type": "integer", "description": "Step number 1-3."},
				"nonce": map[string]any{"type": "string", "description": "Nonce returned by the previous step (START for step 1)."},
			},
		},
		Required: []string{"step", "nonce"},
	}
}

func (t *typedReceiptStepTool) RequiresApproval() bool     { return false }
func (t *typedReceiptStepTool) IsReadOnlyCall(string) bool { return false }

func (t *typedReceiptStepTool) Run(_ context.Context, argsJSON string) (agent.ToolResult, error) {
	var args struct {
		Step  int    `json:"step"`
		Nonce string `json:"nonce"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil || args.Step < 1 || args.Step > 3 {
		return agent.ValidationError("step must be 1-3 and nonce is required"), nil
	}
	expected := "START"
	if args.Step > 1 {
		expected = typedReceiptNonce(args.Step - 1)
	}
	if strings.TrimSpace(args.Nonce) != expected {
		return agent.ValidationError(fmt.Sprintf("wrong nonce for step %d", args.Step)), nil
	}
	t.mu.Lock()
	t.calls++
	t.mu.Unlock()
	if args.Step < 3 {
		return agent.ToolResult{Content: fmt.Sprintf("STEP-%d OK. Next: call step=%d with nonce=%s.", args.Step, args.Step+1, typedReceiptNonce(args.Step))}, nil
	}
	return agent.ToolResult{Content: fmt.Sprintf("STEP-3 OK. FINAL RECEIPT: %s. Deliver it exactly.", t.receipt)}, nil
}

func (t *typedReceiptStepTool) count() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.calls
}

// typedReceiptReportTool records the receipt the model submits as a JSON
// argument — the typed channel under test.
type typedReceiptReportTool struct {
	mu       sync.Mutex
	received []string
}

func (t *typedReceiptReportTool) Info() agent.ToolInfo {
	return agent.ToolInfo{
		Name:        "report_result",
		Description: "Submit the final receipt exactly as returned by step 3.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"receipt": map[string]any{"type": "string", "description": "The final receipt, verbatim."},
			},
		},
		Required: []string{"receipt"},
	}
}

func (t *typedReceiptReportTool) RequiresApproval() bool     { return false }
func (t *typedReceiptReportTool) IsReadOnlyCall(string) bool { return false }

func (t *typedReceiptReportTool) Run(_ context.Context, argsJSON string) (agent.ToolResult, error) {
	var args struct {
		Receipt string `json:"receipt"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil || strings.TrimSpace(args.Receipt) == "" {
		return agent.ValidationError("receipt is required"), nil
	}
	t.mu.Lock()
	t.received = append(t.received, args.Receipt)
	t.mu.Unlock()
	return agent.ToolResult{Content: "ok, receipt recorded"}, nil
}

func (t *typedReceiptReportTool) snapshot() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string(nil), t.received...)
}

func typedReceiptValue(rng *rand.Rand) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < 12; i++ {
		b.WriteByte(hex[rng.Intn(len(hex))])
	}
	return b.String()
}

type typedReceiptRun struct {
	Arm           string `json:"arm"`
	Repetition    int    `json:"repetition"`
	Receipt       string `json:"receipt"`
	Exact         bool   `json:"exact"`
	Submitted     string `json:"submitted"`
	Answer        string `json:"answer"`
	StepCalls     int    `json:"step_calls"`
	ReportCalls   int    `json:"report_calls"`
	ContainsValue bool   `json:"answer_contains_value"`
	LatencyMillis int64  `json:"latency_millis"`
	LLMCalls      int    `json:"llm_calls"`
	CostUSD       float64 `json:"cost_usd"`
}

func TestLive_TypedReceiptAB(t *testing.T) {
	if os.Getenv("SHANNON_E2E_LIVE") != "1" {
		t.Skip("set SHANNON_E2E_LIVE=1 to authorize real provider calls")
	}
	if os.Getenv(typedReceiptGateEnv) != "1" {
		t.Skipf("set %s=1 to run the paid typed-receipt A/B lane", typedReceiptGateEnv)
	}
	cfg := loadAgentLabQualityConfig(t)
	provider := client.NewGatewayClient(cfg.endpoint, cfg.apiKey)

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

	type job struct {
		arm string
		rep int
	}
	var jobs []job
	for rep := 1; rep <= typedReceiptReps; rep++ {
		jobs = append(jobs, job{"free_text", rep}, job{"typed", rep})
	}
	rand.New(rand.NewSource(typedReceiptSeed)).Shuffle(len(jobs), func(i, j int) { jobs[i], jobs[j] = jobs[j], jobs[i] })

	var results []typedReceiptRun
	totalCost := 0.0
	for _, jb := range jobs {
		if totalCost > typedReceiptMaxCostUSD {
			t.Fatalf("cost ceiling %.2f USD exceeded at %.4f", typedReceiptMaxCostUSD, totalCost)
		}
		run := runTypedReceiptJob(t, provider, fastProfile, jb.arm, jb.rep)
		totalCost += run.CostUSD
		results = append(results, run)
		t.Logf("typed_receipt arm=%s rep=%d exact=%v receipt=%s submitted=%q latency_ms=%d cost=%.6f",
			run.Arm, run.Repetition, run.Exact, run.Receipt, run.Submitted, run.LatencyMillis, run.CostUSD)
	}

	byArm := map[string][2]int{}
	for _, r := range results {
		cell := byArm[r.Arm]
		cell[1]++
		if r.Exact {
			cell[0]++
		}
		byArm[r.Arm] = cell
	}
	for _, arm := range []string{"free_text", "typed"} {
		cell := byArm[arm]
		t.Logf("ARM %-9s exact=%d/%d", arm, cell[0], cell[1])
	}
	t.Logf("typed_receipt_ab complete runs=%d total_cost=%.6f", len(results), totalCost)

	outputPath := strings.TrimSpace(os.Getenv(typedReceiptOutputEnv))
	if outputPath == "" {
		outputPath = filepath.Join(os.TempDir(), fmt.Sprintf("kocoro-typed-receipt-ab-%d.json", typedReceiptSeed))
	}
	body, err := json.MarshalIndent(map[string]any{
		"schema_version": "kocoro.typed_receipt_ab.v1",
		"seed":           typedReceiptSeed,
		"repetitions":    typedReceiptReps,
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

func runTypedReceiptJob(
	t *testing.T,
	provider *client.GatewayClient,
	fastProfile executionprofile.Profile,
	arm string,
	rep int,
) typedReceiptRun {
	t.Helper()
	armIndex := int64(0)
	if arm == "typed" {
		armIndex = 1
	}
	rng := rand.New(rand.NewSource(typedReceiptSeed + int64(rep)*1000 + armIndex))
	receipt := typedReceiptValue(rng)

	registry := agent.NewToolRegistry()
	step := &typedReceiptStepTool{receipt: receipt}
	registry.Register(step)
	var report *typedReceiptReportTool
	if arm == "typed" {
		report = &typedReceiptReportTool{}
		registry.Register(report)
	}

	loop := agent.NewAgentLoop(provider, registry, "medium", t.TempDir(), 10, 30_000, 200, nil, nil, nil)
	loop.SetCacheSource("typed_receipt_ab")
	loop.SetSkillDiscovery(false)
	loop.SetMaxTokens(400)
	loop.SetTemperature(0)
	loop.SetHandler(&fastABHandler{})
	loop.SetKoeExecutionProfile(fastProfile)

	prompt := "使用 receipt_step 工具完成三步严格串行任务：从 step=1、nonce=START 开始，每一步的返回会告诉你下一步需要的 nonce。完成第 3 步后，只输出最终 receipt，逐字精确，不要任何其他文字。"
	if arm == "typed" {
		prompt = "使用 receipt_step 工具完成三步严格串行任务：从 step=1、nonce=START 开始，每一步的返回会告诉你下一步需要的 nonce。完成第 3 步后，通过 report_result 工具提交最终 receipt（逐字精确），然后回复 done。"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	started := time.Now()
	answer, usage, err := loop.Run(ctx, prompt, nil, nil)
	run := typedReceiptRun{
		Arm: arm, Repetition: rep, Receipt: receipt,
		Answer:        strings.TrimSpace(answer),
		StepCalls:     step.count(),
		LatencyMillis: time.Since(started).Milliseconds(),
		ContainsValue: strings.Contains(answer, receipt),
	}
	if err != nil {
		run.Submitted = "loop_error:" + err.Error()
		return run
	}
	if usage != nil {
		run.LLMCalls = usage.LLMCalls
		run.CostUSD = usage.CostUSD
	}
	if arm == "typed" {
		received := report.snapshot()
		run.ReportCalls = len(received)
		if len(received) > 0 {
			run.Submitted = received[len(received)-1]
			run.Exact = run.Submitted == receipt && len(received) == 1
		}
	} else {
		run.Submitted = run.Answer
		run.Exact = run.Answer == receipt
	}
	return run
}
