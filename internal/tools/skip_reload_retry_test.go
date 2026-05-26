package tools

import (
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/mcp"
)

func TestShouldSkipReloadRetry_NilMgr(t *testing.T) {
	if ShouldSkipReloadRetry(nil, "playwright", mcp.MCPServerConfig{}) {
		t.Error("nil mgr should not skip — caller will no-op anyway")
	}
}

func TestShouldSkipReloadRetry_KeepAliveTrue(t *testing.T) {
	mgr := mcp.NewClientManager()
	if ShouldSkipReloadRetry(mgr, "playwright", mcp.MCPServerConfig{KeepAlive: true}) {
		t.Error("KeepAlive=true playwright doesn't go through discover-then-disconnect, must not skip")
	}
}

func TestShouldSkipReloadRetry_NonPlaywright(t *testing.T) {
	mgr := mcp.NewClientManager()
	// Other servers don't currently use the discover-then-disconnect pattern
	// (see PostConnectDisconnectIfDiscoveryOnly), so they must always
	// participate in reload retries.
	if ShouldSkipReloadRetry(mgr, "intercom", mcp.MCPServerConfig{KeepAlive: false}) {
		t.Error("non-playwright servers must always be retried")
	}
	if ShouldSkipReloadRetry(mgr, "custom", mcp.MCPServerConfig{KeepAlive: false}) {
		t.Error("non-playwright servers must always be retried")
	}
}

func TestShouldSkipReloadRetry_PlaywrightFirstFailedConnect(t *testing.T) {
	// No tools cached yet → previous connect failed → genuine retry case.
	mgr := mcp.NewClientManager()
	if ShouldSkipReloadRetry(mgr, "playwright", mcp.MCPServerConfig{KeepAlive: false}) {
		t.Error("playwright with empty tool cache means first connect failed; must retry")
	}
}

func TestShouldSkipReloadRetry_PlaywrightAfterDiscovery(t *testing.T) {
	// Tools cached → previous connect succeeded → current Disconnect was
	// intentional via PostConnectDisconnectIfDiscoveryOnly. Skip retry so
	// reload doesn't relaunch Chrome.
	mgr := mcp.NewClientManager()
	mgr.SeedToolCache("playwright", []mcp.RemoteTool{{ServerName: "playwright"}})
	if !ShouldSkipReloadRetry(mgr, "playwright", mcp.MCPServerConfig{KeepAlive: false}) {
		t.Error("playwright with cached tools and KeepAlive=false must skip retry to avoid relaunching Chrome")
	}
}
