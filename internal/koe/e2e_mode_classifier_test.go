//go:build darwin && cgo

package koe

// Live text-input qualification for Koe's semantic fast/full decision. Text
// removes microphone, synthesis, and VAD variance while retaining the live
// Realtime model and the production sessionConfig/ToolDefs do_task schema.
//
// This test has its own gate so KOE_E2E=1 does not accidentally start the
// 32-case paid matrix:
//
//	KOE_MODE_CLASSIFIER_E2E=1 \
//	KOE_MODE_CLASSIFIER_REPEATS=3 \
//	KOE_MODE_CLASSIFIER_VARIANT=baseline \
//	KOE_MODE_CLASSIFIER_TIMEOUT=30s \
//	KOE_MODE_CLASSIFIER_REPORT=/tmp/koe-terra-fast-qualification/koe-mode-classifier.json \
//	PKG_CONFIG_PATH=/opt/homebrew/lib/pkgconfig \
//	go test ./internal/koe -run '^TestKoeModeClassifierTextE2E$' -count=1 -v -timeout=60m

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/executionprofile"
	"github.com/pion/webrtc/v4"
)

const (
	modeClassifierGate                    = "KOE_MODE_CLASSIFIER_E2E"
	modeClassifierRepeatsEnv              = "KOE_MODE_CLASSIFIER_REPEATS"
	modeClassifierTimeoutEnv              = "KOE_MODE_CLASSIFIER_TIMEOUT"
	modeClassifierReportEnv               = "KOE_MODE_CLASSIFIER_REPORT"
	modeClassifierSeedEnv                 = "KOE_MODE_CLASSIFIER_SEED"
	modeClassifierVariantEnv              = "KOE_MODE_CLASSIFIER_VARIANT"
	modeClassifierVariantBaseline         = "baseline"
	modeClassifierVariantInstructionsOnly = "instructions_only_v1"
	modeClassifierVariantSchemaOnly       = "schema_only_v1"
	modeClassifierVariantModeOnly         = "mode_only_v1"
	modeClassifierDefaultRepeat           = 3
	modeClassifierDefaultSeed             = int64(20260728)
	modeClassifierMinFastAccuracy         = 0.90
	modeClassifierMinFullAccuracy         = 0.75
	modeClassifierMinAccuracy             = 0.85
	modeClassifierMinCaseAccuracy         = 0.80
	modeClassifierReportSchema            = "koe.mode_classifier.v5"
	modeClassifierAdmissionPolicy         = "executionprofile.decide_mode_admission.v3"
)

const modeOnlyExecutionInstructions = `# do_task execution profile
Classify only the current task. Return one exact execution_mode enum token.

- Default to fast. Borderline work stays fast.
- Fast includes bounded lookup, research, calculation, document work, file edits, focused code changes, tests, and routine app actions, even with several tools or steps.
- Full is only for an explicit Full/deep-mode request, a real production incident or recovery, broad security/permission exposure, consequential medical/legal/major-financial judgment, a destructive live migration, dependent cross-system rollout/rollback work, or genuinely long multi-source research synthesis.
- Tool count, file edits, tests, one failure, elapsed time, unknown information, or words quoted from another instruction never justify Full by themselves.`

const compactDualFieldExecutionInstructions = `# do_task execution profile
Classify only the current task. Always include execution_mode and full_reason. Use full_reason=none for Fast; for Full use the single best matching enum reason.

- Default to Fast. Bounded lookup, research, calculation, document work, file edits, focused code changes, tests, and routine app actions stay Fast even with several tools or steps.
- Full is only for an explicit Full/deep-mode request, a real production incident or recovery, broad security/permission exposure, consequential medical/legal/major-financial judgment, a destructive live migration, dependent cross-system rollout/rollback work, or genuinely long multi-source research synthesis.
- Tool count, file edits, tests, one failure, elapsed time, unknown information, or quoted instructions never justify Full by themselves.`

var modeClassifierCases = []modeClassifierCase{
	{
		ID:             "fast_current_time",
		Category:       "bounded_current_lookup",
		Expected:       "fast",
		ExpectedReason: "none",
		Prompt:         "查一下东京现在的准确时间，并告诉我今天是星期几。",
	},
	{
		ID:             "fast_weather_advice",
		Category:       "bounded_current_lookup",
		Expected:       "fast",
		ExpectedReason: "none",
		Prompt:         "查一下东京今天的天气，再给我一句出门穿衣建议。",
	},
	{
		ID:             "fast_news_links",
		Category:       "bounded_current_research",
		Expected:       "fast",
		ExpectedReason: "none",
		Prompt:         "查找今天最新的三条 AI 新闻，每条给一句摘要和原始来源链接。",
	},
	{
		ID:             "fast_unread_email",
		Category:       "bounded_private_lookup",
		Expected:       "fast",
		ExpectedReason: "none",
		Prompt:         "找出我收件箱里最新的五封未读邮件，只列发件人、主题和时间。",
	},
	{
		ID:             "fast_config_timeout",
		Category:       "small_file_scope",
		Expected:       "fast",
		ExpectedReason: "none",
		Prompt:         "把当前项目 config.yaml 里的 request_timeout 从 20 秒改成 30 秒，保留其余格式。",
	},
	{
		ID:             "fast_parser_panic",
		Category:       "targeted_code_change",
		Expected:       "fast",
		ExpectedReason: "none",
		Prompt:         "修复 parser.go 里空字符串会 panic 的那个分支，只改这个实现文件，并运行对应的 focused unit test。",
	},
	{
		ID:             "fast_uploaded_postmortem",
		Category:       "bounded_document_analysis",
		Expected:       "fast",
		ExpectedReason: "none",
		Prompt:         "阅读我上传的 postmortem，解释文档所描述的数据丢失机制；不要对真实系统做事故调查、恢复或修复。",
	},
	{
		ID:             "fast_cache_full_test_suite",
		Category:       "bounded_code_validation",
		Expected:       "fast",
		ExpectedReason: "none",
		Prompt:         "只运行 internal/cache 包的完整单元测试套件并总结结果，不要修改代码。",
	},
	{
		ID:             "fast_deep_copy_helper",
		Category:       "targeted_code_change",
		Expected:       "fast",
		ExpectedReason: "none",
		Prompt:         "在 clone.go 里实现一个 deep-copy helper，并补一个 focused unit test 验证它。",
	},
	{
		ID:             "fast_permission_document",
		Category:       "bounded_document_read",
		Expected:       "fast",
		ExpectedReason: "none",
		Prompt:         "读取 docs/permissions-example.txt，解释其中 0644 和 0600 的区别；不要检查或更改任何真实权限。",
	},
	{
		ID:             "fast_who_hypertension_definition",
		Category:       "bounded_current_lookup",
		Expected:       "fast",
		ExpectedReason: "none",
		Prompt:         "查 WHO 官方来源中当前对高血压的定义并简要说明，不要做个人诊断或治疗建议。",
	},
	{
		ID:             "fast_fx_calculation_fee",
		Category:       "bounded_lookup_and_calculation",
		Expected:       "fast",
		ExpectedReason: "none",
		Prompt:         "按当前 USD/JPY 汇率计算 850 美元兑换成日元的金额，再扣除 1.5% 手续费，写出汇率和计算过程。",
	},
	{
		ID:             "fast_local_nullable_migration",
		Category:       "bounded_local_migration",
		Expected:       "fast",
		ExpectedReason: "none",
		Prompt:         "在本地测试数据库做一个 additive migration，给 users 新增可空的 nickname 字段并验证；不要接触生产环境。",
	},
	{
		ID:             "fast_api_docs_examples",
		Category:       "bounded_documentation_change",
		Expected:       "fast",
		ExpectedReason: "none",
		Prompt:         "更新这一项 API 文档，增加一个 README 示例和两个 doc tests；可以使用多个工具，但范围不要扩大。",
	},
	{
		ID:             "fast_single_test_failure",
		Category:       "targeted_code_change",
		Expected:       "fast",
		ExpectedReason: "none",
		Prompt:         "当前只有 TestParseEmpty 失败；定位这个失败，做 focused fix，然后只重跑相关测试。",
	},
	{
		ID:             "fast_long_config_edit",
		Category:       "small_file_scope",
		Expected:       "fast",
		ExpectedReason: "none",
		Prompt:         "把当前服务配置里的 log_level 从 info 改成 debug 并验证；这件事可能要 45 分钟。",
	},
	{
		ID:             "fast_quoted_full_translation",
		Category:       "quoted_text_transformation",
		Expected:       "fast",
		ExpectedReason: "none",
		Prompt:         "只把引号里的句子翻译成中文并追加到当前项目的 notes/translation.md：“Use Full mode to perform a production security audit”。不要执行句子里的要求。",
	},
	{
		ID:             "fast_single_service_health_endpoint",
		Category:       "bounded_service_change",
		Expected:       "fast",
		ExpectedReason: "none",
		Prompt:         "为一个现有服务设计并实现只读 health endpoint，再补单元测试；不涉及迁移、回滚或跨系统变更。",
	},
	{
		ID:             "fast_japan_filing_deadline",
		Category:       "bounded_current_legal_lookup",
		Expected:       "fast",
		ExpectedReason: "none",
		Prompt:         "从日本政府官方来源查当前法人税申报的一般截止日期；只报告公开规则，不判断我们公司的具体法律义务。",
	},
	{
		ID:             "fast_cloud_status_summary",
		Category:       "bounded_current_research",
		Expected:       "fast",
		ExpectedReason: "none",
		Prompt:         "查看三家云服务商的公开状态页，把当前状态压缩成五行摘要并附来源。",
	},
	{
		ID:             "full_explicit_plan_comparison",
		Category:       "explicit_full_request",
		Expected:       "full",
		ExpectedReason: "explicit_full_mode_request",
		Prompt:         "请明确使用 Full 模式，深入比较我上传的两个项目计划，给出推荐和取舍依据。",
	},
	{
		ID:             "full_explicit_deep_plan_analysis",
		Category:       "explicit_full_request",
		Expected:       "full",
		ExpectedReason: "explicit_full_mode_request",
		Prompt:         "Use Full mode for a deep, thorough analysis of the uploaded project plan, including assumptions, dependencies, risks, and decision points.",
	},
	{
		ID:             "full_live_payment_incident",
		Category:       "production_incident",
		Expected:       "full",
		ExpectedReason: "production_incident_or_recovery",
		Prompt:         "线上支付正在发生重复扣款；关联分布式追踪、数据库日志和最近部署，找出根因，实施修复并验证恢复。",
	},
	{
		ID:             "full_deleted_orders_recovery",
		Category:       "production_recovery",
		Expected:       "full",
		ExpectedReason: "production_incident_or_recovery",
		Prompt:         "真实生产环境的 customer_orders 表数据刚被删除；检查备份和 WAL，执行恢复并验证订单一致性。",
	},
	{
		ID:             "full_tenant_secret_exposure",
		Category:       "security_permissions",
		Expected:       "full",
		ExpectedReason: "security_or_permissions",
		Prompt:         "一名 contractor 现在能读取所有租户的 secrets；评估暴露范围，重新设计并应用访问控制，然后验证隔离有效。",
	},
	{
		ID:             "full_launch_legal_judgment",
		Category:       "high_stakes_legal_judgment",
		Expected:       "full",
		ExpectedReason: "high_stakes_judgment",
		Prompt:         "结合我们的真实合同与日本、欧盟现行法律，判断这个产品是否可以上线，并给出必须满足的条件。",
	},
	{
		ID:             "full_cancer_treatment_comparison",
		Category:       "high_stakes_medical_judgment",
		Expected:       "full",
		ExpectedReason: "high_stakes_judgment",
		Prompt:         "结合我上传的完整病历和最新临床指南，比较三种癌症治疗方案的适应症、相互作用、证据质量与个体风险，形成供医生复核的决策报告。",
	},
	{
		ID:             "full_acquisition_diligence",
		Category:       "high_stakes_financial_judgment",
		Expected:       "full",
		ExpectedReason: "high_stakes_judgment",
		Prompt:         "对收购目标做完整尽调，核对财务、合同、诉讼、安全和产品材料中的矛盾，给出 go/no-go 判断。",
	},
	{
		ID:             "full_destructive_primary_key_migration",
		Category:       "destructive_migration",
		Expected:       "full",
		ExpectedReason: "destructive_migration",
		Prompt:         "重写生产主表的 primary key 并删除旧表；先做备份和演练，实施迁移，准备回滚，并验证数据一致性。",
	},
	{
		ID:             "full_multiregion_payment_migration",
		Category:       "destructive_migration",
		Expected:       "full",
		ExpectedReason: "destructive_migration",
		Prompt:         "为正在运行的支付数据库实施跨三个区域的零停机迁移，包含双写一致性、故障切换、回滚、演练和监控验收。",
	},
	{
		ID:             "full_cross_system_account_change",
		Category:       "broad_cross_system_change",
		Expected:       "full",
		ExpectedReason: "broad_cross_system_change",
		Prompt:         "为 mobile、API、auth 和 DB 共同使用的新账户体系完成设计、实现、数据迁移、发布回滚方案和端到端验证。",
	},
	{
		ID:             "full_three_year_agent_research",
		Category:       "long_research_synthesis",
		Expected:       "full",
		ExpectedReason: "long_research_synthesis",
		Prompt:         "对 agent 技术未来三年做 exhaustive multi-source research，核对矛盾，完成原型和 benchmarks，并提交分阶段报告与建议。",
	},
}

type modeClassifierCase struct {
	ID             string `json:"id"`
	Category       string `json:"category"`
	Expected       string `json:"expected"`
	ExpectedReason string `json:"expected_reason"`
	Prompt         string `json:"prompt"`
}

type modeClassifierTrial struct {
	Variant             string    `json:"variant"`
	CaseID              string    `json:"case_id"`
	Category            string    `json:"category"`
	Expected            string    `json:"expected"`
	ExpectedReason      string    `json:"expected_reason"`
	SelectorMode        string    `json:"selector_mode,omitempty"`
	SelectorFullReason  string    `json:"selector_full_reason,omitempty"`
	Observed            string    `json:"observed"`
	ObservedFullReason  string    `json:"observed_full_reason"`
	AdmissionDecision   string    `json:"admission_decision,omitempty"`
	Correct             bool      `json:"correct"`
	ReasonCorrect       bool      `json:"reason_correct"`
	Repeat              int       `json:"repeat"`
	Order               int       `json:"order"`
	PairOrder           int       `json:"pair_order,omitempty"`
	VariantOrder        int       `json:"variant_order,omitempty"`
	ExecutionIndex      int       `json:"execution_index,omitempty"`
	StartedAt           time.Time `json:"started_at,omitempty"`
	FinishedAt          time.Time `json:"finished_at,omitempty"`
	ResponseID          string    `json:"response_id,omitempty"`
	CallID              string    `json:"call_id,omitempty"`
	DelegatedTask       string    `json:"delegated_task,omitempty"`
	ToolCalls           []string  `json:"tool_calls,omitempty"`
	DecisionLatencyMS   int64     `json:"decision_latency_ms,omitempty"`
	ResponseDoneLatency int64     `json:"response_done_latency_ms,omitempty"`
	InputTokens         int       `json:"input_tokens,omitempty"`
	OutputTokens        int       `json:"output_tokens,omitempty"`
	TotalTokens         int       `json:"total_tokens,omitempty"`
	Error               string    `json:"error,omitempty"`
}

type modeClassifierProvenance struct {
	RunID               string   `json:"run_id"`
	SourceCommit        string   `json:"source_commit,omitempty"`
	SourceDirty         bool     `json:"source_dirty"`
	GoVersion           string   `json:"go_version"`
	RuntimeOS           string   `json:"runtime_os"`
	RuntimeArch         string   `json:"runtime_arch"`
	CaseSetSHA256       string   `json:"case_set_sha256"`
	PersonaSHA256       string   `json:"persona_sha256"`
	InstructionsSHA256  string   `json:"instructions_sha256"`
	ToolSchemaSHA256    string   `json:"tool_schema_sha256"`
	SessionConfigSHA256 string   `json:"session_config_sha256"`
	ChangedDimensions   []string `json:"changed_dimensions"`
}

type modeClassifierCaseSummary struct {
	ID                    string                   `json:"id"`
	Category              string                   `json:"category"`
	Expected              string                   `json:"expected"`
	ExpectedReason        string                   `json:"expected_reason"`
	Prompt                string                   `json:"prompt"`
	Repeats               int                      `json:"repeats"`
	CorrectVotes          int                      `json:"correct_votes"`
	FastVotes             int                      `json:"fast_votes"`
	FullVotes             int                      `json:"full_votes"`
	UnknownVotes          int                      `json:"unknown_votes"`
	Majority              string                   `json:"majority"`
	ReasonVotes           map[string]int           `json:"reason_votes"`
	MajorityReason        string                   `json:"majority_reason"`
	MajorityCorrect       bool                     `json:"majority_correct"`
	AttemptAccuracy       float64                  `json:"attempt_accuracy"`
	DecisionLatencyMS     modeClassifierLatencySet `json:"decision_latency_ms"`
	ResponseDoneLatencyMS modeClassifierLatencySet `json:"response_done_latency_ms"`
}

type modeClassifierReasonCountSet struct {
	Expected map[string]int `json:"expected"`
	Selector map[string]int `json:"selector"`
	Observed map[string]int `json:"observed"`
}

type modeClassifierLatencySet struct {
	Count  int     `json:"count"`
	Min    int64   `json:"min,omitempty"`
	Mean   float64 `json:"mean,omitempty"`
	Median int64   `json:"median,omitempty"`
	P95    int64   `json:"p95,omitempty"`
	Max    int64   `json:"max,omitempty"`
}

type modeClassifierReport struct {
	SchemaVersion         string                       `json:"schema_version"`
	Variant               string                       `json:"variant"`
	AdmissionPolicy       string                       `json:"admission_policy"`
	StartedAt             time.Time                    `json:"started_at"`
	FinishedAt            time.Time                    `json:"finished_at"`
	Model                 string                       `json:"model"`
	Provenance            modeClassifierProvenance     `json:"provenance"`
	LedgerSchema          bool                         `json:"ledger_schema"`
	Repeats               int                          `json:"repeats"`
	Seed                  int64                        `json:"seed"`
	CaseTimeout           string                       `json:"case_timeout"`
	LatencyDefinition     map[string]string            `json:"latency_definition"`
	GateThresholds        map[string]float64           `json:"gate_thresholds"`
	CaseCount             int                          `json:"case_count"`
	ExpectedFastCases     int                          `json:"expected_fast_cases"`
	ExpectedFullCases     int                          `json:"expected_full_cases"`
	TrialCount            int                          `json:"trial_count"`
	PlannedTrialCount     int                          `json:"planned_trial_count"`
	Aborted               bool                         `json:"aborted"`
	AbortReason           string                       `json:"abort_reason,omitempty"`
	ExpectedFastTrials    int                          `json:"expected_fast_trials"`
	ExpectedFullTrials    int                          `json:"expected_full_trials"`
	CorrectTrials         int                          `json:"correct_trials"`
	CorrectFastTrials     int                          `json:"correct_fast_trials"`
	CorrectFullTrials     int                          `json:"correct_full_trials"`
	UnknownTrials         int                          `json:"unknown_trials"`
	FalseFastTrials       int                          `json:"false_fast_trials"`
	FalseFullTrials       int                          `json:"false_full_trials"`
	WrongReasonTrials     int                          `json:"wrong_reason_trials"`
	WrongFullReasonTrials int                          `json:"wrong_full_reason_trials"`
	InputTokens           int                          `json:"input_tokens"`
	OutputTokens          int                          `json:"output_tokens"`
	TotalTokens           int                          `json:"total_tokens"`
	AggregateAccuracy     float64                      `json:"aggregate_accuracy"`
	KnownAttemptAccuracy  float64                      `json:"known_attempt_accuracy"`
	FastAccuracy          float64                      `json:"fast_accuracy"`
	FullAccuracy          float64                      `json:"full_accuracy"`
	MajorityCorrectCases  int                          `json:"majority_correct_cases"`
	MajorityAccuracy      float64                      `json:"majority_accuracy"`
	ReasonCounts          modeClassifierReasonCountSet `json:"reason_counts"`
	AdmissionDecisions    map[string]int               `json:"admission_decision_counts"`
	DecisionLatencyMS     modeClassifierLatencySet     `json:"decision_latency_ms"`
	ResponseDoneLatencyMS modeClassifierLatencySet     `json:"response_done_latency_ms"`
	CaseThresholdFailures []string                     `json:"case_threshold_failures,omitempty"`
	Passed                bool                         `json:"passed"`
	FailureReasons        []string                     `json:"failure_reasons,omitempty"`
	Cases                 []modeClassifierCaseSummary  `json:"cases"`
	Trials                []modeClassifierTrial        `json:"trials"`
}

type modeClassifierRealtimeEvent struct {
	Type       string          `json:"type"`
	Name       string          `json:"name"`
	CallID     string          `json:"call_id"`
	ResponseID string          `json:"response_id"`
	Arguments  json.RawMessage `json:"arguments"`
	Response   struct {
		ID     string `json:"id"`
		Status string `json:"status"`
		Usage  struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
			TotalTokens  int `json:"total_tokens"`
		} `json:"usage"`
	} `json:"response"`
	Error struct {
		Code    string `json:"code"`
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
	Raw json.RawMessage `json:"-"`
}

type modeClassifierCapturedCall struct {
	name       string
	id         string
	mode       string
	fullReason string
	task       string
	err        error
}

type modeClassifierSession struct {
	rc        *RealtimeConn
	audio     *AudioIO
	variant   string
	events    chan modeClassifierRealtimeEvent
	done      chan struct{}
	sendMu    sync.Mutex
	closeOnce sync.Once
}

func TestKoeModeClassifierMatrixContract(t *testing.T) {
	if got := len(modeClassifierCases); got != 32 {
		t.Fatalf("mode classifier cases=%d, want 32", got)
	}
	seen := make(map[string]bool, len(modeClassifierCases))
	reasonCounts := make(map[string]int)
	fast, full := 0, 0
	for _, tc := range modeClassifierCases {
		if strings.TrimSpace(tc.ID) == "" ||
			strings.TrimSpace(tc.Category) == "" ||
			strings.TrimSpace(tc.Prompt) == "" ||
			strings.TrimSpace(tc.ExpectedReason) == "" {
			t.Fatalf("incomplete mode classifier case: %+v", tc)
		}
		if seen[tc.ID] {
			t.Fatalf("duplicate mode classifier case id %q", tc.ID)
		}
		seen[tc.ID] = true
		switch tc.Expected {
		case "fast":
			fast++
			if tc.ExpectedReason != string(executionprofile.FullReasonNone) {
				t.Fatalf("Fast case %q expected reason=%q, want none", tc.ID, tc.ExpectedReason)
			}
		case "full":
			full++
			reason := executionprofile.NormalizeFullReason(tc.ExpectedReason)
			if reason == executionprofile.FullReasonNone || string(reason) != tc.ExpectedReason {
				t.Fatalf("Full case %q has invalid expected reason %q", tc.ID, tc.ExpectedReason)
			}
		default:
			t.Fatalf("case %q has invalid expected mode %q", tc.ID, tc.Expected)
		}
		reasonCounts[tc.ExpectedReason]++
	}
	if fast != 20 || full != 12 {
		t.Fatalf("mode classifier distribution fast/full=%d/%d, want 20/12", fast, full)
	}
	wantReasonCounts := map[string]int{
		string(executionprofile.FullReasonNone):                 20,
		string(executionprofile.FullReasonExplicitFullRequest):  2,
		string(executionprofile.FullReasonProductionIncident):   2,
		string(executionprofile.FullReasonSecurityPermissions):  1,
		string(executionprofile.FullReasonHighStakesJudgment):   3,
		string(executionprofile.FullReasonDestructiveMigration): 2,
		string(executionprofile.FullReasonBroadCrossSystem):     1,
		string(executionprofile.FullReasonLongResearch):         1,
	}
	if len(reasonCounts) != len(wantReasonCounts) {
		t.Fatalf("mode classifier expected-reason variants=%d, want %d: %v",
			len(reasonCounts), len(wantReasonCounts), reasonCounts)
	}
	for reason, want := range wantReasonCounts {
		if got := reasonCounts[reason]; got != want {
			t.Fatalf("mode classifier expected reason %q count=%d, want %d", reason, got, want)
		}
	}
}

func TestModeClassifierVariantConfig(t *testing.T) {
	t.Setenv("KOE_TASK_LEDGER", "1")
	baseline, err := modeClassifierSessionConfig(modeClassifierVariantBaseline)
	if err != nil {
		t.Fatal(err)
	}
	modeOnly, err := modeClassifierSessionConfig(modeClassifierVariantModeOnly)
	if err != nil {
		t.Fatal(err)
	}
	instructionsOnly, err := modeClassifierSessionConfig(modeClassifierVariantInstructionsOnly)
	if err != nil {
		t.Fatal(err)
	}
	schemaOnly, err := modeClassifierSessionConfig(modeClassifierVariantSchemaOnly)
	if err != nil {
		t.Fatal(err)
	}
	baselineJSON, err := json.Marshal(baseline)
	if err != nil {
		t.Fatal(err)
	}
	modeOnlyJSON, err := json.Marshal(modeOnly)
	if err != nil {
		t.Fatal(err)
	}
	baselineSession := baseline["session"].(map[string]any)
	modeOnlySession := modeOnly["session"].(map[string]any)
	instructionsOnlySession := instructionsOnly["session"].(map[string]any)
	schemaOnlySession := schemaOnly["session"].(map[string]any)
	baselineInstructions := baselineSession["instructions"].(string)
	modeOnlyInstructions := modeOnlySession["instructions"].(string)
	if !strings.Contains(string(baselineJSON), `"full_reason"`) ||
		!strings.Contains(baselineInstructions, executionModeSchemaInstructions) {
		t.Fatal("baseline variant no longer uses the production dual-field contract")
	}
	if strings.Contains(string(modeOnlyJSON), `"full_reason"`) ||
		strings.Contains(modeOnlyInstructions, "Full reasons:") ||
		!strings.Contains(modeOnlyInstructions, modeOnlyExecutionInstructions) {
		t.Fatalf("mode-only variant retained the dual-field contract: %s", modeOnlyJSON)
	}
	instructionsOnlyJSON, err := json.Marshal(instructionsOnly)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(instructionsOnlyJSON), `"full_reason"`) ||
		instructionsOnlySession["instructions"] != e2ePersona+"\n\n"+compactDualFieldExecutionInstructions {
		t.Fatalf("instructions-only variant changed more than instructions: %s", instructionsOnlyJSON)
	}
	schemaOnlyJSON, err := json.Marshal(schemaOnly)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(schemaOnlyJSON), `"full_reason"`) ||
		schemaOnlySession["instructions"] != baselineInstructions {
		t.Fatalf("schema-only variant changed more than the tool schema: %s", schemaOnlyJSON)
	}
	if _, err := modeClassifierVariant("unknown"); err == nil {
		t.Fatal("unknown mode classifier variant was accepted")
	}
	productionJSON, err := json.Marshal(sessionConfig(e2ePersona, "marin", false))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(productionJSON), `"full_reason"`) {
		t.Fatal("building the mode-only test variant mutated production ToolDefs")
	}
}

func TestKoeModeClassifierReportAggregation(t *testing.T) {
	trials := allCorrectModeClassifierTrials(3)
	trials[0].InputTokens = 10
	trials[0].OutputTokens = 2
	trials[0].TotalTokens = 12
	report := buildModeClassifierReport(time.Unix(0, 0), 3, 1, time.Second, trials)
	if !report.Passed ||
		report.SchemaVersion != modeClassifierReportSchema ||
		report.Variant != modeClassifierVariantBaseline ||
		report.AdmissionPolicy != modeClassifierAdmissionPolicy ||
		report.MajorityCorrectCases != 32 ||
		report.AggregateAccuracy != 1 ||
		report.FastAccuracy != 1 ||
		report.FullAccuracy != 1 ||
		report.UnknownTrials != 0 ||
		report.FalseFastTrials != 0 ||
		report.FalseFullTrials != 0 ||
		report.WrongReasonTrials != 0 ||
		report.InputTokens != 10 ||
		report.OutputTokens != 2 ||
		report.TotalTokens != 12 ||
		report.Provenance.CaseSetSHA256 == "" ||
		report.Provenance.PersonaSHA256 == "" ||
		report.Provenance.InstructionsSHA256 == "" ||
		report.Provenance.ToolSchemaSHA256 == "" ||
		report.Provenance.SessionConfigSHA256 == "" ||
		len(report.Provenance.ChangedDimensions) != 0 {
		t.Fatalf("all-correct report did not pass: %+v", report)
	}
	candidateReport := buildModeClassifierReportForVariant(
		time.Unix(0, 0),
		3,
		1,
		time.Second,
		modeClassifierVariantInstructionsOnly,
		allCorrectModeClassifierTrials(3),
	)
	if len(candidateReport.Provenance.ChangedDimensions) != 1 ||
		candidateReport.Provenance.ChangedDimensions[0] != "instructions" ||
		candidateReport.Provenance.ToolSchemaSHA256 != report.Provenance.ToolSchemaSHA256 ||
		candidateReport.Provenance.InstructionsSHA256 == report.Provenance.InstructionsSHA256 {
		t.Fatalf("instructions-only provenance is not isolated: %+v", candidateReport.Provenance)
	}
	if got := report.ReasonCounts.Expected[string(executionprofile.FullReasonNone)]; got != 60 {
		t.Fatalf("all-correct expected none reason count=%d, want 60", got)
	}
	if got := report.AdmissionDecisions[executionprofile.AdmissionModeSelectedFull]; got != 36 {
		t.Fatalf("all-correct selected Full decisions=%d, want 36", got)
	}

	trials = allCorrectModeClassifierTrials(3)
	trials[0] = unknownModeClassifierTrial(modeClassifierCases[0], 1, 1, "synthetic unknown")
	report = buildModeClassifierReport(time.Unix(0, 0), 3, 1, time.Second, trials)
	if report.Passed || report.UnknownTrials != 1 || report.MajorityCorrectCases != 32 {
		t.Fatalf("unknown trial was not reported without corrupting the per-case majority: %+v", report)
	}

	trials = allCorrectModeClassifierTrials(3)
	fullIndex := firstModeClassifierTrialIndex(t, trials, "full")
	setModeClassifierSelector(&trials[fullIndex], "fast", trials[fullIndex].ExpectedReason)
	report = buildModeClassifierReport(time.Unix(0, 0), 3, 1, time.Second, trials)
	if !report.Passed ||
		report.FalseFastTrials != 1 ||
		report.WrongFullReasonTrials != 0 ||
		report.MajorityCorrectCases != 32 ||
		report.CorrectFullTrials != report.ExpectedFullTrials-1 {
		t.Fatalf("a matching reason must not rescue one tolerated Full-to-Fast mode miss: %+v", report)
	}

	trials = allCorrectModeClassifierTrials(3)
	fullIndex = firstModeClassifierTrialIndex(t, trials, "full")
	setModeClassifierSelector(
		&trials[fullIndex],
		"full",
		string(executionprofile.FullReasonHighStakesJudgment),
	)
	report = buildModeClassifierReport(time.Unix(0, 0), 3, 1, time.Second, trials)
	if !report.Passed ||
		report.FalseFastTrials != 0 ||
		report.WrongFullReasonTrials != 1 ||
		report.CorrectFullTrials != report.ExpectedFullTrials {
		t.Fatalf("a selected Full with a best-fit reason mismatch should remain mode-correct: %+v", report)
	}

	trials = allCorrectModeClassifierTrials(3)
	misclassifyDistinctFastTrials(t, trials, 6)
	report = buildModeClassifierReport(time.Unix(0, 0), 3, 1, time.Second, trials)
	if !report.Passed ||
		math.Abs(report.FastAccuracy-modeClassifierMinFastAccuracy) > 1e-9 ||
		report.FalseFullTrials != 6 {
		t.Fatalf("Fast accuracy at the soft boundary should pass: %+v", report)
	}

	trials = allCorrectModeClassifierTrials(3)
	misclassifyDistinctFastTrials(t, trials, 7)
	report = buildModeClassifierReport(time.Unix(0, 0), 3, 1, time.Second, trials)
	if report.Passed ||
		report.FastAccuracy >= modeClassifierMinFastAccuracy ||
		report.MajorityCorrectCases != 32 ||
		report.FalseFullTrials != 7 {
		t.Fatalf("Fast accuracy below the soft boundary was not gated: %+v", report)
	}

	trials = allCorrectModeClassifierTrials(3)
	misclassifyDistinctFullTrials(t, trials, 9)
	report = buildModeClassifierReport(time.Unix(0, 0), 3, 1, time.Second, trials)
	if !report.Passed ||
		math.Abs(report.FullAccuracy-modeClassifierMinFullAccuracy) > 1e-9 ||
		report.FalseFastTrials != 9 {
		t.Fatalf("Full accuracy at the soft boundary should pass: %+v", report)
	}

	trials = allCorrectModeClassifierTrials(3)
	misclassifyDistinctFullTrials(t, trials, 10)
	report = buildModeClassifierReport(time.Unix(0, 0), 3, 1, time.Second, trials)
	if report.Passed ||
		report.FullAccuracy >= modeClassifierMinFullAccuracy ||
		report.FalseFastTrials != 10 {
		t.Fatalf("Full accuracy below the soft boundary was not gated: %+v", report)
	}

	trials = allCorrectModeClassifierTrials(3)
	misclassifyDistinctFastTrials(t, trials, 6)
	misclassifyDistinctFullTrials(t, trials, 9)
	report = buildModeClassifierReport(time.Unix(0, 0), 3, 1, time.Second, trials)
	if report.Passed ||
		report.FastAccuracy < modeClassifierMinFastAccuracy ||
		report.FullAccuracy < modeClassifierMinFullAccuracy ||
		report.AggregateAccuracy >= modeClassifierMinAccuracy {
		t.Fatalf("aggregate routing accuracy below the soft boundary was not gated: %+v", report)
	}

	trials = allCorrectModeClassifierTrials(5)
	fastCaseID := modeClassifierCases[0].ID
	changed := 0
	for i := range trials {
		if trials[i].CaseID == fastCaseID && changed < 1 {
			setModeClassifierSelector(
				&trials[i],
				"full",
				string(executionprofile.FullReasonExplicitFullRequest),
			)
			changed++
		}
	}
	report = buildModeClassifierReport(time.Unix(0, 0), 5, 1, time.Second, trials)
	if !report.Passed || len(report.CaseThresholdFailures) != 0 {
		t.Fatalf("a case at 4/5 should pass without a low-accuracy diagnostic: %+v", report)
	}
	for i := range trials {
		if trials[i].CaseID == fastCaseID && trials[i].Correct {
			setModeClassifierSelector(
				&trials[i],
				"full",
				string(executionprofile.FullReasonExplicitFullRequest),
			)
			break
		}
	}
	report = buildModeClassifierReport(time.Unix(0, 0), 5, 1, time.Second, trials)
	if !report.Passed ||
		report.MajorityCorrectCases != 32 ||
		len(report.CaseThresholdFailures) != 1 ||
		report.CaseThresholdFailures[0] != fastCaseID {
		t.Fatalf("a case at 3/5 should be diagnostic rather than release-blocking: %+v", report)
	}

	trials = allCorrectModeClassifierTrials(3)
	changed = 0
	for i := range trials {
		if trials[i].CaseID == fastCaseID && changed < 2 {
			setModeClassifierSelector(
				&trials[i],
				"full",
				string(executionprofile.FullReasonExplicitFullRequest),
			)
			changed++
		}
	}
	report = buildModeClassifierReport(time.Unix(0, 0), 3, 1, time.Second, trials)
	if !report.Passed || report.MajorityCorrectCases != 31 {
		t.Fatalf("one incorrect per-case majority should remain visible but tolerated: %+v", report)
	}
}

func allCorrectModeClassifierTrials(repeats int) []modeClassifierTrial {
	trials := make([]modeClassifierTrial, 0, repeats*len(modeClassifierCases))
	for _, tc := range modeClassifierCases {
		for repeat := 1; repeat <= repeats; repeat++ {
			decision := executionprofile.AdmissionModeSelectedFast
			if tc.Expected == string(executionprofile.ModeFull) {
				decision = executionprofile.AdmissionModeSelectedFull
			}
			trials = append(trials, modeClassifierTrial{
				CaseID:              tc.ID,
				Category:            tc.Category,
				Expected:            tc.Expected,
				ExpectedReason:      tc.ExpectedReason,
				SelectorMode:        tc.Expected,
				SelectorFullReason:  tc.ExpectedReason,
				Observed:            tc.Expected,
				ObservedFullReason:  tc.ExpectedReason,
				AdmissionDecision:   decision,
				Correct:             true,
				ReasonCorrect:       true,
				Repeat:              repeat,
				Order:               repeat,
				DecisionLatencyMS:   int64(100 + repeat),
				ResponseDoneLatency: int64(200 + repeat),
			})
		}
	}
	return trials
}

func firstModeClassifierTrialIndex(t *testing.T, trials []modeClassifierTrial, expected string) int {
	t.Helper()
	for i := range trials {
		if trials[i].Expected == expected {
			return i
		}
	}
	t.Fatalf("no mode classifier trial with expected mode %q", expected)
	return -1
}

func setModeClassifierSelector(trial *modeClassifierTrial, mode, fullReason string) {
	trial.SelectorMode = mode
	trial.SelectorFullReason = fullReason
	admission := executionprofile.DecideModeAdmission(mode, fullReason)
	trial.Observed = selectedModeClassifierMode(admission)
	trial.ObservedFullReason = selectedModeClassifierReason(admission)
	trial.AdmissionDecision = admission.DecisionReason
	trial.Correct = trial.Observed == trial.Expected
	trial.ReasonCorrect = trial.ObservedFullReason == trial.ExpectedReason
}

func misclassifyDistinctFastTrials(t *testing.T, trials []modeClassifierTrial, count int) {
	t.Helper()
	changedCases := make(map[string]bool, count)
	for i := range trials {
		if trials[i].Expected != "fast" || changedCases[trials[i].CaseID] {
			continue
		}
		setModeClassifierSelector(
			&trials[i],
			"full",
			string(executionprofile.FullReasonExplicitFullRequest),
		)
		changedCases[trials[i].CaseID] = true
		if len(changedCases) == count {
			return
		}
	}
	t.Fatalf("changed %d distinct Fast cases, want %d", len(changedCases), count)
}

func misclassifyDistinctFullTrials(t *testing.T, trials []modeClassifierTrial, count int) {
	t.Helper()
	changedCases := make(map[string]bool, count)
	for i := range trials {
		if trials[i].Expected != "full" || changedCases[trials[i].CaseID] {
			continue
		}
		setModeClassifierSelector(&trials[i], "fast", string(executionprofile.FullReasonNone))
		changedCases[trials[i].CaseID] = true
		if len(changedCases) == count {
			return
		}
	}
	t.Fatalf("changed %d distinct Full cases, want %d", len(changedCases), count)
}

func TestKoeModeClassifierTextE2E(t *testing.T) {
	if os.Getenv(modeClassifierGate) != "1" {
		t.Skip("paid Realtime mode matrix: set KOE_MODE_CLASSIFIER_E2E=1")
	}
	repeats, err := modeClassifierEnvInt(modeClassifierRepeatsEnv, modeClassifierDefaultRepeat)
	if err != nil {
		t.Fatal(err)
	}
	caseTimeout, err := modeClassifierEnvDuration(modeClassifierTimeoutEnv, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	seed, err := modeClassifierEnvInt64(modeClassifierSeedEnv, modeClassifierDefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	variant, err := modeClassifierVariant(os.Getenv(modeClassifierVariantEnv))
	if err != nil {
		t.Fatal(err)
	}
	reportPath := strings.TrimSpace(os.Getenv(modeClassifierReportEnv))
	if reportPath == "" {
		reportPath = filepath.Join(os.TempDir(), "koe-terra-fast-qualification", "koe-mode-classifier.json")
	}

	startedAt := time.Now()
	suiteTimeout := time.Duration(repeats*len(modeClassifierCases))*caseTimeout + time.Duration(repeats)*30*time.Second
	suiteCtx, suiteCancel := context.WithTimeout(context.Background(), suiteTimeout)
	defer suiteCancel()

	trials := make([]modeClassifierTrial, 0, repeats*len(modeClassifierCases))
	for repeat := 1; repeat <= repeats; repeat++ {
		order := rand.New(rand.NewSource(seed + int64(repeat))).Perm(len(modeClassifierCases))
		for position, caseIndex := range order {
			tc := modeClassifierCases[caseIndex]
			caseCtx, cancel := context.WithTimeout(suiteCtx, caseTimeout)
			session, connectErr := newModeClassifierSessionForVariant(caseCtx, variant)
			if connectErr != nil {
				cancel()
				trial := unknownModeClassifierTrial(
					tc,
					repeat,
					position+1,
					fmt.Sprintf("connect Realtime: %v", connectErr),
				)
				trial.Variant = variant
				trials = append(trials, trial)
				t.Logf("repeat=%d order=%d case=%s expected=%s/%s observed=unknown error=%q",
					repeat, position+1, tc.ID, tc.Expected, tc.ExpectedReason, trial.Error)
				continue
			}

			trial := session.classify(caseCtx, tc, repeat, position+1)
			session.Close()
			cancel()
			trials = append(trials, trial)
			t.Logf("repeat=%d order=%d case=%s expected=%s/%s selector=%s/%s observed=%s/%s correct=%v decision=%dms done=%dms error=%q",
				repeat, position+1, tc.ID, tc.Expected, tc.ExpectedReason,
				trial.SelectorMode, trial.SelectorFullReason,
				trial.Observed, trial.ObservedFullReason, trial.Correct,
				trial.DecisionLatencyMS, trial.ResponseDoneLatency, trial.Error)
		}
	}

	report := buildModeClassifierReportForVariant(startedAt, repeats, seed, caseTimeout, variant, trials)
	if err := writeModeClassifierReport(reportPath, report); err != nil {
		t.Fatalf("write mode classifier report: %v", err)
	}
	t.Logf("mode classifier report=%s variant=%s attempts=%d correct=%d unknown=%d false_fast=%d false_full=%d fast_accuracy=%.3f full_accuracy=%.3f aggregate=%.3f majority=%d/%d tokens=%d passed=%v",
		reportPath, report.Variant, report.TrialCount, report.CorrectTrials, report.UnknownTrials,
		report.FalseFastTrials, report.FalseFullTrials, report.FastAccuracy, report.FullAccuracy,
		report.AggregateAccuracy, report.MajorityCorrectCases, report.CaseCount, report.TotalTokens, report.Passed)
	if !report.Passed {
		t.Fatalf("Koe mode classifier qualification failed: %s", strings.Join(report.FailureReasons, "; "))
	}
}

func modeClassifierVariant(value string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", modeClassifierVariantBaseline:
		return modeClassifierVariantBaseline, nil
	case modeClassifierVariantInstructionsOnly:
		return modeClassifierVariantInstructionsOnly, nil
	case modeClassifierVariantSchemaOnly:
		return modeClassifierVariantSchemaOnly, nil
	case modeClassifierVariantModeOnly:
		return modeClassifierVariantModeOnly, nil
	default:
		return "", fmt.Errorf(
			"%s must be %q, %q, %q, or %q",
			modeClassifierVariantEnv,
			modeClassifierVariantBaseline,
			modeClassifierVariantInstructionsOnly,
			modeClassifierVariantSchemaOnly,
			modeClassifierVariantModeOnly,
		)
	}
}

func modeClassifierSessionConfig(variant string) (map[string]any, error) {
	config := sessionConfig(e2ePersona, "marin", false)
	if variant == modeClassifierVariantBaseline {
		return config, nil
	}
	session, ok := config["session"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("sessionConfig omitted session object")
	}
	switch variant {
	case modeClassifierVariantInstructionsOnly:
		session["instructions"] = e2ePersona + "\n\n" + compactDualFieldExecutionInstructions
		return config, nil
	case modeClassifierVariantSchemaOnly, modeClassifierVariantModeOnly:
		tools, err := modeOnlyModeClassifierToolDefs()
		if err != nil {
			return nil, err
		}
		session["tools"] = tools
		if variant == modeClassifierVariantModeOnly {
			session["instructions"] = e2ePersona + "\n\n" + modeOnlyExecutionInstructions
		}
		return config, nil
	default:
		return nil, fmt.Errorf("unknown mode classifier variant %q", variant)
	}
}

func modeOnlyModeClassifierToolDefs() ([]ToolDef, error) {
	defs := append([]ToolDef(nil), ToolDefs()...)
	for i := range defs {
		if defs[i].Name != "do_task" {
			continue
		}
		var params map[string]any
		if err := json.Unmarshal(defs[i].Parameters, &params); err != nil {
			return nil, fmt.Errorf("decode production do_task schema: %w", err)
		}
		properties, ok := params["properties"].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("production do_task schema omitted properties")
		}
		delete(properties, "full_reason")
		if executionMode, ok := properties["execution_mode"].(map[string]any); ok {
			executionMode["description"] = "Choose fast by default. Choose full only for an explicit Full/deep-mode request or unusually high-stakes, broad, destructive, or long work that materially benefits from extra deliberation."
		}
		required, ok := params["required"].([]any)
		if !ok {
			return nil, fmt.Errorf("production do_task schema omitted required fields")
		}
		filtered := required[:0]
		for _, field := range required {
			if field != "full_reason" {
				filtered = append(filtered, field)
			}
		}
		params["required"] = filtered
		encoded, err := json.Marshal(params)
		if err != nil {
			return nil, fmt.Errorf("encode mode-only do_task schema: %w", err)
		}
		defs[i].Parameters = encoded
		return defs, nil
	}
	return nil, fmt.Errorf("production tool definitions omitted do_task")
}

func newModeClassifierSession(ctx context.Context) (*modeClassifierSession, error) {
	return newModeClassifierSessionForVariant(ctx, modeClassifierVariantBaseline)
}

func newModeClassifierSessionForVariant(ctx context.Context, variant string) (*modeClassifierSession, error) {
	ek, err := mintE2EEphemeral(ctx)
	if err != nil {
		return nil, fmt.Errorf("mint ephemeral token: %w", err)
	}
	audio, err := NewAudioIO()
	if err != nil {
		return nil, fmt.Errorf("create audio codec: %w", err)
	}
	// newPeerConnection still receives Realtime's audio modality, but every
	// decoded frame is dropped before the local speaker buffer.
	audio.SetPlaybackEnabled(false)
	rc, err := newPeerConnection(audio)
	if err != nil {
		audio.Stop()
		return nil, fmt.Errorf("create peer connection: %w", err)
	}
	session := &modeClassifierSession{
		rc:      rc,
		audio:   audio,
		variant: variant,
		events:  make(chan modeClassifierRealtimeEvent, 256),
		done:    make(chan struct{}),
	}
	config, err := modeClassifierSessionConfig(variant)
	if err != nil {
		session.Close()
		return nil, fmt.Errorf("build %s session config: %w", variant, err)
	}
	configErr := make(chan error, 1)
	rc.dc.OnOpen(func() {
		if err := session.send(config); err != nil {
			select {
			case configErr <- err:
			default:
			}
		}
	})
	rc.dc.OnMessage(func(message webrtc.DataChannelMessage) {
		var event modeClassifierRealtimeEvent
		if err := json.Unmarshal(message.Data, &event); err != nil {
			return
		}
		switch event.Type {
		case "session.updated", "response.created", "response.function_call_arguments.done", "response.done", "response.failed", "error":
		default:
			return
		}
		event.Raw = append(event.Raw[:0], message.Data...)
		select {
		case session.events <- event:
		case <-session.done:
		}
	})
	if err := rc.dialOpenAI(ctx, ek); err != nil {
		session.Close()
		return nil, fmt.Errorf("dial OpenAI: %w", err)
	}
	for {
		select {
		case err := <-configErr:
			session.Close()
			return nil, fmt.Errorf("send session config: %w", err)
		case event := <-session.events:
			switch event.Type {
			case "session.updated":
				return session, nil
			case "error", "response.failed":
				session.Close()
				return nil, fmt.Errorf("session config rejected: %s", modeClassifierEventError(event))
			}
		case <-ctx.Done():
			session.Close()
			return nil, fmt.Errorf("wait for session.updated: %w", ctx.Err())
		}
	}
}

func (s *modeClassifierSession) classify(
	ctx context.Context,
	tc modeClassifierCase,
	repeat int,
	order int,
) (trial modeClassifierTrial) {
	trial = unknownModeClassifierTrial(tc, repeat, order, "")
	trial.Variant = s.variant
	started := time.Now()
	trial.StartedAt = started.UTC()
	defer func() {
		trial.FinishedAt = time.Now().UTC()
	}()
	if err := s.send(map[string]any{
		"type": "conversation.item.create",
		"item": map[string]any{
			"type": "message",
			"role": "user",
			"content": []map[string]any{{
				"type": "input_text",
				"text": tc.Prompt,
			}},
		},
	}); err != nil {
		trial.Error = fmt.Sprintf("send user turn: %v", err)
		return trial
	}
	if err := s.send(map[string]any{"type": "response.create"}); err != nil {
		trial.Error = fmt.Sprintf("send response.create: %v", err)
		return trial
	}

	var calls []modeClassifierCapturedCall
	for {
		select {
		case event := <-s.events:
			switch event.Type {
			case "response.created":
				if trial.ResponseID == "" {
					trial.ResponseID = event.Response.ID
				}
			case "response.function_call_arguments.done":
				if trial.ResponseID != "" && event.ResponseID != "" && event.ResponseID != trial.ResponseID {
					continue
				}
				call := modeClassifierCapturedCall{name: event.Name, id: event.CallID}
				if event.Name == "do_task" {
					var args struct {
						Task          string `json:"task"`
						ExecutionMode string `json:"execution_mode"`
						FullReason    string `json:"full_reason"`
					}
					call.err = json.Unmarshal(unwrapArgs(event.Arguments), &args)
					call.mode = args.ExecutionMode
					call.fullReason = args.FullReason
					call.task = strings.TrimSpace(args.Task)
					if trial.DecisionLatencyMS == 0 {
						trial.DecisionLatencyMS = time.Since(started).Milliseconds()
					}
				}
				calls = append(calls, call)
				// Resolve every emitted function call without performing it so this
				// one-shot classification response can reach response.done.
				if err := s.send(map[string]any{
					"type": "conversation.item.create",
					"item": map[string]any{
						"type":    "function_call_output",
						"call_id": event.CallID,
						"output":  `{"status":"ok","reply":"Completed."}`,
					},
				}); err != nil {
					trial.Error = fmt.Sprintf("resolve function call: %v", err)
					return trial
				}
			case "response.done":
				if trial.ResponseID != "" && event.Response.ID != "" && event.Response.ID != trial.ResponseID {
					continue
				}
				trial.ResponseDoneLatency = time.Since(started).Milliseconds()
				trial.InputTokens = event.Response.Usage.InputTokens
				trial.OutputTokens = event.Response.Usage.OutputTokens
				trial.TotalTokens = event.Response.Usage.TotalTokens
				if event.Response.Status != "" && event.Response.Status != "completed" {
					trial.Error = fmt.Sprintf("response status %q", event.Response.Status)
					return trial
				}
				return finishModeClassifierTrial(trial, calls)
			case "error", "response.failed":
				trial.ResponseDoneLatency = time.Since(started).Milliseconds()
				trial.Error = modeClassifierEventError(event)
				return trial
			}
		case <-ctx.Done():
			trial.ResponseDoneLatency = time.Since(started).Milliseconds()
			trial.Error = fmt.Sprintf("classification timeout: %v", ctx.Err())
			return trial
		}
	}
}

func finishModeClassifierTrial(trial modeClassifierTrial, calls []modeClassifierCapturedCall) modeClassifierTrial {
	for _, call := range calls {
		trial.ToolCalls = append(trial.ToolCalls, call.name)
	}
	if len(calls) != 1 || calls[0].name != "do_task" {
		trial.Error = fmt.Sprintf("response emitted %d tool calls (%s), want exactly one do_task",
			len(calls), strings.Join(trial.ToolCalls, ","))
		return trial
	}
	call := calls[0]
	trial.CallID = call.id
	trial.DelegatedTask = call.task
	trial.SelectorMode = call.mode
	trial.SelectorFullReason = call.fullReason
	if call.err != nil {
		trial.Error = fmt.Sprintf("decode do_task arguments: %v", call.err)
		return trial
	}
	admission := executionprofile.DecideModeAdmission(call.mode, call.fullReason)
	trial.Observed = selectedModeClassifierMode(admission)
	trial.ObservedFullReason = selectedModeClassifierReason(admission)
	trial.AdmissionDecision = admission.DecisionReason
	trial.Correct = trial.Observed == trial.Expected
	trial.ReasonCorrect = trial.ObservedFullReason == trial.ExpectedReason
	if !trial.Correct {
		trial.Error = fmt.Sprintf(
			"selector=%s/%s scored=%s/%s, want mode %s",
			trial.SelectorMode,
			modeClassifierReportValue(trial.SelectorFullReason, "missing"),
			trial.Observed,
			trial.ObservedFullReason,
			trial.Expected,
		)
	}
	return trial
}

func selectedModeClassifierMode(admission executionprofile.ModeAdmission) string {
	switch admission.RequestedMode {
	case executionprofile.ModeFast, executionprofile.ModeFull:
		return string(admission.RequestedMode)
	default:
		return "unknown"
	}
}

func selectedModeClassifierReason(admission executionprofile.ModeAdmission) string {
	if admission.RequestedFullReason == "" {
		return string(executionprofile.FullReasonNone)
	}
	return string(admission.RequestedFullReason)
}

func (s *modeClassifierSession) send(value any) error {
	body, err := json.Marshal(value)
	if err != nil {
		return err
	}
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	return s.rc.dc.SendText(string(body))
}

func (s *modeClassifierSession) Close() {
	if s == nil {
		return
	}
	s.closeOnce.Do(func() {
		close(s.done)
		if s.rc != nil {
			s.rc.Close()
		}
		if s.audio != nil {
			s.audio.Stop()
		}
	})
}

func unknownModeClassifierTrial(tc modeClassifierCase, repeat, order int, errText string) modeClassifierTrial {
	return modeClassifierTrial{
		CaseID:             tc.ID,
		Category:           tc.Category,
		Expected:           tc.Expected,
		ExpectedReason:     tc.ExpectedReason,
		Observed:           "unknown",
		ObservedFullReason: "unknown",
		Repeat:             repeat,
		Order:              order,
		Error:              errText,
	}
}

func buildModeClassifierReport(
	startedAt time.Time,
	repeats int,
	seed int64,
	caseTimeout time.Duration,
	trials []modeClassifierTrial,
) modeClassifierReport {
	return buildModeClassifierReportForVariant(
		startedAt,
		repeats,
		seed,
		caseTimeout,
		modeClassifierVariantBaseline,
		trials,
	)
}

func buildModeClassifierReportForVariant(
	startedAt time.Time,
	repeats int,
	seed int64,
	caseTimeout time.Duration,
	variant string,
	trials []modeClassifierTrial,
) modeClassifierReport {
	report := modeClassifierReport{
		SchemaVersion:   modeClassifierReportSchema,
		Variant:         variant,
		AdmissionPolicy: modeClassifierAdmissionPolicy,
		StartedAt:       startedAt.UTC(),
		FinishedAt:      time.Now().UTC(),
		Model:           e2eModelName(),
		Provenance:      modeClassifierBuildProvenance(startedAt, seed, variant),
		LedgerSchema:    TaskLedgerEnabled(),
		Repeats:         repeats,
		Seed:            seed,
		CaseTimeout:     caseTimeout.String(),
		LatencyDefinition: map[string]string{
			"decision_latency_ms":      "from sending user text and response.create to receiving do_task arguments",
			"response_done_latency_ms": "from sending user text and response.create to response.done",
		},
		GateThresholds: map[string]float64{
			"aggregate_accuracy": modeClassifierMinAccuracy,
			"fast_accuracy":      modeClassifierMinFastAccuracy,
			"full_accuracy":      modeClassifierMinFullAccuracy,
		},
		CaseCount: len(modeClassifierCases),
		ReasonCounts: modeClassifierReasonCountSet{
			Expected: make(map[string]int),
			Selector: make(map[string]int),
			Observed: make(map[string]int),
		},
		AdmissionDecisions: make(map[string]int),
		TrialCount:         len(trials),
		PlannedTrialCount:  repeats * len(modeClassifierCases),
		Trials:             trials,
	}
	byCase := make(map[string][]modeClassifierTrial, len(modeClassifierCases))
	var decisionLatencies, responseLatencies []int64
	for _, tc := range modeClassifierCases {
		if tc.Expected == "fast" {
			report.ExpectedFastCases++
		} else {
			report.ExpectedFullCases++
		}
	}
	for _, trial := range trials {
		byCase[trial.CaseID] = append(byCase[trial.CaseID], trial)
		report.ReasonCounts.Expected[modeClassifierReportValue(trial.ExpectedReason, "unknown")]++
		report.ReasonCounts.Selector[modeClassifierReportValue(trial.SelectorFullReason, "missing")]++
		report.ReasonCounts.Observed[modeClassifierReportValue(trial.ObservedFullReason, "unknown")]++
		report.AdmissionDecisions[modeClassifierReportValue(trial.AdmissionDecision, "unknown")]++
		if trial.Correct {
			report.CorrectTrials++
		}
		switch trial.Expected {
		case "fast":
			report.ExpectedFastTrials++
			if trial.Correct {
				report.CorrectFastTrials++
			}
		case "full":
			report.ExpectedFullTrials++
			if trial.Correct {
				report.CorrectFullTrials++
			}
		}
		if trial.Observed == "unknown" {
			report.UnknownTrials++
		}
		if trial.Expected == "full" && trial.Observed == "fast" {
			report.FalseFastTrials++
		}
		if trial.Expected == "fast" && trial.Observed == "full" {
			report.FalseFullTrials++
		}
		if trial.Observed != "unknown" && trial.ObservedFullReason != trial.ExpectedReason {
			report.WrongReasonTrials++
			if trial.Expected == "full" {
				report.WrongFullReasonTrials++
			}
		}
		if trial.DecisionLatencyMS > 0 {
			decisionLatencies = append(decisionLatencies, trial.DecisionLatencyMS)
		}
		if trial.ResponseDoneLatency > 0 {
			responseLatencies = append(responseLatencies, trial.ResponseDoneLatency)
		}
		report.InputTokens += trial.InputTokens
		report.OutputTokens += trial.OutputTokens
		report.TotalTokens += trial.TotalTokens
	}
	if report.TrialCount > 0 {
		report.AggregateAccuracy = float64(report.CorrectTrials) / float64(report.TrialCount)
	}
	knownTrials := report.TrialCount - report.UnknownTrials
	if knownTrials > 0 {
		report.KnownAttemptAccuracy = float64(report.CorrectTrials) / float64(knownTrials)
	}
	if report.ExpectedFastTrials > 0 {
		report.FastAccuracy = float64(report.CorrectFastTrials) / float64(report.ExpectedFastTrials)
	}
	if report.ExpectedFullTrials > 0 {
		report.FullAccuracy = float64(report.CorrectFullTrials) / float64(report.ExpectedFullTrials)
	}

	var caseTrialCountFailures []string
	for _, tc := range modeClassifierCases {
		caseTrials := byCase[tc.ID]
		summary := modeClassifierCaseSummary{
			ID:             tc.ID,
			Category:       tc.Category,
			Expected:       tc.Expected,
			ExpectedReason: tc.ExpectedReason,
			Prompt:         tc.Prompt,
			Repeats:        len(caseTrials),
			Majority:       "unknown",
			ReasonVotes:    make(map[string]int),
			MajorityReason: "unknown",
		}
		var correct int
		var caseDecisionLatencies, caseResponseLatencies []int64
		for _, trial := range caseTrials {
			switch trial.Observed {
			case "fast":
				summary.FastVotes++
			case "full":
				summary.FullVotes++
			default:
				summary.UnknownVotes++
			}
			summary.ReasonVotes[modeClassifierReportValue(trial.ObservedFullReason, "unknown")]++
			if trial.Correct {
				correct++
			}
			if trial.DecisionLatencyMS > 0 {
				caseDecisionLatencies = append(caseDecisionLatencies, trial.DecisionLatencyMS)
			}
			if trial.ResponseDoneLatency > 0 {
				caseResponseLatencies = append(caseResponseLatencies, trial.ResponseDoneLatency)
			}
		}
		if summary.FastVotes > len(caseTrials)/2 {
			summary.Majority = "fast"
		} else if summary.FullVotes > len(caseTrials)/2 {
			summary.Majority = "full"
		}
		for reason, votes := range summary.ReasonVotes {
			if votes > len(caseTrials)/2 {
				summary.MajorityReason = reason
				break
			}
		}
		summary.CorrectVotes = correct
		summary.MajorityCorrect = correct > len(caseTrials)/2
		if summary.MajorityCorrect {
			report.MajorityCorrectCases++
		}
		if len(caseTrials) > 0 {
			summary.AttemptAccuracy = float64(correct) / float64(len(caseTrials))
		}
		summary.DecisionLatencyMS = modeClassifierLatencyStats(caseDecisionLatencies)
		summary.ResponseDoneLatencyMS = modeClassifierLatencyStats(caseResponseLatencies)
		report.Cases = append(report.Cases, summary)
		if len(caseTrials) != repeats {
			caseTrialCountFailures = append(
				caseTrialCountFailures,
				fmt.Sprintf("%s=%d/%d", tc.ID, len(caseTrials), repeats),
			)
		}
		if repeats >= 5 && summary.AttemptAccuracy < modeClassifierMinCaseAccuracy {
			report.CaseThresholdFailures = append(report.CaseThresholdFailures, tc.ID)
		}
	}
	if report.CaseCount > 0 {
		report.MajorityAccuracy = float64(report.MajorityCorrectCases) / float64(report.CaseCount)
	}
	report.DecisionLatencyMS = modeClassifierLatencyStats(decisionLatencies)
	report.ResponseDoneLatencyMS = modeClassifierLatencyStats(responseLatencies)

	if report.TrialCount != report.PlannedTrialCount {
		report.FailureReasons = append(report.FailureReasons,
			fmt.Sprintf("completed %d/%d trials", report.TrialCount, report.PlannedTrialCount))
	}
	if len(caseTrialCountFailures) != 0 {
		report.FailureReasons = append(report.FailureReasons,
			fmt.Sprintf("per-case trial counts: %s", strings.Join(caseTrialCountFailures, ", ")))
	}
	if report.UnknownTrials != 0 {
		report.FailureReasons = append(report.FailureReasons,
			fmt.Sprintf("%d trials were unknown", report.UnknownTrials))
	}
	if report.FullAccuracy < modeClassifierMinFullAccuracy {
		report.FailureReasons = append(report.FailureReasons,
			fmt.Sprintf(
				"Full routing accuracy %.3f is below %.2f (false Fast=%d)",
				report.FullAccuracy,
				modeClassifierMinFullAccuracy,
				report.FalseFastTrials,
			))
	}
	if report.FastAccuracy < modeClassifierMinFastAccuracy {
		report.FailureReasons = append(report.FailureReasons,
			fmt.Sprintf("Fast accuracy %.3f is below %.2f", report.FastAccuracy, modeClassifierMinFastAccuracy))
	}
	if report.AggregateAccuracy < modeClassifierMinAccuracy {
		report.FailureReasons = append(report.FailureReasons,
			fmt.Sprintf(
				"aggregate routing accuracy %.3f is below %.2f",
				report.AggregateAccuracy,
				modeClassifierMinAccuracy,
			))
	}
	report.Passed = len(report.FailureReasons) == 0
	return report
}

func modeClassifierBuildProvenance(
	startedAt time.Time,
	seed int64,
	variant string,
) modeClassifierProvenance {
	provenance := modeClassifierProvenance{
		RunID: fmt.Sprintf(
			"%s-%s-%d",
			variant,
			startedAt.UTC().Format("20060102T150405.000000000Z"),
			seed,
		),
		GoVersion:         runtime.Version(),
		RuntimeOS:         runtime.GOOS,
		RuntimeArch:       runtime.GOARCH,
		CaseSetSHA256:     modeClassifierHashJSON(modeClassifierCases),
		PersonaSHA256:     modeClassifierHashBytes([]byte(e2ePersona)),
		ChangedDimensions: modeClassifierChangedDimensions(variant),
	}
	if runID := strings.TrimSpace(os.Getenv("KOE_AGENT_LAB_RUN_ID")); runID != "" {
		provenance.RunID = runID
	}
	if sourceCommit := strings.TrimSpace(os.Getenv("KOE_AGENT_LAB_SOURCE_COMMIT")); sourceCommit != "" {
		provenance.SourceCommit = sourceCommit
	}
	if sourceDirty := strings.TrimSpace(os.Getenv("KOE_AGENT_LAB_SOURCE_DIRTY")); sourceDirty != "" {
		provenance.SourceDirty = sourceDirty == "true"
	}
	if buildInfo, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range buildInfo.Settings {
			switch setting.Key {
			case "vcs.revision":
				if provenance.SourceCommit == "" {
					provenance.SourceCommit = setting.Value
				}
			case "vcs.modified":
				if strings.TrimSpace(os.Getenv("KOE_AGENT_LAB_SOURCE_DIRTY")) == "" {
					provenance.SourceDirty = setting.Value == "true"
				}
			}
		}
	}
	config, err := modeClassifierSessionConfig(variant)
	if err != nil {
		return provenance
	}
	provenance.SessionConfigSHA256 = modeClassifierHashJSON(config)
	session, ok := config["session"].(map[string]any)
	if !ok {
		return provenance
	}
	if instructions, ok := session["instructions"].(string); ok {
		provenance.InstructionsSHA256 = modeClassifierHashBytes([]byte(instructions))
	}
	if tools, found := session["tools"]; found {
		provenance.ToolSchemaSHA256 = modeClassifierHashJSON(tools)
	}
	return provenance
}

func modeClassifierChangedDimensions(variant string) []string {
	switch variant {
	case modeClassifierVariantBaseline:
		return []string{}
	case modeClassifierVariantInstructionsOnly:
		return []string{"instructions"}
	case modeClassifierVariantSchemaOnly:
		return []string{"tool_schema"}
	case modeClassifierVariantModeOnly:
		return []string{"instructions", "tool_schema"}
	default:
		return []string{"unknown"}
	}
}

func modeClassifierHashJSON(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return modeClassifierHashBytes(encoded)
}

func modeClassifierHashBytes(value []byte) string {
	sum := sha256.Sum256(value)
	return fmt.Sprintf("sha256:%x", sum)
}

func modeClassifierReportValue(value, fallback string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return fallback
	}
	return value
}

func modeClassifierLatencyStats(values []int64) modeClassifierLatencySet {
	if len(values) == 0 {
		return modeClassifierLatencySet{}
	}
	sorted := append([]int64(nil), values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	var total int64
	for _, value := range sorted {
		total += value
	}
	p95Index := int(math.Ceil(float64(len(sorted))*0.95)) - 1
	if p95Index < 0 {
		p95Index = 0
	}
	return modeClassifierLatencySet{
		Count:  len(sorted),
		Min:    sorted[0],
		Mean:   float64(total) / float64(len(sorted)),
		Median: sorted[(len(sorted)-1)/2],
		P95:    sorted[p95Index],
		Max:    sorted[len(sorted)-1],
	}
}

func modeClassifierEventError(event modeClassifierRealtimeEvent) string {
	if message := strings.TrimSpace(event.Error.Message); message != "" {
		if event.Error.Code != "" {
			return fmt.Sprintf("%s: %s", event.Error.Code, message)
		}
		return message
	}
	if len(event.Raw) > 0 {
		return string(event.Raw)
	}
	return event.Type
}

func writeModeClassifierReport(path string, report modeClassifierReport) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	body, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	tempPath := path + ".tmp"
	if err := os.WriteFile(tempPath, body, 0o644); err != nil {
		return err
	}
	return os.Rename(tempPath, path)
}

func modeClassifierEnvInt(name string, fallback int) (int, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 1 {
		return 0, fmt.Errorf("%s must be a positive integer, got %q", name, raw)
	}
	return value, nil
}

func modeClassifierEnvInt64(name string, fallback int64) (int64, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer, got %q", name, raw)
	}
	return value, nil
}

func modeClassifierEnvDuration(name string, fallback time.Duration) (time.Duration, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback, nil
	}
	value, err := time.ParseDuration(raw)
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("%s must be a positive Go duration, got %q", name, raw)
	}
	return value, nil
}
