package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
)

func newTestModel() *Model {
	ta := textarea.New()
	ta.SetWidth(80)
	ta.SetHeight(1)
	ta.ShowLineNumbers = false
	ta.CharLimit = 0
	ta.Focus()
	m := &Model{
		state:         stateInput,
		textarea:      ta,
		historyIdx:    -1,
		markdownCache: make(map[string]string),
	}
	return m
}

func sendKey(m *Model, key string) (*Model, tea.Cmd) {
	msg := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)}
	switch key {
	case "ctrl+k":
		msg = tea.KeyMsg{Type: tea.KeyCtrlK}
	case "ctrl+u":
		msg = tea.KeyMsg{Type: tea.KeyCtrlU}
	case "ctrl+w":
		msg = tea.KeyMsg{Type: tea.KeyCtrlW}
	case "ctrl+l":
		msg = tea.KeyMsg{Type: tea.KeyCtrlL}
	}
	result, cmd := m.update(msg)
	return result.(*Model), cmd
}

func TestCtrlK_DeletesToEnd(t *testing.T) {
	m := newTestModel()
	m.textarea.SetValue("hello world")
	// Move cursor to position 5 (after "hello")
	m.textarea.SetCursor(5)

	m2, _ := sendKey(m, "ctrl+k")

	got := m2.textarea.Value()
	if got != "hello" {
		t.Errorf("ctrl+k: expected %q, got %q", "hello", got)
	}
}

func TestCtrlK_AtEnd_NoChange(t *testing.T) {
	m := newTestModel()
	m.textarea.SetValue("hello")
	m.textarea.CursorEnd()

	m2, _ := sendKey(m, "ctrl+k")

	got := m2.textarea.Value()
	if got != "hello" {
		t.Errorf("ctrl+k at end: expected %q, got %q", "hello", got)
	}
}

func TestCtrlU_DeletesToStart(t *testing.T) {
	m := newTestModel()
	m.textarea.SetValue("hello world")
	m.textarea.SetCursor(5)

	m2, _ := sendKey(m, "ctrl+u")

	got := m2.textarea.Value()
	if got != " world" {
		t.Errorf("ctrl+u: expected %q, got %q", " world", got)
	}
}

func TestCtrlU_AtStart_NoChange(t *testing.T) {
	m := newTestModel()
	m.textarea.SetValue("hello")
	m.textarea.CursorStart()

	m2, _ := sendKey(m, "ctrl+u")

	got := m2.textarea.Value()
	if got != "hello" {
		t.Errorf("ctrl+u at start: expected %q, got %q", "hello", got)
	}
}

func TestCtrlW_DeletesPreviousWord(t *testing.T) {
	m := newTestModel()
	m.textarea.SetValue("hello world")
	m.textarea.CursorEnd() // cursor at 11

	m2, _ := sendKey(m, "ctrl+w")

	got := m2.textarea.Value()
	// Should delete "world" leaving "hello "
	if got != "hello " {
		t.Errorf("ctrl+w: expected %q, got %q", "hello ", got)
	}
}

func TestCtrlW_AtStart_NoChange(t *testing.T) {
	m := newTestModel()
	m.textarea.SetValue("hello")
	m.textarea.CursorStart()

	m2, _ := sendKey(m, "ctrl+w")

	got := m2.textarea.Value()
	if got != "hello" {
		t.Errorf("ctrl+w at start: expected %q, got %q", "hello", got)
	}
}

func TestCtrlW_DeletesTrailingSpaceThenWord(t *testing.T) {
	m := newTestModel()
	m.textarea.SetValue("foo bar  ")
	m.textarea.CursorEnd() // cursor at 9

	m2, _ := sendKey(m, "ctrl+w")

	got := m2.textarea.Value()
	// Skips trailing spaces then deletes "bar"
	if got != "foo " {
		t.Errorf("ctrl+w trailing spaces: expected %q, got %q", "foo ", got)
	}
}

func TestCtrlL_ClearsOutput(t *testing.T) {
	m := newTestModel()
	m.output = []outputBlock{{raw: "some output", rendered: "some output"}}
	m.pendingPrints = []string{"pending"}

	m2, cmd := sendKey(m, "ctrl+l")

	if len(m2.output) != 0 {
		t.Errorf("ctrl+l: expected empty output, got %d items", len(m2.output))
	}
	if len(m2.pendingPrints) != 0 {
		t.Errorf("ctrl+l: expected empty pendingPrints, got %d items", len(m2.pendingPrints))
	}
	if cmd == nil {
		t.Error("ctrl+l: expected non-nil cmd (tea.ClearScreen)")
	}
}

func TestTab_EmptyInput_OpensMenu(t *testing.T) {
	m := newTestModel()
	m.slashCommands = []slashCmd{{"/help", "Show help"}, {"/clear", "Clear screen"}}
	m.textarea.SetValue("")

	msg := tea.KeyMsg{Type: tea.KeyTab}
	result, _ := m.update(msg)
	rm := result.(*Model)

	if rm.textarea.Value() != "/" {
		t.Errorf("Tab on empty input: got %q, want %q", rm.textarea.Value(), "/")
	}
	if !rm.menuVisible {
		t.Error("Tab on empty input should open menu")
	}
}

func TestTab_NonEmptyInput_NoOp(t *testing.T) {
	m := newTestModel()
	m.textarea.SetValue("hello")

	msg := tea.KeyMsg{Type: tea.KeyTab}
	result, _ := m.update(msg)
	rm := result.(*Model)

	// Should not change value (falls through to textarea)
	if rm.textarea.Value() == "/" {
		t.Error("Tab on non-empty input should not insert /")
	}
}

func TestReadlineShortcuts_NotInOtherStates(t *testing.T) {
	// In stateProcessing, ctrl+k should NOT be handled by our readline block
	m := newTestModel()
	m.state = stateProcessing
	m.textarea.SetValue("hello world")
	m.textarea.SetCursor(5)

	// update() should not panic; the readline block is guarded by stateInput
	// (the key might be forwarded to textarea or ignored)
	// Just verify it doesn't crash
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("unexpected panic in non-stateInput: %v", r)
		}
	}()
	m.update(tea.KeyMsg{Type: tea.KeyCtrlK})
}

func TestEscDoublePress_ClearsInput(t *testing.T) {
	m := newTestModel()
	m.textarea.SetValue("some input")

	msg := tea.KeyMsg{Type: tea.KeyEscape}
	result, _ := m.update(msg)
	rm := result.(*Model)
	if !rm.escPending {
		t.Fatal("first Esc should set escPending")
	}
	if rm.textarea.Value() != "some input" {
		t.Fatal("first Esc should not clear input")
	}

	result2, _ := rm.update(msg)
	rm2 := result2.(*Model)
	if rm2.textarea.Value() != "" {
		t.Errorf("double Esc: got %q, want empty", rm2.textarea.Value())
	}
	if rm2.escPending {
		t.Error("escPending should be cleared after double press")
	}
}

func TestEscDoublePress_EmptyInputNoOp(t *testing.T) {
	m := newTestModel()
	m.textarea.SetValue("")

	msg := tea.KeyMsg{Type: tea.KeyEscape}
	result, _ := m.update(msg)
	rm := result.(*Model)
	if rm.escPending {
		t.Error("Esc on empty input should not set escPending")
	}
}

func TestEscDoublePress_SavesHistory(t *testing.T) {
	m := newTestModel()
	m.textarea.SetValue("cleared text")

	msg := tea.KeyMsg{Type: tea.KeyEscape}
	result, _ := m.update(msg)
	result2, _ := result.(*Model).update(msg)
	rm := result2.(*Model)

	if len(rm.inputHistory) != 1 || rm.inputHistory[0] != "cleared text" {
		t.Errorf("double Esc should save input to history, got %v", rm.inputHistory)
	}
}

func TestCtrlR_EntersSearchMode(t *testing.T) {
	m := newTestModel()
	m.inputHistory = []string{"hello world", "foo bar"}
	m.textarea.SetValue("current")

	msg := tea.KeyMsg{Type: tea.KeyCtrlR}
	result, _ := m.update(msg)
	rm := result.(*Model)

	if rm.state != stateHistorySearch {
		t.Fatalf("state: got %d, want stateHistorySearch", rm.state)
	}
	if rm.searchDraft != "current" {
		t.Errorf("searchDraft: got %q, want %q", rm.searchDraft, "current")
	}
}

func TestCtrlR_NoHistory_StaysInInput(t *testing.T) {
	m := newTestModel()
	m.inputHistory = nil

	msg := tea.KeyMsg{Type: tea.KeyCtrlR}
	result, _ := m.update(msg)
	rm := result.(*Model)

	if rm.state != stateInput {
		t.Fatalf("should stay in stateInput when no history")
	}
}

func TestHistorySearch_FindsMatch(t *testing.T) {
	m := newTestModel()
	m.state = stateHistorySearch
	m.inputHistory = []string{"alpha", "beta hello", "gamma hello world"}
	m.searchQuery = ""
	m.searchResult = ""

	for _, ch := range "hello" {
		msg := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{ch}}
		m.update(msg)
	}

	if m.searchResult != "gamma hello world" {
		t.Errorf("search 'hello': got %q, want %q", m.searchResult, "gamma hello world")
	}
}

func TestHistorySearch_CtrlR_NextMatch(t *testing.T) {
	m := newTestModel()
	m.state = stateHistorySearch
	m.inputHistory = []string{"hello one", "hello two", "hello three"}
	m.searchQuery = "hello"
	m.searchMatchIdx = 0
	m.searchResult = "hello three"

	msg := tea.KeyMsg{Type: tea.KeyCtrlR}
	m.update(msg)

	if m.searchResult != "hello two" {
		t.Errorf("next match: got %q, want %q", m.searchResult, "hello two")
	}
}

func TestHistorySearch_Enter_AcceptsMatch(t *testing.T) {
	m := newTestModel()
	m.state = stateHistorySearch
	m.searchResult = "accepted entry"

	msg := tea.KeyMsg{Type: tea.KeyEnter}
	result, _ := m.update(msg)
	rm := result.(*Model)

	if rm.state != stateInput {
		t.Fatalf("Enter should return to stateInput")
	}
	if rm.textarea.Value() != "accepted entry" {
		t.Errorf("Enter: got %q, want %q", rm.textarea.Value(), "accepted entry")
	}
}

func TestHistorySearch_Escape_Cancels(t *testing.T) {
	m := newTestModel()
	m.state = stateHistorySearch
	m.searchDraft = "original"
	m.searchResult = "some match"

	msg := tea.KeyMsg{Type: tea.KeyEscape}
	result, _ := m.update(msg)
	rm := result.(*Model)

	if rm.state != stateInput {
		t.Fatalf("Esc should return to stateInput")
	}
	if rm.textarea.Value() != "original" {
		t.Errorf("Esc: got %q, want %q", rm.textarea.Value(), "original")
	}
}

func TestHistorySearch_BackspaceEmpty_Cancels(t *testing.T) {
	m := newTestModel()
	m.state = stateHistorySearch
	m.searchQuery = ""
	m.searchDraft = "draft"

	msg := tea.KeyMsg{Type: tea.KeyBackspace}
	result, _ := m.update(msg)
	rm := result.(*Model)

	if rm.state != stateInput {
		t.Fatal("Backspace on empty query should cancel search")
	}
	if rm.textarea.Value() != "draft" {
		t.Errorf("got %q, want %q", rm.textarea.Value(), "draft")
	}
}

func TestSearchHistory_CaseInsensitive(t *testing.T) {
	m := newTestModel()
	m.inputHistory = []string{"Hello World", "goodbye"}

	result, _, found := m.searchHistory("hello", 0)
	if !found || result != "Hello World" {
		t.Errorf("case-insensitive: got %q found=%v", result, found)
	}
}

func TestApproval_YKey(t *testing.T) {
	m := newTestModel()
	m.state = stateApproval
	m.approvalCh = make(chan approvalResponse, 1)
	m.pendingApprovalTool = "bash"

	msg := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}}
	result, _ := m.update(msg)
	rm := result.(*Model)

	if rm.state != stateProcessing {
		t.Fatalf("state: got %d, want stateProcessing", rm.state)
	}
	resp := <-rm.approvalCh
	if !resp.allowed || resp.always {
		t.Errorf("y should be allowed=true, always=false, got %+v", resp)
	}
}

func TestApproval_AlwaysAllow(t *testing.T) {
	m := newTestModel()
	m.state = stateApproval
	m.approvalCh = make(chan approvalResponse, 1)
	m.sessionAllowed = make(map[string]bool)
	m.pendingApprovalTool = "bash"

	msg := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}}
	result, _ := m.update(msg)
	rm := result.(*Model)

	if rm.state != stateProcessing {
		t.Fatalf("state: got %d, want stateProcessing", rm.state)
	}
	resp := <-rm.approvalCh
	if !resp.allowed || !resp.always {
		t.Errorf("a should be allowed+always, got %+v", resp)
	}
	if !rm.sessionAllowed["bash"] {
		t.Error("bash should be in sessionAllowed")
	}
}

func TestApproval_NKey(t *testing.T) {
	m := newTestModel()
	m.state = stateApproval
	m.approvalCh = make(chan approvalResponse, 1)
	m.pendingApprovalTool = "bash"

	msg := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}}
	result, _ := m.update(msg)
	rm := result.(*Model)

	resp := <-rm.approvalCh
	if resp.allowed {
		t.Error("n should be allowed=false")
	}
}

func TestTruncatePaste_AboveThreshold(t *testing.T) {
	m := newTestModel()
	m.pastedContents = make(map[int]string)

	long := strings.Repeat("line of text\n", 1200) // ~15K chars
	truncated := m.truncatePaste(long)

	if len(truncated) >= len(long) {
		t.Error("should shorten long input")
	}
	if !strings.Contains(truncated, "[...Pasted text #") {
		t.Error("should contain placeholder")
	}
	if len(m.pastedContents) != 1 {
		t.Errorf("pastedContents: got %d, want 1", len(m.pastedContents))
	}
}

func TestTruncatePaste_BelowThreshold(t *testing.T) {
	m := newTestModel()
	m.pastedContents = make(map[int]string)

	short := strings.Repeat("x", 5000)
	result := m.truncatePaste(short)
	if result != short {
		t.Error("below threshold should not truncate")
	}
}

func TestExpandPastedContent(t *testing.T) {
	contents := map[int]string{
		1: "full content here",
	}
	input := "before [...Pasted text #1 +5 lines...] after"
	got := expandPastedContent(input, contents)
	want := "before full content here after"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestExpandPastedContent_NoPlaceholders(t *testing.T) {
	contents := map[int]string{}
	input := "normal text"
	got := expandPastedContent(input, contents)
	if got != input {
		t.Error("no placeholders should return input unchanged")
	}
}
