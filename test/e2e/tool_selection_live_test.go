//go:build live

package e2e

import (
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	toolChoiceGateEnv        = "KOCORO_TOOL_CHOICE_LIVE"
	toolChoiceSampleEnv      = "KOCORO_TOOL_CHOICE_SAMPLE"
	toolChoiceRepetitionsEnv = "KOCORO_TOOL_CHOICE_REPETITIONS"
	toolChoiceSeedEnv        = "KOCORO_TOOL_CHOICE_SEED"
	toolChoiceOutputEnv      = "KOCORO_TOOL_CHOICE_OUTPUT"
	toolChoiceMaxCostEnv     = "KOCORO_TOOL_CHOICE_MAX_COST_USD"

	toolChoiceComparisonRepetitions = 3
	toolChoiceReleaseRepetitions    = 5
	toolChoiceDefaultSeed           = int64(20260813)
)

type toolChoiceConfig struct {
	sample      string
	repetitions int
	seed        int64
	outputPath  string
	maxCostUSD  float64
}

type toolChoiceFixture struct {
	root        string
	exactFile   string
	searchToken string
}

type toolChoiceCase struct {
	name           string
	prompt         func(toolChoiceFixture, int) string
	requiredTools  []string
	forbiddenTools []string
	answerContains []string
	maxToolCalls   int
	argumentOracle func(toolChoiceFixture, int, []liveToolFrame) error
	stateOracle    func(toolChoiceFixture, int) error
}

type toolChoiceJob struct {
	caseIndex  int
	repetition int
}

type toolChoiceRun struct {
	Case          string         `json:"case"`
	Repetition    int            `json:"repetition"`
	ScheduleIndex int            `json:"schedule_index"`
	Correct       bool           `json:"correct"`
	Failures      []string       `json:"failures"`
	ToolCounts    map[string]int `json:"tool_counts"`
	ToolFlow      []string       `json:"tool_flow"`
	LatencyMillis int64          `json:"latency_millis"`
	CostUSD       float64        `json:"cost_usd"`
	CostObserved  bool           `json:"cost_observed"`
	Answer        string         `json:"answer"`
}

type toolChoiceReport struct {
	SchemaVersion        string          `json:"schema_version"`
	GeneratedAt          string          `json:"generated_at"`
	Complete             bool            `json:"complete"`
	Sample               string          `json:"sample"`
	RepetitionsPerCase   int             `json:"repetitions_per_case"`
	Seed                 int64           `json:"seed"`
	Scheduled            int             `json:"scheduled"`
	Completed            int             `json:"completed"`
	CorrectRuns          int             `json:"correct_runs"`
	ComparisonQualifying bool            `json:"comparison_qualifying"`
	ReleaseQualifying    bool            `json:"release_qualifying"`
	ReportedCostUSD      float64         `json:"reported_cost_usd"`
	MaxCostUSD           float64         `json:"max_cost_usd"`
	CostObserved         bool            `json:"cost_observed"`
	Runs                 []toolChoiceRun `json:"runs"`
}

// TestLive_ToolChoiceMatrix qualifies whether the production tool surface leads
// the model to the narrowest suitable tool and a verified user outcome. Unlike
// a tool-call smoke test, each cell checks the answer and, for writes, the
// resulting filesystem state. The fixture is local and deterministic; only the
// model call is live.
func TestLive_ToolChoiceMatrix(t *testing.T) {
	if strings.TrimSpace(os.Getenv(toolChoiceGateEnv)) != "1" {
		t.Skipf("set SHANNON_E2E_LIVE=1 and %s=1 to run the paid tool-choice matrix", toolChoiceGateEnv)
	}
	skipUnlessLive(t)
	cfg := loadToolChoiceConfig(t)
	fixture := writeToolChoiceFixture(t)
	cases := toolChoiceCases()
	jobs := buildToolChoiceJobs(len(cases), cfg.repetitions, cfg.seed)
	report := toolChoiceReport{
		SchemaVersion: "kocoro.tool_choice.v1", Sample: cfg.sample, RepetitionsPerCase: cfg.repetitions,
		Seed: cfg.seed, Scheduled: len(jobs), MaxCostUSD: cfg.maxCostUSD,
		Runs: make([]toolChoiceRun, 0, len(jobs)),
	}
	defer func() {
		report.GeneratedAt = time.Now().UTC().Format(time.RFC3339)
		report.Completed = len(report.Runs)
		report.Complete = report.Completed == report.Scheduled
		report.ReportedCostUSD = 0
		finalizeToolChoiceReport(&report)
		writeToolChoiceReport(t, cfg.outputPath, report)
	}()

	daemon := startIsolatedLiveDaemon(t, testBinary(t), isolatedLiveOptions{
		EffortTier:  "max",
		AutoApprove: true,
	})
	for scheduleIndex, job := range jobs {
		if report.ReportedCostUSD > cfg.maxCostUSD {
			t.Fatalf("tool-choice cost $%.6f exceeded cap $%.2f", report.ReportedCostUSD, cfg.maxCostUSD)
		}
		tc := cases[job.caseIndex]
		prompt := tc.prompt(fixture, job.repetition)
		run := streamMessage(t, daemon.baseURL, map[string]interface{}{
			"text": prompt, "source": "kocoro",
		})
		result := evaluateToolChoiceRun(tc, fixture, job.repetition, scheduleIndex+1, run)
		report.Runs = append(report.Runs, result)
		report.ReportedCostUSD += run.CostUSD
		if report.ReportedCostUSD > cfg.maxCostUSD {
			t.Fatalf("tool-choice cost $%.6f exceeded cap $%.2f", report.ReportedCostUSD, cfg.maxCostUSD)
		}
		t.Logf("case=%s repetition=%d correct=%t failures=%v tools=%v latency=%s cost=$%.6f",
			tc.name, job.repetition, result.Correct, result.Failures, result.ToolCounts,
			run.Duration.Round(time.Millisecond), run.CostUSD)
	}
	for _, run := range report.Runs {
		if !run.Correct {
			t.Errorf("case=%s repetition=%d failed: %v", run.Case, run.Repetition, run.Failures)
		}
	}
	finalizeToolChoiceReport(&report)
	if !report.ComparisonQualifying {
		t.Fatalf("tool-choice comparison did not qualify; report=%s", cfg.outputPath)
	}
	if cfg.sample == "release" && !report.ReleaseQualifying {
		t.Fatalf("tool-choice release did not qualify; report=%s", cfg.outputPath)
	}
}

func loadToolChoiceConfig(t *testing.T) toolChoiceConfig {
	t.Helper()
	cfg := toolChoiceConfig{
		sample: "comparison", repetitions: toolChoiceComparisonRepetitions,
		seed: toolChoiceDefaultSeed, maxCostUSD: 8,
	}
	if raw := strings.TrimSpace(os.Getenv(toolChoiceSampleEnv)); raw != "" {
		cfg.sample = raw
	}
	if cfg.sample != "comparison" && cfg.sample != "release" {
		t.Fatalf("%s must be comparison or release", toolChoiceSampleEnv)
	}
	if raw := strings.TrimSpace(os.Getenv(toolChoiceRepetitionsEnv)); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > 30 {
			t.Fatalf("%s must be an integer in [1,30]", toolChoiceRepetitionsEnv)
		}
		cfg.repetitions = value
	}
	if cfg.sample == "release" && cfg.repetitions < toolChoiceReleaseRepetitions {
		t.Fatalf("release tool-choice sample requires %s >= %d", toolChoiceRepetitionsEnv, toolChoiceReleaseRepetitions)
	}
	if raw := strings.TrimSpace(os.Getenv(toolChoiceSeedEnv)); raw != "" {
		value, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			t.Fatalf("%s must be a signed 64-bit integer", toolChoiceSeedEnv)
		}
		cfg.seed = value
	}
	if raw := strings.TrimSpace(os.Getenv(toolChoiceMaxCostEnv)); raw != "" {
		value, err := strconv.ParseFloat(raw, 64)
		if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value <= 0 || value > 50 {
			t.Fatalf("%s must be greater than 0 and at most 50", toolChoiceMaxCostEnv)
		}
		cfg.maxCostUSD = value
	}
	cfg.outputPath = strings.TrimSpace(os.Getenv(toolChoiceOutputEnv))
	if cfg.outputPath == "" {
		cfg.outputPath = filepath.Join(os.TempDir(), fmt.Sprintf("kocoro-tool-choice-%d.json", cfg.seed))
	}
	return cfg
}

func toolChoiceCases() []toolChoiceCase {
	return []toolChoiceCase{
		{
			name: "exact_file_read",
			prompt: func(f toolChoiceFixture, _ int) string {
				return fmt.Sprintf("Read %s and reply with only the token stored in that file.", f.exactFile)
			},
			requiredTools: []string{"file_read"}, forbiddenTools: []string{"bash", "grep"},
			answerContains: []string{"READ-ORACLE-731"}, maxToolCalls: 2,
			argumentOracle: func(f toolChoiceFixture, _ int, frames []liveToolFrame) error {
				return requireToolArgsContain(frames, "file_read", f.exactFile)
			},
		},
		{
			name: "dedicated_content_search",
			prompt: func(f toolChoiceFixture, _ int) string {
				return fmt.Sprintf("Search every file under %s for the exact string %s. Report each matching relative path and matching line.", f.root, f.searchToken)
			},
			requiredTools: []string{"grep"}, forbiddenTools: []string{"bash"},
			answerContains: []string{"nested/telemetry.txt", "QUASAR-TELEMETRY-731"}, maxToolCalls: 3,
			argumentOracle: func(f toolChoiceFixture, _ int, frames []liveToolFrame) error {
				return requireToolArgsContain(frames, "grep", f.root, f.searchToken)
			},
		},
		{
			name: "directory_listing",
			prompt: func(f toolChoiceFixture, _ int) string {
				return fmt.Sprintf("List the immediate entries in %s and tell me the name of the .csv file. Do not search file contents.", f.root)
			},
			requiredTools: []string{"directory_list"}, forbiddenTools: []string{"bash", "grep"},
			answerContains: []string{"inventory.csv"}, maxToolCalls: 2,
			argumentOracle: func(f toolChoiceFixture, _ int, frames []liveToolFrame) error {
				return requireToolArgsContain(frames, "directory_list", f.root)
			},
		},
		{
			name: "file_write_effect",
			prompt: func(f toolChoiceFixture, repetition int) string {
				path := filepath.Join(f.root, fmt.Sprintf("created-%02d.txt", repetition))
				return fmt.Sprintf("Create %s with exactly this content and no trailing explanation: WRITE-ORACLE-%02d", path, repetition)
			},
			requiredTools: []string{"file_write"}, forbiddenTools: []string{"bash"},
			maxToolCalls: 2,
			argumentOracle: func(f toolChoiceFixture, repetition int, frames []liveToolFrame) error {
				return requireToolArgsContain(frames, "file_write",
					filepath.Join(f.root, fmt.Sprintf("created-%02d.txt", repetition)),
					fmt.Sprintf("WRITE-ORACLE-%02d", repetition))
			},
			stateOracle: func(f toolChoiceFixture, repetition int) error {
				path := filepath.Join(f.root, fmt.Sprintf("created-%02d.txt", repetition))
				body, err := os.ReadFile(path)
				if err != nil {
					return fmt.Errorf("written_file_missing:%v", err)
				}
				if string(body) != fmt.Sprintf("WRITE-ORACLE-%02d", repetition) {
					return fmt.Errorf("written_content_mismatch:%q", string(body))
				}
				return nil
			},
		},
		{
			name: "shell_automation",
			prompt: func(_ toolChoiceFixture, repetition int) string {
				return fmt.Sprintf("Use shell automation to print exactly SHELL-ORACLE-%02d, then reply with that marker.", repetition)
			},
			requiredTools: []string{"bash"}, answerContains: []string{"SHELL-ORACLE-"}, maxToolCalls: 3,
			argumentOracle: func(_ toolChoiceFixture, repetition int, frames []liveToolFrame) error {
				return requireToolArgsContain(frames, "bash", fmt.Sprintf("SHELL-ORACLE-%02d", repetition))
			},
		},
		{
			name: "no_tool_rewrite",
			prompt: func(_ toolChoiceFixture, _ int) string {
				return "Rewrite these facts as one concise sentence without using tools: launch is Tuesday; owner is Mina. Preserve both facts."
			},
			forbiddenTools: []string{"bash", "file_read", "grep", "directory_list", "file_write", "tool_search"},
			answerContains: []string{"Tuesday", "Mina"}, maxToolCalls: 0,
		},
	}
}

func evaluateToolChoiceRun(tc toolChoiceCase, fixture toolChoiceFixture, repetition, scheduleIndex int, run liveSSERun) toolChoiceRun {
	tools := runningToolNames(run.Frames)
	counts := make(map[string]int)
	for _, name := range tools {
		counts[name]++
	}
	result := toolChoiceRun{
		Case: tc.name, Repetition: repetition, ScheduleIndex: scheduleIndex,
		ToolCounts: counts, LatencyMillis: run.Duration.Milliseconds(), CostUSD: run.CostUSD,
		CostObserved: run.CostUSD > 0, Answer: doneReply(run.Frames),
		ToolFlow: liveToolFlowWithArguments(decodeLiveToolFrames(run.Frames)),
	}
	for _, name := range tc.requiredTools {
		if counts[name] == 0 {
			result.Failures = append(result.Failures, "required_tool_missing:"+name)
		}
	}
	for _, name := range tc.forbiddenTools {
		if counts[name] > 0 {
			result.Failures = append(result.Failures, "forbidden_tool_used:"+name)
		}
	}
	if len(tools) > tc.maxToolCalls {
		result.Failures = append(result.Failures, fmt.Sprintf("tool_calls_%d_exceed_%d", len(tools), tc.maxToolCalls))
	}
	for _, token := range tc.answerContains {
		if !strings.Contains(result.Answer, token) {
			result.Failures = append(result.Failures, "answer_missing:"+token)
		}
	}
	if strings.TrimSpace(result.Answer) == "" {
		result.Failures = append(result.Failures, "empty_answer")
	}
	if tc.name == "shell_automation" && !strings.Contains(result.Answer, fmt.Sprintf("SHELL-ORACLE-%02d", repetition)) {
		result.Failures = append(result.Failures, "answer_missing_exact_shell_marker")
	}
	if tc.stateOracle != nil {
		if err := tc.stateOracle(fixture, repetition); err != nil {
			result.Failures = append(result.Failures, err.Error())
		}
	}
	if tc.argumentOracle != nil {
		if err := tc.argumentOracle(fixture, repetition, decodeLiveToolFrames(run.Frames)); err != nil {
			result.Failures = append(result.Failures, err.Error())
		}
	}
	result.Correct = len(result.Failures) == 0
	return result
}

func finalizeToolChoiceReport(report *toolChoiceReport) {
	report.Completed = len(report.Runs)
	report.Complete = report.Completed == report.Scheduled
	report.CorrectRuns = 0
	report.ReportedCostUSD = 0
	report.CostObserved = len(report.Runs) > 0
	for _, run := range report.Runs {
		if run.Correct {
			report.CorrectRuns++
		}
		report.ReportedCostUSD += run.CostUSD
		report.CostObserved = report.CostObserved && run.CostObserved
	}
	coverageComplete := toolChoiceCoverageComplete(report.RepetitionsPerCase, report.Scheduled, report.Runs)
	allCorrect := report.Complete && coverageComplete && report.Scheduled > 0 && report.CorrectRuns == report.Scheduled && report.ReportedCostUSD <= report.MaxCostUSD
	report.ComparisonQualifying = allCorrect && report.CostObserved && report.RepetitionsPerCase >= toolChoiceComparisonRepetitions
	report.ReleaseQualifying = allCorrect && report.CostObserved && report.Sample == "release" && report.RepetitionsPerCase >= toolChoiceReleaseRepetitions
}

func toolChoiceCoverageComplete(repetitions, scheduled int, runs []toolChoiceRun) bool {
	if repetitions < 1 || scheduled != len(toolChoiceCases())*repetitions || len(runs) != scheduled {
		return false
	}
	knownCases := make(map[string]bool, len(toolChoiceCases()))
	for _, tc := range toolChoiceCases() {
		knownCases[tc.name] = true
	}
	seen := make(map[string]bool, len(runs))
	for _, run := range runs {
		if !knownCases[run.Case] || run.Repetition < 1 || run.Repetition > repetitions {
			return false
		}
		cell := fmt.Sprintf("%s/%d", run.Case, run.Repetition)
		if seen[cell] {
			return false
		}
		seen[cell] = true
	}
	return len(seen) == scheduled
}

func requireToolArgsContain(frames []liveToolFrame, tool string, required ...string) error {
	for _, frame := range frames {
		if frame.Tool != tool || frame.Status != "running" {
			continue
		}
		for _, value := range required {
			if !strings.Contains(frame.Args, value) {
				return fmt.Errorf("%s_args_missing:%s", tool, value)
			}
		}
		return nil
	}
	return fmt.Errorf("%s_running_frame_missing", tool)
}

func liveToolFlowWithArguments(frames []liveToolFrame) []string {
	flow := make([]string, 0, len(frames))
	for _, frame := range frames {
		entry := frame.Tool + "/" + frame.Status
		if frame.Status == "running" && frame.Args != "" {
			entry += ":" + truncateForLog(frame.Args)
		}
		if frame.Status == "completed" && frame.Preview != "" {
			entry += ":" + truncateForLog(frame.Preview)
		}
		flow = append(flow, entry)
	}
	return flow
}

func TestOffline_ToolChoiceQualificationFailsClosed(t *testing.T) {
	passingRuns := func(repetitions int) []toolChoiceRun {
		runs := make([]toolChoiceRun, 0, len(toolChoiceCases())*repetitions)
		for repetition := 1; repetition <= repetitions; repetition++ {
			for _, tc := range toolChoiceCases() {
				runs = append(runs, toolChoiceRun{
					Case: tc.name, Repetition: repetition,
					Correct: true, CostUSD: 0.01, CostObserved: true,
				})
			}
		}
		return runs
	}
	comparisonRuns := passingRuns(toolChoiceComparisonRepetitions)
	comparison := toolChoiceReport{
		Complete: true, Sample: "comparison", RepetitionsPerCase: toolChoiceComparisonRepetitions,
		Scheduled: len(comparisonRuns), MaxCostUSD: 8, Runs: comparisonRuns,
	}
	finalizeToolChoiceReport(&comparison)
	if !comparison.ComparisonQualifying || comparison.ReleaseQualifying {
		t.Fatalf("comparison qualification=%+v", comparison)
	}
	releaseRuns := passingRuns(toolChoiceReleaseRepetitions)
	release := toolChoiceReport{
		Complete: true, Sample: "release", RepetitionsPerCase: toolChoiceReleaseRepetitions,
		Scheduled: len(releaseRuns), MaxCostUSD: 8, Runs: releaseRuns,
	}
	finalizeToolChoiceReport(&release)
	if !release.ReleaseQualifying {
		t.Fatal("complete all-correct release report did not qualify")
	}
	for name, mutate := range map[string]func(*toolChoiceReport){
		"incomplete": func(report *toolChoiceReport) { report.Runs = report.Runs[:len(report.Runs)-1] },
		"incorrect":  func(report *toolChoiceReport) { report.Runs[0].Correct = false },
		"over_cost": func(report *toolChoiceReport) {
			report.Runs[0].CostUSD = report.MaxCostUSD + 0.01
		},
		"undersized": func(report *toolChoiceReport) { report.RepetitionsPerCase = toolChoiceReleaseRepetitions - 1 },
		"no_cost": func(report *toolChoiceReport) {
			report.Runs[0].CostObserved = false
		},
		"duplicate_cell": func(report *toolChoiceReport) {
			report.Runs[0].Case = report.Runs[1].Case
			report.Runs[0].Repetition = report.Runs[1].Repetition
		},
	} {
		t.Run(name, func(t *testing.T) {
			candidate := release
			candidate.Runs = append([]toolChoiceRun(nil), release.Runs...)
			mutate(&candidate)
			finalizeToolChoiceReport(&candidate)
			if candidate.ReleaseQualifying {
				t.Fatal("invalid report unexpectedly release-qualified")
			}
		})
	}
}

func buildToolChoiceJobs(caseCount, repetitions int, seed int64) []toolChoiceJob {
	jobs := make([]toolChoiceJob, 0, caseCount*repetitions)
	for repetition := 1; repetition <= repetitions; repetition++ {
		for caseIndex := 0; caseIndex < caseCount; caseIndex++ {
			jobs = append(jobs, toolChoiceJob{caseIndex: caseIndex, repetition: repetition})
		}
	}
	rand.New(rand.NewSource(seed)).Shuffle(len(jobs), func(i, j int) { jobs[i], jobs[j] = jobs[j], jobs[i] })
	return jobs
}

func writeToolChoiceReport(t *testing.T, path string, report toolChoiceReport) {
	t.Helper()
	body, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Errorf("marshal tool-choice report: %v", err)
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Errorf("create tool-choice report directory: %v", err)
		return
	}
	if err := os.WriteFile(path, append(body, '\n'), 0o600); err != nil {
		t.Errorf("write tool-choice report: %v", err)
	}
}

func writeToolChoiceFixture(t *testing.T) toolChoiceFixture {
	t.Helper()
	root := neutralTempDir(t, "observatory-data-*")
	files := map[string]string{
		"README.md":            "fixture root\n",
		"exact.txt":            "READ-ORACLE-731\n",
		"inventory.csv":        "item,count\nprobe,1\n",
		"notes/ordinary.txt":   "nothing relevant here\n",
		"nested/telemetry.txt": "status=QUASAR-TELEMETRY-731\n",
		"nested/archive.log":   "QUASAR-TELEMETRY-730\n",
	}
	for name, body := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatalf("create fixture directory: %v", err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("write fixture %s: %v", name, err)
		}
	}
	return toolChoiceFixture{root: root, exactFile: filepath.Join(root, "exact.txt"), searchToken: "QUASAR-TELEMETRY-731"}
}

func runningToolNames(frames []sseFrame) []string {
	var names []string
	for _, frame := range frames {
		if frame.Event != "tool" {
			continue
		}
		var payload struct {
			Tool   string `json:"tool"`
			Status string `json:"status"`
		}
		if json.Unmarshal(frame.Data, &payload) == nil && payload.Status == "running" && payload.Tool != "" {
			names = append(names, payload.Tool)
		}
	}
	sort.Strings(names)
	return names
}

func doneReply(frames []sseFrame) string {
	for _, frame := range frames {
		if frame.Event != "done" {
			continue
		}
		var payload struct {
			Reply string `json:"reply"`
		}
		if json.Unmarshal(frame.Data, &payload) == nil {
			return payload.Reply
		}
	}
	return ""
}
