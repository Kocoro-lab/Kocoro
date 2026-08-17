package daemon

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
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

// Stable {"error","code"} identifiers for reply-envelope ingress validation.
const (
	conversationRepliesMalformedCode    = "conversation_replies_malformed"
	conversationRepliesTooManyCode      = "conversation_replies_too_many"
	conversationReplyQuoteTooLongCode   = "conversation_reply_quote_too_long"
	conversationReplyCommentTooLongCode = "conversation_reply_comment_too_long"
)

type conversationReplyEnvelope struct {
	Replies []struct {
		Quote   string `xml:"quote"`
		Comment string `xml:"comment"`
	} `xml:"reply"`
}

// splitConversationReplyPrompt separates Desktop's model-only reply envelope
// from the user-visible prompt. Envelopes are accepted only at the head of a
// message; ordinary user text containing the same tags remains ordinary text.
// Consecutive head envelopes (produced when a drained mailbox batch merges
// with the triggering message) are consumed as one envelope run.
func splitConversationReplyPrompt(text string) (visible string, envelope string) {
	end := 0
	rest := text
	for strings.HasPrefix(rest, conversationRepliesOpen) {
		closeAt := strings.Index(rest, conversationRepliesClose)
		if closeAt < 0 {
			break
		}
		consumed := closeAt + len(conversationRepliesClose)
		end += consumed
		rest = rest[consumed:]
		// Only step over whitespace when another envelope follows — trailing
		// whitespace before visible text belongs to the visible split below.
		trimmed := strings.TrimLeft(rest, " \t\r\n")
		if !strings.HasPrefix(trimmed, conversationRepliesOpen) {
			break
		}
		end += len(rest) - len(trimmed)
		rest = trimmed
	}
	if end == 0 {
		return text, ""
	}
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

// conversationReplyAnnotations decodes one or more concatenated envelopes.
// A parse failure returns nil; a successful parse always returns a non-nil
// slice (possibly empty) so callers can distinguish "malformed" from
// "well-formed but content-free".
func conversationReplyAnnotations(envelope string) []session.ConversationAnnotation {
	parsed, err := decodeConversationReplyEnvelopes(envelope)
	if err != nil || parsed == nil {
		return nil
	}
	limit := len(parsed)
	if limit > maxConversationAnnotations {
		limit = maxConversationAnnotations
	}
	annotations := make([]session.ConversationAnnotation, 0, limit)
	for _, reply := range parsed[:limit] {
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

type conversationReplyEntry struct {
	Quote   string
	Comment string
}

// decodeConversationReplyEnvelopes parses a run of concatenated envelopes into
// the raw (unbounded) reply list. ("", nil, nil) for empty input; a non-nil
// error for any parse failure.
func decodeConversationReplyEnvelopes(envelope string) ([]conversationReplyEntry, error) {
	if envelope == "" {
		return nil, nil
	}
	dec := xml.NewDecoder(strings.NewReader(envelope))
	replies := []conversationReplyEntry{}
	for {
		var parsed conversationReplyEnvelope
		err := dec.Decode(&parsed)
		if errors.Is(err, io.EOF) {
			return replies, nil
		}
		if err != nil {
			return nil, err
		}
		for _, r := range parsed.Replies {
			replies = append(replies, conversationReplyEntry{Quote: r.Quote, Comment: r.Comment})
		}
	}
}

// validateConversationReplyEnvelope enforces the reply wire limits on the
// exact envelope the model would receive — not the truncated persistence
// projection. Returns ("", "") for text without a head envelope, else a
// stable code + English fallback message for writeErrorCode.
func validateConversationReplyEnvelope(text string) (code, message string) {
	_, envelope := splitConversationReplyPrompt(text)
	if envelope == "" {
		return "", ""
	}
	replies, err := decodeConversationReplyEnvelopes(envelope)
	if err != nil {
		return conversationRepliesMalformedCode, "reply envelope is not well-formed"
	}
	if len(replies) > maxConversationAnnotations {
		return conversationRepliesTooManyCode,
			fmt.Sprintf("reply envelope exceeds %d replies", maxConversationAnnotations)
	}
	for _, reply := range replies {
		if len([]rune(reply.Quote)) > maxConversationAnnotationQuoteRunes {
			return conversationReplyQuoteTooLongCode,
				fmt.Sprintf("reply quote exceeds %d characters", maxConversationAnnotationQuoteRunes)
		}
		if len([]rune(reply.Comment)) > maxConversationAnnotationCommentRunes {
			return conversationReplyCommentTooLongCode,
				fmt.Sprintf("reply comment exceeds %d characters", maxConversationAnnotationCommentRunes)
		}
	}
	return "", ""
}

func boundedConversationAnnotationText(text string, maxRunes int) string {
	runes := []rune(text)
	if len(runes) <= maxRunes {
		return text
	}
	return string(runes[:maxRunes])
}

// conversationReplyPersistedMessage returns the archival copy of a run user
// message plus the annotations decoded from its head envelope. Stripping is
// gated on a successful parse: a delimited-but-malformed envelope is kept
// verbatim so the user's bytes are never silently dropped, and tags anywhere
// past the head are ordinary text.
func conversationReplyPersistedMessage(message client.Message) (client.Message, []session.ConversationAnnotation) {
	if message.Role != "user" {
		return message, nil
	}
	if !message.Content.HasBlocks() {
		visible, annotations, stripped := conversationReplyVisibleText(message.Content.Text())
		if stripped {
			message.Content = client.NewTextContent(visible)
		}
		return message, annotations
	}
	blocks := append([]client.ContentBlock(nil), message.Content.Blocks()...)
	var annotations []session.ConversationAnnotation
	for index := range blocks {
		if blocks[index].Type == "text" {
			visible, decoded, stripped := conversationReplyVisibleText(blocks[index].Text)
			if stripped {
				blocks[index].Text = visible
			}
			annotations = decoded
			break
		}
	}
	message.Content = client.NewBlockContent(blocks)
	return message, annotations
}

// conversationReplyVisibleText splits + decodes a head envelope. stripped is
// false (and visible == text) when there is no envelope or it fails to parse.
// An injected-turn message (the agent loop's "[New message from user]" join of
// drained follow-ups) is handled segment-aware, because each follow-up's
// envelope sits at that follow-up's head inside the joined text, not at the
// message head.
func conversationReplyVisibleText(text string) (visible string, annotations []session.ConversationAnnotation, stripped bool) {
	if rest, ok := strings.CutPrefix(text, agent.InjectedUserMessagePrefix); ok {
		visible, annotations, stripped = stripJoinedConversationReplies(rest)
		return agent.InjectedUserMessagePrefix + visible, annotations, stripped
	}
	split, envelope := splitConversationReplyPrompt(text)
	if envelope == "" {
		return text, nil, false
	}
	annotations = conversationReplyAnnotations(envelope)
	if annotations == nil {
		return text, nil, false
	}
	return split, annotations, true
}

// stripJoinedConversationReplies removes every well-formed envelope run that
// starts at the head of a "\n\n"-joined segment — the positions where a
// drained follow-up's own head can land inside an injected-turn message.
// Anything that fails to parse, or sits anywhere else, stays verbatim.
func stripJoinedConversationReplies(text string) (string, []session.ConversationAnnotation, bool) {
	var out strings.Builder
	var annotations []session.ConversationAnnotation
	stripped := false
	i := 0
	for {
		idx := strings.Index(text[i:], conversationRepliesOpen)
		if idx < 0 {
			out.WriteString(text[i:])
			break
		}
		abs := i + idx
		atSegmentHead := abs == 0 || strings.HasSuffix(text[:abs], "\n\n")
		if atSegmentHead {
			segVisible, envelope := splitConversationReplyPrompt(text[abs:])
			if decoded := conversationReplyAnnotations(envelope); envelope != "" && decoded != nil {
				out.WriteString(text[:abs][i:])
				annotations = append(annotations, decoded...)
				if len(annotations) > maxConversationAnnotations {
					annotations = annotations[:maxConversationAnnotations]
				}
				stripped = true
				// splitConversationReplyPrompt already trimmed the separator
				// between the envelope run and the segment's visible text.
				text = segVisible
				i = 0
				continue
			}
		}
		out.WriteString(text[i : abs+len(conversationRepliesOpen)])
		i = abs + len(conversationRepliesOpen)
	}
	if !stripped {
		return text, nil, false
	}
	return out.String(), annotations, true
}
