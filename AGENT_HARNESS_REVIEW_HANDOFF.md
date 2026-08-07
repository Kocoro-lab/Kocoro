# Kocoro Agent Harness Review Handoff

Date: 2026-08-07 (Asia/Tokyo)

## Review objective

Review whether `experiment/kocoro-agent-lab` is a sound strong-model-era harness for Kocoro as a general-purpose assistant. Kocoro is not a coding-only agent: expected use includes everyday work, research, automation, local macOS actions, portable voice invocation, and user-selected personas.

This handoff records evidence and open questions. It is not merge or release authorization.

## Checkout and artifact state

- Worktree: `/Users/hu/Desktop/projects/kocoro-projects/ShanClaw-agent-lab`
- Branch: `experiment/kocoro-agent-lab`
- Merge base with `origin/main`: `57421c63`
- Functional/report source HEAD: `574e9d3b`
- Diff at handoff start: 81 files, 32,336 insertions, 1,002 deletions
- Self-contained review: `docs/agent-harness-review.html`
- The HTML embeds the full `origin/main...HEAD` code/script diff except the HTML itself, which is excluded to prevent recursive embedding.
- Tracked worktree was clean before this handoff file. The built `shan` binary is ignored by `.gitignore`.

The branch contains earlier selector, loop, qualification, memory, and tool changes in addition to the final prompt/tool-context work. Review the complete merge-base diff; do not assume the latest commits are the entire scope.

## What changed

### Prompt architecture

- Reframed Kocoro as a general-purpose macOS assistant rather than a coding or everyday-work-only agent.
- Preserved user-selected persona and `<user_instructions>` authority while keeping system safety precedence.
- Reduced the shared prompt to model-owned decisions: objective, trust boundaries, care, tool strategy, progress/stopping, evidence, and communication.
- Removed duplicated prose for runtime-owned enforcement such as approval, loop detection, idempotency, budgets, dispatch, and persistence.
- Moved capability-specific guidance into conditional layers based on the final provider-visible tools.
- Exported exact Kocoro base, Koe Full, and Koe Fast system/stable/volatile prompts for review.

### Tool surface and latency

- Added Fast-only, run-local deferral for uncommon schema-heavy local tools: archive inspection/extraction, cloud delegation, document converters, raw HTTP, and system information.
- Kept common memory, session search, file, grep/glob/list, arithmetic/time, schedule/calendar, desktop utility, skill, and Bash openers direct.
- Compressed the Bash tool schema while leaving runtime validation, approval, and execution policy authoritative.
- Bounded a warmed deferred-tool working set to 16 schemas or an estimated 8,000 schema tokens.
- Full and ordinary CLI exposure are not changed by the Fast-only override.

### Runtime harness

- Made loop stopping outcome-aware rather than counting tool names/arguments alone.
- Added stable-outcome ping-pong detection, critical pre-dispatch stops, progress-safe repeated observation handling, and browser observation exceptions.
- Added 14-step long-trajectory, compaction, process-restart, interruption/resume, idempotency, and outcome-unknown gates through production AgentLoop seams.

### Memory

- Removed the implicit small-model preflight from production CLI/TUI/daemon paths.
- Kept `memory_recall` and `session_search` directly visible so the main model chooses whether and how to recall.
- Did not redesign the existing layered sidecar/bundle memory system.

### Evaluation and report

- Added randomized real-provider prompt comparison with correctness, cost, latency tails, matched-pair deltas, failure details, and comparison/release qualification gates.
- Added deterministic general-purpose quality contracts for writing, notes, planning, voice answers, bounded research, and automation.
- Added synthetic-memory A/B with production tool descriptions and no response cache.
- Added a fail-closed HTML generator bound to prompt, memory, quality, loop, offline runtime, and five-case Koe runtime artifacts.

## Measured results

| Surface | Result | Interpretation |
| --- | ---: | --- |
| Representative old system prompt | 26,534 chars | Historical comparison fixture |
| Koe Full system prompt | 7,730 chars | Exact current production-builder audit |
| Koe Fast system prompt | 7,091 chars | Exact current production-builder audit |
| Fast provider-visible tools | 29 / 62,765 chars -> 22 / 47,509 chars | Tool schema surface reduced 24.3% by characters |
| Isolated Fast daemon input | 33,874 -> 27,608 tokens | Measured 18.5% input reduction |
| Single Fast text total latency | 6,066 -> 5,444 ms | Descriptive single samples only; not statistical speed proof |
| Prompt comparison | 132/132 tool trajectories correct | One task-answer failure occurred in the intentionally minimal stress control |
| Prompt selection | `no_improvement` | No candidate met correctness, tail-latency, and token-reduction criteria together |
| General-purpose quality | 18/18 correct | Six deterministic contracts x three repetitions; comparison-only sample |
| Memory A/B | main model 24/24; preflight 24/24 | Synthetic routing fixture, not real sidecar recall quality |
| Memory A/B efficiency | main-model cost -80.1%; mean latency -21.7%; median -11.0% | Observed in the synthetic 3x sample only |
| Loop corpus | 10/10 classified correctly | Productive paths were not flagged; genuine loops were caught |
| Koe isolated runtime | 5/5 Fast/Full text/audio; five sessions persisted across restart | Audio input is synthesized WAV, not a physical microphone qualification |
| Full Go suite | `go test ./... -count=1` passed | Executed before the final report-only commits |
| HTML QA | desktop 1440x1000 and mobile 390x844 passed | 0 console/page errors; no document overflow; anchors/details work |

Both prompt and quality live samples use three repetitions per cell/case and explicitly report `release_qualifying=false`.

## Build delivered

- Binary: `/Users/hu/Desktop/projects/kocoro-projects/ShanClaw-agent-lab/shan`
- Build command:

  ```bash
  PKG_CONFIG_PATH=/opt/homebrew/lib/pkgconfig go build -trimpath \
    -ldflags '-X github.com/Kocoro-lab/ShanClaw/cmd.Version=agent-lab-574e9d3b' \
    -o shan .
  ```

- Format: Mach-O 64-bit arm64
- Size: 67,849,890 bytes
- Version output: `shan version agent-lab-574e9d3b`
- SHA-256: `57c91ee840007d76726dda9f8175662aa2e7572acbf3a24195dc59ee3c087782`
- Signature: linker-generated ad-hoc signature; no Team ID
- `./shan --help` and `./shan daemon --help` both exited successfully.
- The binary was not installed, did not replace the active daemon, and did not modify Desktop.
- This repository has no Dockerfile. Its documented build is native Go; no Docker build was claimed.

## Review risks and open questions

1. **Production prompt changed without a benchmark winner.** The final architecture is more compact and all deterministic gates passed, but the prompt comparison correctly selected `no_improvement`. Decide whether architectural simplification plus preserved observed correctness is sufficient, or whether deployment should wait for larger paired/human-quality evidence.
2. **Samples are too small for rare failures.** The 3x prompt and quality runs are comparison-ready, not release-ready. Do not turn `18/18` or `132/132` into a reliability percentage.
3. **Working-set limits are hard-coded.** Review the 16-schema/8,000-token caps against `AGENTS.md`, especially whether a documented/configurable override is required for power users and very large MCP catalogs.
4. **Fast cold-tool policy is an explicit list.** Check whether archive/document/HTTP/system-info coverage is correct for real non-coding work and whether future tools can drift into the wrong exposure without a registry-level policy test.
5. **Deferred execution coverage is incomplete.** The quality gate exercises discovery with a production-shaped fake Bash tool, while production Bash remains Direct. Review real provider `tool_search -> load -> execute` coverage for each actual Fast-cold tool family.
6. **Bash schema compression needs semantic review.** Runtime safety is unchanged, but verify that the shorter description still lets models choose Bash correctly for automation without encouraging shell use where dedicated tools fit.
7. **Memory quality is not established.** The A/B proves model-driven routing on deterministic fixtures. The local real sidecar was not enabled, so bundle creation, ranking, multilingual entity recall, stale-memory handling, and no-data behavior remain unqualified.
8. **Voice/product UI boundaries remain.** Synthesized audio covers the Realtime/daemon/provider/voice-result path, not physical mic, acoustic echo, VAD/barge-in behavior, signed Desktop UI, or user-perceived interruption quality.
9. **Some report baselines remain historical constants.** The final Fast input and latency are parsed from the supplied Koe runtime log, but historical before values and tool-array character counts are encoded in the generator. Verify their raw source logs before treating report regeneration as fully self-contained provenance.
10. **Report source artifacts live under `/tmp`.** The HTML embeds their contents and hashes, but regenerating it later requires rerunning the harnesses or preserving those artifacts elsewhere.
11. **The branch is broad.** It includes selector admission, loop behavior, memory routing, prompt policy, tool exposure, tests, and a large self-contained HTML. Review and possible integration should be split by behavioral risk, even if the experiment branch stays intact.

## Suggested review order

1. Read `docs/agent-harness-review.html` for the evidence map and exact Full/Fast prompts.
2. Review prompt ownership and conditionals in `internal/prompt/builder.go`.
3. Review Fast exposure and warmed-set behavior in `internal/agent/deferred.go` and `internal/agent/warmset.go`.
4. Review model usability and unchanged runtime authority in `internal/tools/bash.go`, `internal/tools/memory_preflight.go`, `internal/tools/memory.go`, and `internal/tools/session_search.go`.
5. Review loop semantics in `internal/agent/loopdetect.go` and `internal/agent/loop.go` before relying on acceptance tests.
6. Audit evaluators for false positives in `internal/tools/kocoro_prompt_variants_live_test.go`, `test/e2e/agent_lab_quality_live_test.go`, and `test/e2e/agent_lab_runtime_gate_test.go`.
7. Reconcile every claim with `origin/main...HEAD`, not only the report summary.

## Reproduction commands

```bash
cd /Users/hu/Desktop/projects/kocoro-projects/ShanClaw-agent-lab
git status --short
git diff --check
git diff --stat origin/main...HEAD
go test ./... -count=1
PKG_CONFIG_PATH=/opt/homebrew/lib/pkgconfig go build ./...
```

The paid live lanes and exact artifact-generation commands are documented in `docs/agent-harness-review.html` and the corresponding test/script entry points. Reuse the existing local provider credentials only for explicitly authorized E2E, never print them, keep response cache off for comparisons, and preserve daemon state/port isolation.
