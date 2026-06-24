package tui

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/term"

	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/agents"
	"github.com/Kocoro-lab/ShanClaw/internal/audit"
	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/cloudflow"
	"github.com/Kocoro-lab/ShanClaw/internal/config"
	ctxwin "github.com/Kocoro-lab/ShanClaw/internal/context"
	"github.com/Kocoro-lab/ShanClaw/internal/cwdctx"
	"github.com/Kocoro-lab/ShanClaw/internal/hooks"
	"github.com/Kocoro-lab/ShanClaw/internal/instructions"
	"github.com/Kocoro-lab/ShanClaw/internal/memory"
	"github.com/Kocoro-lab/ShanClaw/internal/permissions"
	"github.com/Kocoro-lab/ShanClaw/internal/runstatus"
	"github.com/Kocoro-lab/ShanClaw/internal/session"
	"github.com/Kocoro-lab/ShanClaw/internal/skills"
	"github.com/Kocoro-lab/ShanClaw/internal/tools"
	"github.com/Kocoro-lab/ShanClaw/internal/update"
)

type state int

const (
	stateStartup state = iota
	stateInput
	stateProcessing
	stateApproval
	stateSessionPicker
	statePicker
)

// tuiMemoryFallback adapts session.Manager to the tools.FallbackQuery
// interface for the TUI memory_recall path. MemoryFileSnippet returns
// empty for v1 — daemon path provides the richer fallback; TUI stays
// lightweight.
type tuiMemoryFallback struct {
	sessionMgr *session.Manager
}

// Compile-time check that *tuiMemoryFallback satisfies tools.FallbackQuery.
var _ tools.FallbackQuery = (*tuiMemoryFallback)(nil)

func (t *tuiMemoryFallback) SessionKeyword(_ context.Context, query string, limit int) ([]any, error) {
	if t.sessionMgr == nil {
		return nil, nil
	}
	hits, err := t.sessionMgr.Search(query, limit)
	if err != nil {
		return nil, err
	}
	out := make([]any, 0, len(hits))
	for _, h := range hits {
		out = append(out, h)
	}
	return out, nil
}

func (t *tuiMemoryFallback) MemoryFileSnippet(_ context.Context, _ string) (string, error) {
	return "", nil
}

type agentDoneMsg struct {
	result string
	usage  *agent.TurnUsage
	err    error
	status agent.RunStatus
}

type approvalRequestMsg struct {
	tool string
	args string
}

type healthCheckMsg struct {
	gatewayOK bool
	updateMsg string
}

type serverToolsLoadedMsg struct {
	registry *agent.ToolRegistry
	cleanup  func()
	err      error
}

// streamOutputMsg is sent from goroutines to update the TUI output safely.
type streamOutputMsg struct {
	text string
	raw  string // original markdown text (empty for plain text)
}

// streamDeltaMsg carries an incremental token fragment of the in-flight LLM
// answer. Unlike streamOutputMsg it is NOT committed to scrollback — it feeds a
// transient live-preview region (m.streamLive) shown under the spinner while a
// run is processing, then cleared when the segment finalizes into scrollback.
type streamDeltaMsg struct {
	delta string
}

// outputBlock stores both raw and rendered text so output can be re-rendered on resize.
type outputBlock struct {
	raw      string                 // original markdown (empty for plain text)
	rendered string                 // width-specific rendered text
	rerender func(width int) string // optional: re-render at new width (e.g. startup header)
}

// rerenderDoneMsg signals that the ClearScreen→Println sequence from
// rerenderOutput has completed, so incremental flushPrints can resume.
type rerenderDoneMsg struct{}

// historyLoadedMsg is sent after session history finishes loading in a
// goroutine, so we can re-render at the current terminal width.
type historyLoadedMsg struct{}

// spinnerTickMsg is a slow fallback that advances spinner phrase text
type spinnerTickMsg struct{}

// spinnerFrameMsg drives fast glyph + color animation (~100ms)
type spinnerFrameMsg struct{}

// headerTickMsg advances the startup header animation by one frame.
type headerTickMsg struct{}

// toolCallMsg signals that a tool call is about to start.
type toolCallMsg struct {
	name string
	args string
}

// toolResultMsg is sent from the agent goroutine to deliver tool results safely
// through the Bubbletea event loop, avoiding direct Model field mutation.
type toolResultMsg struct {
	name    string
	args    string
	content string
	isError bool
	elapsed time.Duration
}

// titleGeneratedMsg carries a freshly generated smart title back to the update
// loop, which persists it (on the main goroutine) and refreshes the cached
// session list. Generation runs off-thread; persistence does not, so all
// session mutation stays single-threaded.
type titleGeneratedMsg struct {
	sessionID string
	title     string
	atTurns   int
}

type toolResultEntry struct {
	name    string
	args    string
	content string
	isError bool
	elapsed time.Duration
}

type Model struct {
	baseCfg             *config.Config
	cfg                 *config.Config
	gateway             *client.GatewayClient
	llmClient           client.LLMClient
	sessions            *session.Manager
	toolRegistry        *agent.ToolRegistry
	toolCleanup         func()
	agentLoop           *agent.AgentLoop
	textarea            textarea.Model
	output              []outputBlock
	pendingPrints       []string
	streamLive          string // transient live-preview of the in-flight answer (not yet in scrollback)
	processingStartTime time.Time
	spinnerIdx          int
	spinnerTexts        []string
	glyphIdx            int
	colorIdx            int
	lastSessions        []session.SessionSummary // cached for session picker
	sessionPickerIdx    int
	pickerTitle         string         // generic selection picker (statePicker)
	pickerOpts          []pickerOption // current picker rows
	pickerIdx           int            // highlighted row
	pickerKind          pickerKind     // dispatches Enter to the right apply
	state               state
	width               int
	height              int
	version             string
	approvalCh          chan bool
	program             *tea.Program
	shannonDir          string
	auditor             *audit.AuditLogger
	hookRunner          *hooks.HookRunner
	customCommands      map[string]string // name → prompt content from commands/*.md
	bypassPermissions   bool
	agentOverride       *agents.Agent    // per-agent override for re-application after async tool load
	loadedSkills        []*skills.Skill  // skills for current agent (survives loop re-creation)
	skillsPtr           *[]*skills.Skill // pointer into use_skill tool's skills slice
	memPreflight        tools.MemoryPreflightQuerier
	remoteCleanup       func()             // cleanup for MCP connections from async load
	cancelRun           context.CancelFunc // cancels the running agent loop
	injectCh            chan agent.InjectedMessage
	resumedSession      bool // true when the current session was resumed (not newly created)
	// Tool result display
	pendingToolName string
	pendingToolArgs string
	lastToolResults []toolResultEntry
	toolExpandLevel int // 0=summary only, 1=compact lines, 2=expanded details
	// Slash command completion menu
	slashCommands []slashCmd // built once in New(), includes builtins + custom/agent cmds
	menuVisible   bool
	menuIndex     int
	menuItems     []slashCmd
	menuMatchPos  [][]int // per-item matched rune indices in cmd, aligned with menuItems
	// Startup header animation
	headerFrame     int
	headerDone      bool
	headerHealth    *healthCheckMsg          // buffered until animation ends
	headerSessions  []session.SessionSummary // cached at startup for View()
	headerTipIdx    int                      // stable random tip index
	headerCWD       string                   // cached working directory
	markdownCacheMu sync.RWMutex
	markdownCache   map[string]string
	// Input history
	inputHistory        []string        // past submitted inputs (oldest first)
	historyIdx          int             // -1 = current input, 0..len-1 = history position (from end)
	historySaved        string          // current input saved when entering history
	lastEscTime         time.Time       // for double-escape detection
	sessionAllowed      map[string]bool // tools always-allowed for this session
	pendingApprovalTool string          // tool name awaiting approval
	rerenderPending     bool            // true while rerenderOutput sequence is in flight
}

type slashCmd struct {
	cmd  string
	desc string
}

// SetProgram stores the bubbletea program reference so goroutines can
// inject messages (e.g. approval prompts) into the TUI event loop.
func (m *Model) SetProgram(p *tea.Program) {
	m.program = p
}

func (m *Model) SetBypassPermissions(bypass bool) {
	m.bypassPermissions = bypass
	if m.agentLoop != nil {
		m.agentLoop.SetBypassPermissions(bypass)
	}
}

func (m *Model) modelDisplayLabel() string {
	if m.cfg.Provider == "ollama" {
		return "ollama/" + m.cfg.Ollama.Model
	}
	return m.cfg.ModelTier
}

func (m *Model) cwd() string {
	if m.sessions != nil {
		if sess := m.sessions.Current(); sess != nil && sess.CWD != "" {
			return sess.CWD
		}
	}
	dir, _ := os.Getwd()
	return dir
}

// finishHeaderAnimation completes the startup animation, flushes the final
// header to scrollback, and transitions to stateInput.
func (m *Model) finishHeaderAnimation() tea.Cmd {
	finalHeader := renderStartupHeader(headerTotalFrames-1, m.width, m.version, m.modelDisplayLabel(), m.cfg.Endpoint, m.headerCWD, m.headerSessions, m.headerTipIdx, m.agentLabel())
	// Commit the startup banner to scrollback exactly once (write-once: no
	// rerender closure — resize keeps its original width, as in Codex/CC).
	m.appendOutput(finalHeader)
	m.appendOutput("")
	m.headerDone = true
	m.state = stateInput

	if m.headerHealth != nil {
		ep := m.cfg.Endpoint
		if m.cfg.Provider == "ollama" {
			ep = m.cfg.Ollama.Endpoint
		}
		if m.headerHealth.gatewayOK {
			m.appendOutput(fmt.Sprintf("  Connected to %s", ep))
		} else {
			m.appendOutput(fmt.Sprintf("  Warning: API unreachable at %s", ep))
		}
		if m.headerHealth.updateMsg != "" {
			m.appendOutput(fmt.Sprintf("  %s", m.headerHealth.updateMsg))
		}
		m.appendOutput("")
		m.headerHealth = nil
	}
	// Wipe the animating header from the live region, then emit the committed
	// banner + health lines to scrollback once.
	return tea.Sequence(tea.ClearScreen, m.flushPrints())
}

func New(cfg *config.Config, version string, agentOverride *agents.Agent) *Model {
	// Get terminal width for initial sizing
	width := 80
	if w, _, err := term.GetSize(int(os.Stdout.Fd())); err == nil && w > 0 {
		width = w
	}

	// Detect terminal background NOW, before tea.NewProgram grabs stdin, so the
	// OSC 11 reply isn't swallowed by the event loop. Drives both the adaptive
	// palette and the markdown renderer's light/dark selection.
	warmBackgroundColor()

	ta := textarea.New()
	ta.Placeholder = "Ask Kocoro anything…"
	promptStyle := lipgloss.NewStyle().Foreground(colorInfo)
	ta.SetPromptFunc(2, func(lineIdx int) string {
		if lineIdx == 0 {
			return promptStyle.Render("> ")
		}
		return "  "
	})
	ta.Focus()
	ta.SetHeight(1)
	ta.SetWidth(width - inputBorderOverhead)
	ta.ShowLineNumbers = false
	ta.CharLimit = 0 // unlimited
	// Remove cursor line highlight — we use border bars instead
	ta.FocusedStyle.CursorLine = lipgloss.NewStyle()
	ta.BlurredStyle.CursorLine = lipgloss.NewStyle()

	shannonDir := config.ShannonDir()
	agentsDir := filepath.Join(shannonDir, "agents")
	if err := agents.EnsureBuiltins(agentsDir, version); err != nil {
		// Non-fatal: log and continue
		log.Printf("WARNING: failed to sync builtin agents: %v", err)
	}
	if err := skills.EnsureBuiltinSkills(shannonDir); err != nil {
		log.Printf("WARNING: failed to sync builtin skills: %v", err)
	}
	sessDir := shannonDir + "/sessions"
	if agentOverride != nil {
		sessDir = filepath.Join(shannonDir, "agents", agentOverride.Name, "sessions")
	}
	sessMgr := session.NewManager(sessDir)
	sess := sessMgr.NewSession()

	initialCWD, _ := os.Getwd()
	if agentOverride != nil && agentOverride.Config != nil && agentOverride.Config.CWD != "" {
		initialCWD = agentOverride.Config.CWD
	}
	if err := cwdctx.ValidateCWD(initialCWD); err != nil {
		fallbackCWD, _ := os.Getwd()
		initialCWD = fallbackCWD
	}
	if sess != nil {
		sess.CWD = initialCWD
	}

	runtimeCfg, err := config.RuntimeConfigForCWD(cfg, initialCWD)
	if err != nil {
		log.Printf("WARNING: failed to load runtime config for %q: %v", initialCWD, err)
		runtimeCfg = config.Clone(cfg)
	}

	// Create LLM client from runtimeCfg (after project-level overlay) so
	// project-local provider overrides take effect.
	var llmClient client.LLMClient
	var gateway *client.GatewayClient
	if runtimeCfg.Provider == "ollama" {
		model := runtimeCfg.Ollama.Model
		if runtimeCfg.Agent.Model != "" {
			model = runtimeCfg.Agent.Model
		}
		llmClient = client.NewOllamaClient(runtimeCfg.Ollama.Endpoint, model)
	} else {
		gateway = client.NewGatewayClient(runtimeCfg.Endpoint, runtimeCfg.APIKey)
		if runtimeCfg.Agent.StreamIdleTimeoutSecs > 0 {
			gateway.SetStreamIdleTimeout(time.Duration(runtimeCfg.Agent.StreamIdleTimeoutSecs) * time.Second)
		}
		llmClient = gateway
	}

	// Create audit logger (best-effort)
	var auditor *audit.AuditLogger
	if shannonDir != "" {
		logDir := filepath.Join(shannonDir, "logs")
		if a, err := audit.NewAuditLogger(logDir); err == nil {
			auditor = a
		}
	}

	// Local tools only (fast, sync) — MCP + gateway loaded async in Init
	reg, skillsPtr, toolCleanup := tools.RegisterLocalTools(runtimeCfg, nil)
	tools.RegisterSessionSearch(reg, sessMgr)

	// Memory feature (Phase 2.3) — TUI attach-only path. Probe the daemon's
	// sidecar socket; if reachable, delegate via AttachedQuerier. Otherwise
	// register with a typed-nil MemoryQuerier so the tool falls back to
	// session_search + MEMORY.md.
	var memQuerier tools.MemoryQuerier
	var memPreflightQuerier tools.MemoryPreflightQuerier
	memCfg := memory.LoadConfigFromRuntime(runtimeCfg)
	if memCfg.Provider != "" && memCfg.Provider != "disabled" {
		probeCtx, probeCancel := context.WithTimeout(context.Background(), 1*time.Second)
		ready, _ := memory.AttachPolicy(probeCtx, memCfg.SocketPath)
		probeCancel()
		if ready {
			attached := memory.NewAttachedQuerier(memCfg.SocketPath, memCfg.ClientRequestTimeout)
			memQuerier = attached
			memPreflightQuerier = attached
		}
	}
	tools.RegisterMemoryTool(reg, memQuerier, &tuiMemoryFallback{sessionMgr: sessMgr})

	hookRunner := hooks.NewHookRunner(runtimeCfg.Hooks)
	loop := agent.NewAgentLoop(llmClient, reg, runtimeCfg.ModelTier, shannonDir, runtimeCfg.Agent.MaxIterations, runtimeCfg.Tools.ResultTruncation, runtimeCfg.Tools.ArgsTruncation, &runtimeCfg.Permissions, auditor, hookRunner)
	loop.SetMaxTokens(runtimeCfg.Agent.MaxTokens)
	loop.SetTemperature(runtimeCfg.Agent.Temperature)
	// Seed from the configured model and the session's last-seen model so
	// the first preflight check after a resume/agent-switch uses the right
	// cap, instead of falling back to the static config until the next
	// response arrives. sess at this point is a freshly-created session,
	// so LastSeenModel returns "" — but the configured-model path still
	// applies for known IDs.
	loop.SetContextWindow(agent.SeedContextWindowFromModels(
		runtimeCfg.Agent.Model, sess.LastSeenModel(),
		agent.ContextWindowFloorForProvider(runtimeCfg.Provider, runtimeCfg.Agent.ContextWindow)))
	// Interactive TUI — long-lived session with iteration, 1h cache pays off.
	loop.SetCacheSource("tui")
	loop.SetSkillDiscovery(runtimeCfg.Agent.SkillDiscoveryEnabled())
	if memPreflightQuerier != nil {
		var helperLLM client.LLMClient
		if gateway != nil {
			helperLLM = gateway
		}
		loop.SetMemoryPreflight(tools.NewMemoryPreflight(memPreflightQuerier, helperLLM))
	}
	loop.SetTimeBasedCompactConfig(agent.TimeBasedCompactConfig{
		Enabled:             runtimeCfg.Agent.TimeBasedCompact.Enabled,
		GapThresholdMinutes: runtimeCfg.Agent.TimeBasedCompact.GapThresholdMinutes,
		KeepRecent:          runtimeCfg.Agent.TimeBasedCompact.KeepRecent,
	})
	if runtimeCfg.Agent.Model != "" {
		loop.SetSpecificModel(runtimeCfg.Agent.Model)
	}
	if runtimeCfg.Agent.Thinking && runtimeCfg.Provider != "ollama" {
		if runtimeCfg.Agent.ThinkingMode == "enabled" {
			loop.SetThinking(&client.ThinkingConfig{Type: "enabled", BudgetTokens: runtimeCfg.Agent.ThinkingBudget})
		} else {
			loop.SetThinking(&client.ThinkingConfig{Type: "adaptive"})
		}
	}
	if runtimeCfg.Agent.ReasoningEffort != "" {
		loop.SetReasoningEffort(runtimeCfg.Agent.ReasoningEffort)
	}
	loop.SetResponseLanguage(runtimeCfg.Agent.Language)
	// Per-agent model config overrides
	if agentOverride != nil && agentOverride.Config != nil && agentOverride.Config.Agent != nil {
		ac := agentOverride.Config.Agent
		// SetModelTier and SetSpecificModel write to independent fields on the
		// loop; precedence comes from the request-time resolver, not call order
		// (see applyAgentModelOverlayToLoop in internal/daemon/runner.go).
		if ac.ModelTier != nil && *ac.ModelTier != "" {
			loop.SetModelTier(*ac.ModelTier)
		}
		// != nil (not != ""): explicit "" forces mirror over a locked global.
		if ac.Language != nil {
			loop.SetResponseLanguage(*ac.Language)
		}
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
			loop.SetContextWindowExplicit(*ac.ContextWindow)
		}
	}
	loop.SetDeltaProvider(agent.NewTemporalDelta())
	// Load skills (agent-scoped or global) and wire to loop + use_skill tool
	var loadedSkills []*skills.Skill
	if agentOverride != nil {
		loadedSkills = agentOverride.Skills
	} else {
		var err error
		loadedSkills, err = agents.LoadGlobalSkills(config.ShannonDir())
		if err != nil {
			log.Printf("WARNING: failed to load global skills: %v", err)
		}
	}
	*skillsPtr = loadedSkills

	if agentOverride != nil {
		agentDir := filepath.Join(shannonDir, "agents", agentOverride.Name)
		loop.SwitchAgent(agentOverride.Prompt, agentDir, nil, "", loadedSkills)
		loop.SetAgentName(agentOverride.Name)
		// TUI honors the same persisted always-allow set Desktop writes to.
		// Read-only — TUI has no "Always Allow" write path yet.
		merged := append([]string(nil), runtimeCfg.Permissions.AlwaysAllowTools...)
		if agentOverride.Config != nil && agentOverride.Config.Permissions != nil {
			merged = append(merged, agentOverride.Config.Permissions.AlwaysAllowTools...)
		}
		loop.SetAlwaysAllowTools(merged)
	} else {
		loop.SetAgentName("")
		loop.SetMemoryDir(filepath.Join(shannonDir, "memory"))
		if loadedSkills != nil {
			loop.SetSkills(loadedSkills)
		}
		// Default agent: only the global list applies.
		loop.SetAlwaysAllowTools(runtimeCfg.Permissions.AlwaysAllowTools)
	}
	loop.SetEnableStreaming(true) // deltas feed the live preview (OnStreamDelta); final answer rendered on agentDoneMsg
	loop.SetIdleTimeouts(runtimeCfg.Agent.IdleSoftTimeoutSecs, runtimeCfg.Agent.IdleHardTimeoutSecs)

	settings := config.LoadSettings()

	customCmds, instanceCmds := buildRuntimeCommands(shannonDir, initialCWD, agentOverride)

	m := &Model{
		baseCfg:        cfg,
		cfg:            runtimeCfg,
		gateway:        gateway,
		llmClient:      llmClient,
		sessions:       sessMgr,
		agentLoop:      loop,
		textarea:       ta,
		width:          width,
		version:        version,
		approvalCh:     make(chan bool, 1),
		spinnerTexts:   settings.SpinnerTexts,
		toolRegistry:   reg,
		toolCleanup:    toolCleanup,
		shannonDir:     shannonDir,
		auditor:        auditor,
		hookRunner:     hookRunner,
		customCommands: customCmds,
		agentOverride:  agentOverride,
		loadedSkills:   loadedSkills,
		skillsPtr:      skillsPtr,
		memPreflight:   memPreflightQuerier,
		markdownCache:  make(map[string]string),
		slashCommands:  instanceCmds,
		sessionAllowed: make(map[string]bool),
		historyIdx:     -1,
	}

	return m
}

func buildRuntimeCommands(shannonDir, projectDir string, agentOverride *agents.Agent) (map[string]string, []slashCmd) {
	customCmds, _ := instructions.LoadCustomCommands(shannonDir, projectDir)
	if customCmds == nil {
		customCmds = make(map[string]string)
	}

	instanceCmds := make([]slashCmd, len(baseSlashCommands))
	copy(instanceCmds, baseSlashCommands)
	for name := range customCmds {
		instanceCmds = append(instanceCmds, slashCmd{
			cmd:  "/" + name,
			desc: "Custom command",
		})
	}

	builtinCmds := agents.BuiltinCommands
	if agentOverride != nil {
		for name, content := range agentOverride.Commands {
			if builtinCmds[name] {
				continue
			}
			customCmds[name] = content
			instanceCmds = append(instanceCmds, slashCmd{
				cmd:  "/" + name,
				desc: "Agent command",
			})
		}
		for _, s := range agentOverride.Skills {
			if s.Prompt != "" && !builtinCmds[s.Name] {
				customCmds[s.Name] = s.Prompt
				instanceCmds = append(instanceCmds, slashCmd{
					cmd:  "/" + s.Name,
					desc: s.Description,
				})
			}
		}
	}

	return customCmds, instanceCmds
}

func (m *Model) rebuildAgentLoop() {
	if m == nil || m.cfg == nil || m.toolRegistry == nil {
		return
	}

	m.hookRunner = hooks.NewHookRunner(m.cfg.Hooks)
	loop := agent.NewAgentLoop(m.llmClient, m.toolRegistry, m.cfg.ModelTier, m.shannonDir, m.cfg.Agent.MaxIterations, m.cfg.Tools.ResultTruncation, m.cfg.Tools.ArgsTruncation, &m.cfg.Permissions, m.auditor, m.hookRunner)
	loop.SetMaxTokens(m.cfg.Agent.MaxTokens)
	loop.SetTemperature(m.cfg.Agent.Temperature)
	// Seed the soft context window from the configured model + the
	// currently-active session's last-seen model. After an agent switch
	// the session may already carry usage from prior turns served by a
	// 1M-context model; without this, preflight would re-seed at the
	// static config and over-truncate until the next response arrives.
	var resumedSeenModel string
	if sess := m.sessions.Current(); sess != nil {
		resumedSeenModel = sess.LastSeenModel()
	}
	loop.SetContextWindow(agent.SeedContextWindowFromModels(
		m.cfg.Agent.Model, resumedSeenModel,
		agent.ContextWindowFloorForProvider(m.cfg.Provider, m.cfg.Agent.ContextWindow)))
	// Interactive TUI (switched agent) — same routing as the primary loop.
	loop.SetCacheSource("tui")
	loop.SetSkillDiscovery(m.cfg.Agent.SkillDiscoveryEnabled())
	if m.memPreflight != nil {
		var helperLLM client.LLMClient
		if m.gateway != nil {
			helperLLM = m.gateway
		}
		loop.SetMemoryPreflight(tools.NewMemoryPreflight(m.memPreflight, helperLLM))
	}
	if m.cfg.Agent.Model != "" {
		loop.SetSpecificModel(m.cfg.Agent.Model)
	} else if m.cfg.Provider == "ollama" && m.cfg.Ollama.Model != "" {
		loop.SetSpecificModel(m.cfg.Ollama.Model)
	}
	if m.cfg.Agent.Thinking && m.cfg.Provider != "ollama" {
		if m.cfg.Agent.ThinkingMode == "enabled" {
			loop.SetThinking(&client.ThinkingConfig{Type: "enabled", BudgetTokens: m.cfg.Agent.ThinkingBudget})
		} else {
			loop.SetThinking(&client.ThinkingConfig{Type: "adaptive"})
		}
	}
	if m.cfg.Agent.ReasoningEffort != "" {
		loop.SetReasoningEffort(m.cfg.Agent.ReasoningEffort)
	}
	loop.SetResponseLanguage(m.cfg.Agent.Language)
	if m.agentOverride != nil && m.agentOverride.Config != nil && m.agentOverride.Config.Agent != nil {
		ac := m.agentOverride.Config.Agent
		// SetModelTier and SetSpecificModel write to independent fields on the
		// loop; precedence comes from the request-time resolver, not call order
		// (see applyAgentModelOverlayToLoop in internal/daemon/runner.go).
		if ac.ModelTier != nil && *ac.ModelTier != "" {
			loop.SetModelTier(*ac.ModelTier)
		}
		// != nil (not != ""): explicit "" forces mirror over a locked global.
		if ac.Language != nil {
			loop.SetResponseLanguage(*ac.Language)
		}
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
			loop.SetContextWindowExplicit(*ac.ContextWindow)
		}
	}
	loop.SetBypassPermissions(m.bypassPermissions)
	loop.SetEnableStreaming(true)
	loop.SetDeltaProvider(agent.NewTemporalDelta())
	if m.agentOverride != nil {
		scopedMCPCtx := tools.ResolveMCPContext(m.cfg, m.agentOverride)
		agentDir := filepath.Join(m.shannonDir, "agents", m.agentOverride.Name)
		loop.SwitchAgent(m.agentOverride.Prompt, agentDir, nil, scopedMCPCtx, m.loadedSkills)
		loop.SetAgentName(m.agentOverride.Name)
		merged := append([]string(nil), m.cfg.Permissions.AlwaysAllowTools...)
		if m.agentOverride.Config != nil && m.agentOverride.Config.Permissions != nil {
			merged = append(merged, m.agentOverride.Config.Permissions.AlwaysAllowTools...)
		}
		loop.SetAlwaysAllowTools(merged)
	} else {
		loop.SetAgentName("")
		loop.SetMemoryDir(filepath.Join(m.shannonDir, "memory"))
		if m.loadedSkills != nil {
			loop.SetSkills(m.loadedSkills)
		}
		mcpCtx := tools.ResolveMCPContext(m.cfg)
		if mcpCtx != "" {
			loop.SetMCPContext(mcpCtx)
		}
		loop.SetAlwaysAllowTools(m.cfg.Permissions.AlwaysAllowTools)
	}
	m.agentLoop = loop
}

func (m *Model) applyRuntimeContext(sess *session.Session) string {
	var sessionCWD string
	if m.resumedSession && sess != nil {
		sessionCWD = sess.CWD
	}
	var agentCWD string
	if m.agentOverride != nil && m.agentOverride.Config != nil {
		agentCWD = m.agentOverride.Config.CWD
	}
	effectiveCWD := cwdctx.ResolveEffectiveCWD("", sessionCWD, agentCWD)
	// TUI runs in the user's shell — when nothing is configured explicitly,
	// default to the terminal's current directory so project-level configs are
	// picked up. Daemon-routed runs use a different default (empty + guard).
	if effectiveCWD == "" {
		effectiveCWD, _ = os.Getwd()
	}
	if err := cwdctx.ValidateCWD(effectiveCWD); err != nil {
		fmt.Fprintf(os.Stderr, "[tui] invalid session CWD %q, falling back to process CWD: %v\n", effectiveCWD, err)
		effectiveCWD, _ = os.Getwd()
	}
	if sess != nil {
		sess.CWD = effectiveCWD
	}

	runCfg, err := config.RuntimeConfigForCWD(m.baseCfg, effectiveCWD)
	if err != nil {
		log.Printf("WARNING: failed to load runtime config for %q: %v", effectiveCWD, err)
		runCfg = config.Clone(m.baseCfg)
	}
	m.cfg = runCfg
	m.customCommands, m.slashCommands = buildRuntimeCommands(m.shannonDir, effectiveCWD, m.agentOverride)
	m.toolRegistry = tools.CloneWithRuntimeConfig(m.toolRegistry, m.cfg)
	m.rebuildAgentLoop()
	m.updateMenu()
	return effectiveCWD
}

func (m *Model) Init() tea.Cmd {
	m.state = stateStartup
	m.headerFrame = 0
	m.headerSessions, _ = m.sessions.List()
	m.headerTipIdx = pickTipIdx()
	m.headerCWD = m.cwd()
	m.hookRunner.RunSessionStart(context.Background(), "")

	// Auto-set Ghostty tab title + color for this agent
	if m.agentOverride != nil {
		tools.SetGhosttyTabAppearance(m.agentOverride.Name)
	}

	return tea.Batch(
		textarea.Blink,
		headerFrameTick(),
		m.checkHealth(),
		m.loadServerTools(),
	)
}

func (m *Model) loadServerTools() tea.Cmd {
	return func() tea.Msg {
		if m.toolRegistry == nil {
			return serverToolsLoadedMsg{err: fmt.Errorf("tool registry not initialized")}
		}

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		reg, _, cleanup, err := tools.CompleteRegistration(ctx, m.gateway, m.cfg, m.toolRegistry, m.agentOverride)

		// Cloud delegation tool (gateway only)
		if m.gateway != nil {
			var cloudAgentName, cloudAgentPrompt string
			if m.agentOverride != nil {
				cloudAgentName = m.agentOverride.Name
				cloudAgentPrompt = m.agentOverride.Prompt
			}
			tools.RegisterCloudDelegate(reg, m.gateway, m.cfg, nil, cloudAgentName, cloudAgentPrompt)
			tools.RegisterPublishTool(reg, m.gateway, m.cfg)
			tools.RegisterListPublishedFilesTool(reg, m.gateway, m.cfg)
			tools.RegisterRetractPublishedFileTool(reg, m.gateway, m.cfg)
			tools.RegisterGenerateImageTool(reg, m.gateway, m.cfg)
			tools.RegisterEditImageTool(reg, m.gateway, m.cfg)
		}

		return serverToolsLoadedMsg{
			registry: reg,
			cleanup:  cleanup,
			err:      err,
		}
	}
}

func (m *Model) checkHealth() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()

		msg := healthCheckMsg{}
		if m.gateway != nil {
			msg.gatewayOK = m.gateway.Health(ctx) == nil
		} else if oc, ok := m.llmClient.(*client.OllamaClient); ok {
			msg.gatewayOK = oc.CheckHealth(ctx) == nil
		} else {
			msg.gatewayOK = true
		}

		if m.cfg.AutoUpdateCheck {
			shannonDir := config.ShannonDir()
			if shannonDir != "" {
				msg.updateMsg = update.AutoUpdate(m.version, shannonDir)
			}
		}
		return msg
	}
}

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	model, cmd := m.update(msg)
	// Suppress incremental flushes while a rerenderOutput sequence is in
	// flight — prevents streamOutputMsg from interleaving between
	// ClearScreen and Println (Bug #3 fix).
	if !m.rerenderPending {
		if flush := m.flushPrints(); flush != nil {
			if cmd != nil {
				cmd = tea.Sequence(flush, cmd)
			} else {
				cmd = flush
			}
		}
	}
	return model, cmd
}

func (m *Model) update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		// During startup animation: Ctrl+C quits, any other key skips animation
		if m.state == stateStartup && !m.headerDone && msg.Type != tea.KeyCtrlC {
			m.headerFrame = headerTotalFrames - 1
			return m, m.finishHeaderAnimation()
		}

		switch msg.Type {
		case tea.KeyCtrlC:
			m.hookRunner.RunStop(context.Background(), "")
			m.sessions.Save()
			m.sessions.Close()
			if m.toolCleanup != nil {
				m.toolCleanup()
			}
			if m.remoteCleanup != nil {
				m.remoteCleanup()
			}
			return m, tea.Quit
		case tea.KeyEscape:
			if m.state == stateProcessing || m.state == stateApproval {
				m.streamLive = "" // drop any in-flight preview on cancel
				if m.cancelRun != nil {
					m.cancelRun()
					m.cancelRun = nil
					m.injectCh = nil
				}
				// Unblock approval goroutine if waiting
				if m.state == stateApproval {
					select {
					case m.approvalCh <- false:
					default:
					}
				}
				// Don't roll back the user message — let the agent loop's
				// RunMessages be saved by runAgentLoop when it completes.
				// This preserves tool calls and partial responses so the
				// next run has full context of what happened before cancel.
				cancelStyle := lipgloss.NewStyle().Foreground(colorDim)
				m.appendOutput(cancelStyle.Render("  [Cancelled]"))
				m.state = stateInput
				return m, m.rerenderOutput()
			}
			if m.menuVisible {
				m.menuVisible = false
				return m, nil
			}
			if m.state == stateInput && m.textarea.Value() != "" {
				now := time.Now()
				if !m.lastEscTime.IsZero() && now.Sub(m.lastEscTime) < 800*time.Millisecond {
					m.textarea.SetValue("")
					m.textarea.SetHeight(1)
					m.lastEscTime = time.Time{}
					return m, nil
				}
				m.lastEscTime = now
				return m, nil
			}
		case tea.KeyTab:
			if m.menuVisible && len(m.menuItems) > 0 {
				selected := m.menuItems[m.menuIndex]
				m.textarea.SetValue(selected.cmd + " ")
				m.menuVisible = false
				return m, nil
			}
		case tea.KeyEnter:
			// Alt+Enter: insert newline instead of submitting
			if m.state == stateInput && !m.menuVisible && msg.Alt {
				m.textarea.InsertString("\n")
				m.adjustTextareaHeight()
				return m, nil
			}
			if m.menuVisible && len(m.menuItems) > 0 {
				selected := m.menuItems[m.menuIndex]
				m.menuVisible = false
				if isImmediateCommand(selected.cmd) {
					// No-argument command (e.g. a picker) — execute on this Enter
					// instead of autocompleting and waiting for a second Enter.
					m.textarea.SetValue(selected.cmd)
					return m.handleSubmit()
				}
				// Needs a typed argument — autocomplete and let the user type it.
				m.textarea.SetValue(selected.cmd + " ")
				return m, nil
			}
			if m.state == stateApproval {
				// handled below
			} else if m.state == stateInput {
				return m.handleSubmit()
			}
		case tea.KeyUp:
			if m.state == stateInput && m.menuVisible && len(m.menuItems) > 0 {
				m.menuIndex--
				if m.menuIndex < 0 {
					m.menuIndex = len(m.menuItems) - 1
				}
				return m, nil
			}
			if m.state == stateInput && !m.menuVisible && len(m.inputHistory) > 0 {
				taLines := strings.Count(m.textarea.Value(), "\n") + 1
				if taLines <= 1 { // only navigate history when single-line
					if m.historyIdx == -1 {
						m.historySaved = m.textarea.Value()
					}
					newIdx := m.historyIdx + 1
					histLen := len(m.inputHistory)
					if newIdx >= histLen {
						newIdx = histLen - 1
					}
					m.historyIdx = newIdx
					m.textarea.SetValue(m.inputHistory[histLen-1-newIdx])
					m.textarea.CursorEnd()
					return m, nil
				}
			}
		case tea.KeyDown:
			if m.state == stateInput && m.menuVisible && len(m.menuItems) > 0 {
				m.menuIndex++
				if m.menuIndex >= len(m.menuItems) {
					m.menuIndex = 0
				}
				return m, nil
			}
			if m.state == stateInput && !m.menuVisible && m.historyIdx >= 0 {
				taLines := strings.Count(m.textarea.Value(), "\n") + 1
				if taLines <= 1 {
					m.historyIdx--
					if m.historyIdx < 0 {
						m.textarea.SetValue(m.historySaved)
					} else {
						histLen := len(m.inputHistory)
						m.textarea.SetValue(m.inputHistory[histLen-1-m.historyIdx])
					}
					m.textarea.CursorEnd()
					return m, nil
				}
			}
		}

		// Ctrl+O: expand tool results from last turn (one-shot, shows expanded details)
		if msg.String() == "ctrl+o" && len(m.lastToolResults) > 0 && m.toolExpandLevel == 0 {
			for _, r := range m.lastToolResults {
				m.appendOutput(formatExpandedToolResult(r.name, r.args, r.isError, r.content, r.elapsed))
			}
			m.toolExpandLevel = 1
			return m, m.flushPrints()
		}

		// Readline shortcuts (only in stateInput, single-line, not during menus).
		// CharOffset is relative to the current wrapped line, so these shortcuts
		// would slice the wrong position in multi-line input.
		taLines := strings.Count(m.textarea.Value(), "\n") + 1
		if m.state == stateInput && !m.menuVisible && taLines <= 1 {
			switch msg.Type {
			case tea.KeyCtrlK: // Delete to end of line
				val := m.textarea.Value()
				pos := m.textarea.LineInfo().CharOffset
				runes := []rune(val)
				if pos < len(runes) {
					m.textarea.SetValue(string(runes[:pos]))
				}
				return m, nil
			case tea.KeyCtrlU: // Delete to start of line
				val := m.textarea.Value()
				pos := m.textarea.LineInfo().CharOffset
				runes := []rune(val)
				if pos > 0 && pos <= len(runes) {
					m.textarea.SetValue(string(runes[pos:]))
					m.textarea.CursorStart()
				}
				return m, nil
			case tea.KeyCtrlW: // Delete word backward
				val := m.textarea.Value()
				pos := m.textarea.LineInfo().CharOffset
				runes := []rune(val)
				if pos > 0 && pos <= len(runes) {
					i := pos - 1
					for i > 0 && runes[i] == ' ' {
						i--
					}
					for i > 0 && runes[i-1] != ' ' {
						i--
					}
					newVal := string(runes[:i]) + string(runes[pos:])
					m.textarea.SetValue(newVal)
					m.textarea.SetCursor(i)
				}
				return m, nil
			case tea.KeyCtrlL: // Clear screen
				m.output = nil
				return m, m.rerenderOutput()
			}
		}

		if m.state == stateSessionPicker {
			switch msg.Type {
			case tea.KeyUp:
				m.sessionPickerIdx--
				if m.sessionPickerIdx < 0 {
					m.sessionPickerIdx = len(m.lastSessions) - 1
				}
				return m, nil
			case tea.KeyDown:
				m.sessionPickerIdx++
				if m.sessionPickerIdx >= len(m.lastSessions) {
					m.sessionPickerIdx = 0
				}
				return m, nil
			case tea.KeyEnter:
				if len(m.lastSessions) > 0 {
					target := m.lastSessions[m.sessionPickerIdx].ID
					sess, err := m.sessions.Resume(target)
					if err != nil {
						m.appendOutput(fmt.Sprintf("Error: %v", err))
					} else {
						m.resumedSession = true
						m.sessionAllowed = make(map[string]bool)
						m.applyRuntimeContext(sess)
						m.loadSessionHistory(sess)
					}
				}
				m.state = stateInput
				return m, nil
			case tea.KeyEscape:
				m.state = stateInput
				return m, nil
			}
			return m, nil
		}

		if m.state == statePicker {
			switch msg.Type {
			case tea.KeyUp:
				m.pickerIdx = pickerWrap(m.pickerIdx-1, len(m.pickerOpts))
				return m, nil
			case tea.KeyDown:
				m.pickerIdx = pickerWrap(m.pickerIdx+1, len(m.pickerOpts))
				return m, nil
			case tea.KeyEnter:
				m.state = stateInput
				if len(m.pickerOpts) > 0 {
					sel := m.pickerOpts[m.pickerIdx].value
					switch m.pickerKind {
					case pickerKindModel:
						m.applyModelTier(sel)
						return m, m.flushPrints()
					case pickerKindAgent:
						return m, m.switchToAgent(sel)
					}
				}
				return m, nil
			case tea.KeyEscape:
				m.state = stateInput
				return m, nil
			}
			return m, nil
		}

		if m.state == stateApproval {
			switch msg.String() {
			case "y", "Y":
				select {
				case m.approvalCh <- true:
				default:
				}
				m.state = stateProcessing
				return m, nil
			case "n", "N":
				select {
				case m.approvalCh <- false:
				default:
				}
				m.state = stateProcessing
				return m, nil
			case "a", "A":
				if !agent.DisallowsAutoApproval(m.pendingApprovalTool) {
					m.sessionAllowed[m.pendingApprovalTool] = true
				} else {
					m.sendOutput("  ! Allowed once; this tool cannot be saved as always-allow.")
				}
				select {
				case m.approvalCh <- true:
				default:
				}
				m.state = stateProcessing
				return m, nil
			}
			return m, nil
		}

	case tea.WindowSizeMsg:
		oldWidth := m.width
		m.width = msg.Width
		m.height = msg.Height
		m.textarea.SetWidth(msg.Width - inputBorderOverhead)
		if oldWidth != msg.Width && oldWidth > 0 && len(m.output) > 0 {
			return m, m.rerenderOutput()
		}
		return m, nil

	case spinnerFrameMsg:
		if m.state == stateProcessing {
			m.glyphIdx++
			m.colorIdx++
			return m, spinnerFrameTick()
		}
		return m, nil

	case spinnerTickMsg:
		if m.state == stateProcessing {
			m.spinnerIdx = (m.spinnerIdx + 1) % len(m.spinnerTexts)
			return m, spinnerTick()
		}
		return m, nil

	case agentDoneMsg:
		// If already back to stateInput (Esc was pressed), ignore this message.
		// The Esc handler already showed [Cancelled] and transitioned state.
		if m.state != stateProcessing {
			return m, nil
		}
		m.state = stateInput
		m.streamLive = "" // final answer is rendered to scrollback below
		m.cancelRun = nil
		m.injectCh = nil
		if msg.err != nil && !errors.Is(msg.err, context.Canceled) && !errors.Is(msg.err, agent.ErrMaxIterReached) {
			code := msg.status.FailureCode
			if code == runstatus.CodeNone {
				code = runstatus.CodeFromError(msg.err)
			}
			m.appendOutput("Error: " + runstatus.FriendlyMessage(code))
		}
		// Display the assistant response (rendered here instead of OnText to
		// avoid a race where the Println Cmd arrives after state has changed).
		if msg.result != "" && (msg.err == nil || errors.Is(msg.err, agent.ErrMaxIterReached)) {
			m.appendMarkdownOutput(msg.result, m.renderMarkdownCached(msg.result, m.width))
			m.appendOutput("")
			// Soft warning for loop-detector force-stop: the reply is valid
			// and rendered above, but the run ended early. Show a dim hint,
			// not a red error.
			if msg.err == nil && msg.status.Partial && msg.status.FailureCode == runstatus.CodeIterationLimit {
				dim := lipgloss.NewStyle().Foreground(colorDim).Italic(true)
				m.appendOutput(dim.Render("  Stopped early after repeated failed attempts."))
			}
		}
		// Tool count summary (individual tool lines already shown during execution)
		if len(m.lastToolResults) > 0 {
			m.toolExpandLevel = 0
		}
		// Don't show usage/elapsed for cancelled tasks
		if msg.err == nil || errors.Is(msg.err, agent.ErrMaxIterReached) {
			elapsed := formatElapsed(time.Since(m.processingStartTime))
			usageDim := lipgloss.NewStyle().Foreground(colorDim)
			// Prefer session's cumulative usage (captures direct LLM + cloud_delegate
			// nested LLM calls) over msg.usage (direct LLM only from loop.Run).
			var sessionUsage *session.UsageSummary
			if sess := m.sessions.Current(); sess != nil {
				sessionUsage = sess.Usage
			}
			switch {
			case sessionUsage != nil && (sessionUsage.InputTokens > 0 || sessionUsage.OutputTokens > 0):
				// Show the combined total as "cost:". Resumed sessions may
				// carry a mix of pre-split and split-aware writes (e.g. a
				// legacy session that accrued more spend after upgrading),
				// so an llm/tools breakdown cannot be rendered accurately
				// from the stored summary alone. Users who want the per-
				// turn breakdown can see it in the one-shot CLI footer.
				total := sessionUsage.CostUSD + sessionUsage.ToolCostUSD
				usageStr := fmt.Sprintf("  tokens: %d in / %d out | cost: $%.4f | calls: %d",
					sessionUsage.InputTokens, sessionUsage.OutputTokens,
					total, sessionUsage.LLMCalls)
				if sessionUsage.Model != "" {
					usageStr += " | " + sessionUsage.Model
				}
				usageStr += " | " + elapsed
				m.appendOutput(usageDim.Render(usageStr))
			case msg.usage != nil:
				usageStr := fmt.Sprintf("  tokens: %d | cost: $%.4f", msg.usage.TotalTokens, msg.usage.CostUSD)
				if msg.usage.Model != "" {
					usageStr += " | model: " + msg.usage.Model
				}
				usageStr += " | " + elapsed
				m.appendOutput(usageDim.Render(usageStr))
			default:
				m.appendOutput(usageDim.Render("  " + elapsed))
			}
		}
		m.sessions.Save()
		// Smart session title: upgrade the placeholder asynchronously on a
		// successful turn (same shared core as the daemon path). tea.Batch
		// drops a nil Cmd, so this is a no-op when gating fails.
		var titleCmd tea.Cmd
		if msg.err == nil || errors.Is(msg.err, agent.ErrMaxIterReached) {
			if sess := m.sessions.Current(); sess != nil {
				titleCmd = m.generateTitleCmd(sess.ID, sess.Source, sess.Messages, ctxwin.CountCompletedTurns(sess.Messages))
			}
		}
		// Full clear-and-repaint so the response, usage line, and input bar
		// are all positioned correctly — incremental Println can mis-position
		// lines when the view height changes between processing and input.
		return m, tea.Batch(m.rerenderOutput(), titleCmd)

	case titleGeneratedMsg:
		// The smart title was generated off-thread; persist it here on the main
		// goroutine so all session mutation stays single-threaded (the
		// background goroutine must not write the session the update loop also
		// mutates unlocked). PatchAutoTitle re-checks the user-lock / straggler
		// guards. Refresh the cached session list on a successful write so the
		// startup header / sidebar re-render with the upgraded title.
		if ok, _ := m.sessions.PatchAutoTitle(msg.sessionID, msg.title, msg.atTurns); ok {
			m.headerSessions, _ = m.sessions.List()
		}
		return m, nil

	case approvalRequestMsg:
		m.pendingApprovalTool = msg.tool
		// Check session-level auto-approve
		if m.sessionAllowed[msg.tool] && !agent.DisallowsAutoApproval(msg.tool) {
			select {
			case m.approvalCh <- true:
			default:
			}
			return m, nil
		}
		m.state = stateApproval
		dimStyle := lipgloss.NewStyle().Foreground(colorDim)
		warnIcon := lipgloss.NewStyle().Foreground(colorWarn).Render("?")
		keyArg := toolKeyArg(msg.tool, msg.args)
		m.appendOutput(dimStyle.Render(fmt.Sprintf("⏵ %s(%s)  %s  Allow? [y/n/a]", msg.tool, keyArg, warnIcon)))
		// Full repaint on state transition to avoid cursor mis-positioning
		// (same race as agentDoneMsg — view changes before pending Println arrives).
		return m, m.rerenderOutput()

	case serverToolsLoadedMsg:
		if msg.cleanup != nil {
			m.remoteCleanup = msg.cleanup
		}
		if msg.registry != nil {
			m.toolRegistry = tools.CloneWithRuntimeConfig(msg.registry, m.cfg)
			m.rebuildAgentLoop()
		}
		return m, nil

	case headerTickMsg:
		if m.headerDone {
			return m, nil
		}
		m.headerFrame++
		if m.headerFrame >= headerTotalFrames {
			return m, m.finishHeaderAnimation()
		}
		return m, headerFrameTick()

	case healthCheckMsg:
		if !m.headerDone {
			m.headerHealth = &msg
			return m, nil
		}
		if msg.gatewayOK {
			m.appendOutput(fmt.Sprintf("  Connected to %s", m.cfg.Endpoint))
		} else {
			m.appendOutput(fmt.Sprintf("  Warning: API unreachable at %s", m.cfg.Endpoint))
		}
		if msg.updateMsg != "" {
			m.appendOutput(fmt.Sprintf("  %s", msg.updateMsg))
		}
		m.appendOutput("")
		return m, nil

	case streamDeltaMsg:
		// Accumulate the in-flight answer into the transient preview region.
		// Not committed to scrollback — agentDoneMsg renders the final answer.
		// Bound to a tail: the preview only shows the last streamPreviewLines, so
		// there's no need to retain (and re-split every View) a 100K-char answer.
		m.streamLive = boundStreamTail(m.streamLive + msg.delta)
		return m, nil

	case streamOutputMsg:
		// Something is being committed to scrollback (a preamble, a status, or a
		// cloud delta); the live preview for the just-finished segment is now
		// redundant — drop it so it can't duplicate.
		m.streamLive = ""
		if msg.raw != "" {
			m.appendMarkdownOutput(msg.raw, msg.text)
		} else {
			m.appendOutput(msg.text)
		}
		return m, nil

	case toolCallMsg:
		m.streamLive = ""
		m.pendingToolName = msg.name
		m.pendingToolArgs = msg.args
		// Advance spinner phrase on real events
		m.spinnerIdx = (m.spinnerIdx + 1) % len(m.spinnerTexts)
		return m, nil

	case toolResultMsg:
		m.streamLive = ""
		// Prefer the result event's own (name, args) — they are paired with the
		// specific tool_use_id that produced this result. The pendingTool*
		// scalars are a singleton-style spinner hint and would mis-pair when
		// multiple concurrency-safe tools are in flight (e.g. parallel bash);
		// fall back to them only if the event omits both (legacy callers).
		toolName := msg.name
		toolArgs := msg.args
		if toolName == "" {
			toolName = m.pendingToolName
		}
		if toolArgs == "" {
			toolArgs = m.pendingToolArgs
		}
		if toolName == "think" {
			dimStyle := lipgloss.NewStyle().Foreground(colorDim)
			m.appendOutput(dimStyle.Render(msg.content))
		} else {
			m.appendOutput(formatCompactToolResult(toolName, toolArgs, msg.isError, msg.content, msg.elapsed))
			entry := toolResultEntry{name: toolName, args: toolArgs, content: msg.content, isError: msg.isError, elapsed: msg.elapsed}
			m.lastToolResults = append(m.lastToolResults, entry)
			if len(m.lastToolResults) > 20 {
				m.lastToolResults = m.lastToolResults[1:]
			}
		}
		m.pendingToolName = ""
		m.pendingToolArgs = ""
		m.toolExpandLevel = 0
		return m, nil

	case doctorDoneMsg:
		m.state = stateInput
		m.appendOutput(formatDoctorResults(msg.checks))
		return m, m.rerenderOutput()

	case compactDoneMsg:
		m.state = stateInput
		if msg.err != nil {
			m.appendOutput(fmt.Sprintf("Compact failed: %v", msg.err))
		} else {
			m.appendOutput(formatCompactResult(msg))
		}
		return m, m.rerenderOutput()

	case rerenderDoneMsg:
		m.rerenderPending = false
		// Flush any output that arrived during the rerender sequence
		if flush := m.flushPrints(); flush != nil {
			return m, flush
		}
		return m, nil

	case historyLoadedMsg:
		// Re-render at current width in case terminal was resized during load
		return m, m.rerenderOutput()

	case clipboardResultMsg:
		if msg.err != nil {
			m.appendOutput(fmt.Sprintf("Copy failed: %v", msg.err))
		} else {
			m.appendOutput(fmt.Sprintf("Copied to clipboard (%d chars)", msg.len))
		}
		return m, nil
	}

	if m.state == stateInput {
		var taCmd tea.Cmd
		m.textarea, taCmd = m.textarea.Update(msg)
		m.adjustTextareaHeight()
		m.updateMenu()
		return m, taCmd
	}
	return m, nil
}

// streamPreviewLines bounds the height of the in-flight answer preview. Small
// on purpose: it is a transient "being typed" hint under the spinner, not a
// scrollback replacement (the full answer is rendered on completion). Override:
// none today — bump if the live region feels too cramped.
const streamPreviewLines = 8

// streamLiveMaxBytes caps the retained preview buffer. Only the last
// streamPreviewLines are ever shown, so there's no point keeping (and
// re-splitting each frame) more than a few screenfuls of a long answer.
const streamLiveMaxBytes = 8192

// boundStreamTail trims s to its last streamLiveMaxBytes, cut at a line boundary
// so the preview never starts mid-line. Keeps streamPreview's per-frame work
// O(streamLiveMaxBytes) regardless of total answer length.
func boundStreamTail(s string) string {
	if len(s) <= streamLiveMaxBytes {
		return s
	}
	tail := s[len(s)-streamLiveMaxBytes:]
	if i := strings.IndexByte(tail, '\n'); i >= 0 {
		return tail[i+1:]
	}
	return tail
}

// streamPreview returns the last maxLines lines of the in-flight stream, each
// truncated to the terminal width and dimmed. It deliberately truncates rather
// than wraps so the live region stays a fixed height instead of ballooning.
func streamPreview(text string, width, maxLines int) string {
	text = strings.TrimRight(text, "\n")
	if text == "" {
		return ""
	}
	lines := strings.Split(text, "\n")
	if len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}
	dim := styleDim()
	var sb strings.Builder
	for i, ln := range lines {
		if i > 0 {
			sb.WriteString("\n")
		}
		if width > 0 {
			ln = truncateCells(ln, width, "…")
		}
		sb.WriteString(dim.Render(ln))
	}
	return sb.String()
}

// composeBar renders a full-width status separator with optional captions
// embedded at its left and right ends: <left>────────<right>. Both captions may
// already be ANSI-styled; the fill uses the faint separator color. Width is
// measured with lipgloss.Width so CJK/ANSI is accounted for.
func composeBar(width int, left, right string) string {
	if width < 0 {
		width = 0
	}
	fill := width - lipgloss.Width(left) - lipgloss.Width(right)
	if fill < 0 {
		// Captions can't both fit; fall back to a plain full-width separator so
		// the bar never overflows and wraps the input line on narrow terminals.
		return styleFaint().Render(strings.Repeat("─", width))
	}
	return left + styleFaint().Render(strings.Repeat("─", fill)) + right
}

// inputBorderOverhead reserves columns around the composer: 1 left border +
// 1 right border + 1 trailing column left blank. The trailing blank is
// load-bearing: a live line that fills the FULL terminal width gets no
// EraseLineRight from Bubbletea's inline differ (it only erases lines shorter
// than the width), which desyncs line accounting and clips the trailing status
// bar on each keystroke. Keeping the box 1 column short keeps the differ honest.
const inputBorderOverhead = 3

// statusAgentMarker leads the input status line — a brand-colored bar that
// draws the eye to the active-agent segment.
const statusAgentMarker = "▌"

// renderInputBox wraps the composer view in a rounded, brand-colored border of
// the given total width. Narrow widths pass the content through unboxed so the
// frame never overflows.
func renderInputBox(taView string, totalWidth int) string {
	if totalWidth < 4 {
		return taView
	}
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(colorAccent).
		Width(totalWidth - inputBorderOverhead).
		Render(taView)
}

func (m *Model) View() string {
	var sb strings.Builder

	barStyle := lipgloss.NewStyle().Foreground(colorFaint)
	bar := barStyle.Render(strings.Repeat("─", m.width))

	// --- Input / status line ---
	switch m.state {
	case stateStartup:
		sb.WriteString(renderStartupHeader(m.headerFrame, m.width, m.version, m.modelDisplayLabel(), m.cfg.Endpoint, m.headerCWD, m.headerSessions, m.headerTipIdx, m.agentLabel()))
	case stateInput:
		// Composer wrapped in a rounded brand-colored border (its top border
		// replaces the old plain separator). The textarea is sized to leave room
		// for the border (inputBorderOverhead) at init/resize.
		sb.WriteString(renderInputBox(m.textarea.View(), m.width))
		sb.WriteString("\n")
		// Status line: the active agent is the prominent left segment (brand
		// marker + bold name) followed by the model tier; the slash hint sits
		// dim on the right. agentLabel is a persistent control (Desktop).
		marker := lipgloss.NewStyle().Foreground(colorAccent).Render(statusAgentMarker)
		agentSeg := lipgloss.NewStyle().Foreground(colorAccent).Bold(true).Render(m.agentLabel())
		modelSeg := styleSecondary().Render(m.modelDisplayLabel())
		left := " " + marker + " " + agentSeg + " " + styleDim().Render("·") + " " + modelSeg
		right := styleDim().Render("/ for commands")
		sb.WriteString(composeBar(m.width, left, right))
	case stateProcessing:
		// Live preview of the answer being generated (transient; the finalized
		// answer is rendered to scrollback on agentDoneMsg). Shown above the
		// spinner so the user sees real-time progress instead of a frozen dot.
		if preview := streamPreview(m.streamLive, m.width, streamPreviewLines); preview != "" {
			sb.WriteString(preview)
			sb.WriteString("\n")
		}
		if m.pendingToolName != "" {
			glyph := dotFrames[m.glyphIdx%len(dotFrames)]
			color := spinColors[m.colorIdx%len(spinColors)]
			glyphStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(color))
			dimStyle := lipgloss.NewStyle().Foreground(colorDim)
			keyArg := toolKeyArg(m.pendingToolName, m.pendingToolArgs)
			sb.WriteString(glyphStyle.Render(glyph) + dimStyle.Render(fmt.Sprintf(" %s(%s)", m.pendingToolName, keyArg)))
		} else {
			glyph := dotFrames[m.glyphIdx%len(dotFrames)]
			color := spinColors[m.colorIdx%len(spinColors)]
			glyphStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(color))
			spinnerText := m.spinnerTexts[m.spinnerIdx%len(m.spinnerTexts)]
			sb.WriteString(glyphStyle.Render(glyph) + " " + renderWaveText(spinnerText, m.glyphIdx))
		}
		sb.WriteString("\n")
		// Bottom status bar: left "esc to interrupt" hint (cancelling a run is
		// otherwise undiscoverable) + right model tier and execution timer.
		elapsed := formatElapsed(time.Since(m.processingStartTime))
		leftHint := styleDim().Render(" esc to interrupt")
		rightInfo := styleDim().Render(m.modelDisplayLabel() + " " + elapsed)
		sb.WriteString(composeBar(m.width, leftHint, rightInfo) + "\n")
	case stateApproval:
		sb.WriteString(bar)
		sb.WriteString("\n")
		// Labeled keys instead of a bare "[y/n/a]" so non-technical users know
		// what each choice does.
		keyStyle := lipgloss.NewStyle().Foreground(colorWarn).Bold(true)
		labelStyle := styleDim()
		sb.WriteString("  " +
			keyStyle.Render("[y]") + labelStyle.Render(" approve   ") +
			keyStyle.Render("[n]") + labelStyle.Render(" deny   ") +
			keyStyle.Render("[a]") + labelStyle.Render(" always allow"))
		sb.WriteString("\n")
		sb.WriteString(bar)
	case stateSessionPicker:
		sb.WriteString(lipgloss.NewStyle().Foreground(colorInfo).Render("  Sessions (Up/Down, Enter, Esc)"))
	case statePicker:
		sb.WriteString(lipgloss.NewStyle().Foreground(colorInfo).Render("  " + m.pickerTitle + " (Up/Down, Enter, Esc)"))
	}

	// --- Dropdown (only when visible) ---
	if m.state == stateInput && m.menuVisible {
		sb.WriteString("\n")
		sb.WriteString(m.renderMenu())
	} else if m.state == stateSessionPicker {
		sb.WriteString("\n")
		sb.WriteString(renderDropList(dropListSize, len(m.lastSessions), m.sessionPickerIdx, func(i int) (string, string) {
			s := m.lastSessions[i]
			title := s.Title
			if r := []rune(title); len(r) > 40 {
				title = string(r[:37]) + "..."
			}
			desc := fmt.Sprintf("[%s] %d msgs", s.UpdatedAt.Format("Jan 02 15:04"), s.MsgCount)
			return title, desc
		}))
	} else if m.state == statePicker {
		sb.WriteString("\n")
		sb.WriteString(renderDropList(dropListSize, len(m.pickerOpts), m.pickerIdx, func(i int) (string, string) {
			o := m.pickerOpts[i]
			return o.label, o.desc
		}))
	}

	return sb.String()
}

func (m *Model) handleSubmit() (tea.Model, tea.Cmd) {
	input := strings.TrimSpace(m.textarea.Value())
	m.textarea.Reset()
	m.textarea.SetHeight(1)

	if input == "" {
		return m, nil
	}

	// Record in history (skip duplicates of last entry)
	if len(m.inputHistory) == 0 || m.inputHistory[len(m.inputHistory)-1] != input {
		m.inputHistory = append(m.inputHistory, input)
		if len(m.inputHistory) > 200 {
			m.inputHistory = m.inputHistory[len(m.inputHistory)-200:]
		}
	}
	m.historyIdx = -1
	m.historySaved = ""

	promptMark := lipgloss.NewStyle().Bold(true).Foreground(colorSecondary).Render(">")
	m.appendOutput(fmt.Sprintf("%s %s", promptMark, input))

	// Check slash commands
	if strings.HasPrefix(input, "/") {
		return m.handleSlashCommand(input)
	}

	// If already processing, inject into running loop instead of blocking
	if m.state == stateProcessing && m.injectCh != nil {
		select {
		case m.injectCh <- agent.InjectedMessage{Text: input}:
			// Append to session messages for context persistence
			sess := m.sessions.Current()
			sess.Messages = append(sess.Messages, client.Message{
				Role: "user", Content: client.NewTextContent(input),
			})
			sess.MessageMeta = append(sess.MessageMeta, session.MessageMeta{Source: "local", Timestamp: session.TimePtr(time.Now())})
		default:
			m.appendOutput("(injection queue full — message dropped)")
		}
		return m, nil
	}

	// Local agent loop
	m.state = stateProcessing
	m.lastToolResults = nil
	// Reset any live preview before the new run streams into it: a previous
	// run's late OnStreamDelta (drained after its Esc-cancel) can re-seed
	// streamLive, and clearing only on Esc would let that stale tail show as
	// this run's preview until the first commit boundary.
	m.streamLive = ""
	m.processingStartTime = time.Now()
	sess := m.sessions.Current()
	// Set title from first user message
	if sess.Title == "New session" {
		sess.Title = session.Title(input)
		sess.TitleAuto = true
	}
	userMsgTime := time.Now()
	sess.Messages = append(sess.Messages, client.Message{Role: "user", Content: client.NewTextContent(input)})
	sess.MessageMeta = append(sess.MessageMeta, session.MessageMeta{Source: "local", Timestamp: session.TimePtr(userMsgTime)})

	m.spinnerIdx = 0
	m.glyphIdx = 0
	m.colorIdx = 0
	// Pass everything except the just-appended user message as history,
	// stripping any prior loop-injected guardrail nudges so they can't
	// leak into this run's conversation snapshot.
	priorMsgs := sess.Messages[:len(sess.Messages)-1]
	priorMeta := sess.MessageMeta
	if len(priorMeta) > len(priorMsgs) {
		priorMeta = priorMeta[:len(priorMsgs)]
	}
	history := session.FilterInjected(priorMsgs, priorMeta)
	return m, tea.Batch(m.runAgentLoop(input, history), spinnerTick(), spinnerFrameTick())
}

func (m *Model) runAgentLoop(query string, history []client.Message) tea.Cmd {
	ctx, cancel := context.WithCancel(context.Background())
	m.cancelRun = cancel
	m.injectCh = make(chan agent.InjectedMessage, 10)
	return func() tea.Msg {
		// Handler is hoisted so post-run code can query its accumulated usage.
		handler := &tuiEventHandler{model: m}
		if sess := m.sessions.Current(); sess != nil {
			effectiveCWD := m.applyRuntimeContext(sess)
			m.agentLoop.SetHandler(handler)
			m.agentLoop.SetInjectCh(m.injectCh)
			// Wire handler to cloud_delegate tool so it can stream events
			if ct, ok := m.toolRegistry.Get("cloud_delegate"); ok {
				if cdt, ok := ct.(*tools.CloudDelegateTool); ok {
					cdt.SetHandler(handler)
				}
			}
			m.agentLoop.SetSessionID(sess.ID)
			m.agentLoop.SetToolResultBudgetState(sess.ToolResultReplacements, sess.ToolResultSeen)
			m.agentLoop.SetWorkingSet(m.sessions.WorkingSet(sess.ID))
			m.agentLoop.SetSessionCWD(effectiveCWD)

			cleanupSpills := m.agentLoop.SpillCleanupFunc()
			m.sessions.OnSessionClose(sess.ID, func() {
				cleanupSpills()
				m.agentLoop.SetWorkingSet(nil)
			})
		} else {
			m.agentLoop.SetHandler(handler)
			m.agentLoop.SetInjectCh(m.injectCh)
			if ct, ok := m.toolRegistry.Get("cloud_delegate"); ok {
				if cdt, ok := ct.(*tools.CloudDelegateTool); ok {
					cdt.SetHandler(handler)
				}
			}
			m.agentLoop.SetSessionID("")
			m.agentLoop.SetToolResultBudgetState(nil, nil)
			m.agentLoop.SetWorkingSet(nil)
		}
		result, usage, err := m.agentLoop.Run(ctx, query, nil, history)

		// Persist the run's messages to session. Use RunMessages() for
		// rich history (tool_use/tool_result blocks) so resumed sessions
		// give the LLM full context — including cancelled runs.
		// Only mutate sess.Messages when we intend to save, so hard errors
		// don't leave in-memory partial state without disk persistence.
		sess := m.sessions.Current()
		isCancelled := errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
		shouldPersist := isCancelled || err == nil || errors.Is(err, agent.ErrMaxIterReached)
		if shouldPersist {
			runMsgs := m.agentLoop.RunMessages()
			runInjected := m.agentLoop.RunMessageInjected()
			runTimestamps := m.agentLoop.RunMessageTimestamps()
			if len(runMsgs) > 0 {
				// RunMessages includes the user prompt as first entry;
				// skip it since handleSubmit already appended it.
				startIdx := 0
				if runMsgs[0].Role == "user" {
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
					meta := session.MessageMeta{Source: "local", Timestamp: session.TimePtr(ts)}
					if idx < len(runInjected) && runInjected[idx] {
						meta.SystemInjected = true
					}
					sess.MessageMeta = append(sess.MessageMeta, meta)
				}
			} else if result != "" {
				// Fallback: flat text (no RunMessages, e.g. early error).
				sess.Messages = append(sess.Messages, client.Message{Role: "assistant", Content: client.NewTextContent(result)})
				sess.MessageMeta = append(sess.MessageMeta, session.MessageMeta{Source: "local", Timestamp: session.TimePtr(time.Now())})
			}
			// Persist handler-accumulated usage (direct LLM + cloud_delegate +
			// gateway tool billing) into the session's cumulative Usage
			// summary. LLM and tool costs are stored in separate fields.
			if sess != nil {
				sess.ToolResultReplacements = m.agentLoop.ToolResultReplacements()
				sess.ToolResultSeen = m.agentLoop.ToolResultSeen()
				acc := handler.Usage()
				llm := acc.LLM
				if llm.LLMCalls > 0 || acc.ToolCalls > 0 || llm.InputTokens > 0 {
					m.sessions.AddUsage(sess.ID, session.UsageFromAccumulated(
						llm.LLMCalls, llm.InputTokens, llm.OutputTokens, llm.TotalTokens,
						llm.CostUSD, llm.CacheReadTokens, llm.CacheCreationTokens, llm.CacheCreation5mTokens, llm.CacheCreation1hTokens, llm.Model,
						acc.ToolCalls, acc.ToolCostUSD,
					))
				}
			}
			m.sessions.Save()
		}
		return agentDoneMsg{result: result, usage: usage, err: err, status: m.agentLoop.LastRunStatus()}
	}
}

func (m *Model) loadSessionHistory(sess *session.Session) {
	m.output = nil
	m.pendingPrints = nil

	messages := append([]client.Message(nil), sess.Messages...)
	width := m.width
	m.appendOutput(fmt.Sprintf("  Session: %s", sess.Title))
	m.appendOutput("")

	if m.program == nil {
		pm := lipgloss.NewStyle().Bold(true).Foreground(colorSecondary).Render(">")
		for _, msg := range messages {
			switch msg.Role {
			case "user":
				m.appendOutput(fmt.Sprintf("%s %s", pm, msg.Content.Text()))
			case "assistant":
				raw := msg.Content.Text()
				m.appendMarkdownOutput(raw, m.renderMarkdownCached(raw, width))
				m.appendOutput("")
			}
		}
		return
	}

	go func() {
		pm := lipgloss.NewStyle().Bold(true).Foreground(colorSecondary).Render(">")
		for _, msg := range messages {
			switch msg.Role {
			case "user":
				m.sendOutput(fmt.Sprintf("%s %s", pm, msg.Content.Text()))
			case "assistant":
				raw := msg.Content.Text()
				m.sendMarkdownOutput(raw, m.renderMarkdownCached(raw, width))
				m.sendOutput("")
			}
		}
		// Trigger a re-render after load completes so content uses the
		// current terminal width (fixes stale-width if resize happened
		// during history loading — Bug #4).
		if m.program != nil {
			m.program.Send(historyLoadedMsg{})
		}
	}()
}

func (m *Model) appendOutput(text string) {
	m.output = append(m.output, outputBlock{rendered: text})
	m.pendingPrints = append(m.pendingPrints, text)
}

func (m *Model) appendMarkdownOutput(raw, rendered string) {
	m.output = append(m.output, outputBlock{raw: raw, rendered: rendered})
	m.pendingPrints = append(m.pendingPrints, rendered)
}

func (m *Model) adjustTextareaHeight() {
	lines := strings.Count(m.textarea.Value(), "\n") + 1
	height := lines
	if height > 6 {
		height = 6
	}
	if height < 1 {
		height = 1
	}
	m.textarea.SetHeight(height)
}

// flushPrints returns a Cmd that prints all pending output above the view.
func (m *Model) flushPrints() tea.Cmd {
	if len(m.pendingPrints) == 0 {
		return nil
	}
	texts := make([]string, len(m.pendingPrints))
	copy(texts, m.pendingPrints)
	m.pendingPrints = m.pendingPrints[:0]
	return tea.Println(strings.Join(texts, "\n"))
}

// rerenderOutput is the write-once scrollback gate. Terminal scrollback is
// immutable once emitted: tea.ClearScreen erases only the VISIBLE screen
// (\x1b[2J), not the saved-lines, so re-printing m.output stacks duplicates
// every turn, and the ClearScreen→Println→rerenderDoneMsg round-trip leaves the
// composer needing a second keypress to repaint ("double Enter"). Two behaviors:
//
//   - The caller wiped the conversation (m.output niled: /clear, /reset,
//     Ctrl+L) → wipe the visible screen; there is nothing left to reprint.
//   - Otherwise → emit ONLY the newly-appended blocks (flushPrints) and never
//     reprint the backlog, which already lives in terminal scrollback.
//
// This generalizes the switchToAgent fix (picker.go) to every caller, killing
// the header duplication and the double-Enter together. Tradeoff (accepted, and
// the choice Codex/Claude Code also make): resize does not re-flow already-
// committed scrollback — old lines keep their original wrap width.
func (m *Model) rerenderOutput() tea.Cmd {
	if len(m.output) == 0 {
		m.pendingPrints = m.pendingPrints[:0]
		return tea.ClearScreen
	}
	return m.flushPrints()
}

// generateTitleCmd generates a smart session title in the background and
// reports it back to the update loop, which persists it on the main goroutine.
// Returns nil (a no-op Cmd — Bubbletea ignores it) when gating fails. Only the
// gateway is captured into a local so the returned closure does NOT close over
// the Model (Bubbletea value-copies the Model through Update) — and critically,
// the closure never touches m.sessions, so it cannot write the active session
// the unlocked update loop also mutates. Persistence (the only session write)
// happens in the titleGeneratedMsg handler on the main goroutine.
func (m *Model) generateTitleCmd(sessionID, source string, msgs []client.Message, turns int) tea.Cmd {
	if m.gateway == nil || m.sessions == nil || sessionID == "" || !ctxwin.TitleTriggerTurns[turns] {
		return nil
	}
	gw := m.gateway
	msgsCopy := append([]client.Message(nil), msgs...)
	return func() tea.Msg {
		// TUI is an interactive (non-IM) entry point with no per-sender/channel
		// distinction; pass "" for both. Bound the call so a hung gateway can't
		// keep this throwaway-title goroutine alive for its full 600s HTTP timeout.
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		smart, err := ctxwin.GenerateTitle(ctx, gw, msgsCopy)
		if err != nil {
			return nil
		}
		final := ctxwin.DecorateTitle(source, "", "", smart)
		if final == "" {
			return nil
		}
		return titleGeneratedMsg{sessionID: sessionID, title: final, atTurns: turns}
	}
}

func markdownCacheKey(text string, width int) string {
	sum := sha256.Sum256([]byte(text))
	return fmt.Sprintf("%d:%x", width, sum[:])
}

func (m *Model) renderMarkdownCached(text string, width int) string {
	key := markdownCacheKey(text, width)
	m.markdownCacheMu.RLock()
	cached, ok := m.markdownCache[key]
	m.markdownCacheMu.RUnlock()
	if ok {
		return cached
	}
	rendered := renderMarkdown(text, width)
	m.markdownCacheMu.Lock()
	m.markdownCache[key] = rendered
	m.markdownCacheMu.Unlock()
	return rendered
}

// Braille dot spinner frames (MiniDot style)
var dotFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// Color gradient: purple → blue → cyan → white (ANSI 256 codes)
var spinColors = []string{"99", "105", "111", "117", "123", "159", "195", "231"}

func spinnerFrameTick() tea.Cmd {
	return tea.Tick(100*time.Millisecond, func(t time.Time) tea.Msg {
		return spinnerFrameMsg{}
	})
}

func spinnerTick() tea.Cmd {
	return tea.Tick(5*time.Second, func(t time.Time) tea.Msg {
		return spinnerTickMsg{}
	})
}

// shimmer endpoints: resting green → bright lime peak, interpolated in RGB so
// the highlight glows on and off smoothly instead of snapping between two ANSI
// indices. lipgloss downsamples to 256/16-color on terminals without truecolor.
var (
	shimmerBase = [3]int{0x3A, 0x9A, 0x3A}
	shimmerPeak = [3]int{0xC6, 0xF0, 0x8C}
)

// renderWaveText renders text with a soft highlight that sweeps across it. Each
// character's brightness follows a gaussian falloff from the moving center, so
// the glow ramps up and down (a raised-cosine "breathing" wave) rather than the
// old hard 1-character on/off step.
func renderWaveText(text string, tick int) string {
	runes := []rune(text)
	n := len(runes)
	if n == 0 {
		return ""
	}
	// A tail gap (period > n) lets the highlight fully exit before restarting.
	period := n + 6
	center := float64(tick % period)
	const sigma = 2.2 // highlight half-width, in characters

	var sb strings.Builder
	for i, r := range runes {
		d := center - float64(i)
		t := math.Exp(-(d * d) / (2 * sigma * sigma)) // falloff in [0,1]
		cr := shimmerBase[0] + int(float64(shimmerPeak[0]-shimmerBase[0])*t)
		cg := shimmerBase[1] + int(float64(shimmerPeak[1]-shimmerBase[1])*t)
		cb := shimmerBase[2] + int(float64(shimmerPeak[2]-shimmerBase[2])*t)
		hex := fmt.Sprintf("#%02X%02X%02X", cr, cg, cb)
		sb.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color(hex)).Render(string(r)))
	}
	return sb.String()
}

// formatElapsed formats a duration as a compact timer string.
func formatElapsed(d time.Duration) string {
	s := int(d.Seconds())
	if s < 60 {
		return fmt.Sprintf("%ds", s)
	}
	m := s / 60
	s = s % 60
	if m < 60 {
		return fmt.Sprintf("%dm %ds", m, s)
	}
	h := m / 60
	m = m % 60
	return fmt.Sprintf("%dh %dm", h, m)
}

// sendOutput sends output from a goroutine through the bubbletea event loop
// so the TUI actually re-renders. Use this instead of appendOutput from goroutines.
func (m *Model) sendOutput(text string) {
	if m.program != nil {
		m.program.Send(streamOutputMsg{text: text})
		return
	}
	m.appendOutput(text)
}

// sendMarkdownOutput sends pre-rendered markdown with raw source for resize re-rendering.
func (m *Model) sendMarkdownOutput(raw, rendered string) {
	if m.program != nil {
		m.program.Send(streamOutputMsg{text: rendered, raw: raw})
		return
	}
	m.appendMarkdownOutput(raw, rendered)
}

// sendStatus sends an ephemeral status pill from a goroutine. It replaces the
// previous status (like the desktop frontend's status pills).

func (m *Model) handleSlashCommand(input string) (tea.Model, tea.Cmd) {
	parts := strings.Fields(input)
	cmd := parts[0]

	switch cmd {
	case "/quit", "/exit":
		m.hookRunner.RunStop(context.Background(), "")
		m.sessions.Save()
		m.sessions.Close()
		if m.toolCleanup != nil {
			m.toolCleanup()
		}
		if m.remoteCleanup != nil {
			m.remoteCleanup()
		}
		return m, tea.Quit
	case "/help":
		m.appendOutput(helpText())
	case "/clear":
		m.output = nil
		sess := m.sessions.NewSession()
		m.resumedSession = false
		m.sessionAllowed = make(map[string]bool)
		m.applyRuntimeContext(sess)
		return m, m.rerenderOutput()
	case "/reset":
		sess := m.sessions.Current()
		if sess == nil {
			m.appendOutput("No active session to reset")
			break
		}
		if err := m.sessions.Reset(sess.ID); err != nil {
			m.appendOutput(fmt.Sprintf("Reset failed: %v", err))
			break
		}
		m.output = nil
		m.sessionAllowed = make(map[string]bool)
		m.applyRuntimeContext(m.sessions.Current())
		return m, m.rerenderOutput()
	case "/sessions":
		m.openSessionPicker()
	case "/session":
		if len(parts) < 2 {
			m.openSessionPicker() // bare /session → selectable list
			break
		}
		{
			switch parts[1] {
			case "new":
				sess := m.sessions.NewSession()
				m.resumedSession = false
				m.sessionAllowed = make(map[string]bool)
				m.applyRuntimeContext(sess)
				m.appendOutput("Started new session")
			case "resume":
				if len(parts) < 3 {
					m.openSessionPicker() // /session resume → selectable list
				} else {
					target := parts[2]
					// Try as 1-based index from /sessions list
					if n, err := strconv.Atoi(target); err == nil && n >= 1 && n <= len(m.lastSessions) {
						target = m.lastSessions[n-1].ID
					}
					sess, err := m.sessions.Resume(target)
					if err != nil {
						m.appendOutput(fmt.Sprintf("Error: %v", err))
					} else {
						m.resumedSession = true
						m.sessionAllowed = make(map[string]bool)
						m.applyRuntimeContext(sess)
						m.loadSessionHistory(sess)
					}
				}
			}
		}
	case "/model":
		if m.cfg.Provider == "ollama" {
			if len(parts) > 1 {
				newModel := parts[1]
				m.cfg.Ollama.Model = newModel
				if m.baseCfg != nil {
					m.baseCfg.Ollama.Model = newModel
				}
				m.agentLoop.SetSpecificModel(newModel)
				saveCfg := m.cfg
				if m.baseCfg != nil {
					saveCfg = m.baseCfg
				}
				if err := config.Save(saveCfg); err != nil {
					m.appendOutput(fmt.Sprintf("Model: %s (failed to save: %v)", newModel, err))
				} else {
					m.appendOutput(fmt.Sprintf("Model: %s (saved)", newModel))
				}
			} else {
				oc, ok := m.llmClient.(*client.OllamaClient)
				if !ok {
					m.appendOutput(fmt.Sprintf("Current model: %s", m.cfg.Ollama.Model))
					break
				}
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				models, err := oc.ListModels(ctx)
				cancel()
				if err != nil {
					m.appendOutput(fmt.Sprintf("Current model: %s (could not list: %v)", m.cfg.Ollama.Model, err))
					break
				}
				var sb strings.Builder
				sb.WriteString("Available models:\n")
				for _, mdl := range models {
					marker := "  "
					if mdl.Name == m.cfg.Ollama.Model {
						marker = "→ "
					}
					sizeGB := float64(mdl.Size) / 1e9
					sb.WriteString(fmt.Sprintf("  %s%s (%.1f GB)\n", marker, mdl.Name, sizeGB))
				}
				sb.WriteString("\nUse /model <name> to switch")
				m.appendOutput(sb.String())
			}
		} else {
			if len(parts) > 1 {
				m.applyModelTier(parts[1]) // /model <tier> — direct (power users)
			} else {
				m.openModelPicker() // bare /model — interactive picker
			}
		}
	case "/agent", "/agents":
		if len(parts) > 1 && m.agentExists(parts[1]) {
			return m, m.switchToAgent(parts[1]) // /agent <valid-name> — direct
		}
		// bare /agent, or a typed name that doesn't exist → selectable picker
		// (so "I'll just type it" / a typo still lands on the list, not an error)
		m.openAgentPicker()
	case "/config":
		m.appendOutput(formatConfigDisplay(m.cfg))
	case "/setup":
		m.appendOutput("Setup cannot run inside the TUI. Exit and run: shan --setup")
	case "/update":
		m.appendOutput("Checking for updates...")
		newVersion, err := update.DoUpdate(m.version)
		if err != nil {
			m.appendOutput(fmt.Sprintf("  %v", err))
		} else {
			m.appendOutput(fmt.Sprintf("  Updated to %s. Restart to use new version.", newVersion))
		}
	case "/copy":
		sess := m.sessions.Current()
		if sess != nil && len(sess.Messages) > 0 {
			// Find the last assistant message
			for i := len(sess.Messages) - 1; i >= 0; i-- {
				if sess.Messages[i].Role == "assistant" {
					return m, copyToClipboard(sess.Messages[i].Content.Text())
				}
			}
			m.appendOutput("No assistant message to copy")
		} else {
			m.appendOutput("No messages in session")
		}
	case "/rename":
		newTitle := strings.TrimSpace(strings.TrimPrefix(input, "/rename "))
		if newTitle == "" {
			m.appendOutput("Usage: /rename <new title>")
		} else {
			sess := m.sessions.Current()
			if sess == nil {
				m.appendOutput("No active session to rename")
			} else {
				sess.Title = newTitle
				m.sessions.Save()
				m.appendOutput(fmt.Sprintf("Session renamed: %s", newTitle))
			}
		}
	case "/research":
		return m.handleResearch(parts[1:])
	case "/swarm":
		return m.handleSwarm(parts[1:])
	case "/search":
		if len(parts) < 2 {
			m.appendOutput("Usage: /search <query>")
		} else {
			query := strings.Join(parts[1:], " ")
			results, err := m.sessions.Search(query, 20)
			if err != nil {
				m.appendOutput(fmt.Sprintf("Search error: %v", err))
			} else if len(results) == 0 {
				m.appendOutput("No matching sessions found.")
			} else {
				m.appendOutput(fmt.Sprintf("Found %d matches:", len(results)))
				for i, r := range results {
					m.appendOutput(fmt.Sprintf("  %d. [%s] %s (%s): %s",
						i+1, r.CreatedAt.Format("Jan 02"), r.SessionTitle, r.Role, r.Snippet))
				}
			}
		}
	case "/status":
		sess := m.sessions.Current()
		agentName := "default"
		if m.agentOverride != nil {
			agentName = m.agentOverride.Name
		}
		sessID := "(none)"
		msgCount := 0
		tokenEst := 0
		if sess != nil {
			sessID = sess.ID
			msgCount = len(sess.Messages)
			tokenEst = ctxwin.EstimateTokens(sess.Messages)
		}
		ctxWindow := m.cfg.Agent.ContextWindow
		if ctxWindow <= 0 {
			ctxWindow = 200000
		}
		pct := float64(tokenEst) / float64(ctxWindow) * 100
		toolCount := 0
		if m.toolRegistry != nil {
			toolCount = m.toolRegistry.Len()
		}
		dimStyle := lipgloss.NewStyle().Foreground(colorDim)
		m.appendOutput(dimStyle.Render(fmt.Sprintf(
			"  Version:     %s\n"+
				"  Model:       %s\n"+
				"  Endpoint:    %s\n"+
				"  Agent:       %s\n"+
				"  Session:     %s (%d messages)\n"+
				"  Context:     ~%s / %s tokens (%.1f%%)\n"+
				"  Tools:       %d registered",
			m.version, m.cfg.ModelTier, m.cfg.Endpoint, agentName,
			sessID, msgCount,
			formatTokenCount(tokenEst), formatTokenCount(ctxWindow), pct,
			toolCount,
		)))
	case "/doctor":
		m.appendOutput("Running diagnostics...")
		m.state = stateProcessing
		m.processingStartTime = time.Now()
		return m, tea.Batch(m.runDoctor(), spinnerTick(), spinnerFrameTick())
	case "/permissions":
		if len(parts) == 1 {
			m.appendOutput(formatPermissions(&m.cfg.Permissions))
		} else {
			sub := parts[1]
			if len(parts) < 3 {
				m.appendOutput("Usage: /permissions allow|deny|remove <pattern>")
				break
			}
			pattern := strings.Join(parts[2:], " ")
			switch sub {
			case "allow":
				m.cfg.Permissions.AllowedCommands = append(m.cfg.Permissions.AllowedCommands, pattern)
				if m.baseCfg != nil {
					m.baseCfg.Permissions.AllowedCommands = append(m.baseCfg.Permissions.AllowedCommands, pattern)
				}
				if err := config.Save(m.baseCfg); err != nil {
					m.appendOutput(fmt.Sprintf("Allowed %q (save failed: %v)", pattern, err))
				} else {
					m.appendOutput(fmt.Sprintf("Allowed: %s (saved)", pattern))
				}
			case "deny":
				m.cfg.Permissions.DeniedCommands = append(m.cfg.Permissions.DeniedCommands, pattern)
				if m.baseCfg != nil {
					m.baseCfg.Permissions.DeniedCommands = append(m.baseCfg.Permissions.DeniedCommands, pattern)
				}
				if err := config.Save(m.baseCfg); err != nil {
					m.appendOutput(fmt.Sprintf("Denied %q (save failed: %v)", pattern, err))
				} else {
					m.appendOutput(fmt.Sprintf("Denied: %s (saved)", pattern))
				}
			case "remove":
				removed := false
				m.cfg.Permissions.AllowedCommands = removePattern(m.cfg.Permissions.AllowedCommands, pattern)
				m.cfg.Permissions.DeniedCommands = removePattern(m.cfg.Permissions.DeniedCommands, pattern)
				if m.baseCfg != nil {
					before := len(m.baseCfg.Permissions.AllowedCommands) + len(m.baseCfg.Permissions.DeniedCommands)
					m.baseCfg.Permissions.AllowedCommands = removePattern(m.baseCfg.Permissions.AllowedCommands, pattern)
					m.baseCfg.Permissions.DeniedCommands = removePattern(m.baseCfg.Permissions.DeniedCommands, pattern)
					after := len(m.baseCfg.Permissions.AllowedCommands) + len(m.baseCfg.Permissions.DeniedCommands)
					removed = before != after
				}
				if removed {
					config.Save(m.baseCfg)
					m.appendOutput(fmt.Sprintf("Removed: %s", pattern))
				} else {
					m.appendOutput(fmt.Sprintf("Pattern not found: %s", pattern))
				}
			default:
				m.appendOutput("Usage: /permissions allow|deny|remove <pattern>")
			}
		}
	case "/compact":
		sess := m.sessions.Current()
		if sess == nil || len(sess.Messages) < ctxwin.MinShapeable() {
			m.appendOutput(fmt.Sprintf("Conversation too short to compact (need %d+ messages)", ctxwin.MinShapeable()))
			break
		}
		customInstructions := ""
		if len(parts) > 1 {
			customInstructions = strings.Join(parts[1:], " ")
		}
		m.appendOutput("Compacting context...")
		m.state = stateProcessing
		m.processingStartTime = time.Now()
		compactFn := m.runCompact(customInstructions)
		return m, tea.Batch(func() tea.Msg { return compactFn() }, spinnerTick(), spinnerFrameTick())
	default:
		// Check custom commands
		cmdName := strings.TrimPrefix(cmd, "/")
		if promptContent, ok := m.customCommands[cmdName]; ok {
			// Replace $ARGUMENTS with the rest of the input
			args := ""
			if len(parts) > 1 {
				args = strings.Join(parts[1:], " ")
			}
			expandedPrompt := strings.ReplaceAll(promptContent, "$ARGUMENTS", args)
			// Send as a regular user message through the agent loop
			m.state = stateProcessing
			m.processingStartTime = time.Now()
			m.spinnerIdx = 0
			m.glyphIdx = 0
			m.colorIdx = 0
			sess := m.sessions.Current()
			var history []client.Message
			if sess != nil {
				history = sess.HistoryForLoop()
			}
			return m, tea.Batch(m.runAgentLoop(expandedPrompt, history), spinnerTick(), spinnerFrameTick())
		}
		m.appendOutput(fmt.Sprintf("Unknown command: %s (type /help)", cmd))
	}

	return m, nil
}

func (m *Model) runDoctor() tea.Cmd {
	return func() tea.Msg {
		toolCount := 0
		if m.toolRegistry != nil {
			toolCount = m.toolRegistry.Len()
		}
		checks := runDoctorWithHealth(m.shannonDir, m.cfg.APIKey, m.cfg.Endpoint, m.gateway, &m.cfg.Permissions, m.cfg.MCPServers, toolCount)
		return doctorDoneMsg{checks: checks}
	}
}

func (m *Model) handleResearch(args []string) (tea.Model, tea.Cmd) {
	strategy := "standard"
	query := strings.Join(args, " ")

	if len(args) > 0 {
		switch args[0] {
		case "quick", "standard", "deep", "academic":
			strategy = args[0]
			query = strings.Join(args[1:], " ")
		}
	}

	if query == "" {
		m.appendOutput("Usage: /research [quick|standard|deep] <query>")
		return m, nil
	}

	m.state = stateProcessing
	m.processingStartTime = time.Now()
	m.spinnerIdx = 0
	m.glyphIdx = 0
	m.colorIdx = 0
	m.appendOutput(fmt.Sprintf("Starting %s research...", strategy))

	return m, tea.Batch(m.runRemote(query, map[string]any{"force_research": true}, strategy), spinnerTick(), spinnerFrameTick())
}

func (m *Model) handleSwarm(args []string) (tea.Model, tea.Cmd) {
	query := strings.Join(args, " ")
	if query == "" {
		m.appendOutput("Usage: /swarm <query>")
		return m, nil
	}

	m.state = stateProcessing
	m.processingStartTime = time.Now()
	m.spinnerIdx = 0
	m.glyphIdx = 0
	m.colorIdx = 0
	m.appendOutput("Starting swarm workflow...")

	return m, tea.Batch(m.runRemote(query, map[string]any{"force_swarm": true}, ""), spinnerTick(), spinnerFrameTick())
}

func (m *Model) runRemote(query string, ctx map[string]any, strategy string) tea.Cmd {
	if m.gateway == nil {
		return func() tea.Msg {
			return agentDoneMsg{err: fmt.Errorf("remote tasks require gateway provider (not available with ollama)")}
		}
	}
	// Set title from query if still default
	sess := m.sessions.Current()
	if sess.Title == "New session" {
		sess.Title = session.Title(query)
		sess.TitleAuto = true
	}
	return func() tea.Msg {
		taskReq := client.TaskRequest{
			Query:            query,
			SessionID:        m.sessions.Current().ID,
			Context:          ctx,
			ResearchStrategy: strategy,
		}

		resp, err := m.gateway.SubmitTaskStream(context.Background(), taskReq)
		if err != nil {
			return agentDoneMsg{err: fmt.Errorf("submit task: %w", err)}
		}

		m.sessions.Current().RemoteTasks = append(m.sessions.Current().RemoteTasks, resp.WorkflowID)

		var finalResult string
		var workflowErr error

		// Use API-provided stream URL if available, otherwise construct from base
		streamURL := resp.StreamURL
		if streamURL == "" {
			streamURL = m.gateway.StreamURL(resp.WorkflowID)
		}
		streamURL = m.gateway.ResolveURL(streamURL)

		err = client.StreamSSE(context.Background(), streamURL, m.cfg.APIKey, func(ev client.SSEEvent) {
			// Common event structure — most events have a message field
			var event struct {
				Message  string                 `json:"message"`
				AgentID  string                 `json:"agent_id"`
				Delta    string                 `json:"delta"`
				Response string                 `json:"response"`
				Type     string                 `json:"type"`
				Payload  map[string]interface{} `json:"payload"`
			}
			json.Unmarshal([]byte(ev.Data), &event)

			switch ev.Event {
			// --- Streaming content ---
			case "thread.message.delta", "LLM_PARTIAL":
				// Deltas suppressed — final result rendered on completion.
			case "thread.message.completed", "LLM_OUTPUT":
				if event.AgentID == "title_generator" {
					// Capture generated title for session
					if event.Response != "" {
						title := strings.TrimSpace(event.Response)
						title = strings.Trim(title, "\"'`")
						if title != "" {
							m.sessions.Current().Title = session.Title(title)
						}
					}
					break
				}
				if event.Response != "" {
					finalResult = event.Response
				}

			// --- Status pill events (ephemeral, replace previous) ---
			case "WORKFLOW_STARTED":
				m.sendOutput("  > " + statusMessage(event.Message, "Starting workflow..."))
			case "PROGRESS", "STATUS_UPDATE":
				m.sendOutput("  > " + statusMessage(event.Message, "Processing..."))
			case "AGENT_STARTED":
				m.sendOutput("  > " + statusMessage(event.Message, "Agent working..."))
			case "AGENT_THINKING":
				msg := event.Message
				if len(msg) > 100 {
					msg = "" // skip verbose reasoning (matches desktop behavior)
				}
				m.sendOutput("  ~ " + statusMessage(msg, "Thinking..."))
			case "DELEGATION":
				m.sendOutput("  > " + statusMessage(event.Message, "Delegating task..."))
			case "DATA_PROCESSING":
				m.sendOutput("  > " + statusMessage(event.Message, "Processing data..."))
			case "TOOL_INVOKED", "TOOL_STARTED":
				m.sendOutput("  ? " + statusMessage(event.Message, "Calling tool..."))
			case "TOOL_OBSERVATION", "TOOL_COMPLETED":
				m.sendOutput("  * " + statusMessage(event.Message, "Tool completed"))
			case "WAITING":
				m.sendOutput("  . " + statusMessage(event.Message, "Waiting..."))
			case "LLM_PROMPT":
				// Not shown in conversation (matches desktop)

			// --- Terminal events (persist in output) ---
			case "AGENT_COMPLETED":
				m.sendOutput("  + " + statusMessage(event.Message, "Agent completed"))
			case "WORKFLOW_COMPLETED":

				if finalResult == "" {
					finalResult = event.Message
				}
			case "WORKFLOW_FAILED", "error", "ERROR_OCCURRED":
				m.sendOutput("  ! Error: " + statusMessage(event.Message, "Workflow failed"))
				workflowErr = fmt.Errorf("workflow failed: %s", event.Message)

			// --- Control flow events ---
			case "workflow.pausing":
				m.sendOutput("  || Pausing at next checkpoint...")
			case "workflow.paused":
				m.sendOutput("  || Workflow paused")
			case "workflow.resumed":
				m.sendOutput("  > Resumed")
			case "workflow.cancelling":
				m.sendOutput("  x Cancelling...")
			case "workflow.cancelled":
				m.sendOutput("  Task was cancelled.")
				workflowErr = fmt.Errorf("workflow cancelled")

			// --- Informational (show as status briefly) ---
			case "APPROVAL_REQUESTED":
				m.sendOutput("  ! " + statusMessage(event.Message, "Awaiting approval..."))
			case "ERROR_RECOVERY":
				m.sendOutput("  ~ " + statusMessage(event.Message, "Recovering from error..."))
			case "ROLE_ASSIGNED", "TEAM_RECRUITED", "TEAM_RETIRED", "TEAM_STATUS",
				"DEPENDENCY_SATISFIED", "MESSAGE_SENT", "MESSAGE_RECEIVED",
				"WORKSPACE_UPDATED", "APPROVAL_DECISION", "BUDGET_THRESHOLD":
				if event.Message != "" {
					m.sendOutput("  > " + event.Message)
				}

			// --- Research plan HITL ---
			case "RESEARCH_PLAN_READY", "RESEARCH_PLAN_UPDATED":
				m.sendOutput("  Research plan ready for review")
			case "RESEARCH_PLAN_APPROVED":
				m.sendOutput("  Research plan approved, executing...")

			// --- Swarm-specific events ---
			case "LEAD_DECISION":
				if msg := event.Message; msg != "" && len(msg) <= 150 {
					m.sendOutput("  ~ " + msg)
				}
			case "TASKLIST_UPDATED":
				if payload := event.Payload; payload != nil {
					if tasks, ok := payload["tasks"].([]interface{}); ok && len(tasks) > 0 {
						completed := 0
						for _, task := range tasks {
							if tm, ok := task.(map[string]interface{}); ok {
								if tm["status"] == "completed" {
									completed++
								}
							}
						}
						m.sendOutput(fmt.Sprintf("  > Tasks: %d/%d done", completed, len(tasks)))
					}
				}
			case "HITL_RESPONSE":
				if event.Message != "" {
					m.sendOutput("  ~ Lead responding to your input")
				}

			default:
				// Unknown events — show message if present, skip raw JSON
				if event.Message != "" {
					m.sendOutput("  > " + event.Message)
				}
			}
		})

		if err != nil {
			return agentDoneMsg{err: fmt.Errorf("stream: %w", err)}
		}
		if workflowErr != nil {
			return agentDoneMsg{err: workflowErr}
		}

		if finalResult != "" {
			// Response display is handled by agentDoneMsg to avoid races.
			sess := m.sessions.Current()
			workflowUserTime := time.Now()
			sess.Messages = append(sess.Messages,
				client.Message{Role: "user", Content: client.NewTextContent(query)},
				client.Message{Role: "assistant", Content: client.NewTextContent(finalResult)},
			)
			sess.MessageMeta = append(sess.MessageMeta,
				session.MessageMeta{Source: "local", Timestamp: session.TimePtr(workflowUserTime)},
				session.MessageMeta{Source: "local", Timestamp: session.TimePtr(time.Now())},
			)
		} else {
			return agentDoneMsg{err: fmt.Errorf("workflow completed but returned no response")}
		}

		return agentDoneMsg{result: finalResult}
	}
}

func (m *Model) showSessions() {
	sessions, err := m.sessions.List()
	if err != nil {
		m.appendOutput(fmt.Sprintf("Error: %v", err))
		return
	}
	if len(sessions) == 0 {
		m.appendOutput("No saved sessions")
		return
	}
	m.lastSessions = sessions
	for i, s := range sessions {
		m.appendOutput(fmt.Sprintf("  %d. [%s] %s (%d messages)",
			i+1, s.UpdatedAt.Format("Jan 02"), s.Title, s.MsgCount))
	}
	m.appendOutput("  Use /session resume <number> to resume")
}

func helpText() string {
	return `Keys:
  Alt+Enter                      Insert newline (multi-line input)
  Enter                          Submit message
  Up/Down                        Navigate input history
  Esc Esc                        Clear input
  Ctrl+K                         Delete to end of line
  Ctrl+U                         Delete to start of line
  Ctrl+W                         Delete word backward
  Ctrl+L                         Clear screen
  Ctrl+O                         Expand last tool results

Commands:
  /help                          Show this help
  /research [quick|standard|deep] <query>  Remote research
  /swarm <query>                 Multi-agent swarm
  /config                        Show configuration
  /setup                         Reconfigure endpoint & API key
  /sessions                      List saved sessions
  /search <query>                Search session history
  /session new                   Start new session
  /session resume <id>           Resume a saved session
  /model [small|medium|large]    Switch model tier
  /agent [name]                  Switch agent (picker if no name)
  /rename <title>                Rename current session
  /copy                          Copy last response to clipboard
  /clear                         New session + clear screen
  /reset                         Clear current session history in place
  /compact [instructions]        Compress context, keep summary
  /status                        Show session status
  /doctor                        Run diagnostic checks
  /permissions                   Show/manage tool permissions
  /quit                          Exit`
}

// tuiEventHandler bridges agent events to the TUI
type tuiEventHandler struct {
	model          *Model
	cloudStreaming bool // when true, OnStreamDelta forwards to TUI (for cloud_delegate)
	usage          agent.UsageAccumulator
}

// Usage returns the cumulative usage collected during this handler's lifetime,
// split into LLM and gateway-tool billing.
func (h *tuiEventHandler) Usage() agent.AccumulatedUsage { return h.usage.Snapshot() }

// ResetUsage clears accumulated totals. Called between TUI prompts to scope
// usage reporting to a single run.
func (h *tuiEventHandler) ResetUsage() { h.usage.Reset() }

func (h *tuiEventHandler) OnToolCall(name string, args string, toolUseID string) {
	// Skip spinner/indicator for think tool — its content is shown dimmed on result.
	if name == "think" {
		return
	}
	if h.model.program != nil {
		h.model.program.Send(toolCallMsg{name: name, args: truncate(args, 200)})
	}
}

func (h *tuiEventHandler) OnToolResult(name string, args string, toolUseID string, result agent.ToolResult, elapsed time.Duration) {
	if h.model.program != nil {
		h.model.program.Send(toolResultMsg{
			name:    name,
			args:    args,
			content: result.Content,
			isError: result.IsError,
			elapsed: elapsed,
		})
	}
}

func (h *tuiEventHandler) OnText(text string) {
	// Final-answer rendering happens in agentDoneMsg (app.go ~line 1037) which
	// uses the markdown renderer. Rendering here would double the output.
}

// OnPreamble renders mid-turn agent narration (preamble emitted alongside
// native tool_use blocks) inline through the Bubbletea event loop, so the
// user sees the agent's "what I'm about to do" text between tool calls.
// Triggered by the tool-call branch in AgentLoop.Run (loop.go ~line 2499).
func (h *tuiEventHandler) OnPreamble(text string) {
	if text == "" {
		return
	}
	h.model.sendOutput(text)
}

func (h *tuiEventHandler) OnStreamDelta(delta string) {
	if delta == "" {
		return
	}
	// cloud_delegate streams its nested run's text straight into scrollback.
	if h.cloudStreaming {
		h.model.sendOutput(delta)
		return
	}
	// Local LLM: feed the transient live-preview region so the answer is seen
	// growing in real time instead of appearing all at once when the turn ends.
	// The finalized answer is still rendered to scrollback by agentDoneMsg; the
	// preview is cleared at every commit boundary so it never duplicates.
	if h.model.program != nil {
		h.model.program.Send(streamDeltaMsg{delta: delta})
	}
}

// SetCloudStreaming enables/disables delta forwarding for cloud_delegate events.
func (h *tuiEventHandler) SetCloudStreaming(enabled bool) {
	h.cloudStreaming = enabled
}

func (h *tuiEventHandler) OnUsage(usage agent.TurnUsage) {
	h.usage.Add(usage)
}

func (h *tuiEventHandler) OnCloudAgent(agentID, status, message string) {
	prefixes := map[string]string{"started": ">", "completed": "+", "thinking": "~", "tool": "?"}
	p := prefixes[status]
	if p == "" {
		p = "-"
	}
	h.OnStreamDelta(fmt.Sprintf("  %s %s\n", p, cloudflow.CloudStatusLine(agentID, status, message)))
}

func (h *tuiEventHandler) OnCloudProgress(completed, total int) {
	h.OnStreamDelta(fmt.Sprintf("  > Tasks: %d/%d done\n", completed, total))
}

func (h *tuiEventHandler) OnCloudPlan(planType, content string, needsReview bool) {
	switch planType {
	case "research_plan":
		h.OnStreamDelta(fmt.Sprintf("\n--- Research Plan ---\n%s\n", content))
	case "research_plan_updated":
		h.OnStreamDelta(fmt.Sprintf("\n--- Updated Research Plan ---\n%s\n", content))
	case "approved":
		h.OnStreamDelta("\n[Research plan approved, executing...]\n")
	}
}

func (h *tuiEventHandler) OnApprovalNeeded(tool string, args string) bool {
	// Send approval prompt to the TUI event loop, then block until user responds.
	// This runs inside a tea.Cmd goroutine so blocking is safe — it won't freeze the UI.
	if h.model.program != nil {
		h.model.program.Send(approvalRequestMsg{tool: tool, args: truncate(args, 200)})
		return <-h.model.approvalCh
	}
	// No program reference — deny by default (should not happen in normal flow)
	return false
}

type clipboardResultMsg struct {
	err error
	len int
}

func copyToClipboard(text string) tea.Cmd {
	return func() tea.Msg {
		// macOS: pbcopy, Linux: xclip or xsel
		var cmd *exec.Cmd
		switch runtime.GOOS {
		case "darwin":
			cmd = exec.Command("pbcopy")
		case "linux":
			cmd = exec.Command("xclip", "-selection", "clipboard")
		default:
			return clipboardResultMsg{err: fmt.Errorf("clipboard not supported on %s", runtime.GOOS)}
		}
		cmd.Stdin = strings.NewReader(text)
		err := cmd.Run()
		return clipboardResultMsg{err: err, len: len(text)}
	}
}

var baseSlashCommands = []slashCmd{
	{"/help", "Show help"},
	{"/research", "Remote research"},
	{"/swarm", "Multi-agent swarm"},
	{"/copy", "Copy last response"},
	{"/model", "Switch model tier"},
	{"/agent", "Switch agent"},
	{"/config", "Show configuration"},
	{"/setup", "Reconfigure endpoint & API key"},
	{"/sessions", "List saved sessions"},
	{"/search", "Search session history"},
	{"/session", "new | resume <n>"},
	{"/rename", "Rename current session"},
	{"/clear", "New session + clear screen"},
	{"/reset", "Clear current session history in place"},
	{"/compact", "Compress context (keep summary)"},
	{"/status", "Show session status"},
	{"/doctor", "Run diagnostic checks"},
	{"/permissions", "Manage tool permissions"},
	{"/update", "Check for updates"},
	{"/quit", "Exit"},
	{"/exit", "Exit"},
}

// immediateCommands take no required argument, so a single Enter in the
// autocomplete menu executes them (opening the /agent, /model, or /session
// picker, or running a no-arg command) instead of autocompleting and waiting
// for a second Enter. Commands needing a typed argument (/research, /swarm,
// /search, /rename) are absent so they still autocomplete for the argument.
var immediateCommands = map[string]bool{
	"/help": true, "/agent": true, "/agents": true, "/model": true,
	"/config": true, "/setup": true, "/sessions": true, "/session": true,
	"/clear": true, "/reset": true, "/compact": true, "/status": true,
	"/doctor": true, "/permissions": true, "/update": true,
	"/quit": true, "/exit": true, "/copy": true,
}

func isImmediateCommand(cmd string) bool { return immediateCommands[cmd] }

func (m *Model) updateMenu() {
	input := m.textarea.Value()
	if !strings.HasPrefix(input, "/") || strings.Contains(input, " ") {
		m.menuVisible = false
		m.menuItems = nil
		m.menuMatchPos = nil
		m.menuIndex = 0
		return
	}

	// Two tiers: exact prefix matches first (the common case), then looser
	// subsequence matches so a typo'd or abbreviated "/rsch" still finds
	// "/research". Declaration order is preserved within each tier.
	lowIn := strings.ToLower(input)
	inRunes := len([]rune(input))
	var prefix, fuzzy []slashCmd
	var prefixPos, fuzzyPos [][]int
	for _, c := range m.slashCommands {
		if strings.HasPrefix(strings.ToLower(c.cmd), lowIn) {
			// The matched run is the first inRunes runes of c.cmd. This relies on
			// slash-command names being ASCII (lowercasing preserves rune count
			// and indices); the builtin + custom command set satisfies that.
			pos := make([]int, inRunes)
			for i := range pos {
				pos[i] = i
			}
			prefix = append(prefix, c)
			prefixPos = append(prefixPos, pos)
			continue
		}
		// Only loosen to subsequence matching once enough has been typed to
		// disambiguate; at 1 char after "/" it would flood with noise (e.g.
		// "/r" matching "/clear"). Typos worth recovering happen later anyway.
		if inRunes >= 3 {
			if pos, ok := fuzzySubsequence(input, c.cmd); ok {
				fuzzy = append(fuzzy, c)
				fuzzyPos = append(fuzzyPos, pos)
			}
		}
	}
	m.menuItems = append(prefix, fuzzy...)
	m.menuMatchPos = append(prefixPos, fuzzyPos...)
	m.menuVisible = len(m.menuItems) > 0
	if m.menuIndex >= len(m.menuItems) {
		m.menuIndex = 0
	}
}

// fuzzySubsequence reports whether pattern appears in target as an ordered,
// case-insensitive subsequence, returning the matched rune indices in target.
func fuzzySubsequence(pattern, target string) ([]int, bool) {
	if pattern == "" {
		return nil, true
	}
	p := []rune(strings.ToLower(pattern))
	t := []rune(strings.ToLower(target))
	pos := make([]int, 0, len(p))
	pi := 0
	for ti := 0; ti < len(t) && pi < len(p); ti++ {
		if t[ti] == p[pi] {
			pos = append(pos, ti)
			pi++
		}
	}
	if pi == len(p) {
		return pos, true
	}
	return nil, false
}

const dropListSize = 5

func (m *Model) renderMenu() string {
	return renderHighlightedList(dropListSize, len(m.menuItems), m.menuIndex, func(i int) (string, string, []int) {
		var pos []int
		if i < len(m.menuMatchPos) {
			pos = m.menuMatchPos[i]
		}
		return m.menuItems[i].cmd, m.menuItems[i].desc, pos
	})
}

// renderHighlightedList is renderDropList plus per-character match highlighting:
// the runes at pos (rune indices in label) are drawn bold/accented so fuzzy
// hits stand out. Windowing/padding matches renderDropList for layout stability.
func renderHighlightedList(maxVisible, total, selected int, item func(i int) (label, desc string, pos []int)) string {
	if total == 0 {
		return strings.Repeat("\n", maxVisible)
	}

	baseLabel := styleDim()
	selLabel := lipgloss.NewStyle().Foreground(colorSecondary)
	matchStyle := lipgloss.NewStyle().Foreground(colorSelect).Bold(true)
	descStyle := styleDim()
	selDescStyle := lipgloss.NewStyle().Foreground(colorSelectDesc)

	visible := total
	if visible > maxVisible {
		visible = maxVisible
	}
	start := 0
	if selected >= maxVisible {
		start = selected - maxVisible + 1
	}
	if start+visible > total {
		start = total - visible
	}
	if start < 0 {
		start = 0
	}

	var sb strings.Builder
	for i := start; i < start+visible; i++ {
		label, desc, pos := item(i)
		labelBase := baseLabel
		ds := descStyle
		marker := "    "
		if i == selected {
			labelBase = selLabel
			ds = selDescStyle
			marker = "  > "
		}
		styledLabel := highlightChars(label, pos, labelBase, matchStyle)
		padWidth := 16 - lipgloss.Width(label)
		if padWidth < 1 {
			padWidth = 1
		}
		sb.WriteString(marker + styledLabel + strings.Repeat(" ", padWidth) + ds.Render(desc) + "\n")
	}
	for i := visible; i < maxVisible; i++ {
		sb.WriteString("\n")
	}
	return sb.String()
}

// highlightChars renders label with the runes at the given indices drawn in hi
// and the rest in base.
func highlightChars(label string, pos []int, base, hi lipgloss.Style) string {
	if len(pos) == 0 {
		return base.Render(label)
	}
	want := make(map[int]bool, len(pos))
	for _, p := range pos {
		want[p] = true
	}
	var sb strings.Builder
	for i, r := range []rune(label) {
		if want[i] {
			sb.WriteString(hi.Render(string(r)))
		} else {
			sb.WriteString(base.Render(string(r)))
		}
	}
	return sb.String()
}

// renderDropList renders a scrollable drop-down list with a fixed visible window.
// Always pads to maxVisible lines so the layout doesn't jump.
func renderDropList(maxVisible, total, selected int, item func(i int) (label, desc string)) string {
	if total == 0 {
		// Pad empty lines to keep layout stable
		return strings.Repeat("\n", maxVisible)
	}

	dimStyle := lipgloss.NewStyle().Foreground(colorDim)
	highlightLabel := lipgloss.NewStyle().Foreground(colorSelect).Bold(true)
	highlightDesc := lipgloss.NewStyle().Foreground(colorSelectDesc)

	// Calculate sliding window
	visible := total
	if visible > maxVisible {
		visible = maxVisible
	}
	start := 0
	if selected >= maxVisible {
		start = selected - maxVisible + 1
	}
	if start+visible > total {
		start = total - visible
	}
	if start < 0 {
		start = 0
	}

	var sb strings.Builder
	for i := start; i < start+visible; i++ {
		label, desc := item(i)
		labelWidth := lipgloss.Width(label)
		padWidth := 16 - labelWidth
		if padWidth < 1 {
			padWidth = 1
		}
		padding := strings.Repeat(" ", padWidth)
		if i == selected {
			sb.WriteString(fmt.Sprintf("  > %s%s%s\n",
				highlightLabel.Render(label),
				padding,
				highlightDesc.Render(desc)))
		} else {
			sb.WriteString(fmt.Sprintf("    %s%s%s\n",
				dimStyle.Render(label),
				padding,
				dimStyle.Render(desc)))
		}
	}

	// Pad remaining lines to keep layout stable
	for i := visible; i < maxVisible; i++ {
		sb.WriteString("\n")
	}
	return sb.String()
}

func statusMessage(msg, fallback string) string {
	if msg == "" {
		return fallback
	}
	if r := []rune(msg); len(r) > 150 {
		return string(r[:147]) + "..."
	}
	return msg
}

func formatConfigDisplay(cfg *config.Config) string {
	var sb strings.Builder
	sb.WriteString("Kocoro CLI Configuration\n")

	if cfg.Provider == "ollama" {
		sb.WriteString(fmt.Sprintf("  provider: ollama\n"))
		sb.WriteString(fmt.Sprintf("  ollama.endpoint: %s\n", cfg.Ollama.Endpoint))
		sb.WriteString(fmt.Sprintf("  ollama.model: %s\n", cfg.Ollama.Model))
		sb.WriteString("\n")
	}

	srcLabel := func(key string) string {
		if cfg.Sources == nil {
			return ""
		}
		s, ok := cfg.Sources[key]
		if !ok {
			return "(default)"
		}
		if s.File == "" {
			return fmt.Sprintf("(%s)", s.Level)
		}
		return fmt.Sprintf("(%s: %s)", s.Level, s.File)
	}

	apiKeyDisplay := "(not set)"
	if cfg.APIKey != "" {
		if len(cfg.APIKey) > 4 {
			apiKeyDisplay = "****" + cfg.APIKey[len(cfg.APIKey)-4:]
		} else {
			apiKeyDisplay = "****"
		}
	}

	sb.WriteString(fmt.Sprintf("  endpoint: %s %s\n", cfg.Endpoint, srcLabel("endpoint")))
	sb.WriteString(fmt.Sprintf("  api_key: %s %s\n", apiKeyDisplay, srcLabel("api_key")))
	sb.WriteString(fmt.Sprintf("  model_tier: %s %s\n", cfg.ModelTier, srcLabel("model_tier")))
	sb.WriteString(fmt.Sprintf("  auto_update_check: %v %s\n", cfg.AutoUpdateCheck, srcLabel("auto_update_check")))

	sb.WriteString("\nPermissions:\n")
	if len(cfg.Permissions.AllowedDirs) > 0 {
		sb.WriteString("  allowed_dirs:\n")
		for _, d := range cfg.Permissions.AllowedDirs {
			sb.WriteString(fmt.Sprintf("    - %s\n", d))
		}
	}
	if len(cfg.Permissions.AllowedCommands) > 0 {
		sb.WriteString("  allowed_commands:\n")
		for _, c := range cfg.Permissions.AllowedCommands {
			sb.WriteString(fmt.Sprintf("    - %s\n", c))
		}
	}
	if len(cfg.Permissions.DeniedCommands) > 0 {
		sb.WriteString("  denied_commands:\n")
		for _, c := range cfg.Permissions.DeniedCommands {
			sb.WriteString(fmt.Sprintf("    - %s\n", c))
		}
	}
	if len(cfg.Permissions.AllowedDirs) == 0 && len(cfg.Permissions.AllowedCommands) == 0 && len(cfg.Permissions.DeniedCommands) == 0 {
		sb.WriteString("  (none configured)\n")
	}

	sb.WriteString("\nAgent:\n")
	sb.WriteString(fmt.Sprintf("  max_iterations: %d %s\n", cfg.Agent.MaxIterations, srcLabel("agent.max_iterations")))
	sb.WriteString(fmt.Sprintf("  temperature: %g %s\n", cfg.Agent.Temperature, srcLabel("agent.temperature")))
	// max_tokens: 0 = auto, resolved per request by agent.MaxTokensForModel.
	// Display "auto" instead of a literal 0 so it doesn't read like a broken
	// or forgotten config to a user inspecting /doctor.
	if cfg.Agent.MaxTokens == 0 {
		sb.WriteString(fmt.Sprintf("  max_tokens: auto %s\n", srcLabel("agent.max_tokens")))
	} else {
		sb.WriteString(fmt.Sprintf("  max_tokens: %d %s\n", cfg.Agent.MaxTokens, srcLabel("agent.max_tokens")))
	}
	sb.WriteString(fmt.Sprintf("  thinking: %v %s\n", cfg.Agent.Thinking, srcLabel("agent.thinking")))
	if cfg.Agent.Thinking {
		sb.WriteString(fmt.Sprintf("  thinking_mode: %s %s\n", cfg.Agent.ThinkingMode, srcLabel("agent.thinking_mode")))
		if cfg.Agent.ThinkingMode == "enabled" {
			sb.WriteString(fmt.Sprintf("  thinking_budget: %d %s\n", cfg.Agent.ThinkingBudget, srcLabel("agent.thinking_budget")))
		}
	}
	if cfg.Agent.ReasoningEffort != "" {
		sb.WriteString(fmt.Sprintf("  reasoning_effort: %s %s\n", cfg.Agent.ReasoningEffort, srcLabel("agent.reasoning_effort")))
	}
	if cfg.Agent.Model != "" {
		sb.WriteString(fmt.Sprintf("  model: %s %s\n", cfg.Agent.Model, srcLabel("agent.model")))
	}

	sb.WriteString("\nTools:\n")
	sb.WriteString(fmt.Sprintf("  bash_timeout: %d %s\n", cfg.Tools.BashTimeout, srcLabel("tools.bash_timeout")))
	sb.WriteString(fmt.Sprintf("  bash_max_output: %d %s\n", cfg.Tools.BashMaxOutput, srcLabel("tools.bash_max_output")))
	sb.WriteString(fmt.Sprintf("  result_truncation: %d %s\n", cfg.Tools.ResultTruncation, srcLabel("tools.result_truncation")))

	return sb.String()
}

func formatTokenCount(n int) string {
	if n < 1000 {
		return fmt.Sprintf("%d", n)
	}
	return fmt.Sprintf("%d,%03d", n/1000, n%1000)
}

// truncate caps s to max DISPLAY CELLS (not runes), so CJK/wide text is
// measured correctly. The "..." suffix counts toward the budget.
func truncate(s string, max int) string {
	return truncateCells(s, max, "...")
}

func formatPermissions(p *permissions.PermissionsConfig) string {
	var sb strings.Builder
	dimStyle := lipgloss.NewStyle().Foreground(colorDim)

	sb.WriteString(dimStyle.Render("  Allowed commands:") + "\n")
	if len(p.AllowedCommands) == 0 {
		sb.WriteString(dimStyle.Render("    (none)") + "\n")
	} else {
		for _, c := range p.AllowedCommands {
			sb.WriteString(dimStyle.Render(fmt.Sprintf("    - %s", c)) + "\n")
		}
	}

	sb.WriteString(dimStyle.Render("  Denied commands:") + "\n")
	if len(p.DeniedCommands) == 0 {
		sb.WriteString(dimStyle.Render("    (none)") + "\n")
	} else {
		for _, c := range p.DeniedCommands {
			sb.WriteString(dimStyle.Render(fmt.Sprintf("    - %s", c)) + "\n")
		}
	}

	if len(p.AllowedDirs) > 0 {
		sb.WriteString(dimStyle.Render("  Allowed dirs:") + "\n")
		for _, d := range p.AllowedDirs {
			sb.WriteString(dimStyle.Render(fmt.Sprintf("    - %s", d)) + "\n")
		}
	}

	return strings.TrimRight(sb.String(), "\n")
}

func removePattern(list []string, pattern string) []string {
	result := make([]string, 0, len(list))
	for _, item := range list {
		if item != pattern {
			result = append(result, item)
		}
	}
	return result
}
