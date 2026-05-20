package cmd

import (
	"encoding/json"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/daemon"
)

// recordedLifecycleEvent captures the args passed to SendEvent so tests can
// inspect what the helper emitted without going through a real WS connection.
type recordedLifecycleEvent struct {
	MessageID string
	EventType string
	Message   string
	Data      map[string]interface{}
}

// fakeLifecycleSender is the minimal lifecycleEventSender implementation used
// by these tests. It records every SendEvent call in order so tests can assert
// on the most recent event or the total count.
type fakeLifecycleSender struct {
	events []recordedLifecycleEvent
}

func (f *fakeLifecycleSender) SendEvent(messageID, eventType, message string, data map[string]interface{}) error {
	f.events = append(f.events, recordedLifecycleEvent{
		MessageID: messageID,
		EventType: eventType,
		Message:   message,
		Data:      data,
	})
	return nil
}

func (f *fakeLifecycleSender) SentEventCount() int { return len(f.events) }

func (f *fakeLifecycleSender) LastSentEvent(t *testing.T) recordedLifecycleEvent {
	t.Helper()
	if len(f.events) == 0 {
		t.Fatal("no events sent")
	}
	return f.events[len(f.events)-1]
}

func TestEmitLifecycleReceived(t *testing.T) {
	fake := &fakeLifecycleSender{}
	rawCtx := json.RawMessage(`{"platform":"slack","channel_id":"Cxxx","message_ts":"1.2"}`)
	emitLifecycleReceived(fake, "envelope-1", rawCtx)

	evt := fake.LastSentEvent(t)
	if evt.EventType != daemon.EventTypeMessageLifecycle {
		t.Fatalf("expected MESSAGE_LIFECYCLE, got %q", evt.EventType)
	}
	if evt.MessageID != "envelope-1" {
		t.Fatalf("expected message_id envelope-1, got %q", evt.MessageID)
	}
	if state, _ := evt.Data["state"].(string); state != daemon.LifecycleReceived {
		t.Fatalf("expected state=received, got %v", evt.Data["state"])
	}
	// im_status_context is stored as json.RawMessage in the event data map; the
	// fake records it by reference so a direct type assertion is sufficient.
	gotCtx, ok := evt.Data["im_status_context"].(json.RawMessage)
	if !ok {
		// Fall back to []byte in case a future refactor changes how SendEvent
		// stores the value internally.
		if gotBytes, okBytes := evt.Data["im_status_context"].([]byte); okBytes {
			if string(gotBytes) != string(rawCtx) {
				t.Fatalf("im_status_context bytes mismatch: got %s want %s", gotBytes, rawCtx)
			}
			return
		}
		t.Fatalf("im_status_context missing or wrong type: %T %v", evt.Data["im_status_context"], evt.Data["im_status_context"])
	}
	if string(gotCtx) != string(rawCtx) {
		t.Fatalf("im_status_context mismatch: got %s want %s", gotCtx, rawCtx)
	}
}

func TestEmitLifecycleReceivedNoOpOnEmptyContext(t *testing.T) {
	fake := &fakeLifecycleSender{}
	emitLifecycleReceived(fake, "envelope-1", nil)
	if fake.SentEventCount() != 0 {
		t.Fatal("expected no event when IMStatusContext is empty")
	}
}

func TestEmitLifecycleReceivedNoOpOnEmptyMessageID(t *testing.T) {
	fake := &fakeLifecycleSender{}
	rawCtx := json.RawMessage(`{"platform":"slack"}`)
	emitLifecycleReceived(fake, "", rawCtx)
	if fake.SentEventCount() != 0 {
		t.Fatal("expected no event when messageID is empty")
	}
}
