#!/usr/bin/env bash
# Tests for scripts/check-doc-links.sh.
#
# Worth testing because this gate's failure mode is silence: a broken pattern,
# a renamed guide, or a wrong working directory all look exactly like a clean
# repo. The zero-match guard exists for that, and nothing else verifies it
# fires.
#
# Each case builds a throwaway git repo in a temp dir, because the script
# resolves references through `git ls-files` (what a fresh checkout gets)
# rather than the local filesystem.

set -uo pipefail

SCRIPT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/check-doc-links.sh"
[ -f "$SCRIPT" ] || { echo "missing $SCRIPT"; exit 1; }

pass=0
fail=0

# run <name> <expected-exit> <setup-fn>
run() {
  local name="$1" want="$2" setup="$3"
  local dir
  dir="$(mktemp -d)"
  (
    cd "$dir" || exit 1
    git init -q .
    git config user.email t@example.com
    git config user.name t
    mkdir -p scripts
    cp "$SCRIPT" scripts/check-doc-links.sh
    "$setup"
    git add -A >/dev/null 2>&1
    git commit -qm fixture >/dev/null 2>&1
    bash scripts/check-doc-links.sh >/dev/null 2>&1
  )
  local got=$?
  rm -rf "$dir"
  if [ "$got" -eq "$want" ]; then
    printf '  ok    %s\n' "$name"; pass=$((pass + 1))
  else
    printf '  FAIL  %s (want exit %s, got %s)\n' "$name" "$want" "$got"; fail=$((fail + 1))
  fi
}

clean() {
  mkdir -p docs
  echo "real" > docs/real.md
  printf '# G\n\n- see `docs/real.md`\n' > CLAUDE.md
}

dead_file() {
  clean
  printf -- '- and `docs/ghost.md`\n' >> CLAUDE.md
}

excused_file() {
  clean
  printf -- '- `docs/ghost.md` is untracked, local-only on the maintainer machine\n' >> CLAUDE.md
}

# The excuse must sit near the reference. A guide line can be kilobytes long,
# so one legitimately-absent doc must not launder a genuinely dead sibling.
excuse_too_far() {
  clean
  {
    printf -- '- `docs/ghost.md` then '
    printf 'x%.0s' $(seq 1 400)
    printf ' this one is untracked and local-only\n'
  } >> CLAUDE.md
}

# Present on disk but never committed: absent for everyone else and in CI.
untracked_target() {
  clean
  mkdir -p docs/local
  echo x > docs/local/note.md
  printf -- '- see `docs/local/note.md`\n' >> CLAUDE.md
  printf 'docs/local/\n' > .gitignore
}

dead_directory() {
  clean
  printf -- '- browse `docs/nowhere/`\n' >> CLAUDE.md
}

dead_glob() {
  clean
  printf -- '- see `docs/2026-*.md`\n' >> CLAUDE.md
}

# A pattern that matches nothing is indistinguishable from tidy guides.
broken_pattern() {
  clean
  sed -i.bak "s|^PREFIX=.*|PREFIX='(zzzz)'|" scripts/check-doc-links.sh
  rm -f scripts/check-doc-links.sh.bak
}

no_guide() {
  mkdir -p docs
  echo real > docs/real.md
}

echo "check-doc-links.sh"
run "clean repo passes"                     0 clean
run "dead file reference fails"             1 dead_file
run "excused file reference passes"         0 excused_file
run "excuse beyond the window still fails"  1 excuse_too_far
run "untracked target fails"                1 untracked_target
run "dead directory reference fails"        1 dead_directory
run "glob matching nothing fails"           1 dead_glob
run "broken pattern fails loudly"           1 broken_pattern
run "no guide at all fails"                 1 no_guide

printf '\n%d passed, %d failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
