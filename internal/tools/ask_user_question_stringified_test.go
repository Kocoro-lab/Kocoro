package tools

import (
	"encoding/json"
	"testing"
)

// TestAskUserQuestionToleratesStringifiedArgs guards the recovery from a model
// that double-encodes a nested array argument as a JSON string. A live Desktop
// run produced "questions":"[{...}]" (a stringified array) and the tool
// hard-failed into a 5x [validation error] retry loop; the tolerant decode must
// unwrap it so the card still lands.
func TestAskUserQuestionToleratesStringifiedArgs(t *testing.T) {
	cases := map[string]string{
		"questions stringified": `{"questions":"[{\"header\":\"色\",\"question\":\"Pick\",\"options\":[{\"label\":\"Red\"},{\"label\":\"Blue\"}]}]"}`,
		"options stringified":    `{"questions":[{"question":"Pick","options":"[{\"label\":\"Red\"},{\"label\":\"Blue\"}]"}]}`,
		"normal array":           `{"questions":[{"question":"Pick","options":[{"label":"Red"},{"label":"Blue"}]}]}`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			var args askUserQuestionArgs
			if err := json.Unmarshal([]byte(raw), &args); err != nil {
				t.Fatalf("unmarshal failed (should tolerate): %v", err)
			}
			if len(args.Questions) != 1 {
				t.Fatalf("got %d questions, want 1", len(args.Questions))
			}
			q := args.Questions[0]
			if q.Question != "Pick" {
				t.Errorf("question = %q, want Pick", q.Question)
			}
			if len(q.Options) != 2 || q.Options[0].Label != "Red" || q.Options[1].Label != "Blue" {
				t.Fatalf("options = %+v, want [Red Blue]", q.Options)
			}
		})
	}
}
