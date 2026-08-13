#!/usr/bin/env bash
set -uo pipefail
export PYTHONDONTWRITEBYTECODE=1

repo_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
lane="${AGENT_LAB_LANE:-offline}"
output_dir="${1:-${TMPDIR:-/tmp}/kocoro-agent-lab/$lane}"
quality_release_repetitions=15
provider_release_repetitions=30
mkdir -p "$output_dir"

case "$lane" in
  offline|provider_live|quality_live|provider_release|quality_release) ;;
  *)
    echo "AGENT_LAB_LANE must be offline, provider_live, quality_live, provider_release, or quality_release" >&2
    exit 2
    ;;
esac

run_id="agent-lab-$lane-$(date -u +%Y%m%dT%H%M%SZ)"
started_at="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
source_commit="$(git -C "$repo_dir" rev-parse HEAD)"
source_dirty=false
if [[ -n "$(git -C "$repo_dir" status --porcelain)" ]]; then
  source_dirty=true
fi

check_names=()
check_statuses=()

run_check() {
  local name="$1"
  shift
  local status=0
  "$@" 2>&1 | tee "$output_dir/$name.log" || status="${PIPESTATUS[0]}"
  check_names+=("$name")
  check_statuses+=("$status")
}

require_tests_executed() {
  local name="$1"
  local log_path="$2"
  shift 2
  local status=0
  : > "$output_dir/$name.log"
  for test_name in "$@"; do
    if ! grep -Eq "^--- (PASS|FAIL): ${test_name}([[:space:]]|$)" "$log_path"; then
      echo "Expected test $test_name did not execute (a skipped or unmatched test is not qualifying)." \
        2>&1 | tee -a "$output_dir/$name.log"
      status=2
    else
      echo "Verified test execution: $test_name" | tee -a "$output_dir/$name.log"
    fi
  done
  check_names+=("$name")
  check_statuses+=("$status")
}

validate_quality_report() {
  python3 - "$@" <<'PY'
import json
import math
import pathlib
import sys

path = pathlib.Path(sys.argv[1])
expected_sample = sys.argv[2]
expected_repetitions = int(sys.argv[3])
expected_seed = int(sys.argv[4])
require_release = sys.argv[5] == "1"

def reject_duplicates(pairs):
    value = {}
    for key, item in pairs:
        if key in value:
            raise ValueError(f"duplicate JSON key: {key}")
        value[key] = item
    return value

def fail(message):
    raise ValueError(message)

try:
    if not path.is_file():
        fail(f"report does not exist: {path}")
    report = json.loads(path.read_text(encoding="utf-8"), object_pairs_hook=reject_duplicates)
    if report.get("schema_version") != "kocoro.agent_lab_quality.v1":
        fail("unexpected schema_version")
    if report.get("sample") != expected_sample:
        fail(f"sample={report.get('sample')!r}, expected {expected_sample!r}")
    if type(report.get("repetitions_per_case")) is not int or report["repetitions_per_case"] != expected_repetitions:
        fail("repetitions_per_case does not match the requested repetitions")
    if type(report.get("seed")) is not int or report["seed"] != expected_seed:
        fail("seed does not match the requested seed")
    scheduled = report.get("scheduled")
    completed = report.get("completed")
    if type(scheduled) is not int or scheduled <= 0:
        fail("scheduled must be a positive integer")
    if type(completed) is not int or completed != scheduled:
        fail(f"completed={completed!r} does not equal scheduled={scheduled!r}")
    if report.get("complete") is not True:
        fail("complete is not true")
    runs = report.get("runs")
    cases = report.get("cases")
    if not isinstance(runs, list) or len(runs) != completed:
        fail("runs length does not equal completed")
    if not isinstance(cases, list) or not cases:
        fail("cases must be a non-empty list")
    if any(not isinstance(item, dict) for item in cases):
        fail("every case summary must be an object")
    if sum(item.get("runs", -1) for item in cases) != completed:
        fail("case run totals do not equal completed")
    if any(type(item.get("runs")) is not int or item["runs"] != expected_repetitions for item in cases):
        fail("a case does not contain the requested repetitions")
    if report.get("correct_runs") != completed or report.get("failures") != []:
        fail("not every completed run is correct")
    if report.get("comparison_qualifying") is not True:
        fail("comparison_qualifying is not true")
    if report.get("usage_observed") is not True or report.get("cost_observed") is not True:
        fail("provider usage and cost observations are required")
    reported_cost = report.get("reported_cost_usd")
    max_cost = report.get("max_cost_usd")
    if (isinstance(reported_cost, bool) or not isinstance(reported_cost, (int, float)) or
            isinstance(max_cost, bool) or not isinstance(max_cost, (int, float)) or
            not math.isfinite(reported_cost) or not math.isfinite(max_cost) or
            reported_cost < 0 or max_cost <= 0):
        fail("reported_cost_usd and max_cost_usd must be finite valid numbers")
    if reported_cost > max_cost:
        fail(f"reported_cost_usd={reported_cost} exceeds max_cost_usd={max_cost}")
    if require_release and report.get("release_qualifying") is not True:
        fail("release_qualifying is not true")
except (OSError, UnicodeError, json.JSONDecodeError, TypeError, ValueError) as error:
    print(f"Quality report validation failed: {error}", file=sys.stderr)
    sys.exit(1)

print(f"Validated quality report: complete={completed} scheduled={scheduled} sample={expected_sample} repetitions={expected_repetitions} release_qualifying={report.get('release_qualifying')}")
PY
}

validate_provider_report() {
  python3 - "$@" <<'PY'
import json
import pathlib
import sys

path = pathlib.Path(sys.argv[1])
expected_sample = sys.argv[2]
expected_repetitions = int(sys.argv[3])
expected_seed = int(sys.argv[4])
require_release = sys.argv[5] == "1"

def reject_duplicates(pairs):
    value = {}
    for key, item in pairs:
        if key in value:
            raise ValueError(f"duplicate JSON key: {key}")
        value[key] = item
    return value

def fail(message):
    raise ValueError(message)

try:
    if not path.is_file():
        fail(f"report does not exist: {path}")
    report = json.loads(path.read_text(encoding="utf-8"), object_pairs_hook=reject_duplicates)
    if report.get("schema_version") != 4:
        fail("unexpected schema_version")
    if type(report.get("repetitions_per_cell")) is not int or report["repetitions_per_cell"] != expected_repetitions:
        fail("repetitions_per_cell does not match the requested repetitions")
    if type(report.get("seed")) is not int or report["seed"] != expected_seed:
        fail("seed does not match the requested seed")
    if report.get("smoke") is not (expected_sample == "smoke"):
        fail("smoke flag does not match the requested sample")
    scheduled = report.get("scheduled")
    completed = report.get("completed")
    if type(scheduled) is not int or scheduled <= 0:
        fail("scheduled must be a positive integer")
    if type(completed) is not int or completed != scheduled:
        fail(f"completed={completed!r} does not equal scheduled={scheduled!r}")
    if report.get("complete") is not True:
        fail("complete is not true")
    runs = report.get("runs")
    if not isinstance(runs, list) or len(runs) != completed:
        fail("runs length does not equal completed")
    if report.get("gate_passed") is not True or report.get("correctness_gate_passed") is not True:
        fail("provider correctness gate did not pass")
    failure_count_fields = (
        "contract_failure_count",
        "runtime_failure_count",
        "duplicate_side_effect_failure_count",
        "cost_failure_count",
    )
    if any(type(report.get(field)) is not int or report[field] != 0 for field in failure_count_fields):
        fail("provider contract, runtime, duplicate side-effect, or cost failure counts are non-zero")
    if require_release:
        if report.get("sample_qualifying") is not True:
            fail("release sample_qualifying is not true")
        if report.get("performance_gate_passed") is not True:
            fail("release performance gate did not pass")
except (OSError, UnicodeError, json.JSONDecodeError, TypeError, ValueError) as error:
    print(f"Provider report validation failed: {error}", file=sys.stderr)
    sys.exit(1)

print(f"Validated provider report: complete={completed} scheduled={scheduled} sample={expected_sample} repetitions={expected_repetitions} sample_qualifying={report.get('sample_qualifying')}")
PY
}

run_offline_lane() {
  run_check loop_detector_corpus env \
    AGENT_LOOP_HARNESS_REPORT="$output_dir/loop-detector.json" \
    go test ./internal/agent -run '^TestLoopDetectorAcceptanceCorpus$' -count=1 -v
  require_tests_executed loop_detector_corpus_execution \
    "$output_dir/loop_detector_corpus.log" \
    TestLoopDetectorAcceptanceCorpus
  run_check loop_runtime_progress go test ./internal/agent \
    -run '^Test(AgentLoopReadOnlyPollingChangingOutcomeReachesCompletion|AgentLoopCriticalReadLoopBlocksWholeBatchBeforeExecution|StreamToolStarterDisablesSpeculationAfterLoopWarning)$' \
    -count=1 -v
  require_tests_executed loop_runtime_progress_execution \
    "$output_dir/loop_runtime_progress.log" \
    TestAgentLoopReadOnlyPollingChangingOutcomeReachesCompletion \
    TestAgentLoopCriticalReadLoopBlocksWholeBatchBeforeExecution \
    TestStreamToolStarterDisablesSpeculationAfterLoopWarning
  run_check mcp_dispatch_faults go test ./internal/tools \
    -run '^TestMCPTool_Run_(WriteToolNotReplayedAfterPostDispatchTransportError|IdempotentToolReplayedAfterTransportError|NoRetryOnNonTransportError|NoRetryOnPerCallTimeoutError|NoRetryAfterCtxCancel)$' \
    -count=1 -v
  require_tests_executed mcp_dispatch_faults_execution \
    "$output_dir/mcp_dispatch_faults.log" \
    TestMCPTool_Run_WriteToolNotReplayedAfterPostDispatchTransportError \
    TestMCPTool_Run_IdempotentToolReplayedAfterTransportError \
    TestMCPTool_Run_NoRetryOnNonTransportError \
    TestMCPTool_Run_NoRetryOnPerCallTimeoutError \
    TestMCPTool_Run_NoRetryAfterCtxCancel
  run_check mcp_outcome_contract go test ./internal/mcp \
    -run '^Test(ToolReplaySafe_AnnotationGating|OutcomeUnknownError_PreservesTransportClassification|ReplaySafeFromCache_MissAndAnnotations)$' \
    -count=1 -v
  require_tests_executed mcp_outcome_contract_execution \
    "$output_dir/mcp_outcome_contract.log" \
    TestToolReplaySafe_AnnotationGating \
    TestOutcomeUnknownError_PreservesTransportClassification \
    TestReplaySafeFromCache_MissAndAnnotations
  run_check daemon_idempotency go test ./internal/daemon \
    -run '^Test(RunAgent_IdempotencyKeyReturnsCompletedRunWithoutSecondLLMCall|RunAgent_FailedIdempotentRequestNeverReplaysAutomatically|TerminalIdempotencyState_SoftFailureWithoutDeliverableFailsClosed|TerminalIdempotencyState_DeliverableIsDurableSuccessEvidence|CompletedIdempotentResultReplaysDeliveryReceiptAndStatus)$' \
    -count=1 -v
  require_tests_executed daemon_idempotency_execution \
    "$output_dir/daemon_idempotency.log" \
    TestRunAgent_IdempotencyKeyReturnsCompletedRunWithoutSecondLLMCall \
    TestRunAgent_FailedIdempotentRequestNeverReplaysAutomatically \
    TestTerminalIdempotencyState_SoftFailureWithoutDeliverableFailsClosed \
    TestTerminalIdempotencyState_DeliverableIsDurableSuccessEvidence \
    TestCompletedIdempotentResultReplaysDeliveryReceiptAndStatus
  run_check general_purpose_runtime go test ./test/e2e \
    -run '^TestOffline_AgentLab(GeneralPurposePromptContract|LongReadTrajectoryReachesOutcome|CompactionPersistsAcrossRestart|InterruptedTrajectoryResumesWithoutReplay)$' \
    -count=1 -v
  require_tests_executed general_purpose_runtime_execution \
    "$output_dir/general_purpose_runtime.log" \
    TestOffline_AgentLabGeneralPurposePromptContract \
    TestOffline_AgentLabLongReadTrajectoryReachesOutcome \
    TestOffline_AgentLabCompactionPersistsAcrossRestart \
    TestOffline_AgentLabInterruptedTrajectoryResumesWithoutReplay
  run_check harness_self_test go test ./test/e2e \
    -run '^TestOffline_(AgentLabScriptsParse|AgentLabLaneReferencesResolve|ProviderQualificationRejectsUndersizedReleaseSample)$' \
    -count=1 -v
  require_tests_executed harness_self_test_execution \
    "$output_dir/harness_self_test.log" \
    TestOffline_AgentLabScriptsParse \
    TestOffline_AgentLabLaneReferencesResolve \
    TestOffline_ProviderQualificationRejectsUndersizedReleaseSample
  run_check quality_harness_self_test go test -tags=live ./test/e2e \
    -run '^TestOffline_AgentLab(Quality(ContractValidators|QualificationFailsClosed|LaneRequiresExplicitPaidGate|LaneRejectsUndersizedReleaseSample|LaneReportValidation)|ProviderLaneReportValidation)$' \
    -count=1 -v
  require_tests_executed quality_harness_self_test_execution \
    "$output_dir/quality_harness_self_test.log" \
    TestOffline_AgentLabQualityContractValidators \
    TestOffline_AgentLabQualityQualificationFailsClosed \
    TestOffline_AgentLabQualityLaneRequiresExplicitPaidGate \
    TestOffline_AgentLabQualityLaneRejectsUndersizedReleaseSample \
    TestOffline_AgentLabQualityLaneReportValidation \
    TestOffline_AgentLabProviderLaneReportValidation
}

run_provider_lane() {
  local required_sample="${1:-smoke}"
  if [[ "${KOE_PROVIDER_AGENTLOOP_E2E:-}" != "1" ]]; then
    echo "Set KOE_PROVIDER_AGENTLOOP_E2E=1 to authorize paid provider qualification." >&2
    check_names+=("provider_agentloop_live")
    check_statuses+=("2")
    return
  fi
  if [[ "$required_sample" == "release" && "${KOE_PROVIDER_SAMPLE:-}" != "release" ]]; then
    echo "provider_release requires KOE_PROVIDER_SAMPLE=release and at least $provider_release_repetitions repetitions." >&2
    check_names+=("provider_agentloop_live")
    check_statuses+=("2")
    return
  fi
  local sample="${KOE_PROVIDER_SAMPLE:-smoke}"
  local repetitions="${KOE_PROVIDER_REPETITIONS:-1}"
  local seed="${KOE_PROVIDER_SEED:-20260728}"
  run_check provider_agentloop_live \
    "$repo_dir/scripts/koe-provider-qualification.sh" \
    "$output_dir/provider"
  require_tests_executed provider_agentloop_execution \
    "$output_dir/provider/provider-qualification.log" \
    TestKoeFastQualificationLive_AgentLoop
  run_check provider_agentloop_report validate_provider_report \
    "$output_dir/provider/provider-qualification.json" \
    "$sample" "$repetitions" "$seed" \
    "$([[ "$required_sample" == "release" ]] && echo 1 || echo 0)"
}

run_quality_lane() {
  local required_sample="${1:-}"
  if [[ "${KOCORO_AGENT_LAB_QUALITY_LIVE:-}" != "1" ]]; then
    echo "Set KOCORO_AGENT_LAB_QUALITY_LIVE=1 to authorize the paid general-purpose quality lane." >&2
    check_names+=("general_purpose_quality_live")
    check_statuses+=("2")
    return
  fi
  local repetitions="${KOCORO_AGENT_LAB_QUALITY_REPETITIONS:-3}"
  local sample="${KOCORO_AGENT_LAB_QUALITY_SAMPLE:-smoke}"
  local seed="${KOCORO_AGENT_LAB_QUALITY_SEED:-20260807}"
  if [[ ! "$repetitions" =~ ^[0-9]+$ || "$repetitions" -lt 1 || "$repetitions" -gt 100 ]]; then
    echo "KOCORO_AGENT_LAB_QUALITY_REPETITIONS must be an integer from 1 through 100." >&2
    check_names+=("general_purpose_quality_live")
    check_statuses+=("2")
    return
  fi
  if [[ "$sample" != "smoke" && "$sample" != "release" ]]; then
    echo "KOCORO_AGENT_LAB_QUALITY_SAMPLE must be smoke or release." >&2
    check_names+=("general_purpose_quality_live")
    check_statuses+=("2")
    return
  fi
  if [[ "$sample" == "release" && "$repetitions" -lt "$quality_release_repetitions" ]]; then
    echo "release quality sample requires KOCORO_AGENT_LAB_QUALITY_REPETITIONS >= $quality_release_repetitions." >&2
    check_names+=("general_purpose_quality_live")
    check_statuses+=("2")
    return
  fi
  if [[ -n "$required_sample" && "$sample" != "$required_sample" ]]; then
    echo "quality_release requires KOCORO_AGENT_LAB_QUALITY_SAMPLE=release and at least $quality_release_repetitions repetitions." >&2
    check_names+=("general_purpose_quality_live")
    check_statuses+=("2")
    return
  fi
  if [[ "$sample" == "release" ]] && ! run_release_source_preflight; then
    return
  fi
  run_check general_purpose_quality_live env \
    SHANNON_E2E_LIVE=1 \
    KOCORO_AGENT_LAB_QUALITY_LIVE=1 \
    KOCORO_AGENT_LAB_QUALITY_SAMPLE="$sample" \
    KOCORO_AGENT_LAB_QUALITY_REPETITIONS="$repetitions" \
    KOCORO_AGENT_LAB_QUALITY_OUTPUT="$output_dir/general-purpose-quality.json" \
    go test -tags=live ./test/e2e -run '^TestLive_AgentLabGeneralPurposeQuality$' \
    -count=1 -v -timeout=60m
  require_tests_executed general_purpose_quality_execution \
    "$output_dir/general_purpose_quality_live.log" \
    TestLive_AgentLabGeneralPurposeQuality
  run_check general_purpose_quality_report validate_quality_report \
    "$output_dir/general-purpose-quality.json" \
    "$sample" "$repetitions" "$seed" \
    "$([[ "$sample" == "release" ]] && echo 1 || echo 0)"
}

run_release_source_preflight() {
  if [[ "$source_dirty" == "true" ]]; then
    echo "$lane requires a clean ShanClaw source tree." >&2
    check_names+=("release_source_clean")
    check_statuses+=("2")
    return 1
  fi
  return 0
}

cd "$repo_dir" || exit 2
case "$lane" in
  offline)
    run_offline_lane
    ;;
  provider_live)
    run_provider_lane smoke
    ;;
  quality_live)
    run_quality_lane
    ;;
  quality_release)
    run_quality_lane release
    ;;
  provider_release)
    if run_release_source_preflight; then
      run_offline_lane
      run_provider_lane release
    fi
    ;;
esac

finished_at="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
manifest_args=()
overall_status=0
for index in "${!check_names[@]}"; do
  manifest_args+=("${check_names[$index]}" "${check_statuses[$index]}")
  if [[ "${check_statuses[$index]}" -ne 0 ]]; then
    overall_status=1
  fi
done

RUN_ID="$run_id" LANE="$lane" STARTED_AT="$started_at" FINISHED_AT="$finished_at" \
SOURCE_COMMIT="$source_commit" SOURCE_DIRTY="$source_dirty" \
OUTPUT_DIR="$output_dir" OVERALL_STATUS="$overall_status" \
python3 -c '
import json, os, pathlib, sys
checks = []
for index in range(2, len(sys.argv), 2):
    checks.append({"name": sys.argv[index], "exit_status": int(sys.argv[index + 1])})
output = pathlib.Path(os.environ["OUTPUT_DIR"])
manifest = {
    "schema_version": "kocoro.agent_lab_manifest.v1",
    "run_id": os.environ["RUN_ID"],
    "lane": os.environ["LANE"],
    "started_at": os.environ["STARTED_AT"],
    "finished_at": os.environ["FINISHED_AT"],
    "source": {
        "commit": os.environ["SOURCE_COMMIT"],
        "dirty": os.environ["SOURCE_DIRTY"] == "true",
    },
    "qualification_scope": "agent_runtime",
    "checks": checks,
    "passed": os.environ["OVERALL_STATUS"] == "0",
    "coverage_boundaries": [
        "No physical microphone, acoustic echo, VAD, or human barge-in qualification.",
        "No signed-in external-service write with post-commit process crash and receipt recovery.",
    ],
    "measurement_notes": [
        "Provider token usage, provider USD cost, and customer quota are separate measures.",
    ],
}
(output / "manifest.json").write_text(
    json.dumps(manifest, ensure_ascii=False, indent=2) + "\n", encoding="utf-8"
)
' agent-lab "${manifest_args[@]}" || overall_status=1

echo "Agent lab artifacts: $output_dir"
exit "$overall_status"
