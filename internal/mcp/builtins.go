package mcp

// Built-in MCP server catalog.
//
// Entries here ship inside the daemon binary and surface to Desktop as
// pre-installed-but-disabled MCP servers. Users flip the toggle in Desktop
// (PATCH /config with `disabled: false`) to activate them; on activation the
// daemon spawns the configured subprocess (or opens the HTTP endpoint) and
// any OAuth flow the upstream server defines runs end-to-end.
//
// Storage model:
//   - The catalog is the single source of truth for command / args / type /
//     url / context. config.Load merges these onto whatever the user has in
//     ~/.shannon/config.yaml; daemon upgrades therefore pick up command or
//     URL fixes without yaml surgery.
//   - User-controlled fields (Disabled, Env, KeepAlive) round-trip through
//     yaml and persist across upgrades.
//   - PATCH /config rejects edits to BuiltinImmutableFields on built-in
//     entries (see internal/daemon/safeguard.go validateBuiltinMCPPatch).
//
// Adding a new built-in:
//   1. Add a name => BuiltinEntry row below.
//   2. No other code changes — config merge + daemon API + Desktop UI all
//      key off this map.

// BuiltinEntry describes a single pre-bundled MCP server.
type BuiltinEntry struct {
	DisplayName  string          // human label (e.g. "Intercom")
	Description  string          // one-line description for the Desktop list
	AuthHint     string          // full text to drop into a confirm modal / tooltip; empty if no auth
	RequiresAuth bool            // true → Desktop should show a confirm dialog (with AuthHint as body) BEFORE flipping the toggle, because activation triggers an out-of-process browser OAuth flow that the user needs to expect
	Config       MCPServerConfig // command/args/type/url/context — overrides user yaml
}

// BuiltinMCPServers is the catalog. Keys MUST be valid MCP server names
// (same charset as user-added entries); Desktop uses the key as the
// stable identifier in PATCH /config payloads.
var BuiltinMCPServers = map[string]BuiltinEntry{
	"intercom": {
		DisplayName: "Intercom",
		Description: "Intercom customer messaging — conversations, contacts, articles, tickets.",
		// Long-form on purpose: Desktop uses this text verbatim as the
		// confirm-modal body (see references/mcp.md). It needs to set
		// realistic time expectations and instruct the user to keep the
		// browser visible, because the first activation downloads mcp-remote
		// via npx, then opens a browser tab for Intercom OAuth — total
		// latency is commonly 5–20 seconds on a cold cache.
		AuthHint:     "Enabling Intercom opens your browser to complete OAuth authorization with Intercom. The browser tab can take 5–20 seconds to appear on first activation while the daemon downloads the bridge tool. Please keep Kocoro open and approve the prompt in your browser to finish setup.",
		RequiresAuth: true,
		Config: MCPServerConfig{
			Command: "npx",
			Args:    []string{"mcp-remote", "https://mcp.intercom.com/mcp"},
			Context: "Intercom MCP server. Use it to read Intercom conversations, contacts, companies, and help center articles, or to send replies on behalf of the workspace owner.",
			// 300s gives the user a 5-minute window to complete the OAuth
			// flow in the browser before the daemon kills the npx subprocess.
			// The default 60s is too short for cold-cache mcp-remote downloads
			// followed by manual OAuth approval (commonly 30–180s).
			ConnectTimeoutSeconds: 300,
		},
	},
}

// BuiltinImmutableFields names the MCPServerConfig fields that the daemon
// owns for built-in entries. PATCH /config refuses to write any of these
// keys onto a built-in server; user-controlled fields (disabled, env,
// keep_alive) stay writable.
var BuiltinImmutableFields = []string{
	"command",
	"args",
	"type",
	"url",
	"context",
}

// IsBuiltin reports whether name refers to an entry in BuiltinMCPServers.
func IsBuiltin(name string) bool {
	_, ok := BuiltinMCPServers[name]
	return ok
}
