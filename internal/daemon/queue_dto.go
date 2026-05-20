package daemon

import (
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agenttypes"
)

// QueuedMessageDTO is the redacted shape used by /queue HTTP responses and
// queue.* SSE events. The full agenttypes.QueuedMessage carries the Cloud
// origin payload, attachment URLs, and other fields the daemon must keep
// internal — wire types intentionally strip those so the global event bus
// cannot leak them to future cross-session subscribers.
//
// Fields are arranged to match the kocoro skill reference doc verbatim:
// any future field addition must also update references/queue.md.
type QueuedMessageDTO struct {
	ID              string    `json:"id"`
	Preview         string    `json:"preview"`
	Editable        bool      `json:"editable"`
	AttachmentCount int       `json:"attachment_count"`
	EnqueuedAt      time.Time `json:"enqueued_at"`
	Source          string    `json:"source"`
}

const queuePreviewMaxRunes = 120

// ToDTO converts a full QueuedMessage to the redacted wire shape. The
// preview is truncated to queuePreviewMaxRunes runes (NOT bytes — Chinese
// payloads need rune-correct slicing) with an ellipsis appended when
// truncation occurred.
func ToDTO(m agenttypes.QueuedMessage) QueuedMessageDTO {
	return QueuedMessageDTO{
		ID:              m.ID,
		Preview:         previewText(m.Text, queuePreviewMaxRunes),
		Editable:        m.Editable,
		AttachmentCount: len(m.Attachments),
		EnqueuedAt:      m.EnqueuedAt,
		Source:          m.Source,
	}
}

// ToDTOs maps a slice of QueuedMessage to its DTO form. Returns nil for
// empty input so JSON encoders emit `null` rather than `[]` — matching
// other daemon listing endpoints.
func ToDTOs(msgs []agenttypes.QueuedMessage) []QueuedMessageDTO {
	if len(msgs) == 0 {
		return nil
	}
	out := make([]QueuedMessageDTO, len(msgs))
	for i, m := range msgs {
		out[i] = ToDTO(m)
	}
	return out
}

func previewText(s string, maxRunes int) string {
	if maxRunes <= 0 || s == "" {
		return s
	}
	count := 0
	for i := range s {
		if count == maxRunes {
			return s[:i] + "…"
		}
		count++
	}
	return s
}
