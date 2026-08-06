package tools

import (
	"context"
	"crypto/sha256"
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

	. "github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/config"
	"github.com/Kocoro-lab/ShanClaw/internal/executionprofile"
	"github.com/Kocoro-lab/ShanClaw/internal/keychain"
	"github.com/Kocoro-lab/ShanClaw/internal/skills"
)

// This qualification makes real provider calls and can consume substantial
// paid tokens. Normal test runs stop at the first gate before reading endpoint
// or credential environment variables.
//
// Full qualification (30 repetitions per lane/workload by default):
//
//   KOE_FAST_QUALIFICATION_LIVE=1 \
//   KOE_FAST_QUALIFICATION_ENDPOINT=https://... \
//   KOE_FAST_QUALIFICATION_API_KEY=... \
//   KOE_FAST_QUALIFICATION_OUTPUT=/tmp/koe-fast-qualification.json \
//   KOE_FAST_QUALIFICATION_REPETITIONS=30 \
//   KOE_FAST_QUALIFICATION_SMOKE=0 \
//   KOE_FAST_QUALIFICATION_MAX_COST_USD=25 \
//   go test -timeout=0 ./internal/tools \
//     -run TestKoeFastQualificationLive_AgentLoop -v
//
// A run below 30 repetitions must opt into an explicitly non-qualifying smoke:
//
//   KOE_FAST_QUALIFICATION_REPETITIONS=1 \
//   KOE_FAST_QUALIFICATION_SMOKE=1 ...
//
// The JSON artifact is deliberately content-free: it never contains prompts,
// model replies, tool arguments, execution profile ids, endpoints, or API keys.

const (
	koeQualificationGateEnv        = "KOE_FAST_QUALIFICATION_LIVE"
	koeQualificationEndpointEnv    = "KOE_FAST_QUALIFICATION_ENDPOINT"
	koeQualificationAPIKeyEnv      = "KOE_FAST_QUALIFICATION_API_KEY"
	koeQualificationOutputEnv      = "KOE_FAST_QUALIFICATION_OUTPUT"
	koeQualificationRepetitionsEnv = "KOE_FAST_QUALIFICATION_REPETITIONS"
	koeQualificationSmokeEnv       = "KOE_FAST_QUALIFICATION_SMOKE"
	koeQualificationSeedEnv        = "KOE_FAST_QUALIFICATION_SEED"
	koeQualificationMaxCostEnv     = "KOE_FAST_QUALIFICATION_MAX_COST_USD"
	koeQualificationPauseEnv       = "KOE_FAST_QUALIFICATION_PAUSE_MS"

	koeQualificationDefaultRepetitions           = 30
	koeQualificationMaxRepetitions               = 50
	koeQualificationDefaultMaxCostUSD            = 25.0
	koeQualificationHardMaxCostUSD               = 100.0
	koeQualificationDefaultSeed            int64 = 20260728
	koeQualificationMaxTokens                    = 4096
	koeQualificationMaxIterations                = 12
	koeQualificationRunTimeout                   = 5 * time.Minute
	koeQualificationTotalTimeout                 = 3 * time.Hour
	koeQualificationResolverTimeout              = 5 * time.Second
	koeQualificationParallelBarrierTimeout       = 2 * time.Second
	koeQualificationProgressInterval             = 10

	koeQualificationFastLane            = "luna_fast"
	koeQualificationSonnetReferenceLane = "sonnet_reference"

	koeQualificationLunaProvider    = "openai"
	koeQualificationLunaModel       = "gpt-5.6-luna"
	koeQualificationLunaAPISurface  = "openai_responses"
	koeQualificationLunaEffort      = "medium"
	koeQualificationLunaServiceTier = "fast"
	koeQualificationSonnetProvider  = "anthropic"
	koeQualificationSonnetModel     = "claude-sonnet-5"
)

var koeQualificationWorkloadNames = []string{
	"no_tool",
	"one_tool",
	"serial_3",
	"parallel",
	"deferred_tool",
	"explicit_use_skill_activation",
	"validation_repair",
}

type koeQualificationRuntimeConfig struct {
	endpoint    string
	apiKey      string
	outputPath  string
	repetitions int
	seed        int64
	smoke       bool
	maxCostUSD  float64
	pause       time.Duration
}

type koeQualificationJob struct {
	Lane       string
	Workload   string
	Repetition int
	Token      string
}

type koeQualificationReport struct {
	SchemaVersion               int                           `json:"schema_version"`
	GeneratedAt                 string                        `json:"generated_at"`
	Complete                    bool                          `json:"complete"`
	Completed                   int                           `json:"completed"`
	Scheduled                   int                           `json:"scheduled"`
	Seed                        int64                         `json:"seed"`
	Repetitions                 int                           `json:"repetitions_per_cell"`
	Smoke                       bool                          `json:"smoke"`
	SampleQualifying            bool                          `json:"sample_qualifying"`
	GatePassed                  *bool                         `json:"gate_passed"`
	CorrectnessGatePassed       *bool                         `json:"correctness_gate_passed"`
	PerformanceGatePassed       *bool                         `json:"performance_gate_passed"`
	BehaviorFailureClasses      []string                      `json:"behavior_failure_classes"`
	PerformanceFailureClasses   []string                      `json:"performance_failure_classes"`
	ContractFailureCount        int                           `json:"contract_failure_count"`
	RuntimeFailureCount         int                           `json:"runtime_failure_count"`
	DuplicateSideEffectFailures int                           `json:"duplicate_side_effect_failure_count"`
	CostFailureCount            int                           `json:"cost_failure_count"`
	MaxCostUSD                  float64                       `json:"max_cost_usd"`
	ReportedCostUSD             float64                       `json:"reported_cost_usd"`
	CostObserved                bool                          `json:"cost_observed"`
	PerformanceNote             string                        `json:"performance_note"`
	Performance                 *koeQualificationPerformance  `json:"performance"`
	Randomized                  bool                          `json:"randomized"`
	Concurrency                 int                           `json:"concurrency"`
	InterRunPauseMillis         int64                         `json:"inter_run_pause_millis"`
	MaxTokens                   int                           `json:"max_tokens"`
	SelectorExercised           bool                          `json:"selector_exercised"`
	ProductionSelectorExercised bool                          `json:"production_selector_exercised"`
	ResolutionPolicyExercised   bool                          `json:"resolution_policy_exercised"`
	AgentLoopOnly               bool                          `json:"agent_loop_only"`
	SemanticDiscoveryEnabled    bool                          `json:"semantic_skill_discovery_enabled"`
	ControlledLane              string                        `json:"controlled_lane"`
	Lanes                       []koeQualificationLaneReport  `json:"lanes"`
	Workloads                   []string                      `json:"workloads"`
	Runs                        []koeQualificationRunReport   `json:"runs"`
	Summary                     []koeQualificationCellSummary `json:"summary"`
}

type koeQualificationPerformance struct {
	LunaTotalP50Millis          int64    `json:"luna_total_p50_millis"`
	SonnetTotalP50Millis        int64    `json:"sonnet_total_p50_millis"`
	LunaTotalP95Millis          int64    `json:"luna_total_p95_millis"`
	SonnetTotalP95Millis        int64    `json:"sonnet_total_p95_millis"`
	LunaP95FasterWorkloads      int      `json:"luna_p95_faster_workloads"`
	ComparedWorkloads           int      `json:"compared_workloads"`
	LunaCostPerSuccessfulTask   *float64 `json:"luna_cost_per_successful_task,omitempty"`
	SonnetCostPerSuccessfulTask *float64 `json:"sonnet_cost_per_successful_task,omitempty"`
}

type koeQualificationLaneReport struct {
	Name                      string `json:"name"`
	Mode                      string `json:"mode"`
	ExpectedProvider          string `json:"expected_provider"`
	ExpectedModel             string `json:"expected_model"`
	ExpectedAPISurface        string `json:"expected_api_surface,omitempty"`
	ExpectedReasoningEffort   string `json:"expected_reasoning_effort"`
	ExpectedServiceTier       string `json:"expected_service_tier,omitempty"`
	ExpectedThinking          string `json:"expected_thinking"`
	ExpectedParallelToolCalls bool   `json:"expected_parallel_tool_calls"`
	ResponseCachePolicy       string `json:"response_cache_policy"`
	ResolverIncluded          bool   `json:"resolver_included"`
	ResolverObservedProvider  string `json:"resolver_observed_provider,omitempty"`
	ResolverObservedModel     string `json:"resolver_observed_model,omitempty"`
	ResolverObservedAPI       string `json:"resolver_observed_api_surface,omitempty"`
	ResolverObservedReasoning string `json:"resolver_observed_reasoning_effort,omitempty"`
	ResolverObservedTier      string `json:"resolver_observed_service_tier,omitempty"`
	RequestRoutingAuthority   string `json:"request_routing_authority"`
}

type koeQualificationRunReport struct {
	ScheduleIndex                 int      `json:"schedule_index"`
	Lane                          string   `json:"lane"`
	Workload                      string   `json:"workload"`
	Repetition                    int      `json:"repetition"`
	Outcome                       string   `json:"outcome"`
	TaskSuccess                   bool     `json:"task_success"`
	ToolCorrectness               bool     `json:"tool_correctness"`
	RouteExact                    bool     `json:"route_exact"`
	ProviderExact                 bool     `json:"provider_exact"`
	ModelExact                    bool     `json:"model_exact"`
	CachePolicyExact              bool     `json:"cache_policy_exact"`
	ResolverIncluded              bool     `json:"resolver_included"`
	ResolverProfileExact          *bool    `json:"resolver_profile_exact,omitempty"`
	ResolverMillis                *int64   `json:"resolver_millis,omitempty"`
	ObservedProviders             []string `json:"observed_provider_classes,omitempty"`
	ObservedModels                []string `json:"observed_model_classes,omitempty"`
	CompletionCalls               int      `json:"completion_calls"`
	ToolIterations                int      `json:"tool_iterations"`
	ToolCalls                     int      `json:"tool_calls"`
	DuplicateModelCalls           int      `json:"duplicate_model_calls"`
	DuplicateToolExecutions       int      `json:"duplicate_tool_executions"`
	SideEffectExecutions          int      `json:"side_effect_executions"`
	DuplicateSideEffectExecutions int      `json:"duplicate_side_effect_executions"`
	ParallelMaxInFlight           int      `json:"parallel_max_in_flight,omitempty"`
	FirstSemanticDeltaMillis      *int64   `json:"first_semantic_delta_millis,omitempty"`
	FirstTextDeltaMillis          *int64   `json:"first_text_delta_millis,omitempty"`
	FirstToolCallReadyMillis      *int64   `json:"first_tool_call_ready_millis,omitempty"`
	TotalMillis                   int64    `json:"total_millis"`
	GatewayLatencyMs              int      `json:"gateway_latency_ms"`
	InputTokens                   int      `json:"input_tokens"`
	OutputTokens                  int      `json:"output_tokens"`
	TotalTokens                   int      `json:"total_tokens"`
	CacheReadTokens               int      `json:"cache_read_tokens"`
	CacheCreationTokens           int      `json:"cache_creation_tokens"`
	CostUSD                       float64  `json:"cost_usd"`
	CostObserved                  bool     `json:"cost_observed"`
	ResponseCacheHits             int      `json:"response_cache_hits"`
	RetryCount                    int      `json:"retry_count"`
	RetryableClientErrors         int      `json:"retryable_client_errors"`
	HTTPStatuses                  []int    `json:"http_error_statuses,omitempty"`
	TransportErrors               int      `json:"transport_errors,omitempty"`
	RuntimeErrorClass             string   `json:"runtime_error_class,omitempty"`
}

type koeQualificationCellSummary struct {
	Lane                          string   `json:"lane"`
	Workload                      string   `json:"workload"`
	Runs                          int      `json:"runs"`
	SuccessfulTasks               int      `json:"successful_tasks"`
	TaskSuccessRate               float64  `json:"task_success_rate"`
	ToolCorrectnessRate           float64  `json:"tool_correctness_rate"`
	RouteExactRate                float64  `json:"route_exact_rate"`
	ProviderExactRate             float64  `json:"provider_exact_rate"`
	ModelExactRate                float64  `json:"model_exact_rate"`
	CachePolicyExactRate          float64  `json:"cache_policy_exact_rate"`
	ResolverProfileExactRate      *float64 `json:"resolver_profile_exact_rate,omitempty"`
	HTTPErrorRuns                 int      `json:"http_error_runs"`
	TransportErrorRuns            int      `json:"transport_error_runs"`
	DuplicateModelCalls           int      `json:"duplicate_model_calls"`
	DuplicateToolExecutions       int      `json:"duplicate_tool_executions"`
	SideEffectExecutions          int      `json:"side_effect_executions"`
	DuplicateSideEffectExecutions int      `json:"duplicate_side_effect_executions"`
	CompletionCallsMean           float64  `json:"completion_calls_mean"`
	ToolIterationsMean            float64  `json:"tool_iterations_mean"`
	RetryCount                    int      `json:"retry_count"`
	RetryRunRate                  float64  `json:"retry_run_rate"`
	RetryableClientErrors         int      `json:"retryable_client_errors"`
	RetriesPerCompletionCall      float64  `json:"retries_per_completion_call"`
	FirstSemanticDeltaP50Millis   *int64   `json:"first_semantic_delta_p50_millis,omitempty"`
	FirstSemanticDeltaP95Millis   *int64   `json:"first_semantic_delta_p95_millis,omitempty"`
	FirstSemanticDeltaSampleCount int      `json:"first_semantic_delta_sample_count"`
	FirstSemanticDeltaCoverage    float64  `json:"first_semantic_delta_coverage_rate"`
	FirstTextDeltaP50Millis       *int64   `json:"first_text_delta_p50_millis,omitempty"`
	FirstTextDeltaP95Millis       *int64   `json:"first_text_delta_p95_millis,omitempty"`
	FirstTextDeltaSampleCount     int      `json:"first_text_delta_sample_count"`
	FirstTextDeltaCoverage        float64  `json:"first_text_delta_coverage_rate"`
	FirstToolCallReadyP50Millis   *int64   `json:"first_tool_call_ready_p50_millis,omitempty"`
	FirstToolCallReadyP95Millis   *int64   `json:"first_tool_call_ready_p95_millis,omitempty"`
	FirstToolCallReadySampleCount int      `json:"first_tool_call_ready_sample_count"`
	FirstToolCallReadyCoverage    float64  `json:"first_tool_call_ready_coverage_rate"`
	TotalP50Millis                int64    `json:"total_p50_millis"`
	TotalP95Millis                int64    `json:"total_p95_millis"`
	InputTokensMean               float64  `json:"input_tokens_mean"`
	OutputTokensMean              float64  `json:"output_tokens_mean"`
	TotalTokensMean               float64  `json:"total_tokens_mean"`
	CacheReadTokensMean           float64  `json:"cache_read_tokens_mean"`
	CacheCreationTokensMean       float64  `json:"cache_creation_tokens_mean"`
	CostUSDTotal                  float64  `json:"cost_usd_total"`
	CostUSDMean                   float64  `json:"cost_usd_mean"`
	CostUSDPerSuccessfulTask      *float64 `json:"cost_usd_per_successful_task,omitempty"`
}

type koeQualificationClientError struct {
	HTTPStatus int
	Transport  bool
	Retryable  bool
}

// koeQualificationLLMClient records only in-memory assertions and content-free
// timing/error metrics. Every real-client error is sanitized before it crosses
// into AgentLoop, whose production retry logs write error strings to stderr.
type koeQualificationLLMClient struct {
	inner client.LLMClient
	// start is the whole job start, so Luna callback latency includes its
	// production resolver call while Full correctly has no resolver overhead.
	start time.Time

	mu                 sync.Mutex
	requests           []client.CompletionRequest
	responses          []client.CompletionResponse
	errors             []koeQualificationClientError
	firstSemanticDelta *time.Duration
	firstTextDelta     *time.Duration
	firstToolCallReady *time.Duration
}

func (c *koeQualificationLLMClient) Complete(
	ctx context.Context,
	req client.CompletionRequest,
) (*client.CompletionResponse, error) {
	c.recordRequest(req)
	response, err := c.inner.Complete(ctx, req)
	c.recordResponse(response)
	return response, c.recordAndSanitizeError(err)
}

func (c *koeQualificationLLMClient) CompleteStream(
	ctx context.Context,
	req client.CompletionRequest,
	onDelta func(client.StreamDelta),
) (*client.CompletionResponse, error) {
	c.recordRequest(req)
	response, err := c.inner.CompleteStream(ctx, req, func(delta client.StreamDelta) {
		c.recordStreamDelta(delta)
		onDelta(delta)
	})
	c.recordResponse(response)
	return response, c.recordAndSanitizeError(err)
}

func (c *koeQualificationLLMClient) recordAndSanitizeError(err error) error {
	if err == nil {
		return nil
	}
	item := classifyKoeQualificationClientError(err)
	c.mu.Lock()
	c.errors = append(c.errors, item)
	c.mu.Unlock()
	return sanitizeKoeQualificationClientError(err)
}

func classifyKoeQualificationClientError(err error) koeQualificationClientError {
	item := koeQualificationClientError{
		Transport: client.TransportErrorShape(err),
		Retryable: koeQualificationRetryableError(err),
	}
	var apiErr *client.APIError
	if errors.As(err, &apiErr) {
		item.HTTPStatus = apiErr.StatusCode
		item.Transport = false
	}
	return item
}

func (c *koeQualificationLLMClient) recordRequest(req client.CompletionRequest) {
	c.mu.Lock()
	c.requests = append(c.requests, req)
	c.mu.Unlock()
}

func (c *koeQualificationLLMClient) recordResponse(response *client.CompletionResponse) {
	if response == nil {
		return
	}
	c.mu.Lock()
	c.responses = append(c.responses, *response)
	c.mu.Unlock()
}

func (c *koeQualificationLLMClient) recordStreamDelta(delta client.StreamDelta) {
	if delta.Text == "" && delta.ToolCall == nil {
		return
	}
	elapsed := time.Since(c.start)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.firstSemanticDelta == nil {
		value := elapsed
		c.firstSemanticDelta = &value
	}
	if delta.Text != "" && c.firstTextDelta == nil {
		value := elapsed
		c.firstTextDelta = &value
	}
	if delta.ToolCall != nil && c.firstToolCallReady == nil {
		value := elapsed
		c.firstToolCallReady = &value
	}
}

func (c *koeQualificationLLMClient) snapshot() (
	[]client.CompletionRequest,
	[]client.CompletionResponse,
	[]koeQualificationClientError,
	*time.Duration,
	*time.Duration,
	*time.Duration,
) {
	c.mu.Lock()
	defer c.mu.Unlock()
	requests := append([]client.CompletionRequest(nil), c.requests...)
	responses := append([]client.CompletionResponse(nil), c.responses...)
	clientErrors := append([]koeQualificationClientError(nil), c.errors...)
	return requests,
		responses,
		clientErrors,
		cloneDuration(c.firstSemanticDelta),
		cloneDuration(c.firstTextDelta),
		cloneDuration(c.firstToolCallReady)
}

func cloneDuration(value *time.Duration) *time.Duration {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func koeQualificationRetryableError(err error) bool {
	if err == nil || errors.Is(err, client.ErrStreamIdleTimeout) {
		return false
	}
	var apiErr *client.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.StatusCode {
		case 429, 500, 502, 503, 504, 529:
			return true
		default:
			return false
		}
	}
	return client.TransportErrorShape(err)
}

func sanitizeKoeQualificationClientError(err error) error {
	if err == nil {
		return nil
	}
	var apiErr *client.APIError
	if errors.As(err, &apiErr) {
		body := ""
		lowerBody := strings.ToLower(apiErr.Body)
		if apiErr.StatusCode == 400 &&
			(strings.Contains(lowerBody, "prompt is too long") ||
				strings.Contains(lowerBody, "context_length_exceeded")) {
			body = "context_length_exceeded"
		}
		return &client.APIError{
			StatusCode: apiErr.StatusCode,
			Body:       body,
		}
	}
	if errors.Is(err, client.ErrStreamIdleTimeout) {
		return client.ErrStreamIdleTimeout
	}
	transport := client.TransportErrorShape(err)
	transportMarker := sanitizedKoeQualificationTransportMarker(err)
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		if transport {
			return fmt.Errorf(
				"%s qualification transport deadline: %w",
				transportMarker,
				context.DeadlineExceeded,
			)
		}
		return context.DeadlineExceeded
	case errors.Is(err, context.Canceled):
		if transport {
			return fmt.Errorf(
				"%s qualification transport canceled: %w",
				transportMarker,
				context.Canceled,
			)
		}
		return context.Canceled
	case transport:
		return fmt.Errorf("%s qualification transport error", transportMarker)
	default:
		return errors.New("qualification client error")
	}
}

// sanitizedKoeQualificationTransportMarker preserves only the content-free
// transport shape needed to diagnose the live harness. It deliberately drops
// the original error body, prompt, tool arguments, endpoint, and credential.
func sanitizedKoeQualificationTransportMarker(err error) string {
	if err == nil {
		return "request failed:"
	}
	message := err.Error()
	for _, marker := range []string{
		"stream ended without done event",
		"stream read error:",
		"decode response:",
		"request failed:",
	} {
		if strings.Contains(message, marker) {
			return marker
		}
	}
	return "request failed:"
}

// koeQualificationEventHandler intentionally discards all content-bearing
// callback values. Retry notifications are counted without retaining messages;
// tool_search is recorded after its real production Tool.Run returns.
type koeQualificationEventHandler struct {
	state *koeQualificationToolState

	mu         sync.Mutex
	retryCount int
}

func (h *koeQualificationEventHandler) OnToolCall(string, string, string) {}
func (h *koeQualificationEventHandler) OnToolResult(
	name string,
	args string,
	_ string,
	_ ToolResult,
	elapsed time.Duration,
) {
	if name == "tool_search" && elapsed > 0 && h.state != nil {
		h.state.recordExecution(name, args)
	}
}
func (h *koeQualificationEventHandler) OnText(string)                    {}
func (h *koeQualificationEventHandler) OnPreamble(string)                {}
func (h *koeQualificationEventHandler) OnStreamDelta(string)             {}
func (h *koeQualificationEventHandler) OnUsage(TurnUsage)                {}
func (h *koeQualificationEventHandler) OnCloudProgress(int, int)         {}
func (h *koeQualificationEventHandler) OnCloudPlan(string, string, bool) {}
func (h *koeQualificationEventHandler) OnApprovalNeeded(string, string) bool {
	return true
}
func (h *koeQualificationEventHandler) OnCloudAgent(_, status, _ string) {
	if status != "retry" {
		return
	}
	h.mu.Lock()
	h.retryCount++
	h.mu.Unlock()
}

func (h *koeQualificationEventHandler) RetryCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.retryCount
}

type koeQualificationToolObservation struct {
	Name   string
	Text   string
	Number int
	Valid  bool
}

type koeQualificationToolState struct {
	mu sync.Mutex

	observations                  []koeQualificationToolObservation
	executions                    map[string]int
	effects                       map[string]int
	duplicateToolExecutions       int
	sideEffectExecutions          int
	duplicateSideEffectExecutions int
	serialNext                    int
	parallelInFlight              int
	parallelMaxInFlight           int
	parallelReady                 chan struct{}
	parallelReadyOnce             sync.Once
}

func newKoeQualificationToolState() *koeQualificationToolState {
	return &koeQualificationToolState{
		executions:    make(map[string]int),
		effects:       make(map[string]int),
		serialNext:    1,
		parallelReady: make(chan struct{}),
	}
}

func (s *koeQualificationToolState) record(obs koeQualificationToolObservation) {
	s.mu.Lock()
	s.observations = append(s.observations, obs)
	s.mu.Unlock()
}

func (s *koeQualificationToolState) recordExecution(
	name string,
	args string,
) {
	key := name + "\x00" + koeQualificationCanonicalJSON(args)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.executions[key]++
	if s.executions[key] > 1 {
		s.duplicateToolExecutions++
	}
}

// recordEffect is called only after strict schema/value validation and the
// controlled in-memory effect is actually applied.
func (s *koeQualificationToolState) recordEffect(
	name string,
	args string,
) {
	key := name + "\x00" + koeQualificationCanonicalJSON(args)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.effects[key]++
	s.sideEffectExecutions++
	if s.effects[key] > 1 {
		s.duplicateSideEffectExecutions++
	}
}

func (s *koeQualificationToolState) runSerial(
	name string,
	step int,
) (koeQualificationToolObservation, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	valid := step == s.serialNext
	obs := koeQualificationToolObservation{
		Name:   name,
		Number: step,
		Valid:  valid,
	}
	s.observations = append(s.observations, obs)
	if valid {
		s.serialNext++
	}
	return obs, valid
}

func (s *koeQualificationToolState) awaitParallel(
	ctx context.Context,
) bool {
	s.mu.Lock()
	s.parallelInFlight++
	if s.parallelInFlight > s.parallelMaxInFlight {
		s.parallelMaxInFlight = s.parallelInFlight
	}
	if s.parallelInFlight >= 2 {
		s.parallelReadyOnce.Do(func() {
			close(s.parallelReady)
		})
	}
	ready := s.parallelReady
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		s.parallelInFlight--
		s.mu.Unlock()
	}()
	timer := time.NewTimer(koeQualificationParallelBarrierTimeout)
	defer timer.Stop()
	select {
	case <-ready:
		return true
	case <-ctx.Done():
		return false
	case <-timer.C:
		return false
	}
}

func (s *koeQualificationToolState) snapshot() (
	[]koeQualificationToolObservation,
	int,
	int,
	int,
	int,
) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]koeQualificationToolObservation, len(s.observations))
	copy(out, s.observations)
	return out,
		s.parallelMaxInFlight,
		s.duplicateToolExecutions,
		s.sideEffectExecutions,
		s.duplicateSideEffectExecutions
}

type koeQualificationTool struct {
	name            string
	description     string
	kind            string
	argName         string
	expectedText    string
	result          string
	state           *koeQualificationToolState
	source          ToolSource
	exposure        ToolExposure
	namespace       string
	concurrent      bool
	parallelBarrier bool
	sideEffect      bool
}

func (t *koeQualificationTool) Info() ToolInfo {
	property := map[string]any{
		"type":        "string",
		"description": "Exact qualification value requested by the user.",
	}
	if t.kind == "serial" {
		property = map[string]any{
			"type":        "integer",
			"minimum":     1,
			"maximum":     3,
			"description": "The next serial step, from 1 through 3.",
		}
	} else if t.kind == "parallel" {
		property["enum"] = []string{"left", "right"}
	}
	return ToolInfo{
		Name:        t.name,
		Description: t.description,
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				t.argName: property,
			},
			"required":             []string{t.argName},
			"additionalProperties": false,
		},
		Required: []string{t.argName},
	}
}

func (t *koeQualificationTool) Run(
	ctx context.Context,
	args string,
) (ToolResult, error) {
	t.state.recordExecution(t.name, args)
	if result, ok := ValidateToolArguments(t.Info(), args); !ok {
		return result, nil
	}

	if t.kind == "serial" {
		var parsed struct {
			Step int `json:"step"`
		}
		if err := json.Unmarshal([]byte(args), &parsed); err != nil {
			return ValidationError(t.name + ": invalid step"), nil
		}
		_, valid := t.state.runSerial(t.name, parsed.Step)
		if !valid {
			return ValidationError(t.name + ": step is out of order"), nil
		}
		return ToolResult{
			Content: fmt.Sprintf("%s_STEP_%d_OK", t.result, parsed.Step),
		}, nil
	}

	var parsed map[string]any
	if err := json.Unmarshal([]byte(args), &parsed); err != nil {
		return ValidationError(t.name + ": invalid arguments"), nil
	}
	value, _ := parsed[t.argName].(string)
	valid := value == t.expectedText
	if t.kind == "parallel" {
		valid = value == "left" || value == "right"
	} else if t.kind == "skill_action" {
		valid = valid && koeQualificationSkillActivated(ctx)
	}
	t.state.record(koeQualificationToolObservation{
		Name:  t.name,
		Text:  value,
		Valid: valid,
	})
	if !valid {
		return ValidationError(t.name + ": qualification value mismatch"), nil
	}
	if t.sideEffect {
		t.state.recordEffect(t.name, args)
	}

	if t.parallelBarrier {
		if !t.state.awaitParallel(ctx) {
			return TransientError("qualification parallel barrier not reached"), nil
		}
	}

	return ToolResult{Content: t.result}, nil
}

func (t *koeQualificationTool) RequiresApproval() bool { return false }
func (t *koeQualificationTool) ToolSource() ToolSource { return t.source }
func (t *koeQualificationTool) ToolExposure() ToolExposure {
	return t.exposure
}
func (t *koeQualificationTool) ToolSearchNamespace() string {
	return t.namespace
}
func (t *koeQualificationTool) IsReadOnlyCall(string) bool {
	return !t.sideEffect
}
func (t *koeQualificationTool) IsConcurrencySafeCall(string) bool {
	return t.concurrent
}

func koeQualificationSkillActivated(ctx context.Context) bool {
	set := skills.ActivatedFromContext(ctx)
	if set == nil {
		return false
	}
	for _, name := range set.Names() {
		if name == "qualification-skill" {
			return true
		}
	}
	return false
}

// koeQualificationUseSkillTool instruments the real production use_skill
// implementation without replacing any activation, filtering, or validation
// behavior.
type koeQualificationUseSkillTool struct {
	inner Tool
	state *koeQualificationToolState
}

func (t *koeQualificationUseSkillTool) Info() ToolInfo {
	return t.inner.Info()
}

func (t *koeQualificationUseSkillTool) Run(
	ctx context.Context,
	args string,
) (ToolResult, error) {
	t.state.recordExecution("use_skill", args)
	result, err := t.inner.Run(ctx, args)
	var parsed struct {
		SkillName string `json:"skill_name"`
	}
	_ = json.Unmarshal([]byte(args), &parsed)
	t.state.record(koeQualificationToolObservation{
		Name: "use_skill",
		Text: parsed.SkillName,
		Valid: err == nil && !result.IsError &&
			parsed.SkillName == "qualification-skill",
	})
	return result, err
}

func (t *koeQualificationUseSkillTool) RequiresApproval() bool {
	return t.inner.RequiresApproval()
}
func (t *koeQualificationUseSkillTool) IsReadOnlyCall(args string) bool {
	checker, ok := t.inner.(ReadOnlyChecker)
	return ok && checker.IsReadOnlyCall(args)
}
func (t *koeQualificationUseSkillTool) SkillExempt() bool {
	return IsSkillExempt(t.inner)
}

type koeQualificationWorkload struct {
	name     string
	prompt   string
	receipt  string
	registry *ToolRegistry
	skills   []*skills.Skill
	state    *koeQualificationToolState
}

func TestKoeQualificationClientErrorSanitization(t *testing.T) {
	tests := []struct {
		name            string
		err             error
		expectedAPIBody string
	}{
		{
			name: "api status",
			err: &client.APIError{
				StatusCode: 503,
				Body:       "secret prompt and tool arguments",
			},
		},
		{
			name: "context length prompt marker",
			err: &client.APIError{
				StatusCode: 400,
				Body:       "secret: prompt is too long for this model",
			},
			expectedAPIBody: "context_length_exceeded",
		},
		{
			name: "context length code marker",
			err: &client.APIError{
				StatusCode: 400,
				Body:       `{"code":"context_length_exceeded","secret":"x"}`,
			},
			expectedAPIBody: "context_length_exceeded",
		},
		{
			name: "other bad request body",
			err: &client.APIError{
				StatusCode: 400,
				Body:       "secret invalid tool arguments",
			},
		},
		{
			name: "transport",
			err:  errors.New("request failed: secret prompt and tool arguments"),
		},
		{
			name: "transport deadline",
			err: fmt.Errorf(
				"request failed: secret prompt: %w",
				context.DeadlineExceeded,
			),
		},
		{name: "deadline", err: context.DeadlineExceeded},
		{name: "canceled", err: context.Canceled},
		{name: "stream idle", err: client.ErrStreamIdleTimeout},
		{
			name: "unknown",
			err:  errors.New("secret prompt and tool arguments"),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			sanitized := sanitizeKoeQualificationClientError(test.err)
			if sanitized == nil {
				t.Fatal("sanitized error is nil")
			}
			if strings.Contains(sanitized.Error(), "secret") ||
				strings.Contains(sanitized.Error(), "prompt") ||
				strings.Contains(sanitized.Error(), "arguments") {
				t.Fatalf("sanitized error retained content: %q", sanitized.Error())
			}
			if got, want := client.TransportErrorShape(sanitized),
				client.TransportErrorShape(test.err); got != want {
				t.Fatalf("transport shape = %t, want %t", got, want)
			}
			if got, want := koeQualificationRetryableError(sanitized),
				koeQualificationRetryableError(test.err); got != want {
				t.Fatalf("retryable = %t, want %t", got, want)
			}
			if errors.Is(test.err, context.DeadlineExceeded) &&
				!errors.Is(sanitized, context.DeadlineExceeded) {
				t.Fatal("deadline identity was not preserved")
			}
			if errors.Is(test.err, context.Canceled) &&
				!errors.Is(sanitized, context.Canceled) {
				t.Fatal("cancellation identity was not preserved")
			}
			if errors.Is(test.err, client.ErrStreamIdleTimeout) &&
				!errors.Is(sanitized, client.ErrStreamIdleTimeout) {
				t.Fatal("stream idle identity was not preserved")
			}
			var originalAPI, sanitizedAPI *client.APIError
			if errors.As(test.err, &originalAPI) {
				if !errors.As(sanitized, &sanitizedAPI) {
					t.Fatal("APIError type was not preserved")
				}
				if sanitizedAPI.StatusCode != originalAPI.StatusCode {
					t.Fatalf(
						"API status = %d, want %d",
						sanitizedAPI.StatusCode,
						originalAPI.StatusCode,
					)
				}
				if sanitizedAPI.Body != test.expectedAPIBody {
					t.Fatalf(
						"API body = %q, want %q",
						sanitizedAPI.Body,
						test.expectedAPIBody,
					)
				}
			}
		})
	}
}

func TestKoeQualificationRuntimeErrorClassificationPreservesAgentSentinels(
	t *testing.T,
) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "empty final response",
			err:  fmt.Errorf("wrapped: %w", ErrEmptyFinalResponse),
			want: "empty_final_response",
		},
		{
			name: "max iterations",
			err:  fmt.Errorf("wrapped: %w", ErrMaxIterReached),
			want: "max_iterations",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			class, status := koeQualificationRuntimeErrorClass(test.err)
			if class != test.want || status != 0 {
				t.Fatalf(
					"class=%q status=%d, want %q/0",
					class,
					status,
					test.want,
				)
			}
		})
	}
}

func TestKoeQualificationStreamMetricsComeFromClientCallback(t *testing.T) {
	recorder := &koeQualificationLLMClient{
		start: time.Now().Add(-time.Second),
	}
	recorder.recordStreamDelta(client.StreamDelta{})
	_, _, _, semantic, textDelta, toolReady := recorder.snapshot()
	if semantic != nil || textDelta != nil || toolReady != nil {
		t.Fatal("empty callback changed stream metrics")
	}
	recorder.recordStreamDelta(client.StreamDelta{
		ToolCall: &client.FunctionCall{},
	})
	_, _, _, semantic, textDelta, toolReady = recorder.snapshot()
	if semantic == nil || toolReady == nil || textDelta != nil {
		t.Fatal("tool-ready callback metrics are not separated")
	}
	recorder.recordStreamDelta(client.StreamDelta{Text: "x"})
	_, _, _, _, textDelta, _ = recorder.snapshot()
	if textDelta == nil {
		t.Fatal("text callback did not record first_text_delta")
	}
}

func TestKoeQualificationObservedRouteIsAllowlisted(t *testing.T) {
	exact, classes := koeQualificationObservedExact(
		[]client.CompletionResponse{{Provider: "server-private-value"}},
		koeQualificationLunaProvider,
		func(response client.CompletionResponse) string {
			return response.Provider
		},
	)
	if exact || len(classes) != 1 || classes[0] != "unexpected" {
		t.Fatalf("exact=%t classes=%v, want false/[unexpected]", exact, classes)
	}
}

func TestKoeQualificationExplicitUseSkillUsesProductionTool(t *testing.T) {
	job := koeQualificationJob{
		Workload: "explicit_use_skill_activation",
		Token:    "ABC123",
	}
	workload := buildKoeQualificationWorkload(job)
	useSkill, ok := workload.registry.Get("use_skill")
	if !ok {
		t.Fatal("use_skill is not registered")
	}
	instrumented, ok := useSkill.(*koeQualificationUseSkillTool)
	if !ok {
		t.Fatalf("use_skill type = %T", useSkill)
	}
	if _, ok := instrumented.inner.(*useSkillTool); !ok {
		t.Fatalf("inner use_skill type = %T, want production *useSkillTool", instrumented.inner)
	}
	action, ok := workload.registry.Get("qualification_skill_action")
	if !ok {
		t.Fatal("qualification_skill_action is not registered")
	}
	ctx := skills.WithActivatedSet(
		context.Background(),
		skills.NewActivatedSet(),
	)
	result, err := useSkill.Run(ctx, `{"skill_name":"qualification-skill"}`)
	if err != nil || result.IsError {
		t.Fatalf("production use_skill failed: err=%v is_error=%t", err, result.IsError)
	}
	if len(result.SkillToolFilter) != 1 ||
		result.SkillToolFilter[0] != "qualification_skill_action" ||
		!strings.Contains(result.SkillToolHint, "qualification_skill_action") {
		t.Fatal("production use_skill filter/hint contract was not preserved")
	}
	result, err = action.Run(ctx, `{"token":"ABC123"}`)
	if err != nil || result.IsError || result.Content != workload.receipt {
		t.Fatalf(
			"activated action failed: err=%v is_error=%t content_matches=%t",
			err,
			result.IsError,
			result.Content == workload.receipt,
		)
	}

	unactivated := buildKoeQualificationWorkload(job)
	action, _ = unactivated.registry.Get("qualification_skill_action")
	result, err = action.Run(context.Background(), `{"token":"ABC123"}`)
	if err != nil || !result.IsError {
		t.Fatalf(
			"unactivated action err=%v is_error=%t, want validation error",
			err,
			result.IsError,
		)
	}
}

func TestKoeQualificationSideEffectsRequireSuccessfulToolRun(t *testing.T) {
	workload := buildKoeQualificationWorkload(koeQualificationJob{
		Workload: "one_tool",
		Token:    "ABC123",
	})
	tool, ok := workload.registry.Get("qualification_echo")
	if !ok {
		t.Fatal("qualification_echo is not registered")
	}

	result, err := tool.Run(context.Background(), `{}`)
	if err != nil || !result.IsError {
		t.Fatalf("invalid run err=%v is_error=%t, want validation error", err, result.IsError)
	}
	_, _, duplicateTools, effects, duplicateEffects :=
		workload.state.snapshot()
	if duplicateTools != 0 || effects != 0 || duplicateEffects != 0 {
		t.Fatalf(
			"invalid run duplicate_tools=%d effects=%d duplicate_effects=%d, want zero",
			duplicateTools,
			effects,
			duplicateEffects,
		)
	}

	for run := 1; run <= 2; run++ {
		result, err = tool.Run(
			context.Background(),
			`{"token":"ABC123"}`,
		)
		if err != nil || result.IsError {
			t.Fatalf(
				"valid run %d err=%v is_error=%t",
				run,
				err,
				result.IsError,
			)
		}
	}
	_, _, duplicateTools, effects, duplicateEffects =
		workload.state.snapshot()
	if duplicateTools != 1 || effects != 2 || duplicateEffects != 1 {
		t.Fatalf(
			"valid runs duplicate_tools=%d effects=%d duplicate_effects=%d, want 1/2/1",
			duplicateTools,
			effects,
			duplicateEffects,
		)
	}
}

func TestKoeQualificationToolSearchExecutionRequiresPositiveElapsed(t *testing.T) {
	state := newKoeQualificationToolState()
	handler := &koeQualificationEventHandler{state: state}
	const args = `{"query":"select:qualification_deferred_lookup"}`
	handler.OnToolResult("tool_search", args, "", ToolResult{}, 0)
	handler.OnToolResult("tool_search", args, "", ToolResult{}, time.Millisecond)
	_, _, duplicateTools, _, _ := state.snapshot()
	if duplicateTools != 0 {
		t.Fatalf("one real execution produced %d duplicates", duplicateTools)
	}
	handler.OnToolResult("tool_search", args, "", ToolResult{}, time.Millisecond)
	_, _, duplicateTools, _, _ = state.snapshot()
	if duplicateTools != 1 {
		t.Fatalf("two real executions produced %d duplicates, want 1", duplicateTools)
	}
}

func TestKoeQualificationUnifiedGateSmokeAndFormal(t *testing.T) {
	t.Run("smoke is strict and has no performance gate", func(t *testing.T) {
		cfg := koeQualificationRuntimeConfig{
			repetitions: 1,
			seed:        koeQualificationDefaultSeed,
			smoke:       true,
			maxCostUSD:  koeQualificationDefaultMaxCostUSD,
		}
		jobs := buildKoeQualificationJobs(cfg)
		results := syntheticKoeQualificationResults(1, 70, 100)
		report := newKoeQualificationReport(cfg, jobs, results, true)
		if report.SampleQualifying ||
			report.GatePassed == nil || !*report.GatePassed ||
			report.CorrectnessGatePassed == nil ||
			!*report.CorrectnessGatePassed ||
			report.PerformanceGatePassed != nil ||
			report.Performance != nil {
			t.Fatalf(
				"unexpected passing smoke gate: sample=%t gate=%v correctness=%v performance=%v",
				report.SampleQualifying,
				report.GatePassed,
				report.CorrectnessGatePassed,
				report.PerformanceGatePassed,
			)
		}

		results[0].TaskSuccess = false
		report = newKoeQualificationReport(cfg, jobs, results, true)
		if report.GatePassed == nil || *report.GatePassed ||
			len(report.BehaviorFailureClasses) == 0 {
			t.Fatal("one failed smoke run passed the unified correctness gate")
		}
	})

	t.Run("formal sample enforces performance and cost", func(t *testing.T) {
		cfg := koeQualificationRuntimeConfig{
			repetitions: koeQualificationDefaultRepetitions,
			seed:        koeQualificationDefaultSeed,
			maxCostUSD:  koeQualificationDefaultMaxCostUSD,
		}
		jobs := buildKoeQualificationJobs(cfg)
		results := syntheticKoeQualificationResults(
			koeQualificationDefaultRepetitions,
			70,
			100,
		)
		report := newKoeQualificationReport(cfg, jobs, results, true)
		if !report.SampleQualifying ||
			report.GatePassed == nil || !*report.GatePassed ||
			report.CorrectnessGatePassed == nil ||
			!*report.CorrectnessGatePassed ||
			report.PerformanceGatePassed == nil ||
			!*report.PerformanceGatePassed ||
			report.Performance == nil ||
			report.Performance.LunaP95FasterWorkloads !=
				len(koeQualificationWorkloadNames) {
			t.Fatal("valid formal sample did not pass the unified gate")
		}

		results[0].CostObserved = false
		report = newKoeQualificationReport(cfg, jobs, results, true)
		if report.GatePassed == nil || *report.GatePassed ||
			report.CostFailureCount != 1 || report.CostObserved {
			t.Fatal("missing Cloud cost observation did not fail closed")
		}

		results[0].CostObserved = true
		cfg.maxCostUSD = 1
		report = newKoeQualificationReport(cfg, jobs, results, true)
		if report.GatePassed == nil || *report.GatePassed ||
			report.CostFailureCount != 1 {
			t.Fatal("reported cost above the configured budget did not fail closed")
		}
	})
}

func TestKoeQualificationSummaryReportsTimingCoverage(t *testing.T) {
	semantic := int64(10)
	text := int64(20)
	results := []koeQualificationRunReport{
		{
			Lane:                     koeQualificationFastLane,
			Workload:                 "no_tool",
			FirstSemanticDeltaMillis: &semantic,
			FirstTextDeltaMillis:     &text,
			TotalMillis:              30,
		},
		{
			Lane:         koeQualificationFastLane,
			Workload:     "no_tool",
			TotalMillis:  40,
			TaskSuccess:  true,
			CostObserved: true,
		},
	}
	summaries := summarizeKoeQualification(results)
	if len(summaries) != 1 {
		t.Fatalf("summary count=%d, want 1", len(summaries))
	}
	summary := summaries[0]
	if summary.FirstSemanticDeltaSampleCount != 1 ||
		summary.FirstSemanticDeltaCoverage != 0.5 ||
		summary.FirstTextDeltaSampleCount != 1 ||
		summary.FirstTextDeltaCoverage != 0.5 ||
		summary.FirstToolCallReadySampleCount != 0 ||
		summary.FirstToolCallReadyCoverage != 0 {
		t.Fatalf(
			"unexpected timing coverage: semantic=%d/%.2f text=%d/%.2f tool=%d/%.2f",
			summary.FirstSemanticDeltaSampleCount,
			summary.FirstSemanticDeltaCoverage,
			summary.FirstTextDeltaSampleCount,
			summary.FirstTextDeltaCoverage,
			summary.FirstToolCallReadySampleCount,
			summary.FirstToolCallReadyCoverage,
		)
	}
}

func syntheticKoeQualificationResults(
	repetitions int,
	fastMillis int64,
	fullMillis int64,
) []koeQualificationRunReport {
	results := make(
		[]koeQualificationRunReport,
		0,
		len(koeQualificationWorkloadNames)*repetitions*2,
	)
	for _, workload := range koeQualificationWorkloadNames {
		for repetition := 1; repetition <= repetitions; repetition++ {
			for _, lane := range []string{
				koeQualificationFastLane,
				koeQualificationSonnetReferenceLane,
			} {
				totalMillis := fullMillis
				costUSD := 0.02
				resolverIncluded := false
				var resolverExact *bool
				if lane == koeQualificationFastLane {
					totalMillis = fastMillis
					costUSD = 0.01
					resolverIncluded = true
					resolverExact = koeQualificationBoolPointer(true)
				}
				sideEffects := 0
				if workload == "one_tool" {
					sideEffects = 1
				}
				results = append(results, koeQualificationRunReport{
					Lane:                 lane,
					Workload:             workload,
					Repetition:           repetition,
					TaskSuccess:          true,
					ToolCorrectness:      true,
					RouteExact:           true,
					ProviderExact:        true,
					ModelExact:           true,
					CachePolicyExact:     true,
					ResolverIncluded:     resolverIncluded,
					ResolverProfileExact: resolverExact,
					CompletionCalls:      1,
					SideEffectExecutions: sideEffects,
					TotalMillis:          totalMillis,
					CostUSD:              costUSD,
					CostObserved:         true,
				})
			}
		}
	}
	return results
}

func TestKoeQualificationReportAtomicPartial(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "qualification.json.partial")
	cfg := koeQualificationRuntimeConfig{
		repetitions: koeQualificationDefaultRepetitions,
		seed:        koeQualificationDefaultSeed,
		maxCostUSD:  koeQualificationDefaultMaxCostUSD,
	}
	jobs := []koeQualificationJob{{
		Lane:       koeQualificationFastLane,
		Workload:   "no_tool",
		Repetition: 1,
	}}
	report := newKoeQualificationReport(cfg, jobs, nil, false)
	if err := writeKoeQualificationReport(path, report); err != nil {
		t.Fatalf("write partial: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read partial: %v", err)
	}
	var decoded koeQualificationReport
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode partial: %v", err)
	}
	if decoded.Complete || decoded.SampleQualifying ||
		decoded.GatePassed != nil ||
		decoded.Completed != 0 || decoded.Scheduled != 1 {
		t.Fatalf(
			"partial state = complete:%t sample_qualifying:%t gate_present:%t completed:%d scheduled:%d",
			decoded.Complete,
			decoded.SampleQualifying,
			decoded.GatePassed != nil,
			decoded.Completed,
			decoded.Scheduled,
		)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat partial: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("partial mode = %o, want 600", info.Mode().Perm())
	}
	matches, err := filepath.Glob(filepath.Join(dir, ".*.tmp"))
	if err != nil {
		t.Fatalf("glob temp files: %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("atomic writer left temp files: %v", matches)
	}
}

func TestKoeQualificationReportRejectsContentBearingKeys(t *testing.T) {
	for _, key := range []string{
		"prompt",
		"output",
		"args",
		"body",
		"profile_id",
		"api_key",
		"endpoint",
		"tool_arguments",
		"response_text",
		"result_text",
	} {
		encoded := []byte(fmt.Sprintf(`{"nested":{"%s":"secret"}}`, key))
		if err := validateKoeQualificationReportKeys(encoded); err == nil {
			t.Fatalf("key %q was accepted", key)
		}
	}
}

func TestKoeFastQualificationLive_AgentLoop(t *testing.T) {
	if os.Getenv(koeQualificationGateEnv) != "1" {
		t.Skip("set KOE_FAST_QUALIFICATION_LIVE=1 to run paid AgentLoop qualification")
	}

	cfg := loadKoeQualificationRuntimeConfig(t)
	jobs := buildKoeQualificationJobs(cfg)
	results := make([]koeQualificationRunReport, 0, len(jobs))
	partialPath := cfg.outputPath + ".partial"
	initial := newKoeQualificationReport(cfg, jobs, results, false)
	if err := writeKoeQualificationReport(partialPath, initial); err != nil {
		t.Fatalf("write initial content-free partial report: %v", err)
	}

	totalCtx, totalCancel := context.WithTimeout(
		context.Background(),
		koeQualificationTotalTimeout,
	)
	defer totalCancel()
	for index, job := range jobs {
		if totalCtx.Err() != nil {
			partial := newKoeQualificationReport(cfg, jobs, results, false)
			if err := writeKoeQualificationReport(partialPath, partial); err != nil {
				t.Fatalf("write timed-out content-free partial report: %v", err)
			}
			t.Fatalf(
				"qualification total timeout: completed=%d scheduled=%d partial=%s",
				len(results),
				len(jobs),
				partialPath,
			)
		}
		result := runKoeQualificationJob(totalCtx, cfg, job)
		result.ScheduleIndex = index + 1
		results = append(results, result)
		completed := len(results)
		partial := newKoeQualificationReport(cfg, jobs, results, false)
		if err := writeKoeQualificationReport(partialPath, partial); err != nil {
			t.Fatalf("write content-free partial report: %v", err)
		}
		if result.CompletionCalls > 0 && !result.CostObserved {
			t.Fatalf(
				"qualification cost observation missing: completed=%d scheduled=%d partial=%s",
				completed,
				len(jobs),
				partialPath,
			)
		}
		if partial.ReportedCostUSD > partial.MaxCostUSD {
			t.Fatalf(
				"qualification cost budget exceeded: completed=%d scheduled=%d reported_usd=%.6f max_usd=%.6f partial=%s",
				completed,
				len(jobs),
				partial.ReportedCostUSD,
				partial.MaxCostUSD,
				partialPath,
			)
		}
		if completed%koeQualificationProgressInterval == 0 ||
			completed == len(jobs) {
			t.Logf(
				"qualification progress: completed=%d scheduled=%d partial=%s",
				completed,
				len(jobs),
				partialPath,
			)
		}
		if totalCtx.Err() != nil {
			t.Fatalf(
				"qualification total timeout: completed=%d scheduled=%d partial=%s",
				completed,
				len(jobs),
				partialPath,
			)
		}
		if completed < len(jobs) {
			waitKoeQualificationPause(t, totalCtx, cfg.pause)
		}
	}

	report := newKoeQualificationReport(cfg, jobs, results, true)
	if err := writeKoeQualificationReport(cfg.outputPath, report); err != nil {
		t.Fatalf("write final content-free qualification report: %v", err)
	}
	if err := os.Remove(partialPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("remove completed partial report: %v", err)
	}

	if report.GatePassed == nil || report.CorrectnessGatePassed == nil {
		t.Fatal("final qualification report is missing gate evaluation")
	}
	t.Logf(
		"content-free qualification report written: complete=%t completed=%d scheduled=%d smoke=%t gate_passed=%t correctness_passed=%t performance_evaluated=%t behavior_failure_classes=%d performance_failure_classes=%d contract_failures=%d runtime_failures=%d duplicate_side_effect_failures=%d cost_failures=%d reported_cost_usd=%.6f path=%s",
		report.Complete,
		report.Completed,
		report.Scheduled,
		cfg.smoke,
		*report.GatePassed,
		*report.CorrectnessGatePassed,
		report.PerformanceGatePassed != nil,
		len(report.BehaviorFailureClasses),
		len(report.PerformanceFailureClasses),
		report.ContractFailureCount,
		report.RuntimeFailureCount,
		report.DuplicateSideEffectFailures,
		report.CostFailureCount,
		report.ReportedCostUSD,
		cfg.outputPath,
	)
	if !*report.GatePassed {
		t.Errorf(
			"qualification gate failed: behavior_classes=%d performance_classes=%d contract=%d runtime=%d duplicate_side_effect=%d cost=%d; inspect the content-free report",
			len(report.BehaviorFailureClasses),
			len(report.PerformanceFailureClasses),
			report.ContractFailureCount,
			report.RuntimeFailureCount,
			report.DuplicateSideEffectFailures,
			report.CostFailureCount,
		)
	}
}

func newKoeQualificationReport(
	cfg koeQualificationRuntimeConfig,
	jobs []koeQualificationJob,
	results []koeQualificationRunReport,
	complete bool,
) koeQualificationReport {
	resolverObserved := koeQualificationResolverObserved(results)
	summary := summarizeKoeQualification(results)
	gate := evaluateKoeQualificationGate(
		cfg,
		len(jobs),
		results,
		summary,
		complete,
	)
	return koeQualificationReport{
		SchemaVersion: 4,
		GeneratedAt:   time.Now().UTC().Format(time.RFC3339Nano),
		Complete:      complete,
		Completed:     len(results),
		Scheduled:     len(jobs),
		Seed:          cfg.seed,
		Repetitions:   cfg.repetitions,
		Smoke:         cfg.smoke,
		SampleQualifying: koeQualificationSampleQualifying(
			cfg,
			len(jobs),
			len(results),
			complete,
		),
		GatePassed:                  gate.gatePassed,
		CorrectnessGatePassed:       gate.correctnessPassed,
		PerformanceGatePassed:       gate.performancePassed,
		BehaviorFailureClasses:      gate.behaviorFailures,
		PerformanceFailureClasses:   gate.performanceFailures,
		ContractFailureCount:        gate.contractFailures,
		RuntimeFailureCount:         gate.runtimeFailures,
		DuplicateSideEffectFailures: gate.duplicateSideEffectFailures,
		CostFailureCount:            gate.costFailures,
		MaxCostUSD:                  cfg.maxCostUSD,
		ReportedCostUSD:             gate.reportedCostUSD,
		CostObserved:                gate.costObserved,
		PerformanceNote: "nearest-rank p95 has low tail precision at " +
			"the minimum qualifying n=30; inspect raw runs",
		Performance:                 gate.performance,
		Randomized:                  true,
		Concurrency:                 1,
		InterRunPauseMillis:         cfg.pause.Milliseconds(),
		MaxTokens:                   koeQualificationMaxTokens,
		SelectorExercised:           false,
		ProductionSelectorExercised: false,
		ResolutionPolicyExercised:   true,
		AgentLoopOnly:               false,
		SemanticDiscoveryEnabled:    false,
		ControlledLane:              "controlled_direct_requested_mode",
		Lanes: []koeQualificationLaneReport{
			{
				Name:                      koeQualificationFastLane,
				Mode:                      string(executionprofile.ModeFast),
				ExpectedProvider:          koeQualificationLunaProvider,
				ExpectedModel:             koeQualificationLunaModel,
				ExpectedAPISurface:        koeQualificationLunaAPISurface,
				ExpectedReasoningEffort:   koeQualificationLunaEffort,
				ExpectedServiceTier:       koeQualificationLunaServiceTier,
				ExpectedThinking:          "disabled",
				ExpectedParallelToolCalls: true,
				ResponseCachePolicy:       string(executionprofile.ResponseCacheOff),
				ResolverIncluded:          true,
				ResolverObservedProvider:  resolverObserved.provider,
				ResolverObservedModel:     resolverObserved.model,
				ResolverObservedAPI:       resolverObserved.apiSurface,
				ResolverObservedReasoning: resolverObserved.reasoning,
				ResolverObservedTier:      resolverObserved.serviceTier,
				RequestRoutingAuthority:   "execution_profile_id",
			},
			{
				Name:                      koeQualificationSonnetReferenceLane,
				Mode:                      string(executionprofile.ModeFull),
				ExpectedProvider:          koeQualificationSonnetProvider,
				ExpectedModel:             koeQualificationSonnetModel,
				ExpectedReasoningEffort:   "unset",
				ExpectedThinking:          "adaptive",
				ExpectedParallelToolCalls: false,
				ResponseCachePolicy:       "default",
				ResolverIncluded:          false,
				RequestRoutingAuthority:   "specific_model",
			},
		},
		Workloads: append([]string(nil), koeQualificationWorkloadNames...),
		Runs:      append([]koeQualificationRunReport(nil), results...),
		Summary:   summary,
	}
}

type koeQualificationResolverObservation struct {
	provider    string
	model       string
	apiSurface  string
	reasoning   string
	serviceTier string
}

func koeQualificationResolverObserved(
	results []koeQualificationRunReport,
) koeQualificationResolverObservation {
	found := false
	exact := true
	for _, result := range results {
		if result.Lane != koeQualificationFastLane {
			continue
		}
		found = true
		if result.ResolverProfileExact == nil || !*result.ResolverProfileExact {
			exact = false
		}
	}
	if !found {
		return koeQualificationResolverObservation{
			provider:    "unobserved",
			model:       "unobserved",
			apiSurface:  "unobserved",
			reasoning:   "unobserved",
			serviceTier: "unobserved",
		}
	}
	if !exact {
		return koeQualificationResolverObservation{
			provider:    "unexpected",
			model:       "unexpected",
			apiSurface:  "unexpected",
			reasoning:   "unexpected",
			serviceTier: "unexpected",
		}
	}
	return koeQualificationResolverObservation{
		provider:    koeQualificationLunaProvider,
		model:       koeQualificationLunaModel,
		apiSurface:  koeQualificationLunaAPISurface,
		reasoning:   koeQualificationLunaEffort,
		serviceTier: koeQualificationLunaServiceTier,
	}
}

func loadKoeQualificationRuntimeConfig(t *testing.T) koeQualificationRuntimeConfig {
	t.Helper()
	if os.Getenv("SHANNON_CACHE_DEBUG") == "1" ||
		os.Getenv("SHANNON_CACHE_DEBUG_RAW") == "1" {
		t.Fatal("qualification refuses SHANNON_CACHE_DEBUG=1 or SHANNON_CACHE_DEBUG_RAW=1")
	}
	endpoint := strings.TrimSpace(os.Getenv(koeQualificationEndpointEnv))
	if endpoint == "" {
		endpoint = koeQualificationEndpointFromUserConfig()
	}
	if endpoint == "" {
		t.Fatal("live qualification needs KOE_FAST_QUALIFICATION_ENDPOINT or a configured user endpoint")
	}
	apiKey := strings.TrimSpace(os.Getenv(koeQualificationAPIKeyEnv))
	if apiKey == "" {
		// Reuse the existing ToolSearch live-test credential convention without
		// ever logging or persisting the value.
		apiKey = strings.TrimSpace(os.Getenv("TOOLSEARCH_CLOUD_API_KEY"))
	}
	if apiKey == "" {
		apiKey = koeQualificationAPIKeyFromCredentialStore()
	}
	if apiKey == "" {
		t.Fatal("live qualification needs an explicit API key or a signed-in daemon credential")
	}

	repetitions := koeQualificationDefaultRepetitions
	if raw := strings.TrimSpace(os.Getenv(koeQualificationRepetitionsEnv)); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value <= 0 {
			t.Fatal("KOE_FAST_QUALIFICATION_REPETITIONS must be a positive integer")
		}
		repetitions = value
	}
	if repetitions > koeQualificationMaxRepetitions {
		t.Fatalf(
			"KOE_FAST_QUALIFICATION_REPETITIONS must not exceed %d",
			koeQualificationMaxRepetitions,
		)
	}
	smoke := os.Getenv(koeQualificationSmokeEnv) == "1"
	if repetitions < koeQualificationDefaultRepetitions && !smoke {
		t.Fatalf(
			"repetitions below %d require KOE_FAST_QUALIFICATION_SMOKE=1",
			koeQualificationDefaultRepetitions,
		)
	}

	maxCostUSD := koeQualificationDefaultMaxCostUSD
	if raw := strings.TrimSpace(os.Getenv(koeQualificationMaxCostEnv)); raw != "" {
		value, err := strconv.ParseFloat(raw, 64)
		if err != nil || !koeQualificationPositiveFinite(value) {
			t.Fatal("KOE_FAST_QUALIFICATION_MAX_COST_USD must be a positive finite number")
		}
		maxCostUSD = value
	}
	if maxCostUSD > koeQualificationHardMaxCostUSD {
		t.Fatalf(
			"KOE_FAST_QUALIFICATION_MAX_COST_USD must not exceed %.0f",
			koeQualificationHardMaxCostUSD,
		)
	}

	pause := time.Duration(0)
	if raw := strings.TrimSpace(os.Getenv(koeQualificationPauseEnv)); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 0 || value > 60_000 {
			t.Fatal("KOE_FAST_QUALIFICATION_PAUSE_MS must be an integer from 0 to 60000")
		}
		pause = time.Duration(value) * time.Millisecond
	}

	seed := koeQualificationDefaultSeed
	if raw := strings.TrimSpace(os.Getenv(koeQualificationSeedEnv)); raw != "" {
		value, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			t.Fatal("KOE_FAST_QUALIFICATION_SEED must be a signed 64-bit integer")
		}
		seed = value
	}
	outputPath := strings.TrimSpace(os.Getenv(koeQualificationOutputEnv))
	if outputPath == "" {
		outputPath = filepath.Join(
			os.TempDir(),
			fmt.Sprintf("koe-fast-qualification-%d.json", seed),
		)
	}

	return koeQualificationRuntimeConfig{
		endpoint:    strings.TrimRight(endpoint, "/"),
		apiKey:      apiKey,
		outputPath:  outputPath,
		repetitions: repetitions,
		seed:        seed,
		smoke:       smoke,
		maxCostUSD:  maxCostUSD,
		pause:       pause,
	}
}

// The explicit paid gate above is the authority boundary. Once enabled, these
// fallbacks reuse the same local endpoint and credential store as the daemon
// without logging or persisting either value in qualification artifacts.
func koeQualificationEndpointFromUserConfig() string {
	data, err := os.ReadFile(filepath.Join(config.ShannonDir(), "config.yaml"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "endpoint:") {
			return strings.TrimSpace(strings.TrimPrefix(line, "endpoint:"))
		}
	}
	return ""
}

func koeQualificationAPIKeyFromCredentialStore() string {
	if !keychain.Supported() {
		return ""
	}
	store, err := keychain.NewOSStoreAt(config.ShannonDir(), nil)
	if err != nil {
		return ""
	}
	if uid, err := store.Read(
		keychain.ServiceDaemonState,
		keychain.AccountCurrentUser,
	); err == nil && uid != "" {
		if key, err := store.Read(
			keychain.ServiceDaemonAPIKey,
			uid,
		); err == nil && key != "" {
			return key
		}
	}
	key, _ := store.Read(
		keychain.ServiceDaemonAPIKey,
		keychain.AccountLegacy,
	)
	return key
}

func waitKoeQualificationPause(
	t *testing.T,
	ctx context.Context,
	pause time.Duration,
) {
	t.Helper()
	if pause <= 0 {
		return
	}
	timer := time.NewTimer(pause)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
		t.Fatalf("qualification interrupted during inter-run pause: %v", ctx.Err())
	}
}

func buildKoeQualificationJobs(
	cfg koeQualificationRuntimeConfig,
) []koeQualificationJob {
	jobs := make(
		[]koeQualificationJob,
		0,
		len(koeQualificationWorkloadNames)*cfg.repetitions*2,
	)
	for _, workload := range koeQualificationWorkloadNames {
		for repetition := 1; repetition <= cfg.repetitions; repetition++ {
			token := koeQualificationToken(cfg.seed, workload, repetition)
			for _, lane := range []string{
				koeQualificationFastLane,
				koeQualificationSonnetReferenceLane,
			} {
				jobs = append(jobs, koeQualificationJob{
					Lane:       lane,
					Workload:   workload,
					Repetition: repetition,
					Token:      token,
				})
			}
		}
	}
	rng := rand.New(rand.NewSource(cfg.seed))
	rng.Shuffle(len(jobs), func(i, j int) {
		jobs[i], jobs[j] = jobs[j], jobs[i]
	})
	return jobs
}

func koeQualificationToken(seed int64, workload string, repetition int) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d/%s/%d", seed, workload, repetition)))
	return strings.ToUpper(fmt.Sprintf("%x", sum[:6]))
}

func runKoeQualificationJob(
	parentCtx context.Context,
	cfg koeQualificationRuntimeConfig,
	job koeQualificationJob,
) koeQualificationRunReport {
	start := time.Now()
	jobCtx, jobCancel := context.WithTimeout(parentCtx, koeQualificationRunTimeout)
	defer jobCancel()

	gateway := client.NewGatewayClient(cfg.endpoint, cfg.apiKey)
	var (
		fastProfile          executionprofile.Profile
		resolverMillis       *int64
		resolverExact        *bool
		resolverErrors       []koeQualificationClientError
		resolverFailureClass string
	)
	if job.Lane == koeQualificationFastLane {
		resolveStart := time.Now()
		resolveCtx, resolveCancel := context.WithTimeout(
			jobCtx,
			koeQualificationResolverTimeout,
		)
		cloudProfile, resolveErr := gateway.ResolveKoeExecutionProfile(resolveCtx)
		resolveCancel()
		elapsed := time.Since(resolveStart).Milliseconds()
		resolverMillis = &elapsed
		if resolveErr != nil {
			resolverErrors = append(
				resolverErrors,
				classifyKoeQualificationClientError(resolveErr),
			)
		}
		fastProfile = executionprofile.Resolve(executionprofile.ResolutionInput{
			RequestedMode: executionprofile.ModeFast,
			FastEnabled:   true,
			CloudProfile:  &cloudProfile,
			CloudError:    resolveErr,
		})
		exact := resolveErr == nil &&
			cloudProfile == fastProfile &&
			fastProfile.ValidateFast() == nil
		resolverExact = &exact
		if resolveErr != nil {
			resolverFailureClass = "resolver_" +
				koeQualificationRuntimeErrorClassOnly(resolveErr)
		} else if !exact {
			resolverFailureClass = "resolver_contract_error"
		}
	}

	if resolverFailureClass != "" {
		report := koeQualificationRunReport{
			Lane:                 job.Lane,
			Workload:             job.Workload,
			Repetition:           job.Repetition,
			ResolverIncluded:     true,
			ResolverProfileExact: resolverExact,
			ResolverMillis:       resolverMillis,
			TotalMillis:          time.Since(start).Milliseconds(),
			RuntimeErrorClass:    resolverFailureClass,
		}
		for _, item := range resolverErrors {
			if item.HTTPStatus != 0 {
				report.HTTPStatuses = append(report.HTTPStatuses, item.HTTPStatus)
			}
			if item.Transport {
				report.TransportErrors++
			}
			if item.Retryable {
				report.RetryableClientErrors++
			}
		}
		sort.Ints(report.HTTPStatuses)
		report.Outcome = koeQualificationOutcome(report)
		return report
	}

	workload := buildKoeQualificationWorkload(job)
	qualificationClient := &koeQualificationLLMClient{
		inner: gateway,
		start: start,
	}

	loop := NewAgentLoop(
		qualificationClient,
		workload.registry,
		"",
		"",
		koeQualificationMaxIterations,
		4000,
		200,
		nil,
		nil,
		nil,
	)
	loop.SetMaxTokens(koeQualificationMaxTokens)
	loop.SetEnableStreaming(true)
	// Semantic skill discovery is deliberately disabled for every controlled
	// lane. The explicit_use_skill_activation cell still executes production
	// newUseSkillTool; disabling the separate selector isolates activation
	// correctness from semantic-selector quality, which has its own Gate.
	loop.SetSkillDiscovery(false)
	loop.SetSkills(workload.skills)
	loop.SetCacheSource("koe_fast_qualification")
	loop.SetSessionID(fmt.Sprintf(
		"koe-qualification-%s-%s-%d",
		job.Lane,
		job.Workload,
		job.Repetition,
	))
	if job.Lane == koeQualificationFastLane {
		loop.SetKoeExecutionProfile(fastProfile)
	} else {
		loop.SetSpecificModel(koeQualificationSonnetModel)
		loop.SetThinking(&client.ThinkingConfig{Type: "adaptive"})
		loop.SetReasoningEffort("")
		loop.SetEffortTier("")
		loop.SetKoeExecutionProfile(executionprofile.FullProfile(
			executionprofile.ModeFull,
			"qualification_control",
		))
	}

	handler := &koeQualificationEventHandler{
		state: workload.state,
	}
	loop.SetHandler(handler)
	resultText, usage, runErr := loop.Run(jobCtx, workload.prompt, nil, nil)
	total := time.Since(start)

	requests,
		responses,
		clientErrors,
		firstSemanticDelta,
		firstTextDelta,
		firstToolCallReady := qualificationClient.snapshot()
	clientErrors = append(resolverErrors, clientErrors...)
	toolCalls, toolIterations := koeQualificationToolCalls(responses)
	observations,
		parallelMax,
		duplicateToolExecutions,
		sideEffectExecutions,
		duplicateSideEffectExecutions := workload.state.snapshot()
	taskSuccess, toolCorrectness := evaluateKoeQualificationWorkload(
		workload,
		job.Lane,
		resultText,
		requests,
		responses,
		toolCalls,
		observations,
		parallelMax,
		sideEffectExecutions,
	)
	expectedProvider, expectedModel := koeQualificationExpectedRoute(job.Lane)
	providerExact, observedProviders := koeQualificationObservedExact(
		responses,
		expectedProvider,
		func(response client.CompletionResponse) string { return response.Provider },
	)
	modelExact, observedModels := koeQualificationObservedExact(
		responses,
		expectedModel,
		func(response client.CompletionResponse) string { return response.Model },
	)
	routeExact := koeQualificationRouteExact(
		job.Lane,
		fastProfile.ProfileID,
		requests,
	)
	cacheExact, cacheHits := koeQualificationCacheExact(responses)

	report := koeQualificationRunReport{
		Lane:                          job.Lane,
		Workload:                      job.Workload,
		Repetition:                    job.Repetition,
		TaskSuccess:                   runErr == nil && taskSuccess,
		ToolCorrectness:               toolCorrectness,
		RouteExact:                    routeExact,
		ProviderExact:                 providerExact,
		ModelExact:                    modelExact,
		CachePolicyExact:              cacheExact,
		ResolverIncluded:              job.Lane == koeQualificationFastLane,
		ResolverProfileExact:          resolverExact,
		ResolverMillis:                resolverMillis,
		ObservedProviders:             observedProviders,
		ObservedModels:                observedModels,
		CompletionCalls:               len(requests),
		ToolIterations:                toolIterations,
		ToolCalls:                     len(toolCalls),
		DuplicateModelCalls:           koeQualificationDuplicateCalls(toolCalls),
		DuplicateToolExecutions:       duplicateToolExecutions,
		SideEffectExecutions:          sideEffectExecutions,
		DuplicateSideEffectExecutions: duplicateSideEffectExecutions,
		TotalMillis:                   total.Milliseconds(),
		GatewayLatencyMs:              koeQualificationGatewayLatency(responses),
		ResponseCacheHits:             cacheHits,
		RetryCount:                    handler.RetryCount(),
	}
	if job.Workload == "parallel" {
		report.ParallelMaxInFlight = parallelMax
	}
	if usage != nil {
		report.InputTokens = usage.InputTokens
		report.OutputTokens = usage.OutputTokens
		report.TotalTokens = usage.TotalTokens
		report.CacheReadTokens = usage.CacheReadTokens
		report.CacheCreationTokens = usage.CacheCreationTokens
		report.CostUSD = usage.CostUSD
	}
	report.CostObserved = report.CompletionCalls > 0 &&
		usage != nil &&
		usage.LLMCalls == report.CompletionCalls &&
		koeQualificationPositiveFinite(report.CostUSD)
	report.FirstSemanticDeltaMillis = durationMillis(firstSemanticDelta)
	report.FirstTextDeltaMillis = durationMillis(firstTextDelta)
	report.FirstToolCallReadyMillis = durationMillis(firstToolCallReady)
	for _, item := range clientErrors {
		if item.HTTPStatus != 0 {
			report.HTTPStatuses = append(report.HTTPStatuses, item.HTTPStatus)
		}
		if item.Transport {
			report.TransportErrors++
		}
		if item.Retryable {
			report.RetryableClientErrors++
		}
	}
	sort.Ints(report.HTTPStatuses)
	if runErr != nil {
		report.RuntimeErrorClass, _ = koeQualificationRuntimeErrorClass(runErr)
	}
	report.Outcome = koeQualificationOutcome(report)
	return report
}

func durationMillis(value *time.Duration) *int64 {
	if value == nil {
		return nil
	}
	ms := value.Milliseconds()
	return &ms
}

func koeQualificationPositiveFinite(value float64) bool {
	return value > 0 && !math.IsNaN(value) && !math.IsInf(value, 0)
}

func buildKoeQualificationWorkload(
	job koeQualificationJob,
) koeQualificationWorkload {
	state := newKoeQualificationToolState()
	registry := NewToolRegistry()
	workload := koeQualificationWorkload{
		name:     job.Workload,
		registry: registry,
		state:    state,
	}

	switch job.Workload {
	case "no_tool":
		workload.receipt = "NO_TOOL_RECEIPT_" + job.Token
		workload.prompt = fmt.Sprintf(
			"Do not call any tool. Reply with exactly %s and nothing else.",
			workload.receipt,
		)

	case "one_tool":
		workload.receipt = "ONE_TOOL_RECEIPT_" + job.Token
		registry.Register(&koeQualificationTool{
			name:         "qualification_echo",
			description:  "Return the deterministic receipt for one qualification token.",
			kind:         "token",
			argName:      "token",
			expectedText: job.Token,
			result:       workload.receipt,
			state:        state,
			sideEffect:   true,
		})
		workload.prompt = fmt.Sprintf(
			"Call qualification_echo exactly once with token %q. "+
				"After observing its result, reply with exactly %s.",
			job.Token,
			workload.receipt,
		)

	case "serial_3":
		workload.receipt = "SERIAL_RECEIPT_" + job.Token
		registry.Register(&koeQualificationTool{
			name:        "qualification_serial_step",
			description: "Execute exactly one stateful qualification step in strict ascending order.",
			kind:        "serial",
			argName:     "step",
			result:      "SERIAL_" + job.Token,
			state:       state,
		})
		workload.prompt = fmt.Sprintf(
			"Call qualification_serial_step three times, strictly serially: "+
				"call step 1 and wait for its result, then step 2 and wait, then step 3 and wait. "+
				"Never place two steps in one assistant tool-call turn. "+
				"After all three succeed, reply with exactly %s.",
			workload.receipt,
		)

	case "parallel":
		workload.receipt = "PARALLEL_RECEIPT_" + job.Token
		registry.Register(&koeQualificationTool{
			name:            "qualification_parallel_lookup",
			description:     "Look up one independent side of a two-key qualification pair.",
			kind:            "parallel",
			argName:         "key",
			state:           state,
			concurrent:      true,
			parallelBarrier: true,
			result:          "PARALLEL_VALUE_" + job.Token,
		})
		workload.prompt = fmt.Sprintf(
			"In one assistant tool-call turn, call qualification_parallel_lookup "+
				"twice in parallel: once with key %q and once with key %q. "+
				"Wait for both results, then reply with exactly %s.",
			"left",
			"right",
			workload.receipt,
		)

	case "deferred_tool":
		workload.receipt = "DEFERRED_RECEIPT_" + job.Token
		registry.Register(&koeQualificationTool{
			name:         "qualification_deferred_lookup",
			description:  "Retrieve a hidden nebula qualification receipt by exact probe token.",
			kind:         "token",
			argName:      "probe_token",
			expectedText: job.Token,
			result:       workload.receipt,
			state:        state,
			source:       SourceIntegration,
			exposure:     ToolExposureDeferred,
			namespace:    "qualification_nebula",
			concurrent:   true,
		})
		workload.prompt = fmt.Sprintf(
			"First discover qualification_deferred_lookup with tool_search using "+
				"the exact query %q. Then call the discovered tool exactly once "+
				"with probe_token %q. After observing its result, reply with exactly %s.",
			"select:qualification_deferred_lookup",
			job.Token,
			workload.receipt,
		)

	case "explicit_use_skill_activation":
		workload.receipt = "SKILL_RECEIPT_" + job.Token
		workload.skills = []*skills.Skill{{
			Name:         "qualification-skill",
			Slug:         "qualification-skill",
			Description:  "Activate this skill for the isolated qualification action.",
			Prompt:       "Call qualification_skill_action exactly once with the requested token.",
			AllowedTools: []string{"qualification_skill_action"},
		}}
		registry.Register(&koeQualificationUseSkillTool{
			inner: newUseSkillTool(&workload.skills),
			state: state,
		})
		registry.Register(&koeQualificationTool{
			name:         "qualification_skill_action",
			description:  "Execute the action allowed by the active qualification skill.",
			kind:         "skill_action",
			argName:      "token",
			expectedText: job.Token,
			result:       workload.receipt,
			state:        state,
		})
		workload.prompt = fmt.Sprintf(
			"First call use_skill with skill_name %q. After it activates, call "+
				"qualification_skill_action exactly once with token %q. "+
				"Then reply with exactly %s.",
			"qualification-skill",
			job.Token,
			workload.receipt,
		)

	case "validation_repair":
		workload.receipt = "REPAIR_RECEIPT_" + job.Token
		expected := "fixed-" + job.Token
		registry.Register(&koeQualificationTool{
			name:         "qualification_repair",
			description:  "Validate and accept the repaired qualification token.",
			kind:         "repair",
			argName:      "token",
			expectedText: expected,
			result:       workload.receipt,
			state:        state,
		})
		workload.prompt = fmt.Sprintf(
			"Exercise validation recovery exactly as follows. First call "+
				"qualification_repair with an empty object so the required token "+
				"validation fails. After observing that validation error, call it "+
				"again with token %q. Then reply with exactly %s.",
			expected,
			workload.receipt,
		)

	default:
		panic("unknown Koe qualification workload: " + job.Workload)
	}
	return workload
}

func evaluateKoeQualificationWorkload(
	workload koeQualificationWorkload,
	lane string,
	resultText string,
	requests []client.CompletionRequest,
	responses []client.CompletionResponse,
	calls []client.FunctionCall,
	observations []koeQualificationToolObservation,
	parallelMax int,
	sideEffectExecutions int,
) (bool, bool) {
	taskSuccess := strings.TrimSpace(resultText) == workload.receipt
	expectedEffects := 0
	if workload.name == "one_tool" {
		expectedEffects = 1
	}
	if sideEffectExecutions != expectedEffects {
		return taskSuccess, false
	}
	switch workload.name {
	case "no_tool":
		return taskSuccess,
			len(calls) == 0 &&
				len(observations) == 0

	case "one_tool":
		return taskSuccess,
			koeQualificationExactObserved(
				observations,
				[]string{"qualification_echo"},
				[]string{workloadTokenFromReceipt(workload.receipt)},
			) &&
				koeQualificationCallNames(calls, "qualification_echo")

	case "serial_3":
		if len(calls) != 3 || len(observations) != 3 {
			return taskSuccess, false
		}
		for index := range observations {
			if observations[index].Name != "qualification_serial_step" ||
				observations[index].Number != index+1 ||
				!observations[index].Valid {
				return taskSuccess, false
			}
		}
		if koeQualificationToolIterationsForName(
			responses,
			"qualification_serial_step",
		) != 3 {
			return taskSuccess, false
		}
		return taskSuccess, koeQualificationCallNames(calls,
			"qualification_serial_step",
			"qualification_serial_step",
			"qualification_serial_step",
		)

	case "parallel":
		if len(calls) != 2 || len(observations) != 2 || parallelMax < 2 {
			return taskSuccess, false
		}
		for _, observation := range observations {
			if observation.Name != "qualification_parallel_lookup" ||
				!observation.Valid {
				return taskSuccess, false
			}
		}
		keys := koeQualificationArgumentStrings(calls, "key")
		sort.Strings(keys)
		sameTurn := false
		for _, response := range responses {
			count := 0
			for _, call := range response.AllToolCalls() {
				if call.Name == "qualification_parallel_lookup" {
					count++
				}
			}
			if count == 2 {
				sameTurn = true
			}
		}
		return taskSuccess,
			sameTurn &&
				len(keys) == 2 &&
				keys[0] == "left" &&
				keys[1] == "right"

	case "deferred_tool":
		if len(calls) != 2 ||
			!koeQualificationCallNames(
				calls,
				"tool_search",
				"qualification_deferred_lookup",
			) ||
			len(observations) != 1 ||
			observations[0].Name != "qualification_deferred_lookup" ||
			!observations[0].Valid {
			return taskSuccess, false
		}
		return taskSuccess, koeQualificationDeferredProtocolExact(lane, requests)

	case "explicit_use_skill_activation":
		return taskSuccess,
			koeQualificationCallNames(
				calls,
				"use_skill",
				"qualification_skill_action",
			) &&
				len(observations) == 2 &&
				observations[0].Name == "use_skill" &&
				observations[0].Valid &&
				observations[1].Name == "qualification_skill_action" &&
				observations[1].Valid

	case "validation_repair":
		if len(calls) != 2 ||
			!koeQualificationCallNames(
				calls,
				"qualification_repair",
				"qualification_repair",
			) ||
			len(observations) != 1 ||
			!observations[0].Valid {
			return taskSuccess, false
		}
		first := koeQualificationArgumentString(calls[0], "token")
		second := koeQualificationArgumentString(calls[1], "token")
		return taskSuccess,
			first == "" &&
				second == observations[0].Text &&
				strings.HasPrefix(second, "fixed-")
	}
	return taskSuccess, false
}

func workloadTokenFromReceipt(receipt string) string {
	const prefix = "ONE_TOOL_RECEIPT_"
	return strings.TrimPrefix(receipt, prefix)
}

func koeQualificationExactObserved(
	observations []koeQualificationToolObservation,
	names []string,
	values []string,
) bool {
	if len(observations) != len(names) || len(names) != len(values) {
		return false
	}
	for index := range observations {
		if observations[index].Name != names[index] ||
			observations[index].Text != values[index] ||
			!observations[index].Valid {
			return false
		}
	}
	return true
}

func koeQualificationCallNames(
	calls []client.FunctionCall,
	names ...string,
) bool {
	if len(calls) != len(names) {
		return false
	}
	for index := range calls {
		if calls[index].Name != names[index] {
			return false
		}
	}
	return true
}

func koeQualificationArgumentStrings(
	calls []client.FunctionCall,
	name string,
) []string {
	values := make([]string, 0, len(calls))
	for _, call := range calls {
		value := koeQualificationArgumentString(call, name)
		if value != "" {
			values = append(values, value)
		}
	}
	return values
}

func koeQualificationArgumentString(
	call client.FunctionCall,
	name string,
) string {
	var values map[string]any
	if err := json.Unmarshal([]byte(call.ArgumentsString()), &values); err != nil {
		return ""
	}
	value, _ := values[name].(string)
	return value
}

func koeQualificationToolCalls(
	responses []client.CompletionResponse,
) ([]client.FunctionCall, int) {
	var calls []client.FunctionCall
	iterations := 0
	for index := range responses {
		responseCalls := responses[index].AllToolCalls()
		if len(responseCalls) > 0 {
			iterations++
			calls = append(calls, responseCalls...)
		}
	}
	return calls, iterations
}

func koeQualificationToolIterationsForName(
	responses []client.CompletionResponse,
	name string,
) int {
	iterations := 0
	for index := range responses {
		found := false
		for _, call := range responses[index].AllToolCalls() {
			if call.Name == name {
				found = true
			}
		}
		if found {
			iterations++
		}
	}
	return iterations
}

func koeQualificationDuplicateCalls(calls []client.FunctionCall) int {
	seen := make(map[string]int)
	duplicates := 0
	for _, call := range calls {
		key := call.Name + "\x00" + koeQualificationCanonicalArguments(call)
		seen[key]++
		if seen[key] > 1 {
			duplicates++
		}
	}
	return duplicates
}

func koeQualificationCanonicalArguments(call client.FunctionCall) string {
	return koeQualificationCanonicalJSON(call.ArgumentsString())
}

func koeQualificationCanonicalJSON(raw string) string {
	var value any
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		return "<invalid>"
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return "<invalid>"
	}
	return string(encoded)
}

func koeQualificationDeferredProtocolExact(
	_ string,
	requests []client.CompletionRequest,
) bool {
	if len(requests) < 2 {
		return false
	}
	first := requests[0]
	if !koeQualificationSchemaState(first, "tool_search", true, false) {
		return false
	}
	const deferredName = "qualification_deferred_lookup"
	if !koeQualificationSchemaState(first, deferredName, false, false) {
		return false
	}
	for _, request := range requests[1:] {
		if koeQualificationRequestHasToolResultText(
			request,
			"LOADED:"+deferredName,
		) {
			return true
		}
	}
	return false
}

func koeQualificationSchemaState(
	request client.CompletionRequest,
	name string,
	wantPresent bool,
	wantDeferred bool,
) bool {
	for _, tool := range request.Tools {
		if koeQualificationSchemaToolName(tool) != name {
			continue
		}
		return wantPresent && tool.DeferLoading == wantDeferred
	}
	return !wantPresent
}

func koeQualificationSchemaToolName(tool client.Tool) string {
	if tool.Function.Name != "" {
		return tool.Function.Name
	}
	return tool.Name
}

func koeQualificationRequestHasToolResultText(
	request client.CompletionRequest,
	substring string,
) bool {
	for _, message := range request.Messages {
		for _, block := range message.Content.Blocks() {
			if block.Type != "tool_result" {
				continue
			}
			if text, ok := block.ToolContent.(string); ok &&
				strings.Contains(text, substring) {
				return true
			}
		}
	}
	return false
}

func koeQualificationRequestHasToolReference(
	request client.CompletionRequest,
	name string,
) bool {
	for _, message := range request.Messages {
		for _, block := range message.Content.Blocks() {
			if block.Type != "tool_result" {
				continue
			}
			nested, ok := block.ToolContent.([]client.ContentBlock)
			if !ok {
				continue
			}
			for _, child := range nested {
				if child.Type == "tool_reference" && child.ToolName == name {
					return true
				}
			}
		}
	}
	return false
}

func koeQualificationExpectedRoute(lane string) (string, string) {
	if lane == koeQualificationFastLane {
		return koeQualificationLunaProvider, koeQualificationLunaModel
	}
	return koeQualificationSonnetProvider, koeQualificationSonnetModel
}

func koeQualificationObservedExact(
	responses []client.CompletionResponse,
	expected string,
	selectValue func(client.CompletionResponse) string,
) (bool, []string) {
	if len(responses) == 0 {
		return false, nil
	}
	values := make(map[string]bool)
	exact := true
	for _, response := range responses {
		value := strings.TrimSpace(selectValue(response))
		if value != expected {
			exact = false
			values["unexpected"] = true
			continue
		}
		values[expected] = true
	}
	out := make([]string, 0, len(values))
	for value := range values {
		out = append(out, value)
	}
	sort.Strings(out)
	return exact, out
}

func koeQualificationRouteExact(
	lane string,
	fastProfileID string,
	requests []client.CompletionRequest,
) bool {
	if len(requests) == 0 {
		return false
	}
	for _, request := range requests {
		if request.MaxTokens != koeQualificationMaxTokens {
			return false
		}
		if lane == koeQualificationFastLane {
			if request.ResponseCachePolicy != executionprofile.ResponseCacheOff ||
				request.ExecutionProfileID == "" ||
				request.ExecutionProfileID != fastProfileID ||
				request.ModelTier != "" ||
				request.SpecificModel != "" ||
				request.ReasoningEffort != "" ||
				request.EffortTier != "" ||
				request.Thinking != nil ||
				!request.ParallelToolCalls {
				return false
			}
			continue
		}
		if request.ResponseCachePolicy != "" ||
			request.ExecutionProfileID != "" ||
			request.ModelTier != "" ||
			request.SpecificModel != koeQualificationSonnetModel ||
			request.ReasoningEffort != "" ||
			request.EffortTier != "" ||
			request.Thinking == nil ||
			request.Thinking.Type != "adaptive" ||
			request.Thinking.BudgetTokens != 0 ||
			request.ParallelToolCalls {
			return false
		}
	}
	return true
}

func koeQualificationCacheExact(
	responses []client.CompletionResponse,
) (bool, int) {
	if len(responses) == 0 {
		return false, 0
	}
	hits := 0
	for _, response := range responses {
		if response.Cached {
			hits++
		}
	}
	return hits == 0, hits
}

func koeQualificationGatewayLatency(
	responses []client.CompletionResponse,
) int {
	total := 0
	for _, response := range responses {
		total += response.LatencyMs
	}
	return total
}

func koeQualificationRuntimeErrorClass(err error) (string, int) {
	if err == nil {
		return "", 0
	}
	var apiErr *client.APIError
	if errors.As(err, &apiErr) {
		return "http_error", apiErr.StatusCode
	}
	switch {
	case errors.Is(err, ErrEmptyFinalResponse):
		return "empty_final_response", 0
	case errors.Is(err, ErrMaxIterReached):
		return "max_iterations", 0
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline", 0
	case errors.Is(err, context.Canceled):
		return "canceled", 0
	case errors.Is(err, client.ErrStreamIdleTimeout):
		return "stream_idle_timeout", 0
	case client.TransportErrorShape(err):
		return "transport_error", 0
	default:
		return "runtime_error", 0
	}
}

func koeQualificationRuntimeErrorClassOnly(err error) string {
	class, _ := koeQualificationRuntimeErrorClass(err)
	return class
}

func koeQualificationOutcome(report koeQualificationRunReport) string {
	switch {
	case len(report.HTTPStatuses) > 0:
		return "http_error"
	case report.TransportErrors > 0:
		return "transport_error"
	case report.RuntimeErrorClass != "":
		return report.RuntimeErrorClass
	case !report.RouteExact:
		return "route_mismatch"
	case !report.ProviderExact:
		return "provider_mismatch"
	case !report.ModelExact:
		return "model_mismatch"
	case !report.CachePolicyExact:
		return "cache_policy_mismatch"
	case !report.ToolCorrectness:
		return "tool_incorrect"
	case !report.TaskSuccess:
		return "task_incorrect"
	default:
		return "success"
	}
}

func summarizeKoeQualification(
	results []koeQualificationRunReport,
) []koeQualificationCellSummary {
	type cellKey struct {
		lane     string
		workload string
	}
	grouped := make(map[cellKey][]koeQualificationRunReport)
	for _, result := range results {
		key := cellKey{lane: result.Lane, workload: result.Workload}
		grouped[key] = append(grouped[key], result)
	}

	keys := make([]cellKey, 0, len(grouped))
	for key := range grouped {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].workload != keys[j].workload {
			return keys[i].workload < keys[j].workload
		}
		return keys[i].lane < keys[j].lane
	})

	out := make([]koeQualificationCellSummary, 0, len(keys))
	for _, key := range keys {
		cell := grouped[key]
		summary := koeQualificationCellSummary{
			Lane:     key.lane,
			Workload: key.workload,
			Runs:     len(cell),
		}
		var (
			taskSuccess, toolCorrect, routeExact   int
			providerExact, modelExact, cacheExact  int
			completionCalls, toolIterations        int
			inputTokens, outputTokens, totalTokens int
			cacheReadTokens, cacheCreationTokens   int
			resolverExact, resolverRuns            int
			retryRuns                              int
			semanticValues, textValues             []int64
			toolReadyValues, totalValues           []int64
		)
		for _, result := range cell {
			taskSuccess += boolInt(result.TaskSuccess)
			toolCorrect += boolInt(result.ToolCorrectness)
			routeExact += boolInt(result.RouteExact)
			providerExact += boolInt(result.ProviderExact)
			modelExact += boolInt(result.ModelExact)
			cacheExact += boolInt(result.CachePolicyExact)
			if len(result.HTTPStatuses) > 0 {
				summary.HTTPErrorRuns++
			}
			if result.TransportErrors > 0 {
				summary.TransportErrorRuns++
			}
			if result.ResolverProfileExact != nil {
				resolverRuns++
				resolverExact += boolInt(*result.ResolverProfileExact)
			}
			summary.DuplicateModelCalls += result.DuplicateModelCalls
			summary.DuplicateToolExecutions += result.DuplicateToolExecutions
			summary.SideEffectExecutions += result.SideEffectExecutions
			summary.DuplicateSideEffectExecutions +=
				result.DuplicateSideEffectExecutions
			completionCalls += result.CompletionCalls
			toolIterations += result.ToolIterations
			summary.RetryCount += result.RetryCount
			summary.RetryableClientErrors += result.RetryableClientErrors
			if result.RetryCount > 0 {
				retryRuns++
			}
			inputTokens += result.InputTokens
			outputTokens += result.OutputTokens
			totalTokens += result.TotalTokens
			cacheReadTokens += result.CacheReadTokens
			cacheCreationTokens += result.CacheCreationTokens
			summary.CostUSDTotal += result.CostUSD
			totalValues = append(totalValues, result.TotalMillis)
			if result.FirstSemanticDeltaMillis != nil {
				semanticValues = append(
					semanticValues,
					*result.FirstSemanticDeltaMillis,
				)
			}
			if result.FirstTextDeltaMillis != nil {
				textValues = append(textValues, *result.FirstTextDeltaMillis)
			}
			if result.FirstToolCallReadyMillis != nil {
				toolReadyValues = append(
					toolReadyValues,
					*result.FirstToolCallReadyMillis,
				)
			}
		}
		count := float64(len(cell))
		summary.SuccessfulTasks = taskSuccess
		summary.TaskSuccessRate = float64(taskSuccess) / count
		summary.ToolCorrectnessRate = float64(toolCorrect) / count
		summary.RouteExactRate = float64(routeExact) / count
		summary.ProviderExactRate = float64(providerExact) / count
		summary.ModelExactRate = float64(modelExact) / count
		summary.CachePolicyExactRate = float64(cacheExact) / count
		summary.CompletionCallsMean = float64(completionCalls) / count
		summary.ToolIterationsMean = float64(toolIterations) / count
		summary.RetryRunRate = float64(retryRuns) / count
		if completionCalls > 0 {
			summary.RetriesPerCompletionCall =
				float64(summary.RetryCount) / float64(completionCalls)
		}
		if resolverRuns > 0 {
			rate := float64(resolverExact) / float64(resolverRuns)
			summary.ResolverProfileExactRate = &rate
		}
		summary.InputTokensMean = float64(inputTokens) / count
		summary.OutputTokensMean = float64(outputTokens) / count
		summary.TotalTokensMean = float64(totalTokens) / count
		summary.CacheReadTokensMean = float64(cacheReadTokens) / count
		summary.CacheCreationTokensMean = float64(cacheCreationTokens) / count
		summary.CostUSDMean = summary.CostUSDTotal / count
		summary.FirstSemanticDeltaSampleCount = len(semanticValues)
		summary.FirstSemanticDeltaCoverage =
			float64(len(semanticValues)) / count
		summary.FirstTextDeltaSampleCount = len(textValues)
		summary.FirstTextDeltaCoverage = float64(len(textValues)) / count
		summary.FirstToolCallReadySampleCount = len(toolReadyValues)
		summary.FirstToolCallReadyCoverage =
			float64(len(toolReadyValues)) / count
		if taskSuccess > 0 {
			perSuccess := summary.CostUSDTotal / float64(taskSuccess)
			summary.CostUSDPerSuccessfulTask = &perSuccess
		}
		summary.TotalP50Millis = koeQualificationPercentile(totalValues, 0.50)
		summary.TotalP95Millis = koeQualificationPercentile(totalValues, 0.95)
		if len(semanticValues) > 0 {
			p50 := koeQualificationPercentile(semanticValues, 0.50)
			p95 := koeQualificationPercentile(semanticValues, 0.95)
			summary.FirstSemanticDeltaP50Millis = &p50
			summary.FirstSemanticDeltaP95Millis = &p95
		}
		if len(textValues) > 0 {
			p50 := koeQualificationPercentile(textValues, 0.50)
			p95 := koeQualificationPercentile(textValues, 0.95)
			summary.FirstTextDeltaP50Millis = &p50
			summary.FirstTextDeltaP95Millis = &p95
		}
		if len(toolReadyValues) > 0 {
			p50 := koeQualificationPercentile(toolReadyValues, 0.50)
			p95 := koeQualificationPercentile(toolReadyValues, 0.95)
			summary.FirstToolCallReadyP50Millis = &p50
			summary.FirstToolCallReadyP95Millis = &p95
		}
		out = append(out, summary)
	}
	return out
}

type koeQualificationGateEvaluation struct {
	gatePassed                  *bool
	correctnessPassed           *bool
	performancePassed           *bool
	behaviorFailures            []string
	performanceFailures         []string
	contractFailures            int
	runtimeFailures             int
	duplicateSideEffectFailures int
	costFailures                int
	reportedCostUSD             float64
	costObserved                bool
	performance                 *koeQualificationPerformance
}

func koeQualificationSampleQualifying(
	cfg koeQualificationRuntimeConfig,
	scheduled int,
	completed int,
	complete bool,
) bool {
	expected := len(koeQualificationWorkloadNames) * cfg.repetitions * 2
	return complete &&
		!cfg.smoke &&
		cfg.repetitions >= koeQualificationDefaultRepetitions &&
		cfg.repetitions <= koeQualificationMaxRepetitions &&
		scheduled == expected &&
		completed == scheduled
}

func evaluateKoeQualificationGate(
	cfg koeQualificationRuntimeConfig,
	scheduled int,
	results []koeQualificationRunReport,
	summaries []koeQualificationCellSummary,
	complete bool,
) koeQualificationGateEvaluation {
	evaluation := koeQualificationGateEvaluation{
		behaviorFailures:    []string{},
		performanceFailures: []string{},
	}
	anyCompletion := false
	allCostObserved := true
	for _, result := range results {
		resolverExact := !result.ResolverIncluded ||
			(result.ResolverProfileExact != nil &&
				*result.ResolverProfileExact)
		if !result.RouteExact || !result.ProviderExact ||
			!result.ModelExact || !result.CachePolicyExact ||
			!resolverExact {
			evaluation.contractFailures++
		}
		if len(result.HTTPStatuses) > 0 || result.TransportErrors > 0 ||
			result.RuntimeErrorClass != "" {
			evaluation.runtimeFailures++
		}
		evaluation.duplicateSideEffectFailures +=
			result.DuplicateSideEffectExecutions
		if result.CompletionCalls > 0 {
			anyCompletion = true
			if !result.CostObserved {
				allCostObserved = false
				evaluation.costFailures++
			}
		}
		if math.IsNaN(result.CostUSD) || math.IsInf(result.CostUSD, 0) ||
			result.CostUSD < 0 {
			allCostObserved = false
			evaluation.costFailures++
			continue
		}
		evaluation.reportedCostUSD += result.CostUSD
	}
	if complete && len(results) != scheduled {
		evaluation.contractFailures++
	}
	expectedScheduled :=
		len(koeQualificationWorkloadNames) * cfg.repetitions * 2
	if complete && scheduled != expectedScheduled {
		evaluation.contractFailures++
	}
	if complete && !cfg.smoke &&
		(cfg.repetitions < koeQualificationDefaultRepetitions ||
			cfg.repetitions > koeQualificationMaxRepetitions) {
		evaluation.contractFailures++
	}
	if evaluation.reportedCostUSD > cfg.maxCostUSD {
		evaluation.costFailures++
	}
	evaluation.costObserved = anyCompletion && allCostObserved
	if !complete {
		return evaluation
	}

	if cfg.smoke {
		evaluation.behaviorFailures =
			evaluateKoeQualificationSmokeBehavior(results, scheduled)
	} else {
		evaluation.behaviorFailures =
			evaluateKoeQualificationBehaviorGate(summaries)
	}
	correctnessPassed := len(evaluation.behaviorFailures) == 0 &&
		evaluation.contractFailures == 0 &&
		evaluation.runtimeFailures == 0 &&
		evaluation.duplicateSideEffectFailures == 0 &&
		evaluation.costFailures == 0
	evaluation.correctnessPassed = koeQualificationBoolPointer(correctnessPassed)

	if cfg.smoke {
		evaluation.gatePassed = koeQualificationBoolPointer(correctnessPassed)
		return evaluation
	}
	evaluation.performance, evaluation.performanceFailures =
		evaluateKoeQualificationPerformance(results, summaries)
	performancePassed := len(evaluation.performanceFailures) == 0
	evaluation.performancePassed = koeQualificationBoolPointer(performancePassed)
	evaluation.gatePassed =
		koeQualificationBoolPointer(correctnessPassed && performancePassed)
	return evaluation
}

func evaluateKoeQualificationSmokeBehavior(
	results []koeQualificationRunReport,
	scheduled int,
) []string {
	failures := make(map[string]bool)
	if len(results) != scheduled {
		failures["smoke:incomplete_sample"] = true
	}
	for _, result := range results {
		if !result.TaskSuccess {
			failures["smoke:run_task_failure"] = true
		}
		if !result.ToolCorrectness {
			failures["smoke:run_tool_failure"] = true
		}
	}
	return sortedKoeQualificationClasses(failures)
}

func evaluateKoeQualificationPerformance(
	results []koeQualificationRunReport,
	summaries []koeQualificationCellSummary,
) (*koeQualificationPerformance, []string) {
	performance := &koeQualificationPerformance{}
	failures := make(map[string]bool)
	var fastTotals, fullTotals []int64
	var fastCost, fullCost float64
	var fastSuccesses, fullSuccesses int
	for _, result := range results {
		switch result.Lane {
		case koeQualificationFastLane:
			fastTotals = append(fastTotals, result.TotalMillis)
			fastCost += result.CostUSD
			fastSuccesses += boolInt(result.TaskSuccess)
		case koeQualificationSonnetReferenceLane:
			fullTotals = append(fullTotals, result.TotalMillis)
			fullCost += result.CostUSD
			fullSuccesses += boolInt(result.TaskSuccess)
		}
	}
	if len(fastTotals) == 0 || len(fullTotals) == 0 {
		failures["performance:aggregate_sample_missing"] = true
	} else {
		performance.LunaTotalP50Millis =
			koeQualificationPercentile(fastTotals, 0.50)
		performance.SonnetTotalP50Millis =
			koeQualificationPercentile(fullTotals, 0.50)
		performance.LunaTotalP95Millis =
			koeQualificationPercentile(fastTotals, 0.95)
		performance.SonnetTotalP95Millis =
			koeQualificationPercentile(fullTotals, 0.95)
		if float64(performance.LunaTotalP50Millis) >
			0.75*float64(performance.SonnetTotalP50Millis) {
			failures["performance:aggregate_p50_not_25pct_faster"] = true
		}
		if float64(performance.LunaTotalP95Millis) >
			0.80*float64(performance.SonnetTotalP95Millis) {
			failures["performance:aggregate_p95_not_20pct_faster"] = true
		}
	}

	type paired struct {
		fast *koeQualificationCellSummary
		full *koeQualificationCellSummary
	}
	cells := make(map[string]*paired)
	for index := range summaries {
		summary := &summaries[index]
		pair := cells[summary.Workload]
		if pair == nil {
			pair = &paired{}
			cells[summary.Workload] = pair
		}
		if summary.Lane == koeQualificationFastLane {
			pair.fast = summary
		} else if summary.Lane == koeQualificationSonnetReferenceLane {
			pair.full = summary
		}
	}
	for _, workload := range koeQualificationWorkloadNames {
		pair := cells[workload]
		if pair == nil || pair.fast == nil || pair.full == nil {
			failures["performance:workload_sample_missing"] = true
			continue
		}
		performance.ComparedWorkloads++
		if pair.fast.TotalP95Millis < pair.full.TotalP95Millis {
			performance.LunaP95FasterWorkloads++
		}
	}
	if performance.LunaP95FasterWorkloads < 5 {
		failures["performance:p95_faster_workloads_below_5"] = true
	}

	if fastSuccesses == 0 || fullSuccesses == 0 {
		failures["performance:cost_per_success_sample_missing"] = true
	} else {
		fastPerSuccess := fastCost / float64(fastSuccesses)
		fullPerSuccess := fullCost / float64(fullSuccesses)
		performance.LunaCostPerSuccessfulTask = &fastPerSuccess
		performance.SonnetCostPerSuccessfulTask = &fullPerSuccess
		if fastPerSuccess > fullPerSuccess {
			failures["performance:cost_per_success_not_lower"] = true
		}
	}
	return performance, sortedKoeQualificationClasses(failures)
}

func sortedKoeQualificationClasses(classes map[string]bool) []string {
	out := make([]string, 0, len(classes))
	for class := range classes {
		out = append(out, class)
	}
	sort.Strings(out)
	return out
}

func koeQualificationBoolPointer(value bool) *bool {
	return &value
}

func evaluateKoeQualificationBehaviorGate(
	summaries []koeQualificationCellSummary,
) []string {
	type paired struct {
		fast *koeQualificationCellSummary
		full *koeQualificationCellSummary
	}
	cells := make(map[string]*paired)
	for index := range summaries {
		summary := &summaries[index]
		pair := cells[summary.Workload]
		if pair == nil {
			pair = &paired{}
			cells[summary.Workload] = pair
		}
		if summary.Lane == koeQualificationFastLane {
			pair.fast = summary
		} else if summary.Lane == koeQualificationSonnetReferenceLane {
			pair.full = summary
		}
	}

	var failures []string
	var fastRuns, fullRuns int
	var fastTasks, fullTasks float64
	var fastTools, fullTools float64
	for _, workload := range koeQualificationWorkloadNames {
		pair := cells[workload]
		if pair == nil || pair.fast == nil || pair.full == nil {
			failures = append(failures, workload+":missing_lane")
			continue
		}
		if pair.fast.TaskSuccessRate < 0.90 {
			failures = append(failures, workload+":fast_task_below_90pct")
		}
		if pair.fast.ToolCorrectnessRate < 0.90 {
			failures = append(failures, workload+":fast_tool_below_90pct")
		}
		if pair.fast.TaskSuccessRate+0.10 < pair.full.TaskSuccessRate {
			failures = append(failures, workload+":task_gap_above_10pct")
		}
		if pair.fast.ToolCorrectnessRate+0.10 < pair.full.ToolCorrectnessRate {
			failures = append(failures, workload+":tool_gap_above_10pct")
		}
		fastRuns += pair.fast.Runs
		fullRuns += pair.full.Runs
		fastTasks += pair.fast.TaskSuccessRate * float64(pair.fast.Runs)
		fullTasks += pair.full.TaskSuccessRate * float64(pair.full.Runs)
		fastTools += pair.fast.ToolCorrectnessRate * float64(pair.fast.Runs)
		fullTools += pair.full.ToolCorrectnessRate * float64(pair.full.Runs)
	}
	if fastRuns > 0 && fullRuns > 0 {
		fastTaskRate := fastTasks / float64(fastRuns)
		fullTaskRate := fullTasks / float64(fullRuns)
		fastToolRate := fastTools / float64(fastRuns)
		fullToolRate := fullTools / float64(fullRuns)
		if fastTaskRate+0.03 < fullTaskRate {
			failures = append(failures, "global:task_gap_above_3pct")
		}
		if fastToolRate+0.03 < fullToolRate {
			failures = append(failures, "global:tool_gap_above_3pct")
		}
	}
	return failures
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func koeQualificationPercentile(values []int64, percentile float64) int64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]int64(nil), values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	index := int(math.Ceil(percentile*float64(len(sorted)))) - 1
	if index < 0 {
		index = 0
	}
	if index >= len(sorted) {
		index = len(sorted) - 1
	}
	return sorted[index]
}

func writeKoeQualificationReport(
	path string,
	report koeQualificationReport,
) error {
	encoded, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	if err := validateKoeQualificationReportKeys(encoded); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	tmp, err := os.CreateTemp(
		filepath.Dir(path),
		"."+filepath.Base(path)+".*.tmp",
	)
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}
	if _, err := tmp.Write(encoded); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return nil
}

func validateKoeQualificationReportKeys(encoded []byte) error {
	var document any
	if err := json.Unmarshal(encoded, &document); err != nil {
		return err
	}
	forbidden := map[string]bool{
		"prompt":         true,
		"output":         true,
		"args":           true,
		"body":           true,
		"profile_id":     true,
		"api_key":        true,
		"endpoint":       true,
		"tool_arguments": true,
		"response_text":  true,
		"result_text":    true,
	}
	var walk func(any) error
	walk = func(value any) error {
		switch item := value.(type) {
		case map[string]any:
			for key, child := range item {
				if forbidden[key] {
					return fmt.Errorf("forbidden content-bearing report key %q", key)
				}
				if err := walk(child); err != nil {
					return err
				}
			}
		case []any:
			for _, child := range item {
				if err := walk(child); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return walk(document)
}
