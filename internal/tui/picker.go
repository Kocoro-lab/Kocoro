package tui

import (
	"fmt"
	"path/filepath"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/Kocoro-lab/ShanClaw/internal/agents"
	"github.com/Kocoro-lab/ShanClaw/internal/config"
	"github.com/Kocoro-lab/ShanClaw/internal/session"
)

// pickerOption is one selectable row in the generic statePicker dropdown.
type pickerOption struct {
	label string
	desc  string
	value string
}

// pickerKind discriminates which command opened the picker so Enter dispatches
// to the right apply logic. Add a kind per selection command.
type pickerKind int

const (
	pickerKindModel pickerKind = iota
	pickerKindAgent
)

// modelTierOptions lists the routing tiers offered by the /model picker.
// config.go validates exactly these three.
func modelTierOptions() []pickerOption {
	return []pickerOption{
		{label: "small", desc: "fastest, lowest cost", value: "small"},
		{label: "medium", desc: "balanced — the default", value: "medium"},
		{label: "large", desc: "most capable", value: "large"},
	}
}

// pickerWrap wraps a picker index around a list of length n (and is safe when
// the list is empty).
func pickerWrap(idx, n int) int {
	if n == 0 {
		return 0
	}
	if idx < 0 {
		return n - 1
	}
	if idx >= n {
		return 0
	}
	return idx
}

// openModelPicker enters the interactive model-tier picker, pre-selecting the
// current tier. Invoked by bare `/model` (typing the value is for power users
// via `/model <tier>`).
func (m *Model) openModelPicker() {
	opts := modelTierOptions()
	m.pickerTitle = "Model tier"
	m.pickerOpts = opts
	m.pickerKind = pickerKindModel
	m.pickerIdx = 0
	for i, o := range opts {
		if o.value == m.cfg.ModelTier {
			m.pickerIdx = i
		}
	}
	m.menuVisible = false // drop any stale slash-menu before switching state
	m.state = statePicker
}

// applyModelTier sets the gateway model tier live (and persists it), mirroring
// the previous `/model <tier>` behavior. Shared by the picker and the direct
// command form.
func (m *Model) applyModelTier(tier string) {
	saveCfg := m.cfg
	if m.baseCfg != nil {
		m.baseCfg.ModelTier = tier
		saveCfg = m.baseCfg
	}
	m.cfg.ModelTier = tier
	m.agentLoop.SetModelTier(tier)
	if err := config.Save(saveCfg); err != nil {
		m.appendOutput(fmt.Sprintf("Model tier: %s (failed to save: %v)", tier, err))
	} else {
		m.appendOutput(fmt.Sprintf("Model tier: %s (saved)", tier))
	}
}

// agentLabel is the display name of the active agent ("default" when none is
// overridden). Surfaced in the status line and switch confirmations.
func (m *Model) agentLabel() string {
	if m.agentOverride != nil {
		return m.agentOverride.Name
	}
	return "default"
}

// agentPickerOptions builds the /agent picker rows: the default (built-in)
// agent first, then each named agent. The option value is the agent name
// ("" = default).
func agentPickerOptions(entries []agents.AgentEntry) []pickerOption {
	opts := []pickerOption{{label: "default", desc: "the built-in assistant", value: ""}}
	for _, e := range entries {
		label := e.Name
		if e.DisplayName != "" {
			label = e.DisplayName
		}
		desc := "agent"
		if e.Builtin {
			desc = "built-in agent"
		}
		opts = append(opts, pickerOption{label: label, desc: desc, value: e.Name})
	}
	return opts
}

// openSessionPicker enters the session picker (stateSessionPicker). Shared by
// `/sessions`, bare `/session`, and `/session resume` (no id) so resuming is a
// selection, not a typed id.
func (m *Model) openSessionPicker() {
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
	m.sessionPickerIdx = 0
	m.menuVisible = false
	m.state = stateSessionPicker
}

// openAgentPicker enters the interactive agent picker, pre-selecting the active
// agent. Invoked by bare `/agent`.
func (m *Model) openAgentPicker() {
	entries, _ := agents.ListAgents(filepath.Join(m.shannonDir, "agents"))
	opts := agentPickerOptions(entries)
	current := ""
	if m.agentOverride != nil {
		current = m.agentOverride.Name
	}
	m.pickerTitle = "Agent"
	m.pickerOpts = opts
	m.pickerKind = pickerKindAgent
	m.pickerIdx = 0
	for i, o := range opts {
		if o.value == current {
			m.pickerIdx = i
		}
	}
	m.menuVisible = false
	m.state = statePicker
}

// agentExists reports whether name is a switchable agent. "" and "default"
// both mean the built-in default (always valid); named agents must exist on
// disk. Used to fall back to the picker on a typed-but-unknown name.
func (m *Model) agentExists(name string) bool {
	if name == "" || name == "default" {
		return true
	}
	entries, _ := agents.ListAgents(filepath.Join(m.shannonDir, "agents"))
	for _, e := range entries {
		if e.Name == name {
			return true
		}
	}
	return false
}

// switchToAgent switches the live agent (loop, skills, per-agent session
// directory) and starts a fresh conversation — mirroring Desktop, where
// switching an agent stops the current chat and begins a new one. name == ""
// (or "default") selects the built-in default agent.
func (m *Model) switchToAgent(name string) tea.Cmd {
	if name == "default" {
		name = "" // the built-in default agent has no named directory
	}
	current := ""
	if m.agentOverride != nil {
		current = m.agentOverride.Name
	}
	if name == current {
		m.appendOutput("  Already on agent: " + m.agentLabel())
		return m.flushPrints()
	}

	var override *agents.Agent
	if name != "" {
		a, err := agents.LoadAgent(filepath.Join(m.shannonDir, "agents"), name)
		if err != nil {
			m.appendOutput(fmt.Sprintf("  Error loading agent %q: %v", name, err))
			return m.flushPrints()
		}
		override = a
	}
	m.agentOverride = override

	// Skills: agent-scoped or the global set for the default agent.
	if override != nil {
		m.loadedSkills = override.Skills
	} else if sk, err := agents.LoadGlobalSkills(m.shannonDir); err == nil {
		m.loadedSkills = sk
	}
	if m.skillsPtr != nil {
		*m.skillsPtr = m.loadedSkills
	}

	// Named agents are multi-session under their own directory; the default
	// agent uses the top-level sessions dir.
	sessDir := filepath.Join(m.shannonDir, "sessions")
	if override != nil {
		sessDir = filepath.Join(m.shannonDir, "agents", override.Name, "sessions")
	}
	m.sessions = session.NewManager(sessDir)

	m.rebuildAgentLoop()

	// Fresh conversation state, but APPEND the notice to scrollback rather than
	// ClearScreen+re-print: ClearScreen (\x1b[2J) does not clear the terminal's
	// saved-lines, so rerenderOutput would leave the old startup header in
	// history and stack a duplicate. flushPrints just emits the notice; the
	// prior conversation stays as immutable scrollback above it.
	m.output = nil
	sess := m.sessions.NewSession()
	m.resumedSession = false
	m.sessionAllowed = make(map[string]bool)
	m.applyRuntimeContext(sess)
	m.appendOutput("  Switched to agent: " + m.agentLabel())
	return m.flushPrints()
}
