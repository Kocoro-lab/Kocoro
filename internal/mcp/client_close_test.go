package mcp

import (
	"sync"
	"testing"
	"time"
)

type closeOrderMCPClient struct {
	*fakeListToolsClient
	closeStarted chan struct{}
	unblockClose chan struct{}
	startOnce    sync.Once
}

func (c *closeOrderMCPClient) Close() error {
	c.startOnce.Do(func() { close(c.closeStarted) })
	if c.unblockClose != nil {
		<-c.unblockClose
	}
	return nil
}

func TestCloseManagedMCPClientAllowsGracefulCloseBeforeCancel(t *testing.T) {
	client := &closeOrderMCPClient{
		fakeListToolsClient: &fakeListToolsClient{},
		closeStarted:        make(chan struct{}),
	}
	cancelled := make(chan struct{})
	closeManagedMCPClient("playwright", client, func() { close(cancelled) }, 100*time.Millisecond)

	select {
	case <-client.closeStarted:
	default:
		t.Fatal("client Close was not called")
	}
	select {
	case <-cancelled:
	default:
		t.Fatal("subprocess context was not released after graceful close")
	}
}

func TestCloseManagedMCPClientCancelsAfterGracePeriod(t *testing.T) {
	unblock := make(chan struct{})
	client := &closeOrderMCPClient{
		fakeListToolsClient: &fakeListToolsClient{},
		closeStarted:        make(chan struct{}),
		unblockClose:        unblock,
	}
	var cancelOnce sync.Once
	start := time.Now()
	closeManagedMCPClient("stuck", client, func() {
		cancelOnce.Do(func() { close(unblock) })
	}, 20*time.Millisecond)

	if elapsed := time.Since(start); elapsed < 15*time.Millisecond {
		t.Fatalf("subprocess was cancelled before the graceful-close window: %s", elapsed)
	}
	select {
	case <-client.closeStarted:
	default:
		t.Fatal("client Close was not attempted before cancellation")
	}
}
