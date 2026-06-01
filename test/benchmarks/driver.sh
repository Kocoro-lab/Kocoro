#!/bin/bash
# Run 8 test tasks sequentially, capturing session_id + stdout.
# Modifying tasks (2, 5) run in throwaway git worktrees.
set -u
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
cd "$REPO_ROOT"
RESULTS="${BENCHMARK_RESULTS_DIR:-/tmp/maxiter_tests/results}"
mkdir -p "$RESULTS"

# session_set writes the basenames (without .json) of every session file
# that currently exists to stdout, one per line, sorted. Used to diff
# before/after so we can attribute the new session(s) to the current task
# even when another `shan` process races us, or when the task exits
# before persisting (in which case the diff is empty).
session_set() {
  ls ~/.shannon/sessions/*.json 2>/dev/null | xargs -I{} basename {} .json | sort
}

run_task() {
  local num=$1; local desc=$2; local prompt=$3; local workdir=${4:-}
  echo "=== Task $num: $desc ===" | tee -a "$RESULTS/driver.log"
  date +"%Y-%m-%d %H:%M:%S" | tee -a "$RESULTS/driver.log"

  local stdout_file="$RESULTS/task${num}.stdout"
  local before_file="$RESULTS/task${num}.sessions.before"
  local after_file="$RESULTS/task${num}.sessions.after"
  session_set > "$before_file"

  if [ -n "$workdir" ]; then
    # modifying task: run in worktree
    ( cd "$workdir" && perl -e 'alarm shift; exec @ARGV' 300 shan -y "$prompt" >"$stdout_file" 2>&1 )
  else
    perl -e 'alarm shift; exec @ARGV' 300 shan -y "$prompt" >"$stdout_file" 2>&1
  fi
  local rc=$?

  # New sessions attributable to this run = set-after minus set-before.
  # Take the most recent by mtime among the new ones so concurrent `shan`
  # processes on the same machine can't silently steal our session id.
  session_set > "$after_file"
  local new_session
  new_session=$(comm -13 "$before_file" "$after_file" | while read -r sid; do
    printf '%s\t%s\n' "$(stat -f '%m' "$HOME/.shannon/sessions/$sid.json" 2>/dev/null || echo 0)" "$sid"
  done | sort -n | tail -1 | awk '{print $2}')
  if [ -z "$new_session" ]; then
    new_session="NO_NEW_SESSION"
  fi
  echo "session_id=$new_session (exit=$rc)" | tee -a "$RESULTS/driver.log"
  echo "$new_session" > "$RESULTS/task${num}.session_id"
  tail -5 "$stdout_file" | sed 's/^/  /' | tee -a "$RESULTS/driver.log"
  echo "" | tee -a "$RESULTS/driver.log"
  sleep 2
}

# Tasks
run_task 1 "grep-trace runForceStopTurn" \
  "找到 runForceStopTurn 函数在 internal/agent/loop.go 里被哪些代码路径触发（具体调用位置），每条路径调用时传的 fallback 文案是什么，总结为一个 markdown 表格"

# Task 2: modifying — use worktree
WT2=/tmp/maxiter_tests/wt_task2
git worktree remove --force "$WT2" 2>/dev/null
git worktree add "$WT2" HEAD >/dev/null 2>&1
run_task 2 "batch edit context.Background in memory/" \
  "把 internal/memory/ 目录下所有 .go 文件里的 context.Background() 调用改成 context.TODO()，改完跑 go vet ./internal/memory/... 验证通过" \
  "$WT2"

run_task 3 "audit log failure analysis" \
  "分析 ~/.shannon/logs/audit.log 最近 500 条记录，给出失败率最高的 3 个工具（output_summary 包含 error 或 failed 视为失败），每个工具给出 1 条代表性失败记录的 input 和 output 摘要"

run_task 4 "analyze context compaction" \
  "分析 ShanClaw 当前 repo 的 context 压缩策略（包括压缩触发点、压缩方式、内存持久化），写到 /tmp/compare.md"

# Task 5: modifying — use worktree
WT5=/tmp/maxiter_tests/wt_task5
git worktree remove --force "$WT5" 2>/dev/null
git worktree add "$WT5" HEAD >/dev/null 2>&1
run_task 5 "add Info.Name tests" \
  "给 internal/tools/ 下每个工具文件（非 _test.go）加一个对应的 _test.go 测试文件，每个测试验证 Info().Name 非空。如果已存在测试文件就追加一个 TestInfoNameNonEmpty 测试。最后跑 go test ./internal/tools/ 验证全部通过" \
  "$WT5"

run_task 6 "memory_recall fallback trace" \
  "在 internal/tools/memory.go 和 internal/memory/ 里追踪：memory_recall 工具的降级路径是什么？从代码证明，给出 3 个失败场景（sidecar 未启动、sidecar 报错、query 超时）下的实际工具行为"

run_task 7 "decision trace: list /etc" \
  "我要你列出 /etc 目录下的文件。完成后告诉我你选了哪个工具、为什么这么选（不用其他工具的原因）"

run_task 8 "TODO/FIXME priority" \
  "在整个 ShanClaw repo 里找 TODO/FIXME/XXX 标记，按优先级（安全/正确性 > 性能 > 可读性）分类，给每条一句修复建议。用表格呈现前 15 条"

echo "=== ALL DONE ==="
date +"%Y-%m-%d %H:%M:%S" | tee -a "$RESULTS/driver.log"

# Cleanup worktrees
git worktree remove --force "$WT2" 2>/dev/null
git worktree remove --force "$WT5" 2>/dev/null
echo "worktrees removed"
