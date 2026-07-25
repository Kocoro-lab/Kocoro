package tools

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/config"
)

func TestAnthropicComputerAdapterAdvertisesNativeComputerWithoutProductionRegistration(t *testing.T) {
	var nilAdapter *AnthropicComputerAdapter
	if definition := nilAdapter.NativeToolDef(); definition != nil {
		t.Fatalf("nil adapter advertised native definition: %+v", definition)
	}
	raw := &ComputerUseTool{}
	adapter := NewAnthropicComputerAdapter(raw, 1280, 800)
	if adapter.Info().Name != client.NativeComputerToolName || !adapter.RequiresApproval() {
		t.Fatalf("adapter identity/approval = %q/%v", adapter.Info().Name, adapter.RequiresApproval())
	}
	definition := adapter.NativeToolDef()
	if definition == nil || definition.Type != client.NativeComputerToolType ||
		definition.Name != client.NativeComputerToolName || definition.DisplayWidthPx != 1280 ||
		definition.DisplayHeightPx != 800 {
		t.Fatalf("native definition = %+v", definition)
	}
	properties := adapter.Info().Parameters["properties"].(map[string]any)
	want := []string{
		"action", "coordinate", "duration", "key", "scroll_amount",
		"scroll_direction", "start_coordinate", "text",
	}
	if got := sortedMapKeys(properties); !reflect.DeepEqual(got, want) {
		t.Fatalf("provider fields = %v, want %v", got, want)
	}
	for _, forbidden := range []string{"state_id", "frame_id", "ref", "x", "y"} {
		if _, found := properties[forbidden]; found {
			t.Fatalf("internal field %q leaked into provider schema", forbidden)
		}
	}
	registry, _, cleanup := RegisterLocalTools(&config.Config{}, nil)
	defer cleanup()
	public, ok := registry.Get(client.NativeComputerToolName)
	if !ok {
		t.Fatal("production registry lost legacy computer")
	}
	if _, enabled := unwrapGUIExecutionGate(public).(*AnthropicComputerAdapter); enabled {
		t.Fatal("experimental adapter was enabled in the production registry")
	}
	rawRegistered, ok := registry.Get("computer_use")
	if !ok {
		t.Fatal("raw ComputerUseTool production registration unexpectedly disappeared")
	}
	if _, ok := unwrapGUIExecutionGate(rawRegistered).(*ComputerUseTool); !ok {
		t.Fatalf("raw computer_use production type = %T", unwrapGUIExecutionGate(rawRegistered))
	}
	var _ agent.Tool = adapter
	var _ agent.NativeToolProvider = adapter
	var _ agent.GUIActionDescriber = adapter
	var _ ConsequentialRiskPreflighterV1 = adapter
}

func TestAnthropicComputerAdapterStrictProviderArgumentsFailBeforeRawCalls(t *testing.T) {
	requireComputerUseDarwin(t)
	harness := newComputerUseCoordinateHarness(t)
	adapter := NewAnthropicComputerAdapter(harness.tool, 1280, 800)
	for _, payload := range []string{
		`{"action":"left_click","coordinate":[0,0],"state_id":"s_hidden"}`,
		`{"action":"left_click","coordinate":[0,0],"frame_id":"frame_hidden"}`,
		`{"action":"left_click","coordinate":[0]}`,
		`{"action":"type"}`,
		`{"action":"key","key":"command+c"}`,
		`{"action":"scroll","scroll_direction":"down","scroll_amount":1,"coordinate":[0,0]}`,
		`{"action":"middle_click","coordinate":[0,0]}`,
		`{"action":"left_mouse_down","coordinate":[0,0]}`,
		`{"action":"left_mouse_up","coordinate":[0,0]}`,
		`{"action":"hold_key","text":"a","duration":1}`,
		`{"action":"zoom","scroll_amount":1}`,
		`{"action":"cursor_position"}`,
	} {
		before := len(harness.fake.calls)
		result, err := adapter.Run(context.Background(), payload)
		if err != nil || !result.IsError || result.ErrorCategory != agent.ErrCategoryValidation {
			t.Fatalf("payload %s result=%+v err=%v", payload, result, err)
		}
		if len(harness.fake.calls) != before {
			t.Fatalf("payload %s reached raw AX calls: %+v", payload, harness.fake.calls[before:])
		}
	}
}

func TestDecodeAnthropicComputerArgsRejectsDuplicateMembers(t *testing.T) {
	tests := []struct {
		name    string
		payload string
	}{
		{
			name:    "duplicate action",
			payload: `{"action":"screenshot","action":"wait"}`,
		},
		{
			name:    "duplicate coordinate",
			payload: `{"action":"left_click","coordinate":[1,2],"coordinate":[3,4]}`,
		},
		{
			name:    "escaped equivalent coordinate",
			payload: `{"action":"left_click","coordinate":[1,2],"\u0063oordinate":[3,4]}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := decodeAnthropicComputerArgs(tt.payload)
			if err == nil {
				t.Fatal("duplicate member was accepted")
			}
			if _, ok := err.(*anthropicComputerValidationError); !ok {
				t.Fatalf("error type = %T, want *anthropicComputerValidationError", err)
			}
			if !strings.Contains(err.Error(), "duplicate JSON object member") {
				t.Fatalf("error = %q, want duplicate-member validation error", err)
			}
		})
	}
}

func TestAnthropicComputerAdapterPreparesFirstDeclarationBeforeScreenshot(t *testing.T) {
	requireComputerUseDarwin(t)
	harness := newComputerUseCoordinateHarness(t)
	adapter := NewAnthropicComputerAdapter(harness.tool, 1280, 800)
	initial := adapter.NativeToolDef()
	if initial.DisplayWidthPx != 1280 || initial.DisplayHeightPx != 800 {
		t.Fatalf("initial native definition = %+v", initial)
	}

	harness.queueObservation(harness.tree, harness.tree)
	if err := adapter.PrepareNativeToolRequest(context.Background()); err != nil {
		t.Fatalf("PrepareNativeToolRequest: %v", err)
	}
	if adapter.pendingBootstrap == nil || harness.tool.snapshot != nil ||
		harness.tool.refs != nil || harness.tool.coordinateArtifact != nil {
		t.Fatalf("native preparation did not quarantine hidden authority: pending=%+v raw=%+v/%+v/%+v",
			adapter.pendingBootstrap, harness.tool.snapshot, harness.tool.refs, harness.tool.coordinateArtifact)
	}

	frame := adapter.pendingBootstrap.artifact.Frame()
	definition := adapter.NativeToolDef()
	if definition.DisplayWidthPx != frame.FinalImage.WidthPX ||
		definition.DisplayHeightPx != frame.FinalImage.HeightPX {
		t.Fatalf("prepared native dimensions=%dx%d image=%dx%d",
			definition.DisplayWidthPx, definition.DisplayHeightPx,
			frame.FinalImage.WidthPX, frame.FinalImage.HeightPX)
	}

	// The provider's first screenshot must consume the exact one-shot prepared
	// artifact. Re-capturing here could change dimensions after the request's
	// native definition was already serialized.
	beforeFirstScreenshot := len(harness.fake.calls)
	first, err := adapter.Run(context.Background(), `{"action":"screenshot"}`)
	if err != nil || first.IsError || len(first.Images) != 1 {
		t.Fatalf("prepared first screenshot result=%+v err=%v", first, err)
	}
	if len(harness.fake.calls) != beforeFirstScreenshot {
		t.Fatalf("first screenshot recaptured after schema declaration: %+v",
			harness.fake.calls[beforeFirstScreenshot:])
	}
	for _, secret := range []string{"state_id", "frame_id", "topology", "digest", harness.tool.snapshot.id} {
		if strings.Contains(first.Content, secret) {
			t.Fatalf("provider result leaked %q: %q", secret, first.Content)
		}
	}
	if !reflect.DeepEqual(first.Images[0], harness.tool.coordinateArtifact.ImageBlock()) {
		t.Fatal("provider screenshot is not the exact raw ComputerUse artifact")
	}
	// Once delivered, the pending bootstrap is consumed and later screenshots
	// return to the normal fresh strict observation/capture path.
	harness.queueObservation(harness.tree, harness.tree)
	second, err := adapter.Run(context.Background(), `{"action":"screenshot"}`)
	if err != nil || second.IsError || len(second.Images) != 1 {
		t.Fatalf("fresh second screenshot result=%+v err=%v", second, err)
	}
	wantMethods := []string{
		"read_tree", "display_topology", "capture_coordinate_window", "read_tree",
		"read_tree", "display_topology", "capture_coordinate_window", "read_tree",
	}
	if got := fakeAXMethods(harness.fake.calls); !reflect.DeepEqual(got, wantMethods) {
		t.Fatalf("provider screenshot AX calls = %v", got)
	}
}

func TestAnthropicComputerAdapterPreparedPrivateImageCannotAuthorizeMutation(t *testing.T) {
	requireComputerUseDarwin(t)
	harness := newComputerUseCoordinateHarness(t)
	adapter := NewAnthropicComputerAdapter(harness.tool, 1280, 800)
	harness.queueObservation(harness.tree, harness.tree)
	if err := adapter.PrepareNativeToolRequest(context.Background()); err != nil {
		t.Fatalf("PrepareNativeToolRequest: %v", err)
	}
	before := len(harness.fake.calls)

	for _, payload := range []string{
		`{"action":"left_click","coordinate":[1,1]}`,
		`{"action":"type","text":"hidden"}`,
		`{"action":"wait","duration":0.01}`,
	} {
		result, err := adapter.Run(context.Background(), payload)
		if err != nil || !result.IsError || result.ErrorCategory != agent.ErrCategoryBusiness {
			t.Fatalf("payload=%s hidden-image result=%+v err=%v", payload, result, err)
		}
		if !strings.Contains(result.Content, "call screenshot first") {
			t.Fatalf("payload=%s hidden-image error = %q", payload, result.Content)
		}
		if len(harness.fake.calls) != before {
			t.Fatalf("payload=%s hidden-image reached AX/input calls: %+v",
				payload, harness.fake.calls[before:])
		}
	}
}

func TestAnthropicComputerAdapterTamperedPreparedImageFailsWithoutRecapture(t *testing.T) {
	requireComputerUseDarwin(t)
	harness := newComputerUseCoordinateHarness(t)
	adapter := NewAnthropicComputerAdapter(harness.tool, 1280, 800)
	harness.queueObservation(harness.tree, harness.tree)
	if err := adapter.PrepareNativeToolRequest(context.Background()); err != nil {
		t.Fatalf("PrepareNativeToolRequest: %v", err)
	}
	adapter.pendingBootstrap.artifact.frame.FrameID = "frame_tampered_after_prepare"
	before := len(harness.fake.calls)

	result, err := adapter.Run(context.Background(), `{"action":"screenshot"}`)
	if err != nil || !result.IsError || result.ErrorCategory != agent.ErrCategoryBusiness ||
		len(result.Images) != 0 {
		t.Fatalf("tampered bootstrap result=%+v err=%v", result, err)
	}
	if len(harness.fake.calls) != before {
		t.Fatalf("tampered bootstrap silently recaptured: %+v", harness.fake.calls[before:])
	}
	if adapter.pendingBootstrap != nil || harness.tool.snapshot != nil ||
		harness.tool.refs != nil || harness.tool.coordinateArtifact != nil {
		t.Fatal("tampered bootstrap retained hidden action authority")
	}
}

func TestAnthropicComputerAdapterPreparationFailureClearsRawAuthority(t *testing.T) {
	requireComputerUseDarwin(t)
	harness := newComputerUseCoordinateHarness(t)
	adapter := NewAnthropicComputerAdapter(harness.tool, 1280, 800)
	harness.fake.queue("read_tree", marshalComputerUseTree(t, harness.tree))
	harness.fake.queue("display_topology", `{}`)

	if err := adapter.PrepareNativeToolRequest(context.Background()); err == nil {
		t.Fatal("invalid strict capture unexpectedly prepared a native request")
	}
	if adapter.pendingBootstrap != nil || harness.tool.snapshot != nil ||
		harness.tool.refs != nil || harness.tool.coordinateArtifact != nil {
		t.Fatal("failed preparation retained partial hidden authority")
	}
}

func TestAnthropicComputerAdapterTranslationIsAccessibilityFirstAndFailClosed(t *testing.T) {
	requireComputerUseDarwin(t)
	t.Run("unique AXPress hit", func(t *testing.T) {
		harness := newComputerUseCoordinateHarness(t)
		harness.tree.Elements[0].Frame = &computerUseFrame{X: -100, Y: 200, Width: 20, Height: 20}
		harness.observe(t)
		adapter := NewAnthropicComputerAdapter(harness.tool, 1280, 800)
		requireAnthropicVisibleTestImage(t, adapter)
		harness.fake.queue("display_topology", marshalDisplayTopology(t, harness.topology))
		translated, _, err := adapter.translate(context.Background(), `{"action":"left_click","coordinate":[0,0]}`)
		if err != nil {
			t.Fatal(err)
		}
		assertTranslatedComputerUseArgs(t, translated, func(args computerUseArgs) {
			if args.Action != "press" || args.Ref != "e1" || args.StateID != harness.tool.snapshot.id ||
				args.X != nil || args.Y != nil {
				t.Fatalf("AX-first translation = %+v", args)
			}
		})
	})

	t.Run("no AXPress hit uses strict coordinate", func(t *testing.T) {
		harness := newComputerUseCoordinateHarness(t)
		harness.observe(t)
		adapter := NewAnthropicComputerAdapter(harness.tool, 1280, 800)
		requireAnthropicVisibleTestImage(t, adapter)
		harness.fake.queue("display_topology", marshalDisplayTopology(t, harness.topology))
		translated, _, err := adapter.translate(context.Background(), `{"action":"double_click","coordinate":[1,2]}`)
		if err != nil {
			t.Fatal(err)
		}
		assertTranslatedComputerUseArgs(t, translated, func(args computerUseArgs) {
			if args.Action != "click" || args.StateID != harness.tool.snapshot.id ||
				args.Clicks != 2 || args.X == nil || int(*args.X) != 1 || args.Y == nil || int(*args.Y) != 2 {
				t.Fatalf("coordinate translation = %+v", args)
			}
		})
	})

	t.Run("ambiguous AXPress hit is blocked", func(t *testing.T) {
		harness := newComputerUseCoordinateHarness(t)
		frame := &computerUseFrame{X: -100, Y: 200, Width: 20, Height: 20}
		harness.tree.Elements[0].Frame = frame
		second := harness.tree.Elements[0]
		second.Ref, second.Path, second.Fingerprint = "e2", "window[0]/AXButton[1]", "axf_coordinate_2"
		second.Frame = frame
		harness.tree.Elements = append(harness.tree.Elements, second)
		harness.tree.RefPaths["e2"] = computerUseRefPath{Path: second.Path, Role: second.Role, Fingerprint: second.Fingerprint}
		harness.observe(t)
		adapter := NewAnthropicComputerAdapter(harness.tool, 1280, 800)
		requireAnthropicVisibleTestImage(t, adapter)
		harness.fake.queue("display_topology", marshalDisplayTopology(t, harness.topology))
		if _, _, err := adapter.translate(context.Background(), `{"action":"left_click","coordinate":[0,0]}`); err == nil {
			t.Fatal("ambiguous trusted AXPress hits were accepted")
		}
	})

	t.Run("expired observation is blocked", func(t *testing.T) {
		harness := newComputerUseCoordinateHarness(t)
		harness.observe(t)
		adapter := NewAnthropicComputerAdapter(harness.tool, 1280, 800)
		requireAnthropicVisibleTestImage(t, adapter)
		harness.now = harness.now.Add(CoordinateFrameMaxTTLV1 + time.Second)
		if _, _, err := adapter.translate(context.Background(), `{"action":"mouse_move","coordinate":[0,0]}`); err == nil {
			t.Fatal("expired provider observation was accepted")
		}
	})
}

func TestAnthropicComputerAdapterMapsFocusedTypeScrollDragKeyAndWait(t *testing.T) {
	requireComputerUseDarwin(t)
	tests := []struct {
		name    string
		payload string
		prepare func(*computerUseCoordinateHarness)
		check   func(*testing.T, computerUseArgs)
	}{
		{
			name: "type unique focused ref", payload: `{"action":"type","text":"secret"}`,
			prepare: func(h *computerUseCoordinateHarness) {
				h.tree.Elements[0].Role = "AXTextField"
				h.tree.Elements[0].Focused = true
				h.tree.Elements[0].Actions = nil
				h.tree.RefPaths["e1"] = computerUseRefPath{Path: h.tree.Elements[0].Path, Role: "AXTextField", Fingerprint: h.tree.Elements[0].Fingerprint}
				focused := "e1"
				h.tree.FocusedRef = &focused
			},
			check: func(t *testing.T, args computerUseArgs) {
				if args.Action != "type" || args.Ref != "e1" || args.Text == nil || *args.Text != "secret" {
					t.Fatalf("type translation = %+v", args)
				}
			},
		},
		{
			name: "semantic scroll", payload: `{"action":"scroll","scroll_direction":"up","scroll_amount":3}`,
			prepare: func(h *computerUseCoordinateHarness) {
				h.tree.Elements[0].Role = "AXScrollArea"
				h.tree.Elements[0].Actions = nil
				h.tree.RefPaths["e1"] = computerUseRefPath{Path: h.tree.Elements[0].Path, Role: "AXScrollArea", Fingerprint: h.tree.Elements[0].Fingerprint}
			},
			check: func(t *testing.T, args computerUseArgs) {
				if args.Action != "scroll" || args.Ref != "e1" || args.DY != -3 || args.DX != 0 {
					t.Fatalf("scroll translation = %+v", args)
				}
			},
		},
		{
			name: "left drag", payload: `{"action":"left_click_drag","start_coordinate":[0,0],"coordinate":[3,4],"duration":0.42}`,
			check: func(t *testing.T, args computerUseArgs) {
				if args.Action != "drag" || args.StartX == nil || int(*args.StartX) != 0 ||
					args.StartY == nil || int(*args.StartY) != 0 || args.EndX == nil || int(*args.EndX) != 3 ||
					args.EndY == nil || int(*args.EndY) != 4 || args.DurationMS != 420 {
					t.Fatalf("drag translation = %+v", args)
				}
			},
		},
		{
			name: "key", payload: `{"action":"key","text":"command+c"}`,
			check: func(t *testing.T, args computerUseArgs) {
				if args.Action != "hotkey" || args.Keys != "command+c" || args.Ref != "" {
					t.Fatalf("key translation = %+v", args)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			harness := newComputerUseCoordinateHarness(t)
			if test.prepare != nil {
				test.prepare(harness)
			}
			harness.observe(t)
			adapter := NewAnthropicComputerAdapter(harness.tool, 1280, 800)
			requireAnthropicVisibleTestImage(t, adapter)
			harness.fake.queue("display_topology", marshalDisplayTopology(t, harness.topology))
			translated, _, err := adapter.translate(context.Background(), test.payload)
			if err != nil {
				t.Fatal(err)
			}
			assertTranslatedComputerUseArgs(t, translated, func(args computerUseArgs) {
				if args.StateID != harness.tool.snapshot.id {
					t.Fatalf("translation lost internal state authority: %+v", args)
				}
				test.check(t, args)
			})
		})
	}

	adapter := NewAnthropicComputerAdapter(&ComputerUseTool{}, 1280, 800)
	translated, mutation, err := adapter.translate(context.Background(), `{"action":"wait","duration":0.25}`)
	if err != nil || mutation {
		t.Fatalf("wait translation mutation=%v err=%v", mutation, err)
	}
	assertTranslatedComputerUseArgs(t, translated, func(args computerUseArgs) {
		if args.Action != "wait" || args.Timeout != 0.25 {
			t.Fatalf("wait translation = %+v", args)
		}
	})
}

func TestAnthropicComputerAdapterMutationReobservesAndPreservesGUIOutcome(t *testing.T) {
	requireComputerUseDarwin(t)
	harness := newComputerUseCoordinateHarness(t)
	harness.tree.Elements[0].Frame = &computerUseFrame{X: -100, Y: 200, Width: 20, Height: 20}
	frame := harness.artifact.Frame()
	adapter := NewAnthropicComputerAdapter(
		harness.tool, frame.FinalImage.WidthPX, frame.FinalImage.HeightPX)
	harness.queueObservation(harness.tree, harness.tree)
	if err := adapter.PrepareNativeToolRequest(context.Background()); err != nil {
		t.Fatalf("PrepareNativeToolRequest: %v", err)
	}
	if first, err := adapter.Run(context.Background(), `{"action":"screenshot"}`); err != nil || first.IsError {
		t.Fatalf("initial screenshot=%+v err=%v", first, err)
	}
	harness.fake.queue("display_topology", marshalDisplayTopology(t, harness.topology))
	harness.fake.queue("read_tree", marshalComputerUseTree(t, harness.tree))
	failureCode := "physical_input_interference"
	harness.tool.semanticPressExecutor = func(_ context.Context, request SemanticPressRequestV2) (SemanticPressResultV2, error) {
		if request.Ref != "e1" || request.ExpectedFingerprint != "axf_coordinate" {
			t.Fatalf("semantic press lost AX authority: %+v", request)
		}
		return SemanticPressResultV2{
			SchemaVersion: 2, Status: "user_interference", CommitState: "committed",
			Phase: "user_interference", FailureCode: &failureCode, RetrySafe: false,
		}, nil
	}
	harness.queueObservation(harness.tree, harness.tree)

	result, err := adapter.Run(context.Background(), `{"action":"left_click","coordinate":[0,0]}`)
	if err != nil || !result.IsError || result.ErrorCategory != agent.ErrCategoryBusiness || len(result.Images) != 1 {
		t.Fatalf("mutation result=%+v err=%v", result, err)
	}
	if result.GUIOutcome == nil || result.GUIOutcome.Result != agent.GUIActionResultUserInterference ||
		result.GUIOutcome.Phase != agent.GUIActionPhaseInputCommitted || result.GUIOutcome.FailureCode != failureCode {
		t.Fatalf("raw GUIOutcome was not preserved: %+v", result.GUIOutcome)
	}
	for _, secret := range []string{"state_id", "frame_id", "topology", "digest", harness.tool.snapshot.id} {
		if strings.Contains(result.Content, secret) {
			t.Fatalf("post-mutation result leaked %q: %q", secret, result.Content)
		}
	}
	if harness.tool.snapshot == nil || harness.tool.coordinateArtifact == nil ||
		!reflect.DeepEqual(result.Images[0], harness.tool.coordinateArtifact.ImageBlock()) {
		t.Fatal("mutation did not return and install the new exact screenshot")
	}
}

func TestAnthropicComputerAdapterClassifiersAndDescriptorUseTranslatedRawCore(t *testing.T) {
	adapter := NewAnthropicComputerAdapter(&ComputerUseTool{}, 1280, 800)
	for _, payload := range []string{`{"action":"screenshot"}`, `{"action":"wait","duration":1}`} {
		if !adapter.IsSafeArgs(payload) || !adapter.IsReadOnlyCall(payload) || adapter.IsConcurrencySafeCall(payload) {
			t.Fatalf("observation classification failed for %s", payload)
		}
	}
	for _, payload := range []string{`{"action":"left_click","coordinate":[0,0]}`, `{"action":"type","text":"x"}`, `not-json`} {
		if adapter.IsSafeArgs(payload) || adapter.IsReadOnlyCall(payload) || adapter.IsConcurrencySafeCall(payload) {
			t.Fatalf("mutation/invalid classification did not fail closed for %s", payload)
		}
	}
	descriptor, err := adapter.DescribeGUIAction(context.Background(), `{"action":"screenshot"}`)
	if err != nil || descriptor.Effect != agent.GUIActionObservation || descriptor.ActionKind != "get_app_state" {
		t.Fatalf("screenshot descriptor=%+v err=%v", descriptor, err)
	}
}

func TestAnthropicComputerAdapterWaitReturnsFreshExactScreenshot(t *testing.T) {
	requireComputerUseDarwin(t)
	harness := newComputerUseCoordinateHarness(t)
	frame := harness.artifact.Frame()
	adapter := NewAnthropicComputerAdapter(
		harness.tool, frame.FinalImage.WidthPX, frame.FinalImage.HeightPX)
	harness.queueObservation(harness.tree, harness.tree)
	if err := adapter.PrepareNativeToolRequest(context.Background()); err != nil {
		t.Fatalf("PrepareNativeToolRequest: %v", err)
	}
	if first, err := adapter.Run(context.Background(), `{"action":"screenshot"}`); err != nil || first.IsError {
		t.Fatalf("initial screenshot=%+v err=%v", first, err)
	}
	harness.queueObservation(harness.tree, harness.tree)

	result, err := adapter.Run(context.Background(), `{"action":"wait","duration":0.001}`)
	if err != nil || result.IsError || len(result.Images) != 1 || harness.tool.coordinateArtifact == nil {
		t.Fatalf("native wait result=%+v err=%v", result, err)
	}
	if !reflect.DeepEqual(result.Images[0], harness.tool.coordinateArtifact.ImageBlock()) {
		t.Fatal("native wait did not return its new exact screenshot")
	}
	for _, secret := range []string{"state_id", "frame_id", "topology", "digest", harness.tool.snapshot.id} {
		if strings.Contains(result.Content, secret) {
			t.Fatalf("native wait leaked %q: %q", secret, result.Content)
		}
	}
	if got := fakeAXMethods(harness.fake.calls); !reflect.DeepEqual(got,
		[]string{
			"read_tree", "display_topology", "capture_coordinate_window", "read_tree",
			"read_tree", "display_topology", "capture_coordinate_window", "read_tree",
		}) {
		t.Fatalf("native wait post-observation calls = %v", got)
	}
}

func TestComputerUseObservationRemainsAllowedAfterConsequentialRiskExecution(t *testing.T) {
	requireComputerUseDarwin(t)
	harness := newComputerUseCoordinateHarness(t)
	approved := ConsequentialRiskDraftV1{
		RequestID: "toolu_confirmed_action",
		Target: ConsequentialRiskTargetAuthorityV1{
			TargetDigest: "digest_confirmed_action",
		},
	}
	ctx := ContextWithConsequentialRiskExecutionV1(
		context.Background(), "cri_AAECAwQFBgcICQoLDA0ODw", approved,
		func(ConsequentialRiskDraftV1) error { return nil },
	)
	harness.queueObservation(harness.tree, harness.tree)
	computerUseGUIOperationMu.Lock()
	result, err := harness.tool.runWithGUIOperationLockHeld(ctx,
		`{"action":"get_app_state","description":"Post-confirmation observation","include_screenshot":true}`)
	computerUseGUIOperationMu.Unlock()
	if err != nil || result.IsError || len(result.Images) != 1 {
		t.Fatalf("risk-context observation result=%+v err=%v", result, err)
	}
}

func TestAnthropicComputerAdapterConfirmedPressReobservesInSameRiskContext(t *testing.T) {
	requireComputerUseDarwin(t)
	harness, _ := coordinateRiskHarness(t, "Send", nil)
	adapter := NewAnthropicComputerAdapter(harness.tool, 1280, 800)
	requireAnthropicVisibleTestImage(t, adapter)
	providerArgs := `{"action":"left_click","coordinate":[0,0]}`
	harness.fake.queue("display_topology", marshalDisplayTopology(t, harness.topology))
	approved, err := adapter.PreflightConsequentialRiskV1(
		context.Background(), providerArgs, "toolu_adapter_confirmed_press")
	if err != nil || approved.Status != ConsequentialRiskPreflightRequiredV1 || approved.Draft == nil {
		t.Fatalf("adapter risk preflight=%+v err=%v", approved, err)
	}

	harness.fake.queue("display_topology", marshalDisplayTopology(t, harness.topology))
	harness.fake.queue("read_tree", marshalComputerUseTree(t, harness.tree))
	consumed := 0
	failureCode := "postcondition_not_declared"
	harness.tool.semanticPressExecutor = func(_ context.Context, request SemanticPressRequestV2) (SemanticPressResultV2, error) {
		if request.RiskDestinationAssertion == nil ||
			request.RiskDestinationAssertion.ExpectedWindowTitle != harness.tree.Window {
			t.Fatalf("confirmed semantic press lost risk assertion: %+v", request)
		}
		return SemanticPressResultV2{
			SchemaVersion: 2, Status: "completed_unverified", CommitState: "committed",
			Phase: "post_verification", FailureCode: &failureCode,
		}, nil
	}
	harness.queueObservation(harness.tree, harness.tree)
	ctx := ContextWithConsequentialRiskExecutionV1(
		context.Background(), "cri_AAECAwQFBgcICQoLDA0ODw", *approved.Draft,
		func(rederived ConsequentialRiskDraftV1) error {
			if !EqualConsequentialRiskDraftV1(rederived, *approved.Draft) {
				t.Fatal("adapter grant consumer received drifted draft")
			}
			consumed++
			return nil
		},
	)
	ctx = agent.ContextWithToolInvocation(ctx, agent.ToolInvocation{
		ToolName: client.NativeComputerToolName, ToolUseID: "toolu_adapter_confirmed_press",
	})
	result, err := adapter.Run(ctx, providerArgs)
	if err != nil || result.IsError || consumed != 1 || len(result.Images) != 1 ||
		result.GUIOutcome == nil || result.GUIOutcome.Result != agent.GUIActionResultCompletedUnverified {
		t.Fatalf("confirmed adapter result=%+v err=%v consumed=%d", result, err, consumed)
	}
	if !strings.Contains(result.Content, "not verified") ||
		!strings.Contains(result.Content, "do not retry automatically") ||
		strings.Contains(result.Content, "action completed; a fresh") {
		t.Fatalf("provider-visible result washed out completed_unverified: %q", result.Content)
	}
	if harness.tool.coordinateArtifact == nil ||
		!reflect.DeepEqual(result.Images[0], harness.tool.coordinateArtifact.ImageBlock()) {
		t.Fatal("confirmed adapter mutation did not return its strict post-observation")
	}
}

func assertTranslatedComputerUseArgs(t *testing.T, payload string, check func(computerUseArgs)) {
	t.Helper()
	var args computerUseArgs
	if err := json.Unmarshal([]byte(payload), &args); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(args.Description) == "" {
		t.Fatalf("translated raw args omitted internal description: %s", payload)
	}
	check(args)
}

func requireAnthropicVisibleTestImage(t *testing.T, adapter *AnthropicComputerAdapter) {
	t.Helper()
	if err := adapter.markCurrentImageVisible(); err != nil {
		t.Fatalf("mark test provider image visible: %v", err)
	}
}

func sortedMapKeys(values map[string]any) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	// The expected provider field list above is kept in lexical order.
	slicesSortStrings(keys)
	return keys
}

func slicesSortStrings(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}
