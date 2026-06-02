package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/client"
	ctxwin "github.com/Kocoro-lab/ShanClaw/internal/context"
	"github.com/Kocoro-lab/ShanClaw/internal/session"
)

type compactDoneMsg struct {
	beforeTokens int
	afterTokens  int
	summary      string
	err          error
}

// runCompact performs context compaction: persist learnings → summarize → shape history.
func (m *Model) runCompact(customInstructions string) func() compactDoneMsg {
	return func() compactDoneMsg {
		sess := m.sessions.Current()
		if sess == nil {
			return compactDoneMsg{err: fmt.Errorf("no active session")}
		}
		messages := sess.Messages
		if len(messages) < ctxwin.MinShapeable() {
			return compactDoneMsg{err: fmt.Errorf("conversation too short to compact (need %d+ messages, have %d)", ctxwin.MinShapeable(), len(messages))}
		}

		beforeTokens := ctxwin.EstimateTokens(messages)

		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		var usage agent.UsageAccumulator

		// Step 1: persist learnings to MEMORY.md
		memoryDir := m.shannonDir + "/memory"
		if m.agentOverride != nil {
			memoryDir = fmt.Sprintf("%s/agents/%s", m.shannonDir, m.agentOverride.Name)
		}
		plUsage, _ := ctxwin.PersistLearnings(ctx, m.gateway, messages, memoryDir)
		if plUsage.InputTokens > 0 || plUsage.OutputTokens > 0 {
			usage.Add(agent.LLMUsageDelta(plUsage, ""))
		}

		// Step 2: generate summary
		msgsForSummary := messages
		if customInstructions != "" {
			hint := client.Message{
				Role:    "user",
				Content: client.NewTextContent("Summarization focus: " + customInstructions),
			}
			msgsForSummary = append([]client.Message{hint}, messages...)
		}
		summary, sumUsage, err := ctxwin.GenerateSummary(ctx, m.gateway, msgsForSummary)
		if sumUsage.InputTokens > 0 || sumUsage.OutputTokens > 0 {
			usage.Add(agent.LLMUsageDelta(sumUsage, ""))
		}
		if err != nil {
			return compactDoneMsg{err: fmt.Errorf("summarization failed: %w", err)}
		}

		// Step 3: shape history.
		// ShapeHistory expects [system] + [first user] + ... but TUI sessions
		// don't persist the system prompt. Prepend a placeholder so the array
		// layout matches, then strip it from the result.
		ctxWindow := m.cfg.Agent.ContextWindow
		if ctxWindow <= 0 {
			ctxWindow = 128000
		}
		withSystem := make([]client.Message, 0, 1+len(messages))
		withSystem = append(withSystem, client.Message{Role: "system", Content: client.NewTextContent("(compaction placeholder)")})
		withSystem = append(withSystem, messages...)
		shaped := ctxwin.ShapeHistory(withSystem, summary, ctxWindow)

		// Strip the placeholder system message from shaped result
		if len(shaped) > 0 && shaped[0].Role == "system" {
			shaped = shaped[1:]
		}

		// Rebuild MessageMeta to stay index-aligned with the new Messages.
		newMeta := make([]session.MessageMeta, len(shaped))
		for i := range newMeta {
			newMeta[i] = session.MessageMeta{Source: "local", Timestamp: session.TimePtr(time.Now())}
		}

		// Update session
		sess.Messages = shaped
		sess.MessageMeta = newMeta
		acc := usage.Snapshot()
		if llm := acc.LLM; llm.LLMCalls > 0 || llm.TotalTokens > 0 || llm.CostUSD > 0 {
			m.sessions.AddUsage(sess.ID, session.UsageFromAccumulated(
				llm.LLMCalls, llm.InputTokens, llm.OutputTokens, llm.TotalTokens,
				llm.CostUSD, llm.CacheReadTokens, llm.CacheCreationTokens, llm.CacheCreation5mTokens, llm.CacheCreation1hTokens, llm.Model,
				acc.ToolCalls, acc.ToolCostUSD,
			))
		}
		m.sessions.Save()

		afterTokens := ctxwin.EstimateTokens(shaped)

		// Truncate summary for display
		displaySummary := summary
		if r := []rune(displaySummary); len(r) > 200 {
			displaySummary = string(r[:200]) + "..."
		}

		return compactDoneMsg{
			beforeTokens: beforeTokens,
			afterTokens:  afterTokens,
			summary:      displaySummary,
		}
	}
}

func formatCompactResult(msg compactDoneMsg) string {
	dimStyle := lipgloss.NewStyle().Foreground(colorDim)
	var sb strings.Builder
	sb.WriteString(dimStyle.Render(fmt.Sprintf("  Context compressed: ~%s → ~%s tokens",
		formatTokenCount(msg.beforeTokens), formatTokenCount(msg.afterTokens))))
	sb.WriteString("\n")
	if msg.summary != "" {
		sb.WriteString(dimStyle.Render("  Summary: " + msg.summary))
	}
	return sb.String()
}
