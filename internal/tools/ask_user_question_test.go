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

func TestAskUserQuestion_InfoSeparatesNeedFromQuestionPresentation(t *testing.T) {
	info := (&AskUserQuestionTool{}).Info()
	for _, guidance := range []string{
		"Use only after determining that an answer is necessary",
		"prefer a reasonable assumption for low-impact ambiguity",
		"`Structured question UI: available`",
		"otherwise ask the necessary question in prose",
		"a necessary question has 2-4 concrete options",
		"call this tool in the same response",
		"merely say you are waiting in prose",
	} {
		if !strings.Contains(info.Description, guidance) {
			t.Errorf("tool description missing question-gating guidance %q", guidance)
		}
	}
	if strings.Contains(info.Description, "explicitly asks") {
		t.Error("tool description must not make any named-option request an automatic trigger")
	}
}

func TestAskUserQuestion_InfoPreventsDuplicateCustomOption(t *testing.T) {
	info := (&AskUserQuestionTool{}).Info()
	properties := info.Parameters["properties"].(map[string]any)
	questions := properties["questions"].(map[string]any)
	questionItems := questions["items"].(map[string]any)
	questionProperties := questionItems["properties"].(map[string]any)
	allowOther := questionProperties["allow_other"].(map[string]any)
	options := questionProperties["options"].(map[string]any)

	allowOtherDescription := allowOther["description"].(string)
	for _, marker := range []string{"Other", "Custom", "自定义", "Never add"} {
		if !strings.Contains(allowOtherDescription, marker) {
			t.Errorf("allow_other description missing %q: %q", marker, allowOtherDescription)
		}
	}
	optionsDescription := options["description"].(string)
	if !strings.Contains(optionsDescription, "concrete choices") ||
		!strings.Contains(optionsDescription, "use allow_other instead") {
		t.Errorf("options description does not exclude custom placeholders: %q", optionsDescription)
	}
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
	if !strings.Contains(res.Content, `"Deploy target?"=["Staging"]`) {
		t.Fatalf("content missing single-select answer: %q", res.Content)
	}
	// allow_other defaults to true when omitted.
	if len(asker.got.Questions) != 1 || !asker.got.Questions[0].AllowOther {
		t.Fatalf("allow_other should default true: %+v", asker.got.Questions)
	}
}

func TestAskUserQuestion_MultiSelectUsesUnambiguousJSONArray(t *testing.T) {
	asker := &stubAsker{result: agent.UIQuestionResult{
		Action:  agent.QuestionActionAnswer,
		Answers: []agent.UIQuestionAnswer{{Question: "Which languages?", Values: []string{"Go, templates", `Rust "stable"`}}},
	}}
	args := `{"questions":[{"question":"Which languages?","multi_select":true,"options":[{"label":"Go"},{"label":"Rust"},{"label":"Python"}]}]}`
	res := runAsk(t, asker, args)
	if !strings.Contains(res.Content, `"Which languages?"=["Go, templates","Rust \"stable\""]`) {
		t.Fatalf("multi-select values must be an unambiguous JSON array: %q", res.Content)
	}
}

func TestAskUserQuestion_MultipleQuestionsPreserveMetadataAndGroundedAnswers(t *testing.T) {
	asker := &stubAsker{result: agent.UIQuestionResult{
		Action: agent.QuestionActionAnswer,
		Answers: []agent.UIQuestionAnswer{
			{Question: "Deployment target?", Values: []string{"Staging (recommended)"}},
			{Question: "Required checks?", Values: []string{"Unit tests", "Race detector"}},
		},
	}}
	args := `{
		"auto_resolution_ms": 90000,
		"questions": [
			{
				"header": "Target",
				"question": "Deployment target?",
				"allow_other": false,
				"options": [
					{"label": "Staging (recommended)", "description": "Safe preview", "preview": "https://staging.example", "recommended": true},
					{"label": "Production", "description": "Public release"}
				]
			},
			{
				"header": "Checks",
				"question": "Required checks?",
				"multi_select": true,
				"options": [
					{"label": "Unit tests"},
					{"label": "Race detector"},
					{"label": "Manual QA"}
				]
			}
		]
	}`
	res := runAsk(t, asker, args)
	if res.IsError {
		t.Fatalf("unexpected error result: %q", res.Content)
	}
	for _, want := range []string{
		`"Deployment target?"=["Staging (recommended)"]`,
		`"Required checks?"=["Unit tests","Race detector"]`,
	} {
		if !strings.Contains(res.Content, want) {
			t.Errorf("grounded result missing %q: %q", want, res.Content)
		}
	}

	if asker.got.AutoResolutionMs != 90000 || len(asker.got.Questions) != 2 {
		t.Fatalf("request metadata not preserved: %+v", asker.got)
	}
	first := asker.got.Questions[0]
	if first.Header != "Target" || first.AllowOther || first.MultiSelect {
		t.Errorf("first question flags not preserved: %+v", first)
	}
	if len(first.Options) != 2 || first.Options[0].Label != "Staging (recommended)" ||
		first.Options[0].Description != "Safe preview" ||
		first.Options[0].Preview != "https://staging.example" ||
		!first.Options[0].Recommended {
		t.Errorf("first option metadata not preserved: %+v", first.Options)
	}
	second := asker.got.Questions[1]
	if !second.MultiSelect || !second.AllowOther {
		t.Errorf("second question defaults/flags not preserved: %+v", second)
	}
}

func TestAskUserQuestion_RejectsOversizedCardContent(t *testing.T) {
	validPrefix := `{"questions":[{"question":"Pick","options":[{"label":"A","preview":"`
	validSuffix := `"},{"label":"B"}]}]}`
	tooLongPreview := validPrefix + strings.Repeat("界", askUserQuestionMaxPreviewRunes+1) + validSuffix
	res := runAsk(t, &stubAsker{}, tooLongPreview)
	if !res.IsError || !strings.Contains(res.Content, "`preview` exceeds") {
		t.Fatalf("oversized preview must be rejected, got %+v", res)
	}

	oversizedPayload := strings.Repeat(" ", askUserQuestionMaxPayloadBytes+1)
	res = runAsk(t, &stubAsker{}, oversizedPayload)
	if !res.IsError || !strings.Contains(res.Content, "arguments exceed") {
		t.Fatalf("oversized payload must be rejected before decode, got %+v", res)
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

func TestAskUserQuestion_CancelIsCleanFallback(t *testing.T) {
	asker := &stubAsker{result: agent.UIQuestionResult{Action: agent.QuestionActionCancel}}
	args := `{"questions":[{"question":"Pick one","options":[{"label":"A"},{"label":"B"}]}]}`
	res := runAsk(t, asker, args)
	if res.IsError || !strings.Contains(res.Content, "Proceed using your best judgment") {
		t.Fatalf("cancel should be a clean best-judgment fallback: %+v", res)
	}
}

func TestAskUserQuestion_AnswerWithoutValuesDoesNotInventGrounding(t *testing.T) {
	asker := &stubAsker{result: agent.UIQuestionResult{Action: agent.QuestionActionAnswer}}
	args := `{"questions":[{"question":"Pick one","options":[{"label":"A"},{"label":"B"}]}]}`
	res := runAsk(t, asker, args)
	if res.IsError || !strings.Contains(res.Content, "no selection") {
		t.Fatalf("empty answer should not invent an option label: %+v", res)
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

func TestAskUserQuestion_ValidationBoundaries(t *testing.T) {
	tests := map[string]string{
		"too many questions": `{"questions":[
			{"question":"1","options":[{"label":"A"},{"label":"B"}]},
			{"question":"2","options":[{"label":"A"},{"label":"B"}]},
			{"question":"3","options":[{"label":"A"},{"label":"B"}]},
			{"question":"4","options":[{"label":"A"},{"label":"B"}]},
			{"question":"5","options":[{"label":"A"},{"label":"B"}]}
		]}`,
		"missing question text": `{"questions":[{"question":"  ","options":[{"label":"A"},{"label":"B"}]}]}`,
		"too many options": `{"questions":[{"question":"Pick","options":[
			{"label":"A"},{"label":"B"},{"label":"C"},{"label":"D"},{"label":"E"}
		]}]}`,
		"missing option label":   `{"questions":[{"question":"Pick","options":[{"label":"A"},{"label":"  "}]}]}`,
		"duplicate option label": `{"questions":[{"question":"Pick","options":[{"label":"A"},{"label":" A "}]}]}`,
	}
	for name, args := range tests {
		t.Run(name, func(t *testing.T) {
			res := runAsk(t, &stubAsker{}, args)
			if !res.IsError || !strings.HasPrefix(res.Content, "[validation error]") {
				t.Fatalf("expected validation error, got %+v", res)
			}
		})
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
