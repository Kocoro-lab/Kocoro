package agent

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

type cancellationNativeTool struct {
	started chan struct{}
}

func (t *cancellationNativeTool) Info() ToolInfo {
	return ToolInfo{Name: client.NativeComputerToolName}
}

func (t *cancellationNativeTool) RequiresApproval() bool { return false }

func (t *cancellationNativeTool) NativeToolDef() *client.NativeToolDef {
	return &client.NativeToolDef{
		Type: client.NativeComputerToolType, Name: client.NativeComputerToolName,
		DisplayWidthPx: 1024, DisplayHeightPx: 768,
	}
}

func (t *cancellationNativeTool) PrepareNativeToolRequest(ctx context.Context) error {
	close(t.started)
	<-ctx.Done()
	return ctx.Err()
}

func (t *cancellationNativeTool) Run(context.Context, string) (ToolResult, error) {
	return ToolResult{Content: "unused"}, nil
}

type completeCountingClient struct {
	calls atomic.Int32
}

func (c *completeCountingClient) Complete(
	context.Context,
	client.CompletionRequest,
) (*client.CompletionResponse, error) {
	c.calls.Add(1)
	return &client.CompletionResponse{OutputText: "unexpected", FinishReason: "end_turn"}, nil
}

func (c *completeCountingClient) CompleteStream(
	ctx context.Context,
	request client.CompletionRequest,
	_ func(client.StreamDelta),
) (*client.CompletionResponse, error) {
	return c.Complete(ctx, request)
}

func TestAgentLoopCancelledNativePreparationMakesZeroProviderRequests(t *testing.T) {
	registry := NewToolRegistry()
	native := &cancellationNativeTool{started: make(chan struct{})}
	registry.Register(native)
	provider := &completeCountingClient{}
	loop := NewAgentLoop(provider, registry, "medium", "", 5, 2000, 200, nil, nil, nil)
	loop.SetSpecificModel("claude-sonnet-4-6")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, _, err := loop.Run(ctx, "inspect the screen", nil, nil)
		done <- err
	}()
	select {
	case <-native.started:
	case <-time.After(2 * time.Second):
		t.Fatal("native preparation did not start")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil || !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled native preparation error = %T %v", err, err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("agent loop did not return after native preparation cancellation")
	}
	if calls := provider.calls.Load(); calls != 0 {
		t.Fatalf("cancelled native preparation made %d provider requests", calls)
	}
}
