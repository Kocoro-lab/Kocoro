package tools

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"testing"
)

func TestScreenshot_Info(t *testing.T) {
	tool := &ScreenshotTool{}
	info := tool.Info()
	if info.Name != "screenshot" {
		t.Errorf("expected name 'screenshot', got %q", info.Name)
	}
	props, ok := info.Parameters["properties"].(map[string]any)
	if !ok {
		t.Fatal("expected properties map in parameters")
	}
	for _, key := range []string{"description", "target", "path", "delay"} {
		if _, exists := props[key]; !exists {
			t.Errorf("expected property %q in schema", key)
		}
	}
	if len(info.Required) != 1 || info.Required[0] != "description" {
		t.Errorf("expected description to be required, got %v", info.Required)
	}
}

func TestScreenshot_RequiresApproval(t *testing.T) {
	tool := &ScreenshotTool{}
	if !tool.RequiresApproval() {
		t.Error("expected RequiresApproval to return true")
	}
}

func TestScreenshot_InvalidArgs(t *testing.T) {
	tool := &ScreenshotTool{}
	result, err := tool.Run(context.Background(), `not valid json`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Error("expected error result for invalid JSON")
	}
}

func TestScreenshot_UnknownTarget(t *testing.T) {
	tool := &ScreenshotTool{}
	result, err := tool.Run(context.Background(), `{"target": "webcam","description":"test invalid target"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Error("expected error result for unknown target")
	}
	if !contains(result.Content, "unknown target") {
		t.Errorf("expected 'unknown target' in error, got: %s", result.Content)
	}
}

func TestScreenshot_BuildArgs_Fullscreen(t *testing.T) {
	args := buildScreencaptureArgs("fullscreen", "/tmp/test.png", 0)
	if len(args) != 1 || args[0] != "/tmp/test.png" {
		t.Errorf("expected [/tmp/test.png], got %v", args)
	}
}

func TestScreenshot_BuildArgs_Window(t *testing.T) {
	args := buildScreencaptureArgs("window", "/tmp/test.png", 0)
	expected := []string{"-w", "/tmp/test.png"}
	if len(args) != len(expected) {
		t.Fatalf("expected %v, got %v", expected, args)
	}
	for i, v := range expected {
		if args[i] != v {
			t.Errorf("args[%d] = %q, want %q", i, args[i], v)
		}
	}
}

func TestScreenshot_BuildArgs_Region(t *testing.T) {
	args := buildScreencaptureArgs("region", "/tmp/test.png", 0)
	expected := []string{"-s", "/tmp/test.png"}
	if len(args) != len(expected) {
		t.Fatalf("expected %v, got %v", expected, args)
	}
	for i, v := range expected {
		if args[i] != v {
			t.Errorf("args[%d] = %q, want %q", i, args[i], v)
		}
	}
}

func TestScreenshot_BuildArgs_WithDelay(t *testing.T) {
	args := buildScreencaptureArgs("fullscreen", "/tmp/test.png", 3)
	expected := []string{"-T", "3", "/tmp/test.png"}
	if len(args) != len(expected) {
		t.Fatalf("expected %v, got %v", expected, args)
	}
	for i, v := range expected {
		if args[i] != v {
			t.Errorf("args[%d] = %q, want %q", i, args[i], v)
		}
	}
}

func TestScreenshot_BuildArgs_WindowWithDelay(t *testing.T) {
	args := buildScreencaptureArgs("window", "/tmp/out.png", 5)
	expected := []string{"-T", "5", "-w", "/tmp/out.png"}
	if len(args) != len(expected) {
		t.Fatalf("expected %v, got %v", expected, args)
	}
	for i, v := range expected {
		if args[i] != v {
			t.Errorf("args[%d] = %q, want %q", i, args[i], v)
		}
	}
}

func TestScreenshot_ReturnsImageBlock(t *testing.T) {
	// Create a minimal valid PNG file to simulate screencapture output
	img := image.NewRGBA(image.Rect(0, 0, 1, 1))
	img.Set(0, 0, color.RGBA{255, 0, 0, 255})
	var buf bytes.Buffer
	png.Encode(&buf, img)

	path := filepath.Join(t.TempDir(), "test-screenshot.png")
	os.WriteFile(path, buf.Bytes(), 0644)

	block, err := EncodeImage(path)
	if err != nil {
		t.Fatalf("EncodeImage error: %v", err)
	}
	if block.MediaType != "image/png" {
		t.Errorf("expected image/png, got %s", block.MediaType)
	}
	if block.Data == "" {
		t.Error("expected non-empty base64 data")
	}
}
