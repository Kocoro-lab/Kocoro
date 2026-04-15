package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/cwdctx"
	"github.com/Kocoro-lab/ShanClaw/internal/mcp"
	mcpproto "github.com/mark3labs/mcp-go/mcp"
)

// fileProducingMCPArgs maps a known MCP tool to the args that carry a
// caller-supplied output filename. When the session has a CWD and the arg is
// a relative path, the adapter rewrites it to an absolute path under that CWD
// before forwarding. This keeps the file where subsequent `file_read`/`bash`
// calls can find it by the same name — otherwise the MCP server (e.g.
// playwright-mcp) writes relative to its own process CWD and the model has
// to guess or grep the filesystem to locate the artifact.
//
// Scope is intentionally narrow: only tools known to take a filename/path
// argument for output appear here. Other MCP results are left opaque.
var fileProducingMCPArgs = map[string][]string{
	// server/tool → arg names in priority order
	"playwright/browser_take_screenshot": {"filename"},
	"playwright/browser_snapshot":        {"filename"},
}

const maxMCPDescLen = 500

var (
	isPlaywrightCDPMode          = mcp.IsPlaywrightCDPMode
	playwrightCDPPort            = mcp.PlaywrightCDPPort
	ensureChromeDebugPort        = mcp.EnsureChromeDebugPort
	shouldPreflightChromeForTool = mcp.ShouldPreflightDedicatedChrome
)

// MCPTool wraps an MCP server tool as a local agent.Tool.
type MCPTool struct {
	serverName string
	tool       mcpproto.Tool
	manager    *mcp.ClientManager
	supervisor *mcp.Supervisor // optional — enables on-demand reconnect
}

// NewMCPTool creates a tool adapter for an MCP server tool.
func NewMCPTool(serverName string, tool mcpproto.Tool, manager *mcp.ClientManager) *MCPTool {
	return &MCPTool{
		serverName: serverName,
		tool:       tool,
		manager:    manager,
	}
}

// SetSupervisor enables on-demand reconnect: if CallTool fails and the server
// is disconnected, ProbeNow triggers reconnect and the call is retried once.
func (t *MCPTool) SetSupervisor(sup *mcp.Supervisor) {
	t.supervisor = sup
}

func (t *MCPTool) Info() agent.ToolInfo {
	desc := t.tool.Description
	if desc == "" {
		desc = fmt.Sprintf("MCP tool from %s", t.serverName)
	}
	if r := []rune(desc); len(r) > maxMCPDescLen {
		desc = string(r[:maxMCPDescLen]) + "..."
	}

	// Strip control characters from tool name
	name := strings.Map(func(r rune) rune {
		if r < 32 || r == 127 {
			return -1
		}
		return r
	}, t.tool.Name)

	// Convert MCP input schema to our parameters format
	params := make(map[string]any)
	if t.tool.InputSchema.Properties != nil {
		params["type"] = "object"
		params["properties"] = t.tool.InputSchema.Properties
	}

	var required []string
	for _, r := range t.tool.InputSchema.Required {
		required = append(required, r)
	}

	return agent.ToolInfo{
		Name:        name,
		Description: fmt.Sprintf("[%s] %s", t.serverName, desc),
		Parameters:  params,
		Required:    required,
	}
}

func (t *MCPTool) Run(ctx context.Context, argsJSON string) (agent.ToolResult, error) {
	var args map[string]any
	if argsJSON != "" && argsJSON != "{}" {
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
			return agent.ToolResult{Content: fmt.Sprintf("invalid arguments: %v", err), IsError: true}, nil
		}
	}
	if args == nil {
		args = make(map[string]any)
	}

	// CDP mode: ensure Chrome is running when playwright is not yet connected.
	// Also preflight the daemon-owned dedicated Chrome on first tool use for the
	// default dedicated port, even if the Playwright MCP process is already connected.
	// This preserves the copied-profile/session behavior instead of letting the MCP
	// server improvise its own temporary browser.
	if t.serverName == "playwright" {
		if cfg, ok := t.manager.ConfigFor(t.serverName); ok && isPlaywrightCDPMode(cfg) {
			port := playwrightCDPPort(cfg)
			if !t.manager.IsConnected(t.serverName) || shouldPreflightChromeForTool(port) {
				if err := ensureChromeDebugPort(port); err != nil {
					return agent.ToolResult{Content: fmt.Sprintf("Chrome CDP unavailable: %v", err), IsError: true}, nil
				}
			}
		}
		// file:// preview bridge: Playwright's Chromium rejects file://
		// navigations. If a bridge is attached to ctx, intercept
		// browser_navigate(url=file://...) and rewrite the URL to a
		// short-lived http://127.0.0.1/<token>/<name> endpoint scoped to
		// exactly that one file.
		if t.tool.Name == "browser_navigate" {
			if rewritten, ok := maybeRewriteFileURL(ctx, args); ok {
				args["url"] = rewritten
			}
		}
	}

	// Relative output filenames for known file-producing MCP tools: if the
	// caller passed a bare name ("snapshot.md"), rewrite it to an absolute
	// path under the session CWD so both the MCP server and our subsequent
	// file_read agree on the same location. Unrelated tools are not touched.
	rewrittenOutPath := maybeRewriteFileProducingArg(ctx, t.serverName, t.tool.Name, args)

	content, isError, err := t.manager.CallTool(ctx, t.serverName, t.tool.Name, args)
	if err != nil && t.supervisor != nil {
		// Connection dead — attempt on-demand reconnect and retry once.
		h := t.supervisor.HealthFor(t.serverName)
		if h.State == mcp.StateDisconnected {
			log.Printf("[mcp-tool] %s/%s: connection dead, triggering on-demand reconnect", t.serverName, t.tool.Name)
			// Re-ensure Chrome CDP is available before reconnecting — Chrome may
			// have died along with the MCP connection.
			if t.serverName == "playwright" {
				if cfg, ok := t.manager.ConfigFor(t.serverName); ok && isPlaywrightCDPMode(cfg) {
					_ = ensureChromeDebugPort(playwrightCDPPort(cfg))
				}
			}
			reconHealth := t.supervisor.ProbeNow(t.serverName)
			if reconHealth.State == mcp.StateHealthy {
				content, isError, err = t.manager.CallTool(ctx, t.serverName, t.tool.Name, args)
			}
		}
	}
	if err != nil {
		return agent.ToolResult{Content: fmt.Sprintf("MCP call failed: %v", err), IsError: true}, nil
	}

	content = normalizeMCPResult(t.serverName, t.tool.Name, content, isError)
	if !isError && rewrittenOutPath != "" {
		content = annotateAbsPath(content, rewrittenOutPath)
	}
	return agent.ToolResult{Content: content, IsError: isError}, nil
}

func (t *MCPTool) RequiresApproval() bool { return false }

// ToolSource implements agent.ToolSourcer for deterministic tool ordering.
func (t *MCPTool) ToolSource() agent.ToolSource { return agent.SourceMCP }

// maybeRewriteFileProducingArg rewrites the first relative output-path arg
// (per fileProducingMCPArgs) to an absolute path under the session CWD. It
// mutates args in place and returns the rewritten absolute path, or "" when
// no rewrite happened (unknown tool, no session CWD, already absolute, arg
// missing, or arg has an unexpected type). This is a best-effort helper —
// a failed rewrite is never fatal; the call continues with original args.
func maybeRewriteFileProducingArg(ctx context.Context, serverName, toolName string, args map[string]any) string {
	argNames, ok := fileProducingMCPArgs[serverName+"/"+toolName]
	if !ok {
		return ""
	}
	cwd := cwdctx.FromContext(ctx)
	if cwd == "" || !filepath.IsAbs(cwd) {
		return ""
	}
	for _, name := range argNames {
		raw, present := args[name]
		if !present {
			continue
		}
		s, isStr := raw.(string)
		if !isStr {
			continue
		}
		trimmed := strings.TrimSpace(s)
		if trimmed == "" {
			continue
		}
		// Tilde-prefixed paths: the caller wants a home-relative absolute
		// path, not a session-scoped one. We must expand `~` ourselves
		// before handing the filename to the MCP server — playwright-mcp
		// (and most Node-based MCPs) do not do shell-style tilde expansion,
		// so a literal `~/Desktop/x.md` would get written to `./~/Desktop/x.md`
		// relative to the server's process CWD. Rewrite the arg in place to
		// the expanded absolute path and return it so the result can be
		// annotated with the real location. This matches the tilde handling
		// elsewhere in the agent (cwdctx.ResolveFilesystemPath, bash tool).
		if strings.HasPrefix(trimmed, "~/") || trimmed == "~" {
			home, err := os.UserHomeDir()
			if err != nil {
				continue
			}
			var expanded string
			if trimmed == "~" {
				expanded = home
			} else {
				expanded = filepath.Join(home, strings.TrimPrefix(trimmed, "~/"))
			}
			expanded = filepath.Clean(expanded)
			args[name] = expanded
			return expanded
		}
		if filepath.IsAbs(trimmed) {
			continue
		}
		// Reject anything that tries to climb out of the session CWD. Keeping
		// the rewrite inside the session sandbox avoids accidentally aiming
		// the MCP server at (say) ~/.ssh. Also reject values that resolve to
		// the session CWD itself (".", "./", trailing ".."): the MCP server
		// needs a real filename, and passing the directory path would produce
		// malformed artifacts. On reject we fall through (empty return); the
		// original relative value still goes to the server, which will use its
		// own CWD — behavior unchanged from pre-fix for that edge case.
		abs := filepath.Clean(filepath.Join(cwd, trimmed))
		if abs == cwd {
			continue
		}
		if !strings.HasPrefix(abs+string(filepath.Separator), cwd+string(filepath.Separator)) {
			continue
		}
		args[name] = abs
		return abs
	}
	return ""
}

// annotateAbsPath ensures a "Saved to: <abs>" line is present in an MCP tool
// result so the model sees an absolute path even when the server echoes only
// a relative reference (e.g. playwright-mcp's `[Snapshot](name.md)`). Idempotent:
// if the path already appears verbatim, the content is returned unchanged.
func annotateAbsPath(content, absPath string) string {
	if absPath == "" || strings.Contains(content, absPath) {
		return content
	}
	line := "Saved to: " + absPath
	if content == "" {
		return line
	}
	return content + "\n\n" + line
}

// maybeRewriteFileURL extracts a file:// URL from a browser_navigate args
// map and rewrites it to the local preview-bridge URL. Returns the
// rewritten URL and true on success; (unchanged, false) if there is no
// file URL, no bridge on ctx, or the rewrite fails for any reason. On
// failure the original URL is left intact so the upstream MCP error
// surface (Chromium's "file:// blocked" message) is preserved.
func maybeRewriteFileURL(ctx context.Context, args map[string]any) (string, bool) {
	bridge := FilePreviewFrom(ctx)
	if bridge == nil {
		return "", false
	}
	raw, ok := args["url"].(string)
	if !ok {
		return "", false
	}
	if !strings.HasPrefix(strings.ToLower(raw), "file://") {
		return "", false
	}
	rewritten, err := bridge.RewriteFileURL(raw)
	if err != nil {
		log.Printf("[mcp-tool] file:// preview rewrite failed for %q: %v", raw, err)
		return "", false
	}
	return rewritten, true
}
