package daemon

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/config"
)

func TestServerStartIsolatedSkipsBackgroundServices(t *testing.T) {
	deps := &ServerDeps{
		Config:     &config.Config{},
		ShannonDir: t.TempDir(),
	}
	server := NewServer(0, nil, deps, "test")
	server.SetIsolated(true)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("isolated server shutdown: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("isolated server did not stop")
		}
	})

	client := &http.Client{Timeout: 250 * time.Millisecond}
	deadline := time.Now().Add(3 * time.Second)
	for {
		response, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/health", server.Port()))
		if err == nil {
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("isolated server did not become ready")
		}
		time.Sleep(10 * time.Millisecond)
	}

	if server.memSvc != nil || deps.MemSvc != nil {
		t.Fatalf("isolated server started memory service: server=%p deps=%p", server.memSvc, deps.MemSvc)
	}
	select {
	case <-server.pullDone:
	default:
		t.Fatal("isolated server left pullDone open")
	}
}
