//go:build darwin && cgo

package koe

import "sync"

// NativeFloorEnabled keeps ASR out of playback-overlap admission. With VPIO
// barge-in active, Realtime hears the raw audio and receives only two narrow
// choices: resume the paused assistant or accept a genuine user turn.
func NativeFloorEnabled() bool { return koeEnvBool("KOE_NATIVE_FLOOR", true) }

func nativeFloorControlEnabled(fullDuplexAEC bool) bool {
	return fullDuplexAEC &&
		koeEnvBool("KOE_VPIO_BARGE_IN", false) &&
		NativeFloorEnabled() &&
		!koeEnvBool("KOE_INTERRUPT_RESPONSE", false)
}

const nativeFloorInstructions = "Decide turn-taking from the user's most recent RAW SPOKEN AUDIO, not from a transcript. Call exactly one function and say nothing. Call resume_playback for a brief backchannel, filler, laugh, acknowledgement, or non-directed sound that does not ask Kocoro to stop, change, answer, or act. Call accept_turn for a real interruption: a question, request, correction, topic change, explicit stop, or any utterance that expects a response. When uncertain, prefer accept_turn."

func nativeFloorToolDefs() []ToolDef {
	return []ToolDef{
		{
			Type:        "function",
			Name:        "resume_playback",
			Description: "The overlapping audio is only a backchannel or non-directed sound. Resume the exact paused assistant response and do not reply.",
			Parameters:  obj(`{"type":"object","properties":{},"required":[]}`),
		},
		{
			Type:        "function",
			Name:        "accept_turn",
			Description: "The overlapping audio is a genuine user turn that expects Kocoro to stop, change, answer, or act. Discard the paused response and handle this turn.",
			Parameters:  obj(`{"type":"object","properties":{},"required":[]}`),
		},
	}
}

type floorDecision uint8

const (
	floorDecisionNone floorDecision = iota
	floorDecisionResume
	floorDecisionAccept
)

type floorStage uint8

const (
	floorIdle floorStage = iota
	floorPaused
	floorWaitingForJudge
	floorJudging
	floorResuming
	floorAccepting
)

type floorToolClaim struct {
	handled   bool
	duplicate bool
	decision  floorDecision
	turnID    int64
	reason    string
}

// nativeFloorController is deliberately semantic-free. It tracks which exact
// assistant response was paused and which narrow judge response may decide its
// fate. The Realtime model makes the semantic decision from raw audio.
type nativeFloorController struct {
	mu               sync.Mutex
	stage            floorStage
	sourceResponseID string
	turnID           int64
	judgeResponseID  string
	claimedCalls     map[string]struct{}
}

func newNativeFloorController() *nativeFloorController { return &nativeFloorController{} }

func (f *nativeFloorController) begin(sourceResponseID string) bool {
	if f == nil || sourceResponseID == "" {
		return false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stage != floorIdle {
		return true
	}
	f.stage = floorPaused
	f.sourceResponseID = sourceResponseID
	f.turnID = 0
	f.judgeResponseID = ""
	f.claimedCalls = nil
	return true
}

// noteUserCommit returns true when this committed user turn is the overlapping
// audio currently holding playback and therefore needs the narrow judge.
func (f *nativeFloorController) noteUserCommit(turnID int64) bool {
	if f == nil || turnID <= 0 {
		return false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stage != floorPaused {
		return false
	}
	f.turnID = turnID
	f.stage = floorWaitingForJudge
	return true
}

func (f *nativeFloorController) bindJudge(responseID string, turnID int64) bool {
	if f == nil || responseID == "" || turnID <= 0 {
		return false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stage != floorWaitingForJudge || f.turnID != turnID {
		return false
	}
	f.judgeResponseID = responseID
	f.claimedCalls = make(map[string]struct{})
	f.stage = floorJudging
	return true
}

func (f *nativeFloorController) claim(responseID, callID, tool string) floorToolClaim {
	if f == nil || responseID == "" {
		return floorToolClaim{}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if responseID != f.judgeResponseID || (f.stage != floorJudging && f.stage != floorResuming && f.stage != floorAccepting) {
		return floorToolClaim{}
	}
	claim := floorToolClaim{handled: true, turnID: f.turnID}
	fingerprint := callID
	if fingerprint == "" {
		fingerprint = tool
	}
	if _, exists := f.claimedCalls[fingerprint]; exists {
		claim.duplicate = true
		claim.reason = "duplicate_floor_event"
		return claim
	}
	f.claimedCalls[fingerprint] = struct{}{}
	if f.stage != floorJudging {
		claim.reason = "floor_already_decided"
		return claim
	}
	switch tool {
	case "resume_playback":
		f.stage = floorResuming
		claim.decision = floorDecisionResume
	case "accept_turn":
		f.stage = floorAccepting
		claim.decision = floorDecisionAccept
	default:
		claim.reason = "invalid_floor_tool"
	}
	return claim
}

func (f *nativeFloorController) holdsPlayback() bool {
	if f == nil {
		return false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stage == floorPaused || f.stage == floorWaitingForJudge || f.stage == floorJudging
}

func (f *nativeFloorController) holdsSource(responseID string) bool {
	if f == nil || responseID == "" {
		return false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sourceResponseID == responseID &&
		(f.stage == floorPaused || f.stage == floorWaitingForJudge || f.stage == floorJudging)
}

func (f *nativeFloorController) finishResponse(responseID string) floorDecision {
	if f == nil || responseID == "" {
		return floorDecisionNone
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if responseID != f.judgeResponseID {
		return floorDecisionNone
	}
	decision := floorDecisionNone
	// A judging response that did not produce a valid narrow tool call must never
	// strand paused speech. Decisions already applied at function-call time only
	// need their controller state cleared here.
	if f.stage == floorJudging {
		decision = floorDecisionResume
	}
	f.resetLocked()
	return decision
}

func (f *nativeFloorController) failTurn(turnID int64) floorDecision {
	if f == nil || turnID <= 0 {
		return floorDecisionNone
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.turnID != turnID || f.stage == floorIdle {
		return floorDecisionNone
	}
	f.resetLocked()
	return floorDecisionResume
}

func (f *nativeFloorController) failJudge(responseID string) floorDecision {
	if f == nil || responseID == "" {
		return floorDecisionNone
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.judgeResponseID != responseID || f.stage != floorJudging {
		return floorDecisionNone
	}
	f.resetLocked()
	return floorDecisionResume
}

func (f *nativeFloorController) abort() bool {
	if f == nil {
		return false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stage == floorIdle {
		return false
	}
	f.resetLocked()
	return true
}

func (f *nativeFloorController) resetLocked() {
	f.stage = floorIdle
	f.sourceResponseID = ""
	f.turnID = 0
	f.judgeResponseID = ""
	f.claimedCalls = nil
}
