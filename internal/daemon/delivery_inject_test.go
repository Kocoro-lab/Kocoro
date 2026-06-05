package daemon

import (
	"strings"
	"testing"
)

func TestFormatDeliveryFailure(t *testing.T) {
	t.Run("permanent phrases reactive removal", func(t *testing.T) {
		got := formatDeliveryFailure(ReplyDeliveryResultPayload{
			OK: false, Channel: "slack", ThreadID: "C1-99.1", Class: "permanent", Reason: "bot was kicked",
		})
		if !strings.Contains(got, "FAILED") || !strings.Contains(got, "bot was kicked") {
			t.Fatalf("permanent line missing parts: %q", got)
		}
		if !strings.Contains(strings.ToLower(got), "will not receive") {
			t.Fatalf("permanent line should carry reactive Gap-3 inference: %q", got)
		}
	})
	t.Run("transient is softer, no removal claim", func(t *testing.T) {
		got := formatDeliveryFailure(ReplyDeliveryResultPayload{
			OK: false, Channel: "slack", Class: "transient", Reason: "gateway closed (1006)",
		})
		if strings.Contains(strings.ToLower(got), "will not receive") {
			t.Fatalf("transient must not assert removal: %q", got)
		}
		if !strings.Contains(got, "may not have been delivered") {
			t.Fatalf("transient phrasing missing: %q", got)
		}
	})
}

func TestHandleReplyDeliveryResult(t *testing.T) {
	store := NewSystemEventStore(20)
	idx := NewReplyRouteIndex(8)
	idx.Put("m-perm", "route-A")
	idx.Put("m-ok", "route-B")

	consumer := newDeliveryResultConsumer(store, idx)

	consumer(ReplyDeliveryResultPayload{OK: true, Channel: "slack"}, "m-ok")
	if got := store.Drain("route-B"); len(got) != 0 {
		t.Fatalf("success must be silent, got %+v", got)
	}

	consumer(ReplyDeliveryResultPayload{OK: false, Channel: "slack", ThreadID: "C1-99.1", Class: "permanent", Reason: "bot was kicked"}, "m-perm")
	got := store.Drain("route-A")
	if len(got) != 1 || !strings.Contains(got[0].Text, "FAILED") {
		t.Fatalf("permanent failure should enqueue a FAILED line, got %+v", got)
	}
	if !got[0].Trusted {
		t.Fatal("our own delivery text is Trusted")
	}

	consumer(ReplyDeliveryResultPayload{OK: false, Class: "permanent"}, "m-unknown") // dropped, no panic
}
