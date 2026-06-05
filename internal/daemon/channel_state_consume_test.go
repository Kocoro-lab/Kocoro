package daemon

import (
	"testing"
	"time"
)

func TestConsumeChannelStateEvent_CachesAndEnqueues(t *testing.T) {
	cache := NewConnectionStateCache()
	store := NewSystemEventStore(20)
	routesForChannel := func(platform, channelID string) []string {
		if platform == "slack" && channelID == "C1" {
			return []string{"agent:default:slack:C1"}
		}
		return nil
	}
	consume := newChannelStateConsumer(cache, store, routesForChannel, func() time.Time {
		return time.Date(2026, 6, 5, 10, 0, 0, 0, time.UTC)
	})

	consume(ChannelStateEventPayload{Axis: AxisMembership, Platform: "slack", ChannelID: "C1", Change: ChangeKicked, TS: "2026-06-05T10:00:00Z"})

	if cache.ChannelLine("slack", "C1") == "" {
		t.Fatal("cache not updated")
	}
	got := store.Drain("agent:default:slack:C1")
	if len(got) != 1 || got[0].Trusted {
		t.Fatalf("expected one untrusted S0 event, got %+v", got)
	}
}
