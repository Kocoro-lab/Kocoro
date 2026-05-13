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

## Layer A: Desktop UI limits (recommended)

Aligned with claude.ai web client conventions, lenient where Kocoro's 1M-context Opus 4.7 makes it safe.

| Constraint | Value | Toast on violation |
|---|---|---|
| Images per message | **10** | `"Maximum of 10 images per message"` |
| Total attachments per message | **20** | `"Maximum of 20 attachments per message"` |
| Single file size | **20 MB** | `"File exceeds 20 MB limit"` |
| Image types | `.png`, `.jpg`, `.jpeg`, `.gif`, `.webp` | `"Unsupported image format"` |
| Document types | `.pdf`, `.docx`, `.xlsx`, `.pptx`, `.csv`, `.txt`, `.md`, `.json` | `"Unsupported file format"` |
| Archive types | `.zip`, `.tar`, `.tar.gz`, `.tgz` | `"Unsupported archive format"` |

**UX rules:**
- Numbered chip per attachment with thumbnail + filename + size + ✕ remove button.
- Live counter (`8/10 images`, `18/20 files`) so the user sees they're approaching the limit.
- Soft warning at 90 %, hard block at 100 % with toast.
- Never silently drop — always toast.

**Rationale:**
- 10 images is more generous than claude.ai's 5/turn cap, justified by Kocoro's 1M-context model and downstream auto-compression.
- 20 total attachments matches claude.ai's 20-files-per-conversation cap, applied per-message.
- 20 MB matches Kocoro daemon's current `maxInlineImage` / `maxImageReadSize` constants (`internal/daemon/runner.go` resolveFileRef, `internal/tools/file_read.go`). Promising the user 30 MB in Desktop UI while daemon rejects anything > 20 MB would create a confusing mismatch. (claude.ai's 30 MB cap depends on server-side preprocessing Kocoro doesn't have. If we later raise daemon caps to 30 MB, also add dimension-based OOM protection — a 30 MB PNG can decode to 240+ MB RGBA in memory.)

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

| Constraint | Anthropic API | claude.ai | Kocoro Desktop (proposed) |
|---|---|---|---|
| Inline image | 5 MB base64 string | 10 MB (server compresses) | 20 MB raw, daemon compresses |
| Images per request | 100 (200K) / 600 (others) | 5 per turn | **10 per message** |
| Total attachments | — | 20 per conversation | **20 per message** |
| Total request | 32 MB | — | bounded by per-file caps |
| Image dimensions | ≤ 8000×8000 (single), ≤ 2000×2000 (many-image, >20) | — | daemon resizes to ≤ 2000×2000 |
| PDF pages | 100 (200K) / 600 (others) | — | daemon renders at 1024 px width |

Sources:
- [Anthropic Vision Docs](https://platform.claude.com/docs/en/build-with-claude/vision)
- [Upload files to Claude Help Center](https://support.claude.com/en/articles/8241126-upload-files-to-claude)
- [Anthropic API Overview — request size limits](https://platform.claude.com/docs/en/api/overview)

## Open questions for the Desktop team

1. Should the 20 MB per-file cap be configurable per workspace? (And if raised to 30 MB to match claude.ai, what dimension/decoded-RGBA cap should we add to avoid OOM?)
2. Should camera photos be auto-rotated based on EXIF in the thumbnail preview?
3. For PDFs, show a page-count badge so users know how many pages will be processed?
