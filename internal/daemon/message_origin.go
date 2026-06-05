package daemon

import (
	"encoding/json"
	"strings"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
)

// MessageOrigin is the flat, platform-neutral form of the inbound IMStatusContext
// blob. Populated by parseMessageOrigin and rendered into the Session Facts
// sticky block so the agent knows the SPECIFIC channel/group/DM + thread it was
// addressed in (S1). Sender is intentionally absent — it rides the existing
// RunAgentRequest.Sender field and the existing `Sender:` sticky line.
type MessageOrigin struct {
	Platform     string // slack / feishu / lark / wecom
	Scope        string // dm | group | channel | "" (unknown)
	ChannelID    string // native id (Slack C/G/D, WeCom conversation_id, Lark chat_id [needs S1b])
	ChannelLabel string // human-readable (#shannon); optional, from S1b best-effort; degrade to ChannelID
	ThreadID     string // Slack message_ts, WeCom thread_key
}

// parseMessageOrigin decodes the per-message IMStatusContext blob into a
// MessageOrigin. Returns nil when there is nothing fine-grained to render —
// empty/unparseable blob, unknown platform, or a platform whose blob lacks a
// channel id (Lark/Feishu before S1b). A nil result makes buildStickyContext
// fall back to the coarse `Channel: <channel>` line (= today's behavior).
//
// The blob's own "platform" field is authoritative (it distinguishes feishu vs
// lark, which share an SDK but route to different endpoints); the source arg is
// only a fallback when the blob omits it.
func parseMessageOrigin(source string, blob json.RawMessage) *MessageOrigin {
	if len(blob) == 0 {
		return nil
	}
	var raw struct {
		Platform string `json:"platform"`
		// slack
		ChannelID string `json:"channel_id"`
		MessageTS string `json:"message_ts"`
		// wecom
		ConversationID string `json:"conversation_id"`
		ChatKey        string `json:"chat_key"`
		ThreadKey      string `json:"thread_key"`
		// lark/feishu (added by S1b)
		ChatID   string `json:"chat_id"`
		ChatType string `json:"chat_type"`
		// optional human label (S1b best-effort, any platform)
		ChannelLabel string `json:"channel_label"`
	}
	if err := json.Unmarshal(blob, &raw); err != nil {
		return nil
	}
	platform := raw.Platform
	if platform == "" {
		platform = strings.ToLower(strings.TrimSpace(source))
	}

	o := &MessageOrigin{Platform: platform, ChannelLabel: raw.ChannelLabel}
	switch platform {
	case "slack":
		o.ChannelID = raw.ChannelID
		o.ThreadID = raw.MessageTS
		o.Scope = slackScope(raw.ChannelID)
	case "wecom":
		o.ChannelID = raw.ConversationID
		o.ThreadID = raw.ThreadKey
		o.Scope = wecomScope(raw.ChatKey)
	case "lark", "feishu":
		o.ChannelID = raw.ChatID // empty until S1b
		o.Scope = larkScope(raw.ChatType)
	default:
		return nil
	}
	if o.ChannelID == "" {
		return nil // nothing fine-grained — caller uses the coarse Channel line
	}
	return o
}

// stickyFromRequest parses the inbound blob into a MessageOrigin and builds the
// sticky-context block. Extracted from runner.go so the parse→render glue is
// unit-testable without a full RunAgent.
func stickyFromRequest(source, channel, sender, agentName, imBindings, extra string, blob json.RawMessage) string {
	origin := parseMessageOrigin(source, blob)
	return buildStickyContext(source, channel, sender, agentName, imBindings, extra, origin)
}

// slackScope maps a Slack channel id's leading char to a human scope.
// C=public channel, G=private group, D=direct message.
func slackScope(channelID string) string {
	if channelID == "" {
		return ""
	}
	switch channelID[0] {
	case 'C':
		return "channel"
	case 'G':
		return "group"
	case 'D':
		return "dm"
	default:
		return ""
	}
}

// wecomScope maps a WeCom chat_key prefix to a human scope (g: group, u: DM).
func wecomScope(chatKey string) string {
	switch {
	case strings.HasPrefix(chatKey, "g:"):
		return "group"
	case strings.HasPrefix(chatKey, "u:"):
		return "dm"
	default:
		return ""
	}
}

// larkScope maps a Lark/Feishu chat_type (present only after S1b) to a scope.
func larkScope(chatType string) string {
	switch chatType {
	case "group":
		return "group"
	case "p2p":
		return "dm"
	default:
		return ""
	}
}

// renderChannelLine builds the enriched Channel: value, e.g.
// "slack · #shannon-discussion · channel", degrading to the native id when no
// human label is present ("slack · C0ABC · channel"). ChannelLabel is
// platform-derived and attacker-influenceable, so it is sanitized before
// rendering (a group renamed "ignore previous instructions" must not break the
// sticky block). Reuses S0's sanitizer.
func (o *MessageOrigin) renderChannelLine() string {
	label := o.ChannelLabel
	if label == "" {
		label = o.ChannelID
	}
	label = agent.SanitizeSystemEventText(label)
	parts := []string{o.Platform}
	if label != "" {
		parts = append(parts, label)
	}
	if o.Scope != "" {
		parts = append(parts, o.Scope)
	}
	return strings.Join(parts, " · ")
}
