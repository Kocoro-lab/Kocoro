package daemon

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/agents"
	"github.com/Kocoro-lab/ShanClaw/internal/audit"
	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/cloudflow"
	"github.com/Kocoro-lab/ShanClaw/internal/config"
	"github.com/Kocoro-lab/ShanClaw/internal/cwdctx"
	"github.com/Kocoro-lab/ShanClaw/internal/hooks"
	"github.com/Kocoro-lab/ShanClaw/internal/mcp"
	"github.com/Kocoro-lab/ShanClaw/internal/memory"
	"github.com/Kocoro-lab/ShanClaw/internal/runstatus"
	"github.com/Kocoro-lab/ShanClaw/internal/schedule"
	"github.com/Kocoro-lab/ShanClaw/internal/session"
	"github.com/Kocoro-lab/ShanClaw/internal/skills"
	"github.com/Kocoro-lab/ShanClaw/internal/tools"
)

var (
	disconnectPlaywrightAfterIdleFn = func(mgr *mcp.ClientManager, d time.Duration) {
		mgr.DisconnectAfterIdle("playwright", d)
	}
	disconnectPlaywrightNowFn = func(mgr *mcp.ClientManager) {
		mgr.Disconnect("playwright")
	}
	stopPlaywrightChromeFn = mcp.StopCDPChrome
)

// RequestContentBlock represents a content block in the POST /message request.
// Supported types: "text" and "image" (passed through to LLM), "file_ref" (resolved by daemon).
type RequestContentBlock struct {
	Type     string              `json:"type"`
	Text     string              `json:"text,omitempty"`
	Source   *client.ImageSource `json:"source,omitempty"`
	FilePath string              `json:"file_path,omitempty"`
	Filename string              `json:"filename,omitempty"`
	ByteSize int64               `json:"byte_size,omitempty"`
}

// RunAgentRequest is the input for RunAgent.
type RunAgentRequest struct {
	Text           string                `json:"text"`
	Content        []RequestContentBlock `json:"content,omitempty"` // multimodal content blocks (optional)
	Agent          string                `json:"agent,omitempty"`
	SessionID      string                `json:"session_id,omitempty"`
	NewSession     bool                  `json:"new_session,omitempty"`
	Source         string                `json:"source,omitempty"`    // "slack", "line", "shanclaw", "webhook"
	Sender         string                `json:"sender,omitempty"`    // user identifier from channel
	Channel        string                `json:"channel,omitempty"`   // channel/thread source context
	ThreadID       string                `json:"thread_id,omitempty"` // thread context for messaging platforms
	CWD            string                `json:"cwd,omitempty"`       // absolute project path override
	RouteKey       string                `json:"-"`                   // internal routing key
	Ephemeral      bool                  `json:"-"`                   // caller owns persistence + events
	ModelOverride  string                `json:"-"`                   // overrides agent model tier
	BypassRouting  bool                  `json:"-"`                   // skip route lock (heartbeat runs)
	SessionHistory []client.Message      `json:"-"`                   // pre-loaded history for LLM context (BypassRouting runs)
	StickyContext  string                `json:"-"`                   // 额外的 sticky context，注入系统提示（对用户不可见）
	Files          []RemoteFile          `json:"-"`                   // remote file attachments from Cloud (WS only)
}

// Validate checks that the request has the minimum required fields.
func (r *RunAgentRequest) Validate() error {
	if strings.TrimSpace(r.Text) == "" && len(r.Content) == 0 {
		return fmt.Errorf("text or content is required")
	}
	if r.Agent != "" {
		if err := agents.ValidateAgentName(r.Agent); err != nil {
			return err
		}
	}
	if r.CWD != "" {
		if err := cwdctx.ValidateCWD(r.CWD); err != nil {
			return fmt.Errorf("invalid cwd: %w", err)
		}
	}
	return nil
}

// ComputeRouteKey builds the route key for session cache/locking decisions.
func ComputeRouteKey(req RunAgentRequest) string {
	if req.BypassRouting {
		return ""
	}
	if req.Agent != "" {
		return "agent:" + req.Agent
	}
	if req.SessionID != "" {
		return "session:" + sanitizeRouteValue(req.SessionID)
	}
	if req.NewSession || shouldBypassRouteCache(req.Source) {
		return ""
	}
	if req.Source != "" && req.Channel != "" {
		return "default:" + sanitizeRouteValue(req.Source) + ":" + sanitizeRouteValue(req.Channel)
	}
	return ""
}

func shouldBypassRouteCache(source string) bool {
	switch strings.ToLower(strings.TrimSpace(source)) {
	case "", ChannelWeb, "webhook", "cron", ChannelSchedule, ChannelSystem:
		return true
	default:
		return false
	}
}

func sanitizeRouteValue(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return ""
	}
	return url.PathEscape(trimmed)
}

// resolveContentBlocks converts request content blocks into client.ContentBlock
// values suitable for the LLM. "text" and "image" blocks are passed through;
// "file_ref" blocks are resolved by reading the referenced file from disk.
func resolveContentBlocks(blocks []RequestContentBlock) []client.ContentBlock {
	out := make([]client.ContentBlock, 0, len(blocks))
	for _, b := range blocks {
		switch b.Type {
		case "text":
			out = append(out, client.ContentBlock{Type: "text", Text: b.Text})
		case "image":
			out = append(out, client.ContentBlock{Type: "image", Source: b.Source})
		case "document":
			out = append(out, client.ContentBlock{Type: "document", Source: b.Source})
		case "file_ref":
			out = append(out, resolveFileRef(b)...)
		}
	}
	return out
}

// imageExtensions are sent as base64 image content blocks to the LLM.
var imageExtensions = map[string]string{
	".jpg": "image/jpeg", ".jpeg": "image/jpeg",
	".png": "image/png", ".gif": "image/gif", ".webp": "image/webp",
}

// resolveFileRef returns the appropriate content blocks for a file_ref.
// Images → model-visible path hint plus base64 image block so the agent has
// both a reusable file handle and inline vision access.
// All other files → text hint with path so the agent reads via file_read tool.
func resolveFileRef(b RequestContentBlock) []client.ContentBlock {
	ext := strings.ToLower(filepath.Ext(b.Filename))

	// Images must be inline base64 — Claude vision requires image data in the request body.
	if mimeType, ok := imageExtensions[ext]; ok {
		info, err := os.Stat(b.FilePath)
		if err != nil {
			log.Printf("WARNING: failed to read attached image %s: %v", b.FilePath, err)
			return []client.ContentBlock{{
				Type: "text",
				Text: fmt.Sprintf("[Error: unable to read image %s]", b.Filename),
			}}
		}
		const maxInlineImage = 20 * 1024 * 1024 // 20 MB
		if info.Size() > maxInlineImage {
			return []client.ContentBlock{{
				Type: "text",
				Text: fmt.Sprintf("[User attached image: %s (%d bytes) at path: %s — too large for inline vision (max %d bytes). Use file_read or another file-based tool with this path.]",
					b.Filename, info.Size(), b.FilePath, maxInlineImage),
			}}
		}
		data, err := os.ReadFile(b.FilePath)
		if err != nil {
			log.Printf("WARNING: failed to read attached image %s: %v", b.FilePath, err)
			return []client.ContentBlock{{
				Type: "text",
				Text: fmt.Sprintf("[Error: unable to read image %s]", b.Filename),
			}}
		}
		encoded := base64.StdEncoding.EncodeToString(data)
		return []client.ContentBlock{
			{
				Type: "text",
				Text: fmt.Sprintf("[User attached image: %s (%d bytes) at path: %s — the image is included inline below for vision. Use the path if a tool needs the original file.]",
					b.Filename, info.Size(), b.FilePath),
			},
			{
				Type:   "image",
				Source: &client.ImageSource{Type: "base64", MediaType: mimeType, Data: encoded},
			},
		}
	}

	// PDF files: file_read natively renders PDF pages as images for vision.
	if ext == ".pdf" {
		return []client.ContentBlock{{
			Type: "text",
			Text: fmt.Sprintf("[User attached PDF: %s (%d bytes) at path: %s — use file_read to analyze (it renders PDF pages as images for vision). Use offset for start page, limit for max pages.]",
				b.Filename, b.ByteSize, b.FilePath),
		}}
	}

	// All other files: let the agent use file_read to access content on demand.
	return []client.ContentBlock{{
		Type: "text",
		Text: fmt.Sprintf("[User attached file: %s (%d bytes) at path: %s — use the file_read tool to read its contents]",
			b.Filename, b.ByteSize, b.FilePath),
	}}
}

// extractUserFilePaths collects file paths from file_ref content blocks.
// These paths represent files the user explicitly attached, so tool access
// to them should be auto-approved without prompting.
func extractUserFilePaths(blocks []RequestContentBlock) []string {
	var paths []string
	for _, b := range blocks {
		if b.Type == "file_ref" && b.FilePath != "" {
			paths = append(paths, b.FilePath)
		}
	}
	return paths
}

// buildUserMsgContent creates the MessageContent for the user message.
// If resolved content contains non-text blocks (images), uses block array format.
// Otherwise, merges all text into a single string for maximum gateway compatibility.
func buildUserMsgContent(prompt string, resolvedContent []client.ContentBlock) client.MessageContent {
	if len(resolvedContent) == 0 {
		return client.NewTextContent(prompt)
	}

	// Check if any block requires array format (images, documents).
	needsBlocks := false
	for _, b := range resolvedContent {
		if b.Type != "text" {
			needsBlocks = true
			break
		}
	}

	if needsBlocks {
		blocks := resolvedContent
		if prompt != "" {
			blocks = append([]client.ContentBlock{{Type: "text", Text: prompt}}, blocks...)
		}
		return client.NewBlockContent(blocks)
	}

	// Text-only: merge into single string.
	merged := prompt
	for _, b := range resolvedContent {
		if b.Text != "" {
			merged += "\n\n" + b.Text
		}
	}
	return client.NewTextContent(merged)
}

// hasPDFAttachment returns true if any file_ref block has a .pdf extension.
func hasPDFAttachment(blocks []RequestContentBlock) bool {
	for _, b := range blocks {
		if b.Type == "file_ref" && strings.ToLower(filepath.Ext(b.Filename)) == ".pdf" {
			return true
		}
	}
	return false
}

// injectBundledSkill appends a bundled skill to the list if not already present.
func injectBundledSkill(existing []*skills.Skill, shannonDir, name string) []*skills.Skill {
	for _, s := range existing {
		if s.Name == name {
			return existing // already loaded
		}
	}
	src, err := skills.BundledSkillSource(shannonDir)
	if err != nil {
		log.Printf("daemon: failed to load bundled skill source for %q: %v", name, err)
		return existing
	}
	loaded, err := skills.LoadSkills(src)
	if err != nil {
		log.Printf("daemon: failed to load bundled skill %q: %v", name, err)
		return existing
	}
	for _, s := range loaded {
		if s.Name == name {
			return append(existing, s)
		}
	}
	return existing
}

// EnsureRouteKey computes and sets the route key if not already set.
func (req *RunAgentRequest) EnsureRouteKey() {
	if req == nil {
		return
	}
	if req.RouteKey == "" {
		req.RouteKey = ComputeRouteKey(*req)
	}
}

// outputFormatForSource maps a request source to an output format profile.
// Only explicit cloud-distributed channel sources use "plain" — Shannon Cloud
// handles final channel rendering for these (Slack mrkdwn, LINE Flex, etc.).
// Everything else (local, cron, schedule, web, unknown) defaults to "markdown".
//
// Shares its cloud-source definition with ensureCloudSessionTmpDir via
// isCloudSource; the two paths must agree on what "cloud-routed" means or the
// allocator and the formatter would drift apart silently.
func outputFormatForSource(source string) string {
	if isCloudSource(source) {
		return "plain"
	}
	return "markdown"
}

// IsMessagingPlatform returns true for sources where the gateway delivers
// an explicit AgentName (or empty = use the daemon's default agent) and any
// "@<botname>" prefix in the message body is user-facing convention, not an
// agent-routing signal.
//
// Callers (e.g. cmd/daemon.go) should skip the @mention parsing fallback for
// these sources — otherwise a literal "@<botname> hello" arriving from a
// group chat parses the bot's display name as a local agent, which won't
// exist in the daemon's registry and surfaces as a confusing error to the
// end user.
//
// NOTE: keep this set aligned with cloudSourceSet in session_cwd.go. The
// invariants differ (CWD allocation vs. @mention handling), but if a channel
// is cloud-routed it almost always belongs in both lists.
func IsMessagingPlatform(source string) bool {
	switch strings.ToLower(strings.TrimSpace(source)) {
	case ChannelSlack, ChannelFeishu, ChannelLark, ChannelWeCom,
		ChannelLINE, ChannelWeChat, ChannelTeams, ChannelDiscord,
		ChannelTelegram:
		return true
	}
	return false
}

// cacheSourceFromDaemonSource maps the daemon-level source (slack/webhook/
// cron/mcp/tui/...) to the cache_source string Shannon uses for prompt-cache
// TTL routing. Channel messages + interactive use → long bucket (1h). Fire-and-
// forget paths → short bucket (5m). See docs/cache-strategy.md.
//
// Unknown / unclassified sources deliberately fall through to "unknown" →
// Shannon routes unknown to 5m (fail cheap, not fail expensive).
func cacheSourceFromDaemonSource(source string) string {
	s := strings.ToLower(strings.TrimSpace(source))
	switch s {
	case "slack", "line", "feishu", "lark", "telegram":
		// Human-conversation channels: idle gaps > 5m are common, 1h pays off.
		return s
	case "tui", "shanclaw":
		// Interactive sessions: TUI and ShanClaw Desktop both have idle gaps >> 5m.
		return s
	case "cache_bench":
		// Synthetic benchmark traffic — treat as long-bucket so bench measures
		// reflect the production channel-message configuration.
		return "cache_bench"
	case "webhook", "cron", "schedule", "mcp":
		// One-shot paths — each invocation starts fresh, no resume.
		return s
	default:
		return "unknown"
	}
}

func routeTitle(source, channel, sender string) string {
	if source == "" {
		return ""
	}
	s := strings.ToLower(strings.TrimSpace(source))
	if s == "" {
		return ""
	}
	label := strings.ToUpper(s[:1]) + s[1:]

	// Use sender name when available (e.g. "Slack · Wayland")
	if sender != "" {
		return label + " · " + sender
	}
	// Fall back to channel if it differs from source (avoid "Slack slack")
	if channel != "" && strings.ToLower(channel) != s {
		return label + " · " + channel
	}
	return label
}

// RunAgentResult is the output from RunAgent.
type RunAgentResult struct {
	Reply     string        `json:"reply"`
	SessionID string        `json:"session_id"`
	Agent     string        `json:"agent"`
	Usage     RunAgentUsage `json:"usage"`
	// Partial=true + FailureCode indicate the run completed "softly" — the
	// reply is valid and should be shown, but the loop layer flagged it as
	// abnormal (e.g. loop-detector force-stop). Treat as a soft warning, not
	// an error.
	Partial     bool           `json:"partial,omitempty"`
	FailureCode runstatus.Code `json:"failure_code,omitempty"`
}

// RunAgentUsage tracks token and cost information for a single agent run.
type RunAgentUsage struct {
	InputTokens  int     `json:"input_tokens"`
	OutputTokens int     `json:"output_tokens"`
	TotalTokens  int     `json:"total_tokens"`
	CostUSD      float64 `json:"cost_usd"`
}

// ServerDeps holds shared dependencies required by both the WS callback
// and the HTTP server for running agent loops.
type ServerDeps struct {
	mu              sync.RWMutex // guards Config, Registry, Cleanup during reload
	Config          *config.Config
	GW              *client.GatewayClient
	Registry        *agent.ToolRegistry
	MCPManager      *mcp.ClientManager  // live MCP connections; swapped on reload
	Supervisor      *mcp.Supervisor     // MCP health supervisor; swapped on reload
	Cleanup         func()              // closes MCP connections; swapped on reload
	BaselineReg     *agent.ToolRegistry // local-only tools; refreshed on reload
	GatewayOverlay  []agent.Tool        // cached gateway tools; refreshed on reload
	PostOverlays    []agent.Tool        // cloud_delegate etc.; refreshed on reload
	ShannonDir      string
	AgentsDir       string
	Auditor         *audit.AuditLogger
	HookRunner      *hooks.HookRunner
	SessionCache    *SessionCache
	EventBus        *EventBus
	ScheduleManager *schedule.Manager
	WSClient        *Client              // WebSocket client for proactive messages
	SecretsStore    *skills.SecretsStore // skill secrets for env injection
	MemSvc          *memory.Service      // structured memory orchestrator (Phase 2.3)
}

// Snapshot returns current Config, Registry, and Supervisor under read lock.
// Callers use the returned values without holding the lock.
func (d *ServerDeps) Snapshot() (*config.Config, *agent.ToolRegistry, *mcp.Supervisor) {
	d.mu.RLock()
	cfg, reg, sup := d.Config, d.Registry, d.Supervisor
	d.mu.RUnlock()
	return cfg, reg, sup
}

// ShutdownCleanup captures and calls the current Cleanup function under lock,
// preventing races with concurrent reload swaps.
func (d *ServerDeps) ShutdownCleanup() {
	d.mu.Lock()
	cleanup := d.Cleanup
	d.Cleanup = nil
	d.mu.Unlock()
	if cleanup != nil {
		cleanup()
	}
}

// WriteLock acquires the write lock on ServerDeps. Used by daemon event
// handler to update in-memory config (e.g., always-allow persistence).
func (d *ServerDeps) WriteLock()   { d.mu.Lock() }
func (d *ServerDeps) WriteUnlock() { d.mu.Unlock() }

// RebuildLayers returns the cached rebuild layers under read lock.
func (d *ServerDeps) RebuildLayers() (*agent.ToolRegistry, []agent.Tool, []agent.Tool, *mcp.ClientManager) {
	d.mu.RLock()
	bl, gw, po, mgr := d.BaselineReg, d.GatewayOverlay, d.PostOverlays, d.MCPManager
	d.mu.RUnlock()
	return bl, gw, po, mgr
}

func cleanupPlaywrightAfterTurn(mgr *mcp.ClientManager) {
	if mgr == nil {
		return
	}
	cfg, ok := mgr.ConfigFor("playwright")
	if !ok || cfg.KeepAlive {
		return
	}
	if mcp.IsPlaywrightCDPMode(cfg) {
		disconnectPlaywrightNowFn(mgr)
		stopPlaywrightChromeFn()
		log.Printf("daemon: Playwright on-demand teardown completed")
		return
	}
	disconnectPlaywrightAfterIdleFn(mgr, 5*time.Minute)
	log.Printf("daemon: Playwright idle disconnect scheduled (5m)")
}

// resumeNamedAgentColdStart resumes the latest persisted named-agent session.
// Returns true only when a session was actually loaded from disk; a fresh
// in-memory session pre-created by the route manager does not count as resumed.
func resumeNamedAgentColdStart(sessMgr *session.Manager) (bool, error) {
	latest, err := sessMgr.ResumeLatest()
	if err != nil {
		return false, err
	}
	if latest != nil {
		return true, nil
	}
	if sessMgr.Current() == nil {
		sessMgr.NewSession()
	}
	return false, nil
}

// RunAgent executes a single agent turn using the shared dependencies.
// The caller provides an EventHandler to control streaming, approval, and
// event reporting (WS uses daemonEventHandler, HTTP uses httpEventHandler).
func RunAgent(ctx context.Context, deps *ServerDeps, req RunAgentRequest, handler agent.EventHandler) (*RunAgentResult, error) {
	// Phase 1: read supervisor atomically, probe if needed
	cfg, _, sup := deps.Snapshot()
	if cfg == nil || deps.GW == nil || deps.SessionCache == nil {
		return nil, fmt.Errorf("daemon not fully configured")
	}
	if sup != nil {
		// Cancel any pending idle disconnect — a new turn is starting.
		if _, _, _, mgr := deps.RebuildLayers(); mgr != nil {
			mgr.CancelIdleDisconnect("playwright")
		}
		// Only probe+reconnect Playwright when it's not already disconnected.
		// When the user closes Chrome, the periodic probe marks it Disconnected.
		// Calling ProbeNow on a Disconnected server triggers attemptReconnect,
		// which relaunches Chrome — disruptive if the task doesn't need browser tools.
		if h := sup.HealthFor("playwright"); h.State != mcp.StateDisconnected {
			sup.ProbeNow("playwright")
		}
	}
	// Phase 2: re-snapshot to get post-swap registry
	cfg, baseReg, _ := deps.Snapshot()
	if baseReg == nil {
		return nil, fmt.Errorf("daemon not fully configured")
	}
	agentName := req.Agent
	prompt := req.Text

	// Download remote file attachments and convert to file_ref blocks.
	// Attachment files must survive across turns (non-image files become
	// file_read hints in session history). Cleanup uses sessMgr.OnClose
	// (append-style, fires on manager close) — not OnSessionClose (which
	// replaces per-session and would clobber previous turns' cleanup).
	// The defer is a safety net for early-return errors before sessMgr
	// is available; it's cancelled once OnClose takes ownership.
	var attachmentCleanup func()
	var attachmentRegistered bool
	defer func() {
		if !attachmentRegistered && attachmentCleanup != nil {
			attachmentCleanup()
		}
	}()
	if len(req.Content) > 0 {
		var inlineCleanup func()
		req.Content, inlineCleanup = materializeInlineImageBlocks(deps.ShannonDir, req.Content)
		attachmentCleanup = combineCleanup(attachmentCleanup, inlineCleanup)
	}
	if len(req.Files) > 0 {
		var fileBlocks []RequestContentBlock
		var remoteCleanup func()
		fileBlocks, remoteCleanup = downloadRemoteFiles(deps.ShannonDir, req.Files)
		attachmentCleanup = combineCleanup(attachmentCleanup, remoteCleanup)
		req.Content = append(req.Content, fileBlocks...)
		// Zero auth headers to prevent lingering tokens in memory.
		for i := range req.Files {
			req.Files[i].AuthHeader = ""
		}
	}

	// Resolve multimodal content blocks (if present).
	var resolvedContent []client.ContentBlock
	if len(req.Content) > 0 {
		resolvedContent = resolveContentBlocks(req.Content)
	}

	// "default" is not a real agent — it means "use base agent, no --agent flag".
	if agentName == "default" {
		agentName = ""
	}
	req.Agent = agentName
	explicitAgent := agentName != "" // explicitly requested, not parsed from @mention

	// Parse @mention if no explicit agent was provided.
	if agentName == "" {
		agentName, prompt = agents.ParseAgentMention(req.Text)
	}
	if prompt == "" {
		prompt = req.Text
	}

	var agentOverride *agents.Agent
	if agentName != "" {
		a, loadErr := agents.LoadAgent(deps.AgentsDir, agentName)
		if loadErr != nil {
			if explicitAgent {
				return nil, fmt.Errorf("agent not found: %s", agentName)
			}
			// @mention fallback: use default agent
			log.Printf("daemon: agent %q not found: %v, using default", agentName, loadErr)
			agentName = ""
			prompt = req.Text
		} else {
			agentOverride = a
		}
	}
	// Resolve agent-scoped slash command: "/cmd-name args" → command content.
	if agentOverride != nil && strings.HasPrefix(prompt, "/") {
		parts := strings.Fields(prompt)
		cmdName := strings.TrimPrefix(parts[0], "/")
		if content, ok := agentOverride.Commands[cmdName]; ok {
			args := ""
			if len(parts) > 1 {
				args = strings.Join(parts[1:], " ")
			}
			prompt = strings.ReplaceAll(content, "$ARGUMENTS", args)
		}
	}
	req.Text = prompt
	// Recompute route key after final agent resolution.
	// Callers may precompute a default/source-channel key before @mention parsing.
	// Recomputing here avoids cross-route contamination.
	req.RouteKey = ComputeRouteKey(req)

	sessionsDir := deps.SessionCache.SessionsDir(agentName)
	var sessMgr *session.Manager

	var route *routeEntry
	var routeDone chan struct{}
	var routeInjectCh chan agent.InjectedMessage
	// Empty route key = no cache entry for routing, always start a fresh local session.
	if req.RouteKey != "" {
		route = deps.SessionCache.LockRouteWithManager(req.RouteKey, sessionsDir)
		sessMgr = route.manager
		reqCtx, cancel := context.WithCancel(ctx)
		routeDone = make(chan struct{})
		routeInjectCh = make(chan agent.InjectedMessage, 10)
		deps.SessionCache.SetRouteRunState(req.RouteKey, routeDone, nil, "")
		ctx = reqCtx
		// Register cancel under sc.mu so CancelRoute sees it immediately.
		// Also fires cancel right away if CancelRoute already set cancelPending.
		deps.SessionCache.SetRouteCancel(req.RouteKey, cancel)
		defer func() {
			deps.SessionCache.ClearRouteRunState(req.RouteKey)
			closeRouteDone(routeDone)
			route.cancel = nil
			// Set sessionID directly — do NOT call SetRouteSessionID which
			// would try to acquire route.mu again (same deadlock).
			if current := sessMgr.Current(); current != nil {
				route.sessionID = current.ID
			}
			deps.SessionCache.UnlockRoute(req.RouteKey)
		}()
	} else {
		managerDir := sessionsDir
		if req.BypassRouting {
			tmpDir, tmpErr := os.MkdirTemp("", "heartbeat-*")
			if tmpErr != nil {
				return nil, fmt.Errorf("create temp session dir: %w", tmpErr)
			}
			defer os.RemoveAll(tmpDir)
			managerDir = tmpDir
		}
		sessMgr = session.NewManager(managerDir)
		defer func() {
			if err := sessMgr.Close(); err != nil {
				log.Printf("daemon: failed to close ephemeral session manager for %q: %v", managerDir, err)
			}
		}()
	}

	resumed := false
	switch {
	case req.SessionID != "":
		// Resume a specific session by ID (reuses cached manager to avoid DB handle leak).
		if _, err := sessMgr.Resume(req.SessionID); err != nil {
			return nil, fmt.Errorf("session not found: %s", req.SessionID)
		}
		resumed = true
	case req.NewSession || req.RouteKey == "":
		sessMgr.NewSession()
	case route != nil && route.sessionID != "":
		if _, err := sessMgr.Resume(route.sessionID); err != nil {
			log.Printf("daemon: failed to resume routed session %q for %q: %v", route.sessionID, req.RouteKey, err)
			sessMgr.NewSession()
		} else {
			resumed = true
		}
	case strings.HasPrefix(req.RouteKey, "agent:"):
		// Named-agent cold start (first run or after daemon restart).
		// route.sessionID is empty — resume latest from disk, or start fresh if none.
		if resumedLatest, err := resumeNamedAgentColdStart(sessMgr); err != nil {
			log.Printf("daemon: failed to resume latest named-agent session for %q: %v", req.RouteKey, err)
			if sessMgr.Current() == nil {
				sessMgr.NewSession()
			}
		} else {
			resumed = resumedLatest
		}
	default:
		sessMgr.NewSession()
	}
	sess := sessMgr.Current()

	// Seed pre-loaded history for bypass-routed runs (e.g., heartbeat).
	// The throwaway manager has an empty session; this gives the LLM context.
	if len(req.SessionHistory) > 0 {
		sess.Messages = req.SessionHistory
	}

	// Resolve effective CWD: request > resumed session > agent config. When all
	// three are empty we deliberately do NOT invent a working directory for
	// most sources — the request runs with no filesystem scope, and filesystem
	// tools (glob, grep, file_read, directory_list) will refuse any relative
	// paths at the tool level. Web-only and pure-reasoning tasks are unaffected.
	//
	// Cloud-routed sources (slack/line/feishu/lark/telegram/webhook) are the
	// one exception: they arrive with no user shell and no persisted CWD, so a
	// tool like browser_snapshot(filename="x.md") has nowhere to land and
	// file_read("x.md") can't resolve it. For those we allocate a per-session
	// scratch dir under ~/.shannon/tmp/sessions/<id>/ as the lowest-priority
	// fallback. Any real CWD (request/resumed/agent) still wins.
	var sessionCWD string
	if resumed {
		sessionCWD = sess.CWD
	}
	var agentCWD string
	if agentOverride != nil && agentOverride.Config != nil {
		agentCWD = agentOverride.Config.CWD
	}
	effectiveCWD := cwdctx.ResolveEffectiveCWD(req.CWD, sessionCWD, agentCWD)
	var cloudSessionCWD string
	if effectiveCWD == "" {
		if dir, err := ensureCloudSessionTmpDir(deps.ShannonDir, sess.ID, req.Source); err != nil {
			log.Printf("daemon: failed to allocate cloud session cwd for %s: %v", sess.ID, err)
		} else if dir != "" {
			cloudSessionCWD = dir
			effectiveCWD = dir
		}
	}
	if effectiveCWD != "" {
		if err := cwdctx.ValidateCWD(effectiveCWD); err != nil {
			return nil, fmt.Errorf("invalid cwd: %w", err)
		}
	}
	if req.RouteKey != "" {
		deps.SessionCache.SetRouteRunState(req.RouteKey, routeDone, routeInjectCh, effectiveCWD)
	}
	runCfg, err := config.RuntimeConfigForCWD(cfg, effectiveCWD)
	if err != nil {
		return nil, fmt.Errorf("runtime config: %w", err)
	}
	// Only write back when we have a real CWD — avoid poisoning the session
	// with an empty value and avoid overwriting an existing non-empty session
	// CWD with an empty fallback. Cloud scratch dirs are deliberately NOT
	// persisted: they live under ~/.shannon/tmp/sessions/<id>/, get removed
	// on session close, and must be re-allocated on every resume. Persisting
	// them would leave sess.CWD pointing at a now-deleted path, and the next
	// run would fail ValidateCWD before it could recreate the scratch.
	if effectiveCWD != "" && cloudSessionCWD == "" {
		sess.CWD = effectiveCWD
	}
	ctx = cwdctx.WithSessionCWD(ctx, effectiveCWD)

	// Wrap the transport handler with a bus-emitting handler so every run
	// publishes progress events regardless of transport. See
	// docs/superpowers/specs/2026-04-23-event-bus-progress-coverage-design.md.
	bus := &busEventHandler{deps: deps, agent: agentName}
	handler = &multiHandler{handlers: []agent.EventHandler{handler, bus}}

	// Notify handler of resolved session ID so it can include it in EventBus payloads.
	if setter, ok := handler.(interface{ SetSessionID(string) }); ok {
		setter.SetSessionID(sess.ID)
	}

	// Route notify tool calls through the EventBus so attached SSE clients
	// (typically the Desktop app) render the banner via UNUserNotificationCenter
	// with correct app attribution and click-through routing. Falls back to
	// the direct osascript path only when EmitTo reports zero deliveries —
	// either because no client is subscribed, or because every subscriber's
	// buffer was full. Using EmitTo's delivery count (rather than a liveness
	// check) means a single stalled subscriber cannot swallow notifications
	// into a silent void.
	if deps.EventBus != nil {
		sessID := sess.ID
		notifyAgent := agentName
		notifySource := req.Source
		ctx = tools.WithNotifyHandler(ctx, func(title, body string, sound bool) bool {
			payload, _ := json.Marshal(map[string]any{
				"session_id": sessID,
				"agent":      notifyAgent,
				"source":     notifySource,
				"title":      title,
				"body":       body,
				"sound":      sound,
			})
			return deps.EventBus.EmitTo(Event{Type: EventNotification, Payload: payload}) > 0
		})
	}

	// Persist session to disk before loop.Run() so there's a record even if
	// the daemon crashes mid-execution. The final save after completion is
	// still needed to capture the assistant's reply.
	// Ephemeral requests skip persistence — the caller owns session lifecycle.
	if !req.Ephemeral {
		if req.Source != "" && req.Channel != "" {
			sess.Source = req.Source
			sess.Channel = req.Channel
		}
		// Only set source-derived title for non-named-agent routes.
		// Named agents always get session.AgentTitle in the post-loop block.
		if sess.Title == "New session" && req.RouteKey != "" && !strings.HasPrefix(req.RouteKey, "agent:") {
			title := routeTitle(req.Source, req.Channel, req.Sender)
			if title != "" {
				sess.Title = title
			}
		}
		if err := sessMgr.Save(); err != nil {
			log.Printf("daemon: failed to pre-save session: %v", err)
		}
	}

	// Snapshot history BEFORE appending the user message so loop.Run(prompt, history)
	// does not receive the user message twice (once as prompt, once in history).
	// HistoryForLoop strips prior loop-injected guardrail nudges (MessageMeta
	// .SystemInjected) so they cannot leak into the current run's conversation
	// snapshot — see session.Session.HistoryForLoop for the full rationale.
	history := sess.HistoryForLoop()

	// For externally-sourced messages (Slack, LINE, etc.), persist the user message
	// before the agent loop so the UI can display it immediately on notification.
	// preLoopUserAppended tracks the in-memory append (not save success) to prevent
	// double-appending in the post-loop persist block.
	userMsgTime := time.Now()
	var preLoopUserAppended bool
	if !req.Ephemeral && req.Source != "" {
		source := req.Source
		if source == "" {
			source = "unknown"
		}
		msgID := generateMessageID()
		userMsgContent := buildUserMsgContent(prompt, resolvedContent)
		sess.Messages = append(sess.Messages,
			client.Message{Role: "user", Content: userMsgContent},
		)
		sess.MessageMeta = append(sess.MessageMeta,
			session.MessageMeta{Source: source, MessageID: msgID, Timestamp: session.TimePtr(userMsgTime)},
		)
		preLoopUserAppended = true
		if err := sessMgr.Save(); err != nil {
			log.Printf("daemon: failed to pre-save user message: %v", err)
		} else if deps.EventBus != nil {
			payload, _ := json.Marshal(map[string]any{
				"agent":      agentName,
				"source":     req.Source,
				"sender":     req.Sender,
				"session_id": sess.ID,
				"message_id": msgID,
				"text":       prompt,
			})
			deps.EventBus.Emit(Event{Type: EventMessageReceived, Payload: payload})
		}
	}

	// Clone and apply per-agent tool filter
	reg := tools.CloneWithRuntimeConfig(baseReg, runCfg)
	if agentOverride != nil {
		reg = tools.ApplyToolFilter(reg, agentOverride)
	}

	// Attach SecretsStore to the session-scoped bash tool so use_skill
	// activations can expose skill secrets as child-process env vars.
	// Baseline bash is created at daemon start before NewServer, so the
	// store has to be wired here, after CloneWithRuntimeConfig has
	// deep-copied bash for this run.
	if deps.SecretsStore != nil {
		if bashTool, ok := reg.Get("bash"); ok {
			if bt, ok := bashTool.(*tools.BashTool); ok {
				bt.SecretsStore = deps.SecretsStore
			}
		}
	}

	// Load skills (agent-scoped or global) and wire to registry
	var loadedSkills []*skills.Skill
	if agentOverride != nil {
		loadedSkills = agentOverride.Skills
	} else {
		var err error
		loadedSkills, err = agents.LoadGlobalSkills(deps.ShannonDir)
		if err != nil {
			log.Printf("WARNING: failed to load global skills: %v", err)
		}
	}

	// Auto-inject bundled skills based on attached file types.
	if hasPDFAttachment(req.Content) {
		loadedSkills = injectBundledSkill(loadedSkills, deps.ShannonDir, "pdf-reader")
	}

	tools.SetRegistrySkills(reg, loadedSkills)

	// Always expose local session search for daemon-served agents.
	// Use the per-agent manager so searches are scoped to that agent's sessions.
	tools.RegisterSessionSearch(reg, sessMgr)

	// memory_recall — talks to the structured memory sidecar when ready and
	// falls back to session keyword search + MEMORY.md grep otherwise. Always
	// register; the tool itself decides whether to use the service or fallback
	// based on the service's Status().
	var memSvc tools.MemoryQuerier
	if deps.MemSvc != nil {
		memSvc = deps.MemSvc
	}
	tools.RegisterMemoryTool(reg, memSvc, &daemonFallback{sessionMgr: sessMgr})

	loop := agent.NewAgentLoop(deps.GW, reg, runCfg.ModelTier, deps.ShannonDir,
		runCfg.Agent.MaxIterations, runCfg.Tools.ResultTruncation, runCfg.Tools.ArgsTruncation,
		&runCfg.Permissions, deps.Auditor, deps.HookRunner)
	loop.SetMaxTokens(runCfg.Agent.MaxTokens)
	loop.SetTemperature(runCfg.Agent.Temperature)
	loop.SetContextWindow(runCfg.Agent.ContextWindow)
	loop.SetEnableStreaming(false)
	loop.SetDeltaProvider(agent.NewTemporalDelta())
	loop.SetCacheSource(cacheSourceFromDaemonSource(req.Source))
	loop.SetSkillDiscovery(runCfg.Agent.SkillDiscoveryEnabled())
	loop.SetTimeBasedCompactConfig(agent.TimeBasedCompactConfig{
		Enabled:             runCfg.Agent.TimeBasedCompact.Enabled,
		GapThresholdMinutes: runCfg.Agent.TimeBasedCompact.GapThresholdMinutes,
		KeepRecent:          runCfg.Agent.TimeBasedCompact.KeepRecent,
	})
	if agentOverride != nil {
		scopedMCPCtx := tools.ResolveMCPContext(runCfg, agentOverride)
		agentDir := filepath.Join(deps.ShannonDir, "agents", agentName)
		loop.SwitchAgent(agentOverride.Prompt, agentDir, nil, scopedMCPCtx, loadedSkills)
	} else {
		loop.SetMemoryDir(filepath.Join(deps.ShannonDir, "memory"))
		if loadedSkills != nil {
			loop.SetSkills(loadedSkills)
		}
		scopedMCPCtx := tools.ResolveMCPContext(runCfg)
		if scopedMCPCtx != "" {
			loop.SetMCPContext(scopedMCPCtx)
		}
	}
	if runCfg.Agent.Model != "" {
		loop.SetSpecificModel(runCfg.Agent.Model)
	}
	if runCfg.Agent.Thinking {
		if runCfg.Agent.ThinkingMode == "enabled" {
			loop.SetThinking(&client.ThinkingConfig{Type: "enabled", BudgetTokens: runCfg.Agent.ThinkingBudget})
		} else {
			loop.SetThinking(&client.ThinkingConfig{Type: "adaptive"})
		}
	}
	if runCfg.Agent.ReasoningEffort != "" {
		loop.SetReasoningEffort(runCfg.Agent.ReasoningEffort)
	}
	// Per-agent model config overrides
	if agentOverride != nil && agentOverride.Config != nil && agentOverride.Config.Agent != nil {
		ac := agentOverride.Config.Agent
		if ac.Model != nil {
			loop.SetSpecificModel(*ac.Model)
		}
		if ac.MaxIterations != nil {
			loop.SetMaxIterations(*ac.MaxIterations)
		}
		if ac.Temperature != nil {
			loop.SetTemperature(*ac.Temperature)
		}
		if ac.MaxTokens != nil {
			loop.SetMaxTokens(*ac.MaxTokens)
		}
		if ac.ContextWindow != nil {
			loop.SetContextWindow(*ac.ContextWindow)
		}
		if ac.IdleSoftTimeoutSecs != nil {
			runCfg.Agent.IdleSoftTimeoutSecs = *ac.IdleSoftTimeoutSecs
		}
		if ac.IdleHardTimeoutSecs != nil {
			runCfg.Agent.IdleHardTimeoutSecs = *ac.IdleHardTimeoutSecs
		}
	}
	// Apply idle-timeout config AFTER per-agent overrides have been folded
	// into runCfg, otherwise agent-level opt-in/override silently does nothing.
	loop.SetIdleTimeouts(runCfg.Agent.IdleSoftTimeoutSecs, runCfg.Agent.IdleHardTimeoutSecs)
	if req.ModelOverride != "" {
		loop.SetModelTier(req.ModelOverride)
	}
	// Inject session metadata as sticky context so it survives compaction.
	{
		var parts []string
		if req.Source != "" {
			parts = append(parts, "Source: "+req.Source)
		}
		if req.Channel != "" {
			parts = append(parts, "Channel: "+req.Channel)
		}
		if req.Sender != "" {
			parts = append(parts, "Sender: "+req.Sender)
		}
		if agentName != "" {
			parts = append(parts, "Agent: "+agentName)
		}
		if req.StickyContext != "" {
			parts = append(parts, req.StickyContext)
		}
		if len(parts) > 0 {
			loop.SetStickyContext(strings.Join(parts, "\n"))
		}
	}

	// Output format: cloud-distributed channels use "plain" (Shannon Cloud
	// handles final channel rendering). Local sources keep "markdown" (default).
	loop.SetOutputFormat(outputFormatForSource(req.Source))

	loop.SetHandler(handler)

	// Wire handler and agent context to the per-run cloud_delegate copy.
	// Must use reg (cloned), not baseReg (shared), to avoid race across routes.
	if ct, ok := reg.Get("cloud_delegate"); ok {
		if cdt, ok := ct.(*tools.CloudDelegateTool); ok {
			cdt.SetHandler(handler)
			if agentOverride != nil {
				cdt.SetAgentContext(agentName, agentOverride.Prompt)
			} else {
				cdt.SetAgentContext("", "")
			}
		}
	}

	if routeInjectCh != nil {
		loop.SetInjectCh(routeInjectCh)
	}
	loop.SetSessionID(sess.ID)
	loop.SetSessionCWD(effectiveCWD)
	loop.SetWorkingSet(sessMgr.WorkingSet(sess.ID))
	// Always set (even nil) to clear paths from a previous run on a reused loop.
	loop.SetUserFilePaths(extractUserFilePaths(req.Content))
	sessMgr.OnSessionClose(sess.ID, loop.SpillCleanupFunc())

	// file:// preview bridge: lazily-started loopback HTTP server that
	// rewrites browser_navigate(file://...) into http://127.0.0.1/<token>/…
	// so Playwright's Chromium deny-list doesn't strand the agent.
	//
	// Allowlist: the bridge only serves files already reachable by the
	// agent's other tools — the effective session CWD subtree plus any
	// explicit user-attached files. This prevents browser_navigate from
	// becoming an escape hatch that reads arbitrary local files outside
	// the normal file-access boundary.
	filePreview := tools.NewFilePreviewBridge()
	if effectiveCWD != "" {
		filePreview.AllowRoot(effectiveCWD)
	}
	for _, p := range extractUserFilePaths(req.Content) {
		filePreview.AllowFile(p)
	}
	sessMgr.OnSessionClose(sess.ID, func() { _ = filePreview.Close() })
	if cloudSessionCWD != "" {
		// Reclaim the per-session scratch dir when the session is closed
		// (SessionCache eviction, daemon shutdown). Artifacts live across turns
		// of the same session but don't accumulate across sessions.
		sessMgr.OnSessionClose(sess.ID, cloudSessionTmpCleanup(cloudSessionCWD))
	}
	ctx = tools.WithFilePreview(ctx, filePreview)
	if attachmentCleanup != nil {
		attachmentRegistered = true // cancel the defer safety net
		sessMgr.OnClose(attachmentCleanup)
	}

	// Turn persistence: capture the session state at turn start so both the
	// mid-turn checkpoint hook and the post-turn final save can rebuild
	// messages + usage idempotently from (baseline + current loop state).
	// This is the single source of truth — no append-on-top anywhere in
	// the turn's persistence path, which would otherwise double-write any
	// transcript that crossed a checkpoint boundary.
	checkpointSource := req.Source
	if checkpointSource == "" {
		checkpointSource = "unknown"
	}
	turnBase := captureTurnBaseline(sess, checkpointSource, preLoopUserAppended)
	// The daemon handler implements agent.UsageProvider; extract once so
	// callsites pass a strongly-typed provider (or nil) to applyTurnState.
	var turnUsage usageProvider
	if up, ok := handler.(agent.UsageProvider); ok {
		turnUsage = up
	}
	loop.SetCheckpointMinInterval(2 * time.Second) // debounce in the loop, not here
	loop.SetCheckpointFunc(func(ctx context.Context) error {
		applyTurnState(sess, loop, turnUsage, turnBase)
		sess.InProgress = true
		if err := sessMgr.Save(); err != nil {
			log.Printf("daemon: mid-turn checkpoint save failed: %v", err)
			// Return the error so AgentLoop.maybeCheckpoint keeps the
			// dirty flag set and the next fire point retries.
			return err
		}
		return nil
	})

	result, usage, runErr := loop.Run(ctx, prompt, resolvedContent, history)
	status := loop.LastRunStatus()
	if runErr != nil && !isSoftRunError(runErr) {
		// Hard error — save a user-friendly error message so the session isn't
		// left with a dangling user message and no assistant reply.
		// Full error detail goes to the log; session/UI gets a clean summary.
		log.Printf("daemon: agent %s run error: %v", agentName, runErr)
		if status.FailureCode == runstatus.CodeNone {
			status.FailureCode = runstatus.CodeFromError(runErr)
		}
		userErr := FriendlyAgentError(runErr)
		savedSessionID := ""
		if !req.Ephemeral && result == "" {
			// Use the same idempotent rebuild as the mid-turn checkpoint
			// and the normal final save: reset messages+usage to
			// (baseline + current snapshot), then append the friendly
			// error stub on top. This handles three previously-broken cases:
			//   (a) a prior checkpoint already persisted partial transcript
			//       — we must not duplicate it by appending the error on
			//       top of what's already there.
			//   (b) a dirty checkpoint was debounced just before the error
			//       — rebuilding from RunMessages picks up the trailing
			//       batches that never got their own save.
			//   (c) usage was already folded by a checkpoint — AddUsage
			//       would double-count, so use baseline+current instead.
			applyTurnMessages(sess, loop, turnBase)
			sess.Messages = append(sess.Messages,
				client.Message{Role: "assistant", Content: client.NewTextContent(userErr)},
			)
			sess.MessageMeta = append(sess.MessageMeta,
				session.MessageMeta{Source: req.Source, Timestamp: session.TimePtr(time.Now())},
			)
			applyTurnUsage(sess, turnUsage, turnBase)
			sess.InProgress = false // hard-error path: turn is over, clear marker
			if err := sessMgr.Save(); err != nil {
				log.Printf("daemon: failed to save error session: %v", err)
			} else {
				savedSessionID = sess.ID
			}
		}
		if deps.EventBus != nil {
			payload, _ := json.Marshal(map[string]any{
				"agent":          agentName,
				"source":         req.Source,
				"session_id":     savedSessionID,
				"error":          fmt.Sprintf("agent run failed: %v", runErr),
				"friendly_error": userErr,
				"failure_code":   status.FailureCode,
			})
			deps.EventBus.Emit(Event{Type: EventAgentError, Payload: payload})
		}
		return nil, fmt.Errorf("agent error for %s: %w", agentName, runErr)
	}
	if errors.Is(runErr, agent.ErrMaxIterReached) {
		log.Printf("daemon: agent %s hit iteration limit, saving partial result", agentName)
	}

	// Tracks persistence outcome so the return value can blank SessionID on
	// failure (in addition to the agent_reply gate inside the block below).
	// Stays nil for ephemeral requests, which is the desired "no failure" state.
	var saveErr error

	// Ephemeral requests skip post-run persistence — the caller owns session lifecycle.
	if !req.Ephemeral {
		// Set title from first user message (named agents get a fixed title).
		if sess.Title == "New session" {
			if agentName != "" {
				sess.Title = session.AgentTitle(agentName)
			} else {
				sess.Title = session.Title(prompt)
			}
		}

		// Final save uses the same (baseline + current snapshot) rebuild as
		// mid-turn checkpoints, so a turn that produced checkpoints never
		// gets its transcript double-written here.
		if len(loop.RunMessages()) > 0 {
			applyTurnMessages(sess, loop, turnBase)
		} else {
			// Fallback: flat text (early LLM error with nothing accumulated).
			// Truncate to baseline first so this path is also idempotent
			// under the (unusual) case where a prior checkpoint ran.
			if len(sess.Messages) > turnBase.msgCount {
				sess.Messages = sess.Messages[:turnBase.msgCount]
			}
			if len(sess.MessageMeta) > turnBase.metaCount {
				sess.MessageMeta = sess.MessageMeta[:turnBase.metaCount]
			}
			if !preLoopUserAppended {
				fallbackContent := buildUserMsgContent(prompt, resolvedContent)
				sess.Messages = append(sess.Messages,
					client.Message{Role: "user", Content: fallbackContent},
				)
				sess.MessageMeta = append(sess.MessageMeta,
					session.MessageMeta{Source: checkpointSource, Timestamp: session.TimePtr(userMsgTime)},
				)
			}
			replyTime := time.Now()
			sess.Messages = append(sess.Messages,
				client.Message{Role: "assistant", Content: client.NewTextContent(result)},
			)
			sess.MessageMeta = append(sess.MessageMeta,
				session.MessageMeta{Source: checkpointSource, Timestamp: session.TimePtr(replyTime)},
			)
		}
		applyTurnUsage(sess, turnUsage, turnBase) // idempotent: baseline + current
		sess.InProgress = false                   // turn completed — clear mid-turn crash marker
		saveErr = sessMgr.Save()
		if saveErr != nil {
			log.Printf("daemon: failed to save session: %v", saveErr)
			if deps.EventBus != nil {
				payload, _ := json.Marshal(map[string]any{
					"agent":        agentName,
					"source":       req.Source,
					"session_id":   sess.ID,
					"error":        fmt.Sprintf("session save failed: %v", saveErr),
					"failure_code": runstatus.CodeUnexpected,
				})
				deps.EventBus.Emit(Event{Type: EventAgentError, Payload: payload})
			}
		}

		// Only emit agent_reply when the session actually persisted. If the
		// save failed, the conversation is not on disk and downstream
		// consumers (e.g. desktop schedule notifications that click through
		// to the session) would point at a session that cannot be loaded.
		if saveErr == nil && deps.EventBus != nil {
			payload := map[string]any{
				"agent":      agentName,
				"source":     req.Source,
				"session_id": sess.ID,
				"text":       result,
			}
			// Soft-warning semantics: force-stop exits still emit a normal
			// agent_reply, but carry partial/failure_code so consumers can
			// show a non-error "stopped early" hint next to the text.
			if status.Partial {
				payload["partial"] = true
				payload["failure_code"] = status.FailureCode
			}
			payloadBytes, _ := json.Marshal(payload)
			deps.EventBus.Emit(Event{Type: EventAgentReply, Payload: payloadBytes})
		}
	}

	// Prefer handler-accumulated LLM totals (includes cloud_delegate nested
	// spend) for the model token fields. Tool billing rolls into CostUSD
	// on top of LLM cost but never into the token fields, so
	// input_tokens+output_tokens==total_tokens stays true for API consumers.
	reportedUsage := RunAgentUsage{
		InputTokens:  usage.InputTokens,
		OutputTokens: usage.OutputTokens,
		TotalTokens:  usage.TotalTokens,
		CostUSD:      usage.CostUSD,
	}
	if up, ok := handler.(agent.UsageProvider); ok {
		acc := up.Usage()
		llm := acc.LLM
		if llm.LLMCalls > 0 || llm.TotalTokens > 0 || llm.CostUSD > 0 || acc.ToolCostUSD > 0 {
			reportedUsage = RunAgentUsage{
				InputTokens:  llm.InputTokens,
				OutputTokens: llm.OutputTokens,
				TotalTokens:  llm.TotalTokens,
				CostUSD:      llm.CostUSD + acc.ToolCostUSD,
			}
		}
	}
	log.Printf("daemon: reply to %s (%d tokens, $%.4f)", agentName, reportedUsage.TotalTokens, reportedUsage.CostUSD)

	// Respect the keep_alive toggle after each completed turn.
	if _, _, _, mgr := deps.RebuildLayers(); mgr != nil {
		cleanupPlaywrightAfterTurn(mgr)
	}

	// On save failure, blank SessionID so HTTP/SSE clients can't click through
	// to a session that isn't on disk (matches the agent_reply gate above).
	returnedSessionID := sess.ID
	if saveErr != nil {
		returnedSessionID = ""
	}
	return &RunAgentResult{
		Reply:       result,
		SessionID:   returnedSessionID,
		Agent:       agentName,
		Usage:       reportedUsage,
		Partial:     status.Partial,
		FailureCode: status.FailureCode,
	}, nil
}

func generateMessageID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "msg-" + hex.EncodeToString(b)
}

// ErrSlashRouteBusy is returned when a slash request lands on a route key
// that already has another run in flight. Callers translate this to an SSE
// error event with reason="active_run_not_ready".
var ErrSlashRouteBusy = errors.New("slash command rejected: another run is active on this route")

// RunSlashWorkflow handles a /research or /swarm HTTP request by dispatching
// directly to Shannon Cloud's Gateway via cloudflow.Run. It mirrors RunAgent's
// return shape so callers can encode the result identically (the SSE writer's
// existing event: done payload works without modification).
//
// Differences from RunAgent:
//   - No agent-loop execution; cloudflow.Run replaces the loop body.
//   - No session-history replay into the LLM (Cloud carries its own memory).
//
// Same as RunAgent (intentionally, to avoid transcript corruption on
// concurrent same-session writes):
//   - Honors req.Agent — slash routes through the agent's SessionsDir and
//     resumes the agent's long-lived session.
//   - Acquires a SessionCache route lock so concurrent slash+RunAgent or
//     slash+slash on the same route serialize via the existing locking model.
//   - Persists user + assistant messages to the local transcript; emits the
//     same EventMessageReceived bus event RunAgent emits at runner.go:837-846.
//
// Caller MUST have already validated that req.Text is a slash command and
// that req.Content is empty (rejected at the HTTP layer).
func RunSlashWorkflow(ctx context.Context, deps *ServerDeps, req RunAgentRequest, cmd *cloudflow.SlashCommand, handler agent.EventHandler) (*RunAgentResult, error) {
	cfg, _, _ := deps.Snapshot()
	if deps.GW == nil {
		return nil, fmt.Errorf("gateway not configured")
	}

	// Honor cfg.Cloud.Timeout the same way register.go:458-461 does for
	// cloud_delegate. Zero falls back to 1 hour so slash and tool paths default
	// identically (cloudflow's own zero-fallback is 30 minutes — diverges from
	// the cloud_delegate baseline if we don't compute this here).
	slashTimeout := time.Duration(cfg.Cloud.Timeout) * time.Second
	if slashTimeout <= 0 {
		slashTimeout = time.Hour
	}

	agentName := req.Agent // honors named-agent lane; "" = default
	sessionsDir := deps.SessionCache.SessionsDir(agentName)

	// Use the same route-key + lock machinery RunAgent uses (runner.go:611-660),
	// but fail fast instead of canceling or waiting for an active route.
	// `route` is hoisted out of the locking block so the resolution switch
	// below can warm-resume from route.sessionID across slash invocations.
	var sessMgr *session.Manager
	var route *routeEntry
	// slashCtx is the cancellable context passed to cloudflow.Run. For routed
	// requests we register slashCancel via SetRouteCancel so POST /cancel can
	// stop the run. For unrouted requests slashCtx == ctx (no cancel target).
	slashCtx := ctx
	if req.RouteKey != "" {
		var busy bool
		route, busy = deps.SessionCache.TryLockRouteWithManager(req.RouteKey, sessionsDir)
		if busy {
			return nil, ErrSlashRouteBusy
		}
		sessMgr = route.manager

		// Wrap ctx so POST /cancel → CancelRoute → slashCancel propagates through
		// cloudflow.Run → StreamSSE. Without this, CancelRoute only sets
		// cancelPending=true (a flag nothing reads) and the cloud workflow
		// continues until its own deadline. Mirrors RunAgent at runner.go:620-628.
		var slashCancel context.CancelFunc
		slashCtx, slashCancel = context.WithCancel(ctx)
		defer slashCancel()

		// Publish run state so concurrent regular POST /message calls on the same
		// route see "active run in progress" via InjectMessage (returns InjectBusy)
		// instead of falling through to start a parallel RunAgent. Mirrors
		// RunAgent's pattern at runner.go:621-624 / 629-631.
		routeDone := make(chan struct{})
		deps.SessionCache.SetRouteRunState(req.RouteKey, routeDone, nil, "")
		deps.SessionCache.SetRouteCancel(req.RouteKey, slashCancel)
		defer func() {
			// Clear cancel registration before unlock so the next caller registers fresh.
			deps.SessionCache.SetRouteCancel(req.RouteKey, nil)
			deps.SessionCache.ClearRouteRunState(req.RouteKey)
			closeRouteDone(routeDone)
			if current := sessMgr.Current(); current != nil {
				route.sessionID = current.ID
			}
			deps.SessionCache.UnlockRoute(req.RouteKey)
		}()
	} else {
		sessMgr = session.NewManager(sessionsDir)
		defer func() {
			if err := sessMgr.Close(); err != nil {
				log.Printf("daemon: failed to close session manager: %v", err)
			}
		}()
	}

	// Resume / new-session — mirrors RunAgent's switch at runner.go:659-687
	// (SessionID > NewSession > warm-resume > agent cold-start > default new).
	switch {
	case req.SessionID != "":
		if _, err := sessMgr.Resume(req.SessionID); err != nil {
			return nil, fmt.Errorf("session not found: %s", req.SessionID)
		}
	case req.NewSession || req.RouteKey == "":
		sessMgr.NewSession()
	case route != nil && route.sessionID != "":
		// Warm resume: a prior run on this route stored its session ID; reuse it
		// so subsequent slash calls on the same routed lane (default:source:channel
		// or agent:foo) append to one continuous local transcript instead of
		// forking a fresh session each time.
		if _, err := sessMgr.Resume(route.sessionID); err != nil {
			log.Printf("daemon: failed to resume routed session %q for %q: %v", route.sessionID, req.RouteKey, err)
			sessMgr.NewSession()
		}
	case strings.HasPrefix(req.RouteKey, "agent:"):
		// Named-agent cold start — resume latest from disk, or NewSession if none.
		if _, err := resumeNamedAgentColdStart(sessMgr); err != nil {
			log.Printf("daemon: failed to resume latest named-agent session: %v", err)
			if sessMgr.Current() == nil {
				sessMgr.NewSession()
			}
		}
	default:
		sessMgr.NewSession()
	}
	sess := sessMgr.Current()

	// Stamp session metadata before persisting — mirrors runner.go:791-803.
	// This makes the session searchable/displayable by source+channel and gives
	// it a stable human-readable title.
	if !req.Ephemeral {
		if req.Source != "" && req.Channel != "" {
			sess.Source = req.Source
			sess.Channel = req.Channel
		}
		// Title precedence (matches RunAgent's combined behavior at lines
		// 798-803 + 1147-1152): named agent > route source/channel > derived
		// from query.
		if sess.Title == "New session" {
			switch {
			case agentName != "":
				sess.Title = session.AgentTitle(agentName)
			case req.RouteKey != "":
				if t := routeTitle(req.Source, req.Channel, req.Sender); t != "" {
					sess.Title = t
				} else {
					sess.Title = session.Title(cmd.Query)
				}
			default:
				sess.Title = session.Title(cmd.Query)
			}
		}
	}

	// Persist the user message (matching runner.go:820-848 verbatim).
	if !req.Ephemeral {
		userMsgID := generateMessageID()
		sess.Messages = append(sess.Messages,
			client.Message{Role: "user", Content: client.NewTextContent(req.Text)},
		)
		sess.MessageMeta = append(sess.MessageMeta,
			session.MessageMeta{Source: req.Source, MessageID: userMsgID, Timestamp: session.TimePtr(time.Now())},
		)
		if err := sessMgr.Save(); err != nil {
			log.Printf("daemon: failed to pre-save user message: %v", err)
		} else if deps.EventBus != nil {
			payload, _ := json.Marshal(map[string]any{
				"agent":      agentName,
				"source":     req.Source,
				"sender":     req.Sender,
				"session_id": sess.ID,
				"message_id": userMsgID,
				"text":       req.Text,
			})
			deps.EventBus.Emit(Event{Type: EventMessageReceived, Payload: payload})
		}
	}

	cf := cloudflow.Request{
		Gateway:      deps.GW,
		APIKey:       cfg.APIKey,
		Query:        cmd.Query,
		WorkflowType: cmd.Type,
		Strategy:     cmd.Strategy,
		SessionID:    sess.ID,
		Timeout:      slashTimeout,
	}
	res, err := cloudflow.Run(slashCtx, cf, handler)
	if err != nil {
		return nil, err
	}

	// Persist assistant message.
	if !req.Ephemeral {
		sess.Messages = append(sess.Messages,
			client.Message{Role: "assistant", Content: client.NewTextContent(res.FinalText)},
		)
		sess.MessageMeta = append(sess.MessageMeta,
			session.MessageMeta{Source: "cloud", Timestamp: session.TimePtr(time.Now())},
		)
		if err := sessMgr.Save(); err != nil {
			log.Printf("daemon: failed to save assistant message: %v", err)
		}
	}

	// RunAgentUsage has exactly four fields (runner.go:394-399): InputTokens,
	// OutputTokens, TotalTokens, CostUSD. There is no Model field.
	return &RunAgentResult{
		Reply:     res.FinalText,
		SessionID: sess.ID,
		Agent:     agentName,
		Usage: RunAgentUsage{
			InputTokens:  res.Usage.InputTokens,
			OutputTokens: res.Usage.OutputTokens,
			TotalTokens:  res.Usage.TotalTokens,
			CostUSD:      res.Usage.CostUSD,
		},
	}, nil
}

func closeRouteDone(done chan struct{}) {
	if done == nil {
		return
	}
	defer func() {
		if recover() != nil {
			// Best effort cleanup; callers may close defensively in multiple paths.
			// Avoid panic if the channel was already closed externally.
		}
	}()
	close(done)
}

// isSoftRunError reports whether err is a normal termination (cancel, timeout,
// max iterations) rather than a hard failure. Soft errors should persist the
// full conversation from RunMessages(), not just a friendly error stub.
func isSoftRunError(err error) bool {
	return errors.Is(err, agent.ErrMaxIterReached) ||
		errors.Is(err, agent.ErrHardIdleTimeout) ||
		errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded)
}

// turnBaseline captures pre-turn session state so both mid-turn checkpoints
// and the post-turn final save can idempotently rebuild the session from
// (baseline + current loop snapshot) — never append-on-top. This is the
// single persistence invariant for a turn: after applyTurnState runs, the
// session reflects exactly one canonical transcript and one usage total
// for the accumulated turn, no matter how many times the function is
// called.
type turnBaseline struct {
	msgCount    int
	metaCount   int
	usage       session.UsageSummary // pre-turn cumulative usage; zero if sess.Usage was nil
	hadUsage    bool                 // true if sess.Usage was non-nil at baseline
	source      string
	preLoopUser bool
}

// captureTurnBaseline snapshots sess state at turn start so subsequent
// applyTurnState calls can rebuild idempotently.
func captureTurnBaseline(sess *session.Session, source string, preLoopUserAppended bool) turnBaseline {
	b := turnBaseline{
		msgCount:    len(sess.Messages),
		metaCount:   len(sess.MessageMeta),
		source:      source,
		preLoopUser: preLoopUserAppended,
	}
	if sess.Usage != nil {
		b.usage = *sess.Usage
		b.hadUsage = true
	}
	return b
}

// applyTurnMessages rebuilds sess.Messages/MessageMeta from baseline +
// loop.RunMessages(). Idempotent — safe to call any number of times with
// changing loop state (compaction shrinks etc.).
func applyTurnMessages(sess *session.Session, loop *agent.AgentLoop, b turnBaseline) {
	if len(sess.Messages) > b.msgCount {
		sess.Messages = sess.Messages[:b.msgCount]
	}
	if len(sess.MessageMeta) > b.metaCount {
		sess.MessageMeta = sess.MessageMeta[:b.metaCount]
	}
	runMsgs := loop.RunMessages()
	if len(runMsgs) == 0 {
		return
	}
	runInjected := loop.RunMessageInjected()
	runTimestamps := loop.RunMessageTimestamps()
	startIdx := 0
	if b.preLoopUser && runMsgs[0].Role == "user" {
		startIdx = 1
	}
	fallbackTime := time.Now()
	for i := startIdx; i < len(runMsgs); i++ {
		ts := fallbackTime
		if i < len(runTimestamps) && !runTimestamps[i].IsZero() {
			ts = runTimestamps[i]
		}
		sess.Messages = append(sess.Messages, runMsgs[i])
		meta := session.MessageMeta{Source: b.source, Timestamp: session.TimePtr(ts)}
		if i < len(runInjected) && runInjected[i] {
			meta.SystemInjected = true
		}
		sess.MessageMeta = append(sess.MessageMeta, meta)
	}
}

// usageProvider is the local interface applyTurnUsage needs. Defined here
// (rather than accepting agent.UsageProvider directly) so the caller type
// is restricted at compile time — a future refactor that dropped the
// interface on the daemon handler would fail to compile instead of
// silently no-op'ing the usage folding at runtime.
type usageProvider interface {
	Usage() agent.AccumulatedUsage
}

// applyTurnUsage sets sess.Usage to (baseline + current accumulator).
// Idempotent — no double-counting across checkpoint + final-save calls.
// A nil provider is a no-op (used by unit tests that exercise only the
// message path).
func applyTurnUsage(sess *session.Session, up usageProvider, b turnBaseline) {
	if up == nil {
		return
	}
	acc := up.Usage()
	llm := acc.LLM
	hasTurnUsage := llm.LLMCalls > 0 || acc.ToolCalls > 0 || llm.InputTokens > 0 ||
		llm.CostUSD > 0 || acc.ToolCostUSD > 0
	if !b.hadUsage && !hasTurnUsage {
		return
	}
	total := b.usage
	if hasTurnUsage {
		total.Add(session.UsageFromAccumulated(
			llm.LLMCalls, llm.InputTokens, llm.OutputTokens, llm.TotalTokens,
			llm.CostUSD, llm.CacheReadTokens, llm.CacheCreationTokens, llm.CacheCreation5mTokens, llm.CacheCreation1hTokens, llm.Model,
			acc.ToolCalls, acc.ToolCostUSD,
		))
	}
	sess.Usage = &total
	if sess.SchemaVersion < 2 {
		sess.SchemaVersion = 2
	}
}

// applyTurnState is the combined rebuild — messages + usage — used by
// both mid-turn checkpoints and the post-turn final save so a turn is
// never persisted twice via different paths. up may be nil (usage skipped).
func applyTurnState(sess *session.Session, loop *agent.AgentLoop,
	up usageProvider, b turnBaseline) {
	applyTurnMessages(sess, loop, b)
	applyTurnUsage(sess, up, b)
}

// FriendlyAgentError maps raw agent errors to user-facing messages.
// Full error detail is logged separately; this keeps session/UI clean.
func FriendlyAgentError(err error) string {
	return runstatus.FriendlyMessage(runstatus.CodeFromError(err))
}
