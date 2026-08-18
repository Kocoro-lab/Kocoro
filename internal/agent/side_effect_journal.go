package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

var (
	// ErrSideEffectOutcomeUnknown is returned after dispatch when neither the
	// tool nor its journal can prove whether the external mutation committed.
	// Callers must not automatically retry the action.
	ErrSideEffectOutcomeUnknown = errors.New("agent: side-effect execution outcome unknown")
	// ErrSideEffectJournalUnavailable is returned before dispatch when the
	// durable execution boundary cannot be established. The tool did not run.
	ErrSideEffectJournalUnavailable = errors.New("agent: side-effect execution journal unavailable")
)

// SideEffectExecution describes a material mutation without persisting its raw
// arguments. The journal implementation owns run/attempt identity and durable
// storage; ToolUseID provides the transcript join key.
type SideEffectExecution struct {
	ToolUseID       string
	ToolName        string
	ArgumentsSHA256 string
}

// PreparedSideEffectExecution is the opaque identity minted by the durable
// journal. IdempotencyKey is only made available to tools through context;
// generic providers do not become idempotent merely because the key exists.
type PreparedSideEffectExecution struct {
	ExecutionID    string
	IdempotencyKey string
}

// SideEffectExecutionJournal owns the dispatch state machine around a material
// Tool.Run call. Prepare may stage its no-dispatch identity in the same
// transaction as the mandatory pre-dispatch checkpoint; MarkDispatching must
// be durable before returning nil. Terminal methods must be durable before
// returning nil. Implementations must not persist raw tool arguments or results.
type SideEffectExecutionJournal interface {
	Prepare(context.Context, SideEffectExecution) (PreparedSideEffectExecution, error)
	MarkDispatching(context.Context, string) error
	MarkCommitted(context.Context, string, string) error
	MarkFailedNoEffect(context.Context, string, string) error
	MarkAbandoned(context.Context, string, string) error
	MarkOutcomeUnknown(context.Context, string, string) error
}

// SideEffectExecutionContext is the opaque per-dispatch identity exposed to a
// capable tool. A tool/provider may use IdempotencyKey if it has a real native
// idempotency contract; otherwise it should ignore it.
type SideEffectExecutionContext struct {
	ExecutionID    string
	IdempotencyKey string
}

type sideEffectExecutionContextKey struct{}

// CheckpointReason identifies a checkpoint whose persistence rules differ
// from an ordinary iteration-tail save.
type CheckpointReason string

const (
	// CheckpointReasonSideEffectPrepared is emitted before MarkDispatching.
	// The current assistant tool call is durable, but results from earlier
	// batches in the same model response have not entered the transcript yet.
	// Callers must not bulk-promote committed executions at this boundary.
	CheckpointReasonSideEffectPrepared CheckpointReason = "side_effect_prepared"
)

type checkpointReasonContextKey struct{}

func withSideEffectExecution(ctx context.Context, prepared PreparedSideEffectExecution) context.Context {
	return context.WithValue(ctx, sideEffectExecutionContextKey{}, SideEffectExecutionContext{
		ExecutionID:    prepared.ExecutionID,
		IdempotencyKey: prepared.IdempotencyKey,
	})
}

// SideEffectExecutionFromContext returns the opaque durable execution identity
// for the current material Tool.Run call.
func SideEffectExecutionFromContext(ctx context.Context) (SideEffectExecutionContext, bool) {
	value, ok := ctx.Value(sideEffectExecutionContextKey{}).(SideEffectExecutionContext)
	return value, ok && value.ExecutionID != "" && value.IdempotencyKey != ""
}

func withCheckpointReason(ctx context.Context, reason CheckpointReason) context.Context {
	return context.WithValue(ctx, checkpointReasonContextKey{}, reason)
}

// CheckpointReasonFromContext lets a persistence callback distinguish the
// pre-dispatch side-effect boundary from an ordinary completed-batch save.
func CheckpointReasonFromContext(ctx context.Context) CheckpointReason {
	reason, _ := ctx.Value(checkpointReasonContextKey{}).(CheckpointReason)
	return reason
}

func hasMaterialSideEffect(tool Tool, args string) bool {
	if checker, ok := tool.(MaterialSideEffectChecker); ok {
		return checker.HasMaterialSideEffect(args)
	}
	if checker, ok := tool.(ReadOnlyChecker); ok {
		return !checker.IsReadOnlyCall(args)
	}
	return true
}

func sideEffectArgumentsDigest(args string) string {
	digest := sha256.Sum256([]byte(normalizeJSON(json.RawMessage(args))))
	return hex.EncodeToString(digest[:])
}

func sideEffectResultDigest(result ToolResult, runErr error) string {
	payload := result.Content
	if runErr != nil {
		payload += "\x00error"
	}
	if result.IsError {
		payload += "\x00tool_error"
	}
	digest := sha256.Sum256([]byte(payload))
	return hex.EncodeToString(digest[:])
}

func validatePreparedSideEffect(prepared PreparedSideEffectExecution) error {
	if prepared.ExecutionID == "" || prepared.IdempotencyKey == "" {
		return fmt.Errorf("journal returned incomplete prepared execution identity")
	}
	return nil
}

func sideEffectJournalUnavailableResult(toolName string) ToolResult {
	return BusinessError(fmt.Sprintf(
		"%s was not executed because its durable side-effect journal was unavailable", toolName,
	))
}

func sideEffectOutcomeUnknownResult(toolName, detail string) ToolResult {
	content := fmt.Sprintf(
		"%s may have changed external state, but its outcome could not be confirmed. "+
			"It was NOT retried automatically, and a byte-identical retry is blocked locally for the rest of this turn. "+
			"Explain the situation to the user in their language and suggest verifying directly in the external system before any retry.",
		toolName,
	)
	if detail = strings.TrimSpace(detail); detail != "" {
		content += "\n\nTool detail before the outcome became uncertain:\n" + detail
	}
	return BusinessError(content)
}

func sideEffectOutcomeDetail(result ToolResult, runErr error) string {
	detail := strings.TrimSpace(result.Content)
	if runErr != nil {
		if detail != "" {
			detail += "\n"
		}
		detail += "tool error: " + runErr.Error()
	}
	return detail
}

func wrapSideEffectExecutionError(sentinel error, toolName string, cause error) error {
	if cause == nil {
		return fmt.Errorf("%w: tool=%s", sentinel, toolName)
	}
	return fmt.Errorf("%w: tool=%s: %v", sentinel, toolName, cause)
}
