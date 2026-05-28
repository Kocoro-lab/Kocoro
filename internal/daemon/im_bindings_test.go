package daemon

import (
	"encoding/json"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

func TestFormatIMBindings(t *testing.T) {
	mkConfig := func(agent, slackChan string) json.RawMessage {
		m := map[string]string{}
		if agent != "" {
			m["agent_name"] = agent
		}
		if slackChan != "" {
			m["slack_channel"] = slackChan
		}
		b, _ := json.Marshal(m)
		return b
	}

	tests := []struct {
		name string
		in   []client.ChannelBinding
		want string
	}{
		{
			name: "no bindings",
			in:   nil,
			want: "",
		},
		{
			name: "single default-agent slack binding",
			in: []client.ChannelBinding{
				{Type: "slack", Name: "kocoro-test-slack", Enabled: true, Config: mkConfig("", "C0AAA")},
			},
			want: "default=slack:kocoro-test-slack",
		},
		{
			name: "named agent feishu binding",
			in: []client.ChannelBinding{
				{Type: "feishu", Name: "engineering", Enabled: true, Config: mkConfig("analyst", "")},
			},
			want: "analyst=feishu:engineering",
		},
		{
			name: "default sorts before named, multiple joined by ;",
			in: []client.ChannelBinding{
				{Type: "feishu", Name: "engineering", Enabled: true, Config: mkConfig("analyst", "")},
				{Type: "slack", Name: "kocoro-test-slack", Enabled: true, Config: mkConfig("", "C0AAA")},
			},
			want: "default=slack:kocoro-test-slack; analyst=feishu:engineering",
		},
		{
			name: "disabled rows skipped",
			in: []client.ChannelBinding{
				{Type: "slack", Name: "old-channel", Enabled: false, Config: mkConfig("", "")},
				{Type: "slack", Name: "current", Enabled: true, Config: mkConfig("", "")},
			},
			want: "default=slack:current",
		},
		{
			name: "non-IM channel types filtered out",
			in: []client.ChannelBinding{
				{Type: "email", Name: "should-skip", Enabled: true, Config: mkConfig("default-agent-name", "")},
				{Type: "slack", Name: "keep", Enabled: true, Config: mkConfig("", "")},
			},
			want: "default=slack:keep",
		},
		{
			name: "agent_name missing key treated as default agent",
			in: []client.ChannelBinding{
				{Type: "slack", Name: "no-key", Enabled: true, Config: json.RawMessage(`{"slack_channel":"C0"}`)},
			},
			want: "default=slack:no-key",
		},
		{
			name: "nil config treated as default agent",
			in: []client.ChannelBinding{
				{Type: "slack", Name: "nil-config", Enabled: true, Config: nil},
			},
			want: "default=slack:nil-config",
		},
		{
			name: "named-agent sort is alphabetical, ties by type",
			in: []client.ChannelBinding{
				{Type: "telegram", Name: "ch-b", Enabled: true, Config: mkConfig("zebra", "")},
				{Type: "slack", Name: "ch-a", Enabled: true, Config: mkConfig("zebra", "")},
				{Type: "feishu", Name: "ch-c", Enabled: true, Config: mkConfig("alpha", "")},
			},
			want: "alpha=feishu:ch-c; zebra=slack:ch-a; zebra=telegram:ch-b",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := formatIMBindings(tc.in)
			if got != tc.want {
				t.Errorf("got\n  %q\nwant\n  %q", got, tc.want)
			}
		})
	}
}
