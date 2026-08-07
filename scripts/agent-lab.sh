#!/usr/bin/env bash
set -uo pipefail
export PYTHONDONTWRITEBYTECODE=1

repo_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
lane="${AGENT_LAB_LANE:-offline}"
output_dir="${1:-${TMPDIR:-/tmp}/kocoro-agent-lab/$lane}"
pkg_config_path="${PKG_CONFIG_PATH:-/opt/homebrew/lib/pkgconfig}"
mkdir -p "$output_dir"

case "$lane" in
  offline|routing_live|selector_live|provider_live|prompt_live|provider_release) ;;
  *)
    echo "AGENT_LAB_LANE must be offline, routing_live, selector_live, provider_live, prompt_live, or provider_release" >&2
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
  run_check harness_self_test go test ./test/e2e \
    -run '^TestOffline_(AgentLabPythonHarness|AgentLabScriptsParse|PromptVariantRunnerRequiresExplicitPaidGate|ProviderQualificationRejectsUndersizedReleaseSample)$' \
    -count=1 -v
}

run_routing_lane() {
  if [[ "${KOE_MODE_CLASSIFIER_E2E:-}" != "1" ]]; then
    echo "Set KOE_MODE_CLASSIFIER_E2E=1 to authorize the paid routing lane." >&2
    check_names+=("routing_live")
    check_statuses+=("2")
    return
  fi
  run_check routing_live env PKG_CONFIG_PATH="$pkg_config_path" \
    "$repo_dir/scripts/koe-mode-ab.sh" "$output_dir/routing"
}

run_selector_lane() {
  if [[ "${KOE_SELECTOR_AGENTLOOP_E2E:-}" != "1" ]]; then
    echo "Set KOE_SELECTOR_AGENTLOOP_E2E=1 to authorize the paid selector-to-AgentLoop lane." >&2
    check_names+=("selector_agentloop_live")
    check_statuses+=("2")
    return
  fi
  local repeats="${KOE_SELECTOR_AGENTLOOP_REPEATS:-3}"
  if [[ ! "$repeats" =~ ^[0-9]+$ || "$repeats" -lt 3 ]]; then
    echo "KOE_SELECTOR_AGENTLOOP_REPEATS must be an integer >= 3 for release qualification." >&2
    check_names+=("selector_agentloop_live")
    check_statuses+=("2")
    return
  fi
  local daemon_url="${KOE_DAEMON_URL:-http://127.0.0.1:7533}"
  if ! curl -fsS --max-time 5 "$daemon_url/status" -o "$output_dir/daemon-status.json"; then
    echo "Koe selector preflight failed: daemon status is unavailable at $daemon_url/status" >&2
    check_names+=("selector_daemon_preflight")
    check_statuses+=("2")
    return
  fi
  run_check selector_agentloop_live env \
    KOE_SELECTOR_AGENTLOOP_E2E=1 \
    PKG_CONFIG_PATH="$pkg_config_path" \
    go test ./internal/koe -run '^TestKoeSelectorToAgentLoopTextE2E$' \
    -count="$repeats" -v -timeout=10m
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

run_prompt_lane() {
  if [[ "${KOCORO_PROMPT_VARIANTS_LIVE:-}" != "1" ]]; then
    echo "Set KOCORO_PROMPT_VARIANTS_LIVE=1 to authorize the paid prompt comparison." >&2
    check_names+=("prompt_variants_live")
    check_statuses+=("2")
    return
  fi
  run_check prompt_variants_live \
    "$repo_dir/scripts/kocoro-prompt-variants.sh" \
    "$output_dir/prompt-variants"
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
  routing_live)
    run_routing_lane
    ;;
  selector_live)
    run_selector_lane
    ;;
  provider_live)
    run_provider_lane smoke
    ;;
  prompt_live)
    run_prompt_lane
    ;;
  provider_release)
    if run_release_source_preflight; then
      run_offline_lane
      run_routing_lane
      run_selector_lane
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
