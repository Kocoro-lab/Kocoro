//go:build darwin && cgo

package koe

import (
	"encoding/json"
	"sync"
)

const (
	// maxTurnTaskActions covers a spoken turn that cancels or corrects existing
	// work while starting distinct new work. On the fifth action the loop rejects
	// it and emits a tools-disabled closure naming what did and did not run. This
	// is an interaction safety budget, not a power-user throughput knob: disable
	// the continuation loop with KOE_TOOL_CONTINUATION=0, or change this constant
	// with the focused tool-loop tests when deliberately revising the contract.
	maxTurnTaskActions = 4
	// maxTrackedToolLoopTurns retains bookkeeping for a long voice call without
	// letting completed turns grow memory forever. Once 64 turns bind, only the
	// oldest turn's replay/provenance records are evicted; active work continues.
	// Raise this code-level retention cap with the tool-loop tests if calls longer
	// than 64 tracked turns need stale-event protection farther into history.
	maxTrackedToolLoopTurns = 64
)

func ToolContinuationEnabled() bool { return koeEnvBool("KOE_TOOL_CONTINUATION", true) }

type responsePurpose string

const (
	responsePurposeUser         responsePurpose = "user"
	responsePurposeContinuation responsePurpose = "continuation"
	responsePurposeClosure      responsePurpose = "closure"
	responsePurposeTaskResult   responsePurpose = "task_result"
	responsePurposeSynthetic    responsePurpose = "synthetic"
	responsePurposeFloor        responsePurpose = "floor"
)

func (purpose responsePurpose) allowsTools() bool {
	return purpose == responsePurposeUser || purpose == responsePurposeContinuation
}

type loopTurn struct {
	actions int
	closed  bool
}

type loopResponse struct {
	turnID         int64
	purpose        responsePurpose
	toolCalls      int
	doTaskCalls    int
	deferredTasks  int
	claimedCalls   map[string]struct{}
	claimedActions map[string]struct{}
	budgetHit      bool
	finished       bool
	messageItems   int
	fuseTripped    bool
}

// noteDeferredDoTask records that a valid do_task was resolved immediately with
// status=running. When every action in a response has this shape, there is no
// synchronous result for a continuation Response to discuss; the mailbox will
// create exactly one result Response when the real work finishes.
func (l *toolLoopLedger) noteDeferredDoTask(responseID string) {
	if l == nil || responseID == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if response := l.responses[responseID]; response != nil {
		response.deferredTasks++
	}
}

type toolLoopDecision int

const (
	toolLoopNone toolLoopDecision = iota
	toolLoopContinue
	toolLoopClose
)

type toolActionClaim struct {
	known                  bool
	allowed                bool
	duplicate              bool
	duplicateAction        bool
	turnID                 int64
	sameResponseDoTaskCall bool
	reason                 string
}

// toolLoopLedger is a semantic-free controller around the native model. The
// model chooses tools from raw audio; this ledger only enforces provenance,
// a four-action budget, a bounded continuation loop, and newer-turn preemption.
type toolLoopLedger struct {
	mu        sync.Mutex
	current   int64
	turns     map[int64]*loopTurn
	responses map[string]*loopResponse
	order     []int64
}

func isFinalVoiceControlTool(tool string) bool {
	return tool == "stop_speaking" || tool == "end_call"
}

func newToolLoopLedger() *toolLoopLedger {
	return &toolLoopLedger{
		turns: make(map[int64]*loopTurn), responses: make(map[string]*loopResponse),
	}
}

func (l *toolLoopLedger) noteUserCommit(turnID int64) {
	if l == nil || turnID <= 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.current = turnID
	if _, ok := l.turns[turnID]; !ok {
		l.turns[turnID] = &loopTurn{}
		l.order = append(l.order, turnID)
		for len(l.order) > maxTrackedToolLoopTurns {
			oldest := l.order[0]
			l.order = l.order[1:]
			delete(l.turns, oldest)
			for responseID, response := range l.responses {
				if response.turnID == oldest {
					delete(l.responses, responseID)
				}
			}
		}
	}
}

func (l *toolLoopLedger) isCurrent(turnID int64) bool {
	if l == nil || turnID <= 0 {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	_, exists := l.turns[turnID]
	return l.current == turnID && exists
}

func (l *toolLoopLedger) bindResponse(responseID string, purpose responsePurpose, turnID int64) {
	if l == nil || responseID == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if purpose == "" {
		purpose = responsePurposeUser
	}
	if turnID <= 0 {
		turnID = l.current
	}
	if turnID <= 0 {
		return
	}
	if _, ok := l.turns[turnID]; !ok {
		l.turns[turnID] = &loopTurn{}
		l.order = append(l.order, turnID)
	}
	l.responses[responseID] = &loopResponse{
		turnID: turnID, purpose: purpose,
		claimedCalls: make(map[string]struct{}), claimedActions: make(map[string]struct{}),
	}
}

func canonicalToolAction(tool string, args []byte) string {
	normalized := args
	var value any
	if json.Unmarshal(args, &value) == nil {
		if encoded, err := json.Marshal(value); err == nil {
			normalized = encoded
		}
	}
	return tool + "\x00" + string(normalized)
}

func (l *toolLoopLedger) claimAction(responseID, callID, tool string, args []byte) toolActionClaim {
	if l == nil || responseID == "" {
		return toolActionClaim{reason: "unknown_response"}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	response, ok := l.responses[responseID]
	if !ok {
		return toolActionClaim{reason: "unknown_response"}
	}
	claim := toolActionClaim{known: true, turnID: response.turnID}
	fingerprint := callID
	if fingerprint == "" {
		fingerprint = tool + "\x00" + string(args)
	}
	if _, duplicate := response.claimedCalls[fingerprint]; duplicate {
		claim.duplicate = true
		claim.reason = "duplicate_tool_event"
		return claim
	}
	// Record accepted and rejected calls alike so a repeated server event can
	// neither replay a side effect nor emit a second function output.
	response.claimedCalls[fingerprint] = struct{}{}
	if !response.purpose.allowsTools() {
		claim.reason = "response_has_no_tool_capability"
		return claim
	}
	turn := l.turns[response.turnID]
	if turn == nil || turn.closed || l.current != response.turnID {
		claim.reason = "turn_preempted"
		return claim
	}
	// Voice-control actions finish the spoken turn and must remain available even
	// after the ordinary action budget is exhausted. end_call is a call-lifecycle
	// terminal; stop_speaking is a silent turn terminal that keeps the call alive.
	// Closing here also denies any parallel tool emitted after either control.
	if isFinalVoiceControlTool(tool) {
		response.toolCalls++
		turn.closed = true
		claim.allowed = true
		return claim
	}
	action := canonicalToolAction(tool, args)
	if tool == "do_task" {
		if _, duplicate := response.claimedActions[action]; duplicate {
			claim.duplicate = true
			claim.duplicateAction = true
			claim.reason = "duplicate_tool_action"
			return claim
		}
	}
	if turn.actions >= maxTurnTaskActions {
		response.budgetHit = true
		claim.reason = "turn_action_budget_exhausted"
		return claim
	}
	turn.actions++
	if tool == "do_task" {
		response.claimedActions[action] = struct{}{}
	}
	response.toolCalls++
	if tool == "do_task" {
		response.doTaskCalls++
		claim.sameResponseDoTaskCall = response.doTaskCalls > 1
	}
	claim.allowed = true
	return claim
}

func (l *toolLoopLedger) finishResponse(responseID string) (toolLoopDecision, int64) {
	if l == nil || responseID == "" {
		return toolLoopNone, 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	response, ok := l.responses[responseID]
	if !ok || response.finished {
		return toolLoopNone, 0
	}
	response.finished = true
	turn := l.turns[response.turnID]
	if turn == nil || turn.closed || l.current != response.turnID || !response.purpose.allowsTools() {
		return toolLoopNone, response.turnID
	}
	if response.toolCalls == 0 && !response.budgetHit {
		turn.closed = true
		return toolLoopNone, response.turnID
	}
	if response.budgetHit || turn.actions >= maxTurnTaskActions {
		turn.closed = true
		return toolLoopClose, response.turnID
	}
	if response.toolCalls == response.doTaskCalls && response.doTaskCalls == response.deferredTasks {
		turn.closed = true
		return toolLoopNone, response.turnID
	}
	return toolLoopContinue, response.turnID
}

// noteMessageItem returns true once, on the second assistant message item in a
// Response. One Response may contain many function calls but only one speech
// item; the second is the deterministic replay fuse.
func (l *toolLoopLedger) noteMessageItem(responseID, itemType string) bool {
	if l == nil || responseID == "" || (itemType != "" && itemType != "message") {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	response, ok := l.responses[responseID]
	if !ok {
		return false
	}
	response.messageItems++
	if response.messageItems > 1 && !response.fuseTripped {
		response.fuseTripped = true
		return true
	}
	return false
}
