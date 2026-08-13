package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/config"
	"github.com/Kocoro-lab/ShanClaw/internal/uploads"
)

var authSensitivePostOverlayNames = []string{
	"cloud_delegate",
	"publish_to_web",
	"list_my_published_files",
	"retract_published_file",
	"generate_image",
	"edit_image",
}

func registerAuthSensitivePostOverlaysForTest(
	reg *agent.ToolRegistry,
	gw *client.GatewayClient,
	cfg *config.Config,
) {
	RegisterCloudDelegate(reg, gw, cfg, nil, "", "")
	RegisterPublishTool(reg, gw, cfg)
	RegisterListPublishedFilesTool(reg, gw, cfg)
	RegisterRetractPublishedFileTool(reg, gw, cfg)
	RegisterGenerateImageTool(reg, gw, cfg)
	RegisterEditImageTool(reg, gw, cfg)
}

func assertStaleAuthSensitiveResult(t *testing.T, name string, result agent.ToolResult, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s Run: %v", name, err)
	}
	if !result.IsError || result.ErrorCategory != agent.ErrCategoryBusiness ||
		!result.SideEffectKnownNoEffect || result.SideEffectOutcomeUnknown {
		t.Fatalf("%s stale result = %#v", name, result)
	}
}

func TestAuthSensitivePostOverlays_CapturedRuntimeCloneFailsClosedAfterPrincipalMutation(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*client.GatewayClient)
	}{
		{
			name: "sign_out",
			mutate: func(gw *client.GatewayClient) {
				gw.SetAPIKey("")
			},
		},
		{
			name: "account_switch",
			mutate: func(gw *client.GatewayClient) {
				gw.SetAPIKey("new-key")
				gw.BindIntegrationPrincipal("account-b", 2)
			},
		},
		{
			name: "same_account_rotation",
			mutate: func(gw *client.GatewayClient) {
				gw.SetAPIKey("old-key")
				gw.BindIntegrationPrincipal("account-a", 2)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				w.WriteHeader(http.StatusInternalServerError)
			}))
			defer server.Close()

			gw := client.NewGatewayClient(server.URL, "old-key")
			gw.BindIntegrationPrincipal("account-a", 1)
			cfg := &config.Config{
				Endpoint: server.URL,
				APIKey:   "old-key",
				Cloud:    config.CloudConfig{Enabled: true},
			}
			live := agent.NewToolRegistry()
			registerAuthSensitivePostOverlaysForTest(live, gw, cfg)
			captured := CloneWithRuntimeConfig(live, cfg)
			assertAuthSensitiveConcreteContracts(t, captured)

			// This is the execution boundary reached after LLM thinking,
			// deferred loading, an approval wait, or same-batch capture: the
			// pointer was captured before rotation but Run starts afterwards.
			tt.mutate(gw)
			for _, name := range authSensitivePostOverlayNames {
				tool, ok := captured.Get(name)
				if !ok {
					t.Fatalf("captured registry missing %s", name)
				}
				result, err := tool.Run(context.Background(), `{}`)
				assertStaleAuthSensitiveResult(t, name, result, err)
			}
			if got := requests.Load(); got != 0 {
				t.Fatalf("stale captured tools dispatched %d HTTP request(s)", got)
			}
		})
	}
}

func assertAuthSensitiveConcreteContracts(t *testing.T, reg *agent.ToolRegistry) {
	t.Helper()
	cloudRaw, ok := reg.Get("cloud_delegate")
	if !ok {
		t.Fatal("captured registry missing cloud_delegate")
	}
	cloud, ok := cloudRaw.(*CloudDelegateTool)
	if !ok {
		t.Fatalf("cloud_delegate type = %T, want *CloudDelegateTool", cloudRaw)
	}
	cloud.SetHandler(nil)
	cloud.SetAgentContext("test-agent", "test-prompt")
	if got := agent.EffectiveToolExposure(cloud); got != agent.ToolExposureDirect {
		t.Fatalf("cloud_delegate exposure = %q, want direct", got)
	}
	if safe, ok := agent.Tool(cloud).(agent.SafeChecker); !ok || safe.IsSafeArgs(`{}`) {
		t.Fatal("cloud_delegate lost its fail-closed SafeChecker contract")
	}

	for _, name := range []string{
		"publish_to_web",
		"list_my_published_files",
		"retract_published_file",
		"generate_image",
		"edit_image",
	} {
		tool, ok := reg.Get(name)
		if !ok {
			t.Fatalf("captured registry missing %s", name)
		}
		if got := agent.EffectiveToolExposure(tool); got != agent.ToolExposureDeferred {
			t.Fatalf("%s exposure = %q, want deferred", name, got)
		}
	}

	listRaw, _ := reg.Get("list_my_published_files")
	list, ok := listRaw.(*ListPublishedFilesTool)
	if !ok {
		t.Fatalf("list_my_published_files type = %T, want *ListPublishedFilesTool", listRaw)
	}
	if readOnly, ok := agent.Tool(list).(agent.ReadOnlyChecker); !ok || !readOnly.IsReadOnlyCall(`{}`) {
		t.Fatal("list_my_published_files lost its ReadOnlyChecker contract")
	}

	retract, _ := reg.Get("retract_published_file")
	if _, ok := retract.(agent.ReadOnlyChecker); ok {
		t.Fatal("retract_published_file unexpectedly became read-only")
	}
	for _, name := range []string{"publish_to_web", "generate_image", "edit_image"} {
		tool, _ := reg.Get(name)
		safe, ok := tool.(agent.SafeChecker)
		if !ok || safe.IsSafeArgs(`{}`) {
			t.Fatalf("%s lost its fail-closed SafeChecker contract", name)
		}
	}
}

func TestAuthSensitivePostOverlay_NewGenerationSucceedsAfterSameAccountRotation(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/uploads" {
			t.Errorf("request = %s %s, want GET /api/v1/uploads", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("X-API-Key"); got != "same-key" {
			t.Errorf("X-API-Key = %q, want same-key", got)
		}
		_ = json.NewEncoder(w).Encode(uploads.ListResponse{})
	}))
	defer server.Close()

	gw := client.NewGatewayClient(server.URL, "same-key")
	gw.BindIntegrationPrincipal("account-a", 1)
	cfg := &config.Config{
		Endpoint: server.URL,
		APIKey:   "same-key",
		Cloud:    config.CloudConfig{Enabled: true},
	}
	oldRegistry := agent.NewToolRegistry()
	RegisterListPublishedFilesTool(oldRegistry, gw, cfg)
	oldTool, ok := oldRegistry.Get("list_my_published_files")
	if !ok {
		t.Fatal("old generation did not register list_my_published_files")
	}

	gw.SetAPIKey("same-key")
	gw.BindIntegrationPrincipal("account-a", 2)
	result, err := oldTool.Run(context.Background(), `{}`)
	assertStaleAuthSensitiveResult(t, "list_my_published_files", result, err)
	if got := requests.Load(); got != 0 {
		t.Fatalf("old same-account generation dispatched %d request(s)", got)
	}

	newRegistry := agent.NewToolRegistry()
	RegisterListPublishedFilesTool(newRegistry, gw, cfg)
	newTool, ok := newRegistry.Get("list_my_published_files")
	if !ok {
		t.Fatal("new verified generation did not register list_my_published_files")
	}
	result, err = newTool.Run(context.Background(), `{}`)
	if err != nil {
		t.Fatalf("new generation Run: %v", err)
	}
	if result.IsError {
		t.Fatalf("new generation result = %#v", result)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("new generation dispatched %d requests, want 1", got)
	}
}

type retryingBlockingListUploader struct {
	attempts atomic.Int32
	entered  chan struct{}
	release  chan struct{}
}

func (f *retryingBlockingListUploader) List(context.Context, uploads.ListOptions) (*uploads.ListResponse, error) {
	f.attempts.Add(1)
	close(f.entered)
	<-f.release
	// Model an internal client retry before Run returns. The generation lease
	// must cover both attempts, not only argument validation or initial dispatch.
	f.attempts.Add(1)
	return &uploads.ListResponse{}, nil
}

func TestAuthSensitivePostOverlay_GenerationLeaseCoversWholeRunAndInternalRetry(t *testing.T) {
	gw := client.NewGatewayClient("http://example.test", "old-key")
	gw.BindIntegrationPrincipal("account-a", 1)
	fake := &retryingBlockingListUploader{
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	reg := agent.NewToolRegistry()
	registerAuthSensitiveTool(reg, gw, NewListPublishedFilesTool(fake))
	tool, ok := reg.Get("list_my_published_files")
	if !ok {
		t.Fatal("list_my_published_files was not registered")
	}

	runDone := make(chan agent.ToolResult, 1)
	go func() {
		result, err := tool.Run(context.Background(), `{}`)
		if err != nil {
			runDone <- agent.ToolResult{Content: err.Error(), IsError: true}
			return
		}
		runDone <- result
	}()
	<-fake.entered

	mutationStarted := make(chan struct{})
	mutationDone := make(chan struct{})
	go func() {
		close(mutationStarted)
		gw.SetAPIKey("new-key")
		close(mutationDone)
	}()
	<-mutationStarted
	select {
	case <-mutationDone:
		t.Fatal("credential mutation crossed an in-progress auth-sensitive Run lease")
	case <-time.After(25 * time.Millisecond):
	}

	close(fake.release)
	if result := <-runDone; result.IsError {
		t.Fatalf("leased Run result = %#v", result)
	}
	select {
	case <-mutationDone:
	case <-time.After(time.Second):
		t.Fatal("credential mutation remained blocked after auth-sensitive Run completed")
	}
	if got := fake.attempts.Load(); got != 2 {
		t.Fatalf("underlying attempts = %d, want 2", got)
	}

	result, err := tool.Run(context.Background(), `{}`)
	assertStaleAuthSensitiveResult(t, "list_my_published_files", result, err)
	if got := fake.attempts.Load(); got != 2 {
		t.Fatalf("stale Run reached underlying client; attempts = %d", got)
	}
}
