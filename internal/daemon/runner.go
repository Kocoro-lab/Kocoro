package daemon

import (
	"context"
	"crypto/rand"
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
	"github.com/Kocoro-lab/ShanClaw/internal/config"
	"github.com/Kocoro-lab/ShanClaw/internal/cwdctx"
	"github.com/Kocoro-lab/ShanClaw/internal/hooks"
	"github.com/Kocoro-lab/ShanClaw/internal/mcp"
	"github.com/Kocoro-lab/ShanClaw/internal/schedule"
	"github.com/Kocoro-lab/ShanClaw/internal/session"
	"github.com/Kocoro-lab/ShanClaw/internal/skills"
	"github.com/Kocoro-lab/ShanClaw/internal/tools"
)

// RunAgentRequest is the input for RunAgent.
type RunAgentRequest struct {
	Text           string           `json:"text"`
	Agent          string           `json:"agent,omitempty"`
	SessionID      string           `json:"session_id,omitempty"`
	NewSession     bool             `json:"new_session,omitempty"`
	Source         string           `json:"source,omitempty"`    // "slack", "line", "shanclaw", "webhook"
	Sender         string           `json:"sender,omitempty"`    // user identifier from channel
	Channel        string           `json:"channel,omitempty"`   // channel/thread source context
	ThreadID       string           `json:"thread_id,omitempty"` // thread context for messaging platforms
	CWD            string           `json:"cwd,omitempty"`       // absolute project path override
	RouteKey       string           `json:"-"`                   // internal routing key
	Ephemeral      bool             `json:"-"`                   // caller owns persistence + events
	ModelOverride  string           `json:"-"`                   // overrides agent model tier
	BypassRouting  bool             `json:"-"`                   // skip route lock (heartbeat runs)
	SessionHistory []client.Message `json:"-"`                   // pre-loaded history for LLM context (BypassRouting runs)
	StickyContext  string           `json:"-"`                   // 额外的 sticky context，注入系统提示（对用户不可见）
}

// Validate checks that the request has the minimum required fields.
func (r *RunAgentRequest) Validate() error {
	if strings.TrimSpace(r.Text) == "" {
		return fmt.Errorf("text is required")
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
func outputFormatForSource(source string) string {
	switch strings.ToLower(strings.TrimSpace(source)) {
	case "slack", "line", "feishu", "lark", "telegram", "webhook":
		return "plain"
	default:
		return "markdown"
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
	WSClient        *Client // WebSocket client for proactive messages
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
	// "default" is not a real agent — it means "use base agent, no --agent flag".
	if agentName == "default" {
		agentName = ""
	}
	req.Agent = agentName

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
			// Agent removed or renamed — fall back to default gracefully.
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

	// Resolve effective CWD: request > resumed session > agent config > process cwd.
	var sessionCWD string
	if resumed {
		sessionCWD = sess.CWD
	}
	var agentCWD string
	if agentOverride != nil && agentOverride.Config != nil {
		agentCWD = agentOverride.Config.CWD
	}
	effectiveCWD := cwdctx.ResolveEffectiveCWD(req.CWD, sessionCWD, agentCWD)
	if err := cwdctx.ValidateCWD(effectiveCWD); err != nil {
		return nil, fmt.Errorf("invalid cwd: %w", err)
	}
	if req.RouteKey != "" {
		deps.SessionCache.SetRouteRunState(req.RouteKey, routeDone, routeInjectCh, effectiveCWD)
	}
	runCfg, err := config.RuntimeConfigForCWD(cfg, effectiveCWD)
	if err != nil {
		return nil, fmt.Errorf("runtime config: %w", err)
	}
	sess.CWD = effectiveCWD
	ctx = cwdctx.WithSessionCWD(ctx, effectiveCWD)

	// Notify handler of resolved session ID so it can include it in EventBus payloads.
	if setter, ok := handler.(interface{ SetSessionID(string) }); ok {
		setter.SetSessionID(sess.ID)
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
		sess.Messages = append(sess.Messages,
			client.Message{Role: "user", Content: client.NewTextContent(prompt)},
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
	tools.SetRegistrySkills(reg, loadedSkills)

	// Always expose local session search for daemon-served agents.
	// Use the per-agent manager so searches are scoped to that agent's sessions.
	tools.RegisterSessionSearch(reg, sessMgr)

	loop := agent.NewAgentLoop(deps.GW, reg, runCfg.ModelTier, deps.ShannonDir,
		runCfg.Agent.MaxIterations, runCfg.Tools.ResultTruncation, runCfg.Tools.ArgsTruncation,
		&runCfg.Permissions, deps.Auditor, deps.HookRunner)
	loop.SetMaxTokens(runCfg.Agent.MaxTokens)
	loop.SetTemperature(runCfg.Agent.Temperature)
	loop.SetContextWindow(runCfg.Agent.ContextWindow)
	loop.SetEnableStreaming(false)
	loop.SetQuiet(true)
	loop.SetDeltaProvider(agent.NewTemporalDelta())
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
		if ac.ModelTier != nil {
			loop.SetModelTier(*ac.ModelTier)
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
	}
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

	// Wire SubAgentRunner: inject a fully-wired child AgentLoop builder per-run.
	// Must use reg (cloned), not baseReg (shared), to avoid race across routes.
	if st, ok := reg.Get("subagent"); ok {
		if sat, ok := st.(*tools.SubAgentTool); ok {
			sat.SetRunner(func(ctx context.Context, ag *agents.Agent, prompt string, childReg *agent.ToolRegistry, handler agent.EventHandler) tools.SubAgentResult {
				start := time.Now()
				childLoop := agent.NewAgentLoop(deps.GW, childReg, runCfg.ModelTier, deps.ShannonDir,
					tools.SubAgentMaxIterations, runCfg.Tools.ResultTruncation, runCfg.Tools.ArgsTruncation,
					&runCfg.Permissions, deps.Auditor, deps.HookRunner)
				childLoop.SetMaxTokens(tools.SubAgentMaxTokens)
				childLoop.SetTemperature(runCfg.Agent.Temperature)
				childLoop.SetContextWindow(runCfg.Agent.ContextWindow)
				childLoop.SetEnableStreaming(false)
				childLoop.SetQuiet(true)
				// Disable thinking for sub-agents to control output token costs.
				// Thinking tokens often exceed actual output; disabling saves 50%+.
				childLoop.SetThinking(&client.ThinkingConfig{Type: "disabled"})
				childLoop.SetSessionCWD(effectiveCWD)
				childLoop.SetHandler(handler)

				childAgentName := ag.Name
				childAgentDir := filepath.Join(deps.ShannonDir, "agents", childAgentName)
				childMCPCtx := tools.ResolveMCPContext(runCfg, ag)
				childLoop.SwitchAgent(ag.Prompt, childAgentDir, nil, childMCPCtx, ag.Skills)

				// Apply per-agent model config overrides
				if ag.Config != nil && ag.Config.Agent != nil {
					ac := ag.Config.Agent
					if ac.Model != nil {
						childLoop.SetSpecificModel(*ac.Model)
					}
					if ac.ModelTier != nil {
						childLoop.SetModelTier(*ac.ModelTier)
						log.Printf("[subagent] %s: model_tier override → %s", childAgentName, *ac.ModelTier)
					}
					if ac.MaxIterations != nil {
						childLoop.SetMaxIterations(*ac.MaxIterations)
					}
					if ac.Temperature != nil {
						childLoop.SetTemperature(*ac.Temperature)
					}
					if ac.MaxTokens != nil {
						childLoop.SetMaxTokens(*ac.MaxTokens)
					}
					if ac.ContextWindow != nil {
						childLoop.SetContextWindow(*ac.ContextWindow)
					}
				}

				childTimeout := 30 * time.Minute // safety net only; maxIterations is the primary limiter
				if ag.Config != nil && ag.Config.Agent != nil && ag.Config.Agent.Timeout != nil {
					if d, err := time.ParseDuration(*ag.Config.Agent.Timeout); err == nil && d > 0 {
						childTimeout = d
					}
				}
				childCtx, cancel := context.WithTimeout(ctx, childTimeout)
				defer cancel()

				output, usage, err := childLoop.Run(childCtx, prompt, nil)
				result := tools.SubAgentResult{
					Output:   output,
					Duration: time.Since(start),
					Err:      err,
				}
				if usage != nil {
					result.InputTokens = usage.InputTokens
					result.OutputTokens = usage.OutputTokens
					result.TotalTokens = usage.TotalTokens
					result.LLMCalls = usage.LLMCalls
					result.CostUSD = usage.CostUSD
				}
				return result
			})
			sat.SetBaseRegistry(reg)
		}
	}

	if routeInjectCh != nil {
		loop.SetInjectCh(routeInjectCh)
	}
	loop.SetSessionID(sess.ID)
	// Scope task store to this session so parallel routes don't mix tasks.
	tools.OverrideTaskStoreDir(reg, filepath.Join(deps.ShannonDir, "tasks", sess.ID))
	loop.SetSessionCWD(effectiveCWD)
	loop.SetWorkingSet(sessMgr.WorkingSet(sess.ID))
	sessMgr.OnSessionClose(sess.ID, loop.SpillCleanupFunc())

	result, usage, runErr := loop.Run(ctx, prompt, history)
	if runErr != nil && !errors.Is(runErr, agent.ErrMaxIterReached) {
		// Hard error — save a user-friendly error message so the session isn't
		// left with a dangling user message and no assistant reply.
		// Full error detail goes to the log; session/UI gets a clean summary.
		log.Printf("daemon: agent %s run error: %v", agentName, runErr)
		if !req.Ephemeral && result == "" {
			userErr := FriendlyAgentError(runErr)
			sess.Messages = append(sess.Messages,
				client.Message{Role: "assistant", Content: client.NewTextContent(userErr)},
			)
			sess.MessageMeta = append(sess.MessageMeta,
				session.MessageMeta{Source: req.Source, Timestamp: session.TimePtr(time.Now())},
			)
			if err := sessMgr.Save(); err != nil {
				log.Printf("daemon: failed to save error session: %v", err)
			}
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

		// Append the turn to the session and persist.
		// Prefer full conversation messages (including tool_use/tool_result turns)
		// from RunMessages() so resumed sessions give the LLM tool-call evidence.
		// Falls back to flat text if RunMessages() is empty (early LLM error).
		source := req.Source
		if source == "" {
			source = "unknown"
		}
		runMsgs := loop.RunMessages()
		runInjected := loop.RunMessageInjected()
		runTimestamps := loop.RunMessageTimestamps()
		if len(runMsgs) > 0 {
			// RunMessages includes: [user prompt, assistant+tool_use, tool_result, ..., final assistant].
			// If the user message was already appended pre-loop, skip the first
			// message from runMsgs (same user prompt) to avoid duplication.
			startIdx := 0
			if preLoopUserAppended && len(runMsgs) > 0 && runMsgs[0].Role == "user" {
				startIdx = 1
			}
			fallbackTime := time.Now()
			for i, msg := range runMsgs[startIdx:] {
				idx := i + startIdx
				ts := fallbackTime
				if idx < len(runTimestamps) && !runTimestamps[idx].IsZero() {
					ts = runTimestamps[idx]
				}
				sess.Messages = append(sess.Messages, msg)
				meta := session.MessageMeta{Source: source, Timestamp: session.TimePtr(ts)}
				if idx < len(runInjected) && runInjected[idx] {
					meta.SystemInjected = true
				}
				sess.MessageMeta = append(sess.MessageMeta, meta)
			}
		} else {
			// Fallback: flat text (early error or no messages accumulated).
			if !preLoopUserAppended {
				sess.Messages = append(sess.Messages,
					client.Message{Role: "user", Content: client.NewTextContent(prompt)},
				)
				sess.MessageMeta = append(sess.MessageMeta,
					session.MessageMeta{Source: source, Timestamp: session.TimePtr(userMsgTime)},
				)
			}
			replyTime := time.Now()
			sess.Messages = append(sess.Messages,
				client.Message{Role: "assistant", Content: client.NewTextContent(result)},
			)
			sess.MessageMeta = append(sess.MessageMeta,
				session.MessageMeta{Source: source, Timestamp: session.TimePtr(replyTime)},
			)
		}
		saveErr = sessMgr.Save()
		if saveErr != nil {
			log.Printf("daemon: failed to save session: %v", saveErr)
			if deps.EventBus != nil {
				payload, _ := json.Marshal(map[string]any{
					"agent":      agentName,
					"source":     req.Source,
					"session_id": sess.ID,
					"error":      fmt.Sprintf("session save failed: %v", saveErr),
				})
				deps.EventBus.Emit(Event{Type: EventAgentError, Payload: payload})
			}
		}

		// Only emit agent_reply when the session actually persisted. If the
		// save failed, the conversation is not on disk and downstream
		// consumers (e.g. desktop schedule notifications that click through
		// to the session) would point at a session that cannot be loaded.
		if saveErr == nil && deps.EventBus != nil {
			payload, _ := json.Marshal(map[string]any{
				"agent":      agentName,
				"source":     req.Source,
				"session_id": sess.ID,
				"text":       result,
			})
			deps.EventBus.Emit(Event{Type: EventAgentReply, Payload: payload})
		}
	}

	log.Printf("daemon: reply to %s (%d tokens, $%.4f)", agentName, usage.TotalTokens, usage.CostUSD)

	// Schedule Playwright idle disconnect unless keep_alive or CDP mode.
	// CDP mode keeps playwright-mcp alive permanently (lightweight WebSocket).
	if sup != nil {
		if h := sup.HealthFor("playwright"); h.State == mcp.StateHealthy {
			if _, _, _, mgr := deps.RebuildLayers(); mgr != nil {
				if cfg, ok := mgr.ConfigFor("playwright"); !ok || (!cfg.KeepAlive && !mcp.IsPlaywrightCDPMode(cfg)) {
					mgr.DisconnectAfterIdle("playwright", 5*time.Minute)
					log.Printf("daemon: Playwright idle disconnect scheduled (5m)")
				}
			}
		}
	}

	// On save failure, blank SessionID so HTTP/SSE clients can't click through
	// to a session that isn't on disk (matches the agent_reply gate above).
	returnedSessionID := sess.ID
	if saveErr != nil {
		returnedSessionID = ""
	}
	return &RunAgentResult{
		Reply:     result,
		SessionID: returnedSessionID,
		Agent:     agentName,
		Usage: RunAgentUsage{
			InputTokens:  usage.InputTokens,
			OutputTokens: usage.OutputTokens,
			TotalTokens:  usage.TotalTokens,
			CostUSD:      usage.CostUSD,
		},
	}, nil
}

func generateMessageID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "msg-" + hex.EncodeToString(b)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
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

// FriendlyAgentError maps raw agent errors to user-facing messages.
// Full error detail is logged separately; this keeps session/UI clean.
func FriendlyAgentError(err error) string {
	// Check context errors structurally before string matching.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "The request was cancelled or timed out."
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "429"):
		return "Sorry, the AI service is currently rate-limited. Please try again in a moment."
	case strings.Contains(msg, "529") || strings.Contains(msg, "overloaded"):
		return "Sorry, the AI service is temporarily overloaded. Please try again shortly."
	case strings.Contains(msg, "500") || strings.Contains(msg, "502") || strings.Contains(msg, "503"):
		return "Sorry, the AI service encountered a temporary error. Please try again."
	case strings.Contains(msg, "request failed:") || strings.Contains(msg, "stream read error"):
		return "Sorry, the connection to the AI service was interrupted. Please try again."
	default:
		return "Sorry, an unexpected error occurred. Please try again."
	}
}
