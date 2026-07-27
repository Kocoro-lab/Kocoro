package daemon

import (
	"crypto/rand"
	"encoding/hex"
)

// This file defines the wire contract for the structured ask-user interaction
// ("question"): the agent enumerates closed choices and the user picks from a
// selection UI instead of typing a bare token the model must then disambiguate.
// It is the second request/resolve interaction kind after approvals; the
// pending-interaction lifecycle is shared with ApprovalBroker rather than
// copied (see CLAUDE.md "Request/resolve interactions"). Emitted daemon→UI on
// the bus as EventQuestionRequest / EventQuestionResolved and on the
// per-request SSE stream as "question"; answered UI→daemon via POST /question.
//
// Field-shape parity note: QuestionRequest mirrors ApprovalRequest's split —
// this struct is the per-request SSE payload (channel/thread_id/agent are
// present-but-empty for foreground runs, no omitempty), while the bus copy is
// built by makeQuestionRequestEmitter with an added `ts`.

// Question auto-resolution bounds. If a QuestionRequest carries a positive
// AutoResolutionMs, the daemon auto-resolves (declines) the pending question
// after that delay so an unanswered card on an unattended/abandoned surface
// never blocks the agent loop indefinitely — the counterpart to
// ApprovalTimeout for the question interaction. The model may request a
// shorter/longer window; the daemon clamps it to this range.
//
//   - workload: a user who walks away mid-question, or a channel whose client
//     never renders the card, must not pin the tool call open for the full
//     5-minute ApprovalTimeout on every ask.
//   - symptom when it binds: a question the model set below 60s auto-declines
//     while the user is still reading; above 240s it approaches ApprovalTimeout
//     and the bound stops mattering.
//   - override: values outside the range are clamped, not rejected; widen here
//     if a genuinely slower deliberation surface appears.
const (
	questionAutoResolutionMinMs = 60_000
	questionAutoResolutionMaxMs = 240_000
)

// Question action constants used on both the POST /question ingress
// (QuestionResponse.Action) and the resolved bus payload
// (QuestionResolvedPayload.Action).
const (
	// QuestionActionAnswer — the user submitted a selection. Answers populated.
	QuestionActionAnswer = "answer"
	// QuestionActionDecline — the user explicitly skipped (MCP "decline"): a
	// deliberate "no answer", distinct from a dismissed/timed-out dialog.
	QuestionActionDecline = "decline"
	// QuestionActionCancel — the dialog was dismissed / timed out without an
	// explicit choice (MCP "cancel"). Daemon-originated (timeout / disconnect)
	// resolutions use this on the resolved bus payload.
	QuestionActionCancel = "cancel"
)

// QuestionOption is one enumerated choice: a display label plus a short
// description of what picking it means.
type QuestionOption struct {
	Label       string `json:"label"`
	Description string `json:"description"`
	// Preview is optional supplementary content (mockup / snippet) rendered
	// when the option is focused. Single-select only, per the AskUserQuestion
	// convention; ignored for multi-select questions.
	Preview string `json:"preview,omitempty"`
	// Recommended surfaces the model's suggested default. The UI renders a
	// "Recommended" affordance and (by convention) the model orders the
	// recommended option first.
	Recommended bool `json:"recommended,omitempty"`
}

// Question is one closed-choice question. A single QuestionRequest may carry
// 1-4 questions, each with 2-4 options (the tool schema enforces the counts;
// the wire type itself is permissive so an older decoder never hard-fails on a
// future count change).
type Question struct {
	// ID is a stable per-request identifier ("q0", "q1", …). Answers on the
	// POST /question ingress are keyed by it, so the model receives each full
	// option label bound to the right question — never a bare index/token.
	ID string `json:"id"`
	// Header is the short chip label (≤12 chars by convention) shown above the
	// question, e.g. "处理方向" / "Auth method".
	Header      string `json:"header"`
	Question    string `json:"question"`
	MultiSelect bool   `json:"multi_select"`
	// AllowOther, when true, tells the UI to auto-inject a free-text "Other"
	// escape. The model never authors an Other option itself — the client
	// injects it — so one primitive covers both closed selection and open
	// clarification.
	AllowOther bool             `json:"allow_other"`
	Options    []QuestionOption `json:"options"`
}

// QuestionRequest is the daemon→UI payload asking the user to pick from
// enumerated options. Per-request SSE payload shape (see file header for the
// bus-copy parity note). RequestID is "qst_"+hex.
type QuestionRequest struct {
	// MessageID is the inbound claim's WS envelope ID, used by Cloud to route a
	// question card back to the originating channel/thread. Never serialized on
	// the local wire (parity with ApprovalRequest.MessageID).
	MessageID string `json:"-"`
	SessionID string `json:"session_id,omitempty"`
	Source    string `json:"source,omitempty"`
	Channel   string `json:"channel"`
	ThreadID  string `json:"thread_id"`
	RequestID string `json:"request_id"`
	Agent     string `json:"agent"`
	Questions []Question `json:"questions"`
	// AutoResolutionMs, when > 0, is the clamped auto-decline window (see
	// questionAutoResolution{Min,Max}Ms). The UI may render a countdown.
	AutoResolutionMs int `json:"auto_resolution_ms,omitempty"`
}

// QuestionAnswer binds a user's selection(s) back to a Question by ID. Values
// holds the chosen option labels (or the free-text string for an "Other"
// choice); multi-select yields multiple entries, single-select exactly one.
type QuestionAnswer struct {
	ID     string   `json:"id"`
	Values []string `json:"values"`
}

// QuestionResponse is the UI→daemon body received at POST /question. Action
// distinguishes an actual selection from an explicit decline; a dismissed /
// timed-out dialog produces no ingress at all (the daemon resolves it via the
// auto-resolution / disconnect cleanup path instead).
type QuestionResponse struct {
	RequestID string           `json:"request_id"`
	Action    string           `json:"action"` // QuestionActionAnswer | QuestionActionDecline
	Answers   []QuestionAnswer `json:"answers,omitempty"`
}

// QuestionResolvedPayload is the terminal bus event (EventQuestionResolved)
// that tells UI clients to dismiss the question card. Mirrors
// ApprovalResolvedPayload's at-most-one-terminal-event contract: exactly one
// resolved event per request_id, whether the user answered/declined
// (ResolvedBy "kocoro") or the daemon abandoned it via timeout/disconnect
// (ResolvedBy "daemon", Action QuestionActionCancel).
type QuestionResolvedPayload struct {
	RequestID  string `json:"request_id"`
	Action     string `json:"action"`      // answer | decline | cancel
	ResolvedBy string `json:"resolved_by"` // "kocoro" (UI) | "daemon" (cleanup)
}

// clampQuestionAutoResolutionMs bounds a model-supplied auto-resolution window
// to [questionAutoResolutionMinMs, questionAutoResolutionMaxMs]. A zero/absent
// value means "no auto-resolution" and is returned unchanged.
func clampQuestionAutoResolutionMs(ms int) int {
	if ms <= 0 {
		return 0
	}
	if ms < questionAutoResolutionMinMs {
		return questionAutoResolutionMinMs
	}
	if ms > questionAutoResolutionMaxMs {
		return questionAutoResolutionMaxMs
	}
	return ms
}

// generateQuestionRequestID mints a "qst_"-prefixed request ID, mirroring
// generateRequestID's "apr_" approvals so the two interaction kinds share a
// legible, collision-free ID convention.
func generateQuestionRequestID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return "qst_" + hex.EncodeToString(b)
}
