# internal/tools — Local Tool Registry

Registration lives in `internal/tools/register.go`. This reference loads when working under
`internal/tools/`; the always-loaded root `CLAUDE.md` keeps only a pointer. Approval/deny-list
policy that applies repo-wide stays in the root file under **Daemon Approval Protocol**.

Always registered (`internal/tools/register.go RegisterLocalTools`):

- **File**: file_read (auto-compresses images >3.75 MB raw, see `imaging_compress.go`), file_write, file_edit, glob, grep, directory_list
- **Archive**: archive_inspect (read-only), archive_extract (approval). Zip/tar/tar.gz via stdlib. Atomic staging+rename; rejects encrypted/absolute/symlink/device/setuid; zipbomb caps (50 MB/entry, 200 MB total, 500 entries). See `archive.go`.
- **Documents**: pdf_to_text, docx_to_text, xlsx_to_text, pptx_to_text. Prefer poppler/pandoc/xlsx2csv; fall back to unzip+XML strip (no fallback for PDF — surfaces `brew install poppler` hint + suggests upload for native Anthropic document block). Fixed-argv, 60s timeout, 100K-rune output cap. See `doc_extract.go`.
- **Shell/system**: bash, system_info, process, http, think
- **macOS GUI**: computer_use (primary native-GUI workflow), accessibility (legacy low-level AX), applescript, screenshot, computer, clipboard, notify, browser, wait_for, ghostty. On daemon runs, the ordinary parent sees one high-level `computer_use` task function and keeps its configured model (Sonnet 5 by default). Only an actual call lazily resolves `openai.computer.v1` and starts a private OpenAI Responses trajectory. A single exact task app binds background-first; semantic press/scroll and ordinary target-bound input stay in that lane, while `foreground_allowed` may activate the target only when an action lacks an exact background primitive. Multi-app tasks retain foreground switching. Screenshots, pointer actions, typing, re-observation, continuation, `state_id`, refs, and coordinate frames stay internal. NSWorkspace + CGWindow provides a coordinate-capable target when AX is incomplete, while foreground OpenAI pointer actions use the visible CGEvent path. Ambient physical interference requires one exact fresh observation before another mutation; explicit Pause/Take Over/Stop retains user-owned quiescence. The whole call uses the shared GUI-operation lock. The standalone `screenshot` tool remains separate and approval-gated. Unattended `computer_use` still requires the explicit persisted global grant.
- **Schedule**: schedule_create / _list / _update / _remove / _show
- **Memory**: memory_append (flock-protected MEMORY.md append)
- **User interaction**: ask_user_question — closed-choice escalation (1-4 questions, 2-4 options each; model receives full option labels, not bare tokens). `RequiresApproval()==false` — its own request/resolve interaction, NOT an approval. Reaches the daemon `QuestionBroker` through an `agent.QuestionAsker` injected on the tool-call context (`internal/tools` can't import `internal/daemon`); no asker on ctx (unattended / non-interactive channel / sync HTTP / TUI) → clean "can't ask here, use best judgment" result. Its explicit Direct exposure keeps it in the first-turn schema set; the volatile `Structured question UI: available` capability line gates calls on surfaces with a live asker. See Wire Contract Discipline + `internal/daemon/question.go` / `question_broker.go` / `pending.go`. Over-asking is suppressed by the "## Asking the user" prompt gate (`internal/prompt/builder.go`), not the tool description.
- **Skills**: use_skill

Conditional:

- `session_search` — when session manager available
- `cloud_delegate` — `cloud.enabled: true`
- `publish_to_web` — `cloud.enabled` + `cfg.APIKey`. Always approval. Path-segment + basename blocklist (`.env`/`.pem`/…); extension allowlist (`cloud.publish_allowed_extensions`). All uploads tagged `kind=other` server-side; the kind enum (`session_share`/`report`/`landing_page`/`image`/`other` — see `internal/uploads/client.go`) is NOT exposed to the model.
- `list_my_published_files` — same gating. Read-only, no approval. `limit` (≤100), `offset`, optional `kind` filter (same enum). Returns paged `UploadEntry` rows keyed by id; rendering surfaces a `kind=…` badge per row so the LLM can answer "which of these are session shares".
- `retract_published_file` — same gating. Destructive, requires approval. Args: `id` (UUID from list) + `description`. 404 conflates not-found/already-retracted/not-yours to avoid existence leak.
- `generate_image` / `edit_image` — same gating. Always approval (paid quota + permanent CDN). Edit requires `image_urls` 1-4 entries starting with `https://static.kocoro.ai/`.
- `tool_search` — registered Direct whenever the effective registry contains cold Deferred tools; keyword retrieval uses the internal deterministic BM25 index in `agent/toolsearch_index.go`
- **`calendar_*` family (8 tools)** — registered only when daemon is a Kocoro Desktop subprocess (`tools.RegisterCalendarTools` no-ops when the `DesktopRPCBroker` is nil; TUI/one-shot/MCP/scheduled paths fall back to `applescript` + Calendar.app). Tools: `calendar_check_permission`, `calendar_request_permission` (approval, 5-min TCC-dialog timeout), `calendar_list_sources`, `calendar_list_events`, `calendar_get_event`, `calendar_create_event` / `_update_event` / `_delete_event` (approval). Backed by `docs/desktop-calendar-rpc.md` v0.5.1 (Unix socket reverse RPC to Desktop's EventKit). `attendees` is metadata-only — `invitations_sent` always `false` in v1. `update_event` rejects `scope=all`; use delete + create.
