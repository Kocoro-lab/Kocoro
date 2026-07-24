package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
)

// stubAsker records the request it received and returns a canned result,
// standing in for the daemon's brokerQuestionAsker.
type stubAsker struct {
	got    agent.UIQuestionRequest
	result agent.UIQuestionResult
}

func (s *stubAsker) AskUserQuestion(_ context.Context, req agent.UIQuestionRequest) agent.UIQuestionResult {
	s.got = req
	return s.result
}

func runAsk(t *testing.T, asker agent.QuestionAsker, argsJSON string) agent.ToolResult {
	t.Helper()
	ctx := context.Background()
	if asker != nil {
		ctx = agent.WithQuestionAsker(ctx, asker)
	}
	res, err := (&AskUserQuestionTool{}).Run(ctx, argsJSON)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	return res
}

func TestAskUserQuestion_SingleSelect(t *testing.T) {
	asker := &stubAsker{result: agent.UIQuestionResult{
		Action:  agent.QuestionActionAnswer,
		Answers: []agent.UIQuestionAnswer{{Question: "Deploy target?", Values: []string{"Staging"}}},
	}}
	args := `{"questions":[{"question":"Deploy target?","options":[{"label":"Staging"},{"label":"Production"}]}]}`
	res := runAsk(t, asker, args)
	if res.IsError {
		t.Fatalf("unexpected error result: %q", res.Content)
	}
	if !strings.Contains(res.Content, `"Deploy target?"="Staging"`) {
		t.Fatalf("content missing single-select answer: %q", res.Content)
	}
	// allow_other defaults to true when omitted.
	if len(asker.got.Questions) != 1 || !asker.got.Questions[0].AllowOther {
		t.Fatalf("allow_other should default true: %+v", asker.got.Questions)
	}
}

func TestAskUserQuestion_MultiSelectCommaJoin(t *testing.T) {
	asker := &stubAsker{result: agent.UIQuestionResult{
		Action:  agent.QuestionActionAnswer,
		Answers: []agent.UIQuestionAnswer{{Question: "Which languages?", Values: []string{"Go", "Rust", "Python"}}},
	}}
	args := `{"questions":[{"question":"Which languages?","multi_select":true,"options":[{"label":"Go"},{"label":"Rust"},{"label":"Python"}]}]}`
	res := runAsk(t, asker, args)
	if !strings.Contains(res.Content, `"Which languages?"="Go,Rust,Python"`) {
		t.Fatalf("multi-select values must be comma-joined: %q", res.Content)
	}
}

func TestAskUserQuestion_Decline(t *testing.T) {
	asker := &stubAsker{result: agent.UIQuestionResult{Action: agent.QuestionActionDecline}}
	args := `{"questions":[{"question":"Pick one","options":[{"label":"A"},{"label":"B"}]}]}`
	res := runAsk(t, asker, args)
	if res.IsError {
		t.Fatalf("decline must not be an error result: %q", res.Content)
	}
	if !strings.Contains(res.Content, "declined") {
		t.Fatalf("decline content missing: %q", res.Content)
	}
}

func TestAskUserQuestion_MissingQuestionsValidationError(t *testing.T) {
	res := runAsk(t, &stubAsker{}, `{}`)
	if !res.IsError {
		t.Fatal("missing questions must be an error")
	}
	if !strings.HasPrefix(res.Content, "[validation error]") {
		t.Fatalf("missing questions must return a ValidationError (with prefix), got: %q", res.Content)
	}
}

func TestAskUserQuestion_TooFewOptionsValidationError(t *testing.T) {
	args := `{"questions":[{"question":"Pick","options":[{"label":"only one"}]}]}`
	res := runAsk(t, &stubAsker{}, args)
	if !res.IsError || !strings.HasPrefix(res.Content, "[validation error]") {
		t.Fatalf("fewer than 2 options must be a ValidationError, got: %q", res.Content)
	}
}

func TestAskUserQuestion_NoAskerCleanFallback(t *testing.T) {
	args := `{"questions":[{"question":"Pick one","options":[{"label":"A"},{"label":"B"}]}]}`
	res := runAsk(t, nil, args) // no asker on ctx (unattended / non-interactive)
	if res.IsError {
		t.Fatalf("no-asker path must not error: %q", res.Content)
	}
	if !strings.Contains(res.Content, "cannot present an interactive question") {
		t.Fatalf("no-asker content missing fallback guidance: %q", res.Content)
	}
}
