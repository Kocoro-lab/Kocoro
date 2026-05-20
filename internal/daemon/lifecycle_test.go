package daemon

import (
	"encoding/json"
	"errors"
	"testing"
)

// recordedSendEvent captures one call to LifecycleEventSender.SendEvent so
// tests can inspect arguments without standing up a real WebSocket server.
type recordedSendEvent struct {
	MessageID string
	EventType string
	Message   string
	Data      map[string]interface{}
}

type fakeLifecycleSender struct {
	events  []recordedSendEvent
	sendErr error
}

func (f *fakeLifecycleSender) SendEvent(messageID, eventType, message string, data map[string]interface{}) error {
	f.events = append(f.events, recordedSendEvent{
		MessageID: messageID,
		EventType: eventType,
		Message:   message,
		Data:      data,
	})
	return f.sendErr
}

// fakeAppender records AppendDrainedInflight calls without owning a real
// route cache. Plugged into RunLifecycleEmitter so tests can verify the
// bookkeeping half independently from the WS send half.
type fakeAppender struct {
	calls []struct {
		Route string
		Entry DrainedInflightEntry
	}
}

func (f *fakeAppender) AppendDrainedInflight(routeKey string, entry DrainedInflightEntry) {
	f.calls = append(f.calls, struct {
		Route string
		Entry DrainedInflightEntry
	}{routeKey, entry})
}

func TestRunLifecycleEmitter_EmitsProcessingAndRecords(t *testing.T) {
	ws := &fakeLifecycleSender{}
	cache := &fakeAppender{}
	emitter := NewRunLifecycleEmitter(ws, cache, "agent:test")

	rawCtx := json.RawMessage(`{"platform":"slack","channel_id":"Cxxx","message_ts":"1.2"}`)
	emitter.OnUserMessageProcessing("envelope-1", rawCtx)

	if len(ws.events) != 1 {
		t.Fatalf("expected 1 ws event, got %d", len(ws.events))
	}
	evt := ws.events[0]
	if evt.EventType != EventTypeMessageLifecycle {
		t.Errorf("event type: got %q want %q", evt.EventType, EventTypeMessageLifecycle)
	}
	if evt.MessageID != "envelope-1" {
		t.Errorf("message id: got %q want envelope-1", evt.MessageID)
	}
	if state, _ := evt.Data["state"].(string); state != LifecycleProcessing {
		t.Errorf("state: got %v want processing", evt.Data["state"])
	}
	gotCtx, ok := evt.Data["im_status_context"].(json.RawMessage)
	if !ok {
		t.Fatalf("im_status_context wrong type: %T", evt.Data["im_status_context"])
	}
	if string(gotCtx) != string(rawCtx) {
		t.Errorf("im_status_context: got %s want %s", gotCtx, rawCtx)
	}

	if len(cache.calls) != 1 {
		t.Fatalf("expected 1 appended entry, got %d", len(cache.calls))
	}
	call := cache.calls[0]
	if call.Route != "agent:test" {
		t.Errorf("route: got %q want agent:test", call.Route)
	}
	if call.Entry.CloudMessageID != "envelope-1" {
		t.Errorf("entry cloud id: got %q want envelope-1", call.Entry.CloudMessageID)
	}
	if string(call.Entry.IMStatusContext) != string(rawCtx) {
		t.Errorf("entry context mismatch")
	}
}

func TestRunLifecycleEmitter_NoEmitOnEmptyContext(t *testing.T) {
	ws := &fakeLifecycleSender{}
	cache := &fakeAppender{}
	emitter := NewRunLifecycleEmitter(ws, cache, "agent:test")

	emitter.OnUserMessageProcessing("envelope-1", nil)

	if len(ws.events) != 0 {
		t.Errorf("expected 0 ws events on empty context, got %d", len(ws.events))
	}
	if len(cache.calls) != 0 {
		t.Errorf("expected 0 append calls on empty context, got %d", len(cache.calls))
	}
}

func TestRunLifecycleEmitter_NoEmitOnEmptyMessageID(t *testing.T) {
	ws := &fakeLifecycleSender{}
	cache := &fakeAppender{}
	emitter := NewRunLifecycleEmitter(ws, cache, "agent:test")

	rawCtx := json.RawMessage(`{"platform":"slack"}`)
	emitter.OnUserMessageProcessing("", rawCtx)

	if len(ws.events) != 0 {
		t.Errorf("expected 0 ws events on empty message id, got %d", len(ws.events))
	}
	if len(cache.calls) != 0 {
		t.Errorf("expected 0 append calls on empty message id, got %d", len(cache.calls))
	}
}

// Cache bookkeeping must run even when the WS send returns an error so the
// run-completion sweep (Task 9) can still emit "done" / "cleared" for the
// entry that did move into an LLM turn.
func TestRunLifecycleEmitter_RecordsEvenWhenSendFails(t *testing.T) {
	ws := &fakeLifecycleSender{sendErr: errors.New("ws closed")}
	cache := &fakeAppender{}
	emitter := NewRunLifecycleEmitter(ws, cache, "agent:test")

	rawCtx := json.RawMessage(`{"platform":"slack"}`)
	emitter.OnUserMessageProcessing("envelope-1", rawCtx)

	if len(ws.events) != 1 {
		t.Errorf("ws send still recorded once: got %d", len(ws.events))
	}
	if len(cache.calls) != 1 {
		t.Errorf("append must run despite ws error: got %d", len(cache.calls))
	}
}

// Test the SessionCache.AppendDrainedInflight surface against a real cache.
func TestSessionCache_AppendDrainedInflight_AppendsToRoute(t *testing.T) {
	cache := NewSessionCache(t.TempDir())
	entry := cache.LockRouteWithManager("agent:test", t.TempDir())
	cache.UnlockRoute("agent:test")
	_ = entry

	rawCtx := json.RawMessage(`{"platform":"slack"}`)
	cache.AppendDrainedInflight("agent:test", DrainedInflightEntry{
		CloudMessageID:  "m1",
		IMStatusContext: rawCtx,
	})
	cache.AppendDrainedInflight("agent:test", DrainedInflightEntry{
		CloudMessageID:  "m2",
		IMStatusContext: rawCtx,
	})

	cache.mu.Lock()
	got := cache.routes["agent:test"].drainedInflight
	cache.mu.Unlock()

	if len(got) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(got))
	}
	if got[0].CloudMessageID != "m1" || got[1].CloudMessageID != "m2" {
		t.Errorf("order/ids wrong: %+v", got)
	}
}

func TestSessionCache_AppendDrainedInflight_NoOpOnMissingRoute(t *testing.T) {
	cache := NewSessionCache(t.TempDir())
	// Append for a route that was never created — must not panic, must not
	// silently create an entry (the map should stay empty).
	cache.AppendDrainedInflight("agent:ghost", DrainedInflightEntry{
		CloudMessageID:  "m1",
		IMStatusContext: json.RawMessage(`{}`),
	})

	cache.mu.Lock()
	_, exists := cache.routes["agent:ghost"]
	cache.mu.Unlock()
	if exists {
		t.Error("append must not auto-create route entries")
	}
}

func TestSessionCache_AppendDrainedInflight_NoOpOnEmptyKeyOrID(t *testing.T) {
	cache := NewSessionCache(t.TempDir())
	cache.LockRouteWithManager("agent:test", t.TempDir())
	cache.UnlockRoute("agent:test")

	cache.AppendDrainedInflight("", DrainedInflightEntry{CloudMessageID: "m1"})
	cache.AppendDrainedInflight("agent:test", DrainedInflightEntry{CloudMessageID: ""})

	cache.mu.Lock()
	got := cache.routes["agent:test"].drainedInflight
	cache.mu.Unlock()
	if len(got) != 0 {
		t.Errorf("expected 0 entries, got %d", len(got))
	}
}
