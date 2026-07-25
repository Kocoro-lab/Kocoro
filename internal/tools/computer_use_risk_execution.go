package tools

import (
	"context"
	"errors"
)

var (
	ErrConsequentialRiskExecutionMissingV1  = errors.New("consequential risk execution context is missing")
	ErrConsequentialRiskExecutionMismatchV1 = errors.New("consequential risk execution context mismatch")
)

type consequentialRiskExecutionKeyV1 struct{}

// ConsequentialRiskGrantConsumerV1 burns the daemon broker's one-shot grant.
// The full rederived draft is supplied so approval binds destination detail as
// well as the target digest. Implementations must not log the draft.
type ConsequentialRiskGrantConsumerV1 func(ConsequentialRiskDraftV1) error

type consequentialRiskExecutionV1 struct {
	IntentID     string
	RequestID    string
	TargetDigest string
	approved     ConsequentialRiskDraftV1
	consume      ConsequentialRiskGrantConsumerV1
}

func ContextWithConsequentialRiskExecutionV1(
	ctx context.Context,
	intentID string,
	approved ConsequentialRiskDraftV1,
	consume ConsequentialRiskGrantConsumerV1,
) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	value := consequentialRiskExecutionV1{
		IntentID: intentID, RequestID: approved.RequestID,
		TargetDigest: approved.Target.TargetDigest,
		approved:     approved, consume: consume,
	}
	return context.WithValue(ctx, consequentialRiskExecutionKeyV1{}, value)
}

func consequentialRiskExecutionFromContextV1(ctx context.Context) (consequentialRiskExecutionV1, bool) {
	if ctx == nil {
		return consequentialRiskExecutionV1{}, false
	}
	value, ok := ctx.Value(consequentialRiskExecutionKeyV1{}).(consequentialRiskExecutionV1)
	return value, ok && value.IntentID != "" && value.RequestID != "" && value.TargetDigest != "" && value.consume != nil
}

func validateConsequentialRiskExecutionV1(
	ctx context.Context,
	result ConsequentialRiskPreflightResultV1,
) (consequentialRiskExecutionV1, error) {
	execution, present := consequentialRiskExecutionFromContextV1(ctx)
	switch result.Status {
	case ConsequentialRiskPreflightNoneV1:
		if present {
			return consequentialRiskExecutionV1{}, ErrConsequentialRiskExecutionMismatchV1
		}
		return consequentialRiskExecutionV1{}, nil
	case ConsequentialRiskPreflightBlockedV1:
		return consequentialRiskExecutionV1{}, ErrConsequentialRiskExecutionMismatchV1
	case ConsequentialRiskPreflightRequiredV1:
		if !present || result.Draft == nil {
			return consequentialRiskExecutionV1{}, ErrConsequentialRiskExecutionMissingV1
		}
		if execution.RequestID != result.Draft.RequestID ||
			execution.TargetDigest != result.Draft.Target.TargetDigest ||
			!EqualConsequentialRiskDraftV1(execution.approved, *result.Draft) {
			return consequentialRiskExecutionV1{}, ErrConsequentialRiskExecutionMismatchV1
		}
		return execution, nil
	default:
		return consequentialRiskExecutionV1{}, ErrConsequentialRiskExecutionMismatchV1
	}
}
