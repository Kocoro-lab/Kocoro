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

## Layer A: Desktop UI limits (shipped + remaining gap)

Status as of 2026-05-13:

| Constraint | Value | Status |
|---|---|---|
| `maxAttachmentsPerMessage` | **20** | ✅ shipped (commit `072841b`) — total file count cap |
| `maxImagesPerMessage` | **10** | ✅ shipped (this work) — image-specific cap, claude.ai parity |
| `maxFileSize` per disk file | **500 MB** | ✅ shipped — aligned with daemon Phase 1 |
| `maxTotalAttachmentSize` | **500 MB** | ✅ shipped |
| Clipboard inline paste cap | **20 MB** | ✅ shipped — matches daemon's `maxInlineImageDecodedBytes` |
| `maxImageDimension` | **8192×8192** | ✅ shipped — matches Anthropic single-image upper bound |
| Image type allowlist | jpg / jpeg / png / gif / webp / heic / heif / avif / tiff / bmp | ✅ shipped |
| Document type allowlist | pdf / docx / xlsx / pptx / key / txt / md / html / rtf / odt / epub | ✅ shipped |
| Code extensions (text fallback) | 50+ entries — see `AttachmentLimits.supportedCodeExtensions` | ✅ shipped |
| Archive types | zip (others fall through to file_ref) | ✅ shipped |
| Folder drops | treated as directory `file_ref` | ✅ shipped (commit `702cf59`) |
| Universal accept (unknown ext → file_ref) | ✅ shipped — daemon decides downstream | |
| **Live counter `N/10 images · M/20 attachments`** | ✅ shipped (this work) — color-shifts at 90% / 100% | |
| Per-violation `lastError` toast | ✅ shipped | |

**UX rules** (current behavior):
- Numbered chip per attachment with thumbnail + filename + size + ✕ remove button.
- Live counter above chip row, only visible when ≥ 1 attachment.
- Counter is `.secondary` text below 90% capacity, `.orange` at 90%, `.red` at 100%.
- On hard block, `AttachmentState.lastError` triggers a toast; counter stays red.
- Universal acceptance: unknown extensions are accepted as file_ref and the daemon decides whether to extract / passthrough / refuse.

**Rationale for Kocoro choosing more permissive numbers than claude.ai**:
- 500 MB per file (vs claude.ai's 30 MB): Kocoro daemon auto-compresses images at source (`internal/tools/imaging_compress.go`) and treats disk files as `file_ref` paths (not inline base64). The 5 MB Anthropic inline limit is enforced server-side by the daemon, not gated client-side.
- 20 attachments per message (vs claude.ai's 5/turn for images): Kocoro's 1M-context Sonnet 4.6 can absorb more parallel files than the smaller-context default behind claude.ai.
- Image cap of 10 (vs claude.ai's 5): more generous given the larger context, but still binds before the total cap so users get image-specific feedback.

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
- Do NOT use a "Maximum of 5 images" copy if your real limit is 10 — keep UI text honest.

## Reference data (Anthropic public limits)

| Constraint | Anthropic API | claude.ai | Kocoro Desktop (shipped) |
|---|---|---|---|
| Inline image | 5 MB base64 string | 10 MB (server compresses) | 500 MB raw on disk (daemon compresses); 20 MB inline paste |
| Images per request | 100 (200K) / 600 (others) | 5 per turn | **10 per message** |
| Total attachments | — | 20 per conversation | **20 per message** |
| Total request | 32 MB | — | 500 MB upload, daemon shrinks |
| Image dimensions | ≤ 8000×8000 (single), ≤ 2000×2000 (many-image, >20) | — | daemon resizes to ≤ 2000×2000 |
| PDF pages | 100 (200K) / 600 (others) | — | daemon renders at 1024 px width |

Sources:
- [Anthropic Vision Docs](https://platform.claude.com/docs/en/build-with-claude/vision)
- [Upload files to Claude Help Center](https://support.claude.com/en/articles/8241126-upload-files-to-claude)
- [Anthropic API Overview — request size limits](https://platform.claude.com/docs/en/api/overview)

## Open questions for the Desktop team (remaining after this PR)

1. Should camera photos be auto-rotated based on EXIF in the thumbnail preview?
2. For PDFs, show a page-count badge so users know how many pages will be processed?
