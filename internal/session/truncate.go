package session

import (
	"errors"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

// RestoredMessage carries the text and attachments captured from a truncated
// user message so callers (cancel/restore-last, rewind) can put them back
// into a UI input box.
//
// Attachments is reserved for Phase 4 (queued attachments); in Phase 1 it is
// always empty.
type RestoredMessage struct {
	Text        string            `json:"text"`
	Attachments []RestoredFileRef `json:"attachments,omitempty"`
}

// RestoredFileRef is a minimal pointer to a previously-attached file. The
// daemon does not re-upload anything; the Desktop client maps `nonce` back
// to the local tmp file or the remote URL.
type RestoredFileRef struct {
	Nonce       string `json:"nonce,omitempty"`
	OriginalURL string `json:"original_url,omitempty"`
	Kind        string `json:"kind,omitempty"`
}

// ErrTruncateOutOfRange is returned when TruncateAt is called with an index
// that is negative, beyond Messages length, or pointing at a non-user role.
var ErrTruncateOutOfRange = errors.New("truncate index out of range or not a user message")

// TruncateAt removes Messages[idx:] AND the parallel MessageMeta entries,
// clears the cached summary, resets tool-result budget state, and clears
// InProgress. The user message originally at idx is captured into a
// RestoredMessage so the caller can put its text back into an input box
// (cancel-restore-last semantics) without making a separate read.
//
// Returns ErrTruncateOutOfRange when idx is invalid or Messages[idx] is not
// a user message. Returns nil if and only if the session was successfully
// mutated.
//
// Callers must hold any external lock guarding the session — Session is not
// concurrency-safe by itself. Daemon callers should be inside
// routeEntry.mu (the established per-route session-mutation lock).
func (s *Session) TruncateAt(idx int) (*RestoredMessage, error) {
	if s == nil {
		return nil, ErrTruncateOutOfRange
	}
	if idx < 0 || idx >= len(s.Messages) {
		return nil, ErrTruncateOutOfRange
	}
	if s.Messages[idx].Role != "user" {
		return nil, ErrTruncateOutOfRange
	}

	restored := &RestoredMessage{Text: messageText(s.Messages[idx])}

	s.Messages = s.Messages[:idx]
	// MessageMeta may be shorter than Messages for sessions created before
	// metadata existed or after partial recovery. Do not extend the slice into
	// stale capacity; clip to the shorter side.
	s.MessageMeta = s.MessageMeta[:min(idx, len(s.MessageMeta))]

	// Summary cache is no longer valid because the conversation tail is gone.
	s.SummaryCache = ""
	s.SummaryCacheKey = ""

	// Reset tool-result budget so the next run doesn't try to "replace" a
	// tool_use id that has been removed from the transcript.
	s.ToolResultReplacements = nil
	s.ToolResultSeen = nil

	// The truncate operation is intentional; if InProgress was set from a
	// crashed run, clear it so the session list no longer flags the row.
	s.InProgress = false

	return restored, nil
}

// SliceBeforeLastUser truncates the session just before its most recent
// `role:"user"` message and returns it as a RestoredMessage. Returns
// (nil, false) when there are no user messages or when the last user
// message has assistant content following it (i.e. a reply was generated
// and a "clean" restore is no longer possible).
//
// The "no content after" check guards the auto-restore path —
// restoring a user message that already has a
// (partial or full) assistant reply downstream would leave the user
// confused about whether the assistant actually responded.
func (s *Session) SliceBeforeLastUser() (*RestoredMessage, bool) {
	if s == nil {
		return nil, false
	}
	for i := len(s.Messages) - 1; i >= 0; i-- {
		if s.Messages[i].Role != "user" {
			continue
		}
		// Found the most recent user message. Reject the restore if any
		// non-user, non-system message follows.
		for j := i + 1; j < len(s.Messages); j++ {
			r := s.Messages[j].Role
			if r != "system" {
				return nil, false
			}
		}
		restored, err := s.TruncateAt(i)
		if err != nil {
			return nil, false
		}
		return restored, true
	}
	return nil, false
}

// FindUserMessageIndex returns the index of the user message with the given
// MessageID (from MessageMeta), or -1 if not found. Used by the rewind
// endpoint to translate a wire-level message_id into a TruncateAt index.
func (s *Session) FindUserMessageIndex(messageID string) int {
	if s == nil || messageID == "" {
		return -1
	}
	limit := min(len(s.Messages), len(s.MessageMeta))
	for i := range limit {
		if s.MessageMeta[i].MessageID != messageID {
			continue
		}
		if s.Messages[i].Role != "user" {
			continue
		}
		return i
	}
	return -1
}

// messageText extracts a plain-text view of a Message.Content for the
// restore payload. client.MessageContent already concatenates text blocks
// via its Text() helper.
func messageText(m client.Message) string {
	return m.Content.Text()
}
