package session

import (
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

func mkMsg(role, text string) client.Message {
	return client.Message{Role: role, Content: client.NewTextContent(text)}
}

func mkSession() *Session {
	now := time.Now()
	return &Session{
		Messages: []client.Message{
			mkMsg("user", "first prompt"),
			mkMsg("assistant", "first reply"),
			mkMsg("user", "second prompt"),
			mkMsg("assistant", "second reply"),
		},
		MessageMeta: []MessageMeta{
			{Source: "local", MessageID: "u1", Timestamp: &now},
			{Source: "local", MessageID: "a1", Timestamp: &now},
			{Source: "local", MessageID: "u2", Timestamp: &now},
			{Source: "local", MessageID: "a2", Timestamp: &now},
		},
		SummaryCache:           "stale summary",
		SummaryCacheKey:        "stale-key",
		ToolResultReplacements: map[string]string{"id1": "x"},
		ToolResultSeen:         map[string]bool{"id1": true},
		InProgress:             true,
	}
}

func TestTruncateAt_TruncatesBothMessagesAndMeta(t *testing.T) {
	s := mkSession()
	restored, err := s.TruncateAt(2)
	if err != nil {
		t.Fatalf("TruncateAt: %v", err)
	}
	if restored.Text != "second prompt" {
		t.Errorf("restored.Text = %q, want %q", restored.Text, "second prompt")
	}
	if len(s.Messages) != 2 {
		t.Errorf("Messages length: want 2, got %d", len(s.Messages))
	}
	if len(s.MessageMeta) != 2 {
		t.Errorf("MessageMeta length: want 2, got %d", len(s.MessageMeta))
	}
	if s.SummaryCache != "" || s.SummaryCacheKey != "" {
		t.Errorf("summary cache should be cleared")
	}
	if len(s.ToolResultReplacements) != 0 || len(s.ToolResultSeen) != 0 {
		t.Errorf("tool result budget should be reset")
	}
	if s.InProgress {
		t.Errorf("InProgress should be cleared")
	}
}

func TestTruncateAt_ClipsShortMessageMetaWithoutPanic(t *testing.T) {
	s := mkSession()
	s.MessageMeta = s.MessageMeta[:1]

	restored, err := s.TruncateAt(2)
	if err != nil {
		t.Fatalf("TruncateAt: %v", err)
	}
	if restored.Text != "second prompt" {
		t.Errorf("restored.Text = %q, want %q", restored.Text, "second prompt")
	}
	if len(s.Messages) != 2 {
		t.Errorf("Messages length: want 2, got %d", len(s.Messages))
	}
	if len(s.MessageMeta) != 1 {
		t.Errorf("MessageMeta length: want existing shorter length 1, got %d", len(s.MessageMeta))
	}
}

func TestTruncateAt_RejectsNonUserMessage(t *testing.T) {
	s := mkSession()
	if _, err := s.TruncateAt(1); err == nil {
		t.Error("TruncateAt at assistant index should error")
	}
}

func TestTruncateAt_RejectsOutOfRange(t *testing.T) {
	s := mkSession()
	if _, err := s.TruncateAt(-1); err == nil {
		t.Error("TruncateAt(-1) should error")
	}
	if _, err := s.TruncateAt(99); err == nil {
		t.Error("TruncateAt(99) should error")
	}
}

func TestTruncateAt_NilSession(t *testing.T) {
	var s *Session
	if _, err := s.TruncateAt(0); err == nil {
		t.Error("TruncateAt on nil session should error")
	}
}

func TestSliceBeforeLastUser_HappyPath(t *testing.T) {
	s := &Session{
		Messages: []client.Message{
			mkMsg("user", "first"),
			mkMsg("assistant", "ok"),
			mkMsg("user", "second"),
		},
		MessageMeta: []MessageMeta{
			{MessageID: "u1"}, {MessageID: "a1"}, {MessageID: "u2"},
		},
	}
	restored, ok := s.SliceBeforeLastUser()
	if !ok {
		t.Fatal("SliceBeforeLastUser should succeed")
	}
	if restored.Text != "second" {
		t.Errorf("restored: %q", restored.Text)
	}
	if len(s.Messages) != 2 {
		t.Errorf("Messages: want 2, got %d", len(s.Messages))
	}
}

func TestSliceBeforeLastUser_RefusesWhenAssistantFollows(t *testing.T) {
	s := &Session{
		Messages: []client.Message{
			mkMsg("user", "first"),
			mkMsg("assistant", "this came after"),
		},
		MessageMeta: []MessageMeta{
			{MessageID: "u1"}, {MessageID: "a1"},
		},
	}
	if _, ok := s.SliceBeforeLastUser(); ok {
		t.Error("SliceBeforeLastUser must refuse when assistant message follows last user")
	}
	if len(s.Messages) != 2 {
		t.Error("session must be unchanged on refusal")
	}
}

func TestSliceBeforeLastUser_AllowsSystemMessagesAfter(t *testing.T) {
	s := &Session{
		Messages: []client.Message{
			mkMsg("user", "first"),
			mkMsg("system", "guard"),
		},
		MessageMeta: []MessageMeta{
			{MessageID: "u1"}, {MessageID: "sys"},
		},
	}
	if _, ok := s.SliceBeforeLastUser(); !ok {
		t.Error("trailing system messages should not block restore")
	}
}

func TestSliceBeforeLastUser_NoUserMessages(t *testing.T) {
	s := &Session{
		Messages: []client.Message{mkMsg("assistant", "no user here")},
	}
	if _, ok := s.SliceBeforeLastUser(); ok {
		t.Error("SliceBeforeLastUser must return false when no user messages exist")
	}
}

func TestFindUserMessageIndex(t *testing.T) {
	s := mkSession()
	if got := s.FindUserMessageIndex("u1"); got != 0 {
		t.Errorf("FindUserMessageIndex(u1) = %d, want 0", got)
	}
	if got := s.FindUserMessageIndex("u2"); got != 2 {
		t.Errorf("FindUserMessageIndex(u2) = %d, want 2", got)
	}
	if got := s.FindUserMessageIndex("a1"); got != -1 {
		t.Errorf("FindUserMessageIndex(a1) should be -1 (assistant), got %d", got)
	}
	if got := s.FindUserMessageIndex("missing"); got != -1 {
		t.Errorf("FindUserMessageIndex(missing) = %d, want -1", got)
	}
}
