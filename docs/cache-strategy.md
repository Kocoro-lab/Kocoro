# Prompt Cache Contract

This document records only the public Kocoro-side prompt-cache contract.
Production measurements, release thresholds, Cloud deployment policy, and the
private QA matrix are intentionally not tracked in this repository.

## Public boundary

- Kocoro uses only public provider cache controls.
- Cloud owns cache TTL selection. `cache_source` is attribution metadata, not
  a Kocoro-side TTL selector.
- Kocoro must not branch agent behavior on a presumed Cloud TTL.
- Cloud implementation paths, deployment variables, and internal test names
  are not part of the public contract.

## Request stability

Cross-turn cache reuse requires logically stable prompt bytes:

- `normalizeToolInput()` canonicalizes tool-use JSON before it reaches the
  gateway.
- `system_stable` contains only values shared by equivalent runs.
- Per-user tool listings and other stable session context are kept outside the
  cross-user system segment.
- Date, CWD, memory, and other volatile context stay in explicitly volatile
  request sections.
- A forked request preserves the parent request except for the documented
  appended content and cache-write flag.

Public regression coverage includes
`TestNormalizeToolInput_CanonicalizesKeyOrdering` and
`TestBuildSystemPrompt_BP1ByteStableAcrossMCPConfigs`.

## Session correlation

`CompletionRequest.SessionID` reaches the gateway for request correlation:

- `internal/agent/loop.go` sets the request session ID.
- `internal/client/gateway.go` serializes it as `session_id`.

The value is correlation metadata; it does not select a cache lifetime.

## Synthetic measurement example

The following values are deliberately invented to illustrate a result format.
They are not a product baseline, target, SLO, release gate, or production
measurement.

| Metric | Synthetic value |
|---|---:|
| Requests | 10 |
| Cache-read ratio | 90% |
| Cache read/write ratio | 9x |
| Model calls per turn | 2.0 |
| Drift events | 0 |

The public `scripts/cache_bench.sh` script is likewise an example harness.
Maintainers must define real fixtures, thresholds, and acceptance policy in the
private release process.

## Maintainer checks

When changing request construction:

1. Run the public byte-stability and gateway tests.
2. Confirm equivalent inputs serialize identically.
3. Confirm volatile or user-specific content does not enter shared segments.
4. Treat any benchmark output as local diagnostic evidence, not a public
   release decision.
5. Update this document only when the public Kocoro contract changes.

## Non-public provider features

Kocoro does not depend on private provider cache-edit protocols or
cache-key-invisible prompt sections. XML tags such as `<system-reminder>` are
ordinary message text and participate in the request byte stream.

## Durable Compaction Prefix

Compaction preserves cache convergence by separating archival and model-live
state. `Session.Messages` remains the complete transcript, while
`Session.CompactionCheckpoint` stores the exact summary + retained tail sent on
later turns. `HistoryForLoop` reuses those bytes and appends only raw transcript
messages created after the checkpoint's exclusive archive index. A
non-compacting turn never regenerates the summary, so the compacted prefix is
stable and can become a cache hit again. This is local prompt construction; it
does not change Cloud's cache TTL or write policy.

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
