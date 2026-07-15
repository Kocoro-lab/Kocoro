package daemon

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/session"
)

const (
	remoteTimelineView = "remote_timeline"

	// The remote response body is subsequently base64-wrapped in a WebSocket
	// envelope. Stay comfortably below maxRemoteResponseBodyBytes so the HTTP
	// JSON encoder's trailing newline and future additive metadata have room.
	remoteTimelineResponseBudgetBytes = 768 * 1024
	remoteTimelineDefaultLimit        = 60
	remoteTimelineMaxLimit            = 100
	remoteTimelineMaxMessageBytes     = 96 * 1024
	remoteTimelineMaxTextBytes        = 64 * 1024
	remoteTimelineMaxBlockTextBytes   = 16 * 1024
	remoteTimelineMaxToolResultBytes  = 4 * 1024
	remoteTimelineMaxToolInputBytes   = 8 * 1024
	remoteTimelineMaxBlocksPerMessage = 64
)

const remoteTimelineTruncationSuffix = "\n\n[Content truncated for remote history]"

// remoteTimelinePage is an opt-in projection for mobile history. It keeps the
// legacy session-detail fields so existing decoding helpers remain reusable,
// but adds explicit paging and omission metadata. It is never persisted.
type remoteTimelinePage struct {
	PageVersion         int                   `json:"page_version"`
	ID                  string                `json:"id"`
	Title               string                `json:"title"`
	CWD                 string                `json:"cwd"`
	CreatedAt           time.Time             `json:"created_at"`
	UpdatedAt           time.Time             `json:"updated_at"`
	Messages            []client.Message      `json:"messages"`
	MessageMeta         []session.MessageMeta `json:"message_meta"`
	StartIndex          int                   `json:"start_index"`
	TotalMessages       int                   `json:"total_messages"`
	HasMore             bool                  `json:"has_more"`
	NextCursor          string                `json:"next_cursor,omitempty"`
	OmittedContentCount int                   `json:"omitted_content_count"`
}

func buildRemoteTimelinePage(sess *session.Session, r *http.Request) (*remoteTimelinePage, error) {
	end := len(sess.Messages)
	if raw := strings.TrimSpace(r.URL.Query().Get("before")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 0 {
			return nil, fmt.Errorf("invalid remote timeline cursor")
		}
		if parsed < end {
			end = parsed
		}
	}

	limit := remoteTimelineDefaultLimit
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 {
			return nil, fmt.Errorf("invalid remote timeline limit")
		}
		limit = parsed
		if parsed > remoteTimelineMaxLimit {
			limit = remoteTimelineMaxLimit
		}
	}

	page := newRemoteTimelinePage(sess, end)
	start := end
	for start > 0 {
		groupStart := start - 1
		// Keep an assistant tool_use beside the immediately following user
		// tool_result so a page never renders a permanently-running orphan card.
		if remoteTimelineHasToolResult(sess.Messages[groupStart]) &&
			groupStart > 0 && remoteTimelineHasToolUse(sess.Messages[groupStart-1]) {
			groupStart--
		}
		groupLen := start - groupStart
		if len(page.Messages) > 0 && len(page.Messages)+groupLen > limit {
			break
		}

		projected := make([]client.Message, 0, groupLen)
		omitted := 0
		for i := groupStart; i < start; i++ {
			msg, count := projectRemoteTimelineMessage(sess.Messages[i])
			projected = append(projected, msg)
			omitted += count
		}
		candidate := *page
		candidate.Messages = append(projected, page.Messages...)
		candidate.MessageMeta = append(remoteTimelineMetaRange(sess, groupStart, start), page.MessageMeta...)
		candidate.StartIndex = groupStart
		candidate.OmittedContentCount += omitted
		candidate.HasMore = groupStart > 0
		candidate.NextCursor = remoteTimelineCursor(groupStart)

		encoded, err := json.Marshal(candidate)
		if err != nil {
			return nil, fmt.Errorf("encode remote timeline page: %w", err)
		}
		if len(encoded)+1 > remoteTimelineResponseBudgetBytes {
			// Per-message projection keeps a single logical group well below the
			// page budget. If the page already has content, stop at this boundary.
			if len(page.Messages) > 0 {
				break
			}
			return nil, fmt.Errorf("remote timeline projection exceeds response budget")
		}

		*page = candidate
		start = groupStart
	}

	page.HasMore = page.StartIndex > 0
	page.NextCursor = remoteTimelineCursor(page.StartIndex)
	return page, nil
}

func newRemoteTimelinePage(sess *session.Session, end int) *remoteTimelinePage {
	page := &remoteTimelinePage{
		PageVersion:   1,
		ID:            sess.ID,
		Title:         sess.Title,
		CWD:           sess.CWD,
		CreatedAt:     sess.CreatedAt,
		UpdatedAt:     sess.UpdatedAt,
		Messages:      []client.Message{},
		MessageMeta:   []session.MessageMeta{},
		StartIndex:    end,
		TotalMessages: len(sess.Messages),
		HasMore:       end > 0,
	}
	page.NextCursor = remoteTimelineCursor(end)
	return page
}

func remoteTimelineCursor(start int) string {
	if start <= 0 {
		return ""
	}
	// Clients treat this as opaque. A decimal cursor keeps v1 debuggable while
	// leaving room to change the encoding behind the capability in a future v2.
	return strconv.Itoa(start)
}

func remoteTimelineMetaRange(sess *session.Session, start, end int) []session.MessageMeta {
	meta := make([]session.MessageMeta, end-start)
	for i := start; i < end && i < len(sess.MessageMeta); i++ {
		meta[i-start] = sess.MessageMeta[i]
	}
	return meta
}

func remoteTimelineHasToolUse(msg client.Message) bool {
	for _, block := range msg.Content.Blocks() {
		if block.Type == "tool_use" {
			return true
		}
	}
	return false
}

func remoteTimelineHasToolResult(msg client.Message) bool {
	for _, block := range msg.Content.Blocks() {
		if block.Type == "tool_result" {
			return true
		}
	}
	return false
}

func projectRemoteTimelineMessage(msg client.Message) (client.Message, int) {
	projected := client.Message{Role: msg.Role, Name: msg.Name, ToolCallID: msg.ToolCallID}
	if !msg.Content.HasBlocks() {
		text, truncated := truncateRemoteTimelineUTF8(msg.Content.Text(), remoteTimelineMaxTextBytes)
		if truncated {
			text += remoteTimelineTruncationSuffix
		}
		projected.Content = client.NewTextContent(text)
		if truncated {
			return projected, 1
		}
		return projected, 0
	}

	blocks := msg.Content.Blocks()
	out := make([]client.ContentBlock, 0, min(len(blocks), remoteTimelineMaxBlocksPerMessage))
	omitted := 0
	for i, block := range blocks {
		if i >= remoteTimelineMaxBlocksPerMessage {
			out = append(out, client.ContentBlock{Type: "text", Text: "[Additional content blocks omitted from remote history]"})
			omitted += len(blocks) - i
			break
		}
		switch block.Type {
		case "text":
			text, truncated := truncateRemoteTimelineUTF8(block.Text, remoteTimelineMaxBlockTextBytes)
			if truncated {
				text += remoteTimelineTruncationSuffix
				omitted++
			}
			out = append(out, client.ContentBlock{Type: "text", Text: text})
		case "image", "document":
			out = append(out, client.ContentBlock{Type: "text", Text: remoteTimelineAttachmentPlaceholder(block)})
			omitted++
		case "thinking", "redacted_thinking":
			// Reasoning blocks are never part of the user-visible transcript. Drop
			// them without raising the UI omission banner or revealing they exist.
		case "tool_use":
			input, truncated := projectRemoteTimelineToolInput(block.Input)
			if truncated {
				omitted++
			}
			out = append(out, client.NewToolUseBlock(block.ID, block.Name, input))
		case "tool_result":
			text := client.ToolResultText(block)
			if text == "" {
				text = "[Non-text tool output omitted from remote history]"
				omitted++
			}
			var truncated bool
			text, truncated = truncateRemoteTimelineUTF8(text, remoteTimelineMaxToolResultBytes)
			if truncated {
				text += remoteTimelineTruncationSuffix
				omitted++
			}
			out = append(out, client.NewToolResultBlock(block.ToolUseID, text, block.IsError))
		case "tool_reference":
			out = append(out, client.ContentBlock{Type: block.Type, ToolName: block.ToolName})
		default:
			label := block.Type
			if label == "" {
				label = "unknown"
			}
			out = append(out, client.ContentBlock{Type: "text", Text: fmt.Sprintf("[%s content omitted from remote history]", label)})
			omitted++
		}
	}
	projected.Content = client.NewBlockContent(out)

	if encoded, err := json.Marshal(projected); err == nil && len(encoded) <= remoteTimelineMaxMessageBytes {
		return projected, omitted
	}

	// Pathological messages with many independently-large blocks still get a
	// visible placeholder rather than reintroducing a hard 413.
	preview, _ := truncateRemoteTimelineUTF8(msg.Content.Text(), remoteTimelineMaxTextBytes/2)
	if strings.TrimSpace(preview) == "" {
		preview = "[Message content omitted from remote history]"
	} else {
		preview += remoteTimelineTruncationSuffix
	}
	projected.Content = client.NewTextContent(preview)
	return projected, omitted + 1
}

func projectRemoteTimelineToolInput(input json.RawMessage) (json.RawMessage, bool) {
	if len(input) <= remoteTimelineMaxToolInputBytes {
		return append(json.RawMessage(nil), input...), false
	}
	preview, _ := truncateRemoteTimelineUTF8(string(input), remoteTimelineMaxToolInputBytes/4)
	replacement, _ := json.Marshal(map[string]any{
		"preview":          preview,
		"remote_truncated": true,
	})
	return replacement, true
}

func remoteTimelineAttachmentPlaceholder(block client.ContentBlock) string {
	kind := block.Type
	if kind == "" {
		kind = "attachment"
	}
	mediaType := ""
	rawBytes := 0
	if block.Source != nil {
		mediaType = block.Source.MediaType
		rawBytes = base64.StdEncoding.DecodedLen(len(block.Source.Data))
	}
	detail := strings.Trim(strings.Join([]string{kind, mediaType}, " · "), " ·")
	if rawBytes > 0 {
		detail += fmt.Sprintf(" · %d bytes", rawBytes)
	}
	return fmt.Sprintf("[%s omitted from remote history]", detail)
}

func truncateRemoteTimelineUTF8(value string, maxBytes int) (string, bool) {
	if len(value) <= maxBytes {
		return value, false
	}
	cut := maxBytes
	for cut > 0 && !utf8.ValidString(value[:cut]) {
		cut--
	}
	return value[:cut], true
}
