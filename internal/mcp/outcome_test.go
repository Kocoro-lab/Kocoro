package mcp

import (
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

func TestToolReplaySafe_AnnotationGating(t *testing.T) {
	cases := []struct {
		name string
		tool mcp.Tool
		want bool
	}{
		{"no annotations fails closed", mcp.Tool{Name: "send_message"}, false},
		{"read-only is safe", mcp.Tool{Name: "list", Annotations: mcp.ToolAnnotation{ReadOnlyHint: mcp.ToBoolPtr(true)}}, true},
		{"idempotent is safe", mcp.Tool{Name: "upsert", Annotations: mcp.ToolAnnotation{IdempotentHint: mcp.ToBoolPtr(true)}}, true},
		{"explicit false stays unsafe", mcp.Tool{Name: "send", Annotations: mcp.ToolAnnotation{ReadOnlyHint: mcp.ToBoolPtr(false), IdempotentHint: mcp.ToBoolPtr(false)}}, false},
	}
	for _, tc := range cases {
		if got := ToolReplaySafe(tc.tool); got != tc.want {
			t.Errorf("%s: ToolReplaySafe=%v, want %v", tc.name, got, tc.want)
		}
	}
}

// The wrap must preserve transport classification: an upper layer that
// probes the error with IsTransportError (e.g. the MCPTool retry gate) must
// still see the underlying dead-pipe chain through OutcomeUnknownError.
func TestOutcomeUnknownError_PreservesTransportClassification(t *testing.T) {
	inner := fmt.Errorf("tools/call failed: %w", io.EOF)
	wrapped := &OutcomeUnknownError{Server: "srv", Tool: "send_message", Err: inner}
	if !IsTransportError(wrapped) {
		t.Error("IsTransportError must classify through the OutcomeUnknownError wrap")
	}
	var ou *OutcomeUnknownError
	if !errors.As(error(wrapped), &ou) || ou.Tool != "send_message" {
		t.Error("errors.As must recover the OutcomeUnknownError")
	}
	if !errors.Is(wrapped, io.EOF) {
		t.Error("Unwrap chain must reach the underlying transport error")
	}
}

// Unsupervised inline path: a post-dispatch transport failure on a tool with
// no cached read-only/idempotent advertisement must NOT be re-dispatched —
// it surfaces as OutcomeUnknownError. The cache-driven predicate is what the
// unsupervised CallTool retry consults.
func TestReplaySafeFromCache_MissAndAnnotations(t *testing.T) {
	m := NewClientManager()
	if m.replaySafeFromCache("srv", "send_message") {
		t.Error("cache miss must be conservatively unsafe")
	}
	m.SeedToolCache("srv", []RemoteTool{
		{ServerName: "srv", Tool: mcp.Tool{Name: "send_message"}},
		{ServerName: "srv", Tool: mcp.Tool{Name: "list_messages", Annotations: mcp.ToolAnnotation{ReadOnlyHint: mcp.ToBoolPtr(true)}}},
	})
	if m.replaySafeFromCache("srv", "send_message") {
		t.Error("unannotated cached tool must be unsafe")
	}
	if !m.replaySafeFromCache("srv", "list_messages") {
		t.Error("read-only cached tool must be replay-safe")
	}
}
