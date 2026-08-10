package agent

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
)

// RunTraceHandler is an optional, read-only observation interface for the
// canonical execution trajectory of one AgentLoop run. Events are emitted by
// the loop goroutine in provider order; handlers must return quickly.
type RunTraceHandler interface {
	OnRunTrace(event RunTraceEvent)
}

type RunTraceEventType string

const (
	RunTraceEventModelResponse RunTraceEventType = "model_response"
	RunTraceEventToolOutcome   RunTraceEventType = "tool_outcome"
	RunTraceEventRetry         RunTraceEventType = "retry"
	RunTraceEventNudge         RunTraceEventType = "nudge"
	RunTraceEventCompaction    RunTraceEventType = "compaction"
	RunTraceEventTerminal      RunTraceEventType = "terminal"
)

type RunTraceEvent struct {
	Seq        int64                  `json:"seq"`
	Iteration  int                    `json:"iteration"`
	Type       RunTraceEventType      `json:"type"`
	Model      *RunTraceModelResponse `json:"model,omitempty"`
	Tool       *RunTraceToolOutcome   `json:"tool,omitempty"`
	Retry      *RunTraceRetry         `json:"retry,omitempty"`
	Nudge      *RunTraceNudge         `json:"nudge,omitempty"`
	Compaction *RunTraceCompaction    `json:"compaction,omitempty"`
	Terminal   *RunTraceTerminal      `json:"terminal,omitempty"`
}

type RunTraceModelResponse struct {
	Attempt      int    `json:"attempt"`
	Model        string `json:"model,omitempty"`
	FinishReason string `json:"finish_reason,omitempty"`
	ToolCalls    int    `json:"tool_calls"`
	Cached       bool   `json:"cached"`
	InputTokens  int    `json:"input_tokens"`
	OutputTokens int    `json:"output_tokens"`
	TotalTokens  int    `json:"total_tokens"`
}

type RunTraceRetry struct {
	Kind           string `json:"kind"`
	Attempt        int    `json:"attempt"`
	ReasonCategory string `json:"reason_category,omitempty"`
	DelayMillis    int64  `json:"delay_ms,omitempty"`
}

type RunTraceNudge struct {
	Kind   string `json:"kind"`
	Action string `json:"action"`
}

type RunTraceCompaction struct {
	Phase           string `json:"phase"`
	Status          string `json:"status"`
	Applied         bool   `json:"applied"`
	MessagesDropped int    `json:"messages_dropped,omitempty"`
}

type RunTraceTerminal struct {
	Partial        bool   `json:"partial"`
	FailureCode    string `json:"failure_code,omitempty"`
	LastTool       string `json:"last_tool,omitempty"`
	RetryCount     int    `json:"retry_count"`
	IterationCount int    `json:"iteration_count"`
}

type RunTraceToolOutcome struct {
	Ordinal              int           `json:"ordinal"`
	Name                 string        `json:"name"`
	ArgumentsHMACSHA256  string        `json:"args_hmac_sha256,omitempty"`
	ResultHMACSHA256     string        `json:"result_hmac_sha256,omitempty"`
	ModelBatchID         int           `json:"model_batch_id"`
	ModelBatchIndex      int           `json:"model_batch_index"`
	ModelBatchSize       int           `json:"model_batch_size"`
	ExecutionBatchIndex  *int          `json:"execution_batch_index"`
	ExecutionBatchSize   int           `json:"execution_batch_size"`
	ExecutionParallel    bool          `json:"execution_parallel"`
	MaxConcurrency       int           `json:"max_concurrency"`
	Executed             bool          `json:"executed"`
	Outcome              string        `json:"outcome"`
	ErrorCategory        ErrorCategory `json:"error_category,omitempty"`
	Retryable            bool          `json:"retryable"`
	DurationMilliseconds int64         `json:"duration_ms"`
}

// runTraceEmitter owns observation-only state for one AgentLoop Run. The
// random HMAC key is deliberately never serialized: traces can correlate equal
// payloads within one run without exposing stable low-entropy hashes across
// release artifacts.
type runTraceEmitter struct {
	handler            RunTraceHandler
	seq                int64
	iteration          int
	nextExecutionBatch int
	digestKey          []byte
}

func newRunTraceEmitter(handler any) *runTraceEmitter {
	rth, ok := handler.(RunTraceHandler)
	if !ok {
		return nil
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		key = nil // fail closed: emit no reversible/stable digest on RNG failure
	}
	return &runTraceEmitter{handler: rth, digestKey: key}
}

func (e *runTraceEmitter) emit(event RunTraceEvent) {
	if e == nil || e.handler == nil {
		return
	}
	if event.Iteration == 0 {
		event.Iteration = e.iteration
	}
	e.seq++
	event.Seq = e.seq
	e.handler.OnRunTrace(event)
}

func (e *runTraceEmitter) setIteration(iteration int) {
	if e != nil {
		e.iteration = iteration
	}
}

func (e *runTraceEmitter) nextBatchOrdinal() int {
	if e == nil {
		return 0
	}
	ordinal := e.nextExecutionBatch
	e.nextExecutionBatch++
	return ordinal
}

func (e *runTraceEmitter) digest(payload string) string {
	if e == nil || len(e.digestKey) == 0 {
		return ""
	}
	mac := hmac.New(sha256.New, e.digestKey)
	_, _ = mac.Write([]byte(payload))
	return hex.EncodeToString(mac.Sum(nil))
}

func (a *AgentLoop) emitRunTrace(event RunTraceEvent) {
	if a != nil && a.runTrace != nil {
		a.runTrace.emit(event)
	}
}

func (a *AgentLoop) emitAppliedCompaction(phase string, messagesDropped int) {
	a.emitRunTrace(RunTraceEvent{
		Type: RunTraceEventCompaction,
		Compaction: &RunTraceCompaction{
			Phase: phase, Status: "applied", Applied: true,
			MessagesDropped: messagesDropped,
		},
	})
}
