package daemon

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/mcp"
	"github.com/Kocoro-lab/ShanClaw/internal/session"
)

func TestRunAgentRequest_Validate_EmptyText(t *testing.T) {
	req := RunAgentRequest{Text: ""}
	if err := req.Validate(); err == nil {
		t.Fatal("expected error for empty text")
	}
}

func TestRunAgentRequest_Validate_WhitespaceOnly(t *testing.T) {
	req := RunAgentRequest{Text: "   "}
	if err := req.Validate(); err == nil {
		t.Fatal("expected error for whitespace-only text")
	}
}

func TestRunAgentRequest_Validate_NonEmpty(t *testing.T) {
	req := RunAgentRequest{Text: "hello"}
	if err := req.Validate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRunAgentRequest_Validate_WithAgent(t *testing.T) {
	req := RunAgentRequest{Text: "do something", Agent: "ops-bot"}
	if err := req.Validate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRunAgentRequest_Validate_WithSessionID(t *testing.T) {
	req := RunAgentRequest{Text: "do something", SessionID: "sess-123"}
	if err := req.Validate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRunAgentRequest_Ephemeral(t *testing.T) {
	req := RunAgentRequest{
		Text:      "test",
		Agent:     "test-agent",
		Source:    "heartbeat",
		Ephemeral: true,
	}
	if err := req.Validate(); err != nil {
		t.Fatalf("valid ephemeral request should not fail: %v", err)
	}
}

func TestRunAgentRequest_ModelOverride(t *testing.T) {
	req := RunAgentRequest{
		Text:          "test",
		ModelOverride: "small",
	}
	if err := req.Validate(); err != nil {
		t.Fatalf("valid model override request should not fail: %v", err)
	}
}

func TestRunAgentRequest_Validate_WithValidCWD(t *testing.T) {
	req := RunAgentRequest{
		Text: "test",
		CWD:  t.TempDir(),
	}
	if err := req.Validate(); err != nil {
		t.Fatalf("valid cwd request should not fail: %v", err)
	}
}

func TestRunAgentRequest_Validate_WithInvalidCWD(t *testing.T) {
	req := RunAgentRequest{
		Text: "test",
		CWD:  "/nonexistent/path/for/inject-validation",
	}
	if err := req.Validate(); err == nil {
		t.Fatal("expected invalid cwd error")
	}
}

func TestComputeRouteKey_BypassRouting(t *testing.T) {
	req := RunAgentRequest{Agent: "my-agent", BypassRouting: true}
	if got := ComputeRouteKey(req); got != "" {
		t.Errorf("ComputeRouteKey with BypassRouting=true returned %q, want empty", got)
	}
}

func TestComputeRouteKey_AgentWithoutBypass(t *testing.T) {
	req := RunAgentRequest{Agent: "my-agent"}
	if got := ComputeRouteKey(req); got != "agent:my-agent" {
		t.Errorf("ComputeRouteKey returned %q, want %q", got, "agent:my-agent")
	}
}

func TestRouteTitle(t *testing.T) {
	tests := []struct {
		source, channel, sender, want string
	}{
		{"slack", "slack", "Wayland", "Slack · Wayland"},
		{"slack", "slack", "", "Slack"},
		{"line", "line", "Tanaka", "Line · Tanaka"},
		{"feishu", "feishu", "", "Feishu"},
		{"slack", "#general", "", "Slack · #general"},
		{"slack", "#general", "Alice", "Slack · Alice"},
		{"webhook", "hook-1", "", "Webhook · hook-1"},
		{"", "slack", "Wayland", ""},
		{"slack", "", "Wayland", "Slack · Wayland"},
		{"", "", "", ""},
	}
	for _, tt := range tests {
		got := routeTitle(tt.source, tt.channel, tt.sender)
		if got != tt.want {
			t.Errorf("routeTitle(%q, %q, %q) = %q, want %q",
				tt.source, tt.channel, tt.sender, got, tt.want)
		}
	}
}

func TestOutputFormatForSource(t *testing.T) {
	tests := []struct {
		source string
		want   string
	}{
		// Cloud-distributed channel sources → plain
		{"slack", "plain"},
		{"line", "plain"},
		{"webhook", "plain"},
		{"feishu", "plain"},
		{"lark", "plain"},
		{"telegram", "plain"},
		{"Slack", "plain"}, // case-insensitive
		{"LINE", "plain"},  // case-insensitive
		// Everything else → markdown (local, cron, schedule, web, unknown)
		{"shanclaw", "markdown"},
		{"desktop", "markdown"},
		{"web", "markdown"},
		{"cron", "markdown"},
		{"schedule", "markdown"},
		{"heartbeat", "markdown"},
		{"", "markdown"},
		{"unknown", "markdown"},
		{"custom-bot", "markdown"},
	}
	for _, tt := range tests {
		got := outputFormatForSource(tt.source)
		if got != tt.want {
			t.Errorf("outputFormatForSource(%q) = %q, want %q", tt.source, got, tt.want)
		}
	}
}

func TestRunAgentRequestSource(t *testing.T) {
	req := RunAgentRequest{
		Text:   "hello",
		Agent:  "test",
		Source: "slack",
	}
	data, _ := json.Marshal(req)
	var decoded RunAgentRequest
	json.Unmarshal(data, &decoded)
	if decoded.Source != "slack" {
		t.Fatalf("expected source 'slack', got %q", decoded.Source)
	}
}

// context.Canceled and context.DeadlineExceeded must be treated as soft errors
// (like ErrMaxIterReached) so the full conversation from RunMessages() is
// persisted, not just a friendly error stub.
func TestIsSoftRunError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"context.Canceled", context.Canceled, true},
		{"context.DeadlineExceeded", context.DeadlineExceeded, true},
		{"ErrMaxIterReached", agent.ErrMaxIterReached, true},
		{"ErrHardIdleTimeout", agent.ErrHardIdleTimeout, true},
		{"wrapped ErrHardIdleTimeout", fmt.Errorf("turn aborted: %w", agent.ErrHardIdleTimeout), true},
		{"wrapped Canceled", errors.Join(errors.New("loop"), context.Canceled), true},
		{"random error", errors.New("something broke"), false},
		{"API error", errors.New("429 rate limited"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isSoftRunError(tt.err)
			if got != tt.want {
				t.Errorf("isSoftRunError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestResumeNamedAgentColdStart_ResumesPersistedEmptySession(t *testing.T) {
	sessionsDir := t.TempDir()
	storedCWD := t.TempDir()
	store := session.NewStore(sessionsDir)
	if err := store.Save(&session.Session{
		ID:    "persisted-empty",
		Title: "Persisted empty session",
		CWD:   storedCWD,
	}); err != nil {
		t.Fatalf("save session: %v", err)
	}

	mgr := session.NewManager(sessionsDir)
	resumed, err := resumeNamedAgentColdStart(mgr)
	if err != nil {
		t.Fatalf("resumeNamedAgentColdStart error: %v", err)
	}
	if !resumed {
		t.Fatal("expected persisted empty session to count as resumed")
	}
	if got := mgr.Current(); got == nil || got.CWD != storedCWD {
		t.Fatalf("expected stored CWD %q, got %#v", storedCWD, got)
	}
}

func TestResumeNamedAgentColdStart_NoPersistedSessionKeepsFreshCurrent(t *testing.T) {
	sessionsDir := t.TempDir()
	mgr := session.NewManager(sessionsDir)
	fresh := mgr.NewSession()

	resumed, err := resumeNamedAgentColdStart(mgr)
	if err != nil {
		t.Fatalf("resumeNamedAgentColdStart error: %v", err)
	}
	if resumed {
		t.Fatal("expected no persisted session to remain fresh")
	}
	if got := mgr.Current(); got == nil || got.ID != fresh.ID {
		t.Fatalf("expected fresh current session %q to be preserved, got %#v", fresh.ID, got)
	}
}

func TestResolveContentBlocks_TextAndImage(t *testing.T) {
	blocks := []RequestContentBlock{
		{Type: "text", Text: "hello"},
		{Type: "image", Source: &client.ImageSource{Type: "base64", MediaType: "image/png", Data: "abc123"}},
	}
	resolved := resolveContentBlocks(blocks)
	if len(resolved) != 2 {
		t.Fatalf("expected 2 blocks, got %d", len(resolved))
	}
	if resolved[0].Type != "text" || resolved[0].Text != "hello" {
		t.Errorf("text block mismatch: %+v", resolved[0])
	}
	if resolved[1].Type != "image" || resolved[1].Source == nil || resolved[1].Source.Data != "abc123" {
		t.Errorf("image block mismatch: %+v", resolved[1])
	}
}

func TestResolveContentBlocks_FileRef(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.txt")
	os.WriteFile(path, []byte("file content here"), 0644)

	blocks := []RequestContentBlock{
		{Type: "file_ref", FilePath: path, Filename: "test.txt", ByteSize: 17},
	}
	resolved := resolveContentBlocks(blocks)
	if len(resolved) != 1 {
		t.Fatalf("expected 1 block, got %d", len(resolved))
	}
	if resolved[0].Type != "text" {
		t.Fatalf("expected text type, got %s", resolved[0].Type)
	}
	expected := "[User attached file: test.txt (17 bytes) at path: " + path + " — use the file_read tool to read its contents]"
	if resolved[0].Text != expected {
		t.Errorf("file ref text mismatch:\ngot:  %q\nwant: %q", resolved[0].Text, expected)
	}
}

func TestResolveContentBlocks_ImageFileRef(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "photo.png")
	raw := []byte("fake-png-data")
	if err := os.WriteFile(path, raw, 0644); err != nil {
		t.Fatalf("write image: %v", err)
	}

	blocks := []RequestContentBlock{
		{Type: "file_ref", FilePath: path, Filename: "photo.png", ByteSize: int64(len(raw))},
	}
	resolved := resolveContentBlocks(blocks)
	if len(resolved) != 2 {
		t.Fatalf("expected 2 blocks, got %d", len(resolved))
	}
	if resolved[0].Type != "text" {
		t.Fatalf("expected first block to be text, got %s", resolved[0].Type)
	}
	expectedText := "[User attached image: photo.png (" + strconv.Itoa(len(raw)) + " bytes) at path: " + path + " — the image is included inline below for vision. Use the path if a tool needs the original file.]"
	if resolved[0].Text != expectedText {
		t.Errorf("image file ref text mismatch:\ngot:  %q\nwant: %q", resolved[0].Text, expectedText)
	}
	if resolved[1].Type != "image" || resolved[1].Source == nil {
		t.Fatalf("expected second block to be image, got %+v", resolved[1])
	}
	if resolved[1].Source.MediaType != "image/png" {
		t.Fatalf("expected image/png, got %q", resolved[1].Source.MediaType)
	}
	if resolved[1].Source.Data != base64.StdEncoding.EncodeToString(raw) {
		t.Errorf("image data mismatch: got %q", resolved[1].Source.Data)
	}
}

func TestResolveContentBlocks_FileRefMissing(t *testing.T) {
	blocks := []RequestContentBlock{
		{Type: "file_ref", FilePath: "/nonexistent/path/file.log", Filename: "file.log"},
	}
	resolved := resolveContentBlocks(blocks)
	if len(resolved) != 1 {
		t.Fatalf("expected 1 block, got %d", len(resolved))
	}
	if resolved[0].Type != "text" {
		t.Fatalf("expected text type, got %s", resolved[0].Type)
	}
	expected := "[User attached file: file.log (0 bytes) at path: /nonexistent/path/file.log — use the file_read tool to read its contents]"
	if resolved[0].Text != expected {
		t.Errorf("error text mismatch:\ngot:  %q\nwant: %q", resolved[0].Text, expected)
	}
}

func TestResolveContentBlocks_UnknownTypeSkipped(t *testing.T) {
	blocks := []RequestContentBlock{
		{Type: "text", Text: "keep"},
		{Type: "unknown_type", Text: "skip"},
	}
	resolved := resolveContentBlocks(blocks)
	if len(resolved) != 1 {
		t.Fatalf("expected 1 block (unknown skipped), got %d", len(resolved))
	}
	if resolved[0].Text != "keep" {
		t.Errorf("expected 'keep', got %q", resolved[0].Text)
	}
}

func TestRunAgentRequest_ContentJSON(t *testing.T) {
	raw := `{
		"text": "analyze this",
		"content": [
			{"type": "text", "text": "describe the image"},
			{"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": "iVBOR"}}
		],
		"source": "shanclaw"
	}`
	var req RunAgentRequest
	if err := json.Unmarshal([]byte(raw), &req); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if req.Text != "analyze this" {
		t.Errorf("text mismatch: %q", req.Text)
	}
	if len(req.Content) != 2 {
		t.Fatalf("expected 2 content blocks, got %d", len(req.Content))
	}
	if req.Content[0].Type != "text" || req.Content[0].Text != "describe the image" {
		t.Errorf("content[0] mismatch: %+v", req.Content[0])
	}
	if req.Content[1].Type != "image" || req.Content[1].Source == nil || req.Content[1].Source.Data != "iVBOR" {
		t.Errorf("content[1] mismatch: %+v", req.Content[1])
	}
}

func TestRunAgentRequest_NoContent(t *testing.T) {
	raw := `{"text": "just text"}`
	var req RunAgentRequest
	if err := json.Unmarshal([]byte(raw), &req); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if req.Text != "just text" {
		t.Errorf("text mismatch: %q", req.Text)
	}
	if req.Content != nil {
		t.Errorf("expected nil content, got %v", req.Content)
	}
}

func TestExtractUserFilePaths(t *testing.T) {
	blocks := []RequestContentBlock{
		{Type: "text", Text: "analyze these"},
		{Type: "file_ref", FilePath: "/tmp/report.pdf", Filename: "report.pdf"},
		{Type: "image", Source: &client.ImageSource{Type: "base64", MediaType: "image/png", Data: "abc"}},
		{Type: "file_ref", FilePath: "/tmp/data.csv", Filename: "data.csv"},
	}
	paths := extractUserFilePaths(blocks)
	if len(paths) != 2 {
		t.Fatalf("expected 2 paths, got %d: %v", len(paths), paths)
	}
	if paths[0] != "/tmp/report.pdf" || paths[1] != "/tmp/data.csv" {
		t.Errorf("unexpected paths: %v", paths)
	}
}

func TestExtractUserFilePaths_Empty(t *testing.T) {
	paths := extractUserFilePaths(nil)
	if len(paths) != 0 {
		t.Errorf("expected empty, got %v", paths)
	}
	paths = extractUserFilePaths([]RequestContentBlock{{Type: "text", Text: "hello"}})
	if len(paths) != 0 {
		t.Errorf("expected empty for text-only, got %v", paths)
	}
}

func TestCleanupPlaywrightAfterTurn_CDPOnDemandStopsBrowser(t *testing.T) {
	mgr := mcp.NewClientManager()
	mgr.SeedConfig("playwright", mcp.MCPServerConfig{
		Command:   "dummy",
		Args:      []string{"--cdp-endpoint", "http://127.0.0.1:9223"},
		KeepAlive: false,
	})

	oldIdle := disconnectPlaywrightAfterIdleFn
	oldNow := disconnectPlaywrightNowFn
	oldStop := stopPlaywrightChromeFn
	defer func() {
		disconnectPlaywrightAfterIdleFn = oldIdle
		disconnectPlaywrightNowFn = oldNow
		stopPlaywrightChromeFn = oldStop
	}()

	idleCalls := 0
	nowCalls := 0
	stopCalls := 0
	disconnectPlaywrightAfterIdleFn = func(*mcp.ClientManager, time.Duration) { idleCalls++ }
	disconnectPlaywrightNowFn = func(*mcp.ClientManager) { nowCalls++ }
	stopPlaywrightChromeFn = func() { stopCalls++ }

	cleanupPlaywrightAfterTurn(mgr)

	if idleCalls != 0 {
		t.Fatalf("expected no idle disconnect scheduling, got %d", idleCalls)
	}
	if nowCalls != 1 {
		t.Fatalf("expected immediate disconnect once, got %d", nowCalls)
	}
	if stopCalls != 1 {
		t.Fatalf("expected dedicated Chrome stop once, got %d", stopCalls)
	}
}

func TestCleanupPlaywrightAfterTurn_KeepAliveLeavesBrowserRunning(t *testing.T) {
	mgr := mcp.NewClientManager()
	mgr.SeedConfig("playwright", mcp.MCPServerConfig{
		Command:   "dummy",
		Args:      []string{"--cdp-endpoint", "http://127.0.0.1:9223"},
		KeepAlive: true,
	})

	oldIdle := disconnectPlaywrightAfterIdleFn
	oldNow := disconnectPlaywrightNowFn
	oldStop := stopPlaywrightChromeFn
	defer func() {
		disconnectPlaywrightAfterIdleFn = oldIdle
		disconnectPlaywrightNowFn = oldNow
		stopPlaywrightChromeFn = oldStop
	}()

	idleCalls := 0
	nowCalls := 0
	stopCalls := 0
	disconnectPlaywrightAfterIdleFn = func(*mcp.ClientManager, time.Duration) { idleCalls++ }
	disconnectPlaywrightNowFn = func(*mcp.ClientManager) { nowCalls++ }
	stopPlaywrightChromeFn = func() { stopCalls++ }

	cleanupPlaywrightAfterTurn(mgr)

	if idleCalls != 0 || nowCalls != 0 || stopCalls != 0 {
		t.Fatalf("expected no teardown while keepAlive=true, got idle=%d disconnect=%d stop=%d", idleCalls, nowCalls, stopCalls)
	}
}

func TestCleanupPlaywrightAfterTurn_NonCDPUsesIdleDisconnect(t *testing.T) {
	mgr := mcp.NewClientManager()
	mgr.SeedConfig("playwright", mcp.MCPServerConfig{
		Command:   "dummy",
		Args:      []string{"--some-stdio-mode"},
		KeepAlive: false,
	})

	oldIdle := disconnectPlaywrightAfterIdleFn
	oldNow := disconnectPlaywrightNowFn
	oldStop := stopPlaywrightChromeFn
	defer func() {
		disconnectPlaywrightAfterIdleFn = oldIdle
		disconnectPlaywrightNowFn = oldNow
		stopPlaywrightChromeFn = oldStop
	}()

	idleCalls := 0
	var idleDuration time.Duration
	nowCalls := 0
	stopCalls := 0
	disconnectPlaywrightAfterIdleFn = func(_ *mcp.ClientManager, d time.Duration) {
		idleCalls++
		idleDuration = d
	}
	disconnectPlaywrightNowFn = func(*mcp.ClientManager) { nowCalls++ }
	stopPlaywrightChromeFn = func() { stopCalls++ }

	cleanupPlaywrightAfterTurn(mgr)

	if idleCalls != 1 {
		t.Fatalf("expected idle disconnect scheduling once, got %d", idleCalls)
	}
	if idleDuration != 5*time.Minute {
		t.Fatalf("expected 5m idle disconnect, got %v", idleDuration)
	}
	if nowCalls != 0 || stopCalls != 0 {
		t.Fatalf("expected no immediate teardown in non-CDP mode, got disconnect=%d stop=%d", nowCalls, stopCalls)
	}
}
