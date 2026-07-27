package tools

import (
	"bytes"
	"context"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
)

func TestDrawAnnotationsUsesExactWindowLogicalBounds(t *testing.T) {
	input := image.NewRGBA(image.Rect(0, 0, 800, 600))
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, input); err != nil {
		t.Fatal(err)
	}
	block, err := drawAnnotationsBytes(encoded.Bytes(), []annotationEntry{{
		Label: 1, X: -900, Y: 250, Width: 20, Height: 20,
	}}, annotationViewport{X: -1000, Y: 200, Width: 400, Height: 300})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := base64.StdEncoding.DecodeString(block.Data)
	if err != nil {
		t.Fatal(err)
	}
	annotated, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	// Center: ((-890)-(-1000))/400*800 = 220, (260-200)/300*600 = 120.
	// Sample inside the red fill but outside the centered digit glyph.
	if got := color.NRGBAModel.Convert(annotated.At(228, 120)).(color.NRGBA); got.A != 230 {
		t.Fatalf("marker alpha = %#v, want an annotation pixel", got)
	}
	if got := color.NRGBAModel.Convert(annotated.At(20, 20)).(color.NRGBA); got.A != 0 {
		t.Fatal("annotation was mapped as a main-screen coordinate instead of exact-window coordinate")
	}
}

func TestDecodeExactAccessibilityWindowRejectsDimensionMismatch(t *testing.T) {
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, 80, 60))); err != nil {
		t.Fatal(err)
	}
	payload := exactAccessibilityWindowResult{
		OK: true, ImageBase64: base64.StdEncoding.EncodeToString(encoded.Bytes()),
		Width: 40, Height: 30,
	}
	signature := strings.Repeat("a", 64)
	payload.ContentSig = signature
	if _, err := decodeExactAccessibilityWindow(payload, signature); err == nil {
		t.Fatal("expected fail-closed result/image dimension mismatch")
	}
}

func TestDecodeExactAccessibilityWindowRejectsChangedAXContentSignature(t *testing.T) {
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, 80, 60))); err != nil {
		t.Fatal(err)
	}
	payload := exactAccessibilityWindowResult{
		OK: true, ImageBase64: base64.StdEncoding.EncodeToString(encoded.Bytes()),
		Width: 80, Height: 60, ContentSig: strings.Repeat("b", 64),
	}
	if _, err := decodeExactAccessibilityWindow(payload, strings.Repeat("a", 64)); err == nil ||
		!strings.Contains(err.Error(), "content signature changed") {
		t.Fatalf("signature mismatch error = %v", err)
	}
}

func TestDecodeExactVisualOnlyWindowRequiresExactPixelsWithoutContentAuthority(
	t *testing.T,
) {
	source := image.NewRGBA(image.Rect(0, 0, 2, 1))
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, source); err != nil {
		t.Fatal(err)
	}
	raw := encoded.Bytes()
	result := exactAccessibilityWindowResult{
		OK:          true,
		ImageBase64: base64.StdEncoding.EncodeToString(raw),
		Width:       2,
		Height:      1,
	}
	block, err := decodeExactVisualOnlyWindow(result)
	if err != nil || block.Data == "" {
		t.Fatalf("visual-only exact window block=%+v err=%v", block, err)
	}
	result.ContentSig = strings.Repeat("a", 64)
	if _, err := decodeExactVisualOnlyWindow(result); err == nil {
		t.Fatal("visual-only exact window accepted annotation content authority")
	}
}

func TestDrawAnnotationsDownscalesRetinaWindowToProviderBandPreservingAspect(t *testing.T) {
	input := image.NewRGBA(image.Rect(0, 0, 3200, 1800))
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, input); err != nil {
		t.Fatal(err)
	}
	block, err := drawAnnotationsBytes(
		encoded.Bytes(), nil,
		annotationViewport{X: 100, Y: 200, Width: 1600, Height: 900})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := base64.StdEncoding.DecodeString(block.Data)
	if err != nil {
		t.Fatal(err)
	}
	config, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if config.Width != 1280 || config.Height != 720 {
		t.Fatalf("annotated Retina image = %dx%d, want 1280x720", config.Width, config.Height)
	}
}

func TestAccessibility_Info(t *testing.T) {
	tool := &AccessibilityTool{client: &AXClient{}}
	info := tool.Info()
	if info.Name != "accessibility" {
		t.Errorf("expected name 'accessibility', got %q", info.Name)
	}
	for _, required := range []string{"action", "description"} {
		if !containsString(info.Required, required) {
			t.Errorf("expected required %q in %v", required, info.Required)
		}
	}
	props, ok := info.Parameters["properties"].(map[string]any)
	if !ok {
		t.Fatal("expected properties map in parameters")
	}
	for _, key := range []string{"action", "description", "app", "max_depth", "filter", "ref", "value"} {
		if _, exists := props[key]; !exists {
			t.Errorf("expected property %q in schema", key)
		}
	}
}

func TestAccessibility_RequiresApproval(t *testing.T) {
	tool := &AccessibilityTool{client: &AXClient{}}
	if !tool.RequiresApproval() {
		t.Error("accessibility mutations must participate in the approval path")
	}
}

func TestAccessibility_SafetyAndSerialization(t *testing.T) {
	tool := &AccessibilityTool{}
	for _, action := range []string{"read_tree", "annotate", "find", "get_value"} {
		args := `{"action":"` + action + `"}`
		if !tool.IsSafeArgs(args) {
			t.Errorf("%s should skip approval", action)
		}
		if tool.IsConcurrencySafeCall(args) {
			t.Errorf("%s must serialize because refs are mutable", action)
		}
	}
	for _, action := range []string{"click", "press", "set_value", "scroll"} {
		if tool.IsSafeArgs(`{"action":"` + action + `"}`) {
			t.Errorf("%s must require approval", action)
		}
	}
}

func TestAccessibility_InvalidJSON(t *testing.T) {
	tool := &AccessibilityTool{client: &AXClient{}}
	result, err := tool.Run(context.Background(), `not valid json`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Error("expected error result for invalid JSON")
	}
	if result.ErrorCategory != agent.ErrCategoryValidation {
		t.Errorf("expected validation category, got %q", result.ErrorCategory)
	}
}

func TestAccessibility_MissingAction(t *testing.T) {
	tool := &AccessibilityTool{client: &AXClient{}}
	result, err := tool.Run(context.Background(), `{}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Error("expected error result for missing action")
	}
	if !strings.Contains(result.Content, "missing required parameter: action") {
		t.Errorf("expected missing action error, got: %s", result.Content)
	}
}

func TestAccessibility_UnknownAction(t *testing.T) {
	tool := &AccessibilityTool{client: &AXClient{}}
	result, err := tool.Run(context.Background(), `{"action": "fly", "description":"Fly app"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Error("expected error result for unknown action")
	}
	if !strings.Contains(result.Content, "unknown action") {
		t.Errorf("expected 'unknown action' in error, got: %s", result.Content)
	}
}

func TestAccessibility_ClickMissingRef(t *testing.T) {
	tool := &AccessibilityTool{client: &AXClient{}}
	result, err := tool.Run(context.Background(), `{"action": "click", "description":"Click control"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Error("expected error result for click without ref")
	}
}

func TestAccessibility_ClickUnknownRef(t *testing.T) {
	tool := &AccessibilityTool{client: &AXClient{}}
	tool.refs = map[string]refEntry{"e1": {path: "window[0]", pid: 1}}
	result, err := tool.Run(context.Background(), `{"action": "click", "ref": "e99", "description":"Click control"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Error("expected error for unknown ref")
	}
	if !strings.Contains(result.Content, "unknown ref") {
		t.Errorf("expected 'unknown ref' error, got: %s", result.Content)
	}
}

func TestAccessibility_SetValueMissingValue(t *testing.T) {
	tool := &AccessibilityTool{client: &AXClient{}}
	tool.refs = map[string]refEntry{"e1": {path: "window[0]/AXTextField[0]", role: "AXTextField", pid: 1}}
	result, err := tool.Run(context.Background(), `{"action": "set_value", "ref": "e1", "description":"Set field"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Error("expected error for set_value without value")
	}
}

func TestAccessibility_NilClient(t *testing.T) {
	tool := &AccessibilityTool{} // no client
	result, err := tool.Run(context.Background(), `{"action": "read_tree", "description":"Inspect app"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Error("expected error for nil client")
	}
}
