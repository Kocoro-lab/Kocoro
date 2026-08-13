package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
)

const generalOutcomeSchemaVersion = "kocoro.general_agent_outcomes.v1"

type generalOutcomeDataset struct {
	SchemaVersion string               `json:"schema_version"`
	Tasks         []generalOutcomeTask `json:"tasks"`
}

type generalOutcomeTask struct {
	ID           string               `json:"id"`
	Category     string               `json:"category"`
	Prompt       string               `json:"prompt"`
	Source       string               `json:"source,omitempty"`
	AllowedTools []string             `json:"allowed_tools"`
	InitialState generalOutcomeState  `json:"initial_state"`
	Oracle       generalOutcomeOracle `json:"oracle"`
}

type generalOutcomeState struct {
	Files    map[string]string                 `json:"files"`
	External map[string]generalOutcomeExternal `json:"external"`
	Effects  map[string]string                 `json:"effects"`
}

type generalOutcomeExternal struct {
	Status       string `json:"status"`
	Content      string `json:"content,omitempty"`
	Receipt      string `json:"receipt,omitempty"`
	SucceedAfter int    `json:"succeed_after,omitempty"`
}

type generalOutcomeOracle struct {
	ExpectedStatus string                       `json:"expected_status"`
	StatusMarkers  []string                     `json:"status_markers,omitempty"`
	Answer         generalOutcomeAnswerOracle   `json:"answer"`
	Evidence       generalOutcomeEvidenceOracle `json:"evidence"`
	State          generalOutcomeStateOracle    `json:"state"`
}

type generalOutcomeAnswerOracle struct {
	Exact       string     `json:"exact,omitempty"`
	ContainsAll []string   `json:"contains_all,omitempty"`
	ContainsAny [][]string `json:"contains_any,omitempty"`
	Forbidden   []string   `json:"forbidden,omitempty"`
	MaxChars    int        `json:"max_chars,omitempty"`
	MaxLines    int        `json:"max_lines,omitempty"`
}

type generalOutcomeEvidenceOracle struct {
	Calls             map[string]generalOutcomeCallRange `json:"calls"`
	RequiredReceipts  []string                           `json:"required_receipts"`
	NoUnexpectedCalls bool                               `json:"no_unexpected_calls"`
}

type generalOutcomeCallRange struct {
	Min int `json:"min"`
	Max int `json:"max"`
}

type generalOutcomeStateOracle struct {
	Unchanged     bool              `json:"unchanged"`
	FileChanges   map[string]string `json:"file_changes,omitempty"`
	EffectChanges map[string]string `json:"effect_changes,omitempty"`
}

type generalOutcomeObservation struct {
	Status   string
	Answer   string
	Calls    map[string]int
	Receipts []string
	State    generalOutcomeState
}

func loadGeneralOutcomeDataset(t *testing.T) generalOutcomeDataset {
	t.Helper()
	path := filepath.Join(repoRoot(), "test", "e2e", "testdata", "general_agent_outcomes.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read general-agent outcome dataset: %v", err)
	}
	var dataset generalOutcomeDataset
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&dataset); err != nil {
		t.Fatalf("decode general-agent outcome dataset: %v", err)
	}
	return dataset
}

func evaluateGeneralOutcome(task generalOutcomeTask, observed generalOutcomeObservation) []string {
	var failures []string
	if observed.Status != task.Oracle.ExpectedStatus {
		failures = append(failures, fmt.Sprintf("status:%s_want_%s", observed.Status, task.Oracle.ExpectedStatus))
	}
	answer := strings.TrimSpace(observed.Answer)
	if task.Oracle.Answer.Exact != "" && answer != task.Oracle.Answer.Exact {
		failures = append(failures, "answer_not_exact")
	}
	for _, required := range task.Oracle.Answer.ContainsAll {
		if !strings.Contains(answer, required) {
			failures = append(failures, "answer_missing:"+required)
		}
	}
	for index, alternatives := range task.Oracle.Answer.ContainsAny {
		matched := false
		for _, alternative := range alternatives {
			if strings.Contains(answer, alternative) {
				matched = true
				break
			}
		}
		if !matched {
			failures = append(failures, fmt.Sprintf("answer_missing_alternative_group:%d", index))
		}
	}
	lowerAnswer := strings.ToLower(answer)
	for _, forbidden := range task.Oracle.Answer.Forbidden {
		if strings.Contains(lowerAnswer, strings.ToLower(forbidden)) {
			failures = append(failures, "answer_forbidden:"+forbidden)
		}
	}
	if max := task.Oracle.Answer.MaxChars; max > 0 && utf8.RuneCountInString(answer) > max {
		failures = append(failures, "answer_too_long")
	}
	if max := task.Oracle.Answer.MaxLines; max > 0 && len(strings.Split(answer, "\n")) > max {
		failures = append(failures, "answer_too_many_lines")
	}
	for name, expected := range task.Oracle.Evidence.Calls {
		actual := observed.Calls[name]
		if actual < expected.Min || actual > expected.Max {
			failures = append(failures, fmt.Sprintf("call_count:%s:%d_not_%d_%d", name, actual, expected.Min, expected.Max))
		}
	}
	if task.Oracle.Evidence.NoUnexpectedCalls {
		for name, count := range observed.Calls {
			if count > 0 {
				if _, allowed := task.Oracle.Evidence.Calls[name]; !allowed {
					failures = append(failures, "unexpected_call:"+name)
				}
			}
		}
	}
	receipts := make(map[string]bool, len(observed.Receipts))
	for _, receipt := range observed.Receipts {
		receipts[receipt] = true
	}
	for _, required := range task.Oracle.Evidence.RequiredReceipts {
		if !receipts[required] {
			failures = append(failures, "missing_receipt:"+required)
		}
	}
	wantFiles := cloneGeneralOutcomeMap(task.InitialState.Files)
	wantEffects := cloneGeneralOutcomeMap(task.InitialState.Effects)
	for path, content := range task.Oracle.State.FileChanges {
		wantFiles[path] = content
	}
	for key, value := range task.Oracle.State.EffectChanges {
		wantEffects[key] = value
	}
	if !reflect.DeepEqual(normalizeGeneralOutcomeMap(observed.State.Files), normalizeGeneralOutcomeMap(wantFiles)) {
		failures = append(failures, "final_files_mismatch")
	}
	if !reflect.DeepEqual(normalizeGeneralOutcomeMap(observed.State.Effects), normalizeGeneralOutcomeMap(wantEffects)) {
		failures = append(failures, "final_effects_mismatch")
	}
	if !reflect.DeepEqual(normalizeGeneralOutcomeExternalMap(observed.State.External), normalizeGeneralOutcomeExternalMap(task.InitialState.External)) {
		failures = append(failures, "external_state_mutated")
	}
	return uniqueGeneralOutcomeFailures(failures)
}

func deriveGeneralOutcomeStatus(task generalOutcomeTask, observed generalOutcomeObservation) string {
	for _, effect := range observed.State.Effects {
		if effect == "outcome_unknown" {
			return "outcome_unknown"
		}
	}
	lower := strings.ToLower(observed.Answer)
	for _, marker := range task.Oracle.StatusMarkers {
		if strings.Contains(lower, strings.ToLower(marker)) {
			return "blocked"
		}
	}
	return "complete"
}

func validateGeneralOutcomeToolPath(root, argsJSON string) (agent.ToolResult, bool) {
	var args struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return agent.ValidationError(fmt.Sprintf("invalid arguments: %v", err)), false
	}
	if strings.TrimSpace(args.Path) == "" {
		return agent.ToolResult{}, true
	}
	target := args.Path
	if !filepath.IsAbs(target) {
		target = filepath.Join(root, target)
	}
	relative, err := filepath.Rel(root, filepath.Clean(target))
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return agent.PermissionError("outcome sandbox rejected path outside its temporary root"), false
	}
	return agent.ToolResult{}, true
}

func TestOffline_GeneralAgentOutcomeDatasetContract(t *testing.T) {
	dataset := loadGeneralOutcomeDataset(t)
	if dataset.SchemaVersion != generalOutcomeSchemaVersion {
		t.Fatalf("schema_version=%q, want %q", dataset.SchemaVersion, generalOutcomeSchemaVersion)
	}
	if len(dataset.Tasks) != 24 {
		t.Fatalf("task count=%d, want 24", len(dataset.Tasks))
	}
	requiredCategories := map[string]bool{
		"writing": false, "extraction_planning": false, "file_ops": false,
		"research_honesty": false, "clarification": false,
		"error_recovery": false, "everyday_voice": false,
	}
	knownTools := map[string]bool{
		"file_read": true, "grep": true, "file_write": true, "file_edit": true,
		"web_fetch": true, "calendar_create": true, "send_email": true,
	}
	seen := map[string]bool{}
	for _, task := range dataset.Tasks {
		if task.ID == "" || seen[task.ID] {
			t.Errorf("task has empty or duplicate id %q", task.ID)
		}
		seen[task.ID] = true
		if _, ok := requiredCategories[task.Category]; !ok {
			t.Errorf("task %s has unknown category %q", task.ID, task.Category)
		} else {
			requiredCategories[task.Category] = true
		}
		if strings.TrimSpace(task.Prompt) == "" {
			t.Errorf("task %s has empty prompt", task.ID)
		}
		if task.Oracle.ExpectedStatus != "complete" && task.Oracle.ExpectedStatus != "blocked" && task.Oracle.ExpectedStatus != "outcome_unknown" {
			t.Errorf("task %s has invalid expected_status %q", task.ID, task.Oracle.ExpectedStatus)
		}
		if task.Oracle.ExpectedStatus == "blocked" && len(task.Oracle.StatusMarkers) == 0 {
			t.Errorf("blocked task %s has no status_markers", task.ID)
		}
		if task.Oracle.ExpectedStatus != "blocked" && len(task.Oracle.StatusMarkers) != 0 {
			t.Errorf("non-blocked task %s declares status_markers", task.ID)
		}
		answer := task.Oracle.Answer
		if answer.Exact == "" && len(answer.ContainsAll) == 0 && len(answer.ContainsAny) == 0 {
			t.Errorf("task %s has no positive answer oracle", task.ID)
		}
		if !task.Oracle.Evidence.NoUnexpectedCalls {
			t.Errorf("task %s must fail closed on unexpected calls", task.ID)
		}
		for _, tool := range task.AllowedTools {
			if !knownTools[tool] {
				t.Errorf("task %s allows unknown tool %q", task.ID, tool)
			}
		}
		allowed := make(map[string]bool, len(task.AllowedTools))
		for _, tool := range task.AllowedTools {
			allowed[tool] = true
		}
		for tool, calls := range task.Oracle.Evidence.Calls {
			if !allowed[tool] {
				t.Errorf("task %s expects calls to unregistered tool %q", task.ID, tool)
			}
			if calls.Min < 0 || calls.Max < calls.Min {
				t.Errorf("task %s has invalid call range for %s: %+v", task.ID, tool, calls)
			}
		}
		stateOracle := task.Oracle.State
		if stateOracle.Unchanged && (len(stateOracle.FileChanges) > 0 || len(stateOracle.EffectChanges) > 0) {
			t.Errorf("task %s marks state unchanged but declares changes", task.ID)
		}
		if !stateOracle.Unchanged && len(stateOracle.FileChanges) == 0 && len(stateOracle.EffectChanges) == 0 {
			t.Errorf("task %s has no final-state oracle", task.ID)
		}
		for key, external := range task.InitialState.External {
			switch external.Status {
			case "ok", "blocked", "not_found", "transient", "business_error", "outcome_unknown":
			default:
				t.Errorf("task %s external %s has invalid status %q", task.ID, key, external.Status)
			}
		}
	}
	for category, present := range requiredCategories {
		if !present {
			t.Errorf("dataset missing category %q", category)
		}
	}
}

func TestOffline_GeneralAgentOutcomeStatusDerivation(t *testing.T) {
	tests := []struct {
		name     string
		task     generalOutcomeTask
		observed generalOutcomeObservation
		want     string
	}{
		{
			name:     "complete_conflict_answer_may_say_cannot",
			task:     generalOutcomeTask{Oracle: generalOutcomeOracle{ExpectedStatus: "complete"}},
			observed: generalOutcomeObservation{Answer: "The sources disagree, so I cannot give one value."},
			want:     "complete",
		},
		{
			name:     "task_scoped_blocked_marker",
			task:     generalOutcomeTask{Oracle: generalOutcomeOracle{ExpectedStatus: "blocked", StatusMarkers: []string{"timezone", "?"}}},
			observed: generalOutcomeObservation{Answer: "Which timezone should 10:00 use?"},
			want:     "blocked",
		},
		{
			name:     "unrelated_question_mark_does_not_block_complete_task",
			task:     generalOutcomeTask{Oracle: generalOutcomeOracle{ExpectedStatus: "complete"}},
			observed: generalOutcomeObservation{Answer: "Saved brief.txt. Anything else?"},
			want:     "complete",
		},
		{
			name:     "outcome_unknown_effect_wins",
			task:     generalOutcomeTask{Oracle: generalOutcomeOracle{ExpectedStatus: "outcome_unknown"}},
			observed: generalOutcomeObservation{State: generalOutcomeState{Effects: map[string]string{"email": "outcome_unknown"}}},
			want:     "outcome_unknown",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := deriveGeneralOutcomeStatus(tt.task, tt.observed); got != tt.want {
				t.Fatalf("status = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestOffline_GeneralAgentOutcomeSandboxRejectsEscapingPaths(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(filepath.Dir(root), "outside.txt")
	for _, path := range []string{"../escape.txt", outside} {
		args, err := json.Marshal(map[string]string{"path": path})
		if err != nil {
			t.Fatal(err)
		}
		result, valid := validateGeneralOutcomeToolPath(root, string(args))
		if valid || !result.IsError || result.ErrorCategory != agent.ErrCategoryPermission {
			t.Fatalf("path %q was not rejected fail-closed: valid=%t result=%+v", path, valid, result)
		}
	}
	inside, err := json.Marshal(map[string]string{"path": "nested/inside.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if result, valid := validateGeneralOutcomeToolPath(root, string(inside)); !valid || result.IsError {
		t.Fatalf("inside path rejected: valid=%t result=%+v", valid, result)
	}
}

func TestOffline_GeneralAgentOutcomeOracleFailsClosed(t *testing.T) {
	task := generalOutcomeTask{
		InitialState: generalOutcomeState{
			Files:    map[string]string{"input.txt": "seed"},
			External: map[string]generalOutcomeExternal{"source": {Status: "ok", Content: "evidence"}},
			Effects:  map[string]string{},
		},
		Oracle: generalOutcomeOracle{
			ExpectedStatus: "complete",
			Answer:         generalOutcomeAnswerOracle{Exact: "done"},
			Evidence: generalOutcomeEvidenceOracle{
				Calls:            map[string]generalOutcomeCallRange{"file_write": {Min: 1, Max: 1}},
				RequiredReceipts: []string{"receipt-1"}, NoUnexpectedCalls: true,
			},
			State: generalOutcomeStateOracle{FileChanges: map[string]string{"output.txt": "result"}},
		},
	}
	passing := generalOutcomeObservation{
		Status: "complete", Answer: "done", Calls: map[string]int{"file_write": 1}, Receipts: []string{"receipt-1"},
		State: generalOutcomeState{
			Files:    map[string]string{"input.txt": "seed", "output.txt": "result"},
			External: map[string]generalOutcomeExternal{"source": {Status: "ok", Content: "evidence"}},
			Effects:  map[string]string{},
		},
	}
	if failures := evaluateGeneralOutcome(task, passing); len(failures) != 0 {
		t.Fatalf("passing fixture failed: %v", failures)
	}
	mutations := []struct {
		name   string
		mutate func(*generalOutcomeObservation)
	}{
		{name: "status", mutate: func(o *generalOutcomeObservation) { o.Status = "blocked" }},
		{name: "answer", mutate: func(o *generalOutcomeObservation) { o.Answer = "maybe" }},
		{name: "evidence", mutate: func(o *generalOutcomeObservation) { o.Calls["file_write"] = 0 }},
		{name: "receipt", mutate: func(o *generalOutcomeObservation) { o.Receipts = nil }},
		{name: "state", mutate: func(o *generalOutcomeObservation) { o.State.Files["output.txt"] = "wrong" }},
		{name: "external_state", mutate: func(o *generalOutcomeObservation) {
			o.State.External["source"] = generalOutcomeExternal{Status: "ok", Content: "changed"}
		}},
		{name: "unexpected_call", mutate: func(o *generalOutcomeObservation) { o.Calls["send_email"] = 1 }},
	}
	for _, tc := range mutations {
		t.Run(tc.name, func(t *testing.T) {
			observed := cloneGeneralOutcomeObservation(passing)
			tc.mutate(&observed)
			if failures := evaluateGeneralOutcome(task, observed); len(failures) == 0 {
				t.Fatal("invalid observation unexpectedly passed")
			}
		})
	}
}

type generalOutcomeQualificationRun struct {
	Correct       bool
	UsageObserved bool
	CostObserved  bool
}

func generalOutcomeQualification(sample string, repetitions, scheduled int, reportedCost, maxCost float64, coverageComplete bool, runs []generalOutcomeQualificationRun) (comparison, release bool) {
	complete := scheduled > 0 && len(runs) == scheduled && coverageComplete
	allCorrect := complete
	usageObserved := complete
	costObserved := complete
	for _, run := range runs {
		allCorrect = allCorrect && run.Correct
		usageObserved = usageObserved && run.UsageObserved
		costObserved = costObserved && run.CostObserved
	}
	withinCostCap := maxCost > 0 && reportedCost <= maxCost
	comparison = allCorrect && usageObserved && costObserved && withinCostCap && repetitions >= 1
	release = sample == "release" && allCorrect && usageObserved && costObserved && withinCostCap && repetitions >= 5
	return comparison, release
}

func TestOffline_GeneralAgentOutcomeQualificationFailsClosed(t *testing.T) {
	passing := func(count int) []generalOutcomeQualificationRun {
		runs := make([]generalOutcomeQualificationRun, count)
		for index := range runs {
			runs[index] = generalOutcomeQualificationRun{Correct: true, UsageObserved: true, CostObserved: true}
		}
		return runs
	}
	if comparison, release := generalOutcomeQualification("comparison", 1, 24, 1, 5, true, passing(24)); !comparison || release {
		t.Fatalf("comparison qualification=(%t,%t), want (true,false)", comparison, release)
	}
	if _, release := generalOutcomeQualification("release", 4, 96, 1, 5, true, passing(96)); release {
		t.Fatal("four repetitions unexpectedly release-qualified")
	}
	if comparison, release := generalOutcomeQualification("release", 5, 120, 1, 5, true, passing(120)); !comparison || !release {
		t.Fatalf("release qualification=(%t,%t), want (true,true)", comparison, release)
	}
	tests := []struct {
		name   string
		mutate func([]generalOutcomeQualificationRun) []generalOutcomeQualificationRun
	}{
		{name: "incomplete", mutate: func(runs []generalOutcomeQualificationRun) []generalOutcomeQualificationRun { return runs[:119] }},
		{name: "incorrect", mutate: func(runs []generalOutcomeQualificationRun) []generalOutcomeQualificationRun {
			runs[0].Correct = false
			return runs
		}},
		{name: "missing_usage", mutate: func(runs []generalOutcomeQualificationRun) []generalOutcomeQualificationRun {
			runs[0].UsageObserved = false
			return runs
		}},
		{name: "missing_cost", mutate: func(runs []generalOutcomeQualificationRun) []generalOutcomeQualificationRun {
			runs[0].CostObserved = false
			return runs
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, release := generalOutcomeQualification("release", 5, 120, 1, 5, true, tc.mutate(passing(120))); release {
				t.Fatal("invalid report unexpectedly release-qualified")
			}
		})
	}
	if _, release := generalOutcomeQualification("release", 5, 120, 6, 5, true, passing(120)); release {
		t.Fatal("over-budget report unexpectedly release-qualified")
	}
	if comparison, _ := generalOutcomeQualification("comparison", 1, 24, 1, 5, false, passing(24)); comparison {
		t.Fatal("incomplete task/repetition coverage unexpectedly comparison-qualified")
	}
	missingCost := passing(24)
	missingCost[0].CostObserved = false
	if comparison, _ := generalOutcomeQualification("comparison", 1, 24, 1, 5, true, missingCost); comparison {
		t.Fatal("comparison without cost observation unexpectedly qualified")
	}
}

func cloneGeneralOutcomeObservation(value generalOutcomeObservation) generalOutcomeObservation {
	value.Calls = cloneGeneralOutcomeIntMap(value.Calls)
	value.Receipts = append([]string(nil), value.Receipts...)
	value.State.Files = cloneGeneralOutcomeMap(value.State.Files)
	value.State.External = cloneGeneralOutcomeExternalMap(value.State.External)
	value.State.Effects = cloneGeneralOutcomeMap(value.State.Effects)
	return value
}

func cloneGeneralOutcomeMap(input map[string]string) map[string]string {
	output := make(map[string]string, len(input))
	for key, value := range input {
		output[key] = value
	}
	return output
}

func cloneGeneralOutcomeIntMap(input map[string]int) map[string]int {
	output := make(map[string]int, len(input))
	for key, value := range input {
		output[key] = value
	}
	return output
}

func cloneGeneralOutcomeExternalMap(input map[string]generalOutcomeExternal) map[string]generalOutcomeExternal {
	output := make(map[string]generalOutcomeExternal, len(input))
	for key, value := range input {
		output[key] = value
	}
	return output
}

func normalizeGeneralOutcomeMap(input map[string]string) map[string]string {
	if len(input) == 0 {
		return map[string]string{}
	}
	return input
}

func normalizeGeneralOutcomeExternalMap(input map[string]generalOutcomeExternal) map[string]generalOutcomeExternal {
	if len(input) == 0 {
		return map[string]generalOutcomeExternal{}
	}
	return input
}

func uniqueGeneralOutcomeFailures(input []string) []string {
	seen := make(map[string]bool, len(input))
	output := make([]string, 0, len(input))
	for _, value := range input {
		if value != "" && !seen[value] {
			seen[value] = true
			output = append(output, value)
		}
	}
	sort.Strings(output)
	return output
}
