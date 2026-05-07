package heartbeat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/agents"
	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/daemon"
	"github.com/Kocoro-lab/ShanClaw/internal/watcher"
)

const maxChecklistChars = 4000

// IsHeartbeatOK returns true if the agent reply is the silent ack token.
func IsHeartbeatOK(reply string) bool {
	return strings.EqualFold(strings.TrimSpace(reply), "HEARTBEAT_OK")
}

// IsHeartbeatOKFromMessages checks the last assistant message in a transcript.
func IsHeartbeatOKFromMessages(messages []client.Message) bool {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "assistant" {
			return strings.EqualFold(strings.TrimSpace(messages[i].Content.Text()), "HEARTBEAT_OK")
		}
	}
	return false
}

// FormatPrompt builds the heartbeat prompt from a checklist body.
func FormatPrompt(checklist string) string {
	return fmt.Sprintf(`This is a periodic heartbeat check. Review the checklist below and check each item using your available tools. If everything is fine, reply with exactly "HEARTBEAT_OK" and nothing else. If something needs attention, describe the issue concisely.

Checklist:
%s`, checklist)
}

// FormatGoalPrompt builds a goal-driven heartbeat prompt.
func FormatGoalPrompt(goals string) string {
	return fmt.Sprintf(`This is a periodic check-in. Review your goals below and your current conversation context. If something needs your attention, take action using your available tools. If nothing needs doing, reply with exactly "HEARTBEAT_OK" and nothing else.

Goals:
%s`, goals)
}

// ReadChecklist reads HEARTBEAT.md at the given path.
// Missing file returns ("", nil) — this is the expected "disabled" state.
// Other read errors return ("", error) so callers can detect degraded monitoring.
// Content exceeding maxChecklistChars is truncated with a warning.
func ReadChecklist(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil // missing = heartbeat disabled for this agent
		}
		return "", fmt.Errorf("read HEARTBEAT.md: %w", err)
	}
	content := strings.TrimSpace(string(data))
	if content == "" {
		return "", nil
	}
	if len(content) > maxChecklistChars {
		log.Printf("heartbeat: HEARTBEAT.md at %s exceeds %d chars, truncating", path, maxChecklistChars)
		content = content[:maxChecklistChars]
	}
	return content, nil
}

// agentHeartbeat holds per-agent heartbeat state.
type agentHeartbeat struct {
	name        string
	interval    time.Duration
	activeHours string
	model       string
	agentDir    string
	mu          sync.Mutex // overlap prevention
}

// Manager runs periodic heartbeat checks for all configured agents.
type Manager struct {
	agents []*agentHeartbeat
	deps   *daemon.ServerDeps
	cancel context.CancelFunc
	done   chan struct{}
}

// New creates a heartbeat Manager by scanning agents for heartbeat config.
// Returns an empty (but valid) Manager if no agents have heartbeat configured.
func New(agentsDir string, deps *daemon.ServerDeps) (*Manager, error) {
	agentEntries, err := agents.ListAgents(agentsDir)
	if err != nil {
		return nil, fmt.Errorf("heartbeat: list agents: %w", err)
	}

	var entries []*agentHeartbeat
	for _, ae := range agentEntries {
		ag, err := agents.LoadAgent(agentsDir, ae.Name)
		if err != nil {
			log.Printf("heartbeat: skip agent %q: %v", ae.Name, err)
			continue
		}
		if ag.Config == nil || ag.Config.Heartbeat == nil || ag.Config.Heartbeat.Every == "" {
			continue
		}
		hb := ag.Config.Heartbeat

		interval, err := time.ParseDuration(hb.Every)
		if err != nil {
			log.Printf("heartbeat: skip agent %q: invalid interval %q: %v", ae.Name, hb.Every, err)
			continue
		}
		if interval < 1*time.Minute {
			log.Printf("heartbeat: skip agent %q: interval %s too short (min 1m)", ae.Name, interval)
			continue
		}

		entries = append(entries, &agentHeartbeat{
			name:        ae.Name,
			interval:    interval,
			activeHours: hb.ActiveHours,
			model:       hb.Model,
			agentDir:    filepath.Join(agentsDir, ae.Name),
		})
	}

	return &Manager{
		agents: entries,
		deps:   deps,
		done:   make(chan struct{}),
	}, nil
}

// Start launches per-agent ticker goroutines. Blocks until ctx is cancelled or Close is called.
func (m *Manager) Start(ctx context.Context) {
	ctx, m.cancel = context.WithCancel(ctx)

	var wg sync.WaitGroup
	for _, ah := range m.agents {
		wg.Add(1)
		go func(ah *agentHeartbeat) {
			defer wg.Done()
			m.runTicker(ctx, ah)
		}(ah)
	}

	go func() {
		wg.Wait()
		close(m.done)
	}()
}

// runTicker runs the heartbeat ticker for a single agent.
func (m *Manager) runTicker(ctx context.Context, ah *agentHeartbeat) {
	ticker := time.NewTicker(ah.interval)
	defer ticker.Stop()

	log.Printf("heartbeat: started for agent %q every %s", ah.name, ah.interval)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.tick(ctx, ah)
		}
	}
}

// tick executes a single heartbeat check for an agent.
func (m *Manager) tick(ctx context.Context, ah *agentHeartbeat) {
	if !ah.mu.TryLock() {
		log.Printf("heartbeat: skip %q (previous tick still running)", ah.name)
		return
	}
	defer ah.mu.Unlock()

	if !watcher.InActiveHours(ah.activeHours, time.Now()) {
		log.Printf("heartbeat: skip %q (outside active hours)", ah.name)
		return
	}

	checklistPath := filepath.Join(ah.agentDir, "HEARTBEAT.md")
	checklist, err := ReadChecklist(checklistPath)
	if err != nil || checklist == "" {
		log.Printf("heartbeat: skip %q (no goals/checklist)", ah.name)
		return
	}

	m.tickGoalDriven(ctx, ah, checklist, time.Now())
}

// tickGoalDriven runs a goal-driven heartbeat with session context.
func (m *Manager) tickGoalDriven(ctx context.Context, ah *agentHeartbeat, goals string, start time.Time) {
	routeKey := "agent:" + ah.name
	sessionsDir := filepath.Join(ah.agentDir, "sessions")

	snapshot, err := m.deps.SessionCache.ResolveLatestSession(routeKey, sessionsDir)
	if err != nil {
		if errors.Is(err, daemon.ErrRouteActive) {
			log.Printf("heartbeat: skip %q (run in progress)", ah.name)
		} else {
			log.Printf("heartbeat: %q skipped_no_session: %v", ah.name, err)
		}
		return
	}
	sessionID := snapshot.ID

	prompt := FormatGoalPrompt(goals)
	collector := &TranscriptCollector{}

	req := daemon.RunAgentRequest{
		Agent:          ah.name,
		Source:         "heartbeat",
		Text:           prompt,
		Ephemeral:      true,
		BypassRouting:  true,
		ModelOverride:  ah.model,
		CWD:            snapshot.CWD,
		SessionHistory: snapshot.Messages,
	}

	result, err := daemon.RunAgent(ctx, m.deps, req, collector)
	elapsed := time.Since(start).Milliseconds()

	if err != nil {
		// Context cancellation is normal during shutdown/reload — don't alert.
		if ctx.Err() != nil {
			log.Printf("heartbeat: %q canceled (session=%s, duration=%dms)", ah.name, sessionID, elapsed)
			return
		}
		log.Printf("heartbeat: %q error (session=%s, duration=%dms): %v", ah.name, sessionID, elapsed, err)
		m.emitAlert(ah.name, fmt.Sprintf("Heartbeat error: %v", err), "")
		return
	}

	if IsHeartbeatOKFromMessages(collector.Messages) {
		log.Printf("heartbeat: %q ok (session=%s, duration=%dms)", ah.name, sessionID, elapsed)
		return
	}

	// Only persist the final assistant message — tool calls and tool results
	// are internal mechanics and should not appear in the user's conversation.
	var finalMsgs []client.Message
	for i := len(collector.Messages) - 1; i >= 0; i-- {
		if collector.Messages[i].Role == "assistant" {
			finalMsgs = []client.Message{collector.Messages[i]}
			break
		}
	}
	if len(finalMsgs) == 0 {
		log.Printf("heartbeat: %q action but no assistant message to persist", ah.name)
		return
	}
	appendErr := m.deps.SessionCache.AppendToSession(routeKey, sessionsDir, sessionID, finalMsgs)
	if appendErr != nil {
		if errors.Is(appendErr, daemon.ErrRouteActive) {
			log.Printf("heartbeat: %q skipped_append (run in progress, session=%s, duration=%dms)", ah.name, sessionID, elapsed)
		} else if errors.Is(appendErr, daemon.ErrSessionChanged) {
			log.Printf("heartbeat: %q session_changed (session=%s, duration=%dms)", ah.name, sessionID, elapsed)
			m.emitAlert(ah.name, "Heartbeat completed but session changed — turn dropped", "")
		} else {
			log.Printf("heartbeat: %q append error (session=%s, duration=%dms): %v", ah.name, sessionID, elapsed, appendErr)
		}
		return
	}

	log.Printf("heartbeat: %q action (session=%s, duration=%dms): %s", ah.name, sessionID, elapsed, result.Reply)
	m.emitAlert(ah.name, result.Reply, sessionID)

	// Deliver to Slack/Lark/etc. via Shannon Cloud
	if m.deps.WSClient != nil {
		if err := m.deps.WSClient.SendProactive(ah.name, result.Reply, sessionID); err != nil {
			log.Printf("heartbeat: %q proactive send failed: %v", ah.name, err)
		}
	}
}

// emitAlert sends a heartbeat alert event via the event bus.
func (m *Manager) emitAlert(agent, text, sessionID string) {
	if m.deps.EventBus == nil {
		return
	}
	payload, _ := json.Marshal(map[string]string{
		"agent":      agent,
		"text":       text,
		"session_id": sessionID,
	})
	m.deps.EventBus.Emit(daemon.Event{
		Type:    daemon.EventHeartbeatAlert,
		Payload: payload,
	})
}

// Close cancels all tickers and waits for goroutines to finish.
func (m *Manager) Close() {
	if m.cancel != nil {
		m.cancel()
	}
	<-m.done
}

// TranscriptCollector captures all messages during a run for post-run inspection.
type TranscriptCollector struct {
	Messages []client.Message
	usage    agent.UsageAccumulator
}

// Usage returns the cumulative usage collected during the heartbeat run.
func (tc *TranscriptCollector) Usage() agent.AccumulatedUsage { return tc.usage.Snapshot() }

func (tc *TranscriptCollector) OnToolCall(name string, args string) {}
func (tc *TranscriptCollector) OnToolResult(name string, args string, result agent.ToolResult, elapsed time.Duration) {
}
func (tc *TranscriptCollector) OnText(text string) {
	tc.Messages = append(tc.Messages, client.Message{Role: "assistant", Content: client.NewTextContent(text)})
}

// OnPreamble is a no-op: heartbeat persists only the final assistant
// message (the last OnText). Preamble narration is not useful for the
// post-run alert and would bloat tc.Messages.
func (tc *TranscriptCollector) OnPreamble(text string)                                 {}
func (tc *TranscriptCollector) OnStreamDelta(delta string)                             {}
func (tc *TranscriptCollector) OnUsage(usage agent.TurnUsage)                          { tc.usage.Add(usage) }
func (tc *TranscriptCollector) OnCloudAgent(agentID, status, message string)           {}
func (tc *TranscriptCollector) OnCloudProgress(completed, total int)                   {}
func (tc *TranscriptCollector) OnCloudPlan(planType, content string, needsReview bool) {}
func (tc *TranscriptCollector) OnApprovalNeeded(tool string, args string) bool {
	return !agent.DisallowsAutoApproval(tool)
}
