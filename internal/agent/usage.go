package agent

import (
	"context"
	"sync"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

// usageEmitKey is a context value used by the agent loop to expose a per-run
// usage sink to tools. Tools that report their own cost (gateway tools calling
// xAI/Grok, SerpAPI, etc.) call EmitUsage with the converted TurnUsage; the
// handler attached to the loop receives it like any other OnUsage event.
type usageEmitKey struct{}

// WithUsageEmit returns a context carrying a usage emitter. Callers (typically
// the agent loop at tool dispatch time) wrap their ctx with this before
// handing it to a tool's Run method.
func WithUsageEmit(ctx context.Context, emit func(TurnUsage)) context.Context {
	if emit == nil {
		return ctx
	}
	return context.WithValue(ctx, usageEmitKey{}, emit)
}

// EmitUsage forwards a usage report through the ctx-attached emitter.
// Safe to call from any tool — no-op if the ctx has no emitter.
func EmitUsage(ctx context.Context, u TurnUsage) {
	if emit, ok := ctx.Value(usageEmitKey{}).(func(TurnUsage)); ok && emit != nil {
		emit(u)
	}
}

// UsageProvider is the optional interface a handler can satisfy to expose
// its accumulated usage. Callers (daemon runner, CLI, TUI) type-assert and
// read Usage() at end-of-run for persistence/display. Returns the combined
// LLM + tool breakdown so callers can report each independently.
type UsageProvider interface {
	Usage() AccumulatedUsage
}

// AccumulatedUsage is the combined snapshot returned by UsageAccumulator.
// LLM and tool billing are tracked separately so callers can report the
// token breakdown without mixing model tokens with gateway tool synthetic
// counts (e.g. SERP tools' 7500-token-per-query billing abstraction).
//
// The invariant input_tokens+output_tokens == total_tokens holds on LLM
// only; ToolCostUSD/ToolTokens are additive on top for "total spend"
// summaries but should never be folded into the LLM token fields.
type AccumulatedUsage struct {
	LLM         TurnUsage // model-only: input/output/cache tokens, LLMCalls, Model
	ToolCalls   int       // count of gateway-tool emissions (tools that billed)
	ToolTokens  int       // sum of gateway-tool reported tokens (may be synthetic)
	ToolCostUSD float64   // sum of gateway-tool cost_usd
}

// TotalCostUSD returns the combined LLM + tool cost.
func (a AccumulatedUsage) TotalCostUSD() float64 {
	return a.LLM.CostUSD + a.ToolCostUSD
}

// UsageAccumulator is a thread-safe collector that handlers embed to
// aggregate TurnUsage events across a run/session. It separates LLM
// events (agent loop + cloud_delegate nested calls, signalled by
// LLMCalls > 0) from gateway tool billing events (server.go emissions,
// signalled by LLMCalls == 0) so the caller can report each independently.
//
// Typical flow:
//  1. Handler embeds an UsageAccumulator (value or pointer).
//  2. Handler's OnUsage(u TurnUsage) calls accumulator.Add(u).
//  3. Caller queries Snapshot() at end-of-run and persists it.
//
// The zero value is ready to use.
type UsageAccumulator struct {
	mu          sync.Mutex
	llm         TurnUsage
	toolCalls   int
	toolTokens  int
	toolCostUSD float64
}

// LLMUsageDelta converts a provider usage payload into the normalized LLM-side
// TurnUsage delta used by handlers, session persistence, and cloud_delegate.
func LLMUsageDelta(u client.Usage, model string) TurnUsage {
	u = u.Normalized()
	return TurnUsage{
		InputTokens:           u.InputTokens,
		OutputTokens:          u.OutputTokens,
		TotalTokens:           u.TotalTokens,
		CostUSD:               u.CostUSD,
		LLMCalls:              1,
		WebSearchCalls:        u.WebSearchCalls,
		Model:                 model,
		CacheReadTokens:       u.CacheReadTokens,
		CacheCreationTokens:   u.CacheCreationTokens,
		CacheCreation5mTokens: u.CacheCreation5mTokens,
		CacheCreation1hTokens: u.CacheCreation1hTokens,
	}
}

// Add merges an incoming TurnUsage delta into the running total, routing
// tool-only emissions (LLMCalls == 0) to the separate tool counters so
// LLM token fields stay consistent (input+output == total).
func (a *UsageAccumulator) Add(u TurnUsage) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if u.LLMCalls == 0 {
		// Tool-only emission: server.go reports gateway-tool billing here.
		// Keep it out of LLM token fields so total_tokens remains explainable
		// as input_tokens + output_tokens on the LLM side.
		a.toolCalls++
		a.toolTokens += u.TotalTokens
		a.toolCostUSD += u.CostUSD
		return
	}
	a.llm.InputTokens += u.InputTokens
	a.llm.OutputTokens += u.OutputTokens
	a.llm.TotalTokens += u.TotalTokens
	a.llm.CostUSD += u.CostUSD
	a.llm.CacheReadTokens += u.CacheReadTokens
	a.llm.CacheCreationTokens += u.CacheCreationTokens
	a.llm.CacheCreation5mTokens += u.CacheCreation5mTokens
	a.llm.CacheCreation1hTokens += u.CacheCreation1hTokens
	a.llm.LLMCalls += u.LLMCalls
	a.llm.WebSearchCalls += u.WebSearchCalls
	if u.Model != "" {
		a.llm.Model = u.Model
	}
}

// Snapshot returns the current cumulative totals split into LLM and tool
// buckets. Safe to call from any goroutine.
func (a *UsageAccumulator) Snapshot() AccumulatedUsage {
	a.mu.Lock()
	defer a.mu.Unlock()
	return AccumulatedUsage{
		LLM:         a.llm,
		ToolCalls:   a.toolCalls,
		ToolTokens:  a.toolTokens,
		ToolCostUSD: a.toolCostUSD,
	}
}

// totalPromptTokens returns the effective prompt size for compaction decisions.
// Anthropic's input_tokens excludes cached tokens (they're reported separately
// as cache_read_input_tokens / cache_creation_input_tokens but still count
// against the model's context window). Using raw input_tokens as the
// compaction gate means warm-cache sessions never trip the threshold.
func totalPromptTokens(u client.Usage) int {
	return u.InputTokens + u.CacheReadTokens + u.CacheCreationTokens
}

// Reset clears accumulated totals. Use between independent runs in a
// long-lived handler (e.g. daemon per-message handler reuse).
func (a *UsageAccumulator) Reset() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.llm = TurnUsage{}
	a.toolCalls = 0
	a.toolTokens = 0
	a.toolCostUSD = 0
}
