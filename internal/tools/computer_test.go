package tools

import (
	"context"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
)

func TestComputer_Info(t *testing.T) {
	tool := &ComputerTool{client: &AXClient{}}
	info := tool.Info()
	if info.Name != "computer" {
		t.Errorf("expected name 'computer', got %q", info.Name)
	}
	if !containsString(info.Required, "action") || !containsString(info.Required, "description") {
		t.Errorf("expected Required to contain action and description, got %v", info.Required)
	}
	props, ok := info.Parameters["properties"].(map[string]any)
	if !ok {
		t.Fatal("expected properties map in parameters")
	}
	for _, key := range []string{"action", "x", "y", "text", "keys", "button", "clicks", "description"} {
		if _, exists := props[key]; !exists {
			t.Errorf("expected property %q in schema", key)
		}
	}
}

func TestLegacyComputerAdvertisesFunctionSchemaNotProviderNative(t *testing.T) {
	tool := &ComputerTool{client: &AXClient{}}
	if _, ok := any(tool).(agent.NativeToolProvider); ok {
		t.Fatal("legacy ComputerTool must not advertise Anthropic's provider-native computer contract")
	}

	registry := agent.NewToolRegistry()
	registry.Register(tool)
	schemas := registry.SortedSchemas()
	if len(schemas) != 1 {
		t.Fatalf("schema count = %d, want 1", len(schemas))
	}
	schema := schemas[0]
	if schema.Type != "function" || schema.Function.Name != "computer" {
		t.Fatalf("legacy computer schema = %+v, want ordinary function named computer", schema)
	}
	if schema.Name != "" || schema.DisplayWidthPx != 0 || schema.DisplayHeightPx != 0 {
		t.Fatalf("legacy function schema leaked provider-native fields: %+v", schema)
	}
	if !containsString(schema.Function.Parameters["required"].([]string), "description") {
		t.Fatalf("legacy function schema does not require approval description: %+v", schema)
	}
}

func TestComputer_RequiresApproval(t *testing.T) {
	tool := &ComputerTool{client: &AXClient{}}
	if !tool.RequiresApproval() {
		t.Error("expected RequiresApproval to return true")
	}
}

func TestComputer_InvalidArgs(t *testing.T) {
	tool := &ComputerTool{client: &AXClient{}}
	result, err := tool.Run(context.Background(), `not valid json`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Error("expected error result for invalid JSON")
	}
}

func TestComputer_MissingAction(t *testing.T) {
	tool := &ComputerTool{client: &AXClient{}}
	result, err := tool.Run(context.Background(), `{}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Error("expected error result for missing action")
	}
	if !contains(result.Content, "missing required parameter: action") {
		t.Errorf("expected 'missing required parameter: action' in error, got: %s", result.Content)
	}
}

func TestComputer_UnknownAction(t *testing.T) {
	tool := &ComputerTool{client: &AXClient{}}
	result, err := tool.Run(context.Background(), `{"action": "fly", "description": "Try unknown action"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Error("expected error result for unknown action")
	}
	if !contains(result.Content, "unknown action") {
		t.Errorf("expected 'unknown action' in error, got: %s", result.Content)
	}
}

func TestComputer_TypeMissingText(t *testing.T) {
	tool := &ComputerTool{client: &AXClient{}}
	result, err := tool.Run(context.Background(), `{"action": "type", "description": "Type text"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Error("expected error result for type without text")
	}
	if !contains(result.Content, "type action requires 'text' parameter") {
		t.Errorf("expected text parameter error, got: %s", result.Content)
	}
}

func TestLegacyComputerTypeAcknowledgementDoesNotEchoTypedContent(t *testing.T) {
	secret := "ordinary typed content"
	raw := []byte(`{"result":"typed: ` + secret + `","context":{"app":"Notes","pid":42}}`)
	got := legacyComputerTypeAcknowledgement(raw)
	if contains(got, secret) || contains(got, "typed:") {
		t.Fatalf("legacy type acknowledgement leaked content: %q", got)
	}
	if !contains(got, "Typed text (content redacted)") || !contains(got, "Notes") {
		t.Fatalf("legacy type acknowledgement lost safe status/context: %q", got)
	}
}

func TestComputer_HotkeyMissingKeys(t *testing.T) {
	tool := &ComputerTool{client: &AXClient{}}
	result, err := tool.Run(context.Background(), `{"action": "hotkey", "description": "Press shortcut"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Error("expected error result for hotkey without keys")
	}
	if !contains(result.Content, "hotkey action requires 'keys' parameter") {
		t.Errorf("expected keys parameter error, got: %s", result.Content)
	}
}

func TestComputer_EscapeAppleScript(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{`hello`, `hello`},
		{`say "hi"`, `say \"hi\"`},
		{"line1\nline2", `line1\nline2`},
		{`back\slash`, `back\\slash`},
	}
	for _, tc := range tests {
		got := escapeAppleScript(tc.input)
		if got != tc.expected {
			t.Errorf("escapeAppleScript(%q) = %q, want %q", tc.input, got, tc.expected)
		}
	}
}

func TestComputer_NormalizeArgs_LeftClick(t *testing.T) {
	args := &computerArgs{Action: "left_click", Coordinate: []int{640, 400}}
	normalizeArgs(args)
	if args.Action != "click" {
		t.Errorf("expected action 'click', got %q", args.Action)
	}
	if args.X != 640 || args.Y != 400 {
		t.Errorf("expected (640, 400), got (%d, %d)", args.X, args.Y)
	}
	if args.Button != "left" {
		t.Errorf("expected button 'left', got %q", args.Button)
	}
}

func TestComputer_NormalizeArgs_RightClick(t *testing.T) {
	args := &computerArgs{Action: "right_click", Coordinate: []int{100, 200}}
	normalizeArgs(args)
	if args.Action != "click" || args.Button != "right" {
		t.Errorf("expected click/right, got %s/%s", args.Action, args.Button)
	}
}

func TestComputer_NormalizeArgs_DoubleClick(t *testing.T) {
	args := &computerArgs{Action: "double_click", Coordinate: []int{50, 50}}
	normalizeArgs(args)
	if args.Action != "click" || args.Clicks != 2 {
		t.Errorf("expected click with 2 clicks, got %s/%d", args.Action, args.Clicks)
	}
}

func TestComputer_NormalizeArgs_MouseMove(t *testing.T) {
	args := &computerArgs{Action: "mouse_move", Coordinate: []int{300, 400}}
	normalizeArgs(args)
	if args.Action != "move" {
		t.Errorf("expected 'move', got %q", args.Action)
	}
	if args.X != 300 || args.Y != 400 {
		t.Errorf("expected (300, 400), got (%d, %d)", args.X, args.Y)
	}
}

func TestComputer_NormalizeArgs_Key(t *testing.T) {
	args := &computerArgs{Action: "key", Text: "Return"}
	normalizeArgs(args)
	if args.Action != "hotkey" {
		t.Errorf("expected 'hotkey', got %q", args.Action)
	}
	if args.Keys != "Return" {
		t.Errorf("expected keys 'Return', got %q", args.Keys)
	}
}

func TestComputer_NormalizeArgs_Screenshot(t *testing.T) {
	args := &computerArgs{Action: "screenshot"}
	normalizeArgs(args)
	if args.Action != "screenshot" {
		t.Errorf("expected 'screenshot', got %q", args.Action)
	}
}

func TestComputer_NormalizeArgs_NoOp(t *testing.T) {
	// Our custom actions pass through unchanged
	args := &computerArgs{Action: "click", X: 100, Y: 200}
	normalizeArgs(args)
	if args.Action != "click" || args.X != 100 || args.Y != 200 {
		t.Errorf("expected unchanged, got %s (%d, %d)", args.Action, args.X, args.Y)
	}
}

func TestComputer_ScaleXY(t *testing.T) {
	tool := &ComputerTool{
		client: &AXClient{}, screenW: 1440, screenH: 900,
		toolW: 1280, toolH: 800,
	}
	x, y, err := tool.scaleXY(640, 400)
	if err != nil {
		t.Fatal(err)
	}
	if x != 720 || y != 450 {
		t.Errorf("expected (720, 450), got (%d, %d)", x, y)
	}
}

func TestComputer_ScaleXY_DefaultFallback(t *testing.T) {
	// When screen dims match API dims, no scaling
	tool := &ComputerTool{
		client: &AXClient{}, screenW: 1280, screenH: 800,
		toolW: 1280, toolH: 800,
	}
	x, y, err := tool.scaleXY(100, 200)
	if err != nil {
		t.Fatal(err)
	}
	if x != 100 || y != 200 {
		t.Errorf("expected (100, 200), got (%d, %d)", x, y)
	}
}

func TestComputer_LegacyToolDimensionsAndMappingMatchFinalRetinaScreenshotAspect(t *testing.T) {
	tool := &ComputerTool{
		client: &AXClient{},
		readScreenGeometry: func() (screenGeometry, error) {
			return screenGeometry{
				LogicalWidth: 2560, LogicalHeight: 1440,
				CaptureWidth: 5120, CaptureHeight: 2880,
			}, nil
		},
	}
	if err := tool.ensureScreenDims(); err != nil {
		t.Fatal(err)
	}
	if tool.toolW != 1280 || tool.toolH != 720 {
		t.Fatalf("legacy tool dimensions = %dx%d, want exact final 1280x720", tool.toolW, tool.toolH)
	}
	x, y, err := tool.scaleXY(640, 360)
	if err != nil {
		t.Fatal(err)
	}
	if x != 1280 || y != 720 {
		t.Fatalf("center maps to (%d,%d), want logical center (1280,720)", x, y)
	}
}

func TestComputer_GeometryFailureDoesNotInstallFallbackDimensions(t *testing.T) {
	tool := &ComputerTool{
		client: &AXClient{},
		readScreenGeometry: func() (screenGeometry, error) {
			return screenGeometry{}, os.ErrNotExist
		},
	}

	if err := tool.ensureScreenDims(); err == nil {
		t.Fatal("geometry failure unexpectedly installed legacy dimensions")
	}
	if tool.screenW != 0 || tool.screenH != 0 || tool.toolW != 0 || tool.toolH != 0 {
		t.Fatalf("geometry failure installed fallback dimensions screen=%dx%d tool=%dx%d",
			tool.screenW, tool.screenH, tool.toolW, tool.toolH)
	}
}

func TestComputer_GeometryFailureStopsScreenshotBeforeCapture(t *testing.T) {
	captureCalled := false
	tool := &ComputerTool{
		readScreenGeometry: func() (screenGeometry, error) {
			return screenGeometry{}, os.ErrNotExist
		},
		captureScreen: func(context.Context, int) (string, agent.ImageBlock, error) {
			captureCalled = true
			return "", agent.ImageBlock{}, os.ErrInvalid
		},
	}

	result, err := tool.screenshot(context.Background())
	if err != nil || !result.IsError || !contains(result.Content, "screen geometry unavailable") {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if captureCalled {
		t.Fatal("screenshot capture ran without trusted logical/tool dimensions")
	}
}

func TestComputer_GeometryFailureStopsCoordinateMutationsBeforeAXWrite(t *testing.T) {
	for _, action := range []string{"click", "move"} {
		t.Run(action, func(t *testing.T) {
			writer := &coordinateMouseTestWriter{writeErr: os.ErrInvalid}
			tool := &ComputerTool{
				client: coordinateMouseTestClient(writer),
				readScreenGeometry: func() (screenGeometry, error) {
					return screenGeometry{}, os.ErrNotExist
				},
			}
			args := computerArgs{Action: action, X: 100, Y: 200}
			var result agent.ToolResult
			var err error
			if action == "click" {
				result, err = tool.click(context.Background(), args)
			} else {
				result, err = tool.move(context.Background(), args)
			}
			if err != nil || !result.IsError || !contains(result.Content, "screen geometry unavailable") {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			if writer.writeCount() != 0 {
				t.Fatalf("geometry failure wrote %d AX requests", writer.writeCount())
			}
		})
	}
}

func TestComputer_ScreenshotFailsClosedWhenBytesDoNotMatchDeclaredSpace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mismatch.png")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := png.Encode(file, image.NewRGBA(image.Rect(0, 0, 1280, 720))); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	tool := &ComputerTool{
		client: &AXClient{}, screenW: 1440, screenH: 900,
		toolW: 1280, toolH: 800,
		captureScreen: func(context.Context, int) (string, agent.ImageBlock, error) {
			return path, agent.ImageBlock{MediaType: "image/png", Data: "opaque"}, nil
		},
	}
	result, err := tool.screenshot(context.Background())
	if err != nil || !result.IsError || !contains(result.Content, "do not match declared tool space") {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestComputer_ScreenshotAlwaysRemovesLegacyCaptureTemporaryFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "capture.png")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := png.Encode(file, image.NewRGBA(image.Rect(0, 0, 16, 9))); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	block, err := EncodeImage(path)
	if err != nil {
		t.Fatal(err)
	}
	tool := &ComputerTool{
		client: &AXClient{}, screenW: 16, screenH: 9,
		toolW: 16, toolH: 9,
		captureScreen: func(context.Context, int) (string, agent.ImageBlock, error) {
			return path, block, nil
		},
	}
	result, err := tool.screenshot(context.Background())
	if err != nil || result.IsError {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatalf("legacy capture temp file survived: %v", statErr)
	}
}

func TestComputer_ScreenshotFailsWhenFinalProviderBytesDifferFromCapturePath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "declared.png")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := png.Encode(file, image.NewRGBA(image.Rect(0, 0, 1280, 800))); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	providerPath := filepath.Join(t.TempDir(), "provider.png")
	providerFile, err := os.Create(providerPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := png.Encode(providerFile, image.NewRGBA(image.Rect(0, 0, 1280, 720))); err != nil {
		t.Fatal(err)
	}
	if err := providerFile.Close(); err != nil {
		t.Fatal(err)
	}
	providerBlock, err := EncodeImage(providerPath)
	if err != nil {
		t.Fatal(err)
	}
	tool := &ComputerTool{
		client: &AXClient{}, screenW: 1280, screenH: 800,
		toolW: 1280, toolH: 800,
		captureScreen: func(context.Context, int) (string, agent.ImageBlock, error) {
			return path, providerBlock, nil
		},
	}
	result, err := tool.screenshot(context.Background())
	if err != nil || !result.IsError ||
		!contains(result.Content, "final legacy computer image dimensions 1280x720") {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestComputer_ScreenshotPropagatesActionCancellationToCapture(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	captureStarted := make(chan struct{})
	tool := &ComputerTool{
		client: &AXClient{}, screenW: 16, screenH: 9,
		toolW: 16, toolH: 9,
		captureScreen: func(captureCtx context.Context, _ int) (string, agent.ImageBlock, error) {
			close(captureStarted)
			<-captureCtx.Done()
			return "", agent.ImageBlock{}, captureCtx.Err()
		},
	}
	done := make(chan agent.ToolResult, 1)
	go func() {
		result, _ := tool.screenshot(ctx)
		done <- result
	}()
	<-captureStarted
	cancel()
	select {
	case result := <-done:
		if !result.IsError || !contains(result.Content, "context canceled") {
			t.Fatalf("result = %+v", result)
		}
	case <-time.After(time.Second):
		t.Fatal("computer screenshot ignored action cancellation")
	}
}

func TestComputer_PostActionDelayStopsBeforeCaptureOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	captureCalled := false
	tool := &ComputerTool{
		captureScreen: func(context.Context, int) (string, agent.ImageBlock, error) {
			captureCalled = true
			return "", agent.ImageBlock{}, nil
		},
	}
	cancel()
	started := time.Now()
	result := tool.captureAfterAction(ctx, agent.ToolResult{Content: "mutated"})
	if elapsed := time.Since(started); elapsed > 100*time.Millisecond {
		t.Fatalf("cancelled post-action capture waited %s", elapsed)
	}
	if captureCalled || result.Content != "mutated" {
		t.Fatalf("captureCalled=%v result=%+v", captureCalled, result)
	}
}
