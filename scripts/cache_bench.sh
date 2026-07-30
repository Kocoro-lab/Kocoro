#!/usr/bin/env bash
# cache_bench.sh — synthetic cache-diagnostics example.
#
# The fixtures and warning threshold below are intentionally synthetic. They
# are not Kocoro release QA, product targets, or production baselines. Running
# this script still invokes the configured model and may consume paid quota.
#
# Usage:
#   scripts/cache_bench.sh [runs]          # default 2 runs per fixture
#   scripts/cache_bench.sh 1               # quick sanity (1 run per fixture)
#
# Requires SHANNON_CACHE_DEBUG=1 (auto-set). Assumes `shan` is on PATH and
# the daemon is running. Reads ~/.shannon/logs/cache-debug.log, aggregates
# entries added during this run's window, groups by session_id, and reports
# CHR, CER, and double-write rate.

set -euo pipefail

RUNS=${1:-2}
LOG="$HOME/.shannon/logs/cache-debug.log"

# Ensure the log exists so tail won't fail
mkdir -p "$(dirname "$LOG")"
touch "$LOG"

# Record the pre-run byte offset so we only aggregate new entries
BEFORE_SIZE=$(wc -c < "$LOG" 2>/dev/null | tr -d ' ')
: "${BEFORE_SIZE:=0}"

export SHANNON_CACHE_DEBUG=1

FIXTURES=(
  "synthetic-short: read the first heading in README.md and return it"
  "synthetic-list: list the top-level Markdown files and return their names"
)

# Multi-turn fixture exercises rolling cache_control: turns 2/3 must read the
# accumulated prefix from earlier turns. Sent through the daemon HTTP API so
# session_id is reused (one-shot CLI always forks a new session).
DAEMON_PORT=$(lsof -p "$(cat ~/.shannon/daemon.pid 2>/dev/null)" 2>/dev/null \
  | awk '/LISTEN/ && /localhost/ {split($9,a,":"); print a[2]; exit}')
: "${DAEMON_PORT:=7533}"
MULTI_TURN_PROMPTS=(
  "list the top-level files in this repo (use directory_list)"
  "now read README.md and tell me the project name in one line"
  "what's the main programming language? answer in one word"
)

echo "=== cache_bench: $RUNS runs per fixture, $(date -u +%Y-%m-%dT%H:%M:%SZ) ===" >&2

for i in $(seq 1 "$RUNS"); do
  for f in "${FIXTURES[@]}"; do
    label="${f:0:40}"
    echo "  run $i: $label..." >&2
    # -y auto-approves tools. Suppress shan's output — we only care about log writes.
    shan -y "$f" >/dev/null 2>&1 || true
  done

  # Multi-turn run: same session_id across 3 turns via daemon POST /message.
  # Auto-approval is implicit on the daemon HTTP API (localhost-trusted).
  sid="bench-mt-$(date +%s)-$i"
  echo "  run $i: multi-turn session=$sid..." >&2
  for prompt in "${MULTI_TURN_PROMPTS[@]}"; do
    curl -fsS -X POST "http://localhost:${DAEMON_PORT}/message" \
      -H "Content-Type: application/json" \
      -d "{\"text\":$(printf '%s' "$prompt" | python3 -c 'import json,sys;print(json.dumps(sys.stdin.read()))'),\"session_id\":\"$sid\",\"source\":\"cache_bench\"}" \
      >/dev/null 2>&1 || true
  done
done

echo >&2
echo "=== aggregating new log entries (offset > $BEFORE_SIZE bytes) ===" >&2

# tail -c +N takes a 1-based byte index. We want everything after BEFORE_SIZE,
# so start at BEFORE_SIZE + 1. Pipe new lines to python3's stdin for aggregation.
# Use python3 -c with a captured heredoc so the heredoc-as-stdin doesn't
# override the pipe (the bug that "python3 <<HEREDOC" on a pipeline causes).
AGG_SCRIPT=$(cat <<'PY'
import sys, json
from collections import defaultdict

# Group resp entries by session_id (fall back to req_id for entries without
# session_id — e.g. when SHANNON_CACHE_DEBUG was enabled pre-Task-5.0).
sessions = defaultdict(list)
for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    try:
        d = json.loads(line)
    except json.JSONDecodeError:
        continue
    if d.get("dir") != "resp":
        continue
    key = d.get("session_id") or d.get("req_id") or "unknown"
    sessions[key].append(d)

if not sessions:
    print("no resp entries captured — did the runs actually hit the gateway?")
    sys.exit(0)

totals = {"cc": 0, "cr": 0, "in": 0, "out": 0, "calls": 0}
first_call_cc = []        # cold-write size (turn-1 cc)
subsequent_cc = []        # rolling-write sizes (turn-2+ cc)
subsequent_cr = []        # rolling-read sizes (turn-2+ cr)
warm_starts = 0           # sessions whose first call was pure read (cc=0, cr>0)
multi_turn_sessions = []  # sessions with >=3 calls (proxy for our 3-turn fixture)

for sid, entries in sessions.items():
    entries.sort(key=lambda e: e.get("ts", ""))
    totals["calls"] += len(entries)
    if len(entries) >= 3:
        multi_turn_sessions.append((sid, entries))
    first = entries[0]
    if first.get("cc", 0) == 0 and first.get("cr", 0) > 0:
        warm_starts += 1
    first_call_cc.append(first.get("cc", 0))
    for e in entries[1:]:
        subsequent_cc.append(e.get("cc", 0))
        subsequent_cr.append(e.get("cr", 0))
    for e in entries:
        totals["cc"] += e.get("cc", 0)
        totals["cr"] += e.get("cr", 0)
        totals["in"] += e.get("in", 0)
        totals["out"] += e.get("out", 0)

denom = totals["cr"] + totals["cc"] + totals["in"]
chr_val = totals["cr"] / denom if denom else 0.0
cer = totals["cr"] / totals["cc"] if totals["cc"] else float("inf")
warm_rate = warm_starts / len(sessions) if sessions else 0.0
avg_first_cc = sum(first_call_cc) / len(first_call_cc) if first_call_cc else 0
avg_sub_cc = sum(subsequent_cc) / len(subsequent_cc) if subsequent_cc else 0
avg_sub_cr = sum(subsequent_cr) / len(subsequent_cr) if subsequent_cr else 0
# Rolling efficiency on subsequent calls: each rolling write should be a tiny
# delta while reads accumulate the full prefix. Amplification = cr/cc per
# subsequent call. The warning threshold below is an arbitrary illustration.
# Note: ratio-vs-first-call is unreliable because warm starts make first_cc=0,
# so we report absolute subsequent values + amplification instead.
amplification = avg_sub_cr / avg_sub_cc if avg_sub_cc else float("inf")

print(f"sessions={len(sessions)}  calls={totals['calls']}  "
      f"multi_turn_sessions={len(multi_turn_sessions)}")
print(f"CHR={chr_val:.3f}  CER={cer:.2f}  warm_start_rate={warm_rate:.2%}")
print(f"  read_tokens={totals['cr']}  creation_tokens={totals['cc']}"
      f"  input_tokens={totals['in']}  output_tokens={totals['out']}")
print(f"rolling: avg_first_cc={avg_first_cc:.0f}  avg_subsequent_cc={avg_sub_cc:.0f}  "
      f"avg_subsequent_cr={avg_sub_cr:.0f}  amplification={amplification:.1f}x")

# Per-turn breakdown for multi-turn sessions — the strongest rolling signal
if multi_turn_sessions:
    print(f"\nmulti-turn detail ({len(multi_turn_sessions)} sessions):")
    max_turns = max(len(e) for _, e in multi_turn_sessions)
    for turn_idx in range(max_turns):
        ccs = [e[turn_idx].get("cc", 0) for _, e in multi_turn_sessions if turn_idx < len(e)]
        crs = [e[turn_idx].get("cr", 0) for _, e in multi_turn_sessions if turn_idx < len(e)]
        if not ccs:
            continue
        print(f"  call#{turn_idx+1}: avg_cc={sum(ccs)/len(ccs):.0f}  avg_cr={sum(crs)/len(crs):.0f}  n={len(ccs)}")

# Synthetic diagnostic warnings (not release gates)
if all(len(e) == 1 for e in sessions.values()):
    print("WARN: every session had exactly 1 call — the synthetic fixture "
          "did not exercise session correlation")
if multi_turn_sessions and amplification < 2:
    print(f"WARN: read/write amplification is only {amplification:.1f}x on subsequent calls — "
          "inspect the public cache contract and local request bytes")
elif multi_turn_sessions:
    print(f"INFO: amplification={amplification:.1f}x — above the synthetic "
          "illustration threshold; this is not a release result")
PY
)
tail -c "+$((BEFORE_SIZE + 1))" "$LOG" | python3 -c "$AGG_SCRIPT"
