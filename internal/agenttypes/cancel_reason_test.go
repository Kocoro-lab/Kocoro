package agenttypes

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestCancelReason_StringStable(t *testing.T) {
	cases := []struct {
		r    CancelReason
		want string
	}{
		{ReasonUnknown, "unknown"},
		{ReasonUserCancel, "user_cancel"},
		{ReasonInterrupt, "interrupt"},
		{ReasonBackground, "background"},
		{ReasonIdleTimeout, "idle_timeout"},
		{ReasonSiblingError, "sibling_error"},
	}
	for _, c := range cases {
		if got := c.r.String(); got != c.want {
			t.Errorf("reason=%d: want %q, got %q", c.r, c.want, got)
		}
	}
}

func TestParseCancelReason(t *testing.T) {
	cases := []struct {
		in      string
		want    CancelReason
		wantOK  bool
	}{
		{"", ReasonUserCancel, true},
		{"user_cancel", ReasonUserCancel, true},
		{"interrupt", ReasonInterrupt, true},
		{"background", ReasonBackground, true},
		{"idle_timeout", ReasonIdleTimeout, true},
		{"sibling_error", ReasonSiblingError, true},
		{"USER_CANCEL", ReasonUnknown, false}, // case-sensitive
		{"garbage", ReasonUnknown, false},
	}
	for _, c := range cases {
		got, ok := ParseCancelReason(c.in)
		if got != c.want || ok != c.wantOK {
			t.Errorf("ParseCancelReason(%q) = (%v, %v), want (%v, %v)", c.in, got, ok, c.want, c.wantOK)
		}
	}
}

func TestCancelError_RoundTripViaCause(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(NewCancelError(ReasonUserCancel))

	<-ctx.Done()
	cause := context.Cause(ctx)
	r, ok := ExtractReason(cause)
	if !ok {
		t.Fatalf("ExtractReason returned not-ok for cause %v", cause)
	}
	if r != ReasonUserCancel {
		t.Errorf("round-trip reason: want %v, got %v", ReasonUserCancel, r)
	}
}

func TestExtractReason_NilError(t *testing.T) {
	if r, ok := ExtractReason(nil); ok || r != ReasonUnknown {
		t.Errorf("nil err: want (Unknown, false), got (%v, %v)", r, ok)
	}
}

func TestExtractReason_UnknownErrorReturnsFalse(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(errors.New("some other error"))
	cause := context.Cause(ctx)
	if _, ok := ExtractReason(cause); ok {
		t.Error("ExtractReason on unrelated error should return false")
	}
}

func TestExtractReason_WrappedError(t *testing.T) {
	wrapped := fmt.Errorf("operation failed: %w", NewCancelError(ReasonInterrupt))
	r, ok := ExtractReason(wrapped)
	if !ok || r != ReasonInterrupt {
		t.Errorf("wrapped extract: want (Interrupt, true), got (%v, %v)", r, ok)
	}
}

func TestCancelError_ErrorWithCustomMsg(t *testing.T) {
	e := &CancelError{Reason: ReasonInterrupt, Msg: "user sent another message"}
	if got := e.Error(); got != "user sent another message" {
		t.Errorf("custom Msg: want preserved, got %q", got)
	}
}

func TestCancelError_ErrorDefaultMsg(t *testing.T) {
	e := NewCancelError(ReasonUserCancel)
	if got := e.Error(); got != "user_cancel" {
		t.Errorf("default Msg: want reason string, got %q", got)
	}
}
