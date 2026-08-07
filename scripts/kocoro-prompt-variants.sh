#!/usr/bin/env bash
set -uo pipefail

if [[ "${KOCORO_PROMPT_VARIANTS_LIVE:-}" != "1" ]]; then
  echo "Set KOCORO_PROMPT_VARIANTS_LIVE=1 to authorize the paid prompt comparison." >&2
  exit 2
fi

repo_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
output_dir="${1:-${TMPDIR:-/tmp}/kocoro-prompt-variants}"
mkdir -p "$output_dir"

repetitions="${KOCORO_PROMPT_VARIANTS_REPETITIONS:-3}"
seed="${KOCORO_PROMPT_VARIANTS_SEED:-20260807}"
max_cost="${KOCORO_PROMPT_VARIANTS_MAX_COST_USD:-5}"
pause_ms="${KOCORO_PROMPT_VARIANTS_PAUSE_MS:-0}"
report_path="$output_dir/prompt-variants.json"
source_commit="$(git -C "$repo_dir" rev-parse HEAD)"
source_dirty=false
if [[ -n "$(git -C "$repo_dir" status --porcelain)" ]]; then
  source_dirty=true
fi
started_at="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

runner_status=0
(
  cd "$repo_dir" || exit 2
  KOCORO_PROMPT_VARIANTS_LIVE=1 \
    KOCORO_PROMPT_VARIANTS_OUTPUT="$report_path" \
    KOCORO_PROMPT_VARIANTS_REPETITIONS="$repetitions" \
    KOCORO_PROMPT_VARIANTS_SEED="$seed" \
    KOCORO_PROMPT_VARIANTS_MAX_COST_USD="$max_cost" \
    KOCORO_PROMPT_VARIANTS_PAUSE_MS="$pause_ms" \
    go test ./internal/tools \
      -run '^TestKocoroPromptVariantsLive_AgentLoop$' \
      -count=1 -v -timeout=0
) 2>&1 | tee "$output_dir/prompt-variants.log" || runner_status="${PIPESTATUS[0]}"

report_status=0
if [[ ! -f "$report_path" ]]; then
  report_status=2
elif ! jq -e '
  .complete == true and
  .cost_observed == true and
  .completed == .scheduled and
  (.summary | length) == 4 and
  (.comparison_summary | length) == 4 and
  (.product_gate_passed | type) == "boolean" and
  (.comparison_gate_passed | type) == "boolean" and
  ([.variants[] | select(.product_candidate == true)] | length) == 2 and
  ([.variants[].role] | sort) == ["control", "product_candidate", "product_candidate", "stress_control"]
' "$report_path" >/dev/null; then
  report_status=1
fi

finished_at="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
RUNNER_STATUS="$runner_status" REPORT_STATUS="$report_status" \
REPETITIONS="$repetitions" SEED="$seed" MAX_COST="$max_cost" \
STARTED_AT="$started_at" FINISHED_AT="$finished_at" \
SOURCE_COMMIT="$source_commit" SOURCE_DIRTY="$source_dirty" \
OUTPUT_DIR="$output_dir" \
python3 -c '
import json, os, pathlib
manifest = {
    "schema_version": "kocoro.prompt_variants_run.v1",
    "started_at": os.environ["STARTED_AT"],
    "finished_at": os.environ["FINISHED_AT"],
    "source": {
        "commit": os.environ["SOURCE_COMMIT"],
        "dirty": os.environ["SOURCE_DIRTY"] == "true",
    },
    "repetitions_per_cell": int(os.environ["REPETITIONS"]),
    "seed": int(os.environ["SEED"]),
    "max_cost_usd": float(os.environ["MAX_COST"]),
    "exit_status": {
        "runner": int(os.environ["RUNNER_STATUS"]),
        "report_gate": int(os.environ["REPORT_STATUS"]),
    },
    "artifacts": ["prompt-variants.json", "prompt-variants.log"],
}
pathlib.Path(os.environ["OUTPUT_DIR"], "run-manifest.json").write_text(
    json.dumps(manifest, ensure_ascii=False, indent=2) + "\n",
    encoding="utf-8",
)
'

echo "Prompt comparison artifacts: $output_dir"
if [[ "$runner_status" -ne 0 || "$report_status" -ne 0 ]]; then
  exit 1
fi
