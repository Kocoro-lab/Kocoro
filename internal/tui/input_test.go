package tui

import (
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/config"
	"github.com/Kocoro-lab/ShanClaw/internal/tools"
)

func newInputTestModel() *Model {
	ta := textarea.New()
	ta.SetWidth(80)
	ta.SetHeight(1)
	ta.Focus()
	return &Model{
		state:         stateInput,
		textarea:      ta,
		width:         80,
		height:        24,
		cfg:           &config.Config{ModelTier: "medium"},
		markdownCache: make(map[string]string),
		spinnerTexts:  []string{"thinking"},
	}
}

func TestCtrlK_DeleteToEnd(t *testing.T) {
	m := newInputTestModel()
	m.textarea.SetValue("hello world")
	m.textarea.SetCursor(5)
	m.update(tea.KeyMsg{Type: tea.KeyCtrlK})
	if got := m.textarea.Value(); got != "hello" {
		t.Errorf("Ctrl+K: got %q, want %q", got, "hello")
	}
}

func TestCtrlU_DeleteToStart(t *testing.T) {
	m := newInputTestModel()
	m.textarea.SetValue("hello world")
	m.textarea.SetCursor(5)
	m.update(tea.KeyMsg{Type: tea.KeyCtrlU})
	if got := m.textarea.Value(); got != " world" {
		t.Errorf("Ctrl+U: got %q, want %q", got, " world")
	}
}

func TestCtrlW_DeleteWord(t *testing.T) {
	m := newInputTestModel()
	m.textarea.SetValue("hello world")
	m.textarea.SetCursor(11) // at end
	m.update(tea.KeyMsg{Type: tea.KeyCtrlW})
	if got := m.textarea.Value(); got != "hello " {
		t.Errorf("Ctrl+W: got %q, want %q", got, "hello ")
	}
}

func TestInputHistory_UpDown(t *testing.T) {
	m := newInputTestModel()
	m.inputHistory = []string{"first", "second", "third"}
	m.historyIdx = -1

	// Up → most recent ("third")
	m.update(tea.KeyMsg{Type: tea.KeyUp})
	if got := m.textarea.Value(); got != "third" {
		t.Errorf("Up 1: got %q, want %q", got, "third")
	}

	// Up again → "second"
	m.update(tea.KeyMsg{Type: tea.KeyUp})
	if got := m.textarea.Value(); got != "second" {
		t.Errorf("Up 2: got %q, want %q", got, "second")
	}

	// Down → back to "third"
	m.update(tea.KeyMsg{Type: tea.KeyDown})
	if got := m.textarea.Value(); got != "third" {
		t.Errorf("Down: got %q, want %q", got, "third")
	}

	// Down again → empty (back to current input)
	m.update(tea.KeyMsg{Type: tea.KeyDown})
	if got := m.textarea.Value(); got != "" {
		t.Errorf("Down past end: got %q, want %q", got, "")
	}
}

func TestInputHistory_UpAtTop(t *testing.T) {
	m := newInputTestModel()
	m.inputHistory = []string{"only"}
	m.historyIdx = -1

	m.update(tea.KeyMsg{Type: tea.KeyUp})
	if got := m.textarea.Value(); got != "only" {
		t.Errorf("Up: got %q, want %q", got, "only")
	}

	// Up again — should stay at "only", not panic
	m.update(tea.KeyMsg{Type: tea.KeyUp})
	if got := m.textarea.Value(); got != "only" {
		t.Errorf("Up at top: got %q, want %q", got, "only")
	}
}

func TestEscapeDoubleTap_ClearsInput(t *testing.T) {
	m := newInputTestModel()
	m.textarea.SetValue("some text")

	// First Esc — should NOT clear (just record time)
	m.update(tea.KeyMsg{Type: tea.KeyEscape})
	if got := m.textarea.Value(); got != "some text" {
		t.Errorf("First Esc: got %q, want %q", got, "some text")
	}

	// Simulate second Esc within 800ms by setting lastEscTime to recent past
	m.lastEscTime = time.Now()
	m.update(tea.KeyMsg{Type: tea.KeyEscape})
	if got := m.textarea.Value(); got != "" {
		t.Errorf("Double Esc: got %q, want %q", got, "")
	}
}

func TestAlwaysAllow_PersistsInSession(t *testing.T) {
	m := newInputTestModel()
	m.state = stateApproval
	m.approvalCh = make(chan bool, 1)
	m.sessionAllowed = make(map[string]bool)
	m.pendingApprovalTool = "bash"

	// Press 'a' to always-allow
	m.update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})

	// Check approval was granted
	select {
	case v := <-m.approvalCh:
		if !v {
			t.Error("expected approval to be granted")
		}
	default:
		t.Error("expected value on approvalCh")
	}

	// Check tool is remembered
	if !m.sessionAllowed["bash"] {
		t.Error("expected bash to be session-allowed")
	}
}

// newIntegrationApprovalRegistry returns a registry carrying one
// approval-required integration tool (a Cloud-marked outbound write).
func newIntegrationApprovalRegistry() *agent.ToolRegistry {
	reg := agent.NewToolRegistry()
	reg.Register(tools.NewIntegrationTool(client.ServerToolSchema{
		Name: "gmail_send_email", RequiresApproval: true,
	}, nil))
	return reg
}

// Pressing 'a' on an approval-required integration write tool grants the
// current call only — the session-allow map must not remember it, mirroring
// the daemon broker's persistence refusal.
func TestAlwaysAllow_IntegrationRequiresApprovalNotSessionPersisted(t *testing.T) {
	m := newInputTestModel()
	m.toolRegistry = newIntegrationApprovalRegistry()
	m.state = stateApproval
	m.approvalCh = make(chan bool, 1)
	m.sessionAllowed = make(map[string]bool)
	m.pendingApprovalTool = "gmail_send_email"

	m.update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})

	select {
	case v := <-m.approvalCh:
		if !v {
			t.Error("expected the current call to be approved once")
		}
	default:
		t.Error("expected value on approvalCh")
	}
	if m.sessionAllowed["gmail_send_email"] {
		t.Error("session-allowed a persistence-denied integration tool")
	}
}

// A stale sessionAllowed entry (e.g. minted before the tool's schema was
// known) must not silence the prompt for an approval-required integration
// tool — the request still surfaces the approval UI.
func TestApprovalRequest_IgnoresSessionAllowForIntegrationRequiresApproval(t *testing.T) {
	m := newInputTestModel()
	m.toolRegistry = newIntegrationApprovalRegistry()
	m.approvalCh = make(chan bool, 1)
	m.sessionAllowed = map[string]bool{"gmail_send_email": true}

	m.update(approvalRequestMsg{tool: "gmail_send_email", args: "{}"})

	select {
	case <-m.approvalCh:
		t.Error("stale session-allow entry auto-approved a persistence-denied integration tool")
	default:
	}
	if m.state != stateApproval {
		t.Errorf("state = %v, want stateApproval (prompt shown)", m.state)
	}
}
