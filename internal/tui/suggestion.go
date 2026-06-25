package tui

import (
	"context"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/client"
	ctxwin "github.com/Kocoro-lab/ShanClaw/internal/context"
)

// suggestionReadyMsg carries the generated follow-up suggestion back to the
// Update loop. gen is the suggestion generation it belongs to; a newer turn
// bumps m.suggestionGen and stales this message.
type suggestionReadyMsg struct {
	gen  int
	text string
}

// clearSuggestion drops any shown or in-flight ghost-text suggestion. Bumping
// the generation stales an in-flight maybeSuggestCmd result so a suggestion
// from a now-abandoned conversation can't pop in later.
func (m *Model) clearSuggestion() {
	m.promptSuggestion = ""
	m.suggestionGen++
}

// maybeSuggestCmd returns a Cmd that generates a single follow-up suggestion
// after a completed turn (Desktop's prompt-suggestion logic, replicated at the
// TUI level since the loop only exposes the request/usage snapshots). Returns
// nil when gated out — tea.Batch drops a nil Cmd. The generation is ONE extra
// forked LLM call, kept cheap by SkipCacheWrite + a warm byte-equal prefix and
// gated by MinTurns / cache-cold threshold.
func (m *Model) maybeSuggestCmd(msg agentDoneMsg, gen int) tea.Cmd {
	if !m.cfg.Agent.PromptSuggestion.Enabled || msg.err != nil || strings.TrimSpace(msg.result) == "" {
		return nil
	}
	sess := m.sessions.Current()
	if sess == nil {
		return nil
	}
	uncached := 0
	if u, ok := m.agentLoop.LastLLMUsage(); ok {
		uncached = u.InputTokens - u.CacheReadTokens
	}
	if !agent.ShouldGenerateSuggestion(agent.ShouldGenerateArgs{
		Enabled:                  m.cfg.Agent.PromptSuggestion.Enabled,
		CompletedTurns:           ctxwin.CountCompletedTurns(sess.Messages),
		MinTurns:                 m.cfg.Agent.PromptSuggestion.MinTurns,
		LastTurnUncachedTokens:   uncached,
		CacheColdThresholdTokens: m.cfg.Agent.PromptSuggestion.CacheColdThresholdTokens,
		LastTurnHadError:         false,
		PlanMode:                 false,
	}) {
		return nil
	}
	main, ok := m.agentLoop.LastSentRequest()
	if !ok {
		return nil
	}
	result := msg.result
	llm := m.llmClient
	return func() tea.Msg {
		// Append the assistant reply so the fork sees the full turn — the daemon
		// does this OUTSIDE GenerateSuggestionWithUsage, so we must too. main is
		// a deep copy from LastSentRequest, so this append is private to us.
		main.Messages = append(main.Messages, client.Message{
			Role: "assistant", Content: client.NewTextContent(result),
		})
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		res, err := agent.GenerateSuggestionWithUsage(ctx, llm, main)
		if err != nil {
			return suggestionReadyMsg{gen: gen, text: ""}
		}
		return suggestionReadyMsg{gen: gen, text: res.Text}
	}
}
