#!/usr/bin/env bash
# Fail when an agent-facing guide points at a repo file that is not there.
#
# Stale pointers are the single most common rot in these guides: a doc gets
# renamed or was never committed, the reference stays, and the next reader
# follows it into nothing. This catches that in CI instead of months later.
#
# A missing target is allowed only when the citing line says so -- some design
# docs are deliberately untracked and live only on a maintainer's machine.
# Marking such a reference is what keeps it honest; an unmarked one is a bug.

set -uo pipefail

FILES=("CLAUDE.md" "AGENTS.md")
# Reference is excused when its line carries one of these.
EXCUSE='untracked|local-only|local only|not present|not in the repo|may be absent|absent on your machine|lives in the .* repo'

fail=0
checked=0

for f in "${FILES[@]}"; do
  [ -f "$f" ] || continue
  while IFS= read -r hit; do
    lineno=${hit%%:*}
    rest=${hit#*:}
    target=$(printf '%s' "$rest" | sed -E 's/^[ (`"]+//; s/[)`",.]+$//')
    [ -n "$target" ] || continue
    checked=$((checked + 1))
    [ -e "$target" ] && continue

    line=$(sed -n "${lineno}p" "$f")
    if printf '%s' "$line" | grep -qiE "$EXCUSE"; then
      printf '  note  %s:%s -> %s (declared unavailable)\n' "$f" "$lineno" "$target"
    else
      printf '  DEAD  %s:%s -> %s\n' "$f" "$lineno" "$target"
      fail=1
    fi
  done < <(grep -noE '(^|[ (`"])docs/[A-Za-z0-9._/-]+\.(md|json|yaml|yml)' "$f" 2>/dev/null)
done

printf 'checked %d docs/ reference(s)\n' "$checked"
if [ "$fail" -ne 0 ]; then
  printf '\nA reference above points at nothing. Either fix the path, or -- if the\n'
  printf 'target is deliberately not committed -- say so on the same line so a\n'
  printf 'reader knows not to go looking for it.\n'
  exit 1
fi
exit 0
