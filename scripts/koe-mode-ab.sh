#!/usr/bin/env bash
set -uo pipefail

if [[ "${KOE_MODE_CLASSIFIER_E2E:-}" != "1" ]]; then
  echo "Set KOE_MODE_CLASSIFIER_E2E=1 to authorize the paid Realtime A/B run." >&2
  exit 2
fi

repo_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
output_dir="${1:-${TMPDIR:-/tmp}/koe-mode-ab}"
mkdir -p "$output_dir"

repeats="${KOE_MODE_CLASSIFIER_REPEATS:-3}"
seed="${KOE_MODE_CLASSIFIER_SEED:-20260728}"
case_timeout="${KOE_MODE_CLASSIFIER_TIMEOUT:-30s}"
candidate="${KOE_MODE_CLASSIFIER_CANDIDATE:-instructions_only_v1}"
pkg_config_path="${PKG_CONFIG_PATH:-/opt/homebrew/lib/pkgconfig}"
case "$candidate" in
  instructions_only_v1|schema_only_v1|mode_only_v1) ;;
  *)
    echo "KOE_MODE_CLASSIFIER_CANDIDATE must be instructions_only_v1, schema_only_v1, or mode_only_v1" >&2
    exit 2
    ;;
esac
run_id="koe-mode-ab-$(date -u +%Y%m%dT%H%M%SZ)-$seed"
started_at="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
source_commit="$(git -C "$repo_dir" rev-parse HEAD)"
source_dirty=false
if [[ -n "$(git -C "$repo_dir" status --porcelain)" ]]; then
  source_dirty=true
fi

paired_status=0
(
  cd "$repo_dir" || exit 2
  KOE_TASK_LEDGER="${KOE_TASK_LEDGER:-1}" \
    KOE_MODE_CLASSIFIER_REPEATS="$repeats" \
    KOE_MODE_CLASSIFIER_SEED="$seed" \
    KOE_MODE_CLASSIFIER_TIMEOUT="$case_timeout" \
    KOE_MODE_CLASSIFIER_CANDIDATE="$candidate" \
    KOE_MODE_CLASSIFIER_OUTPUT_DIR="$output_dir" \
    KOE_AGENT_LAB_RUN_ID="$run_id" \
    KOE_AGENT_LAB_SOURCE_COMMIT="$source_commit" \
    KOE_AGENT_LAB_SOURCE_DIRTY="$source_dirty" \
    PKG_CONFIG_PATH="$pkg_config_path" \
    go test ./internal/koe -run '^TestKoeModeClassifierPairedTextE2E$' -count=1 -v -timeout=60m
) 2>&1 | tee "$output_dir/paired.log" || paired_status="${PIPESTATUS[0]}"

comparison_status=0
python3 "$repo_dir/scripts/compare-koe-mode-reports.py" \
  "$output_dir/baseline.json" \
  "$output_dir/$candidate.json" \
  --output "$output_dir/comparison.json" || comparison_status=$?

finished_at="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
RUN_ID="$run_id" STARTED_AT="$started_at" FINISHED_AT="$finished_at" \
SOURCE_COMMIT="$source_commit" SOURCE_DIRTY="$source_dirty" \
REPEATS="$repeats" SEED="$seed" CASE_TIMEOUT="$case_timeout" \
CANDIDATE="$candidate" \
PAIRED_STATUS="$paired_status" COMPARISON_STATUS="$comparison_status" \
OUTPUT_DIR="$output_dir" \
python3 -c '
import json, os, pathlib
output = pathlib.Path(os.environ["OUTPUT_DIR"])
manifest = {
    "schema_version": "koe.agent_lab_run.v1",
    "run_id": os.environ["RUN_ID"],
    "started_at": os.environ["STARTED_AT"],
    "finished_at": os.environ["FINISHED_AT"],
    "source": {
        "commit": os.environ["SOURCE_COMMIT"],
        "dirty": os.environ["SOURCE_DIRTY"] == "true",
    },
    "protocol": {
        "design": "paired_randomized_balanced_interleaved",
        "repeats": int(os.environ["REPEATS"]),
        "seed": int(os.environ["SEED"]),
        "case_timeout": os.environ["CASE_TIMEOUT"],
        "candidate": os.environ["CANDIDATE"],
    },
    "exit_status": {
        "paired_runner": int(os.environ["PAIRED_STATUS"]),
        "comparison_gate": int(os.environ["COMPARISON_STATUS"]),
    },
    "artifacts": [
        "baseline.json",
        os.environ["CANDIDATE"] + ".json",
        "comparison.json",
        "paired.log",
    ],
}
(output / "run-manifest.json").write_text(
    json.dumps(manifest, ensure_ascii=False, indent=2) + "\n", encoding="utf-8"
)
'

echo "A/B artifacts: $output_dir"
if [[ "$paired_status" -ne 0 || "$comparison_status" -ne 0 ]]; then
  exit 1
fi
