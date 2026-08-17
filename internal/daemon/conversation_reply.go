package daemon

import (
	"encoding/xml"
	"strings"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/session"
)

const (
	conversationRepliesOpen               = "<kocoro_replies>"
	conversationRepliesClose              = "</kocoro_replies>"
	maxConversationAnnotations            = 100
	maxConversationAnnotationQuoteRunes   = 8_000
	maxConversationAnnotationCommentRunes = 2_000
)

type conversationReplyEnvelope struct {
	Replies []struct {
		Quote   string `xml:"quote"`
		Comment string `xml:"comment"`
	} `xml:"reply"`
}

// splitConversationReplyPrompt separates Desktop's model-only reply envelope
// from the user-visible prompt. The envelope is accepted only at the head of a
// message; ordinary user text containing the same tags remains ordinary text.
func splitConversationReplyPrompt(text string) (visible string, envelope string) {
	if !strings.HasPrefix(text, conversationRepliesOpen) {
		return text, ""
	}
	end := strings.Index(text, conversationRepliesClose)
	if end < 0 {
		return text, ""
	}
	end += len(conversationRepliesClose)
	return strings.TrimSpace(text[end:]), text[:end]
}

func restoreConversationReplyPrompt(visible, envelope string) string {
	if envelope == "" {
		return visible
	}
	if visible == "" {
		return envelope
	}
	return envelope + "\n\n" + visible
}

func conversationReplyAnnotations(envelope string) []session.ConversationAnnotation {
	if envelope == "" {
		return nil
	}
	var parsed conversationReplyEnvelope
	if err := xml.Unmarshal([]byte(envelope), &parsed); err != nil {
		return nil
	}
	limit := len(parsed.Replies)
	if limit > maxConversationAnnotations {
		limit = maxConversationAnnotations
	}
	annotations := make([]session.ConversationAnnotation, 0, limit)
	for _, reply := range parsed.Replies[:limit] {
		quote := boundedConversationAnnotationText(reply.Quote, maxConversationAnnotationQuoteRunes)
		comment := boundedConversationAnnotationText(reply.Comment, maxConversationAnnotationCommentRunes)
		if quote == "" && comment == "" {
			continue
		}
		annotations = append(annotations, session.ConversationAnnotation{
			SelectedText: quote,
			Comment:      comment,
		})
	}
	return annotations
}

func boundedConversationAnnotationText(text string, maxRunes int) string {
	runes := []rune(text)
	if len(runes) <= maxRunes {
		return text
	}
	return string(runes[:maxRunes])
}

// visibleConversationReplyText removes every well-formed model-only envelope
// from a combined prompt. Multiple envelopes occur when injected follow-ups
// are drained into one run. Persisted transcript, titles, previews, and global
// events must contain only the user's visible text.
func visibleConversationReplyText(text string) string {
	removed := false
	for {
		start := strings.Index(text, conversationRepliesOpen)
		if start < 0 {
			if !removed {
				return text
			}
			return strings.TrimSpace(text)
		}
		relativeEnd := strings.Index(text[start:], conversationRepliesClose)
		if relativeEnd < 0 {
			return strings.TrimSpace(text)
		}
		end := start + relativeEnd + len(conversationRepliesClose)
		text = text[:start] + text[end:]
		removed = true
	}
}

func visibleConversationReplyMessage(message client.Message) client.Message {
	if message.Role != "user" {
		return message
	}
	if !message.Content.HasBlocks() {
		message.Content = client.NewTextContent(visibleConversationReplyText(message.Content.Text()))
		return message
	}
	blocks := append([]client.ContentBlock(nil), message.Content.Blocks()...)
	for index := range blocks {
		if blocks[index].Type == "text" {
			blocks[index].Text = visibleConversationReplyText(blocks[index].Text)
			break
		}
	}
	message.Content = client.NewBlockContent(blocks)
	return message
}
