package tools

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

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

// fileOutputArgHint steers the model's filename choice for the tools in
// fileProducingMCPArgs. 2026-08-02 incident: with the session CWD set to
// ~/Desktop, the model self-addressed browser_snapshot intermediates as
// absolute paths under that CWD. Absolute paths deliberately bypass the
// artifact-scratch rewrite (they are the model's only way to place a
// user-requested deliverable), so machine intermediates piled up on the
// user's Desktop. The adapter cannot tell deliverable from intermediate on
// an absolute path — only the model can, and the tool description is the
// surface it reads while choosing the argument. Appended AFTER the
// maxMCPDescLen truncation so it is never cut off; byte-stable per session,
// so prompt-cache safe.
const fileOutputArgHint = " When saving output to a file for your own later reading, pass a BARE relative filename (e.g. \"page.md\") — it resolves into the session's artifact directory (a per-session scratch dir on daemon-served runs, the working directory in CLI/TUI runs) and the result echoes the absolute path. Only pass an absolute path when the user explicitly asked for the file at that location."

// snapshotOutputArgHint is the browser_snapshot variant: for that tool an
// omitted filename means the INLINE accessibility snapshot — the model's
// primary page-reading channel — so the hint must not nudge it into file
// mode by default.
const snapshotOutputArgHint = " Prefer omitting filename entirely — the inline snapshot is the primary way to read a page. If you DO save to a file for your own later reading, pass a BARE relative filename (e.g. \"page.md\") — it resolves into the session's artifact directory (a per-session scratch dir on daemon-served runs, the working directory in CLI/TUI runs) and the result echoes the absolute path. Only pass an absolute path when the user explicitly asked for the file at that location."

var (
	isPlaywrightCDPMode          = mcp.IsPlaywrightCDPMode
	playwrightCDPPort            = mcp.PlaywrightCDPPort
	ensureChromeDebugPort        = mcp.EnsureChromeDebugPort
	shouldPreflightChromeForTool = mcp.ShouldPreflightDedicatedChrome
	playwrightCDPPageURLs        = mcp.CDPPageURLsOnPort
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
	if key := t.serverName + "/" + t.tool.Name; fileProducingMCPArgs[key] != nil {
		if key == "playwright/browser_snapshot" {
			desc += snapshotOutputArgHint
		} else {
			desc += fileOutputArgHint
		}
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
	if result, valid := agent.ValidateToolArgumentPresence(t.Info(), argsJSON); !valid {
		return result, nil
	}
	var args map[string]any
	if argsJSON != "" && argsJSON != "{}" {
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
			return agent.ValidationError(fmt.Sprintf("invalid arguments: %v", err)), nil
		}
	}
	if args == nil {
		args = make(map[string]any)
	}
	if t.blocksUnrestrictedPlaywrightCode() {
		return xUnrestrictedPlaywrightCodeBlockedResult(), nil
	}
	if blocked, result := t.blockPlaywrightXComposerNavigation(args); blocked {
		return result, nil
	}
	var preDispatchGuard func() error

	// CDP mode: ensure Chrome is running when playwright is not yet connected.
	// Also preflight the daemon-owned dedicated Chrome on first tool use for the
	// default dedicated port, even if the Playwright MCP process is already connected.
	// This preserves the copied-profile/session behavior instead of letting the MCP
	// server improvise its own temporary browser.
	if t.serverName == "playwright" {
		if t.manager != nil {
			if cfg, ok := t.manager.ConfigFor(t.serverName); ok && isPlaywrightCDPMode(cfg) {
				mcp.MarkChromeUsed(ctx)
				port := playwrightCDPPort(cfg)
				if !t.manager.IsConnected(t.serverName) || shouldPreflightChromeForTool(port) {
					if err := ensureChromeDebugPort(port); err != nil {
						return agent.ToolResult{Content: fmt.Sprintf("Chrome CDP unavailable: %v", err), IsError: true}, nil
					}
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
		var blocked agent.ToolResult
		preDispatchGuard, blocked = t.playwrightXMutationGuard(args)
		if blocked.IsError {
			return blocked, nil
		}
	}

	// Known-dead dispatch gate: when the supervisor has already marked this
	// server disconnected (subprocess died or wedged while idle), reconcile
	// via ProbeNow BEFORE dispatching instead of discovering the corpse
	// mid-call. Without this, the call lands on the stale client and blocks
	// until the per-call timeout — 2026-07-29 incident: google-workspace was
	// marked disconnected at 11:53, a 14:11 tool call sat ~6.5 minutes on
	// the dead pipe before erroring, while the eventual reconnect took 12s.
	// ProbeNow re-probes transport and, only if still disconnected, attempts
	// an on-demand reconnect (bounded: 10s probe + 15s reconnect).
	if t.supervisor != nil {
		if h := t.supervisor.HealthFor(t.serverName); h.State == mcp.StateDisconnected {
			log.Printf("[mcp-tool] %s/%s: server known-disconnected, probing before dispatch", t.serverName, t.tool.Name)
			t.supervisor.ProbeNow(t.serverName)
		}
	}

	// Relative output filenames for known file-producing MCP tools: if the
	// caller passed a bare name ("snapshot.md"), rewrite it to an absolute
	// path under the session CWD so both the MCP server and our subsequent
	// file_read agree on the same location. Unrelated tools are not touched.
	rewrittenOutPath := maybeRewriteFileProducingArg(ctx, t.serverName, t.tool.Name, args)

	callTool := func() (mcp.ToolCallContent, bool, error) {
		return t.manager.CallToolGuarded(ctx, t.serverName, t.tool.Name, args, preDispatchGuard)
	}
	out, isError, err := callTool()
	if err != nil && t.supervisor != nil && ctx.Err() == nil && mcp.IsTransportError(err) {
		// Post-dispatch TRANSPORT failure. The request was already written to
		// a server that was alive at dispatch time, and "connection died"
		// does NOT prove the server never acted: a stdio server can execute
		// its side effect and exit before writing the JSON-RPC response —
		// on the wire that is indistinguishable from died-before-acting.
		// Recovery therefore splits into two independent halves:
		//   - REPAIR always runs: MarkTransportSuspect invalidates the
		//     supervisor's <60s health-cache freshness so ProbeNow performs a
		//     REAL probe (and on-demand reconnect) — the NEXT call must not
		//     land on the corpse (2026-07-29: 6.5 min on a dead pipe);
		//   - REPLAY is gated on the tool's own MCP annotations
		//     (readOnlyHint/idempotentHint): only a tool whose duplicate
		//     execution is declared harmless is re-dispatched. Everything
		//     else — send-message, create-event, unannotated tools — surfaces
		//     as outcome-unknown so the model verifies instead of silently
		//     double-executing a write.
		// Deliberately excluded from all of this (see IsTransportError):
		//   - per-call timeouts: the server may still be executing;
		//   - JSON-RPC/protocol errors: a second dispatch fails identically;
		//   - cancelled ctx: retry after user interrupt is wasted work.
		log.Printf("[mcp-tool] %s/%s: transport failure (%v), probing for on-demand reconnect", t.serverName, t.tool.Name, err)
		t.supervisor.MarkTransportSuspect(t.serverName)
		// Re-ensure Chrome CDP is available before reconnecting — Chrome may
		// have died along with the MCP connection.
		if t.serverName == "playwright" {
			if cfg, ok := t.manager.ConfigFor(t.serverName); ok && isPlaywrightCDPMode(cfg) {
				_ = ensureChromeDebugPort(playwrightCDPPort(cfg))
			}
		}
		reconHealth := t.supervisor.ProbeNow(t.serverName)
		if !mcp.ToolReplaySafe(t.tool) {
			err = &mcp.OutcomeUnknownError{Server: t.serverName, Tool: t.tool.Name, Err: err}
		} else if reconHealth.State == mcp.StateHealthy {
			out, isError, err = callTool()
		}
	}
	if err != nil {
		var guardErr *playwrightPreDispatchGuardError
		if errors.As(err, &guardErr) {
			return guardErr.result, nil
		}
		var unknown *mcp.OutcomeUnknownError
		if errors.As(err, &unknown) {
			return agent.ToolResult{
				Content:                  outcomeUnknownResultMessage(unknown),
				IsError:                  true,
				SideEffectOutcomeUnknown: true,
			}, nil
		}
		return agent.ToolResult{Content: fmt.Sprintf("MCP call failed: %v", err), IsError: true}, nil
	}

	content := normalizeMCPResult(t.serverName, t.tool.Name, out.Text, isError)
	if isError && looksLikeRemoteValidationError(content) {
		return agent.ValidationError(strings.TrimPrefix(content, "[validation error] ")), nil
	}
	if !isError && rewrittenOutPath != "" {
		content = annotateAbsPath(content, rewrittenOutPath)
	}
	if !isError {
		// Translate server-relative artifact links (e.g. playwright's
		// first-root-relative screenshot paths) to absolute paths the model
		// can act on. No-op for servers with unknown path semantics.
		content = maybeAnnotateResultPaths(t.serverName, content, t.manager)
	}
	result := agent.ToolResult{Content: content, IsError: isError}
	// An error result's images are not worth the tokens — the text carries the
	// failure. A successful one routes them through the same source-time
	// compression every other image path uses (imaging.go), so the wire-level
	// and persist-time guards see an already-bounded payload.
	if !isError {
		result.Images = decodeMCPImages(t.serverName, t.tool.Name, out.Images)
	}
	return result, nil
}

// decodeMCPImages converts an MCP result's images into vision-compatible
// blocks. An image that fails to decode or compress is dropped with a log
// line rather than failing the call: the text content is usually the primary
// answer, and a broken image must never turn a completed tool call into an
// error.
func decodeMCPImages(serverName, toolName string, imgs []mcp.ToolCallImage) []agent.ImageBlock {
	if len(imgs) == 0 {
		return nil
	}
	blocks := make([]agent.ImageBlock, 0, len(imgs))
	for i, img := range imgs {
		raw, err := base64.StdEncoding.DecodeString(img.Base64)
		if err != nil {
			log.Printf("[mcp-tool] %s/%s: image %d has undecodable base64 (%v), dropped", serverName, toolName, i, err)
			continue
		}
		mediaType := img.MIMEType
		if mediaType == "" {
			mediaType = "image/png"
		}
		block, err := EncodeImageBytes(raw, mediaType)
		if err != nil {
			log.Printf("[mcp-tool] %s/%s: image %d failed compression (%v), dropped", serverName, toolName, i, err)
			continue
		}
		blocks = append(blocks, block)
	}
	if len(blocks) == 0 {
		return nil
	}
	return blocks
}

func (t *MCPTool) blockPlaywrightXComposerNavigation(args map[string]any) (bool, agent.ToolResult) {
	if t == nil || t.serverName != "playwright" {
		return false, agent.ToolResult{}
	}
	if t.tool.Name == "browser_navigate" {
		if raw, _ := args["url"].(string); isXComposerURL(raw) {
			return true, xAutomationBlockedResult()
		}
		return false, agent.ToolResult{}
	}
	return false, agent.ToolResult{}
}

func (t *MCPTool) blocksUnrestrictedPlaywrightCode() bool {
	return t != nil && canonicalPlaywrightToolDisabled(t.serverName, t.tool.Name)
}

func canonicalPlaywrightToolDisabled(serverName, toolName string) bool {
	return serverName == "playwright" &&
		(toolName == "browser_evaluate" || toolName == "browser_run_code")
}

type playwrightPreDispatchGuardError struct {
	result agent.ToolResult
}

func (e *playwrightPreDispatchGuardError) Error() string {
	if e == nil {
		return "Playwright pre-dispatch guard rejected the call"
	}
	return e.result.Content
}

func (t *MCPTool) playwrightXMutationGuard(args map[string]any) (func() error, agent.ToolResult) {
	if t == nil || t.serverName != "playwright" ||
		!playwrightMutationCanPublish(t.tool.Name) {
		return nil, agent.ToolResult{}
	}
	evidence, _ := json.Marshal(args)
	composerHint := looksLikeExplicitXComposerControl(string(evidence))
	if composerHint {
		return nil, xAutomationBlockedResult()
	}
	if t.manager == nil {
		return nil, agent.ToolResult{}
	}
	cfg, ok := t.manager.ConfigFor(t.serverName)
	if !ok || !isPlaywrightCDPMode(cfg) {
		// Non-CDP Playwright has no reliable shared-target introspection. Preserve
		// ordinary mutation instead of globally blocking the browser; only direct
		// composer URLs, explicit composer controls, and unrestricted code tools
		// are covered there.
		return nil, agent.ToolResult{}
	}
	port := playwrightCDPPort(cfg)
	return func() error {
		urls, err := playwrightCDPPageURLs(port)
		if err != nil || len(urls) == 0 {
			return &playwrightPreDispatchGuardError{result: agent.TransientError(
				fmt.Sprintf("Playwright browser is not ready for guarded mutation: %v", err),
			)}
		}
		for _, currentURL := range urls {
			if isXURL(currentURL) {
				// X home/timeline pages embed a full composer, so URL-path checks
				// and element labels cannot distinguish an ordinary click/type from
				// a one-call publish sequence. Keep CDP-backed X access read-only.
				return &playwrightPreDispatchGuardError{result: xPageMutationBlockedResult()}
			}
		}
		return nil
	}, agent.ToolResult{}
}

func playwrightMutationCanPublish(name string) bool {
	switch name {
	case "browser_click", "browser_type", "browser_press_key",
		"browser_drag", "browser_select_option", "browser_file_upload",
		"browser_fill_form":
		return true
	default:
		return false
	}
}

// outcomeUnknownResultMessage renders a post-dispatch transport failure for
// the model. The wording is load-bearing: an error result's default read is
// "the operation did not happen", and for this failure that read is exactly
// what produces duplicate side effects — the model must be steered to verify
// before re-issuing the call, without being forbidden from recovering.
func outcomeUnknownResultMessage(e *mcp.OutcomeUnknownError) string {
	return fmt.Sprintf(
		"MCP call outcome UNKNOWN: %s/%s was dispatched, but the connection to the server died before a response arrived (%v). "+
			"The operation may or may not have taken effect — do not assume it failed. "+
			"If this tool has side effects, verify the outcome first (e.g. with a read-only list/get/search call) and only retry if the effect is confirmed absent; a blind retry can execute the operation twice.",
		e.Server, e.Tool, e.Err)
}

func (t *MCPTool) RequiresApproval() bool { return false }

// ToolSource implements agent.ToolSourcer for deterministic tool ordering.
func (t *MCPTool) ToolSource() agent.ToolSource { return agent.SourceMCP }

// ServerName returns the MCP server this tool belongs to, for per-agent MCP
// scoping (tools.ApplyMCPServerScope).
func (t *MCPTool) ServerName() string { return t.serverName }

// maybeRewriteFileProducingArg rewrites the first relative output-path arg
// (per fileProducingMCPArgs) to an absolute path under the session CWD. It
// mutates args in place and returns the rewritten absolute path, or "" when
// no rewrite happened (unknown tool, no session CWD, already absolute, arg
// missing, or arg has an unexpected type). This is a best-effort helper —
// a failed rewrite is never fatal; the call continues with original args.
func maybeRewriteFileProducingArg(ctx context.Context, serverName, toolName string, args map[string]any) string {
	key := serverName + "/" + toolName
	argNames, ok := fileProducingMCPArgs[key]
	if !ok {
		return ""
	}
	// Target directory precedence: artifact scratch dir (daemon-served runs;
	// keeps machine-generated intermediates out of user-visible folders like
	// ~/Desktop) > session CWD (TUI / one-shot CLI, where artifacts belong in
	// the working directory). Absolute model-supplied paths always win below.
	targetDir := cwdctx.ArtifactDirFromContext(ctx)
	if targetDir == "" || !filepath.IsAbs(targetDir) {
		targetDir = cwdctx.FromContext(ctx)
	}
	if targetDir == "" || !filepath.IsAbs(targetDir) {
		return ""
	}
	// Scratch-internal dirs stay owner-only to match the 0o700 scratch root;
	// non-scratch targets (session CWD fallback) keep conventional 0o755.
	dirPerm := os.FileMode(0o755)
	if artifact := cwdctx.ArtifactDirFromContext(ctx); artifact != "" && targetDir == artifact {
		dirPerm = 0o700
	}
	// Missing output filename: the server would pick its own default
	// location and report a path relative to its own workspace — the
	// ambiguity behind the 2026-07-29 whole-disk search. Inject an absolute
	// name in the artifact dir instead. Guards: only when an artifact dir is
	// present (CWD-only runs keep the legacy no-injection behavior — the
	// server default + result translation cover them), only for tools with a
	// default (see defaultOutputName), and only when EVERY output arg is
	// absent — if the caller supplied any of them, injecting a sibling would
	// silently redirect output the model already addressed.
	anyPresent := false
	for _, name := range argNames {
		if _, present := args[name]; present {
			anyPresent = true
			break
		}
	}
	if !anyPresent {
		if cwdctx.ArtifactDirFromContext(ctx) == "" {
			return ""
		}
		def := defaultOutputName(key, args)
		if def == "" {
			return ""
		}
		abs := filepath.Join(targetDir, def)
		// The scratch dir is created lazily here (0o700 to match its
		// intended root perms); playwright-mcp does not mkdir for us.
		_ = os.MkdirAll(filepath.Dir(abs), dirPerm)
		// argNames is priority-ordered; the first name is the canonical
		// output arg for the tool.
		args[argNames[0]] = abs
		return abs
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
			// Best-effort parent creation: playwright-mcp does not create
			// missing directories and fails with ENOENT.
			_ = os.MkdirAll(filepath.Dir(expanded), 0o755)
			args[name] = expanded
			return expanded
		}
		if filepath.IsAbs(trimmed) {
			continue
		}
		// Reject anything that tries to climb out of the target dir. Keeping
		// the rewrite inside the sandbox avoids accidentally aiming the MCP
		// server at (say) ~/.ssh. Also reject values that resolve to the
		// target dir itself (".", "./", trailing ".."): the MCP server needs
		// a real filename, and passing the directory path would produce
		// malformed artifacts. On reject we fall through (empty return); the
		// original relative value still goes to the server, which will use its
		// own CWD — behavior unchanged from pre-fix for that edge case.
		abs := filepath.Clean(filepath.Join(targetDir, trimmed))
		if abs == targetDir {
			continue
		}
		if !strings.HasPrefix(abs+string(filepath.Separator), targetDir+string(filepath.Separator)) {
			continue
		}
		// Parent creation fixes the 2026-07-29 ENOENT on nested names like
		// ".playwright-mcp/snapshot.md" — the server does not mkdir for us.
		_ = os.MkdirAll(filepath.Dir(abs), dirPerm)
		args[name] = abs
		return abs
	}
	return ""
}

// defaultOutputName returns the filename injected when the model omits the
// output arg of a file-producing MCP tool, or "" for tools without a
// default. The millisecond timestamp keeps repeated captures from
// overwriting each other.
//
// Only tools that ALWAYS write a file belong here — for them the filename
// controls location only. browser_snapshot is deliberately absent: its
// filename is a MODE switch (playwright: "Save snapshot to markdown file
// instead of returning it in the response"), and an omitted filename means
// the inline accessibility snapshot the model reads pages through. Injecting
// a default there would add a file round-trip to every page read.
func defaultOutputName(key string, args map[string]any) string {
	var stem, ext string
	switch key {
	case "playwright/browser_take_screenshot":
		stem, ext = "screenshot", ".png"
		if t, _ := args["type"].(string); strings.EqualFold(t, "jpeg") {
			ext = ".jpeg"
		}
	default:
		return ""
	}
	return stem + "-" + time.Now().Format("20060102-150405.000") + ext
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
