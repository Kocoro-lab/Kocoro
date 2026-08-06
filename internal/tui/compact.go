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
	// gen is the Model.compactGen captured when this pass started. The Update
	// handler drops the message when it no longer matches — an Esc-cancelled
	// pass must not flip the UI state (or print a stale result) under a newer
	// run started meanwhile.
	gen int
	// Deferred-apply payload: the worker goroutine computes but never mutates
	// shared session state; the Update handler applies snapshot + checkpoint +
	// usage + save AFTER the generation check, so a cancelled pass cannot race
	// a newer run's writes. sessionID pins the apply to the session the pass
	// read from.
	sessionID      string
	shaped         []client.Message
	archiveThrough int
	preCompact     []client.Message
	usage          agent.AccumulatedUsage
}

// defaultCompactTimeout bounds the whole /compact pass: PersistLearnings plus
// a summarize that may fold an oversized transcript into up to
// maxSummaryFoldChunks sequential small-tier calls. A 60s shared budget
// aborted exactly the sessions large enough to need /compact. Overridable via
// agent.compact_timeout_secs for slow gateways.
const defaultCompactTimeout = 5 * time.Minute

// compactTimeout resolves the /compact deadline from config, falling back to
// the 5-minute default when unset or non-positive.
func (m *Model) compactTimeout() time.Duration {
	if secs := m.cfg.Agent.CompactTimeoutSecs; secs > 0 {
		return time.Duration(secs) * time.Second
	}
	return defaultCompactTimeout
}

// compactWindowAndOverhead resolves the context window and estimator
// calibration for /compact from the live loop when one exists — its window
// tracks response.model auto-detection and its calibration carries the
// tools-schema mass — falling back to config, then the 128K legacy default.
//
// The returned overhead also includes the loop's last system-prompt estimate:
// /compact shapes a history whose system slot is a tiny placeholder, while
// the calibration is measured against requests whose estimate already carries
// the real prompt — without this, shaping and restoration budgets would treat
// the whole system prompt as free headroom. 0 before the first Run, which
// degrades to the prior behavior.
func (m *Model) compactWindowAndOverhead() (window, overheadTokens int) {
	if m.agentLoop != nil {
		window, _ = m.agentLoop.ContextWindow()
		overheadTokens, _, _ = m.agentLoop.EstOverheadState()
		overheadTokens += m.agentLoop.LastSystemPromptEstimate()
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
// ctx comes from the submit site so Esc/Ctrl+C cancel the LLM calls via
// m.cancelRun; gen tags the result for the staleness guard in Update.
func (m *Model) runCompact(ctx context.Context, customInstructions string, gen int) func() compactDoneMsg {
	return func() compactDoneMsg {
		done := func(msg compactDoneMsg) compactDoneMsg {
			msg.gen = gen
			return msg
		}
		sess := m.sessions.Current()
		if sess == nil {
			return done(compactDoneMsg{err: fmt.Errorf("no active session")})
		}
		messages := sess.HistoryForLoop()
		// Captured next to the history snapshot it must agree with. Reading it
		// after the summarize round-trip would depend on m.state == stateProcessing
		// blocking handleSubmit from appending in between — a correct but
		// implicit cross-file invariant.
		archiveThrough := len(sess.Messages)
		if len(messages) < ctxwin.MinShapeable() {
			return done(compactDoneMsg{err: fmt.Errorf("conversation too short to compact (need %d+ messages, have %d)", ctxwin.MinShapeable(), len(messages))})
		}

		beforeTokens := ctxwin.EstimateTokens(messages)

		ctx, cancel := context.WithTimeout(ctx, m.compactTimeout())
		defer cancel()
		// Fast-fail before any paid call: an already-cancelled pass (Esc
		// raced the submit) must not start LLM work.
		if err := ctx.Err(); err != nil {
			return done(compactDoneMsg{err: err})
		}
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
			return done(compactDoneMsg{err: fmt.Errorf("summarization failed: %w", err)})
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
			return done(compactDoneMsg{err: fmt.Errorf("nothing to compact: %d messages already at minimum shape", len(messages))})
		}

		// Strip the placeholder system message from shaped result
		if len(shaped) > 0 && shaped[0].Role == "system" {
			shaped = shaped[1:]
		}

		// A cancellation that lands after the LLM calls must not proceed to
		// the restore builder (it reads live loop state a newer run may be
		// writing) or the checkpoint payload: the user abandoned the pass.
		if err := ctx.Err(); err != nil {
			return done(compactDoneMsg{err: err})
		}

		// Restore recently read files into the compacted view, mirroring the
		// daemon's proactive/preflight paths: the summary paraphrases file
		// content whose exact text is still on disk, and the loop's
		// ReadTracker (long-lived in the TUI) knows what was read. Manual
		// compaction skips the task reanchor — the user's next prompt is the
		// anchor here. Skipped when the shaped tail already ends with a user
		// message (e.g. a tool_result after an Esc-cancelled model turn):
		// SanitizeCompactedHistory's keep-later merge would delete that
		// earlier user message on the next load, and orphan-stripping would
		// then take its paired tool_use with it.
		restoreBuilder := m.fileRestoreBuilder
		if restoreBuilder == nil && m.agentLoop != nil {
			restoreBuilder = m.agentLoop.BuildPostCompactionFileRestore
		}
		if restoreBuilder != nil && (len(shaped) == 0 || shaped[len(shaped)-1].Role != "user") {
			if restoreMsg, ok := restoreBuilder(shaped, overheadTokens); ok {
				shaped = append(shaped, restoreMsg)
			}
		}

		afterTokens := ctxwin.EstimateTokens(shaped)

		// Truncate summary for display
		displaySummary := summary
		if r := []rune(displaySummary); len(r) > 200 {
			displaySummary = string(r[:200]) + "..."
		}

		// All mutation (snapshot, checkpoint, usage, save) happens in
		// applyCompactResult on the UI goroutine, after the generation check —
		// the worker returns a pure payload so a cancelled pass can never race
		// a newer run's session writes.
		return done(compactDoneMsg{
			beforeTokens:   beforeTokens,
			afterTokens:    afterTokens,
			summary:        displaySummary,
			sessionID:      sess.ID,
			shaped:         agent.SanitizeMessagesForPersistence(shaped),
			archiveThrough: archiveThrough,
			preCompact:     messages,
			usage:          usage.Snapshot(),
		})
	}
}

// applyCompactResult commits a completed /compact pass: pre-replacement
// snapshot, checkpoint swap, usage accounting, and the durable save with
// rollback. Runs on the UI goroutine from the compactDoneMsg handler, after
// the generation check, so it cannot interleave with a newer run's writes.
func (m *Model) applyCompactResult(msg compactDoneMsg) error {
	sess := m.sessions.Current()
	if sess == nil || sess.ID != msg.sessionID {
		return fmt.Errorf("session changed while compacting; result discarded")
	}
	// Preserve the exact prior live state as best-effort rollback material,
	// then replace only the live checkpoint. The transcript and its metadata
	// remain lossless and index-stable for resume/search/share/audit.
	if retention := m.cfg.Agent.CompactionSnapshotRetention; retention > 0 {
		if err := m.sessions.SaveCompactionSnapshot(sess.ID, "manual", msg.preCompact, retention); err != nil {
			log.Printf("tui: manual compaction snapshot failed (session=%s): %v", sess.ID, err)
		}
	}
	// Roll back on a failed save. Without this the user is told compaction
	// failed while the in-memory session already runs on the new checkpoint,
	// and the next successful Save() persists it anyway.
	priorCheckpoint := sess.CompactionCheckpoint
	sess.CompactionCheckpoint = &session.CompactionCheckpoint{
		SchemaVersion:       session.CompactionCheckpointSchemaVersion,
		ArchiveThroughIndex: msg.archiveThrough,
		Messages:            msg.shaped,
	}
	if llm := msg.usage.LLM; llm.LLMCalls > 0 || llm.WebSearchCalls > 0 || llm.TotalTokens > 0 || llm.CostUSD > 0 {
		m.sessions.AddUsage(sess.ID, session.UsageFromAccumulated(
			llm.LLMCalls, llm.WebSearchCalls, llm.InputTokens, llm.OutputTokens, llm.TotalTokens,
			llm.CostUSD, llm.CacheReadTokens, llm.CacheCreationTokens, llm.CacheCreation5mTokens, llm.CacheCreation1hTokens, llm.Model,
			msg.usage.ToolCalls, msg.usage.ToolCostUSD,
		))
	}
	if err := m.sessions.Save(); err != nil {
		sess.CompactionCheckpoint = priorCheckpoint
		return fmt.Errorf("save compaction checkpoint: %w", err)
	}
	return nil
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
