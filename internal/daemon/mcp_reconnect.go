package daemon

import (
	"context"
	"log"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/config"
	"github.com/Kocoro-lab/ShanClaw/internal/mcp"
)

// reconnectSink is the slice of mcp.ReconnectScheduler the async-connect
// result callback drives. Narrowed to an interface so the wiring can be
// tested without a real clock.
type reconnectSink interface {
	OnFailure(serverName string)
	OnSuccess(serverName string)
}

// MCPConnectHooks are the surface-specific hooks around the shared
// connect-result handling. The daemon HTTP server and the CLI startup path
// differ only in how they audit and what they do on success.
type MCPConnectHooks struct {
	// IsCurrent reports whether the manager this result belongs to is still
	// the live one. A nil IsCurrent means "always current".
	IsCurrent func() bool
	// AuditFail records a failed connect attempt. Optional.
	AuditFail func(serverName string, err error)
	// OnConnected runs after a successful connect (supervisor probe and the
	// discovery-only disconnect). Optional.
	OnConnected func(serverName string)
}

// buildMCPConnectResult returns the onResult callback passed to
// StartConnectAll. Beyond logging and auditing, it drives the reconnect
// scheduler: a failure schedules a bounded, backed-off retry instead of
// leaving the server permanently disconnected, and a success clears the
// streak.
//
// Results from a superseded manager are dropped before any hook runs —
// reconnecting on a manager a newer reload already replaced would leak a
// process group.
func buildMCPConnectResult(sink reconnectSink, cb MCPConnectHooks) func(serverName string, err error) {
	return func(serverName string, err error) {
		if cb.IsCurrent != nil && !cb.IsCurrent() {
			return // deps swapped by a newer reload — drop result.
		}
		if err != nil {
			log.Printf("[mcp] %s: async connect failed: %v", serverName, err)
			if cb.AuditFail != nil {
				cb.AuditFail(serverName, err)
			}
			if sink != nil {
				sink.OnFailure(serverName)
			}
			return
		}
		log.Printf("[mcp] %s: async connect succeeded; probing supervisor", serverName)
		if sink != nil {
			sink.OnSuccess(serverName)
		}
		if cb.OnConnected != nil {
			cb.OnConnected(serverName)
		}
	}
}

// MCPConnectTimeout resolves the per-attempt connect timeout the same way
// StartConnectAll's callers do, so a scheduled retry is bounded identically
// to the original attempt. Exported for the CLI startup path, which builds
// the same wiring before the HTTP server exists.
func MCPConnectTimeout(cfg *config.Config) time.Duration {
	// The 60s fallback mirrors StartConnectAll and CompleteRegistrationAsync;
	// keep the three in sync if it ever moves to a named constant.
	const fallback = 60 * time.Second
	if cfg == nil {
		return fallback
	}
	d := time.Duration(cfg.MCP.DefaultConnectTimeoutSecs) * time.Second
	if d <= 0 {
		return fallback
	}
	return d
}

// SwapMCPReconnectScheduler installs next as the live scheduler and stops the
// one it replaces. Retries are owned by the manager that spawned them, so a
// reload that rebuilds the manager must not leave the old ladder running.
func (d *ServerDeps) SwapMCPReconnectScheduler(next *mcp.ReconnectScheduler) {
	d.WriteLock()
	prev := d.MCPReconnect
	d.MCPReconnect = next
	d.WriteUnlock()
	if prev != nil && prev != next {
		prev.Stop()
	}
}

// liveMCPReconnectSink returns the current scheduler as a reconnectSink, or a
// genuinely nil interface when none is installed.
//
// Returning d.MCPReconnect directly would hand back a typed nil — an
// interface that is non-nil to `!= nil` but panics on first use. The explicit
// branch is what keeps buildMCPConnectResult's nil check honest.
func (d *ServerDeps) liveMCPReconnectSink() reconnectSink {
	d.mu.RLock()
	sched := d.MCPReconnect
	d.mu.RUnlock()
	if sched == nil {
		return nil
	}
	return sched
}

// ForgetMCPReconnect re-arms serverName's retry ladder. Called on explicit
// user action (POST /config/reload, the enable toggle) so a server that
// exhausted its automatic attempts gets a fresh streak.
func (d *ServerDeps) ForgetMCPReconnect(serverName string) {
	d.mu.RLock()
	sched := d.MCPReconnect
	d.mu.RUnlock()
	if sched != nil {
		sched.Forget(serverName)
	}
}

// NewMCPReconnectWiring builds the reconnect scheduler together with the
// connect-result callback it drives. The two are mutually recursive — a
// scheduled retry reports its own outcome through the same callback, so a
// second failure advances the backoff ladder — which is why they are
// constructed together rather than by the caller.
//
// The returned scheduler is owned by the caller: Stop it when the manager it
// belongs to is superseded or the daemon shuts down.
func NewMCPReconnectWiring(
	ctx context.Context,
	mgr *mcp.ClientManager,
	defaultTimeout time.Duration,
	cb MCPConnectHooks,
) (func(serverName string, err error), *mcp.ReconnectScheduler) {
	if mgr == nil {
		return buildMCPConnectResult(nil, cb), nil
	}
	var onResult func(serverName string, err error)
	scheduler := mcp.NewReconnectScheduler(func(serverName string) {
		if cb.IsCurrent != nil && !cb.IsCurrent() {
			return
		}
		cfg, ok := mgr.ConfigFor(serverName)
		if !ok || cfg.Disabled {
			// The server was removed or turned off while the retry was
			// pending; reconnecting it would resurrect a server the user
			// deliberately shut down.
			return
		}
		// StartConnectAll's own inFlight set collapses this with any attempt
		// already running for the same server.
		mgr.StartConnectAll(ctx, map[string]mcp.MCPServerConfig{serverName: cfg}, defaultTimeout, onResult)
	})
	onResult = buildMCPConnectResult(scheduler, cb)
	return onResult, scheduler
}
