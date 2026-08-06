package tui

import (
	"context"
	"fmt"
	"log"
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

// compactTimeout bounds the whole /compact pass: PersistLearnings plus a
// summarize that may fold an oversized transcript into up to
// maxSummaryFoldChunks sequential small-tier calls. A 60s shared budget
// aborted exactly the sessions large enough to need /compact.
const compactTimeout = 5 * time.Minute

// compactWindowAndOverhead resolves the context window and estimator
// calibration for /compact from the live loop when one exists — its window
// tracks response.model auto-detection and its calibration carries the
// tools-schema mass — falling back to config, then the 128K legacy default.
func (m *Model) compactWindowAndOverhead() (window, overheadTokens int) {
	if m.agentLoop != nil {
		window, _ = m.agentLoop.ContextWindow()
		overheadTokens, _, _ = m.agentLoop.EstOverheadState()
	}
	if window <= 0 {
		window = m.cfg.Agent.ContextWindow
	}
	if window <= 0 {
		window = 128000
	}
	return window, overheadTokens
}

// runCompact performs context compaction: persist learnings → summarize → shape history.
func (m *Model) runCompact(customInstructions string) func() compactDoneMsg {
	return func() compactDoneMsg {
		sess := m.sessions.Current()
		if sess == nil {
			return compactDoneMsg{err: fmt.Errorf("no active session")}
		}
		messages := sess.HistoryForLoop()
		// Captured next to the history snapshot it must agree with. Reading it
		// after the summarize round-trip would depend on m.state == stateProcessing
		// blocking handleSubmit from appending in between — a correct but
		// implicit cross-file invariant.
		archiveThrough := len(sess.Messages)
		if len(messages) < ctxwin.MinShapeable() {
			return compactDoneMsg{err: fmt.Errorf("conversation too short to compact (need %d+ messages, have %d)", ctxwin.MinShapeable(), len(messages))}
		}

		beforeTokens := ctxwin.EstimateTokens(messages)

		ctx, cancel := context.WithTimeout(context.Background(), compactTimeout)
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
		ctxWindow, overheadTokens := m.compactWindowAndOverhead()
		withSystem := make([]client.Message, 0, 1+len(messages))
		withSystem = append(withSystem, client.Message{Role: "system", Content: client.NewTextContent("(compaction placeholder)")})
		withSystem = append(withSystem, messages...)
		shaped := ctxwin.ForceShapeHistory(withSystem, summary, ctxWindow, overheadTokens)
		if len(shaped) >= len(withSystem) {
			// ForceShapeHistory contract: no net reduction possible. Bail
			// before replacing the live checkpoint or reporting a compression
			// that freed nothing.
			return compactDoneMsg{err: fmt.Errorf("nothing to compact: %d messages already at minimum shape", len(messages))}
		}

		// Strip the placeholder system message from shaped result
		if len(shaped) > 0 && shaped[0].Role == "system" {
			shaped = shaped[1:]
		}

		// Restore recently read files into the compacted view, mirroring the
		// daemon's proactive/preflight paths: the summary paraphrases file
		// content whose exact text is still on disk, and the loop's
		// ReadTracker (long-lived in the TUI) knows what was read. Manual
		// compaction skips the task reanchor — the user's next prompt is the
		// anchor here.
		restoreBuilder := m.fileRestoreBuilder
		if restoreBuilder == nil && m.agentLoop != nil {
			restoreBuilder = m.agentLoop.BuildPostCompactionFileRestore
		}
		if restoreBuilder != nil {
			if restoreMsg, ok := restoreBuilder(shaped, overheadTokens); ok {
				shaped = append(shaped, restoreMsg)
			}
		}

		// Preserve the exact prior live state as best-effort rollback material,
		// then replace only the live checkpoint. The transcript and its metadata
		// remain lossless and index-stable for resume/search/share/audit.
		if retention := m.cfg.Agent.CompactionSnapshotRetention; retention > 0 {
			if err := m.sessions.SaveCompactionSnapshot(sess.ID, "manual", messages, retention); err != nil {
				log.Printf("tui: manual compaction snapshot failed (session=%s): %v", sess.ID, err)
			}
		}
		// Roll back on a failed save. Without this the user is told compaction
		// failed while the in-memory session already runs on the new checkpoint,
		// and the next successful Save() persists it anyway.
		priorCheckpoint := sess.CompactionCheckpoint
		sess.CompactionCheckpoint = &session.CompactionCheckpoint{
			SchemaVersion:       session.CompactionCheckpointSchemaVersion,
			ArchiveThroughIndex: archiveThrough,
			Messages:            agent.SanitizeMessagesForPersistence(shaped),
		}
		acc := usage.Snapshot()
		if llm := acc.LLM; llm.LLMCalls > 0 || llm.WebSearchCalls > 0 || llm.TotalTokens > 0 || llm.CostUSD > 0 {
			m.sessions.AddUsage(sess.ID, session.UsageFromAccumulated(
				llm.LLMCalls, llm.WebSearchCalls, llm.InputTokens, llm.OutputTokens, llm.TotalTokens,
				llm.CostUSD, llm.CacheReadTokens, llm.CacheCreationTokens, llm.CacheCreation5mTokens, llm.CacheCreation1hTokens, llm.Model,
				acc.ToolCalls, acc.ToolCostUSD,
			))
		}
		if err := m.sessions.Save(); err != nil {
			sess.CompactionCheckpoint = priorCheckpoint
			return compactDoneMsg{err: fmt.Errorf("save compaction checkpoint: %w", err)}
		}

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

// persistMidTurnCompactionCheckpoint durably saves an applied compaction's
// checkpoint while the run is still in flight (wired to the loop's
// CheckpointFunc). Run messages still land at run end; ArchiveThroughIndex
// therefore points at the archive as it stands now — the checkpoint messages
// already carry the shaped current-run state, so a crash before run end
// resumes on checkpoint + empty tail, which is self-consistent.
func (m *Model) persistMidTurnCompactionCheckpoint(sess *session.Session, checkpoint []client.Message) error {
	if sess == nil || len(checkpoint) == 0 {
		return nil
	}
	prior := sess.CompactionCheckpoint
	sess.CompactionCheckpoint = &session.CompactionCheckpoint{
		SchemaVersion:       session.CompactionCheckpointSchemaVersion,
		ArchiveThroughIndex: len(sess.Messages),
		Messages:            checkpoint,
	}
	if err := m.sessions.Save(); err != nil {
		sess.CompactionCheckpoint = prior
		return err
	}
	return nil
}

func formatCompactResult(msg compactDoneMsg) string {
	dimStyle := lipgloss.NewStyle().Foreground(colorDim)
	var sb strings.Builder
	// Small sessions can net out larger (few messages dropped, summary
	// inserted) — do not call that "compressed".
	label := "Context compressed"
	if msg.afterTokens >= msg.beforeTokens {
		label = "Context reshaped (no size reduction)"
	}
	sb.WriteString(dimStyle.Render(fmt.Sprintf("  %s: ~%s → ~%s tokens",
		label, formatTokenCount(msg.beforeTokens), formatTokenCount(msg.afterTokens))))
	sb.WriteString("\n")
	if msg.summary != "" {
		sb.WriteString(dimStyle.Render("  Summary: " + msg.summary))
	}
	return sb.String()
}
