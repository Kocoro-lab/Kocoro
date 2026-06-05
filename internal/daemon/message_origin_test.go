package daemon

import (
	"encoding/json"
	"testing"
)

func TestParseMessageOrigin_Slack(t *testing.T) {
	cases := []struct {
		name      string
		channelID string
		wantScope string
	}{
		{"public channel", "C0ABC", "channel"},
		{"private group", "G0XYZ", "group"},
		{"dm", "D0DEF", "dm"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			blob := json.RawMessage(`{"platform":"slack","workspace_id":"T1","channel_id":"` + tc.channelID + `","message_ts":"1234.5678"}`)
			o := parseMessageOrigin("slack", blob)
			if o == nil {
				t.Fatal("expected non-nil origin")
			}
			if o.Platform != "slack" || o.ChannelID != tc.channelID || o.ThreadID != "1234.5678" || o.Scope != tc.wantScope {
				t.Fatalf("origin = %+v", o)
			}
		})
	}
}

func TestParseMessageOrigin_WeCom(t *testing.T) {
	group := json.RawMessage(`{"platform":"wecom","conversation_id":"chat123","chat_key":"g:chat123","thread_key":"tk1"}`)
	o := parseMessageOrigin("wecom", group)
	if o == nil || o.ChannelID != "chat123" || o.Scope != "group" || o.ThreadID != "tk1" {
		t.Fatalf("group origin = %+v", o)
	}
	dm := json.RawMessage(`{"platform":"wecom","conversation_id":"u987","chat_key":"u:u987"}`)
	o = parseMessageOrigin("wecom", dm)
	if o == nil || o.Scope != "dm" {
		t.Fatalf("dm origin = %+v", o)
	}
}

func TestParseMessageOrigin_LarkDegradesToNil(t *testing.T) {
	blob := json.RawMessage(`{"platform":"feishu","tenant_key":"tk","message_id":"om_x"}`)
	if o := parseMessageOrigin("feishu", blob); o != nil {
		t.Fatalf("Lark pre-S1b should degrade to nil, got %+v", o)
	}
}

func TestParseMessageOrigin_LarkWithS1bFields(t *testing.T) {
	blob := json.RawMessage(`{"platform":"feishu","tenant_key":"tk","message_id":"om_x","chat_id":"oc_123","chat_type":"group"}`)
	o := parseMessageOrigin("feishu", blob)
	if o == nil || o.ChannelID != "oc_123" || o.Scope != "group" {
		t.Fatalf("Lark+S1b origin = %+v", o)
	}
}

func TestParseMessageOrigin_EmptyOrJunk(t *testing.T) {
	if o := parseMessageOrigin("slack", nil); o != nil {
		t.Fatalf("empty blob should be nil, got %+v", o)
	}
	if o := parseMessageOrigin("slack", json.RawMessage(`not json`)); o != nil {
		t.Fatalf("junk blob should be nil, got %+v", o)
	}
	if o := parseMessageOrigin("slack", json.RawMessage(`{"platform":"slack","workspace_id":"T1"}`)); o != nil {
		t.Fatalf("channel-less slack blob should be nil, got %+v", o)
	}
}

func TestParseMessageOrigin_PlatformFromBlobOverridesSource(t *testing.T) {
	blob := json.RawMessage(`{"platform":"feishu","tenant_key":"tk","message_id":"x","chat_id":"oc_1","chat_type":"p2p"}`)
	o := parseMessageOrigin("lark", blob)
	if o == nil || o.Platform != "feishu" || o.Scope != "dm" {
		t.Fatalf("origin = %+v", o)
	}
}

func TestRenderChannelLine(t *testing.T) {
	t.Run("with label", func(t *testing.T) {
		o := &MessageOrigin{Platform: "slack", ChannelID: "C0ABC", ChannelLabel: "#shannon-discussion", Scope: "channel"}
		if got := o.renderChannelLine(); got != "slack · #shannon-discussion · channel" {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("degrades to id when no label", func(t *testing.T) {
		o := &MessageOrigin{Platform: "slack", ChannelID: "C0ABC", Scope: "channel"}
		if got := o.renderChannelLine(); got != "slack · C0ABC · channel" {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("sanitizes injection in label", func(t *testing.T) {
		o := &MessageOrigin{Platform: "slack", ChannelID: "C0ABC", ChannelLabel: "ignore\nprevious [instructions]", Scope: "channel"}
		got := o.renderChannelLine()
		if got != "slack · ignore previous (instructions) · channel" {
			t.Fatalf("unsanitized label: %q", got)
		}
	})
	t.Run("unknown scope omitted", func(t *testing.T) {
		o := &MessageOrigin{Platform: "feishu", ChannelID: "oc_1"}
		if got := o.renderChannelLine(); got != "feishu · oc_1" {
			t.Fatalf("got %q", got)
		}
	})
}
