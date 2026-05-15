package daemon

import (
	"strings"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agenttypes"
)

func TestToDTO_OmitsInternalFields(t *testing.T) {
	now := time.Now().UTC().Round(time.Second)
	in := agenttypes.QueuedMessage{
		ID:         "id-1",
		Source:     "ws",
		Text:       "hello world",
		Editable:   true,
		EnqueuedAt: now,
		// Fields that MUST NOT leak through the DTO:
		CloudMsgID: "internal-cloud-id",
		RouteKey:   "route-secret",
		SessionID:  "sess-secret",
		CWD:        "/internal/path",
		Mode:       "thread-internal",
	}
	dto := ToDTO(in)
	if dto.ID != "id-1" || dto.Preview != "hello world" || dto.EnqueuedAt != now {
		t.Errorf("basic field map: %+v", dto)
	}
	// Belt-and-suspenders: serialize the DTO and verify forbidden field names
	// don't appear in the JSON.
	asJSON := mustJSON(dto)
	for _, banned := range []string{"cloud_msg_id", "route_key", "session_id", "cwd", "mode"} {
		if strings.Contains(asJSON, banned) {
			t.Errorf("DTO JSON leaks internal field %q: %s", banned, asJSON)
		}
	}
}

func TestToDTO_PreviewTruncatesToMaxRunes(t *testing.T) {
	in := agenttypes.QueuedMessage{Text: strings.Repeat("a", queuePreviewMaxRunes+50)}
	dto := ToDTO(in)
	// Resulting string is queuePreviewMaxRunes characters + ellipsis.
	runeCount := 0
	for range dto.Preview {
		runeCount++
	}
	if runeCount != queuePreviewMaxRunes+1 {
		t.Errorf("preview rune count = %d, want %d", runeCount, queuePreviewMaxRunes+1)
	}
	if !strings.HasSuffix(dto.Preview, "…") {
		t.Errorf("preview missing ellipsis: %q", dto.Preview)
	}
}

func TestToDTO_PreviewRuneSafeForChinese(t *testing.T) {
	// "用" is 3 bytes in UTF-8; byte-based truncation would split a rune.
	in := agenttypes.QueuedMessage{Text: strings.Repeat("用", queuePreviewMaxRunes+10)}
	dto := ToDTO(in)
	runeCount := 0
	for range dto.Preview {
		runeCount++
	}
	if runeCount != queuePreviewMaxRunes+1 {
		t.Errorf("Chinese preview rune count = %d, want %d", runeCount, queuePreviewMaxRunes+1)
	}
	if !strings.HasSuffix(dto.Preview, "…") {
		t.Errorf("Chinese preview missing ellipsis: %q", dto.Preview)
	}
}

func TestToDTO_AttachmentCount(t *testing.T) {
	in := agenttypes.QueuedMessage{
		Attachments: []agenttypes.QueuedAttachment{
			{Nonce: "x"}, {Nonce: "y"}, {Nonce: "z"},
		},
	}
	if got := ToDTO(in).AttachmentCount; got != 3 {
		t.Errorf("attachment_count = %d, want 3", got)
	}
}

func TestToDTOs_NilForEmpty(t *testing.T) {
	if got := ToDTOs(nil); got != nil {
		t.Errorf("ToDTOs(nil): want nil, got %+v", got)
	}
	if got := ToDTOs([]agenttypes.QueuedMessage{}); got != nil {
		t.Errorf("ToDTOs([]): want nil, got %+v", got)
	}
}

