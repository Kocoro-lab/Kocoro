package daemon

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// wsMockServer answers WebSocket upgrades with either 401 (auth rejected)
// or a successful upgrade that immediately closes. acceptAuth=false makes
// it reject; true accepts and idles. Calls to /ws return a 401 response
// body that the dialer surfaces, which is what we need for the auth
// failure pathway.
type wsMockServer struct {
	server     *httptest.Server
	acceptAuth atomic.Bool
	upgrades   atomic.Int32
}

func newWSMockServer(acceptAuth bool) *wsMockServer {
	m := &wsMockServer{}
	m.acceptAuth.Store(acceptAuth)
	upgrader := websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}
	m.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.upgrades.Add(1)
		if !m.acceptAuth.Load() {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		// Keep open until client cancels.
		_, _, _ = conn.ReadMessage()
	}))
	return m
}

func (m *wsMockServer) url() string {
	return strings.Replace(m.server.URL, "http://", "ws://", 1)
}

func (m *wsMockServer) close() { m.server.Close() }

// --- Tests ---

func TestWSController_Start_AcceptsConnection(t *testing.T) {
	mock := newWSMockServer(true)
	defer mock.close()

	client := NewClient(mock.url(), "sk_test",
		func(MessagePayload) string { return "" },
		func(string) {},
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctl := NewWSController(ctx, client)

	ctl.Start(ctx)
	defer ctl.Stop()

	// Wait for the upgrade to be observed by the mock.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if mock.upgrades.Load() > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if mock.upgrades.Load() == 0 {
		t.Fatal("WSController did not attempt WS upgrade")
	}
	if !ctl.IsRunning() {
		t.Fatal("expected IsRunning=true after Start")
	}
}

func TestWSController_Start_Idempotent(t *testing.T) {
	mock := newWSMockServer(true)
	defer mock.close()

	client := NewClient(mock.url(), "sk_test",
		func(MessagePayload) string { return "" },
		func(string) {},
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctl := NewWSController(ctx, client)

	ctl.Start(ctx)
	defer ctl.Stop()
	ctl.Start(ctx) // second call — must be no-op
	ctl.Start(ctx) // third call — must be no-op

	time.Sleep(50 * time.Millisecond)

	// Wait until at least one upgrade reaches the mock.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if mock.upgrades.Load() > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := mock.upgrades.Load(); got != 1 {
		t.Fatalf("upgrades=%d, expected exactly 1 (Start should be idempotent)", got)
	}
}

func TestWSController_Stop_ReleasesGoroutine(t *testing.T) {
	mock := newWSMockServer(true)
	defer mock.close()

	client := NewClient(mock.url(), "sk_test",
		func(MessagePayload) string { return "" },
		func(string) {},
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctl := NewWSController(ctx, client)

	ctl.Start(ctx)
	// Wait for upgrade.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if mock.upgrades.Load() > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	ctl.Stop()

	// Wait for running flag to flip.
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !ctl.IsRunning() {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if ctl.IsRunning() {
		t.Fatal("expected IsRunning=false after Stop")
	}
}

func TestWSController_AuthRejected_StopsRetrying(t *testing.T) {
	mock := newWSMockServer(false) // 401
	defer mock.close()

	authFailureCalled := make(chan struct{}, 1)
	client := NewClient(mock.url(), "sk_revoked",
		func(MessagePayload) string { return "" },
		func(string) {},
	)
	client.SetOnAuthFailure(func() {
		select {
		case authFailureCalled <- struct{}{}:
		default:
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctl := NewWSController(ctx, client)
	ctl.Start(ctx)
	defer ctl.Stop()

	select {
	case <-authFailureCalled:
	case <-time.After(2 * time.Second):
		t.Fatal("onAuthFailure was not invoked on 401")
	}

	// RunWithReconnect should exit (no backoff retry) — give it a moment.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !ctl.IsRunning() {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if ctl.IsRunning() {
		t.Fatal("expected IsRunning=false after auth rejection")
	}

	// Confirm no backoff retries happen: wait 500ms and verify upgrade count stays at 1.
	beforeUpgrades := mock.upgrades.Load()
	time.Sleep(500 * time.Millisecond)
	if got := mock.upgrades.Load(); got != beforeUpgrades {
		t.Fatalf("upgrades grew from %d to %d after auth rejection — should not retry", beforeUpgrades, got)
	}
}

func TestWSController_Stop_BeforeStart(t *testing.T) {
	client := NewClient("ws://127.0.0.1:0", "",
		func(MessagePayload) string { return "" },
		func(string) {},
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctl := NewWSController(ctx, client)
	// Stop before Start — must not panic, IsRunning stays false.
	ctl.Stop()
	if ctl.IsRunning() {
		t.Fatal("IsRunning should be false")
	}
}
