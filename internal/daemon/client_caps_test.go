package daemon

import "testing"

func TestReplyDeliveryResultCapabilityAdvertised(t *testing.T) {
	found := false
	for _, c := range Capabilities {
		if c == CapReplyDeliveryResultV1 {
			found = true
		}
	}
	if !found {
		t.Fatalf("CapReplyDeliveryResultV1 (%q) not in advertised Capabilities", CapReplyDeliveryResultV1)
	}
	if MsgTypeReplyDeliveryResult != "reply_delivery_result" {
		t.Fatalf("frame type = %q", MsgTypeReplyDeliveryResult)
	}
}
