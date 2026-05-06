package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/session"
)

// TestE2E_RouteKeyComputation verifies route keys for all entry-point scenarios.
func TestE2E_RouteKeyComputation(t *testing.T) {
	tests := []struct {
		name     string
		req      RunAgentRequest
		expected string
	}{
		{
			name:     "named agent",
			req:      RunAgentRequest{Text: "hi", Agent: "research"},
			expected: "agent:research",
		},
		{
			name:     "explicit session_id",
			req:      RunAgentRequest{Text: "hi", SessionID: "abc-123"},
			expected: "session:abc-123",
		},
		{
			name:     "new_session forces empty key",
			req:      RunAgentRequest{Text: "hi", NewSession: true},
			expected: "",
		},
		{
			name:     "slack channel routing",
			req:      RunAgentRequest{Text: "hi", Source: "slack", Channel: "#general"},
			expected: "default:slack:%23general",
		},
		{
			name:     "slack thread routing",
			req:      RunAgentRequest{Text: "hi", Source: "slack", Channel: "slack", ThreadID: "C123-1710000000.000100"},
			expected: "default:slack:C123-1710000000.000100",
		},
		{
			name:     "wecom named agent thread routing",
			req:      RunAgentRequest{Text: "hi", Agent: "ops-bot", Source: "wecom", Channel: "wecom", ThreadID: "g:group-a"},
			expected: "agent:ops-bot:wecom:g:group-a",
		},
		{
			name:     "line channel routing",
			req:      RunAgentRequest{Text: "hi", Source: "line", Channel: "group-abc"},
			expected: "default:line:group-abc",
		},
		{
			name:     "web source bypasses cache",
			req:      RunAgentRequest{Text: "hi", Source: "web", Channel: "session-1"},
			expected: "",
		},
		{
			name:     "webhook bypasses cache",
			req:      RunAgentRequest{Text: "hi", Source: "webhook", Channel: "hook-1"},
			expected: "",
		},
		{
			name:     "named webhook bypasses cache",
			req:      RunAgentRequest{Text: "hi", Agent: "ops-bot", Source: "webhook", Channel: "hook-1"},
			expected: "",
		},
		{
			name:     "cron bypasses cache",
			req:      RunAgentRequest{Text: "hi", Source: "cron", Channel: "job-1"},
			expected: "",
		},
		{
			name:     "schedule bypasses cache",
			req:      RunAgentRequest{Text: "hi", Source: "schedule", Channel: "sched-1"},
			expected: "",
		},
		{
			name:     "no context defaults to empty",
			req:      RunAgentRequest{Text: "hi"},
			expected: "",
		},
		{
			name:     "shanclaw with session_id",
			req:      RunAgentRequest{Text: "hi", Source: "shanclaw", SessionID: "sess-xyz"},
			expected: "session:sess-xyz",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key := ComputeRouteKey(tt.req)
			if key != tt.expected {
				t.Errorf("ComputeRouteKey(%+v) = %q, want %q", tt.req, key, tt.expected)
			}
		})
	}
}

func TestE2E_MessagingThreadRoutesDoNotInjectAcrossThreads(t *testing.T) {
	tests := []struct {
		name string
		a    RunAgentRequest
		b    RunAgentRequest
	}{
		{
			name: "wecom group and dm default agent",
			a:    RunAgentRequest{Source: ChannelWeCom, Channel: ChannelWeCom, ThreadID: "g:group-a"},
			b:    RunAgentRequest{Source: ChannelWeCom, Channel: ChannelWeCom, ThreadID: "u:user-a"},
		},
		{
			name: "wecom two groups default agent",
			a:    RunAgentRequest{Source: ChannelWeCom, Channel: ChannelWeCom, ThreadID: "g:group-a"},
			b:    RunAgentRequest{Source: ChannelWeCom, Channel: ChannelWeCom, ThreadID: "g:group-b"},
		},
		{
			name: "slack two threads default agent",
			a:    RunAgentRequest{Source: ChannelSlack, Channel: ChannelSlack, ThreadID: "C123-1710000000.000100"},
			b:    RunAgentRequest{Source: ChannelSlack, Channel: ChannelSlack, ThreadID: "C123-1710000000.000200"},
		},
		{
			name: "wecom group and dm named agent",
			a:    RunAgentRequest{Agent: "ops-bot", Source: ChannelWeCom, Channel: ChannelWeCom, ThreadID: "g:group-a"},
			b:    RunAgentRequest{Agent: "ops-bot", Source: ChannelWeCom, Channel: ChannelWeCom, ThreadID: "u:user-a"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sc := NewSessionCache(t.TempDir())
			defer sc.CloseAll()

			routeA := ComputeRouteKey(tt.a)
			routeB := ComputeRouteKey(tt.b)
			if routeA == "" || routeB == "" {
				t.Fatalf("expected non-empty route keys, got %q and %q", routeA, routeB)
			}
			if routeA == routeB {
				t.Fatalf("expected distinct route keys, both were %q", routeA)
			}

			injectCh := make(chan agent.InjectedMessage, 10)
			sc.mu.Lock()
			sc.routes[routeA] = &routeEntry{
				injectCh: injectCh,
				done:     make(chan struct{}),
			}
			sc.mu.Unlock()

			if result := sc.InjectMessage(routeB, agent.InjectedMessage{Text: "different thread"}); result != InjectNoActiveRun {
				t.Fatalf("expected different thread to start a new run, got %d", result)
			}
			if result := sc.InjectMessage(routeA, agent.InjectedMessage{Text: "same thread"}); result != InjectOK {
				t.Fatalf("expected same thread to inject, got %d", result)
			}

			select {
			case msg := <-injectCh:
				if msg.Text != "same thread" {
					t.Errorf("expected same-thread injection, got %q", msg.Text)
				}
			default:
				t.Fatal("expected same-thread message in inject channel")
			}
		})
	}
}

// TestE2E_InjectMessage_FullFlow verifies inject → drain flow.
// InjectMessage is called from a separate goroutine (as in production)
// while the route lock is held by the "running" goroutine.
func TestE2E_InjectMessage_FullFlow(t *testing.T) {
	sc := NewSessionCache(t.TempDir())
	defer sc.CloseAll()

	routeKey := ComputeRouteKey(RunAgentRequest{
		Source:   ChannelSlack,
		Channel:  ChannelSlack,
		ThreadID: "C123-1710000000.000100",
	})

	// Simulate an active route with injectCh (as RunAgent sets up)
	injectCh := make(chan agent.InjectedMessage, 10)
	sc.mu.Lock()
	sc.routes[routeKey] = &routeEntry{
		injectCh: injectCh,
		done:     make(chan struct{}),
	}
	sc.mu.Unlock()

	// Inject from "another goroutine" (simulating a second Slack message)
	result := sc.InjectMessage(routeKey, agent.InjectedMessage{Text: "also check stocks"})
	if result != InjectOK {
		t.Fatalf("expected InjectOK, got %d", result)
	}

	select {
	case msg := <-injectCh:
		if msg.Text != "also check stocks" {
			t.Errorf("expected 'also check stocks', got %q", msg.Text)
		}
	default:
		t.Fatal("expected message in inject channel")
	}
}

// TestE2E_CancelRoute_StopsRun verifies hard cancel.
func TestE2E_CancelRoute_StopsRun(t *testing.T) {
	sc := NewSessionCache(t.TempDir())
	defer sc.CloseAll()

	ctx, cancel := context.WithCancel(context.Background())
	routeKey := "agent:research"

	sc.mu.Lock()
	sc.routes[routeKey] = &routeEntry{
		cancel: cancel,
		done:   make(chan struct{}),
	}
	sc.mu.Unlock()

	sc.CancelRoute(routeKey)
	if ctx.Err() == nil {
		t.Fatal("expected context to be cancelled")
	}
}

// TestE2E_CancelEndpoint_HTTP verifies POST /cancel endpoint.
func TestE2E_CancelEndpoint_HTTP(t *testing.T) {
	dir := t.TempDir()
	sc := NewSessionCache(dir)

	ctx, cancel := context.WithCancel(context.Background())
	sc.mu.Lock()
	sc.routes["agent:test-agent"] = &routeEntry{
		cancel: cancel,
		done:   make(chan struct{}),
	}
	sc.mu.Unlock()

	deps := &ServerDeps{
		SessionCache: sc,
		ShannonDir:   dir,
		AgentsDir:    dir,
	}
	c := NewClient("ws://localhost:1/x", "", func(msg MessagePayload) string { return "" }, nil)
	srv := NewServer(0, c, deps, "test")
	srvCtx, srvCancel := context.WithCancel(context.Background())
	defer srvCancel()
	go srv.Start(srvCtx)
	time.Sleep(100 * time.Millisecond)

	body := strings.NewReader(`{"agent":"test-agent"}`)
	resp, err := http.Post(fmt.Sprintf("http://127.0.0.1:%d/cancel", srv.Port()), "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var result map[string]string
	json.NewDecoder(resp.Body).Decode(&result)
	if result["status"] != "cancelled" {
		t.Errorf("expected status=cancelled, got %v", result)
	}
	if ctx.Err() == nil {
		t.Fatal("expected context to be cancelled after /cancel")
	}
}

// TestE2E_InjectEndpoint_HTTP verifies POST /message returns injected.
func TestE2E_InjectEndpoint_HTTP(t *testing.T) {
	dir := t.TempDir()
	sc := NewSessionCache(dir)

	injectCh := make(chan agent.InjectedMessage, 5)
	sc.mu.Lock()
	sc.routes["session:sess-123"] = &routeEntry{
		injectCh: injectCh,
		done:     make(chan struct{}),
	}
	sc.mu.Unlock()

	deps := &ServerDeps{
		SessionCache: sc,
		ShannonDir:   dir,
		AgentsDir:    dir,
	}
	c := NewClient("ws://localhost:1/x", "", func(msg MessagePayload) string { return "" }, nil)
	srv := NewServer(0, c, deps, "test")
	srvCtx, srvCancel := context.WithCancel(context.Background())
	defer srvCancel()
	go srv.Start(srvCtx)
	time.Sleep(100 * time.Millisecond)

	body := strings.NewReader(`{"text":"follow up question","session_id":"sess-123","source":"shanclaw"}`)
	resp, err := http.Post(fmt.Sprintf("http://127.0.0.1:%d/message", srv.Port()), "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var result map[string]string
	json.NewDecoder(resp.Body).Decode(&result)
	if result["status"] != "injected" {
		t.Errorf("expected status=injected, got %v", result)
	}

	select {
	case msg := <-injectCh:
		if msg.Text != "follow up question" {
			t.Errorf("expected 'follow up question', got %q", msg.Text)
		}
	default:
		t.Fatal("expected message in inject channel")
	}
}

// TestE2E_InjectEndpoint_QueueFull_Returns429 verifies queue-full returns 429.
func TestE2E_InjectEndpoint_QueueFull_Returns429(t *testing.T) {
	dir := t.TempDir()
	sc := NewSessionCache(dir)

	injectCh := make(chan agent.InjectedMessage, 1)
	sc.mu.Lock()
	sc.routes["session:sess-456"] = &routeEntry{
		injectCh: injectCh,
		done:     make(chan struct{}),
	}
	sc.mu.Unlock()
	injectCh <- agent.InjectedMessage{Text: "first"} // fill the channel

	deps := &ServerDeps{
		SessionCache: sc,
		ShannonDir:   dir,
		AgentsDir:    dir,
	}
	c := NewClient("ws://localhost:1/x", "", func(msg MessagePayload) string { return "" }, nil)
	srv := NewServer(0, c, deps, "test")
	srvCtx, srvCancel := context.WithCancel(context.Background())
	defer srvCancel()
	go srv.Start(srvCtx)
	time.Sleep(100 * time.Millisecond)

	body := strings.NewReader(`{"text":"second message","session_id":"sess-456","source":"shanclaw"}`)
	resp, err := http.Post(fmt.Sprintf("http://127.0.0.1:%d/message", srv.Port()), "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d", resp.StatusCode)
	}

	var result map[string]string
	json.NewDecoder(resp.Body).Decode(&result)
	if result["status"] != "rejected" || result["reason"] != "queue_full" {
		t.Errorf("expected rejected/queue_full, got %v", result)
	}
}

func TestE2E_InjectEndpoint_CWDConflict_Returns409(t *testing.T) {
	dir := t.TempDir()
	sc := NewSessionCache(dir)

	injectCh := make(chan agent.InjectedMessage, 1)
	projectA := t.TempDir()
	projectB := t.TempDir()
	sc.mu.Lock()
	sc.routes["session:sess-789"] = &routeEntry{
		injectCh:  injectCh,
		done:      make(chan struct{}),
		activeCWD: projectA,
	}
	sc.mu.Unlock()

	deps := &ServerDeps{
		SessionCache: sc,
		ShannonDir:   dir,
		AgentsDir:    dir,
	}
	c := NewClient("ws://localhost:1/x", "", func(msg MessagePayload) string { return "" }, nil)
	srv := NewServer(0, c, deps, "test")
	srvCtx, srvCancel := context.WithCancel(context.Background())
	defer srvCancel()
	go srv.Start(srvCtx)
	time.Sleep(100 * time.Millisecond)

	body := strings.NewReader(fmt.Sprintf(`{"text":"switch project","session_id":"sess-789","source":"shanclaw","cwd":%q}`, projectB))
	resp, err := http.Post(fmt.Sprintf("http://127.0.0.1:%d/message", srv.Port()), "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409, got %d", resp.StatusCode)
	}

	var result map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if result["status"] != "rejected" || result["reason"] != "cwd_conflict" {
		t.Errorf("expected rejected/cwd_conflict, got %v", result)
	}
	select {
	case <-injectCh:
		t.Fatal("did not expect conflicting message to be injected")
	default:
	}
}

// TestE2E_ParallelRoutes_DontBlock verifies different routes run in parallel.
func TestE2E_ParallelRoutes_DontBlock(t *testing.T) {
	sc := NewSessionCache(t.TempDir())
	defer sc.CloseAll()

	sessDir := sc.sessionsDir("")

	var wg sync.WaitGroup
	results := make([]string, 3)

	routes := []string{
		"default:slack:%23general",
		"default:slack:%23random",
		"default:line:group-a",
	}

	for i, key := range routes {
		wg.Add(1)
		go func(idx int, routeKey string) {
			defer wg.Done()
			route := sc.LockRouteWithManager(routeKey, sessDir)
			if route == nil {
				results[idx] = "nil route"
				return
			}
			time.Sleep(50 * time.Millisecond)
			results[idx] = fmt.Sprintf("done:%s", routeKey)
			sc.UnlockRoute(routeKey)
		}(i, key)
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		for i, r := range results {
			if !strings.HasPrefix(r, "done:") {
				t.Errorf("route %d: expected done, got %q", i, r)
			}
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("parallel routes took too long — likely serializing")
	}
}

// TestE2E_SameRoute_Serializes verifies same route serializes.
func TestE2E_SameRoute_Serializes(t *testing.T) {
	sc := NewSessionCache(t.TempDir())
	defer sc.CloseAll()

	sessDir := sc.sessionsDir("")
	routeKey := "default:slack:%23general"

	var order []int
	var mu sync.Mutex

	route := sc.LockRouteWithManager(routeKey, sessDir)
	route.done = make(chan struct{})

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		route2 := sc.LockRouteWithManager(routeKey, sessDir)
		mu.Lock()
		order = append(order, 2)
		mu.Unlock()
		if route2 != nil {
			sc.UnlockRoute(routeKey)
		}
	}()

	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	order = append(order, 1)
	mu.Unlock()
	close(route.done)
	sc.UnlockRoute(routeKey)

	wg.Wait()

	if len(order) != 2 || order[0] != 1 || order[1] != 2 {
		t.Errorf("expected order [1, 2], got %v", order)
	}
}

// TestE2E_SessionPreSave verifies session metadata persists to disk.
func TestE2E_SessionPreSave(t *testing.T) {
	sessDir := t.TempDir()

	mgr := session.NewManager(sessDir)
	sess := mgr.NewSession()
	sess.Title = "pre-save test"
	sess.Source = "slack"
	sess.Channel = "#general"

	if err := mgr.Save(); err != nil {
		t.Fatalf("pre-save failed: %v", err)
	}

	mgr2 := session.NewManager(sessDir)
	loaded, err := mgr2.Resume(sess.ID)
	if err != nil {
		t.Fatalf("failed to load pre-saved session: %v", err)
	}
	if loaded.Title != "pre-save test" {
		t.Errorf("expected title 'pre-save test', got %q", loaded.Title)
	}
	if loaded.Source != "slack" {
		t.Errorf("expected source 'slack', got %q", loaded.Source)
	}
	if loaded.Channel != "#general" {
		t.Errorf("expected channel '#general', got %q", loaded.Channel)
	}
	mgr.Close()
	mgr2.Close()
}
