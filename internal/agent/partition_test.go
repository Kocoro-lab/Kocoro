package agent

import (
	"context"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

// readOnlyStub always classifies as read-only.
type readOnlyStub struct{ name string }

func (t *readOnlyStub) Info() ToolInfo                                  { return ToolInfo{Name: t.name} }
func (t *readOnlyStub) Run(context.Context, string) (ToolResult, error) { return ToolResult{}, nil }
func (t *readOnlyStub) RequiresApproval() bool                          { return false }
func (t *readOnlyStub) IsReadOnlyCall(string) bool                      { return true }

// writeStub always classifies as non-read-only.
type writeStub struct{ name string }

func (t *writeStub) Info() ToolInfo                                  { return ToolInfo{Name: t.name} }
func (t *writeStub) Run(context.Context, string) (ToolResult, error) { return ToolResult{}, nil }
func (t *writeStub) RequiresApproval() bool                          { return false }
func (t *writeStub) IsReadOnlyCall(string) bool                      { return false }

// plainStub does NOT implement ReadOnlyChecker — should default to non-read-only.
type plainStub struct{ name string }

func (t *plainStub) Info() ToolInfo                                  { return ToolInfo{Name: t.name} }
func (t *plainStub) Run(context.Context, string) (ToolResult, error) { return ToolResult{}, nil }
func (t *plainStub) RequiresApproval() bool                          { return false }

// concurrentStub implements both ReadOnlyChecker and ConcurrencySafeChecker so
// the two dimensions can diverge in tests (e.g. non-read-only but
// concurrency-safe, mirroring a future "git status" bash call).
type concurrentStub struct {
	name            string
	readOnly        bool
	concurrencySafe bool
}

func (t *concurrentStub) Info() ToolInfo                                  { return ToolInfo{Name: t.name} }
func (t *concurrentStub) Run(context.Context, string) (ToolResult, error) { return ToolResult{}, nil }
func (t *concurrentStub) RequiresApproval() bool                          { return false }
func (t *concurrentStub) IsReadOnlyCall(string) bool                      { return t.readOnly }
func (t *concurrentStub) IsConcurrencySafeCall(string) bool               { return t.concurrencySafe }

func ac(tool Tool, index int) approvedToolCall {
	return approvedToolCall{
		index:   index,
		fc:      client.FunctionCall{Name: tool.Info().Name},
		tool:    tool,
		argsStr: "{}",
	}
}

func TestPartition_MixedReadWrite(t *testing.T) {
	r := &readOnlyStub{name: "r"}
	w := &writeStub{name: "w"}
	batches := partitionToolCalls([]approvedToolCall{ac(r, 0), ac(r, 1), ac(w, 2), ac(r, 3)})
	if len(batches) != 3 {
		t.Fatalf("expected 3 batches, got %d", len(batches))
	}
	if len(batches[0]) != 2 {
		t.Errorf("batch 0: expected 2 calls, got %d", len(batches[0]))
	}
	if len(batches[1]) != 1 {
		t.Errorf("batch 1: expected 1 call, got %d", len(batches[1]))
	}
	if len(batches[2]) != 1 {
		t.Errorf("batch 2: expected 1 call, got %d", len(batches[2]))
	}
}

func TestPartition_AllWrites(t *testing.T) {
	w := &writeStub{name: "w"}
	batches := partitionToolCalls([]approvedToolCall{ac(w, 0), ac(w, 1)})
	if len(batches) != 2 {
		t.Fatalf("expected 2 batches, got %d", len(batches))
	}
}

func TestPartition_AllReads(t *testing.T) {
	r := &readOnlyStub{name: "r"}
	batches := partitionToolCalls([]approvedToolCall{ac(r, 0), ac(r, 1), ac(r, 2)})
	if len(batches) != 1 {
		t.Fatalf("expected 1 batch, got %d", len(batches))
	}
	if len(batches[0]) != 3 {
		t.Errorf("expected 3 calls, got %d", len(batches[0]))
	}
}

func TestPartition_SingleWrite(t *testing.T) {
	w := &writeStub{name: "w"}
	batches := partitionToolCalls([]approvedToolCall{ac(w, 0)})
	if len(batches) != 1 || len(batches[0]) != 1 {
		t.Fatalf("expected [[w]], got %v", batches)
	}
}

func TestPartition_NoReadOnlyChecker_TreatedAsWrite(t *testing.T) {
	p := &plainStub{name: "mcp_tool"}
	r := &readOnlyStub{name: "r"}
	batches := partitionToolCalls([]approvedToolCall{ac(r, 0), ac(p, 1), ac(r, 2)})
	if len(batches) != 3 {
		t.Fatalf("expected 3 batches (plain treated as write), got %d", len(batches))
	}
}

func TestPartition_Empty(t *testing.T) {
	batches := partitionToolCalls(nil)
	if len(batches) != 0 {
		t.Fatalf("expected 0 batches, got %d", len(batches))
	}
}

// TestPartition_ConcurrencySafeOverridesReadOnly verifies that a tool
// implementing ConcurrencySafeChecker can join a concurrent batch even when
// its IsReadOnlyCall returns false. This is the primary motivation for the
// new interface — letting BashTool participate in concurrent batches for
// commands proven safe by static analysis without claiming read-only status
// (which has cache-relevant side effects).
func TestPartition_ConcurrencySafeOverridesReadOnly(t *testing.T) {
	r1 := &readOnlyStub{name: "file_read"}
	// non-read-only but concurrency-safe — e.g. a future "bash: git status".
	cs := &concurrentStub{name: "bash", readOnly: false, concurrencySafe: true}
	r2 := &readOnlyStub{name: "grep"}
	batches := partitionToolCalls([]approvedToolCall{ac(r1, 0), ac(cs, 1), ac(r2, 2)})
	if len(batches) != 1 {
		t.Fatalf("expected 1 concurrent batch, got %d", len(batches))
	}
	if len(batches[0]) != 3 {
		t.Fatalf("expected 3 calls in the batch, got %d", len(batches[0]))
	}
}

// TestPartition_FallbackToReadOnly verifies the absence of
// ConcurrencySafeChecker preserves the historical isReadOnly-only behavior:
// non-read-only tools each get their own size-1 batch. This guards the
// "zero behavior change for existing tools" invariant of Task 1.
func TestPartition_FallbackToReadOnly(t *testing.T) {
	r1 := &readOnlyStub{name: "file_read"}
	w := &writeStub{name: "bash"}
	r2 := &readOnlyStub{name: "file_read"}
	batches := partitionToolCalls([]approvedToolCall{ac(r1, 0), ac(w, 1), ac(r2, 2)})
	if len(batches) != 3 {
		t.Fatalf("expected 3 sequential batches, got %d", len(batches))
	}
}
