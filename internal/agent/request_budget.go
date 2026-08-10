package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
	ctxwin "github.com/Kocoro-lab/ShanClaw/internal/context"
)

const (
	requestBudgetNormalDispatchLimit   = 64
	requestBudgetHelperDispatchLimit   = 8
	requestBudgetTerminalDispatchLimit = 1
	requestBudgetMinimumTokenExposure  = int64(1_000_000)
	requestBudgetContextMultiplier     = int64(8)
)

// ErrRequestBudgetExhausted is returned before a provider dispatch when the
// current AgentLoop.Run has exhausted its request-scoped provider budget.
var ErrRequestBudgetExhausted = errors.New("agent provider request budget exhausted")

type requestBudgetClass uint8

const (
	requestBudgetMain requestBudgetClass = iota
	requestBudgetHelper
	requestBudgetTerminal
	requestBudgetFork
)

func (c requestBudgetClass) String() string {
	switch c {
	case requestBudgetMain:
		return "main"
	case requestBudgetHelper:
		return "helper"
	case requestBudgetTerminal:
		return "terminal"
	case requestBudgetFork:
		return "fork"
	default:
		return "unknown"
	}
}

type requestBudgetError struct {
	class  requestBudgetClass
	reason string
}

func (e *requestBudgetError) Error() string {
	return fmt.Sprintf("%v: class=%s reason=%s", ErrRequestBudgetExhausted, e.class, e.reason)
}

func (e *requestBudgetError) Unwrap() error { return ErrRequestBudgetExhausted }

// requestLLMBudget bounds provider exposure for one AgentLoop.Run. Normal,
// helper, and fork calls share the normal dispatch and token caps. Terminal
// synthesis has one independent dispatch so exhaustion can still produce a
// useful partial response. Terminal exposure remains observable in the
// snapshot but does not compete with the already-exhausted normal token pool.
type requestLLMBudget struct {
	mu sync.Mutex

	tokenExposureLimit int64

	normalDispatches   int
	helperDispatches   int
	terminalDispatches int

	reservedTokens int64
	consumedTokens int64
	terminalTokens int64
	unknownActual  int
}

type requestBudgetSnapshot struct {
	TokenExposureLimit int64
	NormalDispatches   int
	HelperDispatches   int
	TerminalDispatches int
	ReservedTokens     int64
	ConsumedTokens     int64
	TerminalTokens     int64
	UnknownActual      int
}

func newRequestLLMBudget(contextWindow int) *requestLLMBudget {
	limit := requestBudgetMinimumTokenExposure
	if contextWindow > 0 {
		if scaled := int64(contextWindow) * requestBudgetContextMultiplier; scaled > limit {
			limit = scaled
		}
	}
	return &requestLLMBudget{tokenExposureLimit: limit}
}

type requestBudgetReservation struct {
	budget   *requestLLMBudget
	class    requestBudgetClass
	estimate int64
	once     sync.Once
}

func (b *requestLLMBudget) reserve(class requestBudgetClass, estimate int64) (*requestBudgetReservation, error) {
	if estimate < 0 {
		estimate = 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	if class == requestBudgetTerminal {
		if b.terminalDispatches >= requestBudgetTerminalDispatchLimit {
			return nil, &requestBudgetError{class: class, reason: "terminal_dispatch_limit"}
		}
		b.terminalDispatches++
		return &requestBudgetReservation{budget: b, class: class, estimate: estimate}, nil
	}

	if b.normalDispatches >= requestBudgetNormalDispatchLimit {
		return nil, &requestBudgetError{class: class, reason: "normal_dispatch_limit"}
	}
	if class == requestBudgetHelper && b.helperDispatches >= requestBudgetHelperDispatchLimit {
		return nil, &requestBudgetError{class: class, reason: "helper_dispatch_limit"}
	}
	if b.consumedTokens+b.reservedTokens+estimate > b.tokenExposureLimit {
		return nil, &requestBudgetError{class: class, reason: "token_exposure_limit"}
	}

	b.normalDispatches++
	if class == requestBudgetHelper {
		b.helperDispatches++
	}
	b.reservedTokens += estimate
	return &requestBudgetReservation{budget: b, class: class, estimate: estimate}, nil
}

func (r *requestBudgetReservation) reconcile(resp *client.CompletionResponse, callErr error) {
	if r == nil || r.budget == nil {
		return
	}
	r.once.Do(func() {
		actual, known := responseTokenExposure(resp)
		b := r.budget
		b.mu.Lock()
		defer b.mu.Unlock()

		if r.class == requestBudgetTerminal {
			if known {
				b.terminalTokens += actual
			} else {
				b.terminalTokens += r.estimate
				b.unknownActual++
			}
			return
		}

		b.reservedTokens -= r.estimate
		if b.reservedTokens < 0 {
			b.reservedTokens = 0
		}
		if known {
			b.consumedTokens += actual
			return
		}
		// Provider failures and successful non-cached responses with no usage
		// remain charged at the pre-dispatch estimate. Releasing them would make
		// missing telemetry an admission bypass. callErr is intentionally not used
		// to guess whether the upstream billed the attempt.
		_ = callErr
		b.consumedTokens += r.estimate
		b.unknownActual++
	})
}

func (b *requestLLMBudget) snapshot() requestBudgetSnapshot {
	b.mu.Lock()
	defer b.mu.Unlock()
	return requestBudgetSnapshot{
		TokenExposureLimit: b.tokenExposureLimit,
		NormalDispatches:   b.normalDispatches,
		HelperDispatches:   b.helperDispatches,
		TerminalDispatches: b.terminalDispatches,
		ReservedTokens:     b.reservedTokens,
		ConsumedTokens:     b.consumedTokens,
		TerminalTokens:     b.terminalTokens,
		UnknownActual:      b.unknownActual,
	}
}

type budgetedLLMClient struct {
	delegate client.LLMClient
	budget   *requestLLMBudget
	class    requestBudgetClass
	overhead func() int
}

func newBudgetedLLMClient(delegate client.LLMClient, budget *requestLLMBudget, class requestBudgetClass, overhead func() int) client.LLMClient {
	return &budgetedLLMClient{delegate: delegate, budget: budget, class: class, overhead: overhead}
}

func (c *budgetedLLMClient) Complete(ctx context.Context, req client.CompletionRequest) (*client.CompletionResponse, error) {
	reservation, err := c.reserve(req)
	if err != nil {
		return nil, err
	}
	if c.delegate == nil {
		err = errors.New("agent LLM client is nil")
		reservation.reconcile(nil, err)
		return nil, err
	}
	resp, callErr := c.delegate.Complete(ctx, req)
	reservation.reconcile(resp, callErr)
	return resp, callErr
}

func (c *budgetedLLMClient) CompleteStream(ctx context.Context, req client.CompletionRequest, onDelta func(client.StreamDelta)) (*client.CompletionResponse, error) {
	reservation, err := c.reserve(req)
	if err != nil {
		return nil, err
	}
	if c.delegate == nil {
		err = errors.New("agent LLM client is nil")
		reservation.reconcile(nil, err)
		return nil, err
	}
	resp, callErr := c.delegate.CompleteStream(ctx, req, onDelta)
	reservation.reconcile(resp, callErr)
	return resp, callErr
}

func (c *budgetedLLMClient) reserve(req client.CompletionRequest) (*requestBudgetReservation, error) {
	overhead := 0
	if c.overhead != nil {
		overhead = c.overhead()
	}
	return c.budget.reserve(c.class, estimateCompletionTokenExposure(req, overhead))
}

func estimateCompletionTokenExposure(req client.CompletionRequest, calibratedOverhead int) int64 {
	if calibratedOverhead < 0 {
		calibratedOverhead = 0
	}
	promptTokens := ctxwin.EstimateTokens(req.Messages)
	schemaTokens := 0
	if len(req.Tools) > 0 {
		// Tool schemas are dense JSON and are not included in EstimateTokens.
		// bytes/3 deliberately errs above the message estimator's chars/3.5.
		// Once calibrated, estOverhead already contains schema mass; take the
		// larger estimate instead of adding both and charging schemas twice.
		if encoded, err := json.Marshal(req.Tools); err == nil {
			schemaTokens = (len(encoded) + 2) / 3
		}
	}
	if calibratedOverhead > schemaTokens {
		schemaTokens = calibratedOverhead
	}
	promptTokens += schemaTokens
	outputTokens := req.MaxTokens
	if outputTokens <= 0 {
		outputTokens = MaxTokensForModel(req.SpecificModel)
	}
	return int64(promptTokens + outputTokens)
}

func responseTokenExposure(resp *client.CompletionResponse) (int64, bool) {
	if resp == nil {
		return 0, false
	}
	u := resp.Usage.Normalized()
	known := resp.Cached || u.InputTokens != 0 || u.OutputTokens != 0 || u.TotalTokens != 0 ||
		u.CacheReadTokens != 0 || u.CacheCreationTokens != 0 ||
		u.CacheCreation5mTokens != 0 || u.CacheCreation1hTokens != 0
	if !known {
		return 0, false
	}
	baseTokens := u.InputTokens + u.OutputTokens
	if u.TotalTokens > baseTokens {
		baseTokens = u.TotalTokens
	}
	return int64(baseTokens + u.CacheReadTokens + u.CacheCreationTokens), true
}
