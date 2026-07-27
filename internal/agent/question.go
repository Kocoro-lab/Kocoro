package agent

import "context"

// This file defines the transport-agnostic representation of the structured
// ask-user interaction plus the context-injected asker the ask_user_question
// tool uses to reach the daemon's QuestionBroker. The tool lives in
// internal/tools and cannot import internal/daemon (import cycle), so these
// neutral types are the seam: the daemon adapts them to/from its wire
// QuestionRequest / QuestionResponse. Mirrors how approvals decouple the loop's
// OnApprovalNeeded from the daemon broker.

// Question actions the daemon reports back through the asker. Values match the
// daemon wire constants (answer/decline/cancel) so an adapter is a pass-through.
const (
	QuestionActionAnswer  = "answer"
	QuestionActionDecline = "decline"
	QuestionActionCancel  = "cancel"
)

// UIQuestionOption is one enumerated choice the user can pick.
type UIQuestionOption struct {
	Label       string
	Description string
	Preview     string
	Recommended bool
}

// UIQuestion is one closed-choice question with 2-4 options.
type UIQuestion struct {
	Header      string
	Question    string
	MultiSelect bool
	AllowOther  bool
	Options     []UIQuestionOption
}

// UIQuestionRequest is the tool-built ask, before the daemon assigns wire ids
// and transport identity fields. AutoResolutionMs is the model's requested
// auto-decline window (the daemon clamps it).
type UIQuestionRequest struct {
	Questions        []UIQuestion
	AutoResolutionMs int
}

// UIQuestionAnswer binds a user's selection(s) back to a question. Question is
// the full question text (resolved by the daemon adapter from the wire id) so
// the tool can render "<question>"="<values>" without tracking ids itself.
type UIQuestionAnswer struct {
	Question string
	Values   []string
}

// UIQuestionResult is what the asker returns: an action plus, when
// Action==answer, one UIQuestionAnswer per answered question.
type UIQuestionResult struct {
	Action  string
	Answers []UIQuestionAnswer
}

// QuestionAsker is implemented by the daemon and injected onto the tool call
// context per run. A nil asker (unattended run, synchronous HTTP, TUI/one-shot,
// non-interactive channel with no broker) means the current surface cannot ask
// the user, and the tool returns a clean "proceed with best judgment" result.
type QuestionAsker interface {
	AskUserQuestion(ctx context.Context, req UIQuestionRequest) UIQuestionResult
}

type questionAskerKey struct{}

// WithQuestionAsker returns a context carrying asker for ask_user_question to
// find via QuestionAskerFrom.
func WithQuestionAsker(ctx context.Context, asker QuestionAsker) context.Context {
	return context.WithValue(ctx, questionAskerKey{}, asker)
}

// QuestionAskerFrom returns the QuestionAsker injected on ctx, or nil.
func QuestionAskerFrom(ctx context.Context) QuestionAsker {
	asker, _ := ctx.Value(questionAskerKey{}).(QuestionAsker)
	return asker
}
