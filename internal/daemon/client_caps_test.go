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

func TestAgentDefaultCWDV1CapabilityAdvertised(t *testing.T) {
	for _, c := range Capabilities {
		if c == CapAgentDefaultCWDV1 {
			return
		}
	}
	t.Fatalf("CapAgentDefaultCWDV1 (%q) not in advertised Capabilities", CapAgentDefaultCWDV1)
}

func TestSessionProjectsV1CapabilityAdvertised(t *testing.T) {
	for _, c := range Capabilities {
		if c == CapSessionProjectsV1 {
			return
		}
	}
	t.Fatalf("CapSessionProjectsV1 (%q) not in advertised Capabilities", CapSessionProjectsV1)
}
