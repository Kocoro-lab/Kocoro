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
pkg_config_path="${PKG_CONFIG_PATH:-/opt/homebrew/lib/pkgconfig}"

run_variant() {
  local variant="$1"
  local report="$output_dir/$variant.json"
  local log="$output_dir/$variant.log"
  (
    cd "$repo_dir" || exit 2
    KOE_TASK_LEDGER="${KOE_TASK_LEDGER:-1}" \
      KOE_MODE_CLASSIFIER_VARIANT="$variant" \
      KOE_MODE_CLASSIFIER_REPEATS="$repeats" \
      KOE_MODE_CLASSIFIER_SEED="$seed" \
      KOE_MODE_CLASSIFIER_TIMEOUT="$case_timeout" \
      KOE_MODE_CLASSIFIER_REPORT="$report" \
      PKG_CONFIG_PATH="$pkg_config_path" \
      go test ./internal/koe -run '^TestKoeModeClassifierTextE2E$' -count=1 -v -timeout=60m
  ) 2>&1 | tee "$log"
  return "${PIPESTATUS[0]}"
}

baseline_status=0
candidate_status=0
run_variant baseline || baseline_status=$?
run_variant mode_only_v1 || candidate_status=$?

comparison_status=0
python3 "$repo_dir/scripts/compare-koe-mode-reports.py" \
  "$output_dir/baseline.json" \
  "$output_dir/mode_only_v1.json" \
  --output "$output_dir/comparison.json" || comparison_status=$?

echo "A/B artifacts: $output_dir"
if [[ "$baseline_status" -ne 0 || "$candidate_status" -ne 0 || "$comparison_status" -ne 0 ]]; then
  exit 1
fi
