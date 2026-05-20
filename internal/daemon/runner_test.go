package daemon

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/config"
	"github.com/Kocoro-lab/ShanClaw/internal/mcp"
	"github.com/Kocoro-lab/ShanClaw/internal/session"
)

func TestCacheSourceFromDaemonSource(t *testing.T) {
	cases := []struct {
		source string
		want   string
	}{
		{"slack", "slack"},
		{"Slack", "slack"},
		{"  line  ", "line"},
		{"feishu", "feishu"},
		{"wecom", "wecom"},
		{"telegram", "telegram"},
		{"tui", "tui"},
		{"shanclaw", "shanclaw"},
		// Empty source is defaulted to "kocoro" in server.go before reaching
		// this function; the dedicated empty-string case was removed. Falls
		// through to "unknown" (5m) defensively in case the default is ever
		// bypassed — matches the fail-cheap policy documented in
		// docs/cache-strategy.md.
		{"", "unknown"},
		{"webhook", "webhook"},
		{"cron", "cron"},
		{"schedule", "schedule"},
		{"mcp", "mcp"},
		{"cache_bench", "cache_bench"},
		{"never-classified-source", "unknown"},
	}
	for _, c := range cases {
		if got := cacheSourceFromDaemonSource(c.source); got != c.want {
			t.Errorf("cacheSourceFromDaemonSource(%q) = %q, want %q", c.source, got, c.want)
		}
	}
}

func TestIsMessagingPlatform(t *testing.T) {
	cases := []struct {
		source string
		want   bool
	}{
		// Messaging platforms — gateway delivers explicit AgentName.
		{"slack", true},
		{"feishu", true},
		{"lark", true},
		{"wecom", true},
		{"line", true},
		{"wechat", true},
		{"teams", true},
		{"discord", true},
		{"telegram", true},
		// Case + whitespace normalization.
		{"WeCom", true},
		{"  SLACK  ", true},
		{"Telegram", true},
		// Non-messaging sources — @mention parsing remains valid here.
		{"tui", false},
		{"shanclaw", false},
		{"webhook", false},
		{"cron", false},
		{"schedule", false},
		{"mcp", false},
		{"web", false},
		{"", false},
		{"never-classified-source", false},
	}
	for _, c := range cases {
		if got := IsMessagingPlatform(c.source); got != c.want {
			t.Errorf("IsMessagingPlatform(%q) = %v, want %v", c.source, got, c.want)
		}
	}
}

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

func TestComputeRouteKey_WebhookWithNamedAgentBypassesRoute(t *testing.T) {
	req := RunAgentRequest{Agent: "ops-bot", Source: "webhook", Channel: "github"}
	if got := ComputeRouteKey(req); got != "" {
		t.Errorf("ComputeRouteKey returned %q, want empty route", got)
	}
}

func TestComputeRouteKey_ScheduleWithNamedAgentKeepsAgentRoute(t *testing.T) {
	req := RunAgentRequest{Agent: "ops-bot", Source: ChannelSchedule, Channel: "schedule-daily"}
	if got := ComputeRouteKey(req); got != "agent:ops-bot" {
		t.Errorf("ComputeRouteKey returned %q, want %q", got, "agent:ops-bot")
	}
}

func TestComputeRouteKey_MessagingPlatformThreadRouting(t *testing.T) {
	tests := []struct {
		name string
		req  RunAgentRequest
		want string
	}{
		{
			name: "wecom group default agent",
			req:  RunAgentRequest{Source: ChannelWeCom, Channel: ChannelWeCom, ThreadID: "g:group-a"},
			want: "default:wecom:g:group-a",
		},
		{
			name: "wecom dm default agent",
			req:  RunAgentRequest{Source: ChannelWeCom, Channel: ChannelWeCom, ThreadID: "u:user-a"},
			want: "default:wecom:u:user-a",
		},
		{
			name: "slack thread default agent",
			req:  RunAgentRequest{Source: ChannelSlack, Channel: ChannelSlack, ThreadID: "C123-1710000000.000100"},
			want: "default:slack:C123-1710000000.000100",
		},
		{
			name: "wecom group named agent",
			req:  RunAgentRequest{Agent: "ops-bot", Source: ChannelWeCom, Channel: ChannelWeCom, ThreadID: "g:group-a"},
			want: "agent:ops-bot:wecom:g:group-a",
		},
		{
			name: "session id wins over messaging thread",
			req:  RunAgentRequest{Agent: "ops-bot", SessionID: "sess-123", Source: ChannelWeCom, Channel: ChannelWeCom, ThreadID: "g:group-a"},
			want: "session:sess-123",
		},
		{
			name: "messaging source without thread keeps old fallback",
			req:  RunAgentRequest{Source: ChannelSlack, Channel: "#general"},
			want: "default:slack:%23general",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ComputeRouteKey(tt.req); got != tt.want {
				t.Errorf("ComputeRouteKey(%+v) = %q, want %q", tt.req, got, tt.want)
			}
		})
	}
}

func TestResumeRoutedColdStart_UsesPersistedRouteKey(t *testing.T) {
	dir := t.TempDir()
	mgr := session.NewManager(dir)
	defer mgr.Close()

	sess := mgr.NewSession()
	sess.RouteKey = "default:slack:C123-1710000000.000100"
	sess.Messages = []client.Message{{Role: "user", Content: client.NewTextContent("deploy process")}}
	if err := mgr.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	mgr2 := session.NewManager(dir)
	defer mgr2.Close()
	resumed, err := resumeRoutedColdStart(mgr2, "default:slack:C123-1710000000.000100")
	if err != nil {
		t.Fatalf("resumeRoutedColdStart: %v", err)
	}
	if !resumed {
		t.Fatal("expected route cold start to resume")
	}
	current := mgr2.Current()
	if current == nil || current.ID != sess.ID {
		t.Fatalf("current session = %#v, want %q", current, sess.ID)
	}
}

func TestIsPlainAgentRouteKey(t *testing.T) {
	tests := []struct {
		key  string
		want bool
	}{
		{"agent:ops-bot", true},
		{"agent:ops-bot:wecom:g:group-a", false},
		{"default:wecom:g:group-a", false},
		{"session:sess-123", false},
	}
	for _, tt := range tests {
		if got := isPlainAgentRouteKey(tt.key); got != tt.want {
			t.Errorf("isPlainAgentRouteKey(%q) = %v, want %v", tt.key, got, tt.want)
		}
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
		{"desktop", "shanclaw", "", ""},
		{"shanclaw", "shanclaw", "", ""},
		{"kocoro", "shanclaw", "", ""},
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
		{"wecom", "plain"},
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
	if paths[0].Path != "/tmp/report.pdf" || paths[1].Path != "/tmp/data.csv" {
		t.Errorf("unexpected paths: %v", paths)
	}
	if paths[0].IsDir || paths[1].IsDir {
		t.Errorf("expected IsDir=false for missing/regular paths, got %v", paths)
	}
}

func TestExtractUserFilePaths_DetectsDirectory(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "x.txt")
	if err := os.WriteFile(file, []byte("hi"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	blocks := []RequestContentBlock{
		{Type: "file_ref", FilePath: dir, Filename: filepath.Base(dir)},
		{Type: "file_ref", FilePath: file, Filename: "x.txt"},
	}
	paths := extractUserFilePaths(blocks)
	if len(paths) != 2 {
		t.Fatalf("expected 2 paths, got %d", len(paths))
	}
	if !paths[0].IsDir {
		t.Errorf("expected directory entry IsDir=true, got %+v", paths[0])
	}
	if paths[1].IsDir {
		t.Errorf("expected file entry IsDir=false, got %+v", paths[1])
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
	oldStopWait := stopPlaywrightChromeAndWaitFn
	defer func() {
		disconnectPlaywrightAfterIdleFn = oldIdle
		disconnectPlaywrightNowFn = oldNow
		stopPlaywrightChromeAndWaitFn = oldStopWait
	}()

	idleCalls := 0
	nowCalls := 0
	stopCalls := 0
	disconnectPlaywrightAfterIdleFn = func(*mcp.ClientManager, time.Duration) { idleCalls++ }
	disconnectPlaywrightNowFn = func(*mcp.ClientManager) { nowCalls++ }
	stopPlaywrightChromeAndWaitFn = func(context.Context) error { stopCalls++; return nil }

	ctx := mcp.WithChromeUseLease(context.Background())
	mcp.MarkChromeUsed(ctx) // simulate a browser tool ran this Run

	cleanupPlaywrightAfterTurn(ctx, mgr)

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
	oldStopWait := stopPlaywrightChromeAndWaitFn
	defer func() {
		disconnectPlaywrightAfterIdleFn = oldIdle
		disconnectPlaywrightNowFn = oldNow
		stopPlaywrightChromeAndWaitFn = oldStopWait
	}()

	idleCalls := 0
	nowCalls := 0
	stopCalls := 0
	disconnectPlaywrightAfterIdleFn = func(*mcp.ClientManager, time.Duration) { idleCalls++ }
	disconnectPlaywrightNowFn = func(*mcp.ClientManager) { nowCalls++ }
	stopPlaywrightChromeAndWaitFn = func(context.Context) error { stopCalls++; return nil }

	ctx := mcp.WithChromeUseLease(context.Background())
	mcp.MarkChromeUsed(ctx)

	cleanupPlaywrightAfterTurn(ctx, mgr)

	if idleCalls != 0 || nowCalls != 0 || stopCalls != 0 {
		t.Fatalf("expected no teardown while keepAlive=true, got idle=%d disconnect=%d stop=%d", idleCalls, nowCalls, stopCalls)
	}
	// But the lease counter must return to 0 (no leak).
	if got := mcp.GlobalChromeTrackerActiveCountForTest(); got != 0 {
		t.Fatalf("expected counter back to 0 after keep_alive release, got %d", got)
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
	oldStopWait := stopPlaywrightChromeAndWaitFn
	defer func() {
		disconnectPlaywrightAfterIdleFn = oldIdle
		disconnectPlaywrightNowFn = oldNow
		stopPlaywrightChromeAndWaitFn = oldStopWait
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
	stopPlaywrightChromeAndWaitFn = func(context.Context) error { stopCalls++; return nil }

	// No MarkChromeUsed — non-CDP idle-disconnect runs regardless.
	ctx := mcp.WithChromeUseLease(context.Background())

	cleanupPlaywrightAfterTurn(ctx, mgr)

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

func TestCleanupPlaywrightAfterTurn_NonCDPReleasesLease(t *testing.T) {
	assertGlobalChromeTrackerClean(t)

	mgr := mcp.NewClientManager()
	mgr.SeedConfig("playwright", mcp.MCPServerConfig{
		Command:   "dummy",
		Args:      []string{"--some-stdio-mode"},
		KeepAlive: false,
	})

	oldIdle := disconnectPlaywrightAfterIdleFn
	defer func() { disconnectPlaywrightAfterIdleFn = oldIdle }()
	disconnectPlaywrightAfterIdleFn = func(*mcp.ClientManager, time.Duration) {}

	ctx := mcp.WithChromeUseLease(context.Background())
	lease := mcp.ChromeUseLeaseFrom(ctx)
	if lease == nil {
		t.Fatal("expected lease installed")
	}
	defer lease.ReleaseOnly()
	mcp.MarkChromeUsed(ctx)

	cleanupPlaywrightAfterTurn(ctx, mgr)

	if got := mcp.GlobalChromeTrackerActiveCountForTest(); got != 0 {
		t.Fatalf("expected non-CDP cleanup to release stale lease, got count=%d", got)
	}
}

func TestCleanupPlaywrightAfterTurn_ReleasesLeaseWhenConfigMissing(t *testing.T) {
	assertGlobalChromeTrackerClean(t)

	mgr := mcp.NewClientManager()
	ctx := mcp.WithChromeUseLease(context.Background())
	lease := mcp.ChromeUseLeaseFrom(ctx)
	if lease == nil {
		t.Fatal("expected lease installed")
	}
	defer lease.ReleaseOnly()
	mcp.MarkChromeUsed(ctx)

	cleanupPlaywrightAfterTurn(ctx, mgr)

	if got := mcp.GlobalChromeTrackerActiveCountForTest(); got != 0 {
		t.Fatalf("expected missing-config cleanup to release lease, got count=%d", got)
	}
}

func TestCleanupPlaywrightAfterTurn_CDPSkipsWhenBrowserNotUsed(t *testing.T) {
	mgr := mcp.NewClientManager()
	mgr.SeedConfig("playwright", mcp.MCPServerConfig{
		Command:   "dummy",
		Args:      []string{"--cdp-endpoint", "http://127.0.0.1:9223"},
		KeepAlive: false,
	})

	oldNow := disconnectPlaywrightNowFn
	oldStopWait := stopPlaywrightChromeAndWaitFn
	defer func() {
		disconnectPlaywrightNowFn = oldNow
		stopPlaywrightChromeAndWaitFn = oldStopWait
	}()

	nowCalls := 0
	stopCalls := 0
	disconnectPlaywrightNowFn = func(*mcp.ClientManager) { nowCalls++ }
	stopPlaywrightChromeAndWaitFn = func(context.Context) error { stopCalls++; return nil }

	ctx := mcp.WithChromeUseLease(context.Background())
	// NO MarkChromeUsed call — Run never touched browser.
	cleanupPlaywrightAfterTurn(ctx, mgr)

	if nowCalls != 0 || stopCalls != 0 {
		t.Fatalf("expected no teardown when browser not used, got disconnect=%d stop=%d", nowCalls, stopCalls)
	}
}

func TestCleanupPlaywrightAfterTurn_ConcurrentRunsDeferTeardown(t *testing.T) {
	mgr := mcp.NewClientManager()
	mgr.SeedConfig("playwright", mcp.MCPServerConfig{
		Command:   "dummy",
		Args:      []string{"--cdp-endpoint", "http://127.0.0.1:9223"},
		KeepAlive: false,
	})

	oldNow := disconnectPlaywrightNowFn
	oldStopWait := stopPlaywrightChromeAndWaitFn
	defer func() {
		disconnectPlaywrightNowFn = oldNow
		stopPlaywrightChromeAndWaitFn = oldStopWait
	}()

	stopCalls := 0
	disconnectPlaywrightNowFn = func(*mcp.ClientManager) {}
	stopPlaywrightChromeAndWaitFn = func(context.Context) error { stopCalls++; return nil }

	ctxA := mcp.WithChromeUseLease(context.Background())
	mcp.MarkChromeUsed(ctxA)
	ctxB := mcp.WithChromeUseLease(context.Background())
	mcp.MarkChromeUsed(ctxB)

	cleanupPlaywrightAfterTurn(ctxA, mgr)
	if stopCalls != 0 {
		t.Fatalf("expected no stop after first cleanup (another Run still active), got %d", stopCalls)
	}

	cleanupPlaywrightAfterTurn(ctxB, mgr)
	if stopCalls != 1 {
		t.Fatalf("expected stop after last cleanup, got %d", stopCalls)
	}
}

func TestCleanupPlaywrightAfterTurn_UsesIndependentContext(t *testing.T) {
	mgr := mcp.NewClientManager()
	mgr.SeedConfig("playwright", mcp.MCPServerConfig{
		Command:   "dummy",
		Args:      []string{"--cdp-endpoint", "http://127.0.0.1:9223"},
		KeepAlive: false,
	})

	oldNow := disconnectPlaywrightNowFn
	oldStopWait := stopPlaywrightChromeAndWaitFn
	defer func() {
		disconnectPlaywrightNowFn = oldNow
		stopPlaywrightChromeAndWaitFn = oldStopWait
	}()

	disconnectPlaywrightNowFn = func(*mcp.ClientManager) {}

	// Capture ctx state synchronously inside the stop callback, while ctx is
	// still valid. After cleanupPlaywrightAfterTurn returns, its defer cancel()
	// fires and ctx.Err() would flip to context.Canceled — the captured facts
	// below are the only reliable observation point.
	var observedErr error
	var observedHasDeadline bool
	var observedRemaining time.Duration
	stopPlaywrightChromeAndWaitFn = func(ctx context.Context) error {
		observedErr = ctx.Err()
		deadline, hasDeadline := ctx.Deadline()
		observedHasDeadline = hasDeadline
		if hasDeadline {
			observedRemaining = time.Until(deadline)
		}
		return nil
	}

	parentCtx, parentCancel := context.WithCancel(context.Background())
	ctx := mcp.WithChromeUseLease(parentCtx)
	mcp.MarkChromeUsed(ctx)
	parentCancel() // cancel BEFORE cleanup runs

	cleanupPlaywrightAfterTurn(ctx, mgr)

	if observedErr != nil {
		t.Fatalf("expected cleanup ctx to NOT inherit parent cancellation, got err=%v", observedErr)
	}
	if !observedHasDeadline {
		t.Fatal("expected cleanup ctx to carry a deadline")
	}
	if observedRemaining <= 0 {
		t.Fatalf("expected cleanup ctx deadline to be in the future, got remaining=%v", observedRemaining)
	}
}

func assertGlobalChromeTrackerClean(t *testing.T) {
	t.Helper()
	if got := mcp.GlobalChromeTrackerActiveCountForTest(); got != 0 {
		t.Fatalf("global chrome tracker leaked count=%d from a prior test", got)
	}
}

// fakeGatewayBackend is a minimal httptest server stub for fireSuggestionAfterRun
// tests. It captures every CompletionRequest the daemon sends and returns a
// caller-supplied reply text.
type fakeGatewayBackend struct {
	mu       sync.Mutex
	captured []client.CompletionRequest
	reply    string
}

func (g *fakeGatewayBackend) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req client.CompletionRequest
		_ = json.Unmarshal(body, &req)
		g.mu.Lock()
		g.captured = append(g.captured, req)
		reply := g.reply
		g.mu.Unlock()
		resp := client.CompletionResponse{
			Provider:   "anthropic",
			Model:      "test-model",
			OutputText: reply,
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}
}

func (g *fakeGatewayBackend) requests() []client.CompletionRequest {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]client.CompletionRequest, len(g.captured))
	copy(out, g.captured)
	return out
}

// TestFireSuggestionAfterRun_AppendsAssistantReplyToMain verifies the GPT
// review P0 fix: forked suggestion request must include the just-completed
// assistant reply, otherwise the model predicts the user's "next" message
// without seeing what the assistant actually said.
func TestFireSuggestionAfterRun_AppendsAssistantReplyToMain(t *testing.T) {
	gw := &fakeGatewayBackend{reply: "run the failing test"}
	ts := httptest.NewServer(gw.handler())
	defer ts.Close()

	deps := &ServerDeps{
		GW:          client.NewGatewayClient(ts.URL, "test-key"),
		Suggestions: agent.NewSuggestionState(),
	}

	main := client.CompletionRequest{
		Messages: []client.Message{
			{Role: "user", Content: client.NewTextContent("fix the bug")},
		},
		ModelTier: "medium",
	}

	fireSuggestionAfterRun(context.Background(), deps,
		"test-agent", "sess1",
		main, // SpeculationEnabled removed
		"I'll fix the test in foo.go")

	reqs := gw.requests()
	if len(reqs) != 1 {
		t.Fatalf("expected 1 gateway call, got %d", len(reqs))
	}

	msgs := reqs[0].Messages
	// Expect 3 messages in this order:
	//   [0] user="fix the bug"           (the original main turn input)
	//   [1] assistant="I'll fix the..."  (the just-generated reply — the fix)
	//   [2] user=SuggestionPrompt        (appended by BuildForkedSuggestionRequest)
	if len(msgs) != 3 {
		t.Fatalf("forked request has %d messages, want 3 (user + assistant_reply + SUGGESTION_PROMPT). messages: %+v", len(msgs), msgs)
	}
	if msgs[1].Role != "assistant" {
		t.Errorf("messages[1].Role = %q, want assistant (the just-generated reply)", msgs[1].Role)
	}
	if got := messageText(msgs[1]); got != "I'll fix the test in foo.go" {
		t.Errorf("messages[1] text = %q, want %q (assistant reply)", got, "I'll fix the test in foo.go")
	}

	sug, ok := deps.Suggestions.Get("sess1")
	if !ok || sug.Text != "run the failing test" {
		t.Errorf("SuggestionState entry = %+v, want Text='run the failing test'", sug)
	}
}

// TestFireSuggestionAfterRun_EmptyReplySkipsAll guards against the case
// where loop.Run returned empty text (tool-only turn, partial result).
// Firing a suggestion with no assistant reply produces a misleading
// prediction; skip entirely.
func TestFireSuggestionAfterRun_EmptyReplySkipsAll(t *testing.T) {
	gw := &fakeGatewayBackend{reply: "should never be called"}
	ts := httptest.NewServer(gw.handler())
	defer ts.Close()

	deps := &ServerDeps{
		GW:          client.NewGatewayClient(ts.URL, "test-key"),
		Suggestions: agent.NewSuggestionState(),
	}
	main := client.CompletionRequest{
		Messages:  []client.Message{{Role: "user", Content: client.NewTextContent("hi")}},
		ModelTier: "medium",
	}

	fireSuggestionAfterRun(context.Background(), deps,
		"test-agent", "sess1",
		main,
		"") // empty assistantReply

	if got := len(gw.requests()); got != 0 {
		t.Errorf("gateway called %d times, want 0 (empty reply must skip)", got)
	}
	if _, ok := deps.Suggestions.Get("sess1"); ok {
		t.Error("SuggestionState must remain empty when assistantReply is empty")
	}
}

// messageText extracts the text from a Message's MessageContent for
// assertion purposes. Works across simple-text and multi-block messages
// by falling back to the JSON form if Text() is unavailable.
func messageText(m client.Message) string {
	// MessageContent has Text() helper for text-only payloads.
	if t := m.Content.Text(); t != "" {
		return t
	}
	// Fallback — JSON-encode and let the test assert by substring.
	b, _ := json.Marshal(m.Content)
	return string(b)
}

// TestFireSuggestionAfterRun_StaleGoroutineDoesNotResurrect simulates the
// detached-goroutine race the GPT review flagged as P0/P1: a new turn
// starts (Clear) while the previous turn's suggestion goroutine is still
// blocked on the gateway. The late Set must be dropped, not resurrected.
func TestFireSuggestionAfterRun_StaleGoroutineDoesNotResurrect(t *testing.T) {
	// Gate the fake gateway on a channel so we can interleave Clear()
	// in the middle of the gateway call.
	startResp := make(chan struct{})
	gw := &fakeGatewayBackend{} // reply set just before unblocking
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req client.CompletionRequest
		_ = json.Unmarshal(body, &req)
		gw.mu.Lock()
		gw.captured = append(gw.captured, req)
		gw.mu.Unlock()

		<-startResp // wait for test to clear state and unblock us

		resp := client.CompletionResponse{
			Provider:   "anthropic",
			Model:      "test",
			OutputText: "stale suggestion text",
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer ts.Close()

	deps := &ServerDeps{
		GW:          client.NewGatewayClient(ts.URL, "test"),
		Suggestions: agent.NewSuggestionState(),
	}
	main := client.CompletionRequest{
		Messages:  []client.Message{{Role: "user", Content: client.NewTextContent("hi")}},
		ModelTier: "medium",
	}

	// Fire the suggestion goroutine. It will block in the fake gateway
	// handler until we send on startResp.
	done := make(chan struct{})
	go func() {
		fireSuggestionAfterRun(context.Background(), deps,
			"test-agent", "sess1",
			main,
			"I just replied to you")
		close(done)
	}()

	// Wait briefly to ensure the goroutine has captured CurrentGen
	// (it does so before the gateway call returns).
	time.Sleep(20 * time.Millisecond)

	// Simulate the new-turn lifecycle: Clear bumps the generation.
	deps.Suggestions.Clear("sess1")

	// Unblock the gateway handler. Goroutine now proceeds with its
	// stale-gen SetIfFresh call.
	close(startResp)
	<-done

	if _, ok := deps.Suggestions.Get("sess1"); ok {
		t.Error("stale goroutine resurrected SuggestionState after Clear — race not prevented")
	}
}

func TestRunAgent_NoCleanupBeforeDeferInstalled(t *testing.T) {
	// Misconfigured deps (deps.GW nil) triggers the EARLIEST error return —
	// before RebuildLayers, before the defer is installed. Cleanup MUST NOT
	// fire here, otherwise the defer is placed too early in RunAgent.
	deps := &ServerDeps{}

	oldStopWait := stopPlaywrightChromeAndWaitFn
	oldIdle := disconnectPlaywrightAfterIdleFn
	defer func() {
		stopPlaywrightChromeAndWaitFn = oldStopWait
		disconnectPlaywrightAfterIdleFn = oldIdle
	}()
	stopCalls := 0
	idleCalls := 0
	stopPlaywrightChromeAndWaitFn = func(context.Context) error { stopCalls++; return nil }
	disconnectPlaywrightAfterIdleFn = func(*mcp.ClientManager, time.Duration) { idleCalls++ }

	req := RunAgentRequest{Text: "hi"}
	_, err := RunAgent(context.Background(), deps, req, nil)
	if err == nil {
		t.Fatal("expected error from misconfigured daemon")
	}
	if stopCalls != 0 || idleCalls != 0 {
		t.Fatalf("expected no cleanup on pre-validation error, got stop=%d idle=%d", stopCalls, idleCalls)
	}
}

func TestRunAgent_CleanupFiresOnPostDeferError(t *testing.T) {
	// Construct deps that passes the line-665 validation (Config/GW/SessionCache
	// non-nil) but fails the second-snapshot validation (Registry nil). This
	// proves the defer fires on a real internal error return after agent-loop
	// initialization paths.
	mgr := mcp.NewClientManager()
	mgr.SeedConfig("playwright", mcp.MCPServerConfig{
		Command:   "dummy",
		Args:      []string{"--some-stdio-mode"}, // non-CDP — idle disconnect is observable without MarkUsed
		KeepAlive: false,
	})

	deps := &ServerDeps{
		Config:       &config.Config{},
		GW:           &client.GatewayClient{},
		SessionCache: &SessionCache{},
		MCPManager:   mgr,
		// Registry / BaselineReg intentionally nil — triggers the second-validation error return.
	}

	oldIdle := disconnectPlaywrightAfterIdleFn
	defer func() { disconnectPlaywrightAfterIdleFn = oldIdle }()
	idleCalls := 0
	disconnectPlaywrightAfterIdleFn = func(*mcp.ClientManager, time.Duration) { idleCalls++ }

	_, err := RunAgent(context.Background(), deps, RunAgentRequest{Text: "hi"}, nil)
	if err == nil {
		t.Fatal("expected error from missing Registry")
	}
	// The defer installed BEFORE the second snapshot fires on this error
	// return. Non-CDP + keep_alive=false → idle-disconnect scheduled
	// unconditionally. This proves the defer placement.
	if idleCalls != 1 {
		t.Fatalf("expected deferred cleanup to schedule idle disconnect, got %d", idleCalls)
	}
}

// TestIsSoftRunError_StreamIdleTimeout pins the soft-classification for the
// new sentinel. If this regresses, RunAgent will treat a silent stream drop
// as a hard error: the agent loop's partial reply (captured via streaming
// deltas + emitted as OnRunStatus("stream_idle_timeout") with Partial=true)
// would be overwritten by the FriendlyAgentError stub at runner.go:1617.
// Symptom: agent_reply event missing, user sees "agent error" instead of the
// half-sentence we actually received.
func TestIsSoftRunError_StreamIdleTimeout(t *testing.T) {
	cases := []struct {
		name string
		err  error
		soft bool
	}{
		{"raw stream idle", client.ErrStreamIdleTimeout, true},
		{"wrapped stream idle", fmt.Errorf("stream aborted: %w", client.ErrStreamIdleTimeout), true},
		{"hard idle (existing)", agent.ErrHardIdleTimeout, true},
		{"context canceled (existing)", context.Canceled, true},
		{"random error", fmt.Errorf("upstream 500"), false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isSoftRunError(tc.err); got != tc.soft {
				t.Fatalf("isSoftRunError(%v) = %v, want %v", tc.err, got, tc.soft)
			}
		})
	}
}
