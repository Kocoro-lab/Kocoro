package tools

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
)

type guiTargetFixtureClient struct {
	bundleID string
	appName  string
	fail     bool
}

func (c *guiTargetFixtureClient) Call(_ context.Context, method string, _ any) (json.RawMessage, error) {
	if c.fail {
		return nil, errors.New("target unavailable")
	}
	switch method {
	case "resolve_pid":
		return json.RawMessage(`{"pid":42}`), nil
	case "read_tree":
		payload, _ := json.Marshal(map[string]any{
			"schema_version": 1,
			"pid":            42,
			"app":            c.appName,
			"app_name":       c.appName,
			"bundle_id":      c.bundleID,
			"window_id":      7,
			"window_bounds":  map[string]any{"x": 0, "y": 0, "width": 100, "height": 100},
			"elements":       []any{},
			"ref_paths":      map[string]any{},
		})
		return payload, nil
	default:
		return nil, errors.New("unexpected method")
	}
}

func TestComputerUseGUIActionDescriptorUsesLatestObservedExactBundle(t *testing.T) {
	tool := &ComputerUseTool{
		client: &guiTargetFixtureClient{bundleID: "com.example.other", appName: "Other"},
		snapshot: &computerUseSnapshot{
			id: "s_state", app: "Notes", bundleID: "com.apple.Notes", pid: 42,
		},
	}
	descriptor, err := tool.DescribeGUIAction(context.Background(), `{"action":"press","ref":"e1","state_id":"s_state","description":"press"}`)
	if err != nil {
		t.Fatal(err)
	}
	if descriptor.Effect != agent.GUIActionMutation || descriptor.ActionKind != "press" ||
		descriptor.TargetBundleID != "com.apple.Notes" || descriptor.TargetAppName != "Notes" ||
		descriptor.ExecutionPath != "accessibility" {
		t.Fatalf("descriptor=%+v", descriptor)
	}
}

func TestComputerUseGUIActionDescriptorResolvesInitialObservationTarget(t *testing.T) {
	tool := &ComputerUseTool{client: &guiTargetFixtureClient{bundleID: "com.tinyspeck.slackmacgap", appName: "Slack"}}
	descriptor, err := tool.DescribeGUIAction(context.Background(), `{"action":"get_app_state","app":"Slack","description":"observe"}`)
	if err != nil {
		t.Fatal(err)
	}
	if descriptor.Effect != agent.GUIActionObservation || descriptor.TargetBundleID != "com.tinyspeck.slackmacgap" || descriptor.TargetAppName != "Slack" {
		t.Fatalf("descriptor=%+v", descriptor)
	}
}

func TestComputerUseUnavailableMutationsCannotBeDescribedForExecutionAuthority(t *testing.T) {
	for _, action := range []string{"focus_app", "launch_app", "set_value"} {
		t.Run(action, func(t *testing.T) {
			fake := newFakeAXCaller()
			tool := &ComputerUseTool{client: fake}
			_, err := tool.DescribeGUIAction(context.Background(), `{"action":"`+action+`","app":"Notes","description":"mutate"}`)
			if err == nil {
				t.Fatal("temporarily unavailable action received a GUI descriptor")
			}
			if len(fake.calls) != 0 {
				t.Fatalf("descriptor rejection reached AX/RPC: %+v", fake.calls)
			}
		})
	}
}

func TestComputerUseTargetBoundInputDescriptorsUseExactObservedBundle(t *testing.T) {
	windowID := 7001
	for _, action := range []string{"type", "hotkey", "keypress"} {
		t.Run(action, func(t *testing.T) {
			tool := &ComputerUseTool{
				client: &guiTargetFixtureClient{bundleID: "com.example.other", appName: "Other"},
				snapshot: &computerUseSnapshot{
					id: "s_state", app: "Notes", bundleID: "com.apple.Notes", pid: 42,
					windowID: &windowID,
				},
				refs: map[string]refEntry{
					"e2": {path: "window[0]/AXTextField[0]", role: "AXTextField", fingerprint: "axf_e2", pid: 42},
				},
			}
			args := map[string]any{"action": action, "state_id": "s_state", "description": "input"}
			switch action {
			case "type":
				args["text"] = "redacted"
				args["ref"] = "e2"
			case "hotkey":
				args["keys"] = "CMD+S"
			case "keypress":
				args["key_sequence"] = []string{"a", "b"}
				args["modifiers"] = []string{"command"}
			}
			payload, _ := json.Marshal(args)
			descriptor, err := tool.DescribeGUIAction(context.Background(), string(payload))
			if err != nil {
				t.Fatal(err)
			}
			if descriptor.Effect != agent.GUIActionMutation || descriptor.TargetBundleID != "com.apple.Notes" || descriptor.TargetAppName != "Notes" {
				t.Fatalf("descriptor=%+v", descriptor)
			}
		})
	}
}

func TestComputerUseCoordinateFocusedTypeDescriptorUsesClickBoundTarget(t *testing.T) {
	now := time.Date(2026, 7, 26, 14, 30, 0, 0, time.UTC)
	tool := &ComputerUseTool{
		client:        &guiTargetFixtureClient{bundleID: "com.example.other", appName: "Other"},
		coordinateNow: func() time.Time { return now },
		coordinateFocus: &computerUseCoordinateFocusV1{
			stateID: "s_clicked", pid: 42,
			bundleID: "com.tinyspeck.slackmacgap", app: "Slack",
			windowID: 7001, expiresAt: now.Add(time.Second),
		},
	}
	descriptor, err := tool.DescribeGUIAction(context.Background(),
		`{"action":"type","state_id":"s_clicked","text":"redacted","description":"type"}`)
	if err != nil {
		t.Fatal(err)
	}
	if descriptor.Effect != agent.GUIActionMutation ||
		descriptor.TargetBundleID != "com.tinyspeck.slackmacgap" ||
		descriptor.TargetAppName != "Slack" ||
		descriptor.ExecutionPath != "accessibility" {
		t.Fatalf("descriptor=%+v", descriptor)
	}
}

func TestComputerUseSemanticScrollDescriptorUsesExactObservedAuthority(t *testing.T) {
	windowID := 7001
	tool := &ComputerUseTool{
		client: &guiTargetFixtureClient{bundleID: "com.example.other", appName: "Other"},
		snapshot: &computerUseSnapshot{
			id: "s_state", app: "Notes", bundleID: "com.apple.Notes", pid: 42,
			windowID: &windowID, typed: true,
		},
		refs: map[string]refEntry{
			"e3": {path: "window[0]/AXScrollArea[0]", role: "AXScrollArea", fingerprint: "axf_scroll", pid: 42},
		},
	}
	descriptor, err := tool.DescribeGUIAction(context.Background(),
		`{"action":"scroll","state_id":"s_state","ref":"e3","dy":3,"description":"scroll"}`)
	if err != nil {
		t.Fatal(err)
	}
	if descriptor.Effect != agent.GUIActionMutation || descriptor.TargetBundleID != "com.apple.Notes" ||
		descriptor.TargetAppName != "Notes" || descriptor.ExecutionPath != "accessibility" {
		t.Fatalf("descriptor=%+v", descriptor)
	}
}

func TestComputerUseSemanticScrollDescriptorFailsClosedOnNonCanonicalSnapshotAuthority(t *testing.T) {
	for _, test := range []struct {
		name     string
		bundleID string
		windowID int
	}{
		{name: "whitespace bundle", bundleID: " com.apple.Notes ", windowID: 7001},
		{name: "window overflow", bundleID: "com.apple.Notes", windowID: int(uint64(^uint32(0)) + 1)},
	} {
		t.Run(test.name, func(t *testing.T) {
			tool := &ComputerUseTool{
				client: &guiTargetFixtureClient{bundleID: "com.example.other", appName: "Other"},
				snapshot: &computerUseSnapshot{
					id: "s_state", app: "Notes", bundleID: test.bundleID, pid: 42,
					windowID: &test.windowID, typed: true,
				},
				refs: map[string]refEntry{
					"e3": {path: "window[0]/AXScrollArea[0]", role: "AXScrollArea", fingerprint: "axf_scroll", pid: 42},
				},
			}
			descriptor, err := tool.DescribeGUIAction(context.Background(),
				`{"action":"scroll","state_id":"s_state","ref":"e3","dy":3,"description":"scroll"}`)
			if err != nil {
				t.Fatal(err)
			}
			if descriptor.TargetBundleID != "" {
				t.Fatalf("non-canonical authority was admitted: %+v", descriptor)
			}
		})
	}
}

func TestComputerUseGlobalInputDescriptorsFailClosedWithoutAtomicTarget(t *testing.T) {
	windowID := 7001
	for _, test := range []struct {
		name string
		args map[string]any
	}{
		{name: "type missing state", args: map[string]any{"action": "type", "text": "redacted"}},
		{name: "hotkey stale state", args: map[string]any{"action": "hotkey", "state_id": "stale", "keys": "CMD+S"}},
		{name: "keypress stale state", args: map[string]any{"action": "keypress", "state_id": "stale", "key_sequence": []string{"a"}}},
		{name: "scroll missing ref", args: map[string]any{"action": "scroll", "state_id": "s_state", "dx": 0, "dy": 3}},
		{name: "scroll stale state", args: map[string]any{"action": "scroll", "state_id": "stale", "ref": "e3", "dx": 0, "dy": 3}},
	} {
		t.Run(test.name, func(t *testing.T) {
			tool := &ComputerUseTool{
				client: &guiTargetFixtureClient{bundleID: "com.example.other", appName: "Other"},
				snapshot: &computerUseSnapshot{
					id: "s_state", app: "Notes", bundleID: "com.apple.Notes", pid: 42,
					windowID: &windowID, typed: true,
				},
				refs: map[string]refEntry{
					"e3": {path: "window[0]/AXScrollArea[0]", role: "AXScrollArea", fingerprint: "axf_scroll", pid: 42},
				},
			}
			test.args["description"] = "input"
			payload, _ := json.Marshal(test.args)
			descriptor, err := tool.DescribeGUIAction(context.Background(), string(payload))
			if err != nil {
				t.Fatal(err)
			}
			if descriptor.Effect != agent.GUIActionMutation || descriptor.TargetBundleID != "" || descriptor.TargetAppName != "Notes" {
				t.Fatalf("descriptor=%+v", descriptor)
			}
		})
	}
}

func TestAccessibilityGUIActionDescriptorUsesRefTarget(t *testing.T) {
	tool := &AccessibilityTool{
		client:       nil,
		lastPID:      42,
		lastBundleID: "com.apple.TextEdit",
		lastAppName:  "TextEdit",
		refs: map[string]refEntry{
			"e2": {pid: 42, path: "0.1", role: "AXButton"},
		},
	}
	descriptor, err := tool.DescribeGUIAction(context.Background(), `{"action":"click","ref":"e2","description":"click"}`)
	if err != nil {
		t.Fatal(err)
	}
	if descriptor.Effect != agent.GUIActionMutation || descriptor.TargetBundleID != "com.apple.TextEdit" || descriptor.ExecutionPath != "accessibility" {
		t.Fatalf("descriptor=%+v", descriptor)
	}
}

func TestAccessibilityScrollDescriptorFailsClosedWithoutAtomicTarget(t *testing.T) {
	tool := &AccessibilityTool{
		lastPID:      42,
		lastBundleID: "com.apple.TextEdit",
		lastAppName:  "TextEdit",
		refs: map[string]refEntry{
			"e2": {pid: 42, path: "0.1", role: "AXScrollArea"},
		},
	}
	descriptor, err := tool.DescribeGUIAction(context.Background(), `{"action":"scroll","ref":"e2","direction":"down","amount":3,"description":"scroll"}`)
	if err != nil {
		t.Fatal(err)
	}
	if descriptor.Effect != agent.GUIActionMutation || descriptor.TargetBundleID != "" || descriptor.TargetAppName != "TextEdit" {
		t.Fatalf("descriptor=%+v", descriptor)
	}
}

func TestLegacyGUIActionDescriptorsAreExplicitAndFailClosed(t *testing.T) {
	computer, err := (&ComputerTool{}).DescribeGUIAction(context.Background(), `{"action":"left_click","coordinate":[10,20]}`)
	if err != nil || computer.Effect != agent.GUIActionMutation || computer.TargetBundleID != "" || computer.ExecutionPath != "synthetic_coordinate" {
		t.Fatalf("computer descriptor=%+v err=%v", computer, err)
	}
	applescript, err := (&AppleScriptTool{}).DescribeGUIAction(context.Background(), `{"script":"tell application \"Finder\" to activate","description":"activate"}`)
	if err != nil || applescript.Effect != agent.GUIActionMutation || applescript.TargetBundleID != "" || applescript.ActionKind != "execute_script" {
		t.Fatalf("applescript descriptor=%+v err=%v", applescript, err)
	}
	ghostty, err := (&GhosttyTool{}).DescribeGUIAction(context.Background(), `{"action":"send_input","target":"server","text":"secret","description":"send"}`)
	if err != nil || ghostty.Effect != agent.GUIActionMutation || ghostty.TargetBundleID != ghosttyBundleID || ghostty.ActionKind != "send_input" {
		t.Fatalf("ghostty descriptor=%+v err=%v", ghostty, err)
	}
	list, err := (&GhosttyTool{}).DescribeGUIAction(context.Background(), `{"action":"list_tabs","description":"list"}`)
	if err != nil || list.Participates || list.Effect != agent.GUIActionObservation {
		t.Fatalf("ghostty list descriptor=%+v err=%v", list, err)
	}
}

func TestLegacyComputerGlobalInputDescriptorsFailClosed(t *testing.T) {
	for _, args := range []string{
		`{"action":"type","text":"redacted"}`,
		`{"action":"key","keys":"CMD+S"}`,
		`{"action":"mouse_move","coordinate":[10,20]}`,
	} {
		descriptor, err := (&ComputerTool{}).DescribeGUIAction(context.Background(), args)
		if err != nil {
			t.Fatal(err)
		}
		if descriptor.Effect != agent.GUIActionMutation || descriptor.TargetBundleID != "" {
			t.Fatalf("descriptor=%+v", descriptor)
		}
	}
}
