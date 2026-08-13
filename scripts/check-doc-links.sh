#!/usr/bin/env bash
# Fail when an agent-facing guide points at a repo path that is not there.
#
# Stale pointers are the most common rot in these guides: a doc gets renamed or
# was never committed, the reference stays, and the next reader follows it into
# nothing. This catches that in CI instead of months later.
#
# A missing target is allowed only when the citing text says so -- some design
# docs are deliberately untracked and live only on a maintainer's machine.
# Marking such a reference is what keeps it honest; an unmarked one is a bug.

set -uo pipefail

# Resolve to the repo root: every path below is repo-relative, and a run from a
# subdirectory would otherwise find no guides and pass green on zero work.
cd "$(git rev-parse --show-toplevel)" || exit 1

# Guides to scan, plus any tracked contract docs (they cite paths too).
FILES=("CLAUDE.md" "AGENTS.md")
while IFS= read -r extra; do
  [ -n "$extra" ] && FILES+=("$extra")
done < <(git ls-files 'docs/*contract*.md' 'contracts/*.md' 2>/dev/null)

# Directory prefixes worth checking. Kept to trees whose layout the guides
# actually assert; widening further mostly adds prose false positives.
PREFIX='(docs|contracts|infra|scripts|internal/skills/bundled)'

# A reference is excused when one of these appears NEAR it -- not merely
# somewhere on the same line. These files have multi-kilobyte lines, and a
# line-wide match would let one legitimately-untracked doc launder every other
# reference sharing its row.
EXCUSE='untracked|gitignored|local-only|local only|not present|absent from a fresh checkout|may be absent|absent on your machine'
EXCUSE_WINDOW=200

# Resolve against what a fresh checkout gets, NOT the local filesystem. A file
# that exists here but is untracked (or gitignored) is absent for everyone else
# and in CI -- checking `-e` would pass locally and fail in CI, which is worse
# than no gate at all.
exists_in_checkout() {
  case "$1" in
    */) git ls-files --error-unmatch -- "$1" >/dev/null 2>&1 \
          || [ -n "$(git ls-files -- "$1" | head -1)" ] ;;
     *) git ls-files --error-unmatch -- "$1" >/dev/null 2>&1 ;;
  esac
}

fail=0
checked=0
missing_guide=0

for f in "${FILES[@]}"; do
  if [ ! -f "$f" ]; then
    # Only the two named guides are mandatory; the globbed extras cannot be
    # missing by construction.
    case "$f" in
      CLAUDE.md|AGENTS.md)
        printf '  note  no %s in this repo\n' "$f" ;;
    esac
    continue
  fi
  missing_guide=1

  # Match files (extension required) and directories (trailing slash), so an
  # index rewritten from a file list into directory pointers stays covered.
  while IFS= read -r hit; do
    lineno=${hit%%:*}
    rest=${hit#*:}
    target=$(printf '%s' "$rest" | sed -E 's/^[ (`"]+//; s/[)`",.]+$//')
    [ -n "$target" ] || continue

    line=$(sed -n "${lineno}p" "$f")

    # Globs: report rather than skip, so `checked` never undercounts silently.
    case "$target" in
      *'*'*)
        # shellcheck disable=SC2086
        set -- $target
        checked=$((checked + 1))
        if [ -z "$(git ls-files -- $target | head -1)" ]; then
          printf '  DEAD  %s:%s -> %s (glob matches nothing tracked)\n' "$f" "$lineno" "$target"
          fail=1
        fi
        continue ;;
    esac

    checked=$((checked + 1))
    exists_in_checkout "$target" && continue

    # Excuse must sit within EXCUSE_WINDOW characters of the reference.
    pos=$(awk -v l="$line" -v t="$target" 'BEGIN{print index(l, t)}')
    from=$(( pos > EXCUSE_WINDOW ? pos - EXCUSE_WINDOW : 1 ))
    near=$(printf '%s' "$line" | cut -c "${from}-$((pos + ${#target} + EXCUSE_WINDOW))")

    if printf '%s' "$near" | grep -qiE "$EXCUSE"; then
      printf '  note  %s:%s -> %s (declared unavailable)\n' "$f" "$lineno" "$target"
    else
      printf '  DEAD  %s:%s -> %s\n' "$f" "$lineno" "$target"
      fail=1
    fi
  done < <(grep -noE "(^|[ (\`\"])${PREFIX}/[A-Za-z0-9._*/-]+(\.(md|json|ya?ml|sh|swift|go|py)|/)" "$f" 2>/dev/null)
done

if [ "$missing_guide" -eq 0 ]; then
  printf 'no guide found (expected CLAUDE.md or AGENTS.md at the repo root)\n'
  exit 1
fi

printf 'checked %d reference(s)\n' "$checked"

# A scanner that matches nothing is indistinguishable from a clean repo. Fail
# loudly instead: it means the pattern broke, not that the guides got tidy.
if [ "$checked" -eq 0 ]; then
  printf '\nMatched no references at all. The pattern is broken, not the guides.\n'
  exit 1
fi

if [ "$fail" -ne 0 ]; then
  printf '\nA reference above points at nothing. Either fix the path, or -- if the\n'
  printf 'target is deliberately not committed -- say so next to the reference so a\n'
  printf 'reader knows not to go looking for it.\n'
  exit 1
fi
exit 0
