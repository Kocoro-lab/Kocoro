package tui

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log"
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
	"github.com/Kocoro-lab/ShanClaw/internal/config"
	"github.com/Kocoro-lab/ShanClaw/internal/cwdctx"
	"github.com/Kocoro-lab/ShanClaw/internal/hooks"
	"github.com/Kocoro-lab/ShanClaw/internal/instructions"
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
	stateHistorySearch
)

type agentDoneMsg struct {
	result string
	usage  *agent.TurnUsage
	err    error
}

type approvalRequestMsg struct {
	tool string
	args string
}

type approvalResponse struct {
	allowed bool
	always  bool
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

// outputBlock stores both raw and rendered text so output can be re-rendered on resize.
type outputBlock struct {
	raw      string                 // original markdown (empty for plain text)
	rendered string                 // width-specific rendered text
	rerender func(width int) string // optional: re-render at new width (e.g. startup header)
}

// escTimeoutMsg clears the escape-double-press pending state after 800ms.
type escTimeoutMsg struct{}

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

type toolResultEntry struct {
	name    string
	args    string
	content string
	isError bool
	elapsed time.Duration
}

// liveToolEntry tracks a tool call from start to completion for live display.
type liveToolEntry struct {
	name    string
	keyArg  string        // extracted via toolKeyArg()
	started time.Time
	elapsed time.Duration // set on completion
	done    bool
	isError bool
}

// tokenUpdateMsg carries cumulative token counts from the agent goroutine.
type tokenUpdateMsg struct {
	inputTokens  int
	outputTokens int
}

// stallCheckMsg is sent periodically to detect spinner stalls.
type stallCheckMsg struct{}

// swarmAgentStatusMsg reports sub-agent lifecycle events to the TUI.
type swarmAgentStatusMsg struct {
	id          string // unique per sub-agent invocation
	agentType   string // e.g. "scout", "checker"
	description string // short task description
	status      string // "started", "tool_call", "completed", "error"
	toolName    string // current tool (when status="tool_call")
	toolArgs    string // tool args JSON (when status="tool_call")
	elapsed     time.Duration
	tokens      int
	taskID      string // links to task store ID for unified view
}

// swarmDoneCollapseMsg triggers auto-collapse after all subagents complete.
type swarmDoneCollapseMsg struct{}

// checklistTask represents a single task in the cloud agent workflow checklist.
type checklistTask struct {
	subject string
	status  string // "pending", "in_progress", "completed"
}

// taskChecklistMsg delivers updated task list from cloud agent events.
type taskChecklistMsg struct {
	tasks []checklistTask
}

type Model struct {
	baseCfg             *config.Config
	cfg                 *config.Config
	gateway             *client.GatewayClient
	sessions            *session.Manager
	toolRegistry        *agent.ToolRegistry
	toolCleanup         func()
	agentLoop           *agent.AgentLoop
	textarea            textarea.Model
	output              []outputBlock
	pendingPrints       []string
	processingStartTime time.Time
	spinnerIdx          int
	spinnerTexts        []string
	glyphIdx            int
	colorIdx            int
	lastSessions        []session.SessionSummary // cached for session picker
	sessionPickerIdx    int
	state               state
	width               int
	height              int
	version             string
	approvalCh          chan approvalResponse
	sessionAllowed      map[string]bool // tool → always allowed this session
	pendingApprovalTool string          // tool name for current approval
	pendingApprovalArgs string          // tool args JSON for current approval
	program             *tea.Program
	shannonDir          string
	auditor             *audit.AuditLogger
	hookRunner          *hooks.HookRunner
	customCommands      map[string]string // name → prompt content from commands/*.md
	bypassPermissions   bool
	agentOverride       *agents.Agent      // per-agent override for re-application after async tool load
	loadedSkills        []*skills.Skill    // skills for current agent (survives loop re-creation)
	skillsPtr           *[]*skills.Skill   // pointer into use_skill tool's skills slice
	remoteCleanup       func()             // cleanup for MCP connections from async load
	cancelRun           context.CancelFunc // cancels the running agent loop
	injectCh            chan agent.InjectedMessage
	resumedSession      bool // true when the current session was resumed (not newly created)
	// Tool result display
	pendingToolName string
	pendingToolArgs string
	lastToolResults []toolResultEntry
	toolExpandLevel int // 0=summary only, 1=compact lines, 2=expanded details
	// Live tool progress (replaces pendingToolName/pendingToolArgs during processing)
	liveTools      []liveToolEntry
	lastStreamTime time.Time // last time we received any streaming output
	stallActive    bool      // true when 3s elapsed since lastStreamTime
	tokenInput     int       // cumulative input tokens this turn
	tokenOutput    int       // cumulative output tokens this turn
	// Swarm agent tree
	swarmAgents []swarmAgentEntry
	// Task checklist (from cloud agent workflow)
	checklistTasks []checklistTask
	// View mode toggle (Tab key)
	expandedView string // "", "expanded"
	// Slash command completion menu
	slashCommands []slashCmd // built once in New(), includes builtins + custom/agent cmds
	menuVisible   bool
	menuIndex     int
	menuItems     []slashCmd
	// Startup header animation
	headerFrame     int
	headerDone      bool
	headerHealth    *healthCheckMsg          // buffered until animation ends
	headerSessions  []session.SessionSummary // cached at startup for View()
	headerTipIdx    int                      // stable random tip index
	headerCWD       string                   // cached working directory
	markdownCacheMu sync.RWMutex
	markdownCache   map[string]string
	// Input history (up/down arrow navigation)
	inputHistory []string // most recent last
	historyIdx   int      // -1 = not browsing; 0 = most recent entry
	historyDraft string   // saves current input when browsing starts
	// Escape double-press to clear input
	escPending     bool
	escPendingTime time.Time
	// Ctrl+R history search
	searchQuery    string
	searchMatchIdx int
	searchDraft    string
	searchResult   string
	// Paste content tracking
	pastedContents map[int]string
	pasteNextID    int
	// Task list (file-based task store dir)
	taskDir string
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
	finalHeader := renderStartupHeader(headerTotalFrames-1, m.width, m.version, m.cfg.ModelTier, m.cfg.Endpoint, m.headerCWD, m.headerSessions, m.headerTipIdx)
	// Capture stable values for the rerender closure so the header can be
	// re-rendered at a new width on terminal resize.
	version, tier, ep, cwd := m.version, m.cfg.ModelTier, m.cfg.Endpoint, m.headerCWD
	sessions, tipIdx := m.headerSessions, m.headerTipIdx
	m.output = append(m.output, outputBlock{
		rendered: finalHeader,
		rerender: func(width int) string {
			return renderStartupHeader(headerTotalFrames-1, width, version, tier, ep, cwd, sessions, tipIdx)
		},
	})
	m.appendOutput("")
	m.headerDone = true
	m.state = stateInput

	if m.headerHealth != nil {
		if m.headerHealth.gatewayOK {
			m.appendOutput(fmt.Sprintf("  Connected to %s", m.cfg.Endpoint))
		} else {
			m.appendOutput(fmt.Sprintf("  Warning: API unreachable at %s", m.cfg.Endpoint))
		}
		if m.headerHealth.updateMsg != "" {
			m.appendOutput(fmt.Sprintf("  %s", m.headerHealth.updateMsg))
		}
		m.appendOutput("")
		m.headerHealth = nil
	}
	return m.rerenderOutput()
}

func New(cfg *config.Config, version string, agentOverride *agents.Agent) *Model {
	// Get terminal width for initial sizing
	width := 80
	if w, _, err := term.GetSize(int(os.Stdout.Fd())); err == nil && w > 0 {
		width = w
	}

	ta := textarea.New()
	ta.Placeholder = "Type a message or /help..."
	promptStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("39"))
	ta.SetPromptFunc(2, func(lineIdx int) string {
		if lineIdx == 0 {
			return promptStyle.Render("> ")
		}
		return "  "
	})
	ta.Focus()
	ta.SetHeight(1)
	ta.SetWidth(width)
	ta.ShowLineNumbers = false
	ta.CharLimit = 0 // unlimited
	// Remove cursor line highlight — we use border bars instead
	ta.FocusedStyle.CursorLine = lipgloss.NewStyle()
	ta.BlurredStyle.CursorLine = lipgloss.NewStyle()

	gateway := client.NewGatewayClient(cfg.Endpoint, cfg.APIKey)
	shannonDir := config.ShannonDir()
	agentsDir := filepath.Join(shannonDir, "agents")
	if err := agents.EnsureBuiltins(agentsDir, version); err != nil {
		// Non-fatal: log and continue
		log.Printf("WARNING: failed to sync builtin agents: %v", err)
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

	// Create audit logger (best-effort)
	var auditor *audit.AuditLogger
	if shannonDir != "" {
		logDir := filepath.Join(shannonDir, "logs")
		if a, err := audit.NewAuditLogger(logDir); err == nil {
			auditor = a
		}
	}

	// Local tools only (fast, sync) — MCP + gateway loaded async in Init
	reg, skillsPtr, toolCleanup := tools.RegisterLocalTools(runtimeCfg)
	tools.RegisterSessionSearch(reg, sessMgr)

	hookRunner := hooks.NewHookRunner(runtimeCfg.Hooks)
	loop := agent.NewAgentLoop(gateway, reg, runtimeCfg.ModelTier, shannonDir, runtimeCfg.Agent.MaxIterations, runtimeCfg.Tools.ResultTruncation, runtimeCfg.Tools.ArgsTruncation, &runtimeCfg.Permissions, auditor, hookRunner)
	loop.SetMaxTokens(runtimeCfg.Agent.MaxTokens)
	loop.SetTemperature(runtimeCfg.Agent.Temperature)
	loop.SetContextWindow(runtimeCfg.Agent.ContextWindow)
	if runtimeCfg.Agent.Model != "" {
		loop.SetSpecificModel(runtimeCfg.Agent.Model)
	}
	if runtimeCfg.Agent.Thinking {
		if runtimeCfg.Agent.ThinkingMode == "enabled" {
			loop.SetThinking(&client.ThinkingConfig{Type: "enabled", BudgetTokens: runtimeCfg.Agent.ThinkingBudget})
		} else {
			loop.SetThinking(&client.ThinkingConfig{Type: "adaptive"})
		}
	}
	if runtimeCfg.Agent.ReasoningEffort != "" {
		loop.SetReasoningEffort(runtimeCfg.Agent.ReasoningEffort)
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
	} else {
		loop.SetMemoryDir(filepath.Join(shannonDir, "memory"))
		if loadedSkills != nil {
			loop.SetSkills(loadedSkills)
		}
	}
	loop.SetEnableStreaming(true) // streaming enabled but deltas are suppressed — only final text rendered
	loop.SetQuiet(true)

	settings := config.LoadSettings()

	taskDir := ""
	if shannonDir != "" {
		taskDir = filepath.Join(shannonDir, "tasks", "default")
	}

	customCmds, instanceCmds := buildRuntimeCommands(shannonDir, initialCWD, agentOverride)

	m := &Model{
		baseCfg:        cfg,
		cfg:            runtimeCfg,
		gateway:        gateway,
		sessions:       sessMgr,
		agentLoop:      loop,
		textarea:       ta,
		width:          width,
		version:        version,
		approvalCh:     make(chan approvalResponse, 1),
		sessionAllowed: make(map[string]bool),
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
		markdownCache:  make(map[string]string),
		slashCommands:  instanceCmds,
		historyIdx:     -1,
		pastedContents: make(map[int]string),
		taskDir:        taskDir,
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
	loop := agent.NewAgentLoop(m.gateway, m.toolRegistry, m.cfg.ModelTier, m.shannonDir, m.cfg.Agent.MaxIterations, m.cfg.Tools.ResultTruncation, m.cfg.Tools.ArgsTruncation, &m.cfg.Permissions, m.auditor, m.hookRunner)
	loop.SetMaxTokens(m.cfg.Agent.MaxTokens)
	loop.SetTemperature(m.cfg.Agent.Temperature)
	loop.SetContextWindow(m.cfg.Agent.ContextWindow)
	if m.cfg.Agent.Model != "" {
		loop.SetSpecificModel(m.cfg.Agent.Model)
	}
	if m.cfg.Agent.Thinking {
		if m.cfg.Agent.ThinkingMode == "enabled" {
			loop.SetThinking(&client.ThinkingConfig{Type: "enabled", BudgetTokens: m.cfg.Agent.ThinkingBudget})
		} else {
			loop.SetThinking(&client.ThinkingConfig{Type: "adaptive"})
		}
	}
	if m.cfg.Agent.ReasoningEffort != "" {
		loop.SetReasoningEffort(m.cfg.Agent.ReasoningEffort)
	}
	if m.agentOverride != nil && m.agentOverride.Config != nil && m.agentOverride.Config.Agent != nil {
		ac := m.agentOverride.Config.Agent
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
	loop.SetBypassPermissions(m.bypassPermissions)
	loop.SetEnableStreaming(true)
	loop.SetQuiet(true)
	loop.SetDeltaProvider(agent.NewTemporalDelta())
	if m.agentOverride != nil {
		scopedMCPCtx := tools.ResolveMCPContext(m.cfg, m.agentOverride)
		agentDir := filepath.Join(m.shannonDir, "agents", m.agentOverride.Name)
		loop.SwitchAgent(m.agentOverride.Prompt, agentDir, nil, scopedMCPCtx, m.loadedSkills)
	} else {
		loop.SetMemoryDir(filepath.Join(m.shannonDir, "memory"))
		if m.loadedSkills != nil {
			loop.SetSkills(m.loadedSkills)
		}
		mcpCtx := tools.ResolveMCPContext(m.cfg)
		if mcpCtx != "" {
			loop.SetMCPContext(mcpCtx)
		}
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

		// Cloud delegation tool
		var cloudAgentName, cloudAgentPrompt string
		if m.agentOverride != nil {
			cloudAgentName = m.agentOverride.Name
			cloudAgentPrompt = m.agentOverride.Prompt
		}
		tools.RegisterCloudDelegate(reg, m.gateway, m.cfg, nil, cloudAgentName, cloudAgentPrompt)

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
		msg.gatewayOK = m.gateway.Health(ctx) == nil

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
	// Note: when cmd is rerenderOutput (e.g. agentDoneMsg, approvalRequestMsg),
	// it already drained pendingPrints, so flushPrints returns nil here.
	if flush := m.flushPrints(); flush != nil {
		if cmd != nil {
			cmd = tea.Sequence(flush, cmd)
		} else {
			cmd = flush
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
				if m.cancelRun != nil {
					m.cancelRun()
					m.cancelRun = nil
					m.injectCh = nil
				}
				// Unblock approval goroutine if waiting
				if m.state == stateApproval {
					select {
					case m.approvalCh <- approvalResponse{allowed: false}:
					default:
					}
				}
				// Roll back the user message added in handleSubmit
				sess := m.sessions.Current()
				if len(sess.Messages) > 0 && sess.Messages[len(sess.Messages)-1].Role == "user" {
					sess.Messages = sess.Messages[:len(sess.Messages)-1]
				}
				m.sessions.Save()
				cancelStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("243"))
				m.appendOutput(cancelStyle.Render("  [Cancelled]"))
				m.state = stateInput
				m.textarea.Focus()
				return m, m.rerenderOutput()
			}
			if m.menuVisible {
				m.menuVisible = false
				return m, nil
			}
			// Escape double-press to clear input
			if m.state == stateInput {
				val := strings.TrimSpace(m.textarea.Value())
				if val == "" {
					return m, nil
				}
				if m.escPending && time.Since(m.escPendingTime) < 800*time.Millisecond {
					// Second press: clear input, save to history
					m.inputHistory = append(m.inputHistory, m.textarea.Value())
					m.textarea.Reset()
					m.textarea.SetHeight(1)
					m.historyIdx = -1
					m.historyDraft = ""
					m.escPending = false
					return m, nil
				}
				// First press: start pending
				m.escPending = true
				m.escPendingTime = time.Now()
				return m, tea.Tick(800*time.Millisecond, func(time.Time) tea.Msg {
					return escTimeoutMsg{}
				})
			}
		case tea.KeyTab:
			if m.state == stateProcessing {
				if m.expandedView == "" {
					m.expandedView = "expanded"
				} else {
					m.expandedView = ""
				}
				return m, nil
			}
			// Tab on empty input: open slash command menu
			if m.state == stateInput && !m.menuVisible && m.textarea.Value() == "" {
				m.textarea.SetValue("/")
				m.updateMenu()
				return m, nil
			}
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
				m.textarea.SetValue(selected.cmd + " ")
				m.menuVisible = false
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
			// History navigation: Up when textarea is single-line
			if m.state == stateInput && !m.menuVisible && len(m.inputHistory) > 0 {
				lines := strings.Count(m.textarea.Value(), "\n")
				if lines == 0 {
					if m.historyIdx == -1 {
						// First press: save current input as draft
						m.historyDraft = m.textarea.Value()
						m.historyIdx = 0
					} else if m.historyIdx < len(m.inputHistory)-1 {
						m.historyIdx++
					} else {
						return m, nil // at oldest entry
					}
					entry := m.inputHistory[len(m.inputHistory)-1-m.historyIdx]
					m.textarea.SetValue(entry)
					m.adjustTextareaHeight()
					m.updateMenu()
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
			// History navigation: Down to go back toward draft
			if m.state == stateInput && !m.menuVisible && m.historyIdx >= 0 {
				lines := strings.Count(m.textarea.Value(), "\n")
				if lines == 0 {
					m.historyIdx--
					if m.historyIdx < 0 {
						// Back to draft
						m.historyIdx = -1
						m.textarea.SetValue(m.historyDraft)
						m.historyDraft = ""
					} else {
						entry := m.inputHistory[len(m.inputHistory)-1-m.historyIdx]
						m.textarea.SetValue(entry)
					}
					m.adjustTextareaHeight()
					m.updateMenu()
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

		// Readline shortcuts (stateInput only)
		if m.state == stateInput {
			switch msg.String() {
			case "ctrl+k": // Kill to end of line
				li := m.textarea.LineInfo()
				val := []rune(m.textarea.Value())
				col := li.ColumnOffset
				if col < len(val) {
					m.textarea.SetValue(string(val[:col]))
					m.textarea.CursorEnd()
				}
				return m, nil
			case "ctrl+u": // Kill to start of line
				li := m.textarea.LineInfo()
				val := []rune(m.textarea.Value())
				col := li.ColumnOffset
				if col > 0 {
					m.textarea.SetValue(string(val[col:]))
					m.textarea.CursorStart()
				}
				return m, nil
			case "ctrl+w": // Kill word backward
				li := m.textarea.LineInfo()
				val := []rune(m.textarea.Value())
				col := li.ColumnOffset
				if col == 0 {
					return m, nil
				}
				i := col - 1
				for i > 0 && val[i-1] == ' ' {
					i--
				}
				for i > 0 && val[i-1] != ' ' {
					i--
				}
				newVal := string(val[:i]) + string(val[col:])
				m.textarea.SetValue(newVal)
				m.textarea.SetCursor(i)
				return m, nil
			case "ctrl+l": // Clear screen + scrollback
				m.output = nil
				m.pendingPrints = m.pendingPrints[:0]
				m.textarea.Focus()
				return m, func() tea.Msg {
					os.Stdout.WriteString("\033[3J")
					return tea.ClearScreen()
				}
			case "ctrl+r": // History search
				if len(m.inputHistory) > 0 {
					m.searchDraft = m.textarea.Value()
					m.searchQuery = ""
					m.searchMatchIdx = 0
					m.searchResult = ""
					m.state = stateHistorySearch
					m.textarea.Blur()
				}
				return m, nil
			}
		}

		if m.state == stateHistorySearch {
			switch msg.Type {
			case tea.KeyEscape:
				m.textarea.SetValue(m.searchDraft)
				m.state = stateInput
				m.textarea.Focus()
				return m, nil
			case tea.KeyEnter:
				if m.searchResult != "" {
					m.textarea.SetValue(m.searchResult)
				} else {
					m.textarea.SetValue(m.searchDraft)
				}
				m.state = stateInput
				m.textarea.Focus()
				m.adjustTextareaHeight()
				m.updateMenu()
				return m, nil
			case tea.KeyCtrlR:
				m.searchMatchIdx++
				result, _, found := m.searchHistory(m.searchQuery, m.searchMatchIdx)
				if found {
					m.searchResult = result
				} else {
					m.searchMatchIdx--
				}
				return m, nil
			case tea.KeyBackspace:
				if len(m.searchQuery) == 0 {
					m.textarea.SetValue(m.searchDraft)
					m.state = stateInput
					m.textarea.Focus()
					return m, nil
				}
				runes := []rune(m.searchQuery)
				m.searchQuery = string(runes[:len(runes)-1])
				m.searchMatchIdx = 0
				if m.searchQuery != "" {
					result, _, found := m.searchHistory(m.searchQuery, 0)
					if found {
						m.searchResult = result
					}
				} else {
					m.searchResult = ""
				}
				return m, nil
			case tea.KeyRunes:
				m.searchQuery += string(msg.Runes)
				m.searchMatchIdx = 0
				result, _, found := m.searchHistory(m.searchQuery, 0)
				if found {
					m.searchResult = result
				}
				return m, nil
			}
			return m, nil
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

		if m.state == stateApproval {
			switch msg.String() {
			case "y", "Y":
				select {
				case m.approvalCh <- approvalResponse{allowed: true}:
				default:
				}
				m.state = stateProcessing
				return m, nil
			case "n", "N":
				select {
				case m.approvalCh <- approvalResponse{allowed: false}:
				default:
				}
				m.state = stateProcessing
				return m, nil
			case "a", "A":
				m.sessionAllowed[m.pendingApprovalTool] = true
				select {
				case m.approvalCh <- approvalResponse{allowed: true, always: true}:
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
		m.textarea.SetWidth(msg.Width)
		if oldWidth != msg.Width && oldWidth > 0 && len(m.output) > 0 {
			return m, m.rerenderOutput()
		}
		return m, nil

	case spinnerFrameMsg:
		if m.state == stateProcessing || m.state == stateApproval {
			m.glyphIdx++
			m.colorIdx++
			return m, spinnerFrameTick()
		}
		return m, nil

	case spinnerTickMsg:
		if m.state == stateProcessing || m.state == stateApproval {
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
		m.textarea.Focus()
		m.cancelRun = nil
		m.injectCh = nil
		if msg.err != nil && !errors.Is(msg.err, context.Canceled) {
			m.appendOutput("Error: " + msg.err.Error())
		}
		// Display the assistant response (rendered here instead of OnText to
		// avoid a race where the Println Cmd arrives after state has changed).
		if msg.result != "" && (msg.err == nil || errors.Is(msg.err, agent.ErrMaxIterReached)) {
			m.appendMarkdownOutput(msg.result, m.renderMarkdownCached(msg.result, m.width))
			m.appendOutput("")
		}
		// Tool summary — individual results were shown in the live panel during
		// processing. Show a compact count line now.
		if n := len(m.lastToolResults); n > 0 {
			m.toolExpandLevel = 0
			dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("243"))
			errCount := 0
			for _, tr := range m.lastToolResults {
				if tr.isError {
					errCount++
				}
			}
			summary := fmt.Sprintf("  %d tool calls", n)
			if errCount > 0 {
				summary += fmt.Sprintf(" (%d errors)", errCount)
			}
			m.appendOutput(dimStyle.Render(summary))
		}
		// Don't show usage/elapsed for cancelled tasks
		if msg.err == nil || errors.Is(msg.err, agent.ErrMaxIterReached) {
			elapsed := formatElapsed(time.Since(m.processingStartTime))
			usageDim := lipgloss.NewStyle().Foreground(lipgloss.Color("243"))
			if msg.usage != nil {
				usageStr := fmt.Sprintf("  tokens: %d | cost: $%.4f", msg.usage.TotalTokens, msg.usage.CostUSD)
				if msg.usage.Model != "" {
					usageStr += " | model: " + msg.usage.Model
				}
				usageStr += " | " + elapsed
				m.appendOutput(usageDim.Render(usageStr))
			} else {
				m.appendOutput(usageDim.Render("  " + elapsed))
			}
		}
		m.sessions.Save()
		// Full clear-and-repaint so the response, usage line, and input bar
		// are all positioned correctly — incremental Println can mis-position
		// lines when the view height changes between processing and input.
		return m, m.rerenderOutput()

	case approvalRequestMsg:
		m.state = stateApproval
		m.pendingApprovalTool = msg.tool
		m.pendingApprovalArgs = msg.args
		dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("243"))
		warnIcon := lipgloss.NewStyle().Foreground(lipgloss.Color("214")).Render("?")
		keyArg := toolKeyArg(msg.tool, msg.args)
		m.appendOutput(dimStyle.Render(fmt.Sprintf("⏵ %s(%s)  %s  Allow?", msg.tool, keyArg, warnIcon)))
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

	case streamOutputMsg:
		m.lastStreamTime = time.Now()
		m.stallActive = false
		if msg.raw != "" {
			m.appendMarkdownOutput(msg.raw, msg.text)
		} else {
			m.appendOutput(msg.text)
		}
		return m, nil

	case toolCallMsg:
		m.pendingToolName = msg.name
		m.pendingToolArgs = msg.args
		m.lastStreamTime = time.Now()
		m.stallActive = false
		// Track live tool for progress display
		m.liveTools = append(m.liveTools, liveToolEntry{
			name:    msg.name,
			keyArg:  toolKeyArg(msg.name, msg.args),
			started: time.Now(),
		})
		// Advance spinner phrase on real events
		m.spinnerIdx = (m.spinnerIdx + 1) % len(m.spinnerTexts)
		return m, nil

	case toolResultMsg:
		m.lastStreamTime = time.Now()
		m.stallActive = false
		// Prefer msg fields (authoritative per-result) over pending fields
		// (shared slot, unreliable for concurrent tool calls like subagent).
		toolName := msg.name
		toolArgs := msg.args
		if toolName == "" {
			toolName = m.pendingToolName
		}
		if toolArgs == "" {
			toolArgs = m.pendingToolArgs
		}
		// During processing, tool results are shown via the live panel (liveTools),
		// not as static text. Only record for post-processing summary.
		if toolName != "think" {
			entry := toolResultEntry{name: toolName, args: toolArgs, content: msg.content, isError: msg.isError, elapsed: msg.elapsed}
			m.lastToolResults = append(m.lastToolResults, entry)
			if len(m.lastToolResults) > 20 {
				m.lastToolResults = m.lastToolResults[1:]
			}
		}
		// Mark matching live tool as done
		for i := len(m.liveTools) - 1; i >= 0; i-- {
			if m.liveTools[i].name == msg.name && !m.liveTools[i].done {
				m.liveTools[i].done = true
				m.liveTools[i].isError = msg.isError
				m.liveTools[i].elapsed = msg.elapsed
				break
			}
		}
		m.pendingToolName = ""
		m.pendingToolArgs = ""
		m.toolExpandLevel = 0
		return m, nil

	case clipboardResultMsg:
		if msg.err != nil {
			m.appendOutput(fmt.Sprintf("Copy failed: %v", msg.err))
		} else {
			m.appendOutput(fmt.Sprintf("Copied to clipboard (%d chars)", msg.len))
		}
		return m, nil

	case stallCheckMsg:
		if m.state == stateProcessing || m.state == stateApproval {
			if !m.lastStreamTime.IsZero() && time.Since(m.lastStreamTime) > 3*time.Second {
				m.stallActive = true
			} else {
				m.stallActive = false
			}
			return m, stallCheckTick()
		}

	case tokenUpdateMsg:
		m.tokenInput = msg.inputTokens
		m.tokenOutput = msg.outputTokens

	case swarmAgentStatusMsg:
		switch msg.status {
		case "started":
			m.swarmAgents = append(m.swarmAgents, swarmAgentEntry{
				id:          msg.id,
				agentType:   msg.agentType,
				description: msg.description,
				status:      "running",
				started:     time.Now(),
				taskID:      msg.taskID,
			})
		case "completed", "error":
			for i := range m.swarmAgents {
				if m.swarmAgents[i].id == msg.id {
					m.swarmAgents[i].status = msg.status
					m.swarmAgents[i].elapsed = time.Since(m.swarmAgents[i].started)
					if msg.tokens > 0 {
						m.swarmAgents[i].tokenCount = msg.tokens
					}
					break
				}
			}
			// Auto-collapse after 2s if all agents are done
			allDone := true
			for _, ag := range m.swarmAgents {
				if ag.status == "running" {
					allDone = false
					break
				}
			}
			if allDone && len(m.swarmAgents) > 0 {
				return m, tea.Tick(2*time.Second, func(time.Time) tea.Msg {
					return swarmDoneCollapseMsg{}
				})
			}
		case "tool_call":
			for i := range m.swarmAgents {
				if m.swarmAgents[i].id == msg.id {
					m.swarmAgents[i].activity = msg.toolName
					m.swarmAgents[i].toolUseCount++
					if msg.tokens > 0 {
						m.swarmAgents[i].tokenCount = msg.tokens
					}
					// Update recent tools sliding window (keep last 10)
					isRead, isSearch := classifyTool(msg.toolName)
					entry := recentToolEntry{
						name:     msg.toolName,
						keyArg:   toolKeyArg(msg.toolName, msg.toolArgs),
						isRead:   isRead,
						isSearch: isSearch,
					}
					m.swarmAgents[i].recentTools = append(m.swarmAgents[i].recentTools, entry)
					if len(m.swarmAgents[i].recentTools) > 10 {
						m.swarmAgents[i].recentTools = m.swarmAgents[i].recentTools[1:]
					}
					break
				}
			}
		}

	case taskChecklistMsg:
		m.checklistTasks = msg.tasks

	case swarmDoneCollapseMsg:
		if m.state == stateProcessing {
			m.expandedView = ""
			m.swarmAgents = nil
		}

	case escTimeoutMsg:
		m.escPending = false
	}

	// Detect and truncate large pastes
	if m.state == stateInput {
		if km, ok := msg.(tea.KeyMsg); ok && km.Paste && len(string(km.Runes)) >= pasteThreshold {
			pasted := m.truncatePaste(string(km.Runes))
			m.textarea.InsertString(pasted)
			m.adjustTextareaHeight()
			m.updateMenu()
			return m, nil
		}
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

func (m *Model) View() string {
	var sb strings.Builder

	barStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("237"))
	bar := barStyle.Render(strings.Repeat("─", m.width))

	// --- Input / status line ---
	switch m.state {
	case stateStartup:
		sb.WriteString(renderStartupHeader(m.headerFrame, m.width, m.version, m.cfg.ModelTier, m.cfg.Endpoint, m.headerCWD, m.headerSessions, m.headerTipIdx))
	case stateInput:
		sb.WriteString(bar)
		sb.WriteString("\n")
		sb.WriteString(m.textarea.View())
		sb.WriteString("\n")
		// Bottom bar with right-aligned model tier
		tierDim := lipgloss.NewStyle().Foreground(lipgloss.Color("243"))
		rightInfo := tierDim.Render(m.cfg.ModelTier)
		if m.escPending {
			hintStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("243")).Italic(true)
			rightInfo = hintStyle.Render("Esc again to clear") + "  " + rightInfo
		}
		barWidth := m.width - lipgloss.Width(rightInfo)
		if barWidth < 0 {
			barWidth = 0
		}
		sb.WriteString(barStyle.Render(strings.Repeat("─", barWidth)) + rightInfo)
	case stateProcessing:
		sb.WriteString(bar)
		sb.WriteString("\n")
		glyph := dotFrames[m.glyphIdx%len(dotFrames)]
		color := spinColors[m.colorIdx%len(spinColors)]
		glyphStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(color))
		if m.stallActive {
			glyphStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
		}

		hasSubagents := len(m.swarmAgents) > 0

		var spinnerLine string
		if hasSubagents {
			spinnerLine = glyphStyle.Render(glyph) + " " + renderSummaryLine(m.swarmAgents, m.width-4)
		} else if m.pendingToolName != "" {
			dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("243"))
			keyArg := toolKeyArg(m.pendingToolName, m.pendingToolArgs)
			spinnerLine = glyphStyle.Render(glyph) + dimStyle.Render(fmt.Sprintf(" %s(%s)", m.pendingToolName, keyArg))
		} else {
			spinnerText := m.spinnerTexts[m.spinnerIdx%len(m.spinnerTexts)]
			spinnerLine = glyphStyle.Render(glyph) + " " + renderWaveText(spinnerText, m.glyphIdx)
		}

		totalTokens := m.tokenInput + m.tokenOutput
		if hasSubagents {
			for _, ag := range m.swarmAgents {
				totalTokens += ag.tokenCount
			}
		}
		if totalTokens > 0 {
			tokenDim := lipgloss.NewStyle().Foreground(lipgloss.Color("243"))
			tokenStr := tokenDim.Render(formatTokenCount(totalTokens))
			gap := m.width - lipgloss.Width(spinnerLine) - lipgloss.Width(tokenStr) - 1
			if gap > 0 {
				spinnerLine += strings.Repeat(" ", gap) + tokenStr
			}
		}

		sb.WriteString(spinnerLine)
		sb.WriteString("\n")

		if hasSubagents {
			// Swarm tree: always show agent activity when subagents are running
			if m.expandedView == "expanded" {
				sb.WriteString(renderSwarmTree(m.swarmAgents, m.width))
				sb.WriteString("\n")
				if len(m.checklistTasks) > 0 {
					sb.WriteString(renderChecklist(m.checklistTasks, m.width))
				}
			} else {
				sb.WriteString(renderCompactTree(m.swarmAgents, m.width))
				sb.WriteString("\n")
			}
			// Task list: show alongside swarm tree if tasks exist
			if tl := renderTaskList(m.taskDir, m.swarmAgents, m.width); tl != "" {
				sb.WriteString(tl)
			}
		} else {
			// No subagents — show task list or tool progress
			if tl := renderTaskList(m.taskDir, nil, m.width); tl != "" {
				sb.WriteString(tl)
			} else if len(m.checklistTasks) > 0 {
				sb.WriteString(renderChecklist(m.checklistTasks, m.width))
			} else if len(m.liveTools) > 0 {
				sb.WriteString(renderToolProgress(m.liveTools, m.width, m.stallActive))
				sb.WriteString("\n")
			}
		}

		elapsed := formatElapsed(time.Since(m.processingStartTime))
		tierDim := lipgloss.NewStyle().Foreground(lipgloss.Color("243"))
		var viewHint string
		if m.expandedView != "" {
			viewHint = "[expanded] "
		}
		rightInfo := tierDim.Render(viewHint + m.cfg.ModelTier + " " + elapsed)
		statusBarWidth := m.width - lipgloss.Width(rightInfo)
		if statusBarWidth < 0 {
			statusBarWidth = 0
		}
		sb.WriteString(barStyle.Render(strings.Repeat("─", statusBarWidth)) + rightInfo + "\n")
	case stateApproval:
		// Reuse the full processing view (spinner, tool progress, tasks)
		// so dynamic output stays visible during approval.
		sb.WriteString(bar)
		sb.WriteString("\n")
		glyph := dotFrames[m.glyphIdx%len(dotFrames)]
		color := spinColors[m.colorIdx%len(spinColors)]
		glyphStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(color))

		hasSubagents := len(m.swarmAgents) > 0

		var spinnerLine string
		if hasSubagents {
			spinnerLine = glyphStyle.Render(glyph) + " " + renderSummaryLine(m.swarmAgents, m.width-4)
		} else {
			spinnerText := m.spinnerTexts[m.spinnerIdx%len(m.spinnerTexts)]
			spinnerLine = glyphStyle.Render(glyph) + " " + renderWaveText(spinnerText, m.glyphIdx)
		}

		totalTokens := m.tokenInput + m.tokenOutput
		if hasSubagents {
			for _, ag := range m.swarmAgents {
				totalTokens += ag.tokenCount
			}
		}
		if totalTokens > 0 {
			tokenDim := lipgloss.NewStyle().Foreground(lipgloss.Color("243"))
			tokenStr := tokenDim.Render(formatTokenCount(totalTokens))
			gap := m.width - lipgloss.Width(spinnerLine) - lipgloss.Width(tokenStr) - 1
			if gap > 0 {
				spinnerLine += strings.Repeat(" ", gap) + tokenStr
			}
		}

		sb.WriteString(spinnerLine)
		sb.WriteString("\n")

		if hasSubagents {
			if m.expandedView == "expanded" {
				sb.WriteString(renderSwarmTree(m.swarmAgents, m.width))
				sb.WriteString("\n")
				if len(m.checklistTasks) > 0 {
					sb.WriteString(renderChecklist(m.checklistTasks, m.width))
				}
			} else {
				sb.WriteString(renderCompactTree(m.swarmAgents, m.width))
				sb.WriteString("\n")
			}
		} else {
			if len(m.checklistTasks) > 0 {
				sb.WriteString(renderChecklist(m.checklistTasks, m.width))
			} else if len(m.liveTools) > 0 {
				sb.WriteString(renderToolProgress(m.liveTools, m.width, m.stallActive))
				sb.WriteString("\n")
			}
		}

		if len(m.checklistTasks) == 0 {
			if tl := renderTaskList(m.taskDir, m.swarmAgents, m.width); tl != "" {
				sb.WriteString(tl)
			}
		}

		// Approval prompt line: show which tool needs approval
		warnStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
		dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("243"))
		keyArg := toolKeyArg(m.pendingApprovalTool, m.pendingApprovalArgs)
		sb.WriteString(dimStyle.Render(fmt.Sprintf("  ⏵ %s(%s)", m.pendingApprovalTool, keyArg)))
		sb.WriteString("  " + warnStyle.Render("?") + "  Allow?  ")
		sb.WriteString(warnStyle.Render("[y/n/a]"))
		sb.WriteString("\n")

		// Bottom bar with elapsed time and model tier (same as stateProcessing)
		elapsed := formatElapsed(time.Since(m.processingStartTime))
		tierDim := lipgloss.NewStyle().Foreground(lipgloss.Color("243"))
		rightInfo := tierDim.Render(m.cfg.ModelTier + " " + elapsed)
		statusBarWidth := m.width - lipgloss.Width(rightInfo)
		if statusBarWidth < 0 {
			statusBarWidth = 0
		}
		sb.WriteString(barStyle.Render(strings.Repeat("─", statusBarWidth)) + rightInfo)
	case stateHistorySearch:
		sb.WriteString(bar)
		sb.WriteString("\n")
		searchStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("39"))
		queryStyle := lipgloss.NewStyle().Bold(true)
		sb.WriteString(searchStyle.Render("bck-i-search: ") + queryStyle.Render(m.searchQuery) + "_")
		sb.WriteString("\n")
		if m.searchResult != "" {
			dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("243"))
			sb.WriteString(dimStyle.Render("  " + m.searchResult))
			sb.WriteString("\n")
		}
		sb.WriteString(bar)
	case stateSessionPicker:
		sb.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color("39")).Render("  Sessions (Up/Down, Enter, Esc)"))
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
			desc := fmt.Sprintf("[%s] %d msgs", s.CreatedAt.Format("Jan 02 15:04"), s.MsgCount)
			return title, desc
		}))
	}

	return sb.String()
}

func (m *Model) handleSubmit() (tea.Model, tea.Cmd) {
	input := strings.TrimSpace(m.textarea.Value())
	m.textarea.Reset()
	m.textarea.SetHeight(1)
	m.textarea.Blur()

	if input == "" {
		return m, nil
	}

	// Expand any truncated paste placeholders
	if len(m.pastedContents) > 0 {
		input = expandPastedContent(input, m.pastedContents)
		m.pastedContents = make(map[int]string)
		m.pasteNextID = 0
	}

	// Save to input history and reset browsing state
	m.inputHistory = append(m.inputHistory, input)
	m.historyIdx = -1
	m.historyDraft = ""

	promptMark := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("252")).Render(">")
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
	m.processingStartTime = time.Now()
	sess := m.sessions.Current()
	// Set title from first user message
	if sess.Title == "New session" {
		sess.Title = session.Title(input)
	}
	userMsgTime := time.Now()
	sess.Messages = append(sess.Messages, client.Message{Role: "user", Content: client.NewTextContent(input)})
	sess.MessageMeta = append(sess.MessageMeta, session.MessageMeta{Source: "local", Timestamp: session.TimePtr(userMsgTime)})

	m.spinnerIdx = 0
	m.glyphIdx = 0
	m.colorIdx = 0
	m.liveTools = nil
	m.stallActive = false
	m.lastStreamTime = time.Now()
	m.tokenInput = 0
	m.tokenOutput = 0
	m.swarmAgents = nil
	m.checklistTasks = nil
	clearTaskStore(m.taskDir)
	m.expandedView = ""
	// Pass everything except the just-appended user message as history,
	// stripping any prior loop-injected guardrail nudges so they can't
	// leak into this run's conversation snapshot.
	priorMsgs := sess.Messages[:len(sess.Messages)-1]
	priorMeta := sess.MessageMeta
	if len(priorMeta) > len(priorMsgs) {
		priorMeta = priorMeta[:len(priorMsgs)]
	}
	history := session.FilterInjected(priorMsgs, priorMeta)
	return m, tea.Batch(m.runAgentLoop(input, history), spinnerTick(), spinnerFrameTick(), stallCheckTick())
}

func (m *Model) runAgentLoop(query string, history []client.Message) tea.Cmd {
	ctx, cancel := context.WithCancel(context.Background())
	m.cancelRun = cancel
	m.injectCh = make(chan agent.InjectedMessage, 10)
	return func() tea.Msg {
		if sess := m.sessions.Current(); sess != nil {
			effectiveCWD := m.applyRuntimeContext(sess)
			handler := &tuiEventHandler{model: m}
			m.agentLoop.SetHandler(handler)
			m.agentLoop.SetInjectCh(m.injectCh)
			// Wire handler to cloud_delegate tool so it can stream events
			if ct, ok := m.toolRegistry.Get("cloud_delegate"); ok {
				if cdt, ok := ct.(*tools.CloudDelegateTool); ok {
					cdt.SetHandler(handler)
				}
			}
			// Wire SubAgentRunner so subagent tool can launch child agent loops
			if st, ok := m.toolRegistry.Get("subagent"); ok {
				if sat, ok := st.(*tools.SubAgentTool); ok {
					capturedCWD := effectiveCWD
					sat.SetOnStatus(func(id, agentType, description, status, toolName, toolArgs string, tokens int, taskID string) {
						if m.program != nil {
							m.program.Send(swarmAgentStatusMsg{
								id:          id,
								agentType:   agentType,
								description: description,
								status:      status,
								toolName:    toolName,
								toolArgs:    toolArgs,
								tokens:      tokens,
								taskID:      taskID,
							})
						}
					})
					sat.SetRunner(func(ctx context.Context, ag *agents.Agent, prompt string, childReg *agent.ToolRegistry, handler agent.EventHandler) tools.SubAgentResult {
						start := time.Now()
						childLoop := agent.NewAgentLoop(m.gateway, childReg, m.cfg.ModelTier, m.shannonDir,
							tools.SubAgentMaxIterations, m.cfg.Tools.ResultTruncation, m.cfg.Tools.ArgsTruncation,
							&m.cfg.Permissions, m.auditor, m.hookRunner)
						childLoop.SetMaxTokens(tools.SubAgentMaxTokens)
						childLoop.SetTemperature(m.cfg.Agent.Temperature)
						childLoop.SetContextWindow(m.cfg.Agent.ContextWindow)
						childLoop.SetEnableStreaming(false)
						childLoop.SetQuiet(true)
						childLoop.SetThinking(&client.ThinkingConfig{Type: "disabled"})
						childLoop.SetBypassPermissions(m.bypassPermissions)
						childLoop.SetSessionCWD(capturedCWD)
						if handler != nil {
							childLoop.SetHandler(handler)
						}

						childAgentDir := filepath.Join(m.shannonDir, "agents", ag.Name)
						childMCPCtx := tools.ResolveMCPContext(m.cfg, ag)
						childLoop.SwitchAgent(ag.Prompt, childAgentDir, nil, childMCPCtx, ag.Skills)

						if ag.Config != nil && ag.Config.Agent != nil {
							ac := ag.Config.Agent
							if ac.Model != nil {
								childLoop.SetSpecificModel(*ac.Model)
							}
							if ac.ModelTier != nil {
								childLoop.SetModelTier(*ac.ModelTier)
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

						childCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
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
					sat.SetBaseRegistry(m.toolRegistry)
				}
			}
			m.agentLoop.SetSessionID(sess.ID)
			m.agentLoop.SetWorkingSet(m.sessions.WorkingSet(sess.ID))
			m.agentLoop.SetSessionCWD(effectiveCWD)

			cleanupSpills := m.agentLoop.SpillCleanupFunc()
			m.sessions.OnSessionClose(sess.ID, func() {
				cleanupSpills()
				m.agentLoop.SetWorkingSet(nil)
			})
		} else {
			handler := &tuiEventHandler{model: m}
			m.agentLoop.SetHandler(handler)
			m.agentLoop.SetInjectCh(m.injectCh)
			if ct, ok := m.toolRegistry.Get("cloud_delegate"); ok {
				if cdt, ok := ct.(*tools.CloudDelegateTool); ok {
					cdt.SetHandler(handler)
				}
			}
			// Wire SubAgentRunner so subagent tool can launch child agent loops
			if st, ok := m.toolRegistry.Get("subagent"); ok {
				if sat, ok := st.(*tools.SubAgentTool); ok {
					sat.SetRunner(func(ctx context.Context, ag *agents.Agent, prompt string, childReg *agent.ToolRegistry, handler agent.EventHandler) tools.SubAgentResult {
						start := time.Now()
						childLoop := agent.NewAgentLoop(m.gateway, childReg, m.cfg.ModelTier, m.shannonDir,
							tools.SubAgentMaxIterations, m.cfg.Tools.ResultTruncation, m.cfg.Tools.ArgsTruncation,
							&m.cfg.Permissions, m.auditor, m.hookRunner)
						childLoop.SetMaxTokens(tools.SubAgentMaxTokens)
						childLoop.SetTemperature(m.cfg.Agent.Temperature)
						childLoop.SetContextWindow(m.cfg.Agent.ContextWindow)
						childLoop.SetEnableStreaming(false)
						childLoop.SetQuiet(true)
						childLoop.SetThinking(&client.ThinkingConfig{Type: "disabled"})
						childLoop.SetBypassPermissions(m.bypassPermissions)
						if handler != nil {
							childLoop.SetHandler(handler)
						}

						childAgentDir := filepath.Join(m.shannonDir, "agents", ag.Name)
						childMCPCtx := tools.ResolveMCPContext(m.cfg, ag)
						childLoop.SwitchAgent(ag.Prompt, childAgentDir, nil, childMCPCtx, ag.Skills)

						if ag.Config != nil && ag.Config.Agent != nil {
							ac := ag.Config.Agent
							if ac.Model != nil {
								childLoop.SetSpecificModel(*ac.Model)
							}
							if ac.ModelTier != nil {
								childLoop.SetModelTier(*ac.ModelTier)
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

						childCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
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
					sat.SetBaseRegistry(m.toolRegistry)
				}
			}
			m.agentLoop.SetSessionID("")
			m.agentLoop.SetWorkingSet(nil)
		}
		result, usage, err := m.agentLoop.Run(ctx, query, history)
		if result != "" && (err == nil || errors.Is(err, agent.ErrMaxIterReached)) {
			sess := m.sessions.Current()
			sess.Messages = append(sess.Messages, client.Message{Role: "assistant", Content: client.NewTextContent(result)})
			sess.MessageMeta = append(sess.MessageMeta, session.MessageMeta{Source: "local", Timestamp: session.TimePtr(time.Now())})
		}
		return agentDoneMsg{result: result, usage: usage, err: err}
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
		pm := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("252")).Render(">")
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
		pm := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("252")).Render(">")
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

const pasteThreshold = 10000

// truncatePaste stores full content and returns a truncated version with placeholder.
func (m *Model) truncatePaste(s string) string {
	if len(s) < pasteThreshold {
		return s
	}
	m.pasteNextID++
	id := m.pasteNextID
	m.pastedContents[id] = s

	runes := []rune(s)
	lines := strings.Count(s, "\n")
	head := string(runes[:500])
	tail := string(runes[len(runes)-500:])
	return head + fmt.Sprintf("[...Pasted text #%d +%d lines...]", id, lines) + tail
}

// expandPastedContent replaces paste placeholders with full content.
func expandPastedContent(input string, contents map[int]string) string {
	if len(contents) == 0 {
		return input
	}
	for id, full := range contents {
		prefix := fmt.Sprintf("[...Pasted text #%d ", id)
		start := strings.Index(input, prefix)
		if start == -1 {
			continue
		}
		end := strings.Index(input[start:], "]")
		if end == -1 {
			continue
		}
		placeholder := input[start : start+end+1]
		input = strings.Replace(input, placeholder, full, 1)
	}
	return input
}

// searchHistory finds the Nth match (0-indexed) in inputHistory containing query.
// Searches newest to oldest. Returns (match, matchIndex, found).
func (m *Model) searchHistory(query string, startIdx int) (string, int, bool) {
	if query == "" {
		return "", 0, false
	}
	lowerQuery := strings.ToLower(query)
	seen := 0
	for i := len(m.inputHistory) - 1; i >= 0; i-- {
		if strings.Contains(strings.ToLower(m.inputHistory[i]), lowerQuery) {
			if seen == startIdx {
				return m.inputHistory[i], seen, true
			}
			seen++
		}
	}
	return "", seen, false
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

// rerenderOutput re-renders all output blocks at the current width and reprints them.
// Used when the terminal is resized and when handing off from the startup
// animation to scrollback-backed output.
func (m *Model) rerenderOutput() tea.Cmd {
	width := m.width
	blocks := make([]outputBlock, len(m.output))
	copy(blocks, m.output)

	// Re-render blocks at new width (always, so future output uses new width).
	for i, b := range blocks {
		if b.rerender != nil {
			blocks[i].rendered = b.rerender(width)
			m.output[i].rendered = blocks[i].rendered
		} else if b.raw != "" {
			blocks[i].rendered = m.renderMarkdownCached(b.raw, width)
			m.output[i].rendered = blocks[i].rendered
		}
	}

	lines := make([]string, 0, len(blocks))
	for _, b := range blocks {
		lines = append(lines, b.rendered)
	}

	// The full repaint includes the latest output snapshot, so suppress any
	// queued incremental prints from this update to avoid duplicate lines.
	m.pendingPrints = m.pendingPrints[:0]

	return tea.Sequence(
		tea.ClearScreen,
		tea.Println(strings.Join(lines, "\n")),
	)
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
	return tea.Tick(50*time.Millisecond, func(t time.Time) tea.Msg {
		return spinnerFrameMsg{}
	})
}

func spinnerTick() tea.Cmd {
	return tea.Tick(5*time.Second, func(t time.Time) tea.Msg {
		return spinnerTickMsg{}
	})
}

func stallCheckTick() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg {
		return stallCheckMsg{}
	})
}

// renderWaveText renders text with a shimmer effect.
// Base color 76 (frog green) with a 3-char-wide highlight at
// 82 (bright lime) that sweeps across the text.
func renderWaveText(text string, tick int) string {
	runes := []rune(text)
	if len(runes) == 0 {
		return ""
	}
	waveCenter := tick % (len(runes) + 4)
	baseStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("76"))
	shimmerStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("82"))
	var sb strings.Builder
	for i, r := range runes {
		dist := waveCenter - i
		if dist < 0 {
			dist = -dist
		}
		if dist <= 1 {
			sb.WriteString(shimmerStyle.Render(string(r)))
		} else {
			sb.WriteString(baseStyle.Render(string(r)))
		}
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
	case "/clear", "/new":
		// Start fresh: new session + clear screen and scrollback (like CC's /clear)
		sess := m.sessions.NewSession()
		m.resumedSession = false
		m.applyRuntimeContext(sess)
		m.output = nil
		m.pendingPrints = m.pendingPrints[:0]
		m.textarea.Focus()
		return m, func() tea.Msg {
			// \033[3J clears terminal scrollback buffer, not just visible area
			os.Stdout.WriteString("\033[3J")
			return tea.ClearScreen()
		}
	case "/sessions":
		sessions, err := m.sessions.List()
		if err != nil {
			m.appendOutput(fmt.Sprintf("Error: %v", err))
		} else if len(sessions) == 0 {
			m.appendOutput("No saved sessions")
		} else {
			m.lastSessions = sessions
			m.sessionPickerIdx = 0
			m.state = stateSessionPicker
		}
	case "/session":
		if len(parts) > 1 {
			switch parts[1] {
			case "new":
				sess := m.sessions.NewSession()
				m.resumedSession = false
				m.applyRuntimeContext(sess)
				m.appendOutput("Started new session")
			case "resume":
				if len(parts) < 3 {
					m.appendOutput("Usage: /session resume <number or id>")
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
						m.applyRuntimeContext(sess)
						m.loadSessionHistory(sess)
					}
				}
			}
		}
	case "/model":
		if len(parts) > 1 {
			saveCfg := m.cfg
			if m.baseCfg != nil {
				m.baseCfg.ModelTier = parts[1]
				saveCfg = m.baseCfg
			}
			m.cfg.ModelTier = parts[1]
			m.agentLoop.SetModelTier(parts[1])
			if err := config.Save(saveCfg); err != nil {
				m.appendOutput(fmt.Sprintf("Model tier: %s (failed to save: %v)", parts[1], err))
			} else {
				m.appendOutput(fmt.Sprintf("Model tier: %s (saved)", parts[1]))
			}
		} else {
			m.appendOutput(fmt.Sprintf("Current model tier: %s", m.cfg.ModelTier))
		}
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
	case "/doctor":
		toolCount := 0
		if m.toolRegistry != nil {
			toolCount = m.toolRegistry.Len()
		}
		m.appendOutput(runDoctor(m.cfg, m.version, toolCount))
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

	m.textarea.Focus()
	return m, nil
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
	// Set title from query if still default
	sess := m.sessions.Current()
	if sess.Title == "New session" {
		sess.Title = session.Title(query)
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
			i+1, s.CreatedAt.Format("Jan 02"), s.Title, s.MsgCount))
	}
	m.appendOutput("  Use /session resume <number> to resume")
}

func helpText() string {
	return `Keys:
  Alt+Enter                      Insert newline (multi-line input)
  Enter                          Submit message

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
  /copy                          Copy last response to clipboard
  /clear                         Clear screen
  /quit                          Exit`
}

// tuiEventHandler bridges agent events to the TUI
type tuiEventHandler struct {
	model          *Model
	cloudStreaming bool // when true, OnStreamDelta forwards to TUI (for cloud_delegate)
}

func (h *tuiEventHandler) OnToolCall(name string, args string) {
	// Skip spinner/indicator for think tool — its content is shown dimmed on result.
	if name == "think" {
		return
	}
	if h.model.program != nil {
		// Pre-extract the display key from full args before any truncation.
		// subagent/cloud_delegate args contain long prompt fields that break
		// JSON parsing when truncated, causing toolKeyArg to fallback to raw JSON.
		h.model.program.Send(toolCallMsg{name: name, args: toolKeyArg(name, args)})
	}
}

func (h *tuiEventHandler) OnToolResult(name string, args string, result agent.ToolResult, elapsed time.Duration) {
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
	// Response display is handled by agentDoneMsg to avoid a race where the
	// Println Cmd from flushPrints arrives after agentDoneMsg has already
	// switched the view, causing the first line to render mid-screen.
	// The text is available as agentDoneMsg.result.
}

func (h *tuiEventHandler) OnStreamDelta(delta string) {
	// Suppressed for local LLM streaming (View shows thinking indicator, OnText renders final).
	// cloud_delegate sets cloudStreaming=true so its events render in real time.
	if h.cloudStreaming && delta != "" {
		h.model.sendOutput(delta)
	}
}

// SetCloudStreaming enables/disables delta forwarding for cloud_delegate events.
func (h *tuiEventHandler) SetCloudStreaming(enabled bool) {
	h.cloudStreaming = enabled
}

func (h *tuiEventHandler) OnUsage(usage agent.TurnUsage) {
	if h.model.program != nil {
		h.model.program.Send(tokenUpdateMsg{
			inputTokens:  usage.InputTokens,
			outputTokens: usage.OutputTokens,
		})
	}
}

func (h *tuiEventHandler) OnCloudAgent(agentID, status, message string) {
	prefixes := map[string]string{"started": ">", "completed": "+", "thinking": "~", "tool": "?"}
	p := prefixes[status]
	if p == "" {
		p = "-"
	}
	h.OnStreamDelta(fmt.Sprintf("  %s %s\n", p, message))

	// Bridge to swarm tree component
	if h.model.program != nil && agentID != "" {
		switch status {
		case "started":
			h.model.program.Send(swarmAgentStatusMsg{
				id: agentID, agentType: "agent",
				description: message, status: "started",
			})
		case "completed":
			h.model.program.Send(swarmAgentStatusMsg{
				id: agentID, status: "completed",
			})
		case "tool":
			h.model.program.Send(swarmAgentStatusMsg{
				id: agentID, status: "tool_call", toolName: message,
			})
		}
	}
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
	// Fast path: tool already always-allowed this session.
	if h.model.sessionAllowed[tool] {
		return true
	}
	// Send approval prompt to the TUI event loop, then block until user responds.
	// This runs inside a tea.Cmd goroutine so blocking is safe — it won't freeze the UI.
	if h.model.program != nil {
		h.model.program.Send(approvalRequestMsg{tool: tool, args: truncate(args, 200)})
		resp := <-h.model.approvalCh
		return resp.allowed
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
	{"/config", "Show configuration"},
	{"/setup", "Reconfigure endpoint & API key"},
	{"/sessions", "List saved sessions"},
	{"/search", "Search session history"},
	{"/session", "new | resume <n>"},
	{"/clear", "New session + clear screen"},
	{"/new", "New session + clear screen"},
	{"/update", "Check for updates"},
	{"/doctor", "System diagnostics"},
	{"/quit", "Exit"},
}

func (m *Model) updateMenu() {
	input := m.textarea.Value()
	if !strings.HasPrefix(input, "/") || strings.Contains(input, " ") {
		m.menuVisible = false
		m.menuItems = nil
		m.menuIndex = 0
		return
	}

	var matches []slashCmd
	for _, c := range m.slashCommands {
		if strings.HasPrefix(c.cmd, input) {
			matches = append(matches, c)
		}
	}
	m.menuItems = matches
	m.menuVisible = len(matches) > 0
	if m.menuIndex >= len(matches) {
		m.menuIndex = 0
	}
}

const dropListSize = 5

func (m *Model) renderMenu() string {
	return renderDropList(dropListSize, len(m.menuItems), m.menuIndex, func(i int) (string, string) {
		return m.menuItems[i].cmd, m.menuItems[i].desc
	})
}

// renderDropList renders a scrollable drop-down list with a fixed visible window.
// Always pads to maxVisible lines so the layout doesn't jump.
func renderDropList(maxVisible, total, selected int, item func(i int) (label, desc string)) string {
	if total == 0 {
		// Pad empty lines to keep layout stable
		return strings.Repeat("\n", maxVisible)
	}

	dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("243"))
	highlightLabel := lipgloss.NewStyle().Foreground(lipgloss.Color("111")).Bold(true)
	highlightDesc := lipgloss.NewStyle().Foreground(lipgloss.Color("146"))

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
		if i == selected {
			sb.WriteString(fmt.Sprintf("  > %s  %s\n",
				highlightLabel.Render(fmt.Sprintf("%-14s", label)),
				highlightDesc.Render(desc)))
		} else {
			sb.WriteString(fmt.Sprintf("    %s  %s\n",
				dimStyle.Render(fmt.Sprintf("%-14s", label)),
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
	sb.WriteString("Shannon CLI Configuration\n")

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
	sb.WriteString(fmt.Sprintf("  max_tokens: %d %s\n", cfg.Agent.MaxTokens, srcLabel("agent.max_tokens")))
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
	sb.WriteString(fmt.Sprintf("  grep_max_results: %d %s\n", cfg.Tools.GrepMaxResults, srcLabel("tools.grep_max_results")))

	return sb.String()
}

func truncate(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "..."
}
