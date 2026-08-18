package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// This file implements the same-turn mechanical latch against automatic
// retries of uncertain mutations. When a material tool call ends
// outcome-unknown, the uncertain result now returns to the model as an
// ordinary tool error (so it can narrate and decide) instead of
// hard-stopping the run — but the model must not be able to silently
// re-send the exact same mutation while nobody is watching. The latch
// blocks a byte-identical tool+arguments repeat locally (no network, no
// approval card) for the REST OF THE CURRENT USER TURN. The next user
// message clears it: the dangerous scenario is the model retrying on its
// own within one turn; once the user speaks, a human is back in the loop.
// Calls with different arguments are unaffected.
//
// The latch is in-memory and run-scoped by design (v1): the durable
// side-effect journal already persists the outcome_unknown record, and a
// fresh run begins with a fresh user message anyway.

// unknownOutcomeGateKey identifies one exact call: tool name plus a hash of
// the parsed argument string. Both the arming and blocking sites read the
// same callMeta argsStr representation, so byte-identical model retries hash
// identically.
func unknownOutcomeGateKey(toolName, argsStr string) string {
	sum := sha256.Sum256([]byte(argsStr))
	return toolName + "\x00" + hex.EncodeToString(sum[:])
}

// armUnknownOutcomeGate latches one exact call after its outcome became
// unknown. Loop-goroutine only — no locking, like the sibling per-run state.
func (a *AgentLoop) armUnknownOutcomeGate(toolName, argsStr string) {
	if a.unknownOutcomeRetryGate == nil {
		a.unknownOutcomeRetryGate = make(map[string]struct{})
	}
	a.unknownOutcomeRetryGate[unknownOutcomeGateKey(toolName, argsStr)] = struct{}{}
}

// unknownOutcomeGateBlocks reports whether this exact call is latched.
func (a *AgentLoop) unknownOutcomeGateBlocks(toolName, argsStr string) bool {
	if len(a.unknownOutcomeRetryGate) == 0 {
		return false
	}
	_, blocked := a.unknownOutcomeRetryGate[unknownOutcomeGateKey(toolName, argsStr)]
	return blocked
}

// clearUnknownOutcomeGate drops every latched call. Called when a new user
// message enters the conversation: at run start and when a mid-run injected
// follow-up is committed into the live turn.
func (a *AgentLoop) clearUnknownOutcomeGate() {
	a.unknownOutcomeRetryGate = nil
}

// unknownOutcomeGateBlockedResult is the local rejection paired for a latched
// call. Known no effect: the call was intercepted before any journal entry or
// network dispatch.
func unknownOutcomeGateBlockedResult(toolName string) ToolResult {
	result := BusinessError(fmt.Sprintf(
		"this %s call is byte-identical to a call earlier in this turn whose outcome is still unknown. "+
			"It was blocked locally and NOT re-sent. Tell the user what happened, in their language, and "+
			"suggest verifying directly in the external system (e.g. X) before any retry.",
		toolName,
	))
	result.SideEffectKnownNoEffect = true
	return result
}
