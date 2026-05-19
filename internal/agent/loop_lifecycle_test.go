package agent

import (
	"encoding/json"
	"testing"
)

// recordedLifecycleCall captures a single OnUserMessageProcessing call so
// tests can assert on order, count, and per-message content without standing
// up a real WS client.
type recordedLifecycleCall struct {
	CloudMessageID  string
	IMStatusContext json.RawMessage
}

type fakeLifecycleEmitter struct {
	calls []recordedLifecycleCall
}

func (f *fakeLifecycleEmitter) OnUserMessageProcessing(cloudMessageID string, imStatusContext json.RawMessage) {
	f.calls = append(f.calls, recordedLifecycleCall{
		CloudMessageID:  cloudMessageID,
		IMStatusContext: imStatusContext,
	})
}

func newLoopForLifecycle(t *testing.T) *AgentLoop {
	t.Helper()
	reg := NewToolRegistry()
	return NewAgentLoop(nil, reg, "test", t.TempDir(), 1, 2000, 200, nil, nil, nil)
}

func TestAgentLoop_EmitDrainedLifecycle_PerMessage(t *testing.T) {
	loop := newLoopForLifecycle(t)
	fake := &fakeLifecycleEmitter{}
	loop.SetLifecycleEmitter(fake)

	ctx1 := json.RawMessage(`{"platform":"slack","message_ts":"1.1"}`)
	ctx2 := json.RawMessage(`{"platform":"slack","message_ts":"1.2"}`)
	drained := []InjectedMessage{
		{Text: "f1", CloudMessageID: "m1", IMStatusContext: ctx1},
		{Text: "f2", CloudMessageID: "m2", IMStatusContext: ctx2},
	}
	loop.emitDrainedLifecycle(drained)

	if len(fake.calls) != 2 {
		t.Fatalf("expected 2 processing emits, got %d", len(fake.calls))
	}
	if fake.calls[0].CloudMessageID != "m1" || fake.calls[1].CloudMessageID != "m2" {
		t.Errorf("emit order wrong: %+v", fake.calls)
	}
	if string(fake.calls[0].IMStatusContext) != string(ctx1) {
		t.Errorf("ctx1 mismatch: %s", fake.calls[0].IMStatusContext)
	}
	if string(fake.calls[1].IMStatusContext) != string(ctx2) {
		t.Errorf("ctx2 mismatch: %s", fake.calls[1].IMStatusContext)
	}
}

func TestAgentLoop_EmitDrainedLifecycle_SkipsNonIM(t *testing.T) {
	loop := newLoopForLifecycle(t)
	fake := &fakeLifecycleEmitter{}
	loop.SetLifecycleEmitter(fake)

	// Mix: only the second carries IM context.
	drained := []InjectedMessage{
		{Text: "from-tui"}, // empty IMStatusContext and CloudMessageID
		{Text: "from-slack", CloudMessageID: "m2", IMStatusContext: json.RawMessage(`{"platform":"slack"}`)},
		{Text: "missing-context", CloudMessageID: "m3"}, // empty context: also skipped
		{Text: "missing-id", IMStatusContext: json.RawMessage(`{}`)},
	}
	loop.emitDrainedLifecycle(drained)

	if len(fake.calls) != 1 {
		t.Fatalf("expected 1 processing emit, got %d (%+v)", len(fake.calls), fake.calls)
	}
	if fake.calls[0].CloudMessageID != "m2" {
		t.Errorf("expected m2, got %q", fake.calls[0].CloudMessageID)
	}
}

func TestAgentLoop_EmitDrainedLifecycle_NoEmitterNoPanic(t *testing.T) {
	loop := newLoopForLifecycle(t)
	// No SetLifecycleEmitter — emitter is nil.
	loop.emitDrainedLifecycle([]InjectedMessage{
		{CloudMessageID: "m1", IMStatusContext: json.RawMessage(`{}`)},
	})
	// Just verifying no panic; nothing to assert about absent calls.
}

func TestAgentLoop_EmitFirstTurnLifecycle_FiresOnce(t *testing.T) {
	loop := newLoopForLifecycle(t)
	fake := &fakeLifecycleEmitter{}
	loop.SetLifecycleEmitter(fake)

	rawCtx := json.RawMessage(`{"platform":"slack","message_ts":"1.0"}`)
	loop.SetFirstTurnLifecycle("primary-1", rawCtx)
	loop.emitFirstTurnLifecycle()
	loop.emitFirstTurnLifecycle() // second call must be a no-op (idempotent on the same run)

	if len(fake.calls) != 1 {
		t.Fatalf("expected exactly 1 first-turn emit, got %d", len(fake.calls))
	}
	if fake.calls[0].CloudMessageID != "primary-1" {
		t.Errorf("emit cloud id: got %q want primary-1", fake.calls[0].CloudMessageID)
	}
	if string(fake.calls[0].IMStatusContext) != string(rawCtx) {
		t.Errorf("emit context mismatch: got %s", fake.calls[0].IMStatusContext)
	}

	// Fields should have been cleared after the first emit.
	if loop.firstTurnCloudMessageID != "" || loop.firstTurnIMContext != nil {
		t.Errorf("first-turn fields not cleared: id=%q ctx=%s",
			loop.firstTurnCloudMessageID, loop.firstTurnIMContext)
	}
}

func TestAgentLoop_EmitFirstTurnLifecycle_SkipsWithoutContext(t *testing.T) {
	loop := newLoopForLifecycle(t)
	fake := &fakeLifecycleEmitter{}
	loop.SetLifecycleEmitter(fake)

	// SetFirstTurnLifecycle never called: both fields zero.
	loop.emitFirstTurnLifecycle()
	if len(fake.calls) != 0 {
		t.Errorf("expected 0 emits, got %d", len(fake.calls))
	}

	// Only messageID set, no context — also skips.
	loop.SetFirstTurnLifecycle("primary-1", nil)
	loop.emitFirstTurnLifecycle()
	if len(fake.calls) != 0 {
		t.Errorf("expected 0 emits with empty context, got %d", len(fake.calls))
	}

	// Only context set, no messageID — also skips.
	loop.SetFirstTurnLifecycle("", json.RawMessage(`{}`))
	loop.emitFirstTurnLifecycle()
	if len(fake.calls) != 0 {
		t.Errorf("expected 0 emits with empty id, got %d", len(fake.calls))
	}
}

func TestAgentLoop_EmitFirstTurnLifecycle_NoEmitterNoPanic(t *testing.T) {
	loop := newLoopForLifecycle(t)
	// No emitter installed — fields still populated.
	loop.SetFirstTurnLifecycle("primary-1", json.RawMessage(`{}`))
	loop.emitFirstTurnLifecycle()
	// Just verifying no panic.
}

// Verify SetLifecycleEmitter accepts nil (clear) without breaking subsequent
// emit attempts.
func TestAgentLoop_SetLifecycleEmitter_NilClears(t *testing.T) {
	loop := newLoopForLifecycle(t)
	fake := &fakeLifecycleEmitter{}
	loop.SetLifecycleEmitter(fake)
	loop.SetLifecycleEmitter(nil)

	loop.emitDrainedLifecycle([]InjectedMessage{
		{CloudMessageID: "m1", IMStatusContext: json.RawMessage(`{}`)},
	})
	if len(fake.calls) != 0 {
		t.Errorf("expected 0 emits after nil clear, got %d", len(fake.calls))
	}
}
