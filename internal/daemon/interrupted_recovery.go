package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/agents"
	"github.com/Kocoro-lab/ShanClaw/internal/session"
)

type interruptedTurnCandidate struct {
	SessionID string
	Agent     string
	StoreDir  string
	State     session.InterruptedTurn
	UpdatedAt time.Time
}

// discoverInterruptedTurns scans the default and named-agent session stores.
// Each store maintains a durable marker index, so steady-state discovery loads
// only interrupted candidates. Stores created before the index existed perform
// one lightweight header migration inside session.InterruptedSessions.
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
		mgr := session.NewManager(store.dir)
		interrupted, loadErr := mgr.InterruptedSessions()
		_ = mgr.Close()
		if loadErr != nil {
			return nil, loadErr
		}
		for _, sess := range interrupted {
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
				StoreDir:  store.dir,
				State:     state,
				UpdatedAt: sess.UpdatedAt,
			})
		}
	}

	// Newest first: when several checkpoints fit the staleness window, the
	// most recently interrupted turn is the one the user most plausibly still
	// cares about, and it should not wait behind older candidates.
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].UpdatedAt.Equal(candidates[j].UpdatedAt) {
			if candidates[i].Agent == candidates[j].Agent {
				return candidates[i].SessionID < candidates[j].SessionID
			}
			return candidates[i].Agent < candidates[j].Agent
		}
		return candidates[i].UpdatedAt.After(candidates[j].UpdatedAt)
	})
	return candidates, nil
}

type interruptedRecoveryHandler struct {
	usage agent.UsageAccumulator
}

// IsUnattendedRun is always true: a recovered turn executes at daemon start
// with no human present, regardless of the session's original source. Without
// this marker a desktop/kocoro-source checkpoint would classify as attended
// and bypass the unattended auto-approval deny-list (computer_use, screenshot).
func (h *interruptedRecoveryHandler) IsUnattendedRun() bool { return true }

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

func interruptedResumeAttemptLimit(deps *ServerDeps) int {
	const defaultLimit = 3
	if deps == nil {
		return defaultLimit
	}
	cfg, _, _ := deps.Snapshot()
	if cfg == nil || cfg.Agent.InterruptedResumeMaxAttempts <= 0 {
		return defaultLimit
	}
	return cfg.Agent.InterruptedResumeMaxAttempts
}

// interruptedResumeMaxAge returns the staleness window for auto-resume from
// agent.interrupted_resume_max_age_hours; zero/unset or negative falls back
// to 4h. See the config field comment for the rationale.
func interruptedResumeMaxAge(deps *ServerDeps) time.Duration {
	const defaultMaxAge = 4 * time.Hour
	if deps == nil {
		return defaultMaxAge
	}
	cfg, _, _ := deps.Snapshot()
	if cfg == nil || cfg.Agent.InterruptedResumeMaxAgeHours <= 0 {
		return defaultMaxAge
	}
	return time.Duration(cfg.Agent.InterruptedResumeMaxAgeHours) * time.Hour
}

func interruptedResumeEnabled(deps *ServerDeps) bool {
	if deps == nil {
		return true
	}
	cfg, _, _ := deps.Snapshot()
	if cfg == nil || cfg.Agent.InterruptedResumeEnabled == nil {
		return true
	}
	return *cfg.Agent.InterruptedResumeEnabled
}

// isStaleInterruptedTurn reports whether a checkpoint is too old to auto-resume.
// A zero UpdatedAt (malformed or pre-feature data) is always stale: with no
// trustworthy timestamp the safe reading is "not recently interrupted".
func isStaleInterruptedTurn(candidate interruptedTurnCandidate, maxAge time.Duration, now time.Time) bool {
	ts := candidate.UpdatedAt
	if ts.IsZero() {
		ts = candidate.State.UpdatedAt
	}
	if ts.IsZero() {
		return true
	}
	return now.Sub(ts) > maxAge
}

func cloneInterruptedTurn(state session.InterruptedTurn) session.InterruptedTurn {
	state.IMStatusContext = append(json.RawMessage(nil), state.IMStatusContext...)
	state.Participants = append([]string(nil), state.Participants...)
	return state
}

func persistInterruptedResumeAttempt(mgr *session.Manager, candidate interruptedTurnCandidate, attempt int) error {
	sess, err := mgr.Resume(candidate.SessionID)
	if err != nil {
		return err
	}
	if !sess.InProgress {
		return fmt.Errorf("session is no longer in progress")
	}
	state := cloneInterruptedTurn(candidate.State)
	state.Agent = candidate.Agent
	state.ResumeAttempts = attempt
	state.UpdatedAt = time.Now()
	sess.InterruptedTurn = &state
	return mgr.Save()
}

func abandonInterruptedTurn(mgr *session.Manager, candidate interruptedTurnCandidate) error {
	sess, err := mgr.Resume(candidate.SessionID)
	if err != nil {
		return err
	}
	sess.InProgress = false
	sess.InterruptedTurn = nil
	return mgr.Save()
}

// buildInterruptedResumeRequest constructs the continuation request for a
// durable checkpoint. The original route key is PINNED so the resume takes the
// same route lock concurrent inbound traffic for this session would take —
// SessionID alone would compute session:<id> while a Slack/IM message for the
// same session computes its channel/thread key, letting two loops run over one
// session file. When the session had no route key (fresh web/webhook runs),
// the SessionID branch still yields a stable session:<id> lock shared with
// session-addressed follow-ups.
func buildInterruptedResumeRequest(candidate interruptedTurnCandidate, attempt int) RunAgentRequest {
	state := candidate.State
	return RunAgentRequest{
		Text:                     interruptedTurnContinuation,
		Agent:                    candidate.Agent,
		SessionID:                candidate.SessionID,
		Source:                   state.Source,
		Sender:                   state.Sender,
		Channel:                  state.Channel,
		ThreadID:                 state.ThreadID,
		CWD:                      state.CWD,
		RouteKey:                 state.RouteKey,
		PinnedRouteKey:           state.RouteKey,
		CloudMessageID:           state.CloudMessageID,
		IMStatusContext:          append(json.RawMessage(nil), state.IMStatusContext...),
		Participants:             append([]string(nil), state.Participants...),
		ResumeInterrupted:        true,
		InterruptedResumeAttempt: attempt,
	}
}

func (s *Server) resumeInterruptedTurns(ctx context.Context) {
	if s == nil || s.deps == nil || s.deps.SessionCache == nil || s.deps.GW == nil {
		return
	}
	if !interruptedResumeEnabled(s.deps) {
		return
	}
	candidates, err := discoverInterruptedTurns(s.deps.ShannonDir)
	if err != nil {
		log.Printf("daemon: interrupted-turn discovery failed: %v", err)
		return
	}
	maxAttempts := interruptedResumeAttemptLimit(s.deps)
	maxAge := interruptedResumeMaxAge(s.deps)
	for _, candidate := range candidates {
		if ctx.Err() != nil {
			return
		}
		s.resumeInterruptedCandidate(ctx, candidate, maxAttempts, maxAge)
	}
}

// resumeInterruptedCandidate applies the policy gates and continues one
// durable checkpoint. One session.Manager serves every store access for the
// candidate (attempt persistence + abandon paths) instead of re-opening the
// store per operation.
func (s *Server) resumeInterruptedCandidate(ctx context.Context, candidate interruptedTurnCandidate, maxAttempts int, maxAge time.Duration) {
	mgr := session.NewManager(candidate.StoreDir)
	defer mgr.Close()

	state := candidate.State
	// Staleness gate: a checkpoint older than the window carries a user
	// intent whose context is gone (live incident 2026-07-23: a 3-month-old
	// interrupted destructive turn re-executed on upgrade). Abandon it —
	// clearing the marker — instead of resuming or leaving it to rescan.
	if isStaleInterruptedTurn(candidate, maxAge, time.Now()) {
		if abandonErr := abandonInterruptedTurn(mgr, candidate); abandonErr != nil {
			log.Printf("daemon: stale interrupted turn abandon failed session=%s agent=%s: %v",
				candidate.SessionID, candidate.Agent, abandonErr)
			emitInterruptedRecoveryStatus(s.deps, candidate, "interrupted_turn_resume_failed",
				fmt.Sprintf("failed to clear stale checkpoint: %v", abandonErr))
			return
		}
		emitInterruptedRecoveryStatus(s.deps, candidate, "interrupted_turn_abandoned",
			fmt.Sprintf("checkpoint interrupted at %s exceeds the %s auto-resume window",
				candidate.UpdatedAt.Format(time.RFC3339), maxAge))
		return
	}
	if state.ResumeAttempts >= maxAttempts {
		if abandonErr := abandonInterruptedTurn(mgr, candidate); abandonErr != nil {
			log.Printf("daemon: interrupted turn abandon failed session=%s agent=%s: %v",
				candidate.SessionID, candidate.Agent, abandonErr)
			emitInterruptedRecoveryStatus(s.deps, candidate, "interrupted_turn_resume_failed",
				fmt.Sprintf("failed to clear exhausted recovery marker: %v", abandonErr))
			return
		}
		emitInterruptedRecoveryStatus(s.deps, candidate, "interrupted_turn_abandoned",
			fmt.Sprintf("automatic recovery exhausted after %d attempts", state.ResumeAttempts))
		return
	}

	attempt := state.ResumeAttempts + 1
	if persistErr := persistInterruptedResumeAttempt(mgr, candidate, attempt); persistErr != nil {
		log.Printf("daemon: interrupted turn attempt checkpoint failed session=%s agent=%s: %v",
			candidate.SessionID, candidate.Agent, persistErr)
		emitInterruptedRecoveryStatus(s.deps, candidate, "interrupted_turn_resume_failed",
			fmt.Sprintf("failed to persist recovery attempt %d/%d: %v", attempt, maxAttempts, persistErr))
		return
	}
	state.ResumeAttempts = attempt
	emitInterruptedRecoveryStatus(s.deps, candidate, "interrupted_turn_resuming",
		fmt.Sprintf("continuing durable checkpoint (attempt %d/%d)", attempt, maxAttempts))
	req := buildInterruptedResumeRequest(candidate, attempt)
	result, runErr := RunAgent(ctx, s.deps, req, &interruptedRecoveryHandler{})
	if runErr != nil {
		log.Printf("daemon: interrupted turn resume failed session=%s agent=%s: %v",
			candidate.SessionID, candidate.Agent, runErr)
		if attempt >= maxAttempts {
			if abandonErr := abandonInterruptedTurn(mgr, candidate); abandonErr != nil {
				emitInterruptedRecoveryStatus(s.deps, candidate, "interrupted_turn_resume_failed",
					fmt.Sprintf("attempt %d/%d failed and marker cleanup failed: %v", attempt, maxAttempts, abandonErr))
				return
			}
			emitInterruptedRecoveryStatus(s.deps, candidate, "interrupted_turn_abandoned",
				fmt.Sprintf("automatic recovery exhausted after %d attempts: %v", attempt, runErr))
		} else {
			emitInterruptedRecoveryStatus(s.deps, candidate, "interrupted_turn_resume_failed",
				fmt.Sprintf("attempt %d/%d failed: %v", attempt, maxAttempts, runErr))
		}
		return
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
