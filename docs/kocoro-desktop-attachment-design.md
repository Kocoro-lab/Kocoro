# Kocoro Desktop Attachment Design

**Audience:** Kocoro Desktop frontend team.

**Goal:** Align Desktop's user-facing attachment UI with claude.ai conventions while letting the Kocoro daemon's agent loop read arbitrary numbers of local files via tools.

## Background

Kocoro #135 surfaced two cases with one fix and one UX gap:

1. A single PNG > 3.75 MB raw triggers `400: image exceeds 5 MB maximum` (now fixed in the daemon via three-layer image guard — see `internal/tools/imaging_compress.go`).
2. Loading "all screenshots on my desktop" works on Claude Desktop but Desktop's frontend has no upload limit guardrails — users can drag-drop unlimited files without feedback.

Item 2 is what this doc addresses.

## Two independent channels

```
User intent: "Look at these specific files"
─────────────────────────────────────────────
   ↓ drag/drop/paste in Desktop composer
   ↓ Desktop validates (count, size, type)  ←─── Layer A: UI-side limits
   ↓ Files attached to outgoing message
   ↓ Sent to daemon as message attachments
   ↓ Daemon's resolveFileRef compresses via tools.EncodeImageBytes
   ↓ Sent to Anthropic as image/document content blocks


User intent: "Analyze everything in /Users/me/Desktop"
──────────────────────────────────────────────────────
   ↓ User types prompt; no files attached
   ↓ Daemon hands prompt to agent
   ↓ Agent decides to call file_read N times
   ↓ Each file_read result is auto-compressed by EncodeImage  ←─── Layer B
   ↓ Streamed back via tool calls — model "sees" each image one at a time
```

**Key insight:** Layer A applies only when the user explicitly attaches files. Layer B applies when the agent reads files autonomously. The agent is NOT bound by Layer A's count — it can read 50 files in a turn if the user asks.

## Layer A: Desktop UI limits (shipped status)

Status as of 2026-05-14. All rows are shipped; the only gap relative to claude.ai is the per-conversation cumulative cap (deferred — see "Not implemented" below).

| Constraint | Value | Status |
|---|---|---|
| `maxAttachmentsPerMessage` | **20** | ✅ shipped — total file count cap, matches claude.ai's per-message cap |
| `maxFileSize` per disk file | **500 MB** | ✅ shipped — aligned with daemon Phase 1 |
| `maxTotalAttachmentSize` | **500 MB** | ✅ shipped |
| Clipboard inline paste cap | **20 MB** | ✅ shipped — matches daemon's `maxInlineImageDecodedBytes` |
| `maxImageDimension` | **8192×8192** | ✅ shipped — matches Anthropic single-image upper bound |
| Image type allowlist | jpg / jpeg / png / gif / webp / heic / heif / avif / tiff / bmp | ✅ shipped |
| Document type allowlist | pdf / docx / pptx / key / txt / md / html / rtf / odt / epub | ✅ shipped |
| Data type allowlist | csv / json / xlsx | ✅ shipped (rendered as "Data" chip; the 500 MB / 20-file caps apply — no separate `maxDataAttachments` quota) |
| Code extensions (text fallback) | 50+ entries — see `AttachmentLimits.supportedCodeExtensions` | ✅ shipped |
| Archive types | zip (others fall through to file_ref) | ✅ shipped |
| Folder drops | treated as directory `file_ref` | ✅ shipped (ShanClawKit; SHA omitted — lives in a separate repo) |
| Universal accept (unknown ext → file_ref) | ✅ shipped — daemon decides downstream | |
| Per-violation `lastError` toast | ✅ shipped — only feedback when limit hit, matches claude.ai behavior | |

**UX rules** (current behavior):
- Numbered chip per attachment with thumbnail + filename + size + ✕ remove button.
- No live counter — claude.ai itself doesn't show one. Capacity info is implicit (the chip strip shows what's attached) and the toast on violation tells the user when they've hit the cap.
- On hard block, `AttachmentState.lastError` triggers a toast.
- Universal acceptance: unknown extensions are accepted as file_ref and the daemon decides whether to extract / passthrough / refuse.

**Rationale for Kocoro's 500 MB-per-file cap (vs claude.ai's 30 MB)**:
Kocoro daemon auto-compresses images at source (`internal/tools/imaging_compress.go`) and treats disk files as `file_ref` paths (not inline base64). The 5 MB Anthropic inline limit is enforced server-side by the daemon, not gated client-side. The 20-attachments-per-message cap matches claude.ai exactly.

**Not implemented (future / explicit non-goals)**:
- Image-specific cap (`maxImagesPerMessage`): claude.ai does NOT have one — they use a single per-message attachment cap. Adding an image-only cap was investigated and rejected as scope inflation.
- Per-conversation cumulative cap (claude.ai has "20 files per conversation" across messages): requires cross-message state tracking and is deferred to follow-up.

## Layer B: daemon-side compression (already implemented)

- `internal/tools/imaging_compress.go` — JPEG quality ladder (`compressImage`)
- `internal/tools/imaging.go` — `EncodeImage` (`file_read` etc.) + `EncodeImageBytes` (in-memory buffers)
- `internal/daemon/runner.go:resolveFileRef` — Desktop attachment path also runs through `EncodeImageBytes`
- `internal/agent/oversize_image.go` — wire-time + persist-time sanitizers (`filterOversizeImages`, `SanitizedRunMessages`)
- `internal/client/gateway.go` — `MaxInlineImageBase64Bytes = 5 MB`

## What Desktop should NOT do

- Do NOT enforce Layer A limits on Layer B (agent-read files are independent).
- Do NOT pre-compress on Desktop side; daemon handles it.
- Do NOT silently drop attachments; always toast.
- Do NOT promise a count in UI copy that doesn't match the actual cap — toast wording must match `AttachmentLimits` constants verbatim (currently 20 per message).

## Reference data (Anthropic public limits)

| Constraint | Anthropic API | claude.ai | Kocoro Desktop (shipped) |
|---|---|---|---|
| Inline image | 5 MB base64 string | 30 MB (server compresses) | 500 MB raw on disk (daemon compresses); 20 MB inline paste |
| Images per request | 100 (200K) / 600 (others) | no separate per-image cap | no separate per-image cap |
| Total attachments | — | 20 per message, 20 per conversation (cumulative) | **20 per message** (per-conversation cumulative cap deferred) |
| Total request | 32 MB | — | 500 MB daemon-side disk read; Anthropic still receives ≤32 MB after daemon compression |
| Image dimensions | ≤ 8000×8000 (single), ≤ 2000×2000 (many-image, >20) | — | daemon resizes to ≤ 2000×2000 |
| PDF pages | 100 (200K) / 600 (others) | — | daemon renders at 1024 px width |

Sources:
- [Anthropic Vision Docs](https://platform.claude.com/docs/en/build-with-claude/vision)
- [Upload files to Claude Help Center](https://support.claude.com/en/articles/8241126-upload-files-to-claude)
- [Anthropic API Overview — request size limits](https://platform.claude.com/docs/en/api/overview)

## Open questions for the Desktop team (remaining after this PR)

1. Should camera photos be auto-rotated based on EXIF in the thumbnail preview?
2. For PDFs, show a page-count badge so users know how many pages will be processed?
