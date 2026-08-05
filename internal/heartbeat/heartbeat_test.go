package heartbeat

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/config"
	ctxwin "github.com/Kocoro-lab/ShanClaw/internal/context"
	"github.com/Kocoro-lab/ShanClaw/internal/daemon"
	"github.com/Kocoro-lab/ShanClaw/internal/session"
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

// TestTranscriptCollectorAutoApproves pins the 2026-05-18 policy: heartbeat
// runs auto-approve every tool because the unattended deny-list is empty.
// The plumbing (DisallowsUnattendedAutoApproval call site) is preserved so
// a future entry can be added without rewriting this handler.
func TestTranscriptCollectorAutoApproves(t *testing.T) {
	tc := &TranscriptCollector{}
	for _, tool := range []string{
		"publish_to_web", "generate_image", "edit_image",
		"bash", "file_write",
	} {
		if !tc.OnApprovalNeeded(tool, `{}`) {
			t.Errorf("heartbeat should auto-approve %s (unattended list is empty)", tool)
		}
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

// A heartbeat is an ephemeral continuation of the latest interactive session,
// so it must see the same checkpoint-backed live context as an ordinary turn —
// not the lossless archive the checkpoint replaced. This enters through the
// real heartbeat and RunAgent seams and captures the model request. It also
// pins the ownership boundary: ResolveLatestSession returns a deep snapshot and
// BypassRouting runs in a temporary manager, so the original durable checkpoint
// must remain byte-for-byte intact after the heartbeat.
func TestTickGoalDriven_UsesCompactedLiveHistory(t *testing.T) {
	// GatewayClient attempts streaming first and falls back to non-streaming
	// when this simple JSON fixture has no SSE done event, so leave room for
	// both equivalent requests without blocking the httptest handler.
	requests := make(chan client.CompletionRequest, 2)
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/completions" {
			http.NotFound(w, r)
			return
		}
		var req client.CompletionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode gateway request: %v", err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		requests <- req
		_ = json.NewEncoder(w).Encode(client.CompletionResponse{
			Provider:   "anthropic",
			Model:      "test-model",
			OutputText: "HEARTBEAT_OK",
		})
	}))
	defer gateway.Close()

	shannonDir := t.TempDir()
	agentsDir := filepath.Join(shannonDir, "agents")
	agentDir := filepath.Join(agentsDir, "heartbeat-test")
	if err := os.MkdirAll(agentDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "AGENT.md"), []byte("You are a test agent."), 0o600); err != nil {
		t.Fatal(err)
	}

	sessionsDir := filepath.Join(agentDir, "sessions")
	mgr := session.NewManager(sessionsDir)
	sess := mgr.NewSessionWithID("heartbeat-checkpoint-session")
	sess.Source = "desktop"
	sess.Messages = []client.Message{
		{Role: "user", Content: client.NewTextContent("ARCHIVE_ONLY_OLD_TASK")},
		{Role: "assistant", Content: client.NewTextContent("ARCHIVE_ONLY_OLD_REPLY")},
		{Role: "user", Content: client.NewTextContent("INJECTED_GUARDRAIL_MUST_NOT_REPLAY")},
		{Role: "user", Content: client.NewTextContent("live tail question")},
		{Role: "assistant", Content: client.NewTextContent("live tail answer")},
	}
	sess.MessageMeta = []session.MessageMeta{
		{Source: "desktop"},
		{Source: "desktop"},
		{Source: "desktop", SystemInjected: true},
		{Source: "desktop"},
		{Source: "desktop"},
	}
	sess.CompactionCheckpoint = &session.CompactionCheckpoint{
		SchemaVersion:       session.CompactionCheckpointSchemaVersion,
		ArchiveThroughIndex: 2,
		Messages: []client.Message{
			{Role: "user", Content: client.NewTextContent("stable original primer")},
			{Role: "user", Content: client.NewTextContent(ctxwin.CompactionSummaryPrefix + "stable heartbeat state")},
			{Role: "assistant", Content: client.NewTextContent("compacted recent reply")},
		},
	}
	checkpointBefore, err := json.Marshal(sess.CompactionCheckpoint)
	if err != nil {
		t.Fatal(err)
	}
	if err := mgr.Save(); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Close(); err != nil {
		t.Fatal(err)
	}

	deps := &daemon.ServerDeps{
		Config: &config.Config{
			Provider:  "gateway",
			ModelTier: "medium",
			Agent:     config.AgentConfig{MaxIterations: 2},
		},
		GW:           client.NewGatewayClient(gateway.URL, "test-key"),
		Registry:     agent.NewToolRegistry(),
		BaselineReg:  agent.NewToolRegistry(),
		SessionCache: daemon.NewSessionCache(shannonDir),
		ShannonDir:   shannonDir,
		AgentsDir:    agentsDir,
	}
	defer deps.SessionCache.CloseAll()

	hb := &Manager{deps: deps}
	hb.tickGoalDriven(context.Background(), &agentHeartbeat{
		name:     "heartbeat-test",
		agentDir: agentDir,
	}, "- keep the session healthy", time.Now())

	var captured client.CompletionRequest
	select {
	case captured = <-requests:
	case <-time.After(2 * time.Second):
		t.Fatal("heartbeat made no gateway request")
	}
	joined := make([]string, 0, len(captured.Messages))
	for _, msg := range captured.Messages {
		joined = append(joined, msg.Content.Text())
	}
	requestText := strings.Join(joined, "\n")
	for _, want := range []string{
		"stable original primer",
		ctxwin.CompactionSummaryPrefix + "stable heartbeat state",
		"compacted recent reply",
		"live tail question",
		"live tail answer",
	} {
		if !strings.Contains(requestText, want) {
			t.Errorf("gateway request missing live-history content %q: %s", want, requestText)
		}
	}
	for _, forbidden := range []string{
		"ARCHIVE_ONLY_OLD_TASK",
		"ARCHIVE_ONLY_OLD_REPLY",
		"INJECTED_GUARDRAIL_MUST_NOT_REPLAY",
	} {
		if strings.Contains(requestText, forbidden) {
			t.Errorf("gateway request replayed non-live history %q: %s", forbidden, requestText)
		}
	}

	verify := session.NewManager(sessionsDir)
	defer verify.Close()
	persisted, err := verify.Load("heartbeat-checkpoint-session")
	if err != nil {
		t.Fatal(err)
	}
	checkpointAfter, err := json.Marshal(persisted.CompactionCheckpoint)
	if err != nil {
		t.Fatal(err)
	}
	if string(checkpointAfter) != string(checkpointBefore) {
		t.Fatalf("heartbeat mutated durable checkpoint\nbefore=%s\nafter=%s", checkpointBefore, checkpointAfter)
	}
	if len(persisted.Messages) != 5 {
		t.Fatalf("HEARTBEAT_OK mutated durable archive: %d messages", len(persisted.Messages))
	}
}
