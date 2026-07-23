package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/agents"
	"github.com/Kocoro-lab/ShanClaw/internal/session"
)

type interruptedTurnCandidate struct {
	SessionID string
	Agent     string
	State     session.InterruptedTurn
	UpdatedAt time.Time
}

// discoverInterruptedTurns scans the default and named-agent session stores.
// It intentionally reads session JSON rather than the SQLite summary index:
// InProgress and InterruptedTurn are recovery state, not list-index columns.
func discoverInterruptedTurns(shannonDir string) ([]interruptedTurnCandidate, error) {
	var stores []struct {
		agent string
		dir   string
	}
	stores = append(stores, struct {
		agent string
		dir   string
	}{dir: filepath.Join(shannonDir, "sessions")})

	agentsRoot := filepath.Join(shannonDir, "agents")
	agentEntries, err := os.ReadDir(agentsRoot)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	for _, entry := range agentEntries {
		if !entry.IsDir() || agents.ValidateAgentName(entry.Name()) != nil {
			continue
		}
		stores = append(stores, struct {
			agent string
			dir   string
		}{agent: entry.Name(), dir: filepath.Join(agentsRoot, entry.Name(), "sessions")})
	}

	var candidates []interruptedTurnCandidate
	for _, store := range stores {
		entries, readErr := os.ReadDir(store.dir)
		if errors.Is(readErr, os.ErrNotExist) {
			continue
		}
		if readErr != nil {
			return nil, readErr
		}
		mgr := session.NewManager(store.dir)
		for _, entry := range entries {
			if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
				continue
			}
			id := strings.TrimSuffix(entry.Name(), ".json")
			if !session.IsValidSessionID(id) {
				continue
			}
			sess, loadErr := mgr.Load(id)
			if loadErr != nil || !sess.InProgress {
				continue
			}
			state := session.InterruptedTurn{
				Agent:     store.agent,
				Source:    sess.Source,
				Channel:   sess.Channel,
				RouteKey:  sess.RouteKey,
				CWD:       sess.CWD,
				UpdatedAt: sess.UpdatedAt,
			}
			if sess.InterruptedTurn != nil {
				state = *sess.InterruptedTurn
				state.IMStatusContext = append(json.RawMessage(nil), sess.InterruptedTurn.IMStatusContext...)
				state.Participants = append([]string(nil), sess.InterruptedTurn.Participants...)
			}
			// The directory is authoritative. A stale or malformed persisted
			// agent value must never redirect recovery into another store.
			state.Agent = store.agent
			candidates = append(candidates, interruptedTurnCandidate{
				SessionID: sess.ID,
				Agent:     store.agent,
				State:     state,
				UpdatedAt: sess.UpdatedAt,
			})
		}
		_ = mgr.Close()
	}

	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].UpdatedAt.Equal(candidates[j].UpdatedAt) {
			if candidates[i].Agent == candidates[j].Agent {
				return candidates[i].SessionID < candidates[j].SessionID
			}
			return candidates[i].Agent < candidates[j].Agent
		}
		return candidates[i].UpdatedAt.Before(candidates[j].UpdatedAt)
	})
	return candidates, nil
}

type interruptedRecoveryHandler struct {
	usage agent.UsageAccumulator
}

func (h *interruptedRecoveryHandler) Usage() agent.AccumulatedUsage     { return h.usage.Snapshot() }
func (h *interruptedRecoveryHandler) OnToolCall(string, string, string) {}
func (h *interruptedRecoveryHandler) OnToolResult(string, string, string, agent.ToolResult, time.Duration) {
}
func (h *interruptedRecoveryHandler) OnText(string)                        {}
func (h *interruptedRecoveryHandler) OnPreamble(string)                    {}
func (h *interruptedRecoveryHandler) OnStreamDelta(string)                 {}
func (h *interruptedRecoveryHandler) OnApprovalNeeded(string, string) bool { return false }
func (h *interruptedRecoveryHandler) OnUsage(u agent.TurnUsage)            { h.usage.Add(u) }
func (h *interruptedRecoveryHandler) OnCloudAgent(string, string, string)  {}
func (h *interruptedRecoveryHandler) OnCloudProgress(int, int)             {}
func (h *interruptedRecoveryHandler) OnCloudPlan(string, string, bool)     {}
func (h *interruptedRecoveryHandler) OnRunStatus(string, string)           {}

func emitInterruptedRecoveryStatus(deps *ServerDeps, candidate interruptedTurnCandidate, code, detail string) {
	if deps == nil || deps.EventBus == nil {
		return
	}
	payload, _ := json.Marshal(map[string]any{
		"session_id": candidate.SessionID,
		"agent":      candidate.Agent,
		"code":       code,
		"detail":     detail,
	})
	deps.EventBus.Emit(Event{Type: EventRunStatus, Payload: payload})
}

func (s *Server) resumeInterruptedTurns(ctx context.Context) {
	if s == nil || s.deps == nil || s.deps.SessionCache == nil || s.deps.GW == nil {
		return
	}
	candidates, err := discoverInterruptedTurns(s.deps.ShannonDir)
	if err != nil {
		log.Printf("daemon: interrupted-turn discovery failed: %v", err)
		return
	}
	for _, candidate := range candidates {
		if ctx.Err() != nil {
			return
		}
		emitInterruptedRecoveryStatus(s.deps, candidate, "interrupted_turn_resuming", "continuing durable checkpoint")
		state := candidate.State
		req := RunAgentRequest{
			Text:              interruptedTurnContinuation,
			Agent:             candidate.Agent,
			SessionID:         candidate.SessionID,
			Source:            state.Source,
			Sender:            state.Sender,
			Channel:           state.Channel,
			ThreadID:          state.ThreadID,
			CWD:               state.CWD,
			RouteKey:          state.RouteKey,
			CloudMessageID:    state.CloudMessageID,
			IMStatusContext:   append(json.RawMessage(nil), state.IMStatusContext...),
			Participants:      append([]string(nil), state.Participants...),
			ResumeInterrupted: true,
		}
		result, runErr := RunAgent(ctx, s.deps, req, &interruptedRecoveryHandler{})
		if runErr != nil {
			log.Printf("daemon: interrupted turn resume failed session=%s agent=%s: %v",
				candidate.SessionID, candidate.Agent, runErr)
			emitInterruptedRecoveryStatus(s.deps, candidate, "interrupted_turn_resume_failed", runErr.Error())
			continue
		}
		emitInterruptedRecoveryStatus(s.deps, candidate, "interrupted_turn_resumed", "durable checkpoint completed")

		// Cloud-routed turns no longer have their original claim after a process
		// restart. Use the persisted opaque target for a precise proactive
		// delivery; local/Desktop consumers already receive EventAgentReply.
		if s.deps.WSClient != nil && len(state.IMStatusContext) > 0 && result != nil && result.Reply != "" {
			if sendErr := s.deps.WSClient.SendProactive(
				candidate.Agent, result.Reply, result.SessionID, state.IMStatusContext, nil,
			); sendErr != nil {
				log.Printf("daemon: resumed turn delivery failed session=%s: %v", candidate.SessionID, sendErr)
				emitInterruptedRecoveryStatus(s.deps, candidate, "interrupted_turn_delivery_failed", sendErr.Error())
			}
		}
	}
}
