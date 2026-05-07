package heartbeat

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

func TestIsHeartbeatOK(t *testing.T) {
	tests := []struct {
		reply string
		want  bool
	}{
		{"HEARTBEAT_OK", true},
		{"heartbeat_ok", true},
		{"  HEARTBEAT_OK  ", true},
		{"\nHEARTBEAT_OK\n", true},
		{"HEARTBEAT_OK and some extra text", false},
		{"Everything looks fine", false},
		{"", false},
	}
	for _, tt := range tests {
		t.Run(tt.reply, func(t *testing.T) {
			if got := IsHeartbeatOK(tt.reply); got != tt.want {
				t.Errorf("IsHeartbeatOK(%q) = %v, want %v", tt.reply, got, tt.want)
			}
		})
	}
}

func TestFormatPrompt(t *testing.T) {
	checklist := "- Check disk\n- Check memory"
	got := FormatPrompt(checklist)
	if got == "" {
		t.Fatal("expected non-empty prompt")
	}
	if !strings.Contains(got, "HEARTBEAT_OK") {
		t.Error("prompt should mention HEARTBEAT_OK")
	}
	if !strings.Contains(got, checklist) {
		t.Error("prompt should contain checklist")
	}
}

func TestTranscriptCollectorBlocksPerCallApprovalTools(t *testing.T) {
	tc := &TranscriptCollector{}
	if tc.OnApprovalNeeded("publish_to_web", `{}`) {
		t.Fatal("heartbeat must not auto-approve publish_to_web")
	}
	if !tc.OnApprovalNeeded("bash", `{}`) {
		t.Fatal("heartbeat should keep auto-approving ordinary tools")
	}
}

func TestReadChecklist(t *testing.T) {
	dir := t.TempDir()

	// Missing file — should return empty.
	content, err := ReadChecklist(filepath.Join(dir, "HEARTBEAT.md"))
	if err != nil {
		t.Fatal(err)
	}
	if content != "" {
		t.Errorf("expected empty for missing file, got %q", content)
	}

	// Empty file — should return empty.
	os.WriteFile(filepath.Join(dir, "HEARTBEAT.md"), []byte("   \n\n  "), 0644)
	content, err = ReadChecklist(filepath.Join(dir, "HEARTBEAT.md"))
	if err != nil {
		t.Fatal(err)
	}
	if content != "" {
		t.Errorf("expected empty for whitespace-only file, got %q", content)
	}

	// Valid file.
	os.WriteFile(filepath.Join(dir, "HEARTBEAT.md"), []byte("- Check disk\n- Check memory"), 0644)
	content, err = ReadChecklist(filepath.Join(dir, "HEARTBEAT.md"))
	if err != nil {
		t.Fatal(err)
	}
	if content != "- Check disk\n- Check memory" {
		t.Errorf("unexpected content: %q", content)
	}
}

func TestReadChecklist_PermissionError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "HEARTBEAT.md")
	os.WriteFile(path, []byte("- Check disk"), 0644)
	os.Chmod(path, 0000)
	defer os.Chmod(path, 0644) // restore for cleanup

	content, err := ReadChecklist(path)
	if err == nil {
		t.Fatal("expected error for unreadable file")
	}
	if content != "" {
		t.Errorf("expected empty content on error, got %q", content)
	}
}

func TestReadChecklist_MaxSize(t *testing.T) {
	dir := t.TempDir()
	big := strings.Repeat("x", 5000)
	os.WriteFile(filepath.Join(dir, "HEARTBEAT.md"), []byte(big), 0644)

	content, err := ReadChecklist(filepath.Join(dir, "HEARTBEAT.md"))
	if err != nil {
		t.Fatal(err)
	}
	if len(content) > maxChecklistChars+100 {
		t.Errorf("expected truncated content, got %d chars", len(content))
	}
}

func TestFormatGoalPrompt(t *testing.T) {
	got := FormatGoalPrompt("## Goals\n- Do stuff")
	if !strings.Contains(got, "periodic check-in") {
		t.Error("goal prompt missing check-in text")
	}
	if !strings.Contains(got, "## Goals") {
		t.Error("goal prompt missing goals content")
	}
}

func TestIsHeartbeatOK_FromMessages(t *testing.T) {
	msgs := []client.Message{
		{Role: "user", Content: client.NewTextContent("check-in prompt")},
		{Role: "assistant", Content: client.NewTextContent("HEARTBEAT_OK")},
	}
	if !IsHeartbeatOKFromMessages(msgs) {
		t.Error("expected OK")
	}
}

func TestIsHeartbeatOK_FromMessages_WithToolCalls(t *testing.T) {
	msgs := []client.Message{
		{Role: "user", Content: client.NewTextContent("check-in prompt")},
		{Role: "assistant", Content: client.NewTextContent("let me check")},
		{Role: "tool", Content: client.NewTextContent("result")},
		{Role: "assistant", Content: client.NewTextContent("HEARTBEAT_OK")},
	}
	if !IsHeartbeatOKFromMessages(msgs) {
		t.Error("expected OK from last assistant message")
	}
}

func TestIsHeartbeatOK_FromMessages_NonOK(t *testing.T) {
	msgs := []client.Message{
		{Role: "user", Content: client.NewTextContent("check-in prompt")},
		{Role: "assistant", Content: client.NewTextContent("User needs a reminder about the video")},
	}
	if IsHeartbeatOKFromMessages(msgs) {
		t.Error("expected non-OK")
	}
}
