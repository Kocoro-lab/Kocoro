package daemon

import (
	"testing"
)

func TestNewChannelStateConsumerForDeps_AppliesToCache(t *testing.T) {
	cache := NewConnectionStateCache()
	store := NewSystemEventStore(20)
	sc := NewSessionCache(t.TempDir())
	consume := NewChannelStateConsumerForDeps(cache, store, sc)
	consume(ChannelStateEventPayload{Axis: AxisBinding, Platform: "slack", Change: ChangeTokenRevoked, TS: "x"})
	if cache.PlatformLine("slack") == "" {
		t.Fatal("consumer should have folded the event into the cache")
	}
}
