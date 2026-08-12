//go:build live

package e2e

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/executionprofile"
)

const responseDetailABGateEnv = "KOCORO_RESPONSE_DETAIL_AB"

type responseDetailCacheOffClient struct {
	inner client.LLMClient
}

func (c *responseDetailCacheOffClient) Complete(
	ctx context.Context,
	req client.CompletionRequest,
) (*client.CompletionResponse, error) {
	req.ResponseCachePolicy = executionprofile.ResponseCacheOff
	return c.inner.Complete(ctx, req)
}

func (c *responseDetailCacheOffClient) CompleteStream(
	ctx context.Context,
	req client.CompletionRequest,
	onDelta func(client.StreamDelta),
) (*client.CompletionResponse, error) {
	req.ResponseCachePolicy = executionprofile.ResponseCacheOff
	return c.inner.CompleteStream(ctx, req, onDelta)
}

type responseDetailHandler struct {
	usage agent.UsageAccumulator
}

func (*responseDetailHandler) OnToolCall(string, string, string) {}
func (*responseDetailHandler) OnToolResult(string, string, string, agent.ToolResult, time.Duration) {
}
func (*responseDetailHandler) OnText(string)                        {}
func (*responseDetailHandler) OnPreamble(string)                    {}
func (*responseDetailHandler) OnStreamDelta(string)                 {}
func (*responseDetailHandler) OnApprovalNeeded(string, string) bool { return false }
func (h *responseDetailHandler) OnUsage(usage agent.TurnUsage)      { h.usage.Add(usage) }
func (*responseDetailHandler) OnCloudAgent(string, string, string)  {}
func (*responseDetailHandler) OnCloudProgress(int, int)             {}
func (*responseDetailHandler) OnCloudPlan(string, string, bool)     {}

type responseDetailABResult struct {
	answer       string
	words        int
	latency      time.Duration
	outputTokens int
	costUSD      float64
}

func TestLive_ResponseDetailAcrossProviders(t *testing.T) {
	if os.Getenv("SHANNON_E2E_LIVE") != "1" {
		t.Skip("set SHANNON_E2E_LIVE=1 to authorize real provider calls")
	}
	if os.Getenv(responseDetailABGateEnv) != "1" {
		t.Skipf("set %s=1 to run the paid response-detail A/B lane", responseDetailABGateEnv)
	}

	cfg := loadAgentLabQualityConfig(t)
	provider := client.NewGatewayClient(cfg.endpoint, cfg.apiKey)
	models := []string{"gpt-5.6-luna", "claude-sonnet-5"}
	levels := []string{"concise", "balanced", "detailed"}
	prompts := []string{
		"A friend asks why seasons happen and why summer is not caused by Earth being closer to the Sun. Explain it so an educated non-specialist can understand.",
		"Explain the practical difference between a compiler and an interpreter to a product manager who can read code but does not work on language runtimes.",
		"Explain the main tradeoffs between renting and buying a home without assuming a particular city, income, or interest rate.",
	}

	for _, model := range models {
		t.Run(model, func(t *testing.T) {
			totalWords := make(map[string]int, len(levels))
			for promptIndex, prompt := range prompts {
				for _, level := range levels {
					result := runResponseDetailAB(t, provider, model, level, promptIndex, prompt)
					totalWords[level] += result.words
				}
			}

			concise := totalWords["concise"]
			balanced := totalWords["balanced"]
			detailed := totalWords["detailed"]
			if concise*100 > detailed*85 || concise*100 > balanced*115 || balanced*100 > detailed*115 {
				t.Fatalf(
					"response detail aggregate outside tolerance for %s: concise=%d balanced=%d detailed=%d",
					model, concise, balanced, detailed,
				)
			}
			t.Logf("response_detail_ab_aggregate model=%s concise=%d balanced=%d detailed=%d", model, concise, balanced, detailed)
		})
	}
}

func runResponseDetailAB(
	t *testing.T,
	provider client.LLMClient,
	model string,
	level string,
	promptIndex int,
	prompt string,
) responseDetailABResult {
	t.Helper()
	wrapped := &responseDetailCacheOffClient{inner: provider}
	loop := agent.NewAgentLoop(wrapped, agent.NewToolRegistry(), "medium", t.TempDir(), 2, 30_000, 200, nil, nil, nil)
	loop.SetSpecificModel(model)
	loop.SetResponseDetail(level)
	loop.SetSkillDiscovery(false)
	loop.SetMaxTokens(3000)
	loop.SetTemperature(0)
	handler := &responseDetailHandler{}
	loop.SetHandler(handler)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	started := time.Now()
	answer, _, err := loop.Run(ctx, prompt, nil, nil)
	latency := time.Since(started)
	if err != nil {
		t.Fatalf("%s/%s run failed: %v", model, level, err)
	}
	if strings.TrimSpace(answer) == "" {
		t.Fatalf("%s/%s returned an empty answer", model, level)
	}
	status := loop.LastRunStatus()
	if status.Partial {
		t.Fatalf("%s/%s returned partial status: failure_code=%s", model, level, status.FailureCode)
	}

	usage := handler.usage.Snapshot()
	result := responseDetailABResult{
		answer:       strings.TrimSpace(answer),
		words:        len(strings.Fields(answer)),
		latency:      latency,
		outputTokens: usage.LLM.OutputTokens,
		costUSD:      usage.TotalCostUSD(),
	}
	t.Logf(
		"response_detail_ab model=%s prompt=%d level=%s words=%d output_tokens=%d latency=%s cost=$%.6f",
		model, promptIndex, level, result.words, result.outputTokens, result.latency.Round(time.Millisecond), result.costUSD,
	)
	return result
}
