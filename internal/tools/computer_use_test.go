package tools

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/config"
)

type fakeAXCall struct {
	method string
	params map[string]any
}

type fakeAXCaller struct {
	responses map[string][]json.RawMessage
	errors    map[string][]error
	calls     []fakeAXCall
}

type blockingAXCaller struct {
	started chan struct{}
	release chan struct{}
	result  json.RawMessage
}

func (b *blockingAXCaller) Call(context.Context, string, any) (json.RawMessage, error) {
	close(b.started)
	<-b.release
	return b.result, nil
}

func newFakeAXCaller() *fakeAXCaller {
	return &fakeAXCaller{
		responses: make(map[string][]json.RawMessage),
		errors:    make(map[string][]error),
	}
}

func (f *fakeAXCaller) queue(method string, payload string) {
	f.responses[method] = append(f.responses[method], json.RawMessage(payload))
}

func (f *fakeAXCaller) Call(_ context.Context, method string, params any) (json.RawMessage, error) {
	paramMap, _ := params.(map[string]any)
	f.calls = append(f.calls, fakeAXCall{method: method, params: paramMap})
	if queued := f.errors[method]; len(queued) > 0 {
		f.errors[method] = queued[1:]
		return nil, queued[0]
	}
	queued := f.responses[method]
	if len(queued) == 0 {
		return json.RawMessage(`{"result":"ok"}`), nil
	}
	f.responses[method] = queued[1:]
	return queued[0], nil
}

func treeFixture(title string) string {
	return `{"app":"Notes","pid":42,"window":"Note","elements":[` +
		`{"ref":"e1","role":"AXButton","title":` + mustJSON(title) + `},` +
		`{"ref":"e2","role":"AXTextField","title":"Body","value":"hello"}` +
		`],"ref_paths":{"e1":{"path":"window[0]/AXButton[0]","role":"AXButton"},` +
		`"e2":{"path":"window[0]/AXTextField[0]","role":"AXTextField"}}}`
}

func mustJSON(value string) string {
	b, _ := json.Marshal(value)
	return string(b)
}

func newTestComputerUse(fake *fakeAXCaller) *ComputerUseTool {
	return &ComputerUseTool{
		client:  fake,
		screenW: DefaultAPIWidth,
		screenH: DefaultAPIHeight,
	}
}

func observeNotes(t *testing.T, tool *ComputerUseTool, fake *fakeAXCaller, tree string) string {
	t.Helper()
	fake.queue("resolve_pid", `{"pid":42}`)
	fake.queue("read_tree", tree)
	result, err := tool.Run(context.Background(), `{"action":"get_app_state","app":"Notes","description":"Inspect Notes window"}`)
	if err != nil {
		t.Fatalf("observe returned error: %v", err)
	}
	if result.IsError {
		t.Fatalf("observe failed: %s", result.Content)
	}
	for _, line := range strings.Split(result.Content, "\n") {
		if strings.HasPrefix(line, "state_id: ") {
			return strings.TrimPrefix(line, "state_id: ")
		}
	}
	t.Fatalf("observation missing state_id: %s", result.Content)
	return ""
}

func TestComputerUse_InfoSafetyAndSerialization(t *testing.T) {
	tool := &ComputerUseTool{}
	info := tool.Info()
	if info.Name != "computer_use" {
		t.Fatalf("Info().Name = %q, want computer_use", info.Name)
	}
	for _, required := range []string{"action", "description"} {
		if !containsString(info.Required, required) {
			t.Errorf("required fields %v missing %q", info.Required, required)
		}
	}
	props, ok := info.Parameters["properties"].(map[string]any)
	if !ok {
		t.Fatal("Parameters.properties missing")
	}
	for _, name := range []string{"action", "description", "state_id", "app", "ref", "value", "x", "y", "text", "keys", "include_screenshot"} {
		if _, ok := props[name]; !ok {
			t.Errorf("schema missing %q", name)
		}
	}
	if !tool.RequiresApproval() {
		t.Error("computer_use must participate in the approval path")
	}

	for _, action := range []string{"get_app_state", "get_value", "screenshot", "wait"} {
		args := `{"action":"` + action + `"}`
		if !tool.IsSafeArgs(args) {
			t.Errorf("%s should be approval-free", action)
		}
		if !tool.IsReadOnlyCall(args) {
			t.Errorf("%s should be read-only", action)
		}
		if tool.IsConcurrencySafeCall(args) {
			t.Errorf("%s must serialize because state_id/refs are per-run mutable state", action)
		}
	}
	for _, action := range []string{"focus_app", "launch_app", "click", "press", "set_value", "scroll", "type", "hotkey", "move"} {
		args := `{"action":"` + action + `"}`
		if tool.IsSafeArgs(args) || tool.IsReadOnlyCall(args) {
			t.Errorf("%s must be classified as a mutation", action)
		}
	}
	if tool.IsSafeArgs(`not-json`) || tool.IsReadOnlyCall(`not-json`) {
		t.Error("argument classification must fail closed")
	}
}

func TestComputerUse_ValidationErrors(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("computer_use runtime is macOS-only")
	}
	tool := newTestComputerUse(newFakeAXCaller())
	tests := []struct {
		name string
		args string
		want string
	}{
		{"invalid JSON", `not-json`, "invalid arguments"},
		{"invalid numeric string", `{"action":"click","x":"left","y":"20","description":"Click control"}`, "expected an integer or decimal integer string"},
		{"missing action", `{"description":"Inspect app"}`, "missing required parameter: action"},
		{"missing description", `{"action":"get_app_state"}`, "missing required parameter: description"},
		{"unknown action", `{"action":"fly","description":"Fly app"}`, "unknown action"},
		{"oversized semantic budget", `{"action":"get_app_state","semantic_budget":101,"description":"Inspect app"}`, "semantic_budget must be between 0 and 100"},
		{"negative semantic budget", `{"action":"get_app_state","semantic_budget":-1,"description":"Inspect app"}`, "semantic_budget must be between 0 and 100"},
		{"oversized wait timeout", `{"action":"wait","condition":"titleChanged","timeout":121,"description":"Wait for app"}`, "timeout must not exceed 120 seconds"},
		{"excessive click count", `{"action":"click","x":0,"y":0,"clicks":4,"description":"Click control"}`, "clicks must be between 0 and 3"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result, err := tool.Run(context.Background(), tc.args)
			if err != nil {
				t.Fatalf("Run error: %v", err)
			}
			if !result.IsError || result.ErrorCategory != agent.ErrCategoryValidation {
				t.Fatalf("result = %+v, want categorized validation error", result)
			}
			if !strings.Contains(result.Content, tc.want) {
				t.Errorf("content %q missing %q", result.Content, tc.want)
			}
		})
	}
}

func TestComputerUse_ObservationStateAndDiff(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("computer_use runtime is macOS-only")
	}
	fake := newFakeAXCaller()
	tool := newTestComputerUse(fake)
	firstID := observeNotes(t, tool, fake, treeFixture("Save"))
	if firstID == "" {
		t.Fatal("empty state id")
	}

	secondID := observeNotes(t, tool, fake, treeFixture("Save"))
	if secondID != firstID {
		t.Fatalf("unchanged tree state_id = %q, want %q", secondID, firstID)
	}
	if tool.snapshot == nil || tool.snapshot.status != "unchanged" {
		t.Fatalf("snapshot status = %+v, want unchanged", tool.snapshot)
	}

	fake.queue("resolve_pid", `{"pid":42}`)
	fake.queue("read_tree", treeFixture("Send"))
	changed, err := tool.Run(context.Background(), `{"action":"get_app_state","app":"Notes","description":"Refresh Notes window"}`)
	if err != nil || changed.IsError {
		t.Fatalf("changed observation = %+v, err=%v", changed, err)
	}
	if !strings.Contains(changed.Content, "status: changed") || !strings.Contains(changed.Content, "changed=1") {
		t.Fatalf("changed observation missing compact diff: %s", changed.Content)
	}
	if tool.snapshot.id == firstID {
		t.Error("changed tree retained stale state_id")
	}
}

// TestComputerUse_DiffResetsAcrossObservationScopes guards the diff baseline:
// observing a different app (or filter/budget) after a prior snapshot must
// report "initial", not a meaningless ref-level diff against the other scope.
func TestComputerUse_DiffResetsAcrossObservationScopes(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("computer_use runtime is macOS-only")
	}
	fake := newFakeAXCaller()
	tool := newTestComputerUse(fake)
	observeNotes(t, tool, fake, treeFixture("Save"))

	// Same filter/budget, different app (pid 77) — scope change, not a diff.
	fake.queue("resolve_pid", `{"pid":77}`)
	fake.queue("read_tree", `{"app":"Finder","pid":77,"window":"Downloads","elements":[`+
		`{"ref":"e1","role":"AXButton","title":"Open"}`+
		`],"ref_paths":{"e1":{"path":"window[0]/AXButton[0]","role":"AXButton"}}}`)
	result, err := tool.Run(context.Background(), `{"action":"get_app_state","app":"Finder","description":"Inspect Finder window"}`)
	if err != nil || result.IsError {
		t.Fatalf("cross-app observation = %+v, err=%v", result, err)
	}
	if !strings.Contains(result.Content, "status: initial") {
		t.Fatalf("cross-app observation should be a fresh baseline, got: %s", result.Content)
	}
}

func TestComputerUse_StaleRefRejectedBeforeMutation(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("computer_use runtime is macOS-only")
	}
	fake := newFakeAXCaller()
	tool := newTestComputerUse(fake)
	stateID := observeNotes(t, tool, fake, treeFixture("Save"))
	fake.queue("read_tree", treeFixture("Send"))

	result, err := tool.Run(context.Background(), `{"action":"click","state_id":"`+stateID+`","ref":"e1","description":"Click Save"}`)
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	if !result.IsError || result.ErrorCategory != agent.ErrCategoryBusiness || !strings.Contains(result.Content, "stale state") {
		t.Fatalf("result = %+v, want stale-state business error", result)
	}
	for _, call := range fake.calls {
		if call.method == "click" {
			t.Fatal("click reached ax_server despite stale preflight")
		}
	}
}

func TestComputerUse_RefActionsUsePreflightAndNoAutomaticScreenshot(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("computer_use runtime is macOS-only")
	}
	tests := []struct {
		action string
		ref    string
		extra  string
		method string
	}{
		{"click", "e1", "", "click"},
		{"press", "e1", "", "press"},
		{"set_value", "e2", `,"value":"updated"`, "set_value"},
		{"scroll", "e2", `,"dy":240`, "scroll"},
	}
	for _, tc := range tests {
		t.Run(tc.action, func(t *testing.T) {
			fake := newFakeAXCaller()
			tool := newTestComputerUse(fake)
			stateID := observeNotes(t, tool, fake, treeFixture("Save"))
			fake.queue("read_tree", treeFixture("Save"))
			fake.queue(tc.method, `{"result":"done"}`)
			args := `{"action":"` + tc.action + `","state_id":"` + stateID + `","ref":"` + tc.ref + `","description":"Update Notes"` + tc.extra + `}`
			result, err := tool.Run(context.Background(), args)
			if err != nil || result.IsError {
				t.Fatalf("Run result=%+v err=%v", result, err)
			}
			if len(result.Images) != 0 {
				t.Fatalf("%s attached an automatic screenshot", tc.action)
			}
			if tool.snapshot != nil || len(tool.refs) != 0 {
				t.Fatalf("%s did not invalidate state after mutation", tc.action)
			}
			last := fake.calls[len(fake.calls)-1]
			if last.method != tc.method || last.params["pid"] != 42 || last.params["path"] == "" {
				t.Fatalf("last AX call = %+v", last)
			}
		})
	}
}

func TestComputerUse_GetValueKeepsState(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("computer_use runtime is macOS-only")
	}
	fake := newFakeAXCaller()
	tool := newTestComputerUse(fake)
	stateID := observeNotes(t, tool, fake, treeFixture("Save"))
	fake.queue("read_tree", treeFixture("Save"))
	fake.queue("get_value", `{"result":"hello","role":"AXTextField"}`)

	result, err := tool.Run(context.Background(), `{"action":"get_value","state_id":"`+stateID+`","ref":"e2","description":"Read note body"}`)
	if err != nil || result.IsError {
		t.Fatalf("Run result=%+v err=%v", result, err)
	}
	if !strings.Contains(result.Content, "hello") || tool.snapshot == nil || tool.snapshot.id != stateID {
		t.Fatalf("get_value result/state = %+v, snapshot=%+v", result, tool.snapshot)
	}
}

func TestComputerUse_CoordinateAndKeyboardDispatch(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("computer_use runtime is macOS-only")
	}
	tests := []struct {
		name   string
		args   string
		method string
	}{
		{"coordinate click", `{"action":"click","x":120,"y":240,"button":"right","clicks":2,"description":"Open context menu"}`, "mouse_event"},
		{"string coordinate click", `{"action":"click","x":"120","y":"240","clicks":"1","description":"Open control"}`, "mouse_event"},
		{"move", `{"action":"move","x":12,"y":24,"description":"Move pointer"}`, "mouse_event"},
		{"type", `{"action":"type","text":"hello","description":"Type greeting"}`, "type_text"},
		{"hotkey", `{"action":"hotkey","keys":"command+shift+p","description":"Open command palette"}`, "key_event"},
		{"focus", `{"action":"focus_app","app":"Notes","description":"Focus Notes"}`, "focus"},
		{"launch", `{"action":"launch_app","app":"Notes","description":"Launch Notes"}`, "launch_app"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeAXCaller()
			fake.queue(tc.method, `{"result":"done"}`)
			tool := newTestComputerUse(fake)
			result, err := tool.Run(context.Background(), tc.args)
			if err != nil || result.IsError {
				t.Fatalf("Run result=%+v err=%v", result, err)
			}
			if len(result.Images) != 0 {
				t.Fatal("mutation unexpectedly attached screenshot")
			}
			if len(fake.calls) != 1 || fake.calls[0].method != tc.method {
				t.Fatalf("AX calls = %+v, want one %s", fake.calls, tc.method)
			}
		})
	}
}

func TestComputerUse_WaitAcceptsBoundedDelayWithoutCondition(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("computer_use runtime is macOS-only")
	}
	tool := newTestComputerUse(newFakeAXCaller())
	started := time.Now()
	result, err := tool.Run(context.Background(), `{"action":"wait","timeout":0.01,"description":"Let the UI settle"}`)
	if err != nil || result.IsError {
		t.Fatalf("Run result=%+v err=%v", result, err)
	}
	if elapsed := time.Since(started); elapsed < 8*time.Millisecond || elapsed > time.Second {
		t.Fatalf("wait elapsed %v, want a short bounded delay", elapsed)
	}
	if !strings.Contains(result.Content, "waited") {
		t.Fatalf("wait result missing completion message: %q", result.Content)
	}
}

func TestComputerUse_ScreenshotIsExplicit(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("computer_use runtime is macOS-only")
	}
	fake := newFakeAXCaller()
	// Valid 1x1 transparent PNG.
	pngBytes, err := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=")
	if err != nil {
		t.Fatal(err)
	}
	fake.queue("resolve_pid", `{"pid":42}`)
	fake.queue("read_tree", treeFixture("Save"))
	fake.queue("capture_window", `{"ok":true,"image_base64":"`+base64.StdEncoding.EncodeToString(pngBytes)+`","width":1,"height":1}`)
	tool := newTestComputerUse(fake)

	result, err := tool.Run(context.Background(), `{"action":"get_app_state","app":"Notes","include_screenshot":true,"description":"Inspect Notes visually"}`)
	if err != nil || result.IsError {
		t.Fatalf("Run result=%+v err=%v", result, err)
	}
	if len(result.Images) != 1 || result.Images[0].Data == "" {
		t.Fatalf("expected one encoded image, got %+v", result.Images)
	}
	if fake.calls[len(fake.calls)-1].method != "capture_window" {
		t.Fatalf("last AX call = %+v, want capture_window", fake.calls[len(fake.calls)-1])
	}
}

func TestComputerUse_FullscreenScreenshotUsesCapturePipeline(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("computer_use runtime is macOS-only")
	}
	tool := newTestComputerUse(newFakeAXCaller())
	tool.captureScreen = func(int) (string, agent.ImageBlock, error) {
		return "/tmp/fake-computer-use.png", agent.ImageBlock{MediaType: "image/png", Data: "encoded"}, nil
	}
	result, err := tool.Run(context.Background(), `{"action":"screenshot","description":"Capture desktop"}`)
	if err != nil || result.IsError || len(result.Images) != 1 || result.Images[0].Data != "encoded" {
		t.Fatalf("Run result=%+v err=%v", result, err)
	}
}

func TestComputerUse_AXErrorsAreCategorized(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("computer_use runtime is macOS-only")
	}
	fake := newFakeAXCaller()
	fake.errors["launch_app"] = []error{errors.New("permission denied")}
	tool := newTestComputerUse(fake)
	result, err := tool.Run(context.Background(), `{"action":"launch_app","app":"Notes","description":"Launch Notes"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError || result.ErrorCategory == "" {
		t.Fatalf("AX failure lacks category: %+v", result)
	}
}

func TestComputerUse_RegisteredAndClonedPerRun(t *testing.T) {
	reg, _, cleanup := RegisterLocalTools(&config.Config{}, nil)
	defer cleanup()

	baseRaw, ok := reg.Get("computer_use")
	if !ok {
		t.Fatal("computer_use is not registered as a local tool")
	}
	base := baseRaw.(*ComputerUseTool)
	base.snapshot = &computerUseSnapshot{id: "base-state"}
	base.refs = map[string]refEntry{"e1": {path: "window[0]", pid: 1}}

	cloned := CloneWithRuntimeConfig(reg, &config.Config{})
	cloneRaw, ok := cloned.Get("computer_use")
	if !ok {
		t.Fatal("computer_use missing from per-run clone")
	}
	clone := cloneRaw.(*ComputerUseTool)
	if clone == base {
		t.Fatal("per-run clone shares ComputerUseTool pointer")
	}
	if clone.client != base.client {
		t.Fatal("per-run clone should retain the process-wide AX transport")
	}
	if clone.snapshot != nil || clone.refs != nil {
		t.Fatalf("per-run clone inherited state: snapshot=%+v refs=%+v", clone.snapshot, clone.refs)
	}

	baseAXRaw, _ := reg.Get("accessibility")
	cloneAXRaw, _ := cloned.Get("accessibility")
	if baseAXRaw == cloneAXRaw {
		t.Fatal("legacy accessibility mutable refs are shared across runs")
	}
	baseComputerRaw, _ := reg.Get("computer")
	cloneComputerRaw, _ := cloned.Get("computer")
	if baseComputerRaw == cloneComputerRaw {
		t.Fatal("native computer mutable screen dimensions are shared across runs")
	}
}

func TestComputerUse_SerializesAcrossInboundRuns(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("computer_use runtime is macOS-only")
	}
	firstAX := &blockingAXCaller{
		started: make(chan struct{}),
		release: make(chan struct{}),
		result:  json.RawMessage(treeFixture("Save")),
	}
	secondAX := &blockingAXCaller{
		started: make(chan struct{}),
		release: make(chan struct{}),
		result:  json.RawMessage(`{"result":"clicked"}`),
	}
	first := &ComputerUseTool{client: firstAX, screenW: DefaultAPIWidth, screenH: DefaultAPIHeight}
	second := &ComputerUseTool{client: secondAX, screenW: DefaultAPIWidth, screenH: DefaultAPIHeight}

	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		_, _ = first.Run(context.Background(), `{"action":"get_app_state","description":"Inspect app"}`)
	}()
	<-firstAX.started

	secondDone := make(chan struct{})
	go func() {
		defer close(secondDone)
		_, _ = second.Run(context.Background(), `{"action":"click","x":10,"y":10,"description":"Click control"}`)
	}()

	select {
	case <-secondAX.started:
		t.Fatal("a second inbound run entered computer_use during another run's GUI transaction")
	case <-time.After(100 * time.Millisecond):
	}

	close(firstAX.release)
	<-firstDone
	select {
	case <-secondAX.started:
	case <-time.After(time.Second):
		t.Fatal("second inbound run did not resume after GUI transaction completed")
	}
	close(secondAX.release)
	<-secondDone
}

// TestComputerUse_LegacyGUIToolsShareOperationLock proves the machine-wide
// interleave guarantee spans tool kinds: accessibility, computer, and
// applescript acquire the SAME GUI-operation lock as computer_use, so a
// legacy-tool call from one route cannot land between another route's
// stale-state preflight and action.
func TestComputerUse_LegacyGUIToolsShareOperationLock(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("GUI tools are macOS-only")
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel() // applescript: pre-cancelled ctx fails exec fast, no osascript runs

	cases := []struct {
		name string
		run  func() (agent.ToolResult, error)
	}{
		{"accessibility", func() (agent.ToolResult, error) {
			tool := &AccessibilityTool{client: &AXClient{}}
			// Unknown ref errors AFTER the lock, never reaching the AX client.
			return tool.Run(context.Background(), `{"action":"click","ref":"missing","description":"Click control"}`)
		}},
		{"computer", func() (agent.ToolResult, error) {
			// nil client errors AFTER the lock for the click action.
			return (&ComputerTool{}).Run(context.Background(), `{"action":"click","x":1,"y":1}`)
		}},
		{"applescript", func() (agent.ToolResult, error) {
			return (&AppleScriptTool{}).Run(cancelled, `{"script":"return 1","description":"Run script"}`)
		}},
	}

	computerUseGUIOperationMu.Lock()
	done := make([]chan struct{}, len(cases))
	for i, tc := range cases {
		ch := make(chan struct{})
		done[i] = ch
		go func(run func() (agent.ToolResult, error)) {
			defer close(ch)
			_, _ = run()
		}(tc.run)
	}
	for i, tc := range cases {
		select {
		case <-done[i]:
			computerUseGUIOperationMu.Unlock()
			t.Fatalf("%s completed while the GUI-operation lock was held; legacy tool bypasses the lock", tc.name)
		case <-time.After(100 * time.Millisecond):
		}
	}
	computerUseGUIOperationMu.Unlock()
	for i, tc := range cases {
		select {
		case <-done[i]:
		case <-time.After(5 * time.Second):
			t.Fatalf("%s did not complete after the GUI-operation lock was released", tc.name)
		}
	}
}
