package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chromedp/chromedp"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
)

// backend tracks which browser engine is active.
type browserBackend int

const (
	backendNone     browserBackend = iota
	backendPinchtab                // pinchtab HTTP API
	backendChromedp                // embedded chromedp (fallback)
)

type BrowserTool struct {
	mu      sync.Mutex
	backend browserBackend

	// pinchtab
	pt    *pinchtabClient
	tabID string // active tab in pinchtab

	// chromedp fallback
	ctx             context.Context
	cancel          context.CancelFunc
	chromedpDataDir string
	active          bool

	deprecated atomic.Bool // set by reload handoff; consulted by register.go cleanup gate

	// Test-only observability counters; production code never reads them.
	cleanupCalledForTest         atomic.Int32 // incremented at top of Cleanup
	cleanupChromedpCalledForTest atomic.Int32 // incremented at top of CleanupChromedp
}

// ensureBackendFn is the indirection that BrowserTool.Run uses to set up the
// browser backend. Tests override this to verify ordering invariants (e.g.
// MarkBrowserUsed must run BEFORE backend setup) without launching real
// Chrome. Production uses (*BrowserTool).ensureBackend. Mirrors the pattern
// in internal/tools/mcp_tool.go ensureChromeDebugPort.
var ensureBackendFn = (*BrowserTool).ensureBackend

type browserArgs struct {
	Action       string `json:"action"`
	Description  string `json:"description,omitempty"`
	URL          string `json:"url,omitempty"`
	Selector     string `json:"selector,omitempty"`
	Ref          string `json:"ref,omitempty"`
	Text         string `json:"text,omitempty"`
	Key          string `json:"key,omitempty"`
	Value        string `json:"value,omitempty"`
	Script       string `json:"script,omitempty"`
	Query        string `json:"query,omitempty"`
	Filter       string `json:"filter,omitempty"`
	WaitFor      string `json:"waitFor,omitempty"`
	WaitSelector string `json:"waitSelector,omitempty"`
	BlockImages  bool   `json:"blockImages,omitempty"`
	BlockAds     bool   `json:"blockAds,omitempty"`
	TextMode     string `json:"textMode,omitempty"`
	MaxChars     int    `json:"maxChars,omitempty"`
	Raw          bool   `json:"raw,omitempty"`
	Timeout      int    `json:"timeout,omitempty"`
}

func (t *BrowserTool) Info() agent.ToolInfo {
	return agent.ToolInfo{
		Name: "browser",
		Description: "Control a headless browser with an isolated profile. " +
			"FIRST CHOICE for any web page interaction: navigating, clicking, reading, scraping, screenshots of web content. " +
			"Only skip this for pages requiring user login/authentication — use GUI tools for those. " +
			"Actions: navigate, click, type, scroll, screenshot, read_page, execute_js, wait, close. " +
			"Use 'read_page' (textMode 'raw' for full DOM) to inspect page structure, or 'execute_js' to query the DOM programmatically and return JSON. " +
			"Note: snapshot/find (accessibility-tree actions) are not advertised — they only work with the legacy pinchtab backend; use Playwright MCP for equivalent functionality when available." +
			agent.DescriptionGuidance,
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"action":       map[string]any{"type": "string", "description": "Action: navigate, click, type, scroll, screenshot, read_page, execute_js, wait, close"},
				"description":  agent.DescriptionFieldSpec,
				"url":          map[string]any{"type": "string", "description": "URL to navigate to (for navigate action)"},
				"selector":     map[string]any{"type": "string", "description": "CSS selector (for click, type, read_page, scroll, wait)"},
				"ref":          map[string]any{"type": "string", "description": "Element ref, e.g. 'e5' (for click, type, scroll — alternative to selector). Only meaningful when another tool has produced refs for the current page."},
				"text":         map[string]any{"type": "string", "description": "Text to type (for type action)"},
				"key":          map[string]any{"type": "string", "description": "Key to press, e.g. 'Enter' (for press action via click with key)"},
				"value":        map[string]any{"type": "string", "description": "Value to select (for select action via click with value)"},
				"script":       map[string]any{"type": "string", "description": "JavaScript to execute (for execute_js action). Expression context: a plain expression is evaluated and its value returned. Scripts whose first token is a top-level statement keyword (`return`, `const`, `let`, `var`, `function`, `async`, `if`, `for`, `while`, `try`) are auto-wrapped in an async IIFE on the chromedp backend so they evaluate correctly; plain expressions (including semicolon-terminated or multi-line ones) pass through unchanged."},
				"waitFor":      map[string]any{"type": "string", "description": "Navigation wait strategy: e.g. 'domcontentloaded', 'networkidle' (for navigate action)"},
				"waitSelector": map[string]any{"type": "string", "description": "CSS selector to wait for after navigation"},
				"blockImages":  map[string]any{"type": "boolean", "description": "Disable image loading during navigation"},
				"blockAds":     map[string]any{"type": "boolean", "description": "Enable PinchTab ad blocking during navigation"},
				"textMode":     map[string]any{"type": "string", "description": "Text extraction mode for read_page (for example: 'readability' or 'raw')"},
				"maxChars":     map[string]any{"type": "integer", "description": "Maximum characters for read_page output"},
				"raw":          map[string]any{"type": "boolean", "description": "Convenience flag for read_page raw mode"},
				"timeout":      map[string]any{"type": "integer", "description": "Timeout in seconds (default: 30)"},
			},
		},
		Required: []string{"action", "description"},
	}
}

func (t *BrowserTool) RequiresApproval() bool { return true }

func (t *BrowserTool) IsReadOnlyCall(string) bool { return false }

func (t *BrowserTool) Run(ctx context.Context, argsJSON string) (agent.ToolResult, error) {
	var args browserArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return agent.ValidationError(fmt.Sprintf("invalid arguments: %v", err)), nil
	}

	if args.Action == "" {
		return agent.ValidationError("missing required parameter: action"), nil
	}
	if args.Description == "" {
		return agent.ValidationError("missing required parameter: description"), nil
	}

	timeout := 30 * time.Second
	if args.Timeout > 0 {
		timeout = time.Duration(args.Timeout) * time.Second
	}

	// Validate required params before starting a browser
	if err := t.validateArgs(args); err != nil {
		return agent.ValidationError(err.Error()), nil
	}

	// Mark the per-Run browser lease BEFORE ensureBackend so a concurrent
	// teardown observes our contribution to the counter before deciding
	// whether to skip teardown. Marking after ensureBackend leaves a race
	// window where another Run's defer can kill Chrome between ensureBackend
	// returning and our MarkUsed, dropping us into the action with backendNone
	// / nil t.ctx. Mirrors mcp_tool.go's mcp.MarkChromeUsed-before-ensure
	// order.
	//
	// We always mark (even when pinchtab will be chosen) — the teardown
	// callback is CleanupChromedp, which is a no-op on non-chromedp backends.
	// Cost is one extra tracker.mu round-trip per pinchtab Run.
	MarkBrowserUsed(ctx, t)

	// close doesn't need to start a backend, but it must still participate in
	// the lease so it cannot tear down Chrome while another Run is using it.
	if args.Action == "close" {
		return t.closeBrowser(ctx)
	}

	// Ensure a backend is available (pinchtab preferred, chromedp fallback)
	if err := ensureBackendFn(t, ctx); err != nil {
		return agent.ToolResult{Content: fmt.Sprintf("failed to start browser: %v", err), IsError: true}, nil
	}

	switch args.Action {
	case "navigate":
		return t.navigate(ctx, args, timeout)
	case "click":
		return t.click(ctx, args, timeout)
	case "type":
		return t.typeText(ctx, args, timeout)
	case "scroll":
		return t.scroll(ctx, args, timeout)
	case "screenshot":
		return t.screenshot(ctx, args, timeout)
	case "read_page":
		return t.readPage(ctx, args, timeout)
	case "execute_js":
		return t.executeJS(ctx, args, timeout)
	case "wait":
		return t.waitVisible(ctx, args, timeout)
	case "snapshot":
		// Pinchtab-only; returns a "requires pinchtab" error on the chromedp
		// fallback. No longer advertised in Info() so fresh calls should not
		// arrive here — but the dispatch stays to keep pinchtab environments
		// working (see ensureBackend's pinchtab-first preference).
		return t.snapshotAction(ctx, args)
	case "find":
		return t.findAction(ctx, args)
	default:
		// unreachable — validateArgs catches unknown actions
		return agent.ToolResult{Content: fmt.Sprintf("unknown action: %q", args.Action), IsError: true}, nil
	}
}

// validateArgs checks required params before starting a browser.
func (t *BrowserTool) validateArgs(args browserArgs) error {
	switch args.Action {
	case "navigate":
		if args.URL == "" {
			return fmt.Errorf("navigate action requires 'url' parameter")
		}
	case "click":
		if args.Ref == "" && args.Selector == "" {
			return fmt.Errorf("click action requires 'ref' or 'selector' parameter")
		}
	case "type":
		if args.Ref == "" && args.Selector == "" {
			return fmt.Errorf("type action requires 'ref' or 'selector' parameter")
		}
	case "wait":
		if args.Selector == "" {
			return fmt.Errorf("wait action requires 'selector' parameter")
		}
	case "execute_js":
		if args.Script == "" {
			return fmt.Errorf("execute_js action requires 'script' parameter")
		}
	case "find":
		if args.Query == "" {
			return fmt.Errorf("find action requires 'query' parameter")
		}
	case "scroll", "screenshot", "read_page", "snapshot", "close":
		// no required params
	default:
		return fmt.Errorf("unknown action: %q (valid: navigate, click, type, scroll, screenshot, read_page, execute_js, wait, close)", args.Action)
	}
	return nil
}

// ensureBackend picks pinchtab if available, else falls back to chromedp.
func (t *BrowserTool) ensureBackend(ctx context.Context) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	// Already have a working backend?
	switch t.backend {
	case backendPinchtab:
		if t.pt.available(ctx) {
			return nil
		}
		// pinchtab died — clear stale tab ID, try to restart or fall through to chromedp
		t.tabID = ""
		t.backend = backendNone
	case backendChromedp:
		if t.ctx != nil && t.ctx.Err() == nil {
			return nil
		}
		// chromedp context dead — reset
		if t.cancel != nil {
			t.cancel()
		}
		t.ctx = nil
		t.cancel = nil
		t.active = false
		t.backend = backendNone
	}

	// Try pinchtab first
	if t.pt == nil {
		t.pt = newPinchtabClient()
	}
	if err := t.pt.ensure(ctx); err == nil {
		t.backend = backendPinchtab
		return nil
	}

	// Fall back to chromedp
	return t.startChromedp()
}

func (t *BrowserTool) startChromedp() error {
	dataDir, err := os.MkdirTemp("", "kocoro-chromedp-*")
	if err != nil {
		return fmt.Errorf("failed to create browser profile: %w", err)
	}
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.Flag("headless", false),
		chromedp.Flag("disable-gpu", true),
		chromedp.Flag("no-first-run", true),
		chromedp.Flag("no-default-browser-check", true),
		chromedp.UserDataDir(dataDir),
	)

	allocCtx, allocCancel := chromedp.NewExecAllocator(context.Background(), opts...)
	browserCtx, browserCancel := chromedp.NewContext(allocCtx)

	if err := chromedp.Run(browserCtx); err != nil {
		browserCancel()
		allocCancel()
		_ = os.RemoveAll(dataDir)
		return fmt.Errorf("failed to start browser: %w", err)
	}

	t.ctx = browserCtx
	t.cancel = func() {
		browserCancel()
		allocCancel()
	}
	t.chromedpDataDir = dataDir
	t.active = true
	t.backend = backendChromedp
	return nil
}

func (t *BrowserTool) isPinchtab() bool {
	return t.backend == backendPinchtab
}

// CleanupChromedp tears down only the chromedp backend, leaving pinchtab (a
// long-lived external process) untouched. Safe to call when chromedp is not
// active. Returns an error if the tracked Chrome refused to die after SIGTERM
// and SIGKILL.
//
// The kill runs OUTSIDE t.mu so concurrent ensureBackend calls don't block on
// the up-to-3s poll loop. The lease's tracker.mu is what serializes a fresh
// MarkUsed against an in-flight teardown.
func (t *BrowserTool) CleanupChromedp() error {
	t.cleanupChromedpCalledForTest.Add(1)
	var dataDir string
	t.mu.Lock()
	if t.backend == backendChromedp {
		if t.cancel != nil {
			t.cancel()
		}
		dataDir = t.chromedpDataDir
		t.ctx = nil
		t.cancel = nil
		t.chromedpDataDir = ""
		t.active = false
		t.backend = backendNone
	}
	t.mu.Unlock()
	if dataDir == "" {
		return nil
	}
	return killChromedpChromeForDirFn(dataDir)
}

// --- Actions ---

// formatNavigateResult builds the navigate result string with anti-bot warning and content preview.
func formatNavigateResult(pageURL, title, textPreview string) string {
	content := fmt.Sprintf("Navigated to: %s\nTitle: %s", pageURL, title)

	if detectAntiBotPage(title) {
		content += "\n\nWARNING: This page appears to be an anti-bot challenge or CAPTCHA. " +
			"The page content is likely NOT the expected website content. " +
			"Do NOT attempt to extract data from this page. " +
			"Report to the user that the site blocked automated access."
	}

	preview := strings.TrimSpace(textPreview)
	if preview != "" {
		const maxPreviewRunes = 200
		runes := []rune(preview)
		if len(runes) > maxPreviewRunes {
			preview = string(runes[:maxPreviewRunes]) + "..."
		}
		content += fmt.Sprintf("\nPreview: %s", preview)
	}

	return content
}

func (t *BrowserTool) navigate(_ context.Context, args browserArgs, timeout time.Duration) (agent.ToolResult, error) {
	if t.isPinchtab() {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		// Always open a new tab to isolate navigation from previous tasks
		resp, err := t.pt.navigate(ctx, ptNavigateReq{
			URL:          args.URL,
			NewTab:       true,
			BlockImages:  args.BlockImages,
			BlockAds:     args.BlockAds,
			WaitFor:      args.WaitFor,
			WaitSelector: args.WaitSelector,
		})
		if err != nil {
			return agent.ToolResult{Content: fmt.Sprintf("navigate error: %v", err), IsError: true}, nil
		}
		if resp.TabID != "" {
			t.tabID = resp.TabID
		}

		// Best-effort content preview — don't fail navigate if text fetch fails.
		// Only fetch if we have a valid tab ID from this navigation response.
		var preview string
		if resp.TabID != "" {
			if textResp, err := t.pt.text(ctx, resp.TabID, "", 0, false); err == nil {
				preview = textResp.Text
			}
		}

		return agent.ToolResult{Content: formatNavigateResult(resp.URL, resp.Title, preview)}, nil
	}

	// chromedp
	tCtx, cancel := context.WithTimeout(t.ctx, timeout)
	defer cancel()

	var title, textContent string
	err := chromedp.Run(tCtx,
		chromedp.Navigate(args.URL),
		chromedp.WaitReady("body", chromedp.ByQuery),
		chromedp.Title(&title),
	)
	if err != nil {
		return agent.ToolResult{Content: fmt.Sprintf("navigate error: %v", err), IsError: true}, nil
	}

	// Best-effort content preview
	_ = chromedp.Run(tCtx, chromedp.Evaluate(
		`(document.querySelector("html")?.innerText || "").substring(0, 300)`,
		&textContent,
	))

	return agent.ToolResult{Content: formatNavigateResult(args.URL, title, textContent)}, nil
}

func (t *BrowserTool) click(_ context.Context, args browserArgs, timeout time.Duration) (agent.ToolResult, error) {
	if t.isPinchtab() {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		kind := "click"
		if args.Key != "" {
			kind = "press"
		} else if args.Value != "" {
			kind = "select"
		}
		req := ptActionReq{TabID: t.tabID, Kind: kind, Ref: args.Ref, Selector: args.Selector, Key: args.Key, Value: args.Value}
		resp, err := t.pt.action(ctx, req)
		if err != nil {
			return agent.ToolResult{Content: fmt.Sprintf("click error: %v", err), IsError: true}, nil
		}
		target := args.Ref
		if target == "" {
			target = args.Selector
		}
		_ = resp
		return agent.ToolResult{Content: fmt.Sprintf("Clicked: %s", target)}, nil
	}

	// chromedp (selector only)
	if args.Selector == "" {
		return agent.ToolResult{Content: "chromedp fallback requires 'selector' (refs not supported without pinchtab)", IsError: true}, nil
	}
	tCtx, cancel := context.WithTimeout(t.ctx, timeout)
	defer cancel()
	if err := chromedp.Run(tCtx, chromedp.Click(args.Selector)); err != nil {
		return agent.ToolResult{Content: fmt.Sprintf("click error: %v", err), IsError: true}, nil
	}
	return agent.ToolResult{Content: fmt.Sprintf("Clicked: %s", args.Selector)}, nil
}

func (t *BrowserTool) typeText(_ context.Context, args browserArgs, timeout time.Duration) (agent.ToolResult, error) {
	if t.isPinchtab() {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		req := ptActionReq{TabID: t.tabID, Kind: "type", Ref: args.Ref, Selector: args.Selector, Text: args.Text}
		_, err := t.pt.action(ctx, req)
		if err != nil {
			return agent.ToolResult{Content: fmt.Sprintf("type error: %v", err), IsError: true}, nil
		}
		target := args.Ref
		if target == "" {
			target = args.Selector
		}
		return agent.ToolResult{Content: fmt.Sprintf("Typed into: %s", target)}, nil
	}

	// chromedp
	if args.Selector == "" {
		return agent.ToolResult{Content: "chromedp fallback requires 'selector'", IsError: true}, nil
	}
	tCtx, cancel := context.WithTimeout(t.ctx, timeout)
	defer cancel()
	if err := chromedp.Run(tCtx, chromedp.SendKeys(args.Selector, args.Text)); err != nil {
		return agent.ToolResult{Content: fmt.Sprintf("type error: %v", err), IsError: true}, nil
	}
	return agent.ToolResult{Content: fmt.Sprintf("Typed into: %s", args.Selector)}, nil
}

func (t *BrowserTool) scroll(_ context.Context, args browserArgs, timeout time.Duration) (agent.ToolResult, error) {
	if t.isPinchtab() {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		req := ptActionReq{TabID: t.tabID, Kind: "scroll", Ref: args.Ref, Selector: args.Selector}
		if args.Ref == "" && args.Selector == "" {
			req.ScrollY = 800 // scroll down by default
		}
		_, err := t.pt.action(ctx, req)
		if err != nil {
			return agent.ToolResult{Content: fmt.Sprintf("scroll error: %v", err), IsError: true}, nil
		}
		target := args.Ref
		if target == "" {
			target = args.Selector
		}
		if target == "" {
			target = "page"
		}
		return agent.ToolResult{Content: fmt.Sprintf("Scrolled: %s", target)}, nil
	}

	// chromedp
	tCtx, cancel := context.WithTimeout(t.ctx, timeout)
	defer cancel()

	if args.Selector != "" {
		if err := chromedp.Run(tCtx, chromedp.ScrollIntoView(args.Selector)); err != nil {
			return agent.ToolResult{Content: fmt.Sprintf("scroll error: %v", err), IsError: true}, nil
		}
		return agent.ToolResult{Content: fmt.Sprintf("Scrolled to: %s", args.Selector)}, nil
	}

	var scrollHeight int
	if err := chromedp.Run(tCtx,
		chromedp.Evaluate(`window.scrollTo(0, document.body.scrollHeight); document.body.scrollHeight`, &scrollHeight),
	); err != nil {
		return agent.ToolResult{Content: fmt.Sprintf("scroll error: %v", err), IsError: true}, nil
	}
	return agent.ToolResult{Content: fmt.Sprintf("Scrolled to bottom (height: %d)", scrollHeight)}, nil
}

func (t *BrowserTool) screenshot(_ context.Context, _ browserArgs, timeout time.Duration) (agent.ToolResult, error) {
	if t.isPinchtab() {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		// Note: pinchtab v0.7.6 captures viewport only (no full-page support).
		// For full-page, the LLM can scroll + take multiple screenshots.
		data, err := t.pt.screenshot(ctx, t.tabID)
		if err != nil {
			return agent.ToolResult{Content: fmt.Sprintf("screenshot error: %v", err), IsError: true}, nil
		}

		// Save to temp file, resize for vision loop
		f, err := os.CreateTemp("", "browser-screenshot-*.jpg")
		if err != nil {
			return agent.ToolResult{Content: fmt.Sprintf("failed to create temp file: %v", err), IsError: true}, nil
		}
		f.Write(data)
		f.Close()

		// Best-effort resize — skip if image is too small or sips fails
		ResizeImage(f.Name(), DefaultAPIWidth)

		block, err := EncodeImage(f.Name())
		if err != nil {
			return agent.ToolResult{Content: fmt.Sprintf("encode error: %v", err), IsError: true}, nil
		}
		return agent.ToolResult{
			Content: fmt.Sprintf("Screenshot saved to: %s", f.Name()),
			Images:  []agent.ImageBlock{block},
		}, nil
	}

	// chromedp
	tCtx, cancel := context.WithTimeout(t.ctx, timeout)
	defer cancel()

	var buf []byte
	if err := chromedp.Run(tCtx, chromedp.FullScreenshot(&buf, 90)); err != nil {
		return agent.ToolResult{Content: fmt.Sprintf("screenshot error: %v", err), IsError: true}, nil
	}

	f, err := os.CreateTemp("", "browser-screenshot-*.png")
	if err != nil {
		return agent.ToolResult{Content: fmt.Sprintf("failed to create temp file: %v", err), IsError: true}, nil
	}
	f.Write(buf)
	f.Close()

	// Best-effort resize
	ResizeImage(f.Name(), DefaultAPIWidth)

	block, err := EncodeImage(f.Name())
	if err != nil {
		return agent.ToolResult{Content: fmt.Sprintf("encode error: %v", err), IsError: true}, nil
	}
	return agent.ToolResult{
		Content: fmt.Sprintf("Screenshot saved to: %s", f.Name()),
		Images:  []agent.ImageBlock{block},
	}, nil
}

// isPageContentEmpty returns true if content is empty/whitespace-only.
func isPageContentEmpty(content string) bool {
	return strings.TrimSpace(content) == ""
}

// antiBotTitlePatterns matches common anti-bot/CAPTCHA page titles.
var antiBotTitlePatterns = []string{
	"just a moment",
	"verify you are human",
	"are you a robot",
	"robot check",
	"access denied",
	"attention required",
	"security check",
	"请验证",
	"人机验证",
	"安全验证",
	"please wait while we verify",
	"checking your browser",
	"ddos protection",
	"captcha",
	"bot detection",
}

// detectAntiBotPage checks if a page title indicates an anti-bot/CAPTCHA challenge.
func detectAntiBotPage(title string) bool {
	lower := strings.ToLower(title)
	for _, pattern := range antiBotTitlePatterns {
		if strings.Contains(lower, pattern) {
			return true
		}
	}
	return false
}

func (t *BrowserTool) readPage(_ context.Context, args browserArgs, timeout time.Duration) (agent.ToolResult, error) {
	if t.isPinchtab() {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		resp, err := t.pt.text(ctx, t.tabID, args.TextMode, args.MaxChars, args.Raw)
		if err != nil {
			return agent.ToolResult{Content: fmt.Sprintf("read_page error: %v", err), IsError: true}, nil
		}
		text := resp.Text
		if isPageContentEmpty(text) {
			return agent.ToolResult{Content: fmt.Sprintf("URL: %s\nTitle: %s\n\nread_page returned empty content — the page may not have loaded correctly or may be blocked", resp.URL, resp.Title), IsError: true}, nil
		}
		const maxLen = 10240
		if len(text) > maxLen {
			text = text[:maxLen] + "\n... [truncated to 10KB]"
		}
		return agent.ToolResult{Content: fmt.Sprintf("URL: %s\nTitle: %s\n\n%s", resp.URL, resp.Title, text)}, nil
	}

	// chromedp
	tCtx, cancel := context.WithTimeout(t.ctx, timeout)
	defer cancel()

	selector := "html"
	if args.Selector != "" {
		selector = args.Selector
	}

	var textContent string
	err := chromedp.Run(tCtx, chromedp.Evaluate(
		fmt.Sprintf(`document.querySelector(%q)?.innerText || ""`, selector),
		&textContent,
	))
	if err != nil {
		// Fall back to outerHTML
		var html string
		if err2 := chromedp.Run(tCtx, chromedp.OuterHTML(selector, &html)); err2 != nil {
			return agent.ToolResult{Content: fmt.Sprintf("read_page error: %v (fallback: %v)", err, err2), IsError: true}, nil
		}
		textContent = html
	}

	if isPageContentEmpty(textContent) {
		return agent.ToolResult{Content: "read_page returned empty content — the page may not have loaded correctly or may be blocked", IsError: true}, nil
	}

	const maxLen = 10240
	if len(textContent) > maxLen {
		textContent = textContent[:maxLen] + "\n... [truncated to 10KB]"
	}
	return agent.ToolResult{Content: textContent}, nil
}

func (t *BrowserTool) executeJS(_ context.Context, args browserArgs, timeout time.Duration) (agent.ToolResult, error) {
	if t.isPinchtab() {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		resp, err := t.pt.evaluate(ctx, t.tabID, args.Script)
		if err != nil {
			return agent.ToolResult{Content: fmt.Sprintf("execute_js error: %v", err), IsError: true}, nil
		}
		output := fmt.Sprintf("%v", resp.Result)
		const maxLen = 10240
		if len(output) > maxLen {
			output = output[:maxLen] + "\n... [truncated to 10KB]"
		}
		return agent.ToolResult{Content: output}, nil
	}

	// chromedp: Evaluate runs in expression context, so multi-statement
	// scripts with `return`/`const`/`let` would fail with "Illegal return
	// statement". Transparently wrap them in an async IIFE so the script
	// author can write natural multi-statement JS.
	script := wrapJSForEvaluate(args.Script)

	tCtx, cancel := context.WithTimeout(t.ctx, timeout)
	defer cancel()

	var result any
	if err := chromedp.Run(tCtx, chromedp.Evaluate(script, &result)); err != nil {
		return agent.ToolResult{Content: fmt.Sprintf("execute_js error: %v", err), IsError: true}, nil
	}
	output := fmt.Sprintf("%v", result)
	const maxLen = 10240
	if len(output) > maxLen {
		output = output[:maxLen] + "\n... [truncated to 10KB]"
	}
	return agent.ToolResult{Content: output}, nil
}

func (t *BrowserTool) waitVisible(_ context.Context, args browserArgs, timeout time.Duration) (agent.ToolResult, error) {
	if t.isPinchtab() {
		// Use JS polling via evaluate
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		script := fmt.Sprintf(`
			await new Promise((resolve, reject) => {
				const el = document.querySelector(%q);
				if (el) return resolve(true);
				const obs = new MutationObserver(() => {
					if (document.querySelector(%q)) { obs.disconnect(); resolve(true); }
				});
				obs.observe(document.body, {childList: true, subtree: true});
				setTimeout(() => { obs.disconnect(); reject('timeout'); }, %d);
			})
		`, args.Selector, args.Selector, int(timeout.Milliseconds()))
		_, err := t.pt.evaluate(ctx, t.tabID, script)
		if err != nil {
			return agent.ToolResult{Content: fmt.Sprintf("wait error: %v", err), IsError: true}, nil
		}
		return agent.ToolResult{Content: fmt.Sprintf("Element visible: %s", args.Selector)}, nil
	}

	// chromedp
	tCtx, cancel := context.WithTimeout(t.ctx, timeout)
	defer cancel()
	if err := chromedp.Run(tCtx, chromedp.WaitVisible(args.Selector)); err != nil {
		return agent.ToolResult{Content: fmt.Sprintf("wait error: %v", err), IsError: true}, nil
	}
	return agent.ToolResult{Content: fmt.Sprintf("Element visible: %s", args.Selector)}, nil
}

// --- New pinchtab-only actions ---

func (t *BrowserTool) snapshotAction(_ context.Context, args browserArgs) (agent.ToolResult, error) {
	if !t.isPinchtab() {
		return agent.ToolResult{
			Content: "snapshot action requires pinchtab (not available, using chromedp fallback). Use read_page instead.",
			IsError: true,
		}, nil
	}

	filter := args.Filter
	if filter == "" {
		filter = "interactive"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, err := t.pt.snapshot(ctx, t.tabID, filter)
	if err != nil {
		return agent.ToolResult{Content: fmt.Sprintf("snapshot error: %v", err), IsError: true}, nil
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("URL: %s\nTitle: %s\nElements: %d\n\n", resp.URL, resp.Title, resp.Count))

	for _, n := range resp.Nodes {
		indent := strings.Repeat("  ", n.Depth)
		line := fmt.Sprintf("%s[%s] %s: %s", indent, n.Ref, n.Role, n.Name)
		if n.Value != "" {
			line += fmt.Sprintf(" = %q", n.Value)
		}
		if n.Focused {
			line += " (focused)"
		}
		if n.Disabled {
			line += " (disabled)"
		}
		sb.WriteString(line + "\n")
	}

	content := sb.String()
	const maxLen = 20480 // snapshot can be larger
	if len(content) > maxLen {
		content = content[:maxLen] + "\n... [truncated]"
	}

	return agent.ToolResult{Content: content}, nil
}

func (t *BrowserTool) findAction(_ context.Context, args browserArgs) (agent.ToolResult, error) {
	if !t.isPinchtab() {
		return agent.ToolResult{
			Content: "find action requires pinchtab (not available, using chromedp fallback). Use execute_js or read_page instead.",
			IsError: true,
		}, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	resp, err := t.pt.find(ctx, ptFindReq{Query: args.Query, TabID: t.tabID, TopK: 5})
	if err != nil {
		// /find may not exist in older pinchtab versions — suggest snapshot instead
		if strings.Contains(err.Error(), "404") {
			return agent.ToolResult{
				Content: "find is not available in this pinchtab version. Use 'snapshot' to get element refs, then click/type by ref.",
				IsError: true,
			}, nil
		}
		return agent.ToolResult{Content: fmt.Sprintf("find error: %v", err), IsError: true}, nil
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Best match: %s (confidence: %s, score: %.2f)\n\n", resp.BestRef, resp.Confidence, resp.Score))
	for _, m := range resp.Matches {
		sb.WriteString(fmt.Sprintf("  [%s] %s: %s (score: %.2f)\n", m.Ref, m.Role, m.Name, m.Score))
	}

	return agent.ToolResult{Content: sb.String()}, nil
}

// wrapJSForEvaluate rewrites a script intended for chromedp.Evaluate so that
// bare top-level statements (`return x`, `const x = …; return x`) evaluate
// without a "SyntaxError: Illegal return statement". chromedp.Evaluate runs
// in expression context, so only statement-like leading keywords trigger the
// wrap — plain expressions (including semicolon-terminated or multi-line ones
// like `JSON.stringify(x);` or `a\nb`) must pass through unchanged, because
// wrapping them in an IIFE without an explicit `return` would silently change
// the returned value to `undefined`.
func wrapJSForEvaluate(script string) string {
	trimmed := strings.TrimSpace(script)
	if trimmed == "" {
		return script
	}
	// Already wrapped in a user-authored IIFE? Leave it alone — a redundant
	// wrap is still valid JS, but this keeps behavior predictable in tests.
	if strings.HasPrefix(trimmed, "(async") || strings.HasPrefix(trimmed, "(()") ||
		strings.HasPrefix(trimmed, "(function") {
		return script
	}
	// `async` alone is ambiguous: `async () => expr` is a perfectly valid
	// expression and wrapping it in an IIFE without a `return` would turn
	// the arrow-function result into `undefined`. Only `async function …`
	// (a declaration) needs wrapping, so match that two-token form and
	// leave bare `async` out of the general keyword list.
	if hasAsyncFunctionPrefix(trimmed) {
		return "(async () => { " + script + " })()"
	}
	if !hasLeadingKeyword(trimmed, "return", "const", "let", "var", "function", "if", "for", "while", "try") {
		return script
	}
	return "(async () => { " + script + " })()"
}

// hasAsyncFunctionPrefix reports whether s starts with "async function"
// (with whitespace between the tokens). The arrow-function form
// `async () => …` and identifier-like forms (`asyncFoo`) return false.
func hasAsyncFunctionPrefix(s string) bool {
	const prefix = "async"
	if !strings.HasPrefix(s, prefix) {
		return false
	}
	rest := s[len(prefix):]
	if rest == "" || (rest[0] != ' ' && rest[0] != '\t') {
		return false
	}
	rest = strings.TrimLeft(rest, " \t")
	return hasLeadingKeyword(rest, "function")
}

// hasLeadingKeyword reports whether s starts with any of the keywords followed
// by whitespace, `(`, or `{` — i.e. a statement boundary rather than an
// identifier that happens to share a prefix (`returnValue`).
func hasLeadingKeyword(s string, keywords ...string) bool {
	for _, kw := range keywords {
		if !strings.HasPrefix(s, kw) {
			continue
		}
		if len(s) == len(kw) {
			return true
		}
		next := s[len(kw)]
		if next == ' ' || next == '\t' || next == '(' || next == '{' {
			return true
		}
	}
	return false
}

func (t *BrowserTool) closeBrowser(ctx context.Context) (agent.ToolResult, error) {
	// No pre-check on backend state — dropping the lock between the check and
	// TeardownIfOnlyUser opened a TOCTOU window where a concurrent Cleanup
	// could swap state in the gap, and we'd report a misleading message for
	// work another goroutine did. cleanupAll handles backendNone as a no-op
	// switch fallthrough, so an already-closed browser just produces the
	// same "Browser closed" result the LLM was after.
	_, skipped, err := BrowserUseLeaseFrom(ctx).TeardownIfOnlyUser(t.cleanupAll)
	if skipped {
		// Informational, not a tool failure: another concurrent Run is still
		// using the browser. Returning IsError=true here would burn the LLM's
		// all-errors retry budget (loopdetect) and could trigger a force-stop
		// on a benign "wait your turn" signal.
		return agent.ToolResult{Content: "Browser close skipped: another run is still using the browser"}, nil
	}
	if err != nil {
		return agent.ToolResult{Content: fmt.Sprintf("browser close error: %v", err), IsError: true}, nil
	}
	return agent.ToolResult{Content: "Browser closed"}, nil
}

// Cleanup shuts down the browser. Safe to call multiple times.
func (t *BrowserTool) Cleanup() {
	t.cleanupCalledForTest.Add(1)
	_ = t.cleanupAll()
}

func (t *BrowserTool) cleanupAll() error {
	var dataDir string
	t.mu.Lock()
	switch t.backend {
	case backendPinchtab:
		if t.pt != nil {
			t.pt.close()
		}
		t.tabID = ""
	case backendChromedp:
		if t.cancel != nil {
			t.cancel()
		}
		dataDir = t.chromedpDataDir
		t.ctx = nil
		t.cancel = nil
		t.chromedpDataDir = ""
		t.active = false
	}
	t.backend = backendNone
	t.mu.Unlock()
	if dataDir == "" {
		return nil
	}
	return killChromedpChromeForDirFn(dataDir)
}

// MarkDeprecated flags this BrowserTool as superseded by reload. The
// registration-time cleanup func skips browser.Cleanup() for deprecated
// instances; the per-owner lease teardown path is the cleanup driver instead.
// Idempotent.
func (t *BrowserTool) MarkDeprecated() {
	t.deprecated.Store(true)
}

// IsDeprecated reports whether this BrowserTool has been superseded by a
// reload-time handoff.
func (t *BrowserTool) IsDeprecated() bool {
	return t.deprecated.Load()
}

// CleanupCalledForTest returns the number of times Cleanup has been called.
// Test-only accessor.
func (t *BrowserTool) CleanupCalledForTest() int32 {
	return t.cleanupCalledForTest.Load()
}

// CleanupChromedpCalledForTest returns the number of times CleanupChromedp
// has been called. Test-only accessor.
func (t *BrowserTool) CleanupChromedpCalledForTest() int32 {
	return t.cleanupChromedpCalledForTest.Load()
}

const (
	// chromedpTermGrace is how long killChromedpChromeForDir waits after
	// SIGTERM before escalating to SIGKILL. Matches StopCDPChromeAndWait's
	// pattern.
	chromedpTermGrace = 2 * time.Second
	// chromedpKillGrace is how long to wait after SIGKILL before declaring
	// Chrome unkillable.
	chromedpKillGrace = 1 * time.Second
	// chromedpPollInterval matches StopCDPChromeAndWait's cadence.
	chromedpPollInterval = 200 * time.Millisecond
	// chromedpOrphanPattern matches startChromedp's MkdirTemp prefix
	// ("kocoro-chromedp-*") and the legacy chromedp.DefaultExecAllocatorOptions
	// prefix ("chromedp-runner*"). Used ONLY by CleanupOrphanedChromedp; the
	// per-turn teardown path matches the exact data-dir of the BrowserTool
	// instance that started it.
	//
	// Anchored to the literal --user-data-dir= flag form so a cmdline that
	// only happens to contain the substring "user-data-dir" (e.g. a script
	// referenced from a chromedp-runner-style sibling path) can't trigger a
	// false-positive match. [^ ] prevents the path glob from greedily
	// spanning across argv elements when pgrep -f sees the joined cmdline.
	chromedpOrphanPattern = "--user-data-dir=[^ ]*(kocoro-chromedp|chromedp-runner)"
)

var killChromedpChromeForDirFn = killChromedpChromeForDir

// killChromedpChromeForDir SIGTERMs the Chrome process whose --user-data-dir
// is exactly dataDir, polls for exit, and escalates to SIGKILL when SIGTERM is
// ignored. Returns nil when Chrome is dead (or never alive); returns an error
// only when Chrome survives SIGKILL.
//
// Used by Cleanup paths because cancelling chromedp's allocCtx alone doesn't
// reliably reap the Chrome process tree — production case showed orphan
// chromedp Chrome alive 3h+ after a nominal Cleanup.
//
// The dataDir is removed from disk on every successful path (dead, TERM-dead,
// KILL-dead). When Chrome survives SIGKILL the dir is preserved so a follow-up
// orphan sweep can still find it.
func killChromedpChromeForDir(dataDir string) error {
	if dataDir == "" {
		return nil
	}
	pattern := fmt.Sprintf("user-data-dir=%s", regexp.QuoteMeta(dataDir))
	return killChromedpChromeForPattern(pattern, dataDir)
}

func killChromedpChromeForPattern(pattern string, dataDir string) error {
	if !chromedpChromeAlivePattern(pattern) {
		if dataDir != "" {
			_ = os.RemoveAll(dataDir)
		}
		return nil
	}
	_ = exec.Command("pkill", "-f", pattern).Run()
	if waitChromedpDeadPattern(pattern, chromedpTermGrace) {
		if dataDir != "" {
			_ = os.RemoveAll(dataDir)
		}
		return nil
	}
	// SIGTERM ignored — escalate to SIGKILL.
	_ = exec.Command("pkill", "-9", "-f", pattern).Run()
	if waitChromedpDeadPattern(pattern, chromedpKillGrace) {
		if dataDir != "" {
			_ = os.RemoveAll(dataDir)
		}
		return nil
	}
	return fmt.Errorf("chromedp Chrome still alive after SIGKILL")
}

func chromedpChromeAlivePattern(pattern string) bool {
	out, err := exec.Command("pgrep", "-f", pattern).Output()
	return err == nil && len(strings.TrimSpace(string(out))) > 0
}

func waitChromedpDeadPattern(pattern string, grace time.Duration) bool {
	polls := int(grace / chromedpPollInterval)
	if polls < 1 {
		polls = 1
	}
	for range polls {
		if !chromedpChromeAlivePattern(pattern) {
			return true
		}
		time.Sleep(chromedpPollInterval)
	}
	return !chromedpChromeAlivePattern(pattern)
}

// cleanupOrphanedChromedpCalledForTest is incremented every time
// CleanupOrphanedChromedp runs. Test-only; production code never reads it.
var cleanupOrphanedChromedpCalledForTest int

// CleanupOrphanedChromedp kills any Chrome processes started by chromedp from
// previous daemon runs that weren't properly cleaned up (e.g. force-kill, crash).
// Safe to call at daemon startup before registering tools.
//
// Matches both the current kocoro-chromedp-* MkdirTemp prefix from startChromedp
// and the legacy chromedp-runner-* default prefix used by older daemons.
func CleanupOrphanedChromedp() {
	cleanupOrphanedChromedpCalledForTest++
	out, err := exec.Command("pgrep", "-f", chromedpOrphanPattern).Output()
	if err != nil || strings.TrimSpace(string(out)) == "" {
		return
	}
	pids := strings.Fields(strings.TrimSpace(string(out)))
	for _, pid := range pids {
		exec.Command("kill", pid).Run()
	}
	if len(pids) > 0 {
		log.Printf("cleaned up %d orphaned chromedp Chrome process(es)", len(pids))
	}
}
