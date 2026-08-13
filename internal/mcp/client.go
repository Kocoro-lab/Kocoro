package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
)

// MCPServerConfig describes how to connect to an MCP server.
type MCPServerConfig struct {
	Command   string            `yaml:"command"              mapstructure:"command"   json:"command"`
	Args      []string          `yaml:"args,omitempty"       mapstructure:"args"      json:"args,omitempty"`
	Env       map[string]string `yaml:"env,omitempty"        mapstructure:"env"       json:"env,omitempty"`
	Type      string            `yaml:"type,omitempty"       mapstructure:"type"      json:"type,omitempty"`        // "stdio" (default) or "http"
	URL       string            `yaml:"url,omitempty"        mapstructure:"url"       json:"url,omitempty"`         // for http type
	Disabled  bool              `yaml:"disabled,omitempty"   mapstructure:"disabled"  json:"disabled,omitempty"`    // skip this server
	Context   string            `yaml:"context,omitempty"    mapstructure:"context"   json:"context,omitempty"`     // LLM context injected into system prompt
	KeepAlive bool              `yaml:"keep_alive,omitempty" mapstructure:"keep_alive" json:"keep_alive,omitempty"` // stay connected between turns (skip on-demand teardown)
	// ConnectTimeoutSeconds overrides the per-server connect timeout used by
	// StartConnectAll. Zero falls back to MCPConfig.DefaultConnectTimeoutSecs
	// (configured under `mcp.default_connect_timeout_secs`, default 60s).
	// OAuth-bridged servers like Intercom set this to ~300s in the built-in
	// catalog so the user has time to complete the browser flow before the
	// daemon kills the npx subprocess.
	ConnectTimeoutSeconds int `yaml:"connect_timeout_secs,omitempty" mapstructure:"connect_timeout_secs" json:"connect_timeout_secs,omitempty"`
	// ToolTimeoutSeconds bounds a single tools/call attempt to this server.
	// Zero falls back to the manager default (SetToolCallTimeout, wired from
	// `mcp.tool_timeout_secs`, default 300s). Raise it per-server for MCP
	// tools that legitimately run long (large exports, batch jobs).
	ToolTimeoutSeconds int `yaml:"tool_timeout_secs,omitempty" mapstructure:"tool_timeout_secs" json:"tool_timeout_secs,omitempty"`
	// WorkspaceBase declares the directory this server's tool results render
	// relative paths against (for file-producing MCP servers). When set, the
	// daemon translates relative artifact links in results to absolute paths
	// under this base — but only for files that actually exist there. Servers
	// without it (and outside the built-in table) stay opaque: we never
	// rewrite paths whose semantics we don't know.
	WorkspaceBase string `yaml:"workspace_base,omitempty" mapstructure:"workspace_base" json:"workspace_base,omitempty"`
	// Builtin marks an entry that originated from BuiltinMCPServers. Set by
	// config.Load after merging the in-binary catalog onto user yaml; never
	// persisted (yaml:"-" + mapstructure:"-"). The daemon API surfaces it
	// via GET /config/status so Desktop can distinguish pre-bundled servers
	// from user-added ones.
	Builtin bool `yaml:"-" mapstructure:"-" json:"builtin,omitempty"`
}

// DefaultToolCallTimeout bounds a single tools/call attempt when neither the
// server config (ToolTimeoutSeconds) nor the manager (SetToolCallTimeout,
// wired from `mcp.tool_timeout_secs`) overrides it.
//
// Workload: interactive tool calls from the agent loop — search, browse,
// file ops — that normally finish in seconds. Symptom when it binds: a
// legitimately long MCP tool (large export, batch job) errors at 5 minutes;
// raise `mcp.tool_timeout_secs` globally or the server's `tool_timeout_secs`.
// Why it exists: a wedged-alive MCP subprocess accepts the request write and
// never replies — without this bound the call blocks the turn indefinitely
// (2026-07-29: google-workspace sat 6.5 min on a dead pipe). 300s matches
// the Codex (DEFAULT_TOOL_TIMEOUT) and Hermes MCP defaults.
const (
	DefaultToolCallTimeout          = 300 * time.Second
	mcpClientGracefulCloseTimeout   = 2 * time.Second
	mcpClientForcedCloseWaitTimeout = 4 * time.Second
)

// RemoteTool represents a tool discovered from an MCP server.
type RemoteTool struct {
	ServerName string
	Tool       mcp.Tool
}

// ClientManager manages connections to multiple MCP servers.
type ClientManager struct {
	mu           sync.Mutex
	clients      map[string]mcpclient.MCPClient // server name → client
	configs      map[string]MCPServerConfig     // server name → config (for reconnect)
	toolCache    map[string][]RemoteTool        // server name → last-known tools
	cancellers   map[string]context.CancelFunc  // server name → ctx.Cancel for the spawned subprocess (stdio only); cancelling it SIGTERMs the whole process group
	inFlight     map[string]struct{}            // server name → connect goroutine in progress; StartConnectAll/Reconnect skip duplicates
	reconnectMu  map[string]*sync.Mutex         // per-server reconnect serialization
	supervised   bool                           // when true, skip inline reconnect in CallTool
	idleTimers   map[string]*time.Timer         // per-server idle disconnect timers
	needsSetup   map[string]bool                // servers gated by missing readiness marker
	rootsHandler *RootsHandler                  // advertised to servers honoring the MCP roots capability; nil disables advertisement
	// toolCallTimeout bounds a single tools/call attempt when the server
	// config has no ToolTimeoutSeconds override. Zero = DefaultToolCallTimeout.
	// Wired from `mcp.tool_timeout_secs` via SetToolCallTimeout.
	toolCallTimeout time.Duration
}

// NewClientManager creates a new MCP client manager.
func NewClientManager() *ClientManager {
	return &ClientManager{
		clients:     make(map[string]mcpclient.MCPClient),
		configs:     make(map[string]MCPServerConfig),
		toolCache:   make(map[string][]RemoteTool),
		cancellers:  make(map[string]context.CancelFunc),
		inFlight:    make(map[string]struct{}),
		reconnectMu: make(map[string]*sync.Mutex),
		needsSetup:  make(map[string]bool),
	}
}

// tryReserveInFlight marks serverName as having an in-flight connect attempt.
// Returns (release, true) on success; (nil, false) if another connect goroutine
// is already mid-flight for this server — caller should skip and let the
// existing attempt finish. The release func must be called when the attempt
// terminates (success OR failure) to clear the slot.
//
// Without this guard, a /config/reload that fires while the daemon-startup
// async connect is still inside Initialize/ListTools would spawn a second
// subprocess for the same server; both race to bind the OAuth callback
// loopback port and the loser crashes with EADDRINUSE.
func (m *ClientManager) tryReserveInFlight(serverName string) (func(), bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, busy := m.inFlight[serverName]; busy {
		return nil, false
	}
	m.inFlight[serverName] = struct{}{}
	return func() {
		m.mu.Lock()
		delete(m.inFlight, serverName)
		m.mu.Unlock()
	}, true
}

// SetRootsHandler installs a roots handler that will be advertised to every
// MCP server the manager connects (or reconnects) to. Must be called before
// ConnectAll / any reconnect path; existing live clients are not retrofitted
// because mcp-go does not expose runtime capability updates on the client
// side. Pass nil to disable advertisement.
func (m *ClientManager) SetRootsHandler(h *RootsHandler) {
	m.mu.Lock()
	m.rootsHandler = h
	m.mu.Unlock()
}

// FirstAdvertisedRoot returns the first advertised workspace root that
// currently exists — the directory a roots-honoring server (playwright-mcp)
// renders result paths relative to. Empty when no handler is installed or
// no root exists.
func (m *ClientManager) FirstAdvertisedRoot() string {
	m.mu.Lock()
	h := m.rootsHandler
	m.mu.Unlock()
	if h == nil {
		return ""
	}
	return h.FirstExistingRoot()
}

// ConnectAll connects to all configured MCP servers in parallel and returns discovered tools.
func (m *ClientManager) ConnectAll(ctx context.Context, servers map[string]MCPServerConfig) ([]RemoteTool, error) {
	type result struct {
		tools []RemoteTool
		err   error
		name  string
	}

	var wg sync.WaitGroup
	results := make(chan result, len(servers))

	for name, cfg := range servers {
		if cfg.Disabled {
			continue
		}
		wg.Add(1)
		go func(name string, cfg MCPServerConfig) {
			defer wg.Done()
			serverCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
			defer cancel()
			tools, err := m.connect(serverCtx, name, cfg)
			results <- result{tools: tools, err: err, name: name}
		}(name, cfg)
	}

	go func() {
		wg.Wait()
		close(results)
	}()

	var allTools []RemoteTool
	var errs []string
	for r := range results {
		if r.err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", r.name, r.err))
			continue
		}
		allTools = append(allTools, r.tools...)
	}

	if len(errs) > 0 {
		combined := fmt.Errorf("%s", strings.Join(errs, "; "))
		if len(allTools) == 0 {
			return nil, combined
		}
		return allTools, combined
	}

	return allTools, nil
}

// RegisterConfigs stores server configs without attempting to connect. Use
// before calling Supervisor.Start so the supervisor discovers every entry,
// then call StartConnectAll to kick off the actual connection goroutines.
// Existing entries with the same key are overwritten.
func (m *ClientManager) RegisterConfigs(servers map[string]MCPServerConfig) {
	if len(servers) == 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for name, cfg := range servers {
		m.configs[name] = cfg
	}
}

// StartConnectAll launches per-server connection goroutines and returns
// immediately — the daemon HTTP path is no longer blocked by slow MCP
// handshakes (for example, a subprocess plus an interactive OAuth chain).
//
// Per-server timeout resolves in this order:
//  1. cfg.ConnectTimeoutSeconds (if > 0)
//  2. defaultTimeout (from cfg.MCP.DefaultConnectTimeoutSecs)
//  3. 60 second hardcoded floor
//
// onResult fires once per non-disabled server with (name, err). On success
// (err == nil) the daemon typically calls Supervisor.ProbeNow(name) to flip
// health state and trigger a registry rebuild; on failure it should write
// an audit entry — the supervisor's periodic probes deliberately do NOT
// reconnect, so a failed first attempt stays Disconnected until the user
// re-toggles.
//
// parentCtx cancellation cancels in-flight Initialize/ListTools calls. The
// per-server timeout deadline does too — and when it fires we force-close
// the client (in a side goroutine because mcp-go's Close blocks until the
// inner reads unwind) which SIGTERMs the stdio subprocess and unblocks the
// Initialize read. Net effect: a hung server (e.g. mcp-remote waiting for
// OAuth the user abandoned) is reaped at timeout rather than leaking until
// daemon shutdown.
func (m *ClientManager) StartConnectAll(parentCtx context.Context, servers map[string]MCPServerConfig, defaultTimeout time.Duration, onResult func(serverName string, err error)) {
	// Pre-register every config under one lock acquisition so the supervisor
	// (if already started) sees a consistent picture before any goroutine
	// races ahead.
	m.mu.Lock()
	for name, cfg := range servers {
		m.configs[name] = cfg
	}
	m.mu.Unlock()

	for name, cfg := range servers {
		if cfg.Disabled {
			continue
		}
		timeout := defaultTimeout
		if cfg.ConnectTimeoutSeconds > 0 {
			timeout = time.Duration(cfg.ConnectTimeoutSeconds) * time.Second
		}
		if timeout <= 0 {
			timeout = 60 * time.Second
		}
		go func(name string, cfg MCPServerConfig, timeout time.Duration) {
			// Skip if a connect goroutine is already in flight for this
			// server. The pending attempt will eventually call onResult on
			// its own; firing a duplicate would either lose the EADDRINUSE
			// race or overwrite m.clients/m.cancellers and leak a process
			// group.
			release, ok := m.tryReserveInFlight(name)
			if !ok {
				log.Printf("[mcp] %s: connect already in flight, skipping duplicate StartConnectAll attempt", name)
				// Report the skip instead of returning silently. The pending
				// attempt usually calls onResult itself, but NOT when the slot
				// is held by Supervisor.attemptReconnect → ClientManager
				// .Reconnect, which has no onResult. A caller that re-arms on
				// its own result (the reconnect scheduler) would otherwise
				// stall here with nothing left to wake it.
				if onResult != nil {
					onResult(name, ErrConnectInFlight)
				}
				return
			}
			defer release()

			ctx, cancel := context.WithTimeout(parentCtx, timeout)
			defer cancel()
			// connectWithForceClose actively closes the client when ctx
			// expires. The bare connect() path can leak on mcp-go's stdio
			// transport (Initialize blocks on a stdin read that ignores
			// ctx) — important for OAuth bridges like mcp-remote where the
			// user may walk away without finishing the browser flow.
			_, err := m.connectWithForceClose(ctx, name, cfg)
			if onResult != nil {
				onResult(name, err)
			}
		}(name, cfg, timeout)
	}
}

// ConnectedServers returns the names of all servers that have an active client connection.
// IsConnected returns true if the named server has an active client connection.
func (m *ClientManager) IsConnected(serverName string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.clients[serverName]
	return ok
}

func (m *ClientManager) ConnectedServers() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	names := make([]string, 0, len(m.clients))
	for name := range m.clients {
		names = append(names, name)
	}
	return names
}

// connectWithForceClose is like connect() but reliably kills the subprocess
// when ctx expires. Two issues with the naive path:
//   - mcp-go's stdio transport does NOT honor ctx during Initialize/ListTools
//     (raw pipe read).
//   - mcp-go's Stdio.Close() calls cmd.Wait() — it blocks until the subprocess
//     exits on its own. A well-behaved server exits on stdin EOF; abandoned
//     mcp-remote OAuth flows do not.
//
// Fix: spawn the subprocess under a cancellable cmdCtx (exec.CommandContext
// SIGKILLs on ctx cancel inside mcp-go's spawnCommand), run Initialize in
// an inner goroutine, and on ctx expiry cancel cmdCtx so the subprocess
// dies promptly. Close still runs in a side goroutine because cmd.Wait
// can race the SIGKILL by a few ms on busy systems.
func (m *ClientManager) connectWithForceClose(ctx context.Context, name string, cfg MCPServerConfig) ([]RemoteTool, error) {
	m.mu.Lock()
	m.configs[name] = cfg
	rootsHandler := m.rootsHandler
	m.mu.Unlock()

	clientOpts := []mcpclient.ClientOption{}
	if opt := rootsHandler.clientOption(); opt != nil {
		clientOpts = append(clientOpts, opt)
	}

	var c *mcpclient.Client
	var cmdCancel context.CancelFunc // nil for http; set for stdio so timeout can SIGKILL
	switch cfg.Type {
	case "http":
		if cfg.URL == "" {
			return nil, fmt.Errorf("http MCP server requires url")
		}
		httpTransport, err := transport.NewStreamableHTTP(cfg.URL)
		if err != nil {
			return nil, fmt.Errorf("failed to create HTTP client: %w", err)
		}
		c = mcpclient.NewClient(httpTransport, clientOpts...)
		if err := c.Start(ctx); err != nil {
			return nil, fmt.Errorf("failed to start HTTP client: %w", err)
		}
	default:
		if cfg.Command == "" {
			return nil, fmt.Errorf("stdio MCP server requires command")
		}
		envSlice := buildEnvSlice(cfg.Env)
		// withProcessGroup puts the subprocess in its own process group so
		// cancelling cmdCtx SIGTERMs the entire chain (npx → npm exec →
		// node mcp-remote), not just the direct child. Without this an
		// abandoned-OAuth mcp-remote keeps holding its loopback callback
		// port, and subsequent toggle-on attempts crash with EADDRINUSE.
		stdioTransport := transport.NewStdioWithOptions(cfg.Command, envSlice, cfg.Args, withProcessGroup())
		// Subprocess is bound to cmdCtx via exec.CommandContext; cancel it
		// on timeout to force a SIGKILL. On success path we stash cancel
		// in m.cancellers so Disconnect / Close can reap the group later.
		cmdCtx, cancel := context.WithCancel(context.Background())
		cmdCancel = cancel
		if err := stdioTransport.Start(cmdCtx); err != nil {
			cancel()
			return nil, fmt.Errorf("failed to start MCP server %q: %w", cfg.Command, err)
		}
		c = mcpclient.NewClient(stdioTransport, clientOpts...)
		if err := c.Start(ctx); err != nil {
			cancel()
			_ = c.Close()
			return nil, fmt.Errorf("failed to wire MCP client %q: %w", name, err)
		}
	}

	type initResult struct {
		tools []RemoteTool
		err   error
	}
	resultCh := make(chan initResult, 1)
	go func() {
		_, err := c.Initialize(ctx, mcp.InitializeRequest{
			Params: struct {
				ProtocolVersion string                 `json:"protocolVersion"`
				Capabilities    mcp.ClientCapabilities `json:"capabilities"`
				ClientInfo      mcp.Implementation     `json:"clientInfo"`
			}{
				ProtocolVersion: mcp.LATEST_PROTOCOL_VERSION,
				ClientInfo:      mcp.Implementation{Name: "shannon-cli", Version: "1.0.0"},
			},
		})
		if err != nil {
			resultCh <- initResult{nil, fmt.Errorf("initialize failed: %w", err)}
			return
		}
		toolsResult, err := c.ListTools(ctx, mcp.ListToolsRequest{})
		if err != nil {
			resultCh <- initResult{nil, fmt.Errorf("tools/list failed: %w", err)}
			return
		}
		var tools []RemoteTool
		for _, t := range toolsResult.Tools {
			tools = append(tools, RemoteTool{
				ServerName: name,
				Tool:       t,
			})
		}
		resultCh <- initResult{tools, nil}
	}()

	select {
	case res := <-resultCh:
		if res.err != nil {
			if cmdCancel != nil {
				cmdCancel()
			}
			_ = c.Close()
			return nil, res.err
		}
		// Success: stash cmdCancel in m.cancellers so Disconnect / Close
		// can SIGTERM the whole process group later. mcp-go's Stdio.Close
		// calls cmd.Wait() which blocks until the subprocess exits — for
		// OAuth bridges (mcp-remote listening on a loopback callback port)
		// stdin EOF is not enough to make them exit, so we MUST cancel
		// cmdCtx before c.Close() can return.
		m.mu.Lock()
		m.clients[name] = c
		m.toolCache[name] = res.tools
		if cmdCancel != nil {
			m.cancellers[name] = cmdCancel
		}
		m.mu.Unlock()
		return res.tools, nil
	case <-ctx.Done():
		log.Printf("[mcp] %s: ctx expired, force-killing subprocess + closing client", name)
		// SIGKILL via cmdCancel → subprocess dies → stdout EOF → readResponses
		// closes done → in-flight Initialize/ListTools return → goroutine exits
		// and writes to the buffered resultCh (cap=1, no receiver needed). Close
		// runs in a side goroutine so we never block the outer return on
		// cmd.Wait, even if SIGKILL takes a moment to propagate.
		if cmdCancel != nil {
			cmdCancel()
		}
		go func() { _ = c.Close() }()
		return nil, fmt.Errorf("connect timed out for %q: %w", name, ctx.Err())
	}
}

func (m *ClientManager) connect(ctx context.Context, name string, cfg MCPServerConfig) ([]RemoteTool, error) {
	m.mu.Lock()
	m.configs[name] = cfg
	rootsHandler := m.rootsHandler
	m.mu.Unlock()

	// Every connect path needs to: build a transport, attach optional
	// client-side handlers (currently just roots), then Start. The
	// convenience constructors in mcp-go (NewStdioMCPClient,
	// NewStreamableHttpClient) do not accept ClientOption, so we build
	// the transport and wire the client directly when a handler exists.
	clientOpts := []mcpclient.ClientOption{}
	if opt := rootsHandler.clientOption(); opt != nil {
		clientOpts = append(clientOpts, opt)
	}

	var c *mcpclient.Client
	var cmdCancel context.CancelFunc // nil for http; set for stdio so Disconnect can SIGTERM the group
	switch cfg.Type {
	case "http":
		if cfg.URL == "" {
			return nil, fmt.Errorf("http MCP server requires url")
		}
		httpTransport, err := transport.NewStreamableHTTP(cfg.URL)
		if err != nil {
			return nil, fmt.Errorf("failed to create HTTP client: %w", err)
		}
		c = mcpclient.NewClient(httpTransport, clientOpts...)
		// Client.Start wires up the bidirectional request handler so
		// server-initiated calls (e.g. roots/list from playwright-mcp) reach
		// our RootsHandler. Skipping this step leaves the capability
		// advertised but functionally dead — the server sends roots/list,
		// the transport has no handler, and requests silently drop.
		if err := c.Start(ctx); err != nil {
			return nil, fmt.Errorf("failed to start HTTP client: %w", err)
		}
	default: // stdio
		if cfg.Command == "" {
			return nil, fmt.Errorf("stdio MCP server requires command")
		}
		envSlice := buildEnvSlice(cfg.Env)
		// withProcessGroup ensures cancelling cmdCtx kills the entire
		// process chain (npx → npm exec → node mcp-remote), not just the
		// direct child. Without it, abandoned-OAuth subprocesses keep
		// holding their loopback callback port and the next reconnect
		// crashes with EADDRINUSE.
		stdioTransport := transport.NewStdioWithOptions(cfg.Command, envSlice, cfg.Args, withProcessGroup())
		// Spawn under a cancellable context derived from Background so the
		// MCP server survives the ctx given to connect() (which may carry
		// a short timeout) — Disconnect / Close are the deliberate ways to
		// reap the process group via the stashed cancel fn.
		cmdCtx, cancel := context.WithCancel(context.Background())
		cmdCancel = cancel
		if err := stdioTransport.Start(cmdCtx); err != nil {
			cancel()
			return nil, fmt.Errorf("failed to start MCP server %q: %w", cfg.Command, err)
		}
		c = mcpclient.NewClient(stdioTransport, clientOpts...)
		// Client.Start is idempotent on the transport (stdio guards on its
		// `started` flag) but unconditionally wires SetRequestHandler on the
		// bidirectional transport. Without this call, server-initiated
		// requests like roots/list never reach our handler.
		if err := c.Start(ctx); err != nil {
			cancel()
			_ = c.Close()
			return nil, fmt.Errorf("failed to wire MCP client %q: %w", name, err)
		}
	}

	// Initialize handshake
	_, err := c.Initialize(ctx, mcp.InitializeRequest{
		Params: struct {
			ProtocolVersion string                 `json:"protocolVersion"`
			Capabilities    mcp.ClientCapabilities `json:"capabilities"`
			ClientInfo      mcp.Implementation     `json:"clientInfo"`
		}{
			ProtocolVersion: mcp.LATEST_PROTOCOL_VERSION,
			ClientInfo:      mcp.Implementation{Name: "shannon-cli", Version: "1.0.0"},
		},
	})
	if err != nil {
		if cmdCancel != nil {
			cmdCancel()
		}
		_ = c.Close()
		return nil, fmt.Errorf("initialize failed: %w", err)
	}

	// List available tools
	toolsResult, err := c.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		if cmdCancel != nil {
			cmdCancel()
		}
		_ = c.Close()
		return nil, fmt.Errorf("tools/list failed: %w", err)
	}

	m.mu.Lock()
	m.clients[name] = c
	if cmdCancel != nil {
		m.cancellers[name] = cmdCancel
	}
	m.mu.Unlock()

	var tools []RemoteTool
	for _, t := range toolsResult.Tools {
		tools = append(tools, RemoteTool{
			ServerName: name,
			Tool:       t,
		})
	}

	m.mu.Lock()
	m.toolCache[name] = tools
	m.mu.Unlock()

	return tools, nil
}

// CallTool invokes a tool on the specified MCP server.
// If the call fails with a connection error, it attempts to reconnect once and retry.
func (m *ClientManager) CallTool(ctx context.Context, serverName, toolName string, args map[string]any) (string, bool, error) {
	m.mu.Lock()
	c, ok := m.clients[serverName]
	cfg, hasCfg := m.configs[serverName]
	m.mu.Unlock()

	// Lazy-start: server was discovered at boot but disconnected (keepAlive=false).
	// Reconnect on first tool invocation, serialized per-server to avoid duplicate processes.
	// Never lazy-start a disabled server — a stale registry can still hold its
	// tools right after a disable, and dispatching must not relaunch it.
	if !ok && hasCfg && cfg.Disabled {
		return "", true, fmt.Errorf("MCP server %q is disabled", serverName)
	}
	if !ok && hasCfg {
		m.mu.Lock()
		rmu, rmOK := m.reconnectMu[serverName]
		if !rmOK {
			rmu = &sync.Mutex{}
			m.reconnectMu[serverName] = rmu
		}
		m.mu.Unlock()

		rmu.Lock()
		// Double-check: another goroutine may have connected while we waited.
		m.mu.Lock()
		c, ok = m.clients[serverName]
		m.mu.Unlock()
		if !ok {
			log.Printf("[mcp] %s: not connected, attempting on-demand connect", serverName)
			reconnectCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			if _, err := m.connect(reconnectCtx, serverName, cfg); err != nil {
				cancel()
				rmu.Unlock()
				return "", true, fmt.Errorf("MCP server %q on-demand connect failed: %w", serverName, err)
			}
			cancel()
			m.mu.Lock()
			c = m.clients[serverName]
			m.mu.Unlock()
		}
		rmu.Unlock()
	} else if !ok {
		return "", true, fmt.Errorf("MCP server %q not connected", serverName)
	}

	// Per-attempt timeout: a wedged-alive MCP subprocess accepts the request
	// write and never replies — without a bound the call blocks the turn
	// until the process happens to die (2026-07-29: google-workspace held a
	// tool call for 6.5 minutes). Each attempt (initial + post-reconnect
	// retry) gets its own budget; an earlier caller deadline still wins.
	callTimeout := m.resolveToolCallTimeout(cfg, hasCfg)
	callToolOnce := func(callCtx context.Context) (*mcp.CallToolResult, error) {
		attemptCtx, cancel := context.WithTimeout(callCtx, callTimeout)
		defer cancel()
		res, callErr := c.CallTool(attemptCtx, mcp.CallToolRequest{
			Params: mcp.CallToolParams{
				Name:      toolName,
				Arguments: args,
			},
		})
		if callErr != nil && attemptCtx.Err() == context.DeadlineExceeded && callCtx.Err() == nil {
			callErr = fmt.Errorf("no response within per-call timeout %s (override: mcp server %q tool_timeout_secs, or mcp.tool_timeout_secs): %w", callTimeout, serverName, callErr)
		}
		return res, callErr
	}

	result, err := callToolOnce(ctx)
	if err != nil && IsTransportError(err) {
		m.mu.Lock()
		skip := m.supervised
		m.mu.Unlock()
		if skip {
			return "", true, fmt.Errorf("tools/call failed (supervised, no inline reconnect): %w", err)
		}
		// Transport failure (process died, broken pipe, EOF) AFTER the
		// request was written to a server that was alive at dispatch time.
		// This does NOT prove the server never acted: it may have executed
		// the tool's side effect and died before writing the response, and
		// the wire cannot distinguish that from died-before-acting. So the
		// two halves of recovery are separated:
		//   - reconnect always runs (repairing the transport benefits the
		//     NEXT call regardless);
		//   - re-dispatch runs ONLY for tools whose own annotations say a
		//     duplicate execution is harmless (readOnlyHint/idempotentHint).
		// Everything else returns OutcomeUnknownError so the caller can
		// verify the effect instead of silently double-executing a write.
		replaySafe := m.replaySafeFromCache(serverName, toolName)
		origErr := err
		m.mu.Lock()
		cfg, hasCfg := m.configs[serverName]
		stale := m.clients[serverName]
		staleCancel := m.cancellers[serverName]
		delete(m.cancellers, serverName)
		m.mu.Unlock()

		// The Disabled check is defensive: today a disabled server has no
		// client so a post-dispatch failure can't reach here, but "no path
		// relaunches a disabled server" should hold by construction, not by
		// reachability argument.
		if hasCfg && !cfg.Disabled {
			// Reap the old process group + close the stale client. Skipping
			// staleCancel here would leave an orphan when the client died
			// from something other than transport EOF (e.g. user toggled
			// reload concurrently). Reconnect uses a fresh background context
			// so a cancelled request context doesn't prevent recovery.
			if staleCancel != nil {
				staleCancel()
			}
			if stale != nil {
				_ = stale.Close()
			}
			reconnectCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			if _, reconnErr := m.connect(reconnectCtx, serverName, cfg); reconnErr != nil {
				// Repair-always is the invariant; a failed repair must stay
				// attributable even when the OutcomeUnknownError return below
				// doesn't carry it.
				log.Printf("[mcp] %s: post-dispatch reconnect failed: %v", serverName, reconnErr)
			} else if replaySafe {
				m.mu.Lock()
				c = m.clients[serverName]
				m.mu.Unlock()
				// callToolOnce reads the reassigned c and applies a fresh
				// per-attempt timeout to the retry.
				result, err = callToolOnce(ctx)
			}
		}
		if err != nil {
			if !replaySafe {
				return "", true, &OutcomeUnknownError{Server: serverName, Tool: toolName, Err: origErr}
			}
			// Preserve the original transport error for diagnostics.
			return "", true, fmt.Errorf("tools/call failed: %w (reconnect attempted after: %v)", origErr, err)
		}
	} else if err != nil {
		return "", true, fmt.Errorf("tools/call failed: %w", err)
	}

	// Extract text content from result
	var texts []string
	for _, block := range result.Content {
		if textContent, ok := block.(mcp.TextContent); ok {
			texts = append(texts, textContent.Text)
		} else {
			// For non-text content, marshal to JSON
			b, _ := json.Marshal(block)
			texts = append(texts, string(b))
		}
	}

	content := ""
	if len(texts) > 0 {
		content = texts[0]
		for _, t := range texts[1:] {
			content += "\n" + t
		}
	}

	return content, result.IsError, nil
}

// Close shuts down all connected MCP servers in parallel.
//
// Stdio servers first get a short graceful-close window so servers that own
// detached resources, such as a browser process, can release them on stdin
// EOF. If Close does not return, cancelling the per-server cmdCtx sends
// SIGTERM to the entire process group (npx → npm → node mcp-remote).
func (m *ClientManager) Close() {
	m.mu.Lock()
	clients := make(map[string]mcpclient.MCPClient, len(m.clients))
	cancellers := make(map[string]context.CancelFunc, len(m.cancellers))
	for name, c := range m.clients {
		clients[name] = c
		delete(m.clients, name)
	}
	for name, cancel := range m.cancellers {
		cancellers[name] = cancel
		delete(m.cancellers, name)
	}
	m.mu.Unlock()

	var wg sync.WaitGroup
	for name, c := range clients {
		cancel := cancellers[name]
		wg.Add(1)
		go func(name string, c mcpclient.MCPClient, cancel context.CancelFunc) {
			defer wg.Done()
			closeManagedMCPClient(name, c, cancel, mcpClientGracefulCloseTimeout)
		}(name, c, cancel)
	}
	for name, cancel := range cancellers {
		if _, ok := clients[name]; !ok && cancel != nil {
			cancel()
		}
	}
	wg.Wait()
}

func closeManagedMCPClient(name string, c mcpclient.MCPClient, cancel context.CancelFunc, gracefulTimeout time.Duration) {
	if c == nil {
		if cancel != nil {
			cancel()
		}
		return
	}
	done := make(chan struct{})
	go func() {
		_ = c.Close()
		close(done)
	}()

	timer := time.NewTimer(gracefulTimeout)
	defer timer.Stop()
	select {
	case <-done:
		if cancel != nil {
			cancel()
		}
		return
	case <-timer.C:
		if cancel == nil {
			log.Printf("[mcp] %s: graceful close timed out with no subprocess canceller", name)
			return
		}
		cancel()
	}

	forcedTimer := time.NewTimer(mcpClientForcedCloseWaitTimeout)
	defer forcedTimer.Stop()
	select {
	case <-done:
	case <-forcedTimer.C:
		log.Printf("[mcp] %s: client close still blocked after subprocess cancellation", name)
	}
}

// ConfigFor returns the config for a server, if any.
func (m *ClientManager) ConfigFor(serverName string) (MCPServerConfig, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cfg, ok := m.configs[serverName]
	return cfg, ok
}

// Disconnect closes a single server's client, removing it from the active map.
// The config and tool cache are preserved so the server can reconnect later.
//
// Cancels the per-server cmdCtx before c.Close so the process group dies
// promptly even when the subprocess ignores stdin EOF (see Close for full
// rationale). Without this an mcp-remote stuck on an abandoned OAuth flow
// would keep holding its loopback callback port and crash any subsequent
// reconnect with EADDRINUSE.
func (m *ClientManager) Disconnect(serverName string) {
	m.mu.Lock()
	// Cancel any pending idle timer for this server.
	if t, ok := m.idleTimers[serverName]; ok {
		t.Stop()
		delete(m.idleTimers, serverName)
	}
	c, ok := m.clients[serverName]
	if ok {
		delete(m.clients, serverName)
	}
	cmdCancel := m.cancellers[serverName]
	delete(m.cancellers, serverName)
	m.mu.Unlock()
	if ok && c != nil {
		closeManagedMCPClient(serverName, c, cmdCancel, mcpClientGracefulCloseTimeout)
	} else if cmdCancel != nil {
		cmdCancel()
	}
}

// DisconnectAfterIdle schedules a Disconnect after the given idle duration.
// If called again before the timer fires, the timer resets. This allows
// multi-turn browser workflows to keep the connection alive while
// disconnecting after a period of inactivity.
func (m *ClientManager) DisconnectAfterIdle(serverName string, d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.idleTimers == nil {
		m.idleTimers = make(map[string]*time.Timer)
	}
	if t, ok := m.idleTimers[serverName]; ok {
		t.Stop()
	}
	m.idleTimers[serverName] = time.AfterFunc(d, func() {
		log.Printf("[mcp] %s: idle timeout reached, disconnecting", serverName)
		m.Disconnect(serverName)
	})
}

// CancelIdleDisconnect cancels a pending idle disconnect timer, if any.
func (m *ClientManager) CancelIdleDisconnect(serverName string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if t, ok := m.idleTimers[serverName]; ok {
		t.Stop()
		delete(m.idleTimers, serverName)
	}
}

func (m *ClientManager) SetSupervised(v bool) {
	m.mu.Lock()
	m.supervised = v
	m.mu.Unlock()
}

// SetToolCallTimeout sets the default per-attempt tools/call timeout applied
// when a server config carries no ToolTimeoutSeconds override. Wired from
// `mcp.tool_timeout_secs`. Zero keeps DefaultToolCallTimeout.
func (m *ClientManager) SetToolCallTimeout(d time.Duration) {
	m.mu.Lock()
	m.toolCallTimeout = d
	m.mu.Unlock()
}

// resolveToolCallTimeout picks the per-attempt tools/call bound:
// per-server ToolTimeoutSeconds > manager default > DefaultToolCallTimeout.
func (m *ClientManager) resolveToolCallTimeout(cfg MCPServerConfig, hasCfg bool) time.Duration {
	if hasCfg && cfg.ToolTimeoutSeconds > 0 {
		return time.Duration(cfg.ToolTimeoutSeconds) * time.Second
	}
	m.mu.Lock()
	d := m.toolCallTimeout
	m.mu.Unlock()
	if d > 0 {
		return d
	}
	return DefaultToolCallTimeout
}

// SetNeedsSetup marks a server as needing setup (e.g. readiness marker absent).
func (m *ClientManager) SetNeedsSetup(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.needsSetup[name] = true
}

// NeedsSetup reports whether a server is gated by a missing readiness marker.
func (m *ClientManager) NeedsSetup(name string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.needsSetup[name]
}

func (m *ClientManager) CachedTools(serverName string) []RemoteTool {
	m.mu.Lock()
	defer m.mu.Unlock()
	tools := m.toolCache[serverName]
	if tools == nil {
		return nil
	}
	cp := make([]RemoteTool, len(tools))
	copy(cp, tools)
	return cp
}

// SeedToolCache sets cached tools for a server. Test helper only.
func (m *ClientManager) SeedToolCache(serverName string, tools []RemoteTool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.toolCache[serverName] = tools
}

// SeedClient injects a client for a server. Test helper only.
func (m *ClientManager) SeedClient(serverName string, c mcpclient.MCPClient) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.clients[serverName] = c
}

// SeedConfig sets the config for a server. Test helper only.
func (m *ClientManager) SeedConfig(serverName string, cfg MCPServerConfig) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.configs[serverName] = cfg
}

func (m *ClientManager) ProbeTransport(ctx context.Context, serverName string) error {
	m.mu.Lock()
	c, ok := m.clients[serverName]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("MCP server %q not connected", serverName)
	}
	_, err := c.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		return fmt.Errorf("transport probe failed: %w", err)
	}
	return nil
}

func (m *ClientManager) Reconnect(ctx context.Context, serverName string) ([]RemoteTool, error) {
	m.mu.Lock()
	cfg, hasCfg := m.configs[serverName]
	if !hasCfg {
		m.mu.Unlock()
		return nil, fmt.Errorf("no config for MCP server %q", serverName)
	}
	// A disabled server's config stays in m.configs (so /config/status can
	// render it), but reconnect must never spawn its subprocess — the user
	// turned it off. Without this gate any ProbeNow/reconnect path aimed at
	// a disabled name silently relaunched it.
	if cfg.Disabled {
		m.mu.Unlock()
		return nil, fmt.Errorf("MCP server %q is disabled", serverName)
	}
	rmu, ok := m.reconnectMu[serverName]
	if !ok {
		rmu = &sync.Mutex{}
		m.reconnectMu[serverName] = rmu
	}
	stale := m.clients[serverName]
	staleCancel := m.cancellers[serverName]
	m.mu.Unlock()

	rmu.Lock()
	defer rmu.Unlock()

	// Refuse to reconnect while a different connect goroutine is mid-flight
	// for this server (e.g. StartConnectAll fired from daemon startup or a
	// /config/reload retry). Without this check, the two attempts race to
	// bind the OAuth callback port and the loser crashes with EADDRINUSE.
	release, ok := m.tryReserveInFlight(serverName)
	if !ok {
		return nil, fmt.Errorf("reconnect for %q skipped: connect already in flight", serverName)
	}
	defer release()

	// Reap the old process group first so c.Close (cmd.Wait inside) returns
	// promptly even when the old subprocess ignores stdin EOF.
	if staleCancel != nil {
		staleCancel()
	}
	if stale != nil {
		_ = stale.Close()
	}
	m.mu.Lock()
	delete(m.clients, serverName)
	delete(m.cancellers, serverName)
	m.mu.Unlock()

	return m.connect(ctx, serverName, cfg)
}

// IsTransportError reports whether err indicates a transport/connection failure
// (process exited, broken pipe, EOF) rather than a tool-logic or protocol error.
// Only transport errors should trigger a reconnect attempt — retrying on logic
// errors risks duplicating non-idempotent side effects.
// replaySafeFromCache reports whether the cached tool advertisement for
// serverName/toolName carries annotations that make an automatic
// re-dispatch safe (see ToolReplaySafe). A cache miss — server never
// listed, tool renamed, cache cleared — is conservatively unsafe: without
// the advertisement there is no evidence the tool is side-effect-free.
func (m *ClientManager) replaySafeFromCache(serverName, toolName string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, t := range m.toolCache[serverName] {
		if t.Tool.Name == toolName {
			return ToolReplaySafe(t.Tool)
		}
	}
	return false
}

func IsTransportError(err error) bool {
	// Innermost semantics win over wrapper type: mcp-go's client layer wraps
	// EVERY transport SendRequest error in *transport.Error — including the
	// bare ctx.Err() its stdio transport returns when a per-call deadline
	// expires. A timed-out call is NOT a dead connection; retrying it would
	// re-dispatch a non-idempotent tool call that already ran for the full
	// timeout budget. The dead-process chains below (ErrTransportClosed,
	// broken pipe, EOF) never carry a context error, so this exclusion
	// cannot mask a real transport failure.
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return false
	}
	// mcp-go wraps every transport-level send failure in *transport.Error
	// and signals a dead stdio subprocess with transport.ErrTransportClosed.
	// Classify by TYPE, not message text — live 2026-07-30 repro: a kill -9'd
	// workspace-mcp produced "transport error: transport closed", which the
	// string list below does not match, so the dead-connection retry never
	// fired and the model just saw a hard failure.
	var te *transport.Error
	if errors.As(err, &te) {
		return true
	}
	if errors.Is(err, transport.ErrTransportClosed) {
		return true
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	s := err.Error()
	return strings.Contains(s, "broken pipe") ||
		strings.Contains(s, "use of closed network connection") ||
		strings.Contains(s, "read/write on closed pipe") ||
		strings.Contains(s, "signal: killed") ||
		strings.Contains(s, "process already finished")
}

func buildEnvSlice(env map[string]string) []string {
	if len(env) == 0 {
		return nil
	}
	result := make([]string, 0, len(env))
	for k, v := range env {
		result = append(result, k+"="+v)
	}
	return result
}

// BuildContext collects context strings from all configured MCP servers.
func BuildContext(servers map[string]MCPServerConfig) string {
	var parts []string
	for name, cfg := range servers {
		if cfg.Disabled || cfg.Context == "" {
			continue
		}
		parts = append(parts, fmt.Sprintf("[%s] %s", name, cfg.Context))
	}
	if len(parts) == 0 {
		return ""
	}
	result := ""
	for i, p := range parts {
		if i > 0 {
			result += "\n"
		}
		result += p
	}
	return result
}

// IsPlaywrightCDPMode reports whether the args include --cdp-endpoint.
func IsPlaywrightCDPMode(cfg MCPServerConfig) bool {
	_, ok := playwrightCDPEndpointArg(cfg)
	return ok
}

// NormalizePlaywrightCDPConfig migrates legacy localhost:9222 configs to the
// dedicated daemon-owned CDP port while preserving explicit custom endpoints.
func NormalizePlaywrightCDPConfig(cfg MCPServerConfig) MCPServerConfig {
	if !IsPlaywrightCDPMode(cfg) {
		return cfg
	}
	args := append([]string(nil), cfg.Args...)
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--cdp-endpoint" && i+1 < len(args):
			args[i+1] = normalizePlaywrightCDPEndpoint(args[i+1])
			cfg.Args = args
			return cfg
		case strings.HasPrefix(args[i], "--cdp-endpoint="):
			raw := strings.TrimPrefix(args[i], "--cdp-endpoint=")
			args[i] = "--cdp-endpoint=" + normalizePlaywrightCDPEndpoint(raw)
			cfg.Args = args
			return cfg
		}
	}
	cfg.Args = args
	return cfg
}

func playwrightCDPEndpointArg(cfg MCPServerConfig) (string, bool) {
	for i, arg := range cfg.Args {
		if arg == "--cdp-endpoint" {
			if i+1 < len(cfg.Args) {
				return cfg.Args[i+1], true
			}
			return "", true
		}
		if strings.HasPrefix(arg, "--cdp-endpoint=") {
			return strings.TrimPrefix(arg, "--cdp-endpoint="), true
		}
	}
	return "", false
}

// PlaywrightCDPPort extracts the configured CDP port, defaulting to the
// daemon-owned dedicated port when absent or invalid.
func PlaywrightCDPPort(cfg MCPServerConfig) int {
	endpoint, ok := playwrightCDPEndpointArg(cfg)
	if !ok {
		return DefaultCDPPort
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return DefaultCDPPort
	}
	if port := u.Port(); port != "" {
		if n, err := strconv.Atoi(port); err == nil && n > 0 {
			return n
		}
	}
	return DefaultCDPPort
}

func normalizePlaywrightCDPEndpoint(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	host := u.Hostname()
	port := u.Port()
	if port != strconv.Itoa(LegacyCDPPort) {
		return raw
	}
	if host != "localhost" && host != "127.0.0.1" && host != "::1" {
		return raw
	}
	u.Host = net.JoinHostPort("127.0.0.1", strconv.Itoa(DefaultCDPPort))
	return u.String()
}
