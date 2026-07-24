# Cache Debug — Operator Guide

Diagnostic instrumentation for the Anthropic prompt-cache prefix path. Pairs
with `docs/cache-strategy.md` (which covers the design); this file covers
**how to capture, read, and act on debug data when CER drops**.

## Quickstart

```bash
# Lightweight: hash ladders + per-tool/per-message + compact events
SHANNON_CACHE_DEBUG=1 shan -y 'your prompt'

# Heavy: also dump full request bytes per call (raw bytes diff-able)
SHANNON_CACHE_DEBUG=1 SHANNON_CACHE_DEBUG_RAW=1 shan -y 'your prompt'

# Inspect the log directly (see "Reading drift events" below)
jq -r 'select(.dir == "resp") | "cr=\(.cr) cc=\(.cc) cer=\(if .cc > 0 then .cr / .cc else 0 end)"' \
  ~/.shannon/logs/cache-debug.log | tail -20
```

Both flags are read at every LLM call — toggle without restart. **Daemon
must already be running with the env var exported** (set in the launch
shell, then `shan daemon restart`).

## Files written

| Path | When | Format | Cap |
|---|---|---|---|
| `~/.shannon/logs/cache-debug.log` | `SHANNON_CACHE_DEBUG=1` | JSON-lines | 10 MB self-rotating (keeps newest half) |
| `~/.shannon/logs/cache-debug-raw/<req_id>/{tools,messages,system}.json` | `SHANNON_CACHE_DEBUG_RAW=1` | Pretty JSON, dir-per-call, 0700/0600 | LRU 100 dirs, override `SHANNON_CACHE_DEBUG_RAW_MAX` |

## Log entry types

The log is a JSON-lines stream of three entry kinds, joined chronologically:

### `dir: "req"` — outgoing LLM call

| Field | What |
|---|---|
| `req_id` | 6-byte hex correlation key for the matching `resp` line |
| `session_id` | Session this call belongs to (rolling cache_control marker key) |
| `cache_source` | `oneshot_cli` / `tui` / `slack` / etc. — attribution label; Cloud currently uses the short TTL for all values |
| `force_ttl` | Present only when `SHANNON_FORCE_TTL` is set; values: `off` / `5m` / `1h` |
| `tag` | `complete` or `stream` — which client method invoked the call |
| `model` | `<specific>/<tier>` |
| `system_h` / `system_len` | sha256[:6] + bytes of system message content (anchor 1) |
| `tools_h` / `tools_count` | sha256[:6] of marshalled tools[] array + count (anchor 2) |
| `first_user_h` / `first_user_len` | First user message content (anchor 3 — typically the scaffolded turn 0 prompt) |
| `last_user_h` / `last_user_len` | Most recent user message (sanity for what model just received) |
| `tool_hashes` | Per-tool ladder: `[{name, hash, len, defer?}, ...]` in source order |
| `msg_hashes` | Per-message ladder: `[{i, role, hash, len, blocks?}, ...]` in turn order |
| `msg_hashes[i].blocks` | (only on multi-block turns) `[{type, hash, len, tier?}, ...]` per block |

`tier` field on a block is non-zero when an earlier compaction pass marked
it (`CompressedTier`): 1 = stripped to metadata, 2 = micro-compact / head+tail
truncated. Tier 0 (default) is omitted to keep logs lean.

### `dir: "resp"` — gateway response

| Field | What |
|---|---|
| `req_id` | Joins to the `req` line |
| `gateway_reqid` | Gateway's own request ID (for cross-service tracing) |
| `in` / `out` | Anthropic input / output tokens |
| `cc` / `cc_5m` / `cc_1h` | Cache creation, split by TTL bucket |
| `cr` | Cache read tokens — the win signal |

CER (cache efficiency ratio) = `cr / cc`. Healthy long sessions hit 15× +.

### `dir: "compact"` — in-place message rewrite

Emitted by the agent loop's compaction passes immediately before the next
LLM call. Tells you *why* a position in the next req's `msg_hashes` ladder
just changed.

| Field | What |
|---|---|
| `action` | `tier1` (strip-to-metadata, native blocks) / `tier1_xml` / `tier2` (head+tail or micro-compact summary) / `tbclear` (time-based result clear) / `img_strip` (image-content removal) / `empty_assistant_drop` (empty assistant removed from history) / `empty_text_block_strip` (empty text block stripped from assistant blocks) / `repair_user_merge` (two user messages content-merged after the assistant between them was dropped) |
| `msg_idx` | Position in the message array that was rewritten |
| `old_hash` / `new_hash` | sha256[:6] of the message content before / after — should match the previous `msg_hashes[msg_idx].hash` and the next req's same slot |
| `old_len` / `new_len` | Bytes before / after — the size delta of the rewrite |

No-op rewrites (bytes unchanged, e.g. revisiting an already-compacted block)
are skipped silently — only real wire-byte drift hits the log.

`empty_assistant_drop` is the one structural (non-rewrite) action: the
message at `msg_idx` is REMOVED, not edited. Its `new_hash` corresponds
to an internal sentinel block (`_kocoro_repair_drop_marker`) used only
to defeat the dedup that would otherwise swallow the row — that sentinel
never appears on the wire. Cache-debug tooling that re-hashes messages
from raw dumps will not find this hash; treat the row as "msg_idx was
dropped, downstream indices shifted by one". `repair_user_merge` rows
typically pair with this — they appear at the input index of the
trailing user, with `new_hash` matching the merged user at the previous
position.

## Reading drift events

The standard analysis pipeline:

1. **Find the `EARLIEST DIFF POSITION`** — for each pair of adjacent `req`
   lines, compare their `msg_hashes[k].hash` arrays; the first `k` whose hash
   changed is where the drift starts. Per-call CER is `cr / cc` from the
   matching `resp` line (joined by `req_id`).
2. **Cross-reference with `dir: "compact"` lines** — every drift slot should
   have a matching compact event with the same `msg_idx`. If one doesn't,
   you've found an unexpected mutation path that's not yet instrumented
   (file an issue).
3. **For deep dives, diff raw dumps** — under
   `~/.shannon/logs/cache-debug-raw/<req_id>/messages.json` between two
   adjacent `req_id`s. The byte-level diff localizes the issue inside a
   single tool_result block.

## Common drift patterns

| Pattern | Symptom in log | Root cause |
|---|---|---|
| `tools_h` flips after `tool_search` runs | `TOOLS!(N->N+M)` tag in analyze table, `cc` jumps, `cr=0` for that turn | Expected — first tool_search load is a one-time invalidation. Subsequent calls should restabilize |
| `msg_hashes[k]` flips at increasing `k` each turn | `tier1` / `tier2` compact events at rolling positions | Continuous-compaction cliff (the bug fixed by `time-based microcompact`) |
| `msg_hashes[k]` flips inside one block of a multi-block message | Per-block ladder shows ONE block changed | Single tool_result rewritten — typically Tier 1 strip on an old call |
| `system_h` flips when nothing structural changed | No `compact` event lines up with it | Ordering instability in tool_use input or system prompt — check `normalizeToolInput` |
| `cc=0 cr=0` simultaneously | Tool-use-only turn (no LLM bill) | Normal — model returned tool calls without further generation |

## Performance impact

| Mode | Per-call cost | Disk |
|---|---|---|
| Debug off (default) | 0 | 0 |
| `SHANNON_CACHE_DEBUG=1` | One sha256 over each marshalled tool + message; one append | ~2 KB/call → ~1 MB / 500 calls before rotation |
| `SHANNON_CACHE_DEBUG_RAW=1` | + 3 file writes per call | Hundreds of KB/call (full request) → bounded by `SHANNON_CACHE_DEBUG_RAW_MAX` |

Lightweight mode is cheap enough to leave on for an entire session under
investigation. Raw dump mode is for active reproduction work — disable
when not actively diffing.

## Privacy

`SHANNON_CACHE_DEBUG_RAW=1` writes the **full** request body (system prompt,
user messages, tool inputs, anything secrets injected into bash output)
to disk in plain JSON. Files are 0600 and the parent dir is 0700, but
**don't enable raw mode in shared environments or with sensitive data**.
The lightweight (hash-only) mode contains no plaintext content — only
hashes, lengths, role, type, and TTL/source metadata.

## Related code

- `internal/client/gateway.go` — `logCacheDebug`, `LogCacheCompactEvent`,
  `dumpRawForDebug`, `rotateRawDumpDir`
- `internal/agent/loop.go` — `compressOldToolResults`, `filterOldImages`
  (compact event call sites)
- `internal/agent/timebasedcompact.go` — `timeBasedCompact` (call site)
- `scripts/cache_bench.sh` — fixture-based regression bench
- `docs/cache-strategy.md` — authoritative cache design (4-breakpoint
  allocation, current Cloud TTL policy, source attribution, byte stability)
