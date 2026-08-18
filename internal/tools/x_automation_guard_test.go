package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

func TestXComposerURLClassificationDoesNotBlockOrdinaryXReads(t *testing.T) {
	for _, raw := range []string{
		"https://x.com/intent/tweet?text=review",
		"https://twitter.com/compose/post",
		"https://x.com/i/flow/compose",
		"http://mobile.x.com/compose/tweet",
		"https://x.com/intent/%74weet?text=encoded",
		"https://x.com./compose/post",
	} {
		if !isXComposerURL(raw) {
			t.Errorf("composer URL not classified: %s", raw)
		}
	}
	for _, raw := range []string{
		"https://x.com/KocoroLab/status/123",
		"https://x.com/search?q=kocoro",
		"https://example.com/intent/tweet",
	} {
		if isXComposerURL(raw) {
			t.Errorf("ordinary read URL classified as composer: %s", raw)
		}
	}
}

func TestBrowserBlocksDirectAndCurrentXComposerAutomation(t *testing.T) {
	t.Run("direct navigate", func(t *testing.T) {
		result, err := (&BrowserTool{}).Run(context.Background(),
			`{"action":"navigate","url":"https://x.com/intent/tweet?text=hello","description":"Open the composer"}`)
		if err != nil || !result.IsError || result.ErrorCategory != agent.ErrCategoryBusiness ||
			!strings.Contains(result.Content, "user") {
			t.Fatalf("Run = (%#v, %v)", result, err)
		}
	})

	originalEnsure := ensureBackendFn
	originalCurrent := browserCurrentURLFn
	ensureBackendFn = func(*BrowserTool, context.Context) error { return nil }
	browserCurrentURLFn = func(*BrowserTool, context.Context, time.Duration) (string, error) {
		return "https://x.com/compose/post", nil
	}
	t.Cleanup(func() {
		ensureBackendFn = originalEnsure
		browserCurrentURLFn = originalCurrent
	})
	for _, call := range []string{
		`{"action":"click","selector":"button","description":"Click"}`,
		`{"action":"type","selector":"textarea","text":"hello","description":"Type"}`,
		`{"action":"execute_js","script":"document.querySelector('button').click()","description":"Run script"}`,
	} {
		result, err := (&BrowserTool{}).Run(context.Background(), call)
		if err != nil || !result.IsError || result.ErrorCategory != agent.ErrCategoryBusiness {
			t.Fatalf("Run(%s) = (%#v, %v)", call, result, err)
		}
	}

	browserCurrentURLFn = func(*BrowserTool, context.Context, time.Duration) (string, error) {
		return "", errors.New("location unavailable")
	}
	result, err := (&BrowserTool{}).Run(context.Background(),
		`{"action":"click","selector":"[data-testid=tweetButton]","description":"Click Post"}`)
	if err != nil || !result.IsError || result.ErrorCategory != agent.ErrCategoryBusiness {
		t.Fatalf("explicit composer control with URL error = (%#v, %v)", result, err)
	}

	cached := &BrowserTool{}
	cached.setCurrentURL("https://x.com/compose/post")
	browserCurrentURLFn = func(tool *BrowserTool, _ context.Context, _ time.Duration) (string, error) {
		return tool.cachedCurrentURL(), errors.New("location unavailable")
	}
	result, err = cached.Run(context.Background(),
		`{"action":"click","selector":"button","description":"Click"}`)
	if err != nil || !result.IsError || result.ErrorCategory != agent.ErrCategoryBusiness {
		t.Fatalf("cached composer with URL error = (%#v, %v)", result, err)
	}

	browserCurrentURLFn = func(*BrowserTool, context.Context, time.Duration) (string, error) {
		return "", errors.New("location unavailable")
	}
	result, err = (&BrowserTool{}).Run(context.Background(),
		`{"action":"click","selector":"#read-more","description":"Read more"}`)
	if err != nil || !result.IsError || result.ErrorCategory == agent.ErrCategoryBusiness {
		t.Fatalf("ordinary unknown-page click should reach browser backend = (%#v, %v)", result, err)
	}
}

func TestComputerUseBlocksXComposerMutationButAllowsObservation(t *testing.T) {
	fake := newFakeAXCaller()
	tool := newTestComputerUse(fake)
	tool.snapshot = &computerUseSnapshot{
		id: "s_0000000000000000", app: "Google Chrome",
		bundleID: "com.google.Chrome", pid: 42, window: "Compose / X",
	}
	fake.queue("current_context", `{"app":"Google Chrome","window":"Compose / X","url":"https://x.com/compose/post"}`)
	blocked, result := tool.blockXComposerAutomation(context.Background(), computerUseArgs{Action: "click"})
	if !blocked || !result.IsError || result.ErrorCategory != agent.ErrCategoryBusiness {
		t.Fatalf("guard = (%t, %#v)", blocked, result)
	}
	if got := fakeAXMethods(fake.calls); len(got) != 1 || got[0] != "current_context" {
		t.Fatalf("helper methods = %v", got)
	}

	fake.calls = nil
	blocked, _ = tool.blockXComposerAutomation(context.Background(), computerUseArgs{Action: "get_app_state"})
	if blocked || len(fake.calls) != 0 {
		t.Fatalf("observation blocked=%t calls=%v", blocked, fake.calls)
	}
}

func TestComputerUseXReadNavigationRemainsAvailable(t *testing.T) {
	fake := newFakeAXCaller()
	enabled := true
	fingerprint := "axf_read_link"
	readTitle := "Open post details"
	postTitle := "Post"
	tool := newTestComputerUse(fake)
	tool.snapshot = &computerUseSnapshot{
		id: "s_0000000000000000", app: "Google Chrome",
		bundleID: "com.google.Chrome", pid: 42, window: "Timeline / X",
		elements: []computerUseElement{
			{Ref: "e1", Fingerprint: fingerprint, Role: "AXLink", Title: &readTitle, Enabled: &enabled},
			{Ref: "e2", Fingerprint: "axf_post", Role: "AXButton", Title: &postTitle, Enabled: &enabled},
		},
	}
	tool.refs = map[string]refEntry{
		"e1": {fingerprint: fingerprint, pid: 42},
		"e2": {fingerprint: "axf_post", pid: 42},
	}
	fake.queue("current_context", `{"app":"Google Chrome","window":"Timeline / X","url":"https://x.com/home"}`)
	blocked, _ := tool.blockXComposerAutomation(context.Background(), computerUseArgs{Action: "press", Ref: "e1"})
	if blocked {
		t.Fatal("ordinary ref-based X navigation was blocked")
	}

	fake.queue("current_context", `{"app":"Google Chrome","window":"Timeline / X","url":"https://x.com/home"}`)
	blocked, result := tool.blockXComposerAutomation(context.Background(), computerUseArgs{Action: "press", Ref: "e2"})
	if !blocked || !result.IsError || result.ErrorCategory != agent.ErrCategoryBusiness {
		t.Fatalf("Post control guard = (%t, %#v)", blocked, result)
	}
}

func TestComputerUseExplicitXControlBlocksWhenContextLookupFails(t *testing.T) {
	fake := newFakeAXCaller()
	fake.errors["current_context"] = []error{errors.New("context unavailable")}
	enabled := true
	identifier := "tweetButton"
	tool := newTestComputerUse(fake)
	tool.snapshot = &computerUseSnapshot{
		id: "s_0000000000000000", app: "Google Chrome",
		bundleID: "com.google.Chrome", pid: 42, window: "Browser",
		elements: []computerUseElement{{
			Ref: "e1", Fingerprint: "axf_post", Role: "AXButton",
			Identifier: &identifier, Enabled: &enabled,
		}},
	}
	tool.refs = map[string]refEntry{"e1": {fingerprint: "axf_post", pid: 42}}
	blocked, result := tool.blockXComposerAutomation(
		context.Background(), computerUseArgs{Action: "press", Ref: "e1"},
	)
	if !blocked || !result.IsError || result.ErrorCategory != agent.ErrCategoryBusiness {
		t.Fatalf("guard = (%t, %#v)", blocked, result)
	}
}

type xPrepareBatchClient struct{ calls atomic.Int32 }

func (c *xPrepareBatchClient) Complete(context.Context, client.CompletionRequest) (*client.CompletionResponse, error) {
	call := c.calls.Add(1)
	if call == 1 {
		return &client.CompletionResponse{FinishReason: "tool_use", ToolCalls: []client.FunctionCall{
			{ID: "search", Name: "tool_search", Arguments: json.RawMessage(`{"query":"select:x_prepare_post"}`)},
		}}, nil
	}
	return &client.CompletionResponse{FinishReason: "tool_use", ToolCalls: []client.FunctionCall{
		{ID: "prepare", Name: "x_prepare_post", Arguments: json.RawMessage(`{"text":"hello"}`)},
		{ID: "later", Name: "x_prepare_batch_side_effect", Arguments: json.RawMessage(`{}`)},
	}}, nil
}

func (c *xPrepareBatchClient) CompleteStream(ctx context.Context, req client.CompletionRequest, _ func(client.StreamDelta)) (*client.CompletionResponse, error) {
	return c.Complete(ctx, req)
}

type xPrepareBatchSideEffect struct{ runs atomic.Int32 }

func (*xPrepareBatchSideEffect) Info() agent.ToolInfo {
	return agent.ToolInfo{Name: "x_prepare_batch_side_effect", Parameters: map[string]any{"type": "object"}}
}
func (*xPrepareBatchSideEffect) RequiresApproval() bool { return false }
func (t *xPrepareBatchSideEffect) Run(context.Context, string) (agent.ToolResult, error) {
	t.runs.Add(1)
	return agent.ToolResult{Content: "ran"}, nil
}

func TestXPreparePostEndsTurnBeforeSameBatchAutomation(t *testing.T) {
	llm := &xPrepareBatchClient{}
	later := &xPrepareBatchSideEffect{}
	reg := agent.NewToolRegistry()
	reg.Register(&XPreparePostTool{})
	reg.Register(later)
	loop := agent.NewAgentLoop(llm, reg, "medium", t.TempDir(), 4, 2000, 200, nil, nil, nil)
	text, _, err := loop.Run(context.Background(), "draft an X post", nil, nil)
	if err != nil || !strings.Contains(text, "Review and post on X") {
		t.Fatalf("Run = (%q, %v)", text, err)
	}
	if llm.calls.Load() != 2 || later.runs.Load() != 0 {
		t.Fatalf("llm calls=%d later runs=%d", llm.calls.Load(), later.runs.Load())
	}
}
