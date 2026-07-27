package daemon

import "testing"

func TestValidateQuestionResponse(t *testing.T) {
	questions := []Question{
		{
			ID:         "q0",
			Question:   "Target?",
			AllowOther: false,
			Options:    []QuestionOption{{Label: "Staging"}, {Label: "Production"}},
		},
		{
			ID:          "q1",
			Question:    "Checks?",
			MultiSelect: true,
			AllowOther:  true,
			Options:     []QuestionOption{{Label: "Unit"}, {Label: "Race"}},
		},
	}

	valid := map[string]QuestionResponse{
		"offered labels": {
			Action: QuestionActionAnswer,
			Answers: []QuestionAnswer{
				{ID: "q0", Values: []string{"Staging"}},
				{ID: "q1", Values: []string{"Unit", "Race"}},
			},
		},
		"one custom other value": {
			Action: QuestionActionAnswer,
			Answers: []QuestionAnswer{
				{ID: "q0", Values: []string{"Production"}},
				{ID: "q1", Values: []string{"Unit", "Integration"}},
			},
		},
		"decline without answers": {Action: QuestionActionDecline},
	}
	for name, response := range valid {
		t.Run("valid "+name, func(t *testing.T) {
			if err := validateQuestionResponse(questions, response); err != nil {
				t.Fatalf("unexpected validation error: %v", err)
			}
		})
	}

	invalid := map[string]QuestionResponse{
		"missing one question": {
			Action:  QuestionActionAnswer,
			Answers: []QuestionAnswer{{ID: "q0", Values: []string{"Staging"}}},
		},
		"unknown question id": {
			Action: QuestionActionAnswer,
			Answers: []QuestionAnswer{
				{ID: "q0", Values: []string{"Staging"}},
				{ID: "q9", Values: []string{"Unit"}},
			},
		},
		"duplicate question id": {
			Action: QuestionActionAnswer,
			Answers: []QuestionAnswer{
				{ID: "q0", Values: []string{"Staging"}},
				{ID: "q0", Values: []string{"Production"}},
			},
		},
		"bare token for closed question": {
			Action: QuestionActionAnswer,
			Answers: []QuestionAnswer{
				{ID: "q0", Values: []string{"opt_a"}},
				{ID: "q1", Values: []string{"Unit"}},
			},
		},
		"single select has two values": {
			Action: QuestionActionAnswer,
			Answers: []QuestionAnswer{
				{ID: "q0", Values: []string{"Staging", "Production"}},
				{ID: "q1", Values: []string{"Unit"}},
			},
		},
		"multi select has no values": {
			Action: QuestionActionAnswer,
			Answers: []QuestionAnswer{
				{ID: "q0", Values: []string{"Staging"}},
				{ID: "q1", Values: nil},
			},
		},
		"duplicate value": {
			Action: QuestionActionAnswer,
			Answers: []QuestionAnswer{
				{ID: "q0", Values: []string{"Staging"}},
				{ID: "q1", Values: []string{"Unit", "Unit"}},
			},
		},
		"more than one custom value": {
			Action: QuestionActionAnswer,
			Answers: []QuestionAnswer{
				{ID: "q0", Values: []string{"Staging"}},
				{ID: "q1", Values: []string{"Integration", "Snapshot"}},
			},
		},
		"empty value": {
			Action: QuestionActionAnswer,
			Answers: []QuestionAnswer{
				{ID: "q0", Values: []string{"Staging"}},
				{ID: "q1", Values: []string{"  "}},
			},
		},
		"decline includes answers": {
			Action:  QuestionActionDecline,
			Answers: []QuestionAnswer{{ID: "q0", Values: []string{"Staging"}}},
		},
	}
	for name, response := range invalid {
		t.Run("invalid "+name, func(t *testing.T) {
			if err := validateQuestionResponse(questions, response); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}
