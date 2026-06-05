package daemon

import "testing"

func TestChannelStateEventCapabilityAndType(t *testing.T) {
	found := false
	for _, c := range Capabilities {
		if c == CapChannelStateEventV1 {
			found = true
		}
	}
	if !found {
		t.Fatalf("CapChannelStateEventV1 (%q) not advertised", CapChannelStateEventV1)
	}
	if MsgTypeChannelStateEvent != "channel_state_event" {
		t.Fatalf("frame type = %q", MsgTypeChannelStateEvent)
	}
}
