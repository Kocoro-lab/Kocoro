#!/bin/bash
# Public synthetic skill-discovery example. This is not a release QA matrix.
set -euo pipefail

if [[ "${KOCORO_RUN_SYNTHETIC_BENCHMARK:-}" != "1" ]]; then
  echo "Synthetic benchmark is disabled."
  echo "Set KOCORO_RUN_SYNTHETIC_BENCHMARK=1 to acknowledge that it may use paid model quota."
  exit 2
fi

PROMPTS=(
  "Synthetic example: explain which bundled skill would help inspect a PDF. Do not open files or use network services."
  "Synthetic example: explain which bundled skill would help create a local MCP server. Do not modify files."
  "Synthetic example: explain which bundled skill would help build a local landing page. Do not modify files."
)

RESULTS="${BENCHMARK_RESULTS_DIR:-/tmp/kocoro-synthetic-skill-discovery}"
mkdir -p "$RESULTS"

for i in "${!PROMPTS[@]}"; do
  task=$((i + 1))
  echo "Synthetic task $task"
  shan -y "${PROMPTS[$i]}" >"$RESULTS/task${task}.stdout" 2>"$RESULTS/task${task}.stderr"
done

echo "Synthetic example complete. No production pass threshold is defined here."
