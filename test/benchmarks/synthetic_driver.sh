#!/bin/bash
# Public, synthetic, read-only benchmark example.
set -u

if [[ "${KOCORO_RUN_SYNTHETIC_BENCHMARK:-}" != "1" ]]; then
  echo "Synthetic benchmark is disabled."
  echo "Set KOCORO_RUN_SYNTHETIC_BENCHMARK=1 to acknowledge that it may use paid model quota."
  exit 2
fi

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
RESULTS="${BENCHMARK_RESULTS_DIR:-/tmp/kocoro-synthetic-benchmark}"
mkdir -p "$RESULTS"
cd "$REPO_ROOT"

run_task() {
  local num=$1
  local desc=$2
  local prompt=$3
  local stdout_file="$RESULTS/task${num}.stdout"

  echo "=== Synthetic task $num: $desc ===" | tee -a "$RESULTS/driver.log"
  shan -y "$prompt" >"$stdout_file" 2>&1
  local rc=$?
  echo "exit=$rc" | tee -a "$RESULTS/driver.log"
  return "$rc"
}

run_task 1 "locate a public entry point" \
  "Read this checkout and identify the Go file that defines the CLI entry point. Do not modify files or use network services."

run_task 2 "summarize package names" \
  "Read go.mod and list the top-level internal package directory names. Do not modify files or use network services."

echo "Synthetic example complete. No production acceptance threshold is defined here."
