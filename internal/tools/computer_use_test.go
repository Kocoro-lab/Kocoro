package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
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
	return `{"schema_version":1,"app":"Notes","app_name":"Notes","bundle_id":"com.apple.Notes",` +
		`"pid":42,"window":"Note","window_title":"Note","window_id":7001,` +
		`"window_frame":{"x":0,"y":0,"width":800,"height":600},"focused_ref":null,"elements":[` +
		`{"ref":"e1","fingerprint":"axf_e1","path":"window[0]/AXButton[0]","role":"AXButton","title":` + mustJSON(title) + `,"value_redacted":false,"enabled":true,"focused":false,"selected":false,"actions":["AXPress"],"children":[]},` +
		`{"ref":"e2","fingerprint":"axf_e2","path":"window[0]/AXTextField[0]","role":"AXTextField","title":"Body","value":"hello","value_redacted":false,"enabled":true,"focused":false,"selected":false,"actions":[],"children":[]}` +
		`],"ref_paths":{"e1":{"path":"window[0]/AXButton[0]","role":"AXButton","fingerprint":"axf_e1"},` +
		`"e2":{"path":"window[0]/AXTextField[0]","role":"AXTextField","fingerprint":"axf_e2"}}}`
}

func mustJSON(value string) string {
	b, _ := json.Marshal(value)
	return string(b)
}

func newTestComputerUse(fake *fakeAXCaller) *ComputerUseTool {
	return &ComputerUseTool{client: fake}
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

func TestComputerUseSemanticPressStaleRefIsTypedPrecommitAndCanReobserve(
	t *testing.T,
) {
	fake := newFakeAXCaller()
	tool := newTestComputerUse(fake)
	stateID := observeNotes(t, tool, fake, treeFixture("Save"))

	fake.queue("read_tree", treeFixture("Save As"))
	result, err := tool.Run(
		context.Background(),
		fmt.Sprintf(
			`{"action":"press","state_id":%q,"ref":"e1","description":"Press Save"}`,
			stateID,
		),
	)
	if err != nil {
		t.Fatalf("stale press: %v", err)
	}
	if !result.IsError ||
		result.GUIOutcome == nil ||
		result.GUIOutcome.Result != agent.GUIActionResultFailed ||
		result.GUIOutcome.Phase != agent.GUIActionPhaseActing ||
		result.GUIOutcome.FailureCode != "stale_state" {
		t.Fatalf("stale press outcome = %+v", result)
	}
	if got := fakeAXMethods(fake.calls); !reflect.DeepEqual(
		got,
		[]string{"resolve_pid", "read_tree", "read_tree"},
	) {
		t.Fatalf("stale press reached a mutation helper: %v", got)
	}

	fake.queue("resolve_pid", `{"pid":42}`)
	fake.queue("read_tree", treeFixture("Save As"))
	fresh, err := tool.Run(
		context.Background(),
		`{"action":"get_app_state","app":"Notes","description":"Re-observe Notes"}`,
	)
	if err != nil || fresh.IsError {
		t.Fatalf("fresh observation result=%+v err=%v", fresh, err)
	}
	if strings.Contains(fresh.Content, "state_id: "+stateID) {
		t.Fatalf("fresh observation reused stale state: %q", fresh.Content)
	}
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
	for _, name := range []string{"action", "description", "state_id", "app", "ref", "value", "x", "y", "dx", "dy", "text", "keys", "include_screenshot"} {
		if _, ok := props[name]; !ok {
			t.Errorf("schema missing %q", name)
		}
	}
	if _, leaked := props["frame_id"]; leaked {
		t.Error("internal coordinate frame_id leaked into model tool schema")
	}
	actionSpec, ok := props["action"].(map[string]any)
	if !ok {
		t.Fatal("action schema missing")
	}
	actionDescription, _ := actionSpec["description"].(string)
	if !strings.Contains(actionDescription, ", scroll") {
		t.Fatalf("model schema must advertise strict semantic scroll, got %q", actionDescription)
	}
	for field, required := range map[string][]string{
		"dx": {"dx > 0", "AXIncrement", "right", "dx < 0", "AXDecrement", "left"},
		"dy": {"dy > 0", "AXIncrement", "down", "dy < 0", "AXDecrement", "up"},
	} {
		description := props[field].(map[string]any)["description"].(string)
		for _, phrase := range required {
			if !strings.Contains(description, phrase) {
				t.Fatalf("%s schema omitted sign semantics %q: %q", field, phrase, description)
			}
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

func TestComputerUseReadTreeFallsBackToTypedCGWindowTarget(t *testing.T) {
	fake := newFakeAXCaller()
	fake.errors["read_tree"] = []error{errors.New("no accessible windows")}
	fake.queue("read_window_target",
		`{"schema_version":1,"app":"Slack","app_name":"Slack",`+
			`"bundle_id":"com.tinyspeck.slackmacgap","pid":77,`+
			`"window":"General","window_title":"General","window_id":901,`+
			`"window_frame":{"x":10,"y":20,"width":1200,"height":800},`+
			`"focused_ref":null,"elements":[],"ref_paths":{}}`)
	tool := newTestComputerUse(fake)

	tree, result, ok := tool.readTree(context.Background(), 77, "interactive", 25)

	if !ok || result.IsError {
		t.Fatalf("readTree fallback failed: %+v", result)
	}
	if tree.PID != 77 || tree.BundleID != "com.tinyspeck.slackmacgap" ||
		tree.WindowID == nil || *tree.WindowID != 901 ||
		tree.WindowFrame == nil || tree.SchemaVersion != 1 {
		t.Fatalf("fallback tree = %+v", tree)
	}
	if len(fake.calls) != 2 ||
		fake.calls[0].method != "read_tree" ||
		fake.calls[1].method != "read_window_target" {
		t.Fatalf("calls = %+v", fake.calls)
	}
}

func TestComputerUseReadTreeReplacesIncompleteAXIdentityWithCGWindowTarget(
	t *testing.T,
) {
	fake := newFakeAXCaller()
	fake.queue("read_tree",
		`{"schema_version":1,"app":"Slack","app_name":"Slack",`+
			`"bundle_id":"com.tinyspeck.slackmacgap","pid":77,`+
			`"window":"General","window_title":"General","window_id":null,`+
			`"window_frame":{"x":11,"y":21,"width":1198,"height":798},`+
			`"focused_ref":"e1","elements":[{"ref":"e1","fingerprint":"axf",`+
			`"path":"window[0]/AXGroup[0]","role":"AXGroup",`+
			`"value_redacted":false,"enabled":true,"focused":true,`+
			`"selected":false,"actions":[]}],`+
			`"ref_paths":{"e1":{"path":"window[0]/AXGroup[0]",`+
			`"role":"AXGroup","fingerprint":"axf"}}}`)
	fake.queue("read_window_target",
		`{"schema_version":1,"app":"Slack","app_name":"Slack",`+
			`"bundle_id":"com.tinyspeck.slackmacgap","pid":77,`+
			`"window":"General","window_title":"General","window_id":901,`+
			`"window_frame":{"x":10,"y":20,"width":1200,"height":800},`+
			`"focused_ref":null,"elements":[],"ref_paths":{}}`)
	tool := newTestComputerUse(fake)

	tree, result, ok := tool.readTree(context.Background(), 77, "interactive", 25)

	if !ok || result.IsError {
		t.Fatalf("readTree fallback failed: %+v", result)
	}
	if tree.WindowID == nil || *tree.WindowID != 901 ||
		tree.WindowFrame == nil || tree.WindowFrame.X != 10 ||
		len(tree.Elements) != 0 || tree.FocusedRef != nil ||
		len(tree.RefPaths) != 0 {
		t.Fatalf("incomplete AX identity was mixed with CG authority: %+v", tree)
	}
}

func TestComputerUse_InfoDoesNotAdvertiseTemporarilyUnavailableMutations(t *testing.T) {
	info := (&ComputerUseTool{}).Info()
	props := info.Parameters["properties"].(map[string]any)
	actionSpec := props["action"].(map[string]any)
	actionDescription, _ := actionSpec["description"].(string)
	for _, action := range []string{"focus_app", "launch_app", "set_value"} {
		if strings.Contains(actionDescription, action) {
			t.Fatalf("model schema advertises temporarily unavailable action %q: %q", action, actionDescription)
		}
	}
	valueSpec := props["value"].(map[string]any)
	valueDescription, _ := valueSpec["description"].(string)
	if strings.Contains(valueDescription, "set_value") {
		t.Fatalf("model schema still describes value as set_value input: %q", valueDescription)
	}
}

func TestComputerUse_TemporarilyUnavailableMutationsFailBeforeAX(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("computer_use runtime is macOS-only")
	}
	for _, test := range []struct {
		action string
		args   string
	}{
		{action: "focus_app", args: `{"action":"focus_app","app":"Notes","description":"Focus Notes"}`},
		{action: "launch_app", args: `{"action":"launch_app","app":"Notes","description":"Launch Notes"}`},
		{action: "set_value", args: `{"action":"set_value","state_id":"s1","ref":"e1","value":"redacted","description":"Set field"}`},
	} {
		t.Run(test.action, func(t *testing.T) {
			fake := newFakeAXCaller()
			result, err := newTestComputerUse(fake).Run(context.Background(), test.args)
			if err != nil || !result.IsError || result.ErrorCategory != agent.ErrCategoryBusiness ||
				!strings.Contains(result.Content, test.action) ||
				!strings.Contains(result.Content, "temporarily unavailable") {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			if len(fake.calls) != 0 {
				t.Fatalf("temporarily unavailable %s reached AX/RPC: %+v", test.action, fake.calls)
			}
		})
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
		if call.method == "click" || call.method == "semantic_press" || call.method == "mouse_event" {
			t.Fatalf("mutation reached ax_server despite stale preflight: %+v", call)
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
	}{
		{"click", "e1", ""},
		{"press", "e1", ""},
	}
	for _, tc := range tests {
		t.Run(tc.action, func(t *testing.T) {
			fake := newFakeAXCaller()
			tool := newTestComputerUse(fake)
			stateID := observeNotes(t, tool, fake, treeFixture("Save"))
			fake.queue("read_tree", treeFixture("Save"))
			var executed bool
			tool.semanticPressExecutor = func(
				context.Context, SemanticPressRequestV2,
			) (SemanticPressResultV2, error) {
				executed = true
				code := "postcondition_not_declared"
				return SemanticPressResultV2{
					SchemaVersion: 2, Status: "completed_unverified", CommitState: "committed",
					Phase: "post_verification", FailureCode: &code,
				}, nil
			}
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
			if !executed || last.method != "read_tree" {
				t.Fatalf("typed semantic press executed=%t, last generic AX call = %+v", executed, last)
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

func TestComputerUse_ScreenshotUsesExactTargetWindowObservation(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("computer_use runtime is macOS-only")
	}
	harness := newComputerUseCoordinateHarness(t)
	harness.fake.queue("resolve_pid", `{"pid":42}`)
	harness.queueObservation(harness.tree, harness.tree)

	result, err := harness.tool.Run(
		context.Background(),
		`{"action":"screenshot","app":"Fixture App","description":"Capture Fixture App"}`,
	)
	if err != nil || result.IsError || len(result.Images) != 1 {
		t.Fatalf("Run result=%+v err=%v", result, err)
	}
	if harness.tool.coordinateArtifact == nil ||
		!strings.Contains(result.Content, "state_id: ") ||
		!strings.Contains(result.Content, "app: Fixture App") {
		t.Fatalf("screenshot did not publish exact-window state and coordinates: %+v", result)
	}
}

func TestComputerUse_RegisteredAndClonedPerRun(t *testing.T) {
	reg, _, cleanup := RegisterLocalTools(&config.Config{}, nil)
	defer cleanup()

	baseRaw, ok := reg.Get("computer_use")
	if !ok {
		t.Fatal("computer_use is not registered as a local tool")
	}
	base, ok := unwrapGUIExecutionGate(baseRaw).(*ComputerUseTool)
	if !ok {
		t.Fatalf("registered computer_use inner type = %T", unwrapGUIExecutionGate(baseRaw))
	}
	base.snapshot = &computerUseSnapshot{id: "base-state"}
	base.refs = map[string]refEntry{"e1": {path: "window[0]", pid: 1}}
	base.coordinateArtifact = &CoordinateWindowArtifactV1{}
	base.coordinateFocus = &computerUseCoordinateFocusV1{stateID: "base-state"}
	base.navigationCommit = &computerUseNavigationCommitV1{pid: 1}

	cloned := CloneWithRuntimeConfig(reg, &config.Config{})
	cloneRaw, ok := cloned.Get("computer_use")
	if !ok {
		t.Fatal("computer_use missing from per-run clone")
	}
	clone, ok := unwrapGUIExecutionGate(cloneRaw).(*ComputerUseTool)
	if !ok {
		t.Fatalf("cloned computer_use inner type = %T", unwrapGUIExecutionGate(cloneRaw))
	}
	if clone == base {
		t.Fatal("per-run clone shares ComputerUseTool pointer")
	}
	if clone.client != base.client {
		t.Fatal("per-run clone should retain the process-wide AX transport")
	}
	if clone.coordinateExecutor == nil {
		t.Fatal("per-run clone lost the typed coordinate executor")
	}
	if clone.targetBoundInputExecutor == nil {
		t.Fatal("per-run clone lost the typed target-bound input executor")
	}
	if clone.semanticPressExecutor == nil {
		t.Fatal("per-run clone lost the typed semantic press executor")
	}
	if clone.snapshot != nil || clone.refs != nil || clone.coordinateArtifact != nil ||
		clone.coordinateFocus != nil || clone.navigationCommit != nil {
		t.Fatalf("per-run clone inherited state: snapshot=%+v refs=%+v artifact=%+v focus=%+v navigation=%+v",
			clone.snapshot, clone.refs, clone.coordinateArtifact, clone.coordinateFocus,
			clone.navigationCommit)
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
		result:  json.RawMessage(treeFixture("Send")),
	}
	first := &ComputerUseTool{client: firstAX}
	second := &ComputerUseTool{client: secondAX}

	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		_, _ = first.Run(context.Background(), `{"action":"get_app_state","description":"Inspect app"}`)
	}()
	<-firstAX.started

	secondDone := make(chan struct{})
	go func() {
		defer close(secondDone)
		_, _ = second.Run(context.Background(), `{"action":"get_app_state","description":"Inspect second app"}`)
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
			return (&ComputerTool{}).Run(context.Background(), `{"action":"click","x":1,"y":1,"description":"Click coordinate"}`)
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

func TestComputerUseCallErrorClassifiesDisplayTopologyReconfiguration(t *testing.T) {
	result := computerUseCallError(
		"future topology observer",
		fmt.Errorf("read display topology v1: %w", ErrDisplayTopologyReconfiguringV1),
	)

	if !result.IsError || !result.IsRetryable ||
		result.GUIOutcome == nil ||
		result.GUIOutcome.Result != agent.GUIActionResultFailed ||
		result.GUIOutcome.Phase != agent.GUIActionPhaseObserving ||
		result.GUIOutcome.FailureCode !=
			ComputerUseFailureDisplayTopologyReconfiguringV1 {
		t.Fatalf("display topology result = %+v", result)
	}
}
