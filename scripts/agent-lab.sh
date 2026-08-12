#!/usr/bin/env bash
set -uo pipefail
export PYTHONDONTWRITEBYTECODE=1

repo_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
lane="${AGENT_LAB_LANE:-offline}"
output_dir="${1:-${TMPDIR:-/tmp}/kocoro-agent-lab/$lane}"
mkdir -p "$output_dir"

case "$lane" in
  offline|provider_live|quality_live|provider_release) ;;
  *)
    echo "AGENT_LAB_LANE must be offline, provider_live, quality_live, or provider_release" >&2
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

run_offline_lane() {
  run_check loop_detector_corpus env \
    AGENT_LOOP_HARNESS_REPORT="$output_dir/loop-detector.json" \
    go test ./internal/agent -run '^TestLoopDetectorAcceptanceCorpus$' -count=1 -v
  run_check loop_runtime_progress go test ./internal/agent \
    -run '^Test(AgentLoopReadOnlyPollingChangingOutcomeReachesCompletion|AgentLoopCriticalReadLoopBlocksWholeBatchBeforeExecution|StreamToolStarterDisablesSpeculationAfterLoopWarning)$' \
    -count=1 -v
  run_check mcp_dispatch_faults go test ./internal/tools \
    -run '^TestMCPTool_Run_(WriteToolNotReplayedAfterPostDispatchTransportError|IdempotentToolReplayedAfterTransportError|NoRetryOnNonTransportError|NoRetryOnPerCallTimeoutError|NoRetryAfterCtxCancel)$' \
    -count=1 -v
  run_check mcp_outcome_contract go test ./internal/mcp \
    -run '^Test(ToolReplaySafe_AnnotationGating|OutcomeUnknownError_PreservesTransportClassification|ReplaySafeFromCache_MissAndAnnotations)$' \
    -count=1 -v
  run_check daemon_idempotency go test ./internal/daemon \
    -run '^Test(RunAgent_IdempotencyKeyReturnsCompletedRunWithoutSecondLLMCall|RunAgent_FailedIdempotentRequestNeverReplaysAutomatically|TerminalIdempotencyState_SoftFailureWithoutDeliverableFailsClosed|TerminalIdempotencyState_DeliverableIsDurableSuccessEvidence|CompletedIdempotentResultReplaysDeliveryReceiptAndStatus)$' \
    -count=1 -v
  run_check general_purpose_runtime go test ./test/e2e \
    -run '^TestOffline_AgentLab(GeneralPurposePromptContract|LongReadTrajectoryReachesOutcome|CompactionPersistsAcrossRestart|InterruptedTrajectoryResumesWithoutReplay)$' \
    -count=1 -v
  run_check harness_self_test go test ./test/e2e \
    -run '^TestOffline_(AgentLabPythonHarness|AgentLabScriptsParse|AgentLabLaneReferencesResolve|ProviderQualificationRejectsUndersizedReleaseSample)$' \
    -count=1 -v
	  run_check quality_harness_self_test go test ./test/e2e \
	    -run '^TestOffline_AgentLabQuality(ContractValidators|QualificationFailsClosed|LaneRequiresExplicitPaidGate|LaneRejectsUndersizedReleaseSample)$' \
	    -count=1 -v
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
    echo "provider_release requires KOE_PROVIDER_SAMPLE=release and at least 30 repetitions." >&2
    check_names+=("provider_agentloop_live")
    check_statuses+=("2")
    return
  fi
  run_check provider_agentloop_live \
    "$repo_dir/scripts/koe-provider-qualification.sh" \
    "$output_dir/provider"
}



run_quality_lane() {
  if [[ "${KOCORO_AGENT_LAB_QUALITY_LIVE:-}" != "1" ]]; then
    echo "Set KOCORO_AGENT_LAB_QUALITY_LIVE=1 to authorize the paid general-purpose quality lane." >&2
    check_names+=("general_purpose_quality_live")
    check_statuses+=("2")
    return
  fi
  local repetitions="${KOCORO_AGENT_LAB_QUALITY_REPETITIONS:-3}"
  local sample="${KOCORO_AGENT_LAB_QUALITY_SAMPLE:-smoke}"
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
  if [[ "$sample" == "release" && "$repetitions" -lt 15 ]]; then
    echo "release quality sample requires KOCORO_AGENT_LAB_QUALITY_REPETITIONS >= 15." >&2
    check_names+=("general_purpose_quality_live")
    check_statuses+=("2")
    return
  fi
  run_check general_purpose_quality_live env \
    SHANNON_E2E_LIVE=1 \
    KOCORO_AGENT_LAB_QUALITY_LIVE=1 \
    KOCORO_AGENT_LAB_QUALITY_SAMPLE="$sample" \
    KOCORO_AGENT_LAB_QUALITY_REPETITIONS="$repetitions" \
    KOCORO_AGENT_LAB_QUALITY_OUTPUT="$output_dir/general-purpose-quality.json" \
    go test ./test/e2e -run '^TestLive_AgentLabGeneralPurposeQuality$' \
    -count=1 -v -timeout=60m
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
