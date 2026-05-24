package tools

import (
	"context"
	"regexp"
	"strings"
	"testing"
)

func TestBrowser_Info(t *testing.T) {
	tool := &BrowserTool{}
	info := tool.Info()

	if info.Name != "browser" {
		t.Errorf("expected name 'browser', got %q", info.Name)
	}
	if !containsString(info.Required, "action") || !containsString(info.Required, "description") {
		t.Errorf("expected Required to contain 'action' and 'description', got %v", info.Required)
	}

	props, ok := info.Parameters["properties"].(map[string]any)
	if !ok {
		t.Fatal("expected properties to be map[string]any")
	}

	expectedParams := []string{"action", "url", "selector", "text", "script", "timeout"}
	for _, p := range expectedParams {
		if _, exists := props[p]; !exists {
			t.Errorf("expected parameter %q in properties", p)
		}
	}
}

func TestBrowser_RequiresApproval(t *testing.T) {
	tool := &BrowserTool{}
	if !tool.RequiresApproval() {
		t.Error("expected RequiresApproval to return true")
	}
}

func TestBrowser_InvalidJSON(t *testing.T) {
	tool := &BrowserTool{}
	result, err := tool.Run(context.Background(), `not valid json`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Error("expected error result for invalid JSON")
	}
	if !contains(result.Content, "invalid arguments") {
		t.Errorf("expected 'invalid arguments' in content, got: %s", result.Content)
	}
}

func TestBrowser_MissingAction(t *testing.T) {
	tool := &BrowserTool{}
	result, err := tool.Run(context.Background(), `{}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Error("expected error result for missing action")
	}
	if !contains(result.Content, "missing required parameter: action") {
		t.Errorf("expected missing action message, got: %s", result.Content)
	}
}

func TestBrowser_UnknownAction(t *testing.T) {
	tool := &BrowserTool{}
	result, err := tool.Run(context.Background(), `{"action": "fly", "description": "test"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Error("expected error result for unknown action")
	}
	if !contains(result.Content, "unknown action") {
		t.Errorf("expected 'unknown action' in content, got: %s", result.Content)
	}
}

func TestBrowser_MissingDescription(t *testing.T) {
	tool := &BrowserTool{}
	result, err := tool.Run(context.Background(), `{"action": "read_page"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Error("expected error result for missing description")
	}
	if !contains(result.Content, "[validation error]") || !contains(result.Content, "description") {
		t.Errorf("expected validation error for missing description, got: %s", result.Content)
	}
}

func TestBrowser_NavigateMissingURL(t *testing.T) {
	tool := &BrowserTool{}
	result, err := tool.Run(context.Background(), `{"action": "navigate", "description": "test"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Error("expected error result for navigate without URL")
	}
	if !contains(result.Content, "requires 'url'") {
		t.Errorf("expected url required message, got: %s", result.Content)
	}
}

func TestBrowser_ClickMissingSelector(t *testing.T) {
	tool := &BrowserTool{}
	result, err := tool.Run(context.Background(), `{"action": "click", "description": "test"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Error("expected error result for click without selector")
	}
	if !contains(result.Content, "requires") {
		t.Errorf("expected requires message, got: %s", result.Content)
	}
}

func TestBrowser_TypeMissingSelector(t *testing.T) {
	tool := &BrowserTool{}
	result, err := tool.Run(context.Background(), `{"action": "type", "description": "test"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Error("expected error result for type without selector")
	}
	if !contains(result.Content, "requires") {
		t.Errorf("expected requires message, got: %s", result.Content)
	}
}

func TestBrowser_WaitMissingSelector(t *testing.T) {
	tool := &BrowserTool{}
	result, err := tool.Run(context.Background(), `{"action": "wait", "description": "test"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Error("expected error result for wait without selector")
	}
	if !contains(result.Content, "requires 'selector'") {
		t.Errorf("expected selector required message, got: %s", result.Content)
	}
}

func TestBrowser_ExecuteJSMissingScript(t *testing.T) {
	tool := &BrowserTool{}
	result, err := tool.Run(context.Background(), `{"action": "execute_js", "description": "test"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Error("expected error result for execute_js without script")
	}
	if !contains(result.Content, "requires 'script'") {
		t.Errorf("expected script required message, got: %s", result.Content)
	}
}

func TestBrowser_CloseWhenNotRunning(t *testing.T) {
	tool := &BrowserTool{}
	result, err := tool.Run(context.Background(), `{"action": "close", "description": "test"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Errorf("expected no error when closing non-running browser, got: %s", result.Content)
	}
	// closeBrowser uniformly reports "Browser closed" — dropping the
	// "not running" pre-check eliminated a TOCTOU window where a concurrent
	// Cleanup could swap state between the check and TeardownIfOnlyUser.
	// cleanupAll's switch handles backendNone as a no-op, so already-closed
	// is observationally identical to just-closed from the LLM's POV.
	if !contains(result.Content, "Browser closed") {
		t.Errorf("expected 'Browser closed' message, got: %s", result.Content)
	}
}

func TestValidatePageContent(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    bool // true = content is empty
	}{
		{"empty string", "", true},
		{"whitespace only", "   \n\t  ", true},
		{"valid content", "Hello world", false},
		{"short but valid", "Please verify", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isPageContentEmpty(tt.content)
			if got != tt.want {
				t.Errorf("isPageContentEmpty(%q) = %v, want %v", tt.content, got, tt.want)
			}
		})
	}
}

func TestBrowser_InfoDescription(t *testing.T) {
	tool := &BrowserTool{}
	info := tool.Info()
	if !contains(info.Description, "isolated profile") {
		t.Errorf("expected description to mention isolated profile, got: %s", info.Description)
	}
}

func TestFormatNavigateResult(t *testing.T) {
	tests := []struct {
		name        string
		url         string
		title       string
		preview     string
		wantPreview bool
		wantWarning bool
	}{
		{
			name:        "normal page with content",
			url:         "https://jd.com",
			title:       "京东首页",
			preview:     "京东JD.COM-专业的综合网上购物商城",
			wantPreview: true,
			wantWarning: false,
		},
		{
			name:        "anti-bot page",
			url:         "https://jd.com",
			title:       "请验证您的身份",
			preview:     "",
			wantPreview: false,
			wantWarning: true,
		},
		{
			name:        "empty preview",
			url:         "https://example.com",
			title:       "Example",
			preview:     "",
			wantPreview: false,
			wantWarning: false,
		},
		{
			name:        "long preview truncated",
			url:         "https://example.com",
			title:       "Example",
			preview:     strings.Repeat("あ", 250), // multi-byte chars
			wantPreview: true,
			wantWarning: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := formatNavigateResult(tt.url, tt.title, tt.preview)
			if tt.wantPreview && !strings.Contains(result, "Preview:") {
				t.Error("expected Preview in result")
			}
			if !tt.wantPreview && strings.Contains(result, "Preview:") {
				t.Error("unexpected Preview in result")
			}
			if tt.wantWarning && !strings.Contains(result, "WARNING") {
				t.Error("expected WARNING in result")
			}
			if !tt.wantWarning && strings.Contains(result, "WARNING") {
				t.Error("unexpected WARNING in result")
			}
		})
	}
}

func TestFormatNavigateResult_UTF8Safe(t *testing.T) {
	// Verify multi-byte rune truncation doesn't produce invalid UTF-8
	preview := strings.Repeat("中", 300) // 300 Chinese chars, each 3 bytes
	result := formatNavigateResult("https://example.com", "Test", preview)
	if !strings.Contains(result, "Preview:") {
		t.Fatal("expected preview in result")
	}
	// Verify the result is valid UTF-8 (strings.Contains would panic on invalid)
	if !strings.Contains(result, "...") {
		t.Error("expected truncation marker")
	}
}

func TestDetectAntiBotPage(t *testing.T) {
	tests := []struct {
		title string
		want  bool
	}{
		{"京东-综合网购首选-正品低价", false},
		{"Google", false},
		{"请验证您的身份", true},
		{"Just a moment...", true},
		{"Verify you are human", true},
		{"Access Denied", true},
		{"Attention Required! | Cloudflare", true},
		{"Are you a robot?", true},
		{"Security Check", true},
		{"Please wait while we verify", true},
		{"Robot Check", true},
		{"", false},
	}

	for _, tt := range tests {
		t.Run(tt.title, func(t *testing.T) {
			got := detectAntiBotPage(tt.title)
			if got != tt.want {
				t.Errorf("detectAntiBotPage(%q) = %v, want %v", tt.title, got, tt.want)
			}
		})
	}
}

// TestChromedpOrphanPattern_MatchesBothPrefixes locks in the alternation that
// CleanupOrphanedChromedp relies on: the broad-sweep regex must catch both
// the current kocoro-chromedp-* MkdirTemp prefix used by startChromedp AND
// the legacy chromedp.DefaultExecAllocatorOptions chromedp-runner* prefix
// used by older daemons (so a binary upgrade still cleans up old orphans).
//
// Also pins down the anti-false-positive anchoring: the pattern requires the
// literal `--user-data-dir=` flag, and the path glob uses [^ ] (non-space) so
// it can't greedily span across argv elements when pgrep -f sees the joined
// cmdline.
//
// pgrep uses POSIX extended regex; Go's regexp is RE2 — both honor `(a|b)`
// alternation and `[^ ]` the same way for the literal anchors we care about.
func TestChromedpOrphanPattern_MatchesBothPrefixes(t *testing.T) {
	re := regexp.MustCompile(chromedpOrphanPattern)
	cases := []struct {
		argv string
		want bool
	}{
		// Positive: actual flag form, both prefixes
		{"--user-data-dir=/var/folders/abc/T/kocoro-chromedp-12345", true},
		{"--user-data-dir=/tmp/chromedp-runner99999", true},
		{"chrome --foo --user-data-dir=/tmp/kocoro-chromedp-x --bar", true},

		// Negative: real Chrome profiles that aren't ours
		{"--user-data-dir=/Users/wayland/Library/Application Support/Google/Chrome", false},
		{"--user-data-dir=/tmp/some-other-thing", false},

		// Negative: substring-trap — cmdline that contains 'user-data-dir' as a
		// non-flag word AND contains 'kocoro-chromedp'/'chromedp-runner' in an
		// unrelated path. Without the --user-data-dir= anchor, the old loose
		// pattern would have matched this and killed an unrelated process.
		{"/usr/local/bin/dump-user-data-dir-info /tmp/kocoro-chromedp-readme.txt", false},

		// Negative: greedy-span trap — two separate flags where the first has
		// our flag with a benign value and another argv element later contains
		// 'kocoro-chromedp' as part of an unrelated path. The [^ ] non-space
		// class must stop at the first space so the alternation never reaches
		// the second arg.
		{"--user-data-dir=/Users/x/Chrome --log=/tmp/kocoro-chromedp-debug.log", false},
	}
	for _, tc := range cases {
		got := re.MatchString(tc.argv)
		if got != tc.want {
			t.Fatalf("orphan pattern match %q = %v, want %v", tc.argv, got, tc.want)
		}
	}
}

func TestBrowserTool_Deprecated_Idempotent(t *testing.T) {
	bt := &BrowserTool{}
	if bt.IsDeprecated() {
		t.Fatalf("fresh BrowserTool must not be deprecated")
	}
	bt.MarkDeprecated()
	if !bt.IsDeprecated() {
		t.Fatalf("MarkDeprecated did not set the flag")
	}
	bt.MarkDeprecated() // idempotent
	if !bt.IsDeprecated() {
		t.Fatalf("second MarkDeprecated must remain true")
	}
}

func TestBrowserTool_IsPinchtab_NoRaceWithCleanup(t *testing.T) {
	// Run repeatedly under -race to verify isPinchtab no longer reads
	// t.backend without holding t.mu.
	bt := &BrowserTool{backend: backendChromedp}
	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			_ = bt.isPinchtab()
		}
		done <- struct{}{}
	}()
	for i := 0; i < 1000; i++ {
		bt.mu.Lock()
		bt.backend = backendNone
		bt.mu.Unlock()
		bt.mu.Lock()
		bt.backend = backendChromedp
		bt.mu.Unlock()
	}
	<-done
}

func TestSnapshotChromedpCtx_BackendNone(t *testing.T) {
	bt := &BrowserTool{backend: backendNone}
	if _, ok := bt.snapshotChromedpCtx(); ok {
		t.Fatalf("expected ok=false for backendNone")
	}
}

func TestSnapshotChromedpCtx_BackendChromedpWithCtx(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bt := &BrowserTool{backend: backendChromedp, ctx: ctx}
	got, ok := bt.snapshotChromedpCtx()
	if !ok || got != ctx {
		t.Fatalf("expected ok=true, ctx==ctx; got ok=%v ctx==same=%v", ok, got == ctx)
	}
}

func TestSnapshotChromedpCtx_BackendChromedpNilCtx(t *testing.T) {
	bt := &BrowserTool{backend: backendChromedp, ctx: nil}
	if _, ok := bt.snapshotChromedpCtx(); ok {
		t.Fatalf("expected ok=false when ctx==nil even with chromedp backend")
	}
}
