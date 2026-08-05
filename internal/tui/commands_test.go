package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/textarea"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/config"
	"github.com/Kocoro-lab/ShanClaw/internal/session"
)

func newCommandTestModel(t *testing.T) *Model {
	t.Helper()
	sessDir := t.TempDir()
	sessMgr := session.NewManager(sessDir)
	sessMgr.NewSession()

	ta := textarea.New()
	ta.SetWidth(80)
	ta.SetHeight(1)
	ta.Focus()

	return &Model{
		state:          stateInput,
		textarea:       ta,
		sessions:       sessMgr,
		width:          80,
		height:         24,
		cfg:            &config.Config{ModelTier: "medium", Endpoint: "http://test", Sources: make(map[string]config.ConfigSource)},
		baseCfg:        &config.Config{ModelTier: "medium", Endpoint: "http://test", Sources: make(map[string]config.ConfigSource)},
		markdownCache:  make(map[string]string),
		spinnerTexts:   []string{"thinking"},
		shannonDir:     t.TempDir(),
		sessionAllowed: make(map[string]bool),
		customCommands: make(map[string]string),
		slashCommands:  baseSlashCommands,
		approvalCh:     make(chan bool, 1),
	}
}

func TestClear_CreatesNewSession(t *testing.T) {
	m := newCommandTestModel(t)
	oldID := m.sessions.Current().ID
	m.appendOutput("some output")

	m.handleSlashCommand("/clear")

	newID := m.sessions.Current().ID
	if newID == oldID {
		t.Error("expected /clear to create a new session")
	}
	if len(m.output) != 0 {
		t.Errorf("expected output cleared, got %d blocks", len(m.output))
	}
}

func TestPermissions_ShowsList(t *testing.T) {
	m := newCommandTestModel(t)
	m.cfg.Permissions.AllowedCommands = []string{"git *", "npm test"}
	m.cfg.Permissions.DeniedCommands = []string{"rm -rf *"}

	m.handleSlashCommand("/permissions")

	combined := ""
	for _, b := range m.output {
		combined += b.rendered + "\n"
	}
	if !strings.Contains(combined, "git *") {
		t.Error("expected allowed commands in output")
	}
	if !strings.Contains(combined, "rm -rf *") {
		t.Error("expected denied commands in output")
	}
}

func TestPermissions_Allow(t *testing.T) {
	m := newCommandTestModel(t)

	m.handleSlashCommand("/permissions allow docker *")

	found := false
	for _, c := range m.cfg.Permissions.AllowedCommands {
		if c == "docker *" {
			found = true
		}
	}
	if !found {
		t.Error("expected 'docker *' in allowed commands")
	}
}

func TestPermissions_Deny(t *testing.T) {
	m := newCommandTestModel(t)

	m.handleSlashCommand("/permissions deny curl *")

	found := false
	for _, c := range m.cfg.Permissions.DeniedCommands {
		if c == "curl *" {
			found = true
		}
	}
	if !found {
		t.Error("expected 'curl *' in denied commands")
	}
}

func TestPermissions_Remove(t *testing.T) {
	m := newCommandTestModel(t)
	m.cfg.Permissions.AllowedCommands = []string{"git *", "npm test"}

	m.handleSlashCommand("/permissions remove git *")

	for _, c := range m.cfg.Permissions.AllowedCommands {
		if c == "git *" {
			t.Error("expected 'git *' removed from allowed commands")
		}
	}
}

func TestStatus_ShowsInfo(t *testing.T) {
	m := newCommandTestModel(t)
	m.version = "v0.1.42"
	sess := m.sessions.Current()
	sess.Messages = []client.Message{
		{Role: "user", Content: client.NewTextContent("one")},
		{Role: "assistant", Content: client.NewTextContent("two")},
		{Role: "user", Content: client.NewTextContent("three")},
		{Role: "assistant", Content: client.NewTextContent("four")},
	}
	sess.CompactionCheckpoint = &session.CompactionCheckpoint{
		SchemaVersion:       session.CompactionCheckpointSchemaVersion,
		ArchiveThroughIndex: len(sess.Messages),
		Messages: []client.Message{
			{Role: "user", Content: client.NewTextContent("primer")},
			{Role: "assistant", Content: client.NewTextContent("summary")},
		},
	}

	m.handleSlashCommand("/status")

	if len(m.output) == 0 {
		t.Fatal("expected /status to produce output")
	}
	combined := ""
	for _, b := range m.output {
		combined += b.rendered + "\n"
	}
	for _, want := range []string{"v0.1.42", "medium", "http://test", "4 archived, 2 live messages"} {
		if !strings.Contains(combined, want) {
			t.Errorf("expected output to contain %q", want)
		}
	}
}
