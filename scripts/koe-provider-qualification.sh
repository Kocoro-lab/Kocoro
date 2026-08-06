#!/usr/bin/env bash
set -uo pipefail

if [[ "${KOE_PROVIDER_AGENTLOOP_E2E:-}" != "1" ]]; then
  echo "Set KOE_PROVIDER_AGENTLOOP_E2E=1 to authorize paid provider qualification." >&2
  exit 2
fi

repo_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
output_dir="${1:-${TMPDIR:-/tmp}/koe-provider-qualification}"
mkdir -p "$output_dir"

sample="${KOE_PROVIDER_SAMPLE:-smoke}"
repetitions="${KOE_PROVIDER_REPETITIONS:-1}"
seed="${KOE_PROVIDER_SEED:-20260728}"
case "$sample" in
  smoke)
    smoke=1
    default_cost=5
    default_pause_ms=0
    ;;
  release)
    smoke=0
    default_cost=25
    # Smooth hundreds of paid calls without materially extending the run.
    # Override with KOE_PROVIDER_PAUSE_MS when a provider publishes a tighter limit.
    default_pause_ms=250
    if [[ ! "$repetitions" =~ ^[0-9]+$ || "$repetitions" -lt 30 ]]; then
      echo "Release provider qualification requires KOE_PROVIDER_REPETITIONS >= 30." >&2
      exit 2
    fi
    ;;
  *)
    echo "KOE_PROVIDER_SAMPLE must be smoke or release." >&2
    exit 2
    ;;
esac
pause_ms="${KOE_PROVIDER_PAUSE_MS:-$default_pause_ms}"
if [[ ! "$repetitions" =~ ^[0-9]+$ || "$repetitions" -lt 1 || "$repetitions" -gt 50 ]]; then
  echo "KOE_PROVIDER_REPETITIONS must be an integer from 1 through 50." >&2
  exit 2
fi

max_cost="${KOE_PROVIDER_MAX_COST_USD:-$default_cost}"
report_path="$output_dir/provider-qualification.json"
source_commit="$(git -C "$repo_dir" rev-parse HEAD)"
source_dirty=false
if [[ -n "$(git -C "$repo_dir" status --porcelain)" ]]; then
  source_dirty=true
fi
if [[ "$sample" == "release" && "$source_dirty" == "true" ]]; then
  echo "Release provider qualification requires a clean source tree." >&2
  exit 2
fi
started_at="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

runner_status=0
(
  cd "$repo_dir" || exit 2
  KOE_FAST_QUALIFICATION_LIVE=1 \
    KOE_FAST_QUALIFICATION_OUTPUT="$report_path" \
    KOE_FAST_QUALIFICATION_REPETITIONS="$repetitions" \
    KOE_FAST_QUALIFICATION_SMOKE="$smoke" \
    KOE_FAST_QUALIFICATION_SEED="$seed" \
    KOE_FAST_QUALIFICATION_MAX_COST_USD="$max_cost" \
    KOE_FAST_QUALIFICATION_PAUSE_MS="$pause_ms" \
    go test ./internal/tools \
      -run '^TestKoeFastQualificationLive_AgentLoop$' \
      -count=1 -v -timeout=0
) 2>&1 | tee "$output_dir/provider-qualification.log" || runner_status="${PIPESTATUS[0]}"

report_status=0
if [[ ! -f "$report_path" ]]; then
  report_status=2
elif ! jq -e --arg sample "$sample" '
  .complete == true and
  .gate_passed == true and
  .correctness_gate_passed == true and
  .contract_failure_count == 0 and
  .runtime_failure_count == 0 and
  .duplicate_side_effect_failure_count == 0 and
  .cost_failure_count == 0 and
  ($sample == "smoke" or
    (.sample_qualifying == true and .performance_gate_passed == true))
' "$report_path" >/dev/null; then
  report_status=1
fi

finished_at="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
RUNNER_STATUS="$runner_status" REPORT_STATUS="$report_status" \
SAMPLE="$sample" REPETITIONS="$repetitions" SEED="$seed" \
MAX_COST="$max_cost" STARTED_AT="$started_at" FINISHED_AT="$finished_at" \
SOURCE_COMMIT="$source_commit" SOURCE_DIRTY="$source_dirty" \
OUTPUT_DIR="$output_dir" \
python3 -c '
import json, os, pathlib
manifest = {
    "schema_version": "kocoro.provider_qualification_run.v1",
    "started_at": os.environ["STARTED_AT"],
    "finished_at": os.environ["FINISHED_AT"],
    "source": {
        "commit": os.environ["SOURCE_COMMIT"],
        "dirty": os.environ["SOURCE_DIRTY"] == "true",
    },
    "sample": os.environ["SAMPLE"],
    "repetitions_per_cell": int(os.environ["REPETITIONS"]),
    "seed": int(os.environ["SEED"]),
    "max_cost_usd": float(os.environ["MAX_COST"]),
    "exit_status": {
        "runner": int(os.environ["RUNNER_STATUS"]),
        "report_gate": int(os.environ["REPORT_STATUS"]),
    },
    "artifacts": [
        "provider-qualification.json",
        "provider-qualification.log",
    ],
}
pathlib.Path(os.environ["OUTPUT_DIR"], "run-manifest.json").write_text(
    json.dumps(manifest, ensure_ascii=False, indent=2) + "\n",
    encoding="utf-8",
)
'

echo "Provider qualification artifacts: $output_dir"
if [[ "$runner_status" -ne 0 || "$report_status" -ne 0 ]]; then
  exit 1
fi
