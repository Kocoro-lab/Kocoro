package agenttypes

import "errors"

// CancelReason classifies why a run was cancelled. The reason flows through
// context.WithCancelCause and drives different cleanup paths in the agent
// loop and daemon finalizer:
//
//   - UserCancel: emit "cancelled" event; optionally restore last user
//     message to input box. Partial assistant text preserved.
//   - Interrupt: user submitted while a cancel-tolerant tool was running.
//     Queued message follows immediately, so suppress the "Request
//     interrupted by user" sentinel. BlockingTool tools finish; CancelableTool
//     tools abort.
//   - Background: detach to background; no auto-restore, no sentinel.
//   - IdleTimeout: watchdog killed the loop; emit "timeout" event.
//   - SiblingError: a fatal error elsewhere forced shutdown.
type CancelReason int

const (
	ReasonUnknown      CancelReason = 0
	ReasonUserCancel   CancelReason = 1
	ReasonInterrupt    CancelReason = 2
	ReasonBackground   CancelReason = 3
	ReasonIdleTimeout  CancelReason = 4
	ReasonSiblingError CancelReason = 5
)

// String returns a stable, kebab-form identifier suitable for wire protocols
// and audit logs. Wire callers must check string equality, not numeric value.
func (r CancelReason) String() string {
	switch r {
	case ReasonUserCancel:
		return "user_cancel"
	case ReasonInterrupt:
		return "interrupt"
	case ReasonBackground:
		return "background"
	case ReasonIdleTimeout:
		return "idle_timeout"
	case ReasonSiblingError:
		return "sibling_error"
	}
	return "unknown"
}

// ParseCancelReason returns the matching enum value, or (ReasonUnknown, false)
// for unrecognized input. The empty string maps to ReasonUserCancel, matching
// the daemon's "default reason on /cancel" behavior.
func ParseCancelReason(s string) (CancelReason, bool) {
	switch s {
	case "", "user_cancel":
		return ReasonUserCancel, true
	case "interrupt":
		return ReasonInterrupt, true
	case "background":
		return ReasonBackground, true
	case "idle_timeout":
		return ReasonIdleTimeout, true
	case "sibling_error":
		return ReasonSiblingError, true
	}
	return ReasonUnknown, false
}

// CancelError wraps a CancelReason so it can travel through
// context.WithCancelCause. Pattern: cancel(NewCancelError(ReasonUserCancel)).
// Recovery side: r, ok := ExtractReason(context.Cause(ctx)).
type CancelError struct {
	Reason CancelReason
	Msg    string
}

// NewCancelError returns a CancelError carrying the given reason. The Msg
// field defaults to the reason's String() form.
func NewCancelError(r CancelReason) *CancelError {
	return &CancelError{Reason: r, Msg: r.String()}
}

// Error returns Msg if set, otherwise the reason string.
func (e *CancelError) Error() string {
	if e.Msg != "" {
		return e.Msg
	}
	return e.Reason.String()
}

// ExtractReason returns the CancelReason encoded in err, if any. Returns
// (ReasonUnknown, false) when err is nil, not a *CancelError, or any wrapping
// chain that does not lead to *CancelError.
func ExtractReason(err error) (CancelReason, bool) {
	if err == nil {
		return ReasonUnknown, false
	}
	var ce *CancelError
	if errors.As(err, &ce) {
		return ce.Reason, true
	}
	return ReasonUnknown, false
}
