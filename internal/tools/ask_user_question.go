package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
)

// ask_user_question bounds keep a model-authored card small enough for the
// Desktop UI and the daemon's replay ring. The current workload is at most four
// compact questions with four short choices each; accepting unbounded previews
// caused one call to retain arbitrarily large payloads in both the pending
// broker and event bus. If richer cards become a product requirement, raise
// these constants and their JSON-schema maxLength values together, then update
// the Desktop render/round-trip fixtures.
const (
	askUserQuestionMinQuestions        = 1
	askUserQuestionMaxQuestions        = 4
	askUserQuestionMinOptions          = 2
	askUserQuestionMaxOptions          = 4
	askUserQuestionMaxPayloadBytes     = 32 * 1024
	askUserQuestionMaxHeaderRunes      = 80
	askUserQuestionMaxQuestionRunes    = 1000
	askUserQuestionMaxLabelRunes       = 256
	askUserQuestionMaxDescriptionRunes = 1000
	askUserQuestionMaxPreviewRunes     = 4096
)

// AskUserQuestionTool presents the user a small set of closed-choice questions
// and blocks until they pick (or the surface can't ask). The model receives the
// full chosen option labels, never a bare token — killing the grounding bug
// where a UI returns "1"/"opt_a" and the model has to guess what it meant.
//
// It is NOT an approval: RequiresApproval() is false. It reaches the daemon's
// QuestionBroker through an agent.QuestionAsker injected on the call context
// (internal/tools cannot import internal/daemon), mirroring how approvals
// decouple the loop from the broker.
type AskUserQuestionTool struct{}

type askUserQuestionArgs struct {
	Questions        questionList `json:"questions"`
	AutoResolutionMs int          `json:"auto_resolution_ms,omitempty"`
}

type askUserQuestionQuestion struct {
	Header      string     `json:"header,omitempty"`
	Question    string     `json:"question"`
	MultiSelect bool       `json:"multi_select,omitempty"`
	AllowOther  *bool      `json:"allow_other,omitempty"`
	Options     optionList `json:"options"`
}

type askUserQuestionOption struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
	Preview     string `json:"preview,omitempty"`
	Recommended bool   `json:"recommended,omitempty"`
}

// questionList / optionList tolerate a model that double-encodes a nested
// array argument as a JSON string — e.g. "questions":"[{...}]" instead of
// "questions":[{...}]. This is a real, intermittent tool-calling quirk (seen
// live: the same prompt produced a proper array on one call and a stringified
// array on the next, which hard-failed into a 5x [validation error] retry
// loop). Unwrapping one layer of string encoding before decoding lets the
// recoverable call land the card instead of stalling the turn.
type questionList []askUserQuestionQuestion

func (ql *questionList) UnmarshalJSON(data []byte) error {
	var arr []askUserQuestionQuestion
	if err := unmarshalMaybeStringified(data, &arr); err != nil {
		return err
	}
	*ql = arr
	return nil
}

type optionList []askUserQuestionOption

func (ol *optionList) UnmarshalJSON(data []byte) error {
	var arr []askUserQuestionOption
	if err := unmarshalMaybeStringified(data, &arr); err != nil {
		return err
	}
	*ol = arr
	return nil
}

// unmarshalMaybeStringified unmarshals data into v, first peeling one layer of
// JSON-string wrapping if the model quoted the whole value.
func unmarshalMaybeStringified(data []byte, v any) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) > 0 && trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return err
		}
		data = []byte(s)
	}
	return json.Unmarshal(data, v)
}

func (t *AskUserQuestionTool) Info() agent.ToolInfo {
	return agent.ToolInfo{
		Name: "ask_user_question",
		Description: "Ask the user multiple-choice questions to gather preferences, clarify consequential ambiguity, or make decisions that require their input. " +
			"Presents 1-4 closed-choice questions (2-4 options each) as a selection UI; you receive the full chosen labels back. " +
			"Use only after determining that an answer is necessary. Investigate discoverable facts first and prefer a reasonable assumption for low-impact ambiguity. " +
			"Call this tool only when the current Context contains the exact line `Structured question UI: available`; otherwise ask the necessary question in prose. " +
			"When the UI is available and a necessary question has 2-4 concrete options, call this tool in the same response; do not restate the choices or merely say you are waiting in prose.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"questions": map[string]any{
					"type":        "array",
					"minItems":    askUserQuestionMinQuestions,
					"maxItems":    askUserQuestionMaxQuestions,
					"description": "1-4 questions to ask together.",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"header": map[string]any{
								"type":        "string",
								"maxLength":   askUserQuestionMaxHeaderRunes,
								"description": "Short chip label shown above the question (≤12 chars recommended), e.g. \"Auth method\".",
							},
							"question": map[string]any{
								"type":        "string",
								"maxLength":   askUserQuestionMaxQuestionRunes,
								"description": "The question text shown to the user.",
							},
							"multi_select": map[string]any{
								"type":        "boolean",
								"description": "Allow selecting more than one option. Default false (single choice).",
							},
							"allow_other": map[string]any{
								"type":        "boolean",
								"description": "Let the user type one free-text answer; the client adds the control. Default true. Never add an option named Other, Custom, 自定义, or any equivalent free-form placeholder.",
							},
							"options": map[string]any{
								"type":        "array",
								"minItems":    askUserQuestionMinOptions,
								"maxItems":    askUserQuestionMaxOptions,
								"description": "2-4 concrete choices. Order the recommended one first. Do not include a Custom/Other/free-form placeholder; use allow_other instead.",
								"items": map[string]any{
									"type": "object",
									"properties": map[string]any{
										"label": map[string]any{
											"type":        "string",
											"maxLength":   askUserQuestionMaxLabelRunes,
											"description": "The choice text the user sees and you receive back.",
										},
										"description": map[string]any{
											"type":        "string",
											"maxLength":   askUserQuestionMaxDescriptionRunes,
											"description": "Short explanation of what picking this option means.",
										},
										"preview": map[string]any{
											"type":        "string",
											"maxLength":   askUserQuestionMaxPreviewRunes,
											"description": "Optional supplementary content (mockup/snippet) shown when focused. Single-select only.",
										},
										"recommended": map[string]any{
											"type":        "boolean",
											"description": "Mark this option as your suggested default.",
										},
									},
									"required": []string{"label"},
								},
							},
						},
						"required": []string{"question", "options"},
					},
				},
				"auto_resolution_ms": map[string]any{
					"type":        "integer",
					"description": "Optional auto-decline window in milliseconds (clamped to 60000-240000). Omit for the default timeout.",
				},
			},
			"required": []string{"questions"},
		},
	}
}

// RequiresApproval is false: ask_user_question is its own interaction kind, not
// a tool approval.
func (t *AskUserQuestionTool) RequiresApproval() bool { return false }

// IsReadOnlyCall reports false: the call blocks on user input and mutates
// nothing on disk, but it is a serial, side-effecting interaction (a UI
// prompt), so it must not batch concurrently with other calls.
func (t *AskUserQuestionTool) IsReadOnlyCall(string) bool { return false }

func (t *AskUserQuestionTool) Run(ctx context.Context, argsJSON string) (agent.ToolResult, error) {
	if len(argsJSON) > askUserQuestionMaxPayloadBytes {
		return agent.ValidationError(fmt.Sprintf(
			"ask_user_question: arguments exceed the %d-byte card limit.",
			askUserQuestionMaxPayloadBytes,
		)), nil
	}
	var args askUserQuestionArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return agent.ValidationError(fmt.Sprintf("ask_user_question: invalid arguments: %v", err)), nil
	}

	// Required-field validation (see CLAUDE.md "Tool Required-Field Validation").
	// json.Unmarshal cannot tell "missing" from "zero", so check explicitly and
	// keep the [validation error] prefix for the loop detector's early stop.
	if len(args.Questions) == 0 {
		return agent.ValidationError("ask_user_question: `questions` is required and must contain at least one question."), nil
	}
	if len(args.Questions) > askUserQuestionMaxQuestions {
		return agent.ValidationError(fmt.Sprintf("ask_user_question: at most %d questions per call, got %d.", askUserQuestionMaxQuestions, len(args.Questions))), nil
	}
	for i, q := range args.Questions {
		if utf8.RuneCountInString(q.Header) > askUserQuestionMaxHeaderRunes {
			return agent.ValidationError(fmt.Sprintf("ask_user_question: question %d `header` exceeds %d characters.", i, askUserQuestionMaxHeaderRunes)), nil
		}
		if strings.TrimSpace(q.Question) == "" {
			return agent.ValidationError(fmt.Sprintf("ask_user_question: question %d is missing the required `question` text.", i)), nil
		}
		if utf8.RuneCountInString(q.Question) > askUserQuestionMaxQuestionRunes {
			return agent.ValidationError(fmt.Sprintf("ask_user_question: question %d `question` exceeds %d characters.", i, askUserQuestionMaxQuestionRunes)), nil
		}
		if len(q.Options) < askUserQuestionMinOptions {
			return agent.ValidationError(fmt.Sprintf("ask_user_question: question %d needs at least %d options, got %d.", i, askUserQuestionMinOptions, len(q.Options))), nil
		}
		if len(q.Options) > askUserQuestionMaxOptions {
			return agent.ValidationError(fmt.Sprintf("ask_user_question: question %d allows at most %d options, got %d.", i, askUserQuestionMaxOptions, len(q.Options))), nil
		}
		seenLabels := make(map[string]bool, len(q.Options))
		for j, o := range q.Options {
			if strings.TrimSpace(o.Label) == "" {
				return agent.ValidationError(fmt.Sprintf("ask_user_question: question %d option %d is missing the required `label`.", i, j)), nil
			}
			if utf8.RuneCountInString(o.Label) > askUserQuestionMaxLabelRunes {
				return agent.ValidationError(fmt.Sprintf("ask_user_question: question %d option %d `label` exceeds %d characters.", i, j, askUserQuestionMaxLabelRunes)), nil
			}
			if utf8.RuneCountInString(o.Description) > askUserQuestionMaxDescriptionRunes {
				return agent.ValidationError(fmt.Sprintf("ask_user_question: question %d option %d `description` exceeds %d characters.", i, j, askUserQuestionMaxDescriptionRunes)), nil
			}
			if utf8.RuneCountInString(o.Preview) > askUserQuestionMaxPreviewRunes {
				return agent.ValidationError(fmt.Sprintf("ask_user_question: question %d option %d `preview` exceeds %d characters.", i, j, askUserQuestionMaxPreviewRunes)), nil
			}
			label := strings.TrimSpace(o.Label)
			if seenLabels[label] {
				return agent.ValidationError(fmt.Sprintf("ask_user_question: question %d contains duplicate option label %q.", i, label)), nil
			}
			seenLabels[label] = true
		}
	}

	asker := agent.QuestionAskerFrom(ctx)
	if asker == nil {
		// No interactive surface can render a selection UI (unattended run,
		// synchronous HTTP, non-interactive channel, TUI/one-shot, no injected
		// asker). Return a clean non-error result so the model proceeds on its
		// own judgment instead of stalling on an impossible prompt.
		return agent.ToolResult{
			Content: "The current channel cannot present an interactive question to the user. Proceed using your best judgment and state any assumption you made.",
		}, nil
	}

	ureq := agent.UIQuestionRequest{AutoResolutionMs: args.AutoResolutionMs}
	for _, q := range args.Questions {
		allowOther := true // schema default is true; the client injects "Other".
		if q.AllowOther != nil {
			allowOther = *q.AllowOther
		}
		uq := agent.UIQuestion{
			Header:      q.Header,
			Question:    q.Question,
			MultiSelect: q.MultiSelect,
			AllowOther:  allowOther,
		}
		for _, o := range q.Options {
			uq.Options = append(uq.Options, agent.UIQuestionOption{
				Label:       o.Label,
				Description: o.Description,
				Preview:     o.Preview,
				Recommended: o.Recommended,
			})
		}
		ureq.Questions = append(ureq.Questions, uq)
	}

	res := asker.AskUserQuestion(ctx, ureq)
	switch res.Action {
	case agent.QuestionActionAnswer:
		return agent.ToolResult{Content: formatQuestionAnswers(res.Answers)}, nil
	default:
		// decline / cancel: a deliberate skip or a dismissed/timed-out card.
		// Not an error — the model should continue with a sensible default.
		return agent.ToolResult{
			Content: "User declined to answer. Proceed using your best judgment and state any assumption you made.",
		}, nil
	}
}

// formatQuestionAnswers renders the user's selections as
// <JSON question>=<JSON value array>; ... so commas and quotes inside labels
// remain unambiguous while the model still reads full labels bound to their
// question, never a bare index or token.
func formatQuestionAnswers(answers []agent.UIQuestionAnswer) string {
	if len(answers) == 0 {
		return "User submitted the question with no selection. Proceed using your best judgment."
	}
	parts := make([]string, 0, len(answers))
	for _, a := range answers {
		questionJSON, _ := json.Marshal(a.Question)
		valuesJSON, _ := json.Marshal(a.Values)
		parts = append(parts, string(questionJSON)+"="+string(valuesJSON))
	}
	return "The user answered: " + strings.Join(parts, "; ") + ". Continue with these choices."
}
