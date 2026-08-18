package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/config"
	"github.com/Kocoro-lab/ShanClaw/internal/mcp"
	"github.com/Kocoro-lab/ShanClaw/internal/tools"
)

type postOverlayProbeTool struct{ name string }

func (t *postOverlayProbeTool) Info() agent.ToolInfo {
	return agent.ToolInfo{Name: t.name}
}

func (t *postOverlayProbeTool) RequiresApproval() bool { return false }

func (t *postOverlayProbeTool) Run(context.Context, string) (agent.ToolResult, error) {
	return agent.ToolResult{Content: t.name}, nil
}

func toolNamesContain(tools []agent.Tool, name string) bool {
	for _, tool := range tools {
		if tool != nil && tool.Info().Name == name {
			return true
		}
	}
	return false
}

func TestRebuildAuthSensitiveToolsRefreshesPostOverlaysAndPreservesCalendar(t *testing.T) {
	baseline := agent.NewToolRegistry()
	reg := baseline.Clone()
	for _, name := range authSensitivePostOverlayToolNames {
		reg.Register(&postOverlayProbeTool{name: name})
	}
	calendar := &postOverlayProbeTool{name: "calendar_list_events"}
	reg.Register(calendar)

	cfg := &config.Config{}
	cfg.Cloud.Enabled = true
	cfg.APIKey = "stale-config-key"
	gw := client.NewGatewayClient("http://example.test", "")
	deps := &ServerDeps{
		Config:       cfg,
		GW:           gw,
		Registry:     reg,
		BaselineReg:  baseline,
		PostOverlays: tools.ExtractPostOverlays(reg, baseline),
	}
	server := &Server{deps: deps, auth: &AuthManager{}}

	server.RebuildAuthSensitiveTools(context.Background())

	for _, name := range authSensitivePostOverlayToolNames {
		if _, ok := deps.Registry.Get(name); ok || toolNamesContain(deps.PostOverlays, name) {
			t.Fatalf("stale credential overlay %q survived sign-out rebuild", name)
		}
	}
	if _, ok := deps.Registry.Get("calendar_list_events"); !ok ||
		!toolNamesContain(deps.PostOverlays, "calendar_list_events") {
		t.Fatal("non-auth calendar overlay was dropped by auth rebuild")
	}

	rebuilt := tools.RebuildRegistryForHealth(
		baseline, deps.GatewayOverlay, deps.PostOverlays, nil, nil, nil,
	)
	for _, name := range authSensitivePostOverlayToolNames {
		if _, ok := rebuilt.Get(name); ok {
			t.Fatalf("health rebuild revived stale credential overlay %q", name)
		}
	}
	if _, ok := rebuilt.Get("calendar_list_events"); !ok {
		t.Fatal("health rebuild lost preserved calendar overlay")
	}
}

func TestRegistryHealthBuildAndAuthMutationShareOneTransaction(t *testing.T) {
	baseline := agent.NewToolRegistry()
	stalePublish := &postOverlayProbeTool{name: "publish_to_web"}
	calendar := &postOverlayProbeTool{name: "calendar_list_events"}
	reg := baseline.Clone()
	reg.Register(stalePublish)
	reg.Register(calendar)

	mgr := mcp.NewClientManager()
	supervisor := mcp.NewSupervisor(mgr)
	gw := client.NewGatewayClient("http://example.test", "")
	deps := &ServerDeps{
		Config:       &config.Config{},
		GW:           gw,
		Registry:     reg,
		BaselineReg:  baseline,
		PostOverlays: []agent.Tool{stalePublish, calendar},
		MCPManager:   mgr,
		Supervisor:   supervisor,
	}
	server := &Server{deps: deps, auth: &AuthManager{}}

	originalRebuild := rebuildRegistryForHealthFn
	buildEntered := make(chan struct{})
	releaseBuild := make(chan struct{})
	rebuildRegistryForHealthFn = func(
		baseline *agent.ToolRegistry,
		gatewayOverlay []agent.Tool,
		postOverlays []agent.Tool,
		healthStates map[string]mcp.ServerHealth,
		mcpMgr *mcp.ClientManager,
		sup *mcp.Supervisor,
	) *agent.ToolRegistry {
		close(buildEntered)
		<-releaseBuild
		return originalRebuild(baseline, gatewayOverlay, postOverlays, healthStates, mcpMgr, sup)
	}
	t.Cleanup(func() { rebuildRegistryForHealthFn = originalRebuild })

	healthDone := make(chan bool, 1)
	go func() {
		_, swapped := deps.RebuildRegistryForMCPHealth(supervisor)
		healthDone <- swapped
	}()
	<-buildEntered

	authStarted := make(chan struct{})
	authDone := make(chan struct{})
	go func() {
		close(authStarted)
		server.RebuildAuthSensitiveTools(context.Background())
		close(authDone)
	}()
	<-authStarted
	select {
	case <-authDone:
		t.Fatal("auth mutation crossed an in-progress MCP registry build-to-swap transaction")
	case <-time.After(25 * time.Millisecond):
	}

	close(releaseBuild)
	if swapped := <-healthDone; !swapped {
		t.Fatal("current supervisor health registry did not swap")
	}
	select {
	case <-authDone:
	case <-time.After(time.Second):
		t.Fatal("auth mutation remained blocked after health registry swap")
	}
	if _, ok := deps.Registry.Get("publish_to_web"); ok ||
		toolNamesContain(deps.PostOverlays, "publish_to_web") {
		t.Fatal("old health build revived stale publish_to_web after auth mutation")
	}
	if _, ok := deps.Registry.Get("calendar_list_events"); !ok ||
		!toolNamesContain(deps.PostOverlays, "calendar_list_events") {
		t.Fatal("serialized auth mutation lost the calendar overlay")
	}
}

func TestQueuedSupersededHealthRebuildCannotSwapAfterReload(t *testing.T) {
	baseline := agent.NewToolRegistry()
	oldRegistry := baseline.Clone()
	oldRegistry.Register(&postOverlayProbeTool{name: "old_catalog"})
	newRegistry := baseline.Clone()
	newRegistry.Register(&postOverlayProbeTool{name: "new_catalog"})
	oldSupervisor := mcp.NewSupervisor(mcp.NewClientManager())
	newSupervisor := mcp.NewSupervisor(mcp.NewClientManager())
	deps := &ServerDeps{
		Registry:    oldRegistry,
		BaselineReg: baseline,
		Supervisor:  oldSupervisor,
	}

	unlockReload := deps.LockToolRegistryMutation()
	callbackStarted := make(chan struct{})
	callbackDone := make(chan bool, 1)
	go func() {
		close(callbackStarted)
		_, swapped := deps.RebuildRegistryForMCPHealth(oldSupervisor)
		callbackDone <- swapped
	}()
	<-callbackStarted
	deps.mu.Lock()
	deps.Supervisor = newSupervisor
	deps.Registry = newRegistry
	deps.mu.Unlock()
	unlockReload()

	if swapped := <-callbackDone; swapped {
		t.Fatal("queued callback from superseded supervisor swapped after reload")
	}
	if _, ok := deps.Registry.Get("new_catalog"); !ok {
		t.Fatal("superseded health callback replaced the reload registry")
	}
	if _, ok := deps.Registry.Get("old_catalog"); ok {
		t.Fatal("superseded health callback revived the old catalog")
	}
}

func TestPreserveNonAuthPostOverlaysForReload(t *testing.T) {
	reg := agent.NewToolRegistry()
	preserveNonAuthPostOverlays(reg, []agent.Tool{
		&postOverlayProbeTool{name: "publish_to_web"},
		&postOverlayProbeTool{name: "calendar_list_events"},
	})
	if _, ok := reg.Get("publish_to_web"); ok {
		t.Fatal("reload preserved a credential-capturing post overlay")
	}
	if _, ok := reg.Get("calendar_list_events"); !ok {
		t.Fatal("reload dropped a non-auth post overlay")
	}
}
