# Prompt Cache Strategy

How Kocoro + Shannon allocate Anthropic's 4 `cache_control` breakpoints and route TTL by call-site origin. Canonical reference for anyone touching the LLM request path.

## Design goals

1. **Maximize cache_read / cache_creation (CER)** on long-running conversations — Slack/LINE/TUI where the user comes back over minutes or hours.
2. **Minimize paid cache_creation** on one-shot invocations — cron, webhook, MCP, CLI `shan -y ...`, and every internal subagent (`decompose`, `tool_select`, `lead_decide`, `interpretation`, `stub_cleanup`, `verify`, …) where the cache will never be re-read.
3. **Stay inside the public Anthropic API** — no `cache_edits` protocol, no `DANGEROUS_uncachedSystemPromptSection` marker, no CC-private `<system-reminder>` cache-key-invisible semantics. Kocoro *does* emit `<system-reminder>` tags as plain XML text (Claude is trained to read the tag as framework-internal trusted content — used for skill listings, post-tool nudges, and the instructions block in `prompt/builder.go`), but the wrapped content participates in the cache key like any other byte. The ~5-10 pp CHR gap versus Claude Code is accepted as a structural ceiling.

## Breakpoint allocation (Anthropic cap = 4)

| # | Position | Contents | TTL source |
|---|---|---|---|
| 1 | `system_stable` | gateway-cached persona + tools list + skills (excl. `<!-- volatile -->` tail) | shared request ttl |
| 2 | `tools[-1]` | last tool definition — caches all tool schemas | shared request ttl |
| 3 | `user_1.cache_break` | per-session stable instructions + sticky context (before `<!-- cache_break -->`) | shared request ttl |
| 4 | `rolling marker on claude_messages[-2]` | per-turn rolling cache point; preserved across turns when prev marker is still reachable | shared request ttl |

**BP #1 byte invariant (issue #107):** `system_stable` MUST be byte-identical
for any two users running the same agent on the same OS. Per-user values
(MCP tool names, deferred-tool listings, anything that varies with user
configuration) are routed to BP #3 (`StableContext` via `BuildToolListing`)
or to the volatile segment. Test guard: see
`TestBuildSystemPrompt_BP1ByteStableAcrossMCPConfigs` in
`internal/prompt/builder_test.go`. Production telemetry: `system_stable_hash`
field on `cache_summary` audit entries (`internal/audit/audit.go`).

All 4 breakpoints on a single request use the **same** `cache_control` dict. This trivially satisfies Anthropic's "monotonic non-increasing TTL across breakpoints" rule — you can't be out-of-order with a single TTL.

Volatile content (date, CWD, agent memory, MCP context) lives in the `user_1` block **after** the `<!-- cache_break -->` marker, or in a `<!-- volatile -->` tail inside the system prompt. Both positions leave breakpoints 1-4 byte-stable.

## Source → TTL routing

TTL is resolved by `_ttl_block(request)` in `anthropic_provider.py`. Precedence:

1. `SHANNON_FORCE_TTL` env (operator override — see below)
2. `request.cache_source` → lookup in `_LONG_CACHE_SOURCES` → 1h
3. Fallback → 5m (**fail cheap**: unknown sources pay the lower 1.25x write premium, not 2.0x)

| Source | TTL | Rationale |
|---|---|---|
| `slack` / `line` / `feishu` / `lark` / `telegram` | **1h** | Human-conversation channels; idle gaps > 5 min are common; re-read likely |
| `tui` | **1h** | Iterative developer use; re-read within session |
| `oneshot_interactive` | **1h** | Local `shan` TUI / interactive CLI without explicit source; similar reuse pattern |
| `cache_bench` | **1h** | Synthetic bench traffic reflects a channel-message configuration |
| `webhook` | 5m | One-shot trigger, rarely reused |
| `cron` / `schedule` | 5m | Fire-and-forget scheduled task |
| `mcp` | 5m | External MCP tool call with independent prompts |
| `oneshot_cli` | 5m | `shan -y '...'` — single invocation, no session continuity |
| `swarm_subagent` / `decompose` / `tool_select` / `lead_decide` / `interpretation` / `stub_cleanup` | 5m | Internal prompts that vary every call — no resume |
| `memory_extract` / `verify_*` / `complexity_*` / `evaluate` / `context_summary` / `research_plan` / `web_fetch_*` | 5m | Short internal helper calls |
| `agent_loop` / `agent_execute` / `agent_execute_stream` | 5m | Shannon's internal agent loops; multi-turn continuity not reliable |
| `completions_proxy` / `completions_proxy_stream` | 5m | Raw passthrough endpoints |
| Unknown / unset | 5m | Fail cheap |

When adding a new call-site, either:

- Pick a long bucket name → add to `_LONG_CACHE_SOURCES` in `anthropic_provider.py` **and** `cacheSourceFromDaemonSource` in `internal/daemon/runner.go` if the source originates on the Kocoro side
- Pick a short bucket name → just set `cache_source=<name>` on the call; no list update needed

## Env-var escape hatches

Operator-facing. Set on the `llm-service` container; applies to every request that container processes.

| Value | Effect | When to use |
|---|---|---|
| `SHANNON_FORCE_TTL=off` | Suppress `cache_control` entirely (no prompt-cache writes, no reads) | Bench baselines; isolating cache effects |
| `SHANNON_FORCE_TTL=5m` | Force every request to 5m | A/B vs 1h in production |
| `SHANNON_FORCE_TTL=1h` | Force every request to 1h | Legacy-compat / reproducing pre-Phase-1 behavior |
| *(unset)* | Use source-routed policy above | Normal operation |
| `SHANNON_CACHE_DEBUG=1` | Kocoro writes per-request hashes to `~/.shannon/logs/cache-debug.log` | Measurement / bench |
| `SHANNON_CACHE_DRIFT_DEBUG=1` | Extra `DRIFT[...]` log lines from `_apply_rolling_cache_marker` | Diagnosing byte drift |

## Byte stability

Cross-turn prompt-cache hits require that the same logical message produce the same bytes on the wire every turn. Drift sources and where they're neutralized:

| Drift source | Where neutralized |
|---|---|
| Go `map[string]any` iteration order in tool_use inputs | `normalizeToolInput()` in `gateway.go` (roundtrips through `json.Unmarshal`→`Marshal` for canonical key ordering) |
| Double-encoded JSON strings (OpenAI-shaped adapters) | `normalizeToolInput()` unwraps before canonicalization |
| Null / empty / whitespace tool_use input | `normalizeToolInput()` → `"{}"` |
| Mutated `cache_control` markers in hash input | `_msg_stable_hash` ignores `cache_control` via `_strip_cache_control_for_hash` |
| Pydantic optional-field presence flicker | `_msg_stable_hash` is semantic (role + content signature), not full-JSON |

Regression tests: `internal/client/gateway_test.go::TestNormalizeToolInput_CanonicalizesKeyOrdering`, `python/llm-service/tests/test_byte_stability.py`.

## Rolling marker (`_convert_messages_to_claude_format` only)

A single rolling cache_control marker is placed on `claude_messages[-2]` by `_convert_messages_to_claude_format`. That's the entire story — there is NO cross-turn prev-marker preservation (`_apply_rolling_cache_marker` is defined but not called).

Why not preserve prev marker: a direct bench (2026-04-15) showed it regresses 30-turn CHR from 93.6% → 61% and CER from 18.1x → 4.0x. Root cause: the preservation path calls `_strip_message_cache_control` on `user_1` to free a breakpoint slot for the prev marker, but stripping mutates the block's wire-bytes. Even though non-cache_control content is byte-identical, Anthropic's prefix matcher treats the resulting block as different, so the "free cached prefix up to prev_marker" fails to match, and the whole history falls through to uncached input. The single-rolling-marker layout is optimal under the public API's 4-breakpoint cap.

**Evidence**: bench session `2026-04-15-longbench-1776236xxx` — 30 user turns, 40 model calls (1.3 calls/turn), msgs 2→80, CHR 93.6%, CER 18.07x. Parallel-workload bench (3 sub-tasks per prompt, 15 turns) — 21 reqs, CHR 93.8%, CER 20.14x.

If Anthropic ever treats `cache_control` fields as cache-key-insensitive (i.e. strippable without breaking prefix match), the preservation path becomes viable again. Until then, keep it disabled.

## Session-ID propagation

`CompletionRequest.SessionID` must reach Shannon for prev-marker preservation to work. The chain:

- Kocoro `internal/agent/loop.go` sets `req.SessionID = a.sessionID` on every `Complete` call
- Kocoro `internal/client/gateway.go` marshals `session_id` on the wire (json tag, not `-`)
- Shannon `llm_provider/anthropic_provider.py::_apply_rolling_cache_marker(claude_messages, request.session_id, ttl_block)` keys the prev-marker memo on it

Without this chain, the memo is keyed on `None` and cross-turn continuity collapses to single-rolling-marker-per-request.

## Bench & KPI

Bench command:

```bash
SHANNON_CACHE_DEBUG=1 bash scripts/cache_bench.sh 3
# 30-turn single-session
bash /tmp/bench_longturn.sh 30
# parallel-friendly workload
bash /tmp/bench_parallel.sh 15
```

KPI targets (per-run bench artifacts under `docs/cache-bench-results/` are local-only — not tracked in this repo):

| # | KPI | Target | Measured (final) |
|---|---|---|---|
| K1 | 30-turn CHR | ≥ 80% | **93.6%** ✅ |
| K2 | 30-turn CER | ≥ 4x | **18.07x** ✅ |
| K3 | Short-session CHR (< 10 req) | ≥ 90% | 95%+ ✅ |
| K4 | avg model calls / user turn (parallel-friendly workload) | ≤ 10 | **1.40** ✅ |
| K7 | BYTE_DRIFT sessions | 0 | 0 ✅ |
| K8 | `cache_source = "unknown"` requests | 0 | 0 (all call-sites labeled) ✅ |

### Cross-user KPI (added 2026-04-30, issue #107)

After BP #1 byte stabilization, multi-tenant deployments under one Anthropic
API key should observe cross-user prefix cache hits on default-agent traffic.
Measurement comes from the `system_stable_hash` field in `audit.log` —
distinct hashes per user_id within a TTL window indicate residual drift.

| # | KPI | Target | Measurement |
|---|---|---|---|
| K9 | distinct `system_stable_hash` count among same-OS users on default agent within 1h | 1 | `jq -r '.system_stable_hash' audit.log \| sort -u \| wc -l` |
| K10 | cross-user cache_read tokens / total input tokens (default agent, 1h window) | ≥ 30% | gateway DB or `cache-debug.log` |

If K9 > 1, the BP #1 still has a per-user drift source — re-audit
`buildStaticSystem` and any code that contributes to `parts.System` (notably
`cloudDelegationGuidance` concatenation in `loop.go`).

## What we don't do (vs Claude Code)

These CC mechanisms use non-public Anthropic APIs and are not available to us:

- CC-private `<system-reminder>` cache-key-invisibility — wrapped content participates in the prompt but **not in the cache key**, letting CC re-bind per-user nudges without invalidating the prefix cache
- `cache_edits` protocol — partial-invalidation of a cached prefix
- `DANGEROUS_uncachedSystemPromptSection` — explicit "do not cache this segment" marker

Together these account for a structural 5-10 pp CHR gap that public-API clients cannot close.

### `<system-reminder>` — same tag, different semantics

Kocoro *does* emit `<system-reminder>` tags as plain XML text — see `internal/agent/loop.go:buildSkillListing`, `buildStickySkillReminder`, post-tool nudges in `systemReminder`, and the instructions block in `internal/prompt/builder.go` (issue #125). Anthropic's training causes Claude to treat content inside this tag as **framework-internal trusted content**, which suppresses the prompt-injection false-positive that bare `## Instructions` markdown headers triggered in user-role messages. What we **do not** get from this is CC's cache-key-invisibility — the tag is just text in the message body, so wrapped content is part of the byte stream the cache hashes.

**Workaround for `<system-reminder>` cache-invisibility (issue #107):** dynamic per-user tool
catalogs that CC routes through `<system-reminder>` (cache-key-invisible)
are routed in Kocoro through the user message's `StableContext` — they
land in BP #3 (per-session) rather than BP #1 (cross-user). Cross-user
share is preserved on BP #1 at the cost of no cross-user share on the
listing itself, which is the strictly correct trade since the listing
content is per-user by construction. See `prompt.BuildToolListing`.

## Maintenance playbook

**Adding a new call-site that hits the LLM:**
1. Pick a `cache_source` name that matches the call's lifecycle (long-reuse or one-shot)
2. Pass `cache_source=<name>` into `providers.generate_completion` (or `manager.complete`)
3. If long-reuse: add the name to `_LONG_CACHE_SOURCES` in `anthropic_provider.py`
4. If originating on Kocoro side: add a case in `cacheSourceFromDaemonSource` in `runner.go`
5. After traffic hits production, confirm no new `"unknown"` `cache_source` entries in `~/.shannon/logs/audit.log` (`jq -r '.cache_source' audit.log | sort -u`)

**Diagnosing a CHR regression:**
1. Enable `SHANNON_CACHE_DEBUG=1`, reproduce, and inspect `~/.shannon/logs/cache-debug.log` for `BYTE_DRIFT` (same `system_len`, different `system_h` across calls) — see `docs/cache-debug.md` for log schema
2. If drift: enable `SHANNON_CACHE_DRIFT_DEBUG=1` and compare `payload_h` of drifting requests → find the non-deterministic marshaler
3. If no drift: check `msgs` growth per turn; if > 20 and prompt doesn't encourage parallel tool use, Phase 2 nudge may be stripped by an agent override
4. If neither: check whether `cache_source` was accidentally set to a short-bucket where a long bucket was intended — `jq -r '.cache_source' ~/.shannon/logs/audit.log | sort | uniq -c` gives a per-source breakdown

**Bumping Anthropic SDK:**
SDK changes can silently alter message serialization. After any `anthropic` version bump, run `pytest tests/test_byte_stability.py` and a 30-turn bench to verify CHR hasn't regressed.

## Query-Time Tool Result Budget

Kocoro applies a second tool-result budget immediately before main LLM
requests. This layer is separate from execution-time spill:

- execution-time spill protects the current tool batch before it enters history;
- query-time budget protects the full history that is about to be sent to the model;
- replacements are keyed by `tool_use_id` and persisted in session JSON;
- replacement text is replayed byte-for-byte on later turns and after resume;
- non-text/image/browser/cloud deliverable results are skipped.

The default aggregate cap is 200K chars per user tool-result message. Fresh
replacements use a 2K-char preview and deterministic spill file path under
`~/.shannon/tmp/`.
