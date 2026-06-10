# Schedules

## What is this?

Schedules are automated tasks that run on a cron schedule without any human interaction. You define a prompt (what to do), a cron expression (when to do it), and optionally which agent to use. Shannon runs the task at the scheduled time, executes any tool calls automatically, and delivers the reply: to Kocoro Desktop always, and to every Cloud channel (Slack / Lark / Telegram / WeCom / Feishu) the agent — named or default — is OAuth-bound to.

## Create / update / remove with the native `schedule_*` tools (NOT `http`)

As the kocoro assistant, manage schedules with the local tools `schedule_create` / `schedule_update` / `schedule_remove` / `schedule_show` (and `schedule_list` to read) — do NOT `http POST /schedules` or raw `PATCH /schedules` for schedule management. Only the native tools capture the run's originating agent, IM channel, and conversation context. That captured context is what lets a schedule created from a Slack / Lark / Feishu / … thread proactively deliver its results back to that exact thread, run as the right agent, and understand the task background. Creating via raw HTTP loses all of it (the schedule runs as the default agent and never broadcasts, so the user never hears back). The HTTP endpoints below remain documented for external/admin clients and for reads.

## API Endpoints

### List all schedules
- Method: GET
- Path: /schedules
- Response: `[{"id": "string", "prompt": "string", "cron": "0 9 * * 1-5", "agent": "string", "enabled": true, "stateful": true, "last_run_at": "2024-01-15T09:00:00Z", "last_run_session_id": "string", "last_run_message_start_index": 12, "last_run_message_end_index": 18}]`
- Notes: All `last_run_*` keys (including the message-index range) are absent from the JSON when the schedule has never fired — they are not present as `null`. The index range slices the run's turns out of the schedule's dedicated session; `GET /schedules/{id}/last-run` handles that resolution for you.

### Get schedule details
- Method: GET
- Path: /schedules/{id}
- Response: `{"id": "string", "prompt": "string", "cron": "string", "agent": "string", "enabled": true, "stateful": true, "last_run_at": "RFC3339 string", "last_run_session_id": "string", "last_run_message_start_index": 12, "last_run_message_end_index": 18}`
- Notes: Same shape as the list entry; `last_run_*` keys are omitted when never fired (absent, not `null`). The textual output of the last run is NOT inlined here; use `GET /schedules/{id}/last-run` for that.

### Show last run of a schedule
- Method: GET
- Path: /schedules/{id}/last-run
- Query: `?max_turns=N` (optional, default 5, clamped 1-20)
- Response: `{"last_run_at": "RFC3339 string", "session_id": "string", "agent_name": "string", "turns": [{"role": "assistant", "text": "..."}]}`
- Notes: Resolves the schedule's `last_run_session_id` and reads the linked session file, returning the tail of its assistant turns. When the schedule has never fired, `last_run_at` is absent and `session_id` is the empty string; `turns` is `[]` (never `null`). 404 on unknown schedule id. 500 if the session file is missing (e.g. user manually deleted it from disk).

### Show last run via LLM tool
- Tool: `schedule_show`
- Args: `{"id": "schedule_id", "max_turns": 5}` (max_turns optional)
- Returns: formatted text describing when it last ran, the session id, and the recent assistant turns. Use this when the user asks "what did my schedule produce" — call it yourself rather than pushing the user to run `session_search`.

### Create a schedule
- Method: POST
- Path: /schedules
- Body: `{"prompt": "Check the sales dashboard and summarize any anomalies", "cron": "0 9 * * 1-5", "agent": "analyst", "stateful": false}`
- Response: `{"id": "...", "prompt": "...", "cron": "...", "agent": "...", "enabled": true, "stateful": false}`
- Notes: `agent` is optional — omit to use the default agent. `cron` uses standard 5-field cron format. `stateful` is the single "remember across runs" switch and defaults to `false`: each run starts in a brand-new session with no prior context (recommended for digest/polling/PR-review style tasks). Set `stateful: true` to make the schedule accumulate in one dedicated session and have each run see prior runs' history (continuous tracking, a rolling standup/journal). Applies to both the default and named agents. See the Stateful note below.

### Update a schedule
- Method: PATCH
- Path: /schedules/{id}
- Body: `{"prompt": "Updated task...", "enabled": false, "stateful": true}`
- Response: `{"id": "...", "prompt": "...", "cron": "...", "agent": "...", "enabled": false, "stateful": true}`
- Notes: Only include fields you want to change. `stateful` accepts `true` or `false`; omit to leave the existing setting untouched. Legacy schedules created before this field existed have no `stateful` value on disk and run fresh (each run independent) — send `{"stateful": true}` to make them accumulate across runs.

### Delete a schedule
- Method: DELETE
- Path: /schedules/{id}?confirm=true
- Response: `{"status": "deleted"}`
- Notes: DESTRUCTIVE. `?confirm=true` required.

## Cron Expression Reference

| Schedule | Cron expression | Description |
|----------|----------------|-------------|
| Daily at 9am | `0 9 * * *` | Every day at 09:00 |
| Weekdays at 9am | `0 9 * * 1-5` | Monday–Friday at 09:00 |
| Every hour | `0 * * * *` | At the top of every hour |
| Every 30 minutes | `*/30 * * * *` | Every half hour |
| Weekly on Monday 8am | `0 8 * * 1` | Mondays at 08:00 |
| Monthly on 1st at noon | `0 12 1 * *` | 1st of each month at 12:00 |
| Twice daily (9am, 5pm) | `0 9,17 * * *` | At 9am and 5pm every day |

Format: `minute hour day-of-month month day-of-week`

Day-of-month is validated for feasibility: an impossible day/month combination (e.g. `0 0 31 2 *` — Feb 31, or `0 0 31 4 *` — Apr 31) is rejected at create/update time because it would never fire. For "last day of the month" use `L` (`0 0 L * *`), not `31` — `0 0 31 * *` silently skips every 30-day month and February.

## Common Scenarios

### "Run a daily report at 9am on weekdays"
1. Call `schedule_create` with:
   ```json
   {"agent": "dev-assistant", "cron": "0 9 * * 1-5", "prompt": "Generate a daily summary: check git log for recent commits, open issues, and any failing tests. Send a brief report.", "description": "create weekday daily report schedule"}
   ```
2. Call `schedule_list` to confirm it is listed and `enabled=true`.

### "Pause a schedule temporarily"
1. Call `schedule_update` with `{"id": "<schedule_id>", "enabled": false, "description": "pause schedule temporarily"}`.
2. Schedule is preserved but won't run. Re-enable with `{"id": "<schedule_id>", "enabled": true, "description": "re-enable schedule"}`.

### "Change when a schedule runs"
1. Call `schedule_update` with `{"id": "<schedule_id>", "cron": "0 8 * * *", "description": "change schedule to 8am daily"}`.

### "Check when a schedule last ran and what it did"
1. Call `schedule_show` with `{"id": "<schedule_id>", "description": "show latest schedule output"}`.
2. It returns `last_run_at`, the session id, and the recent assistant turns from that schedule run.

## Safety Notes

- **Runs without interaction**: Scheduled tasks execute automatically and unattended. The agent will use tools without asking for approval. Make sure your prompt is specific enough that the agent knows what to do without needing clarification.
- **Disable vs delete**: Prefer disabling (PATCH with `enabled: false`) over deleting if you might want the schedule again. Deletion is permanent.
- **Agent selection**: If no agent is specified, the default agent runs the task. Specify an agent if you need specific tools, instructions, or memory.
- **Output destinations**: Successful runs are delivered to (a) the local session JSON, (b) Kocoro Desktop via the `schedule_run` SSE event, and (c) **every Cloud channel the agent is OAuth-bound to** (Slack / Lark / Telegram / WeCom / Feishu), gated by the schedule's `broadcast` field.
- **Broadcast gate**: each schedule carries a `broadcast` setting with three values: `auto` (default), `on`, `off`.
  - `auto`: smart default by where the schedule was created. IM-source schedules (Slack / Lark / Feishu / Telegram / WeCom / LINE) broadcast their reply; Desktop / TUI / CLI schedules stay local. Pre-2026-05-27 schedules without the field also stay local (safe default).
  - `on`: always broadcast, regardless of creation source. Use when the user creates a schedule from Desktop but explicitly wants it to push to their bound IM channel.
  - `off`: never broadcast. Use when the user creates a schedule from IM but explicitly wants it local-only.
  - Default agent and named agents follow the same rule.
  - Set via `schedule_create`'s or `schedule_update`'s `broadcast` parameter (string enum `"auto" | "on" | "off"`). Omitting on create defaults to `auto`; omitting on update leaves the existing value unchanged.
  - **Proactive targeting**: a schedule created from an IM thread snapshots that thread's routing context at creation time, so when it later broadcasts, Cloud delivers the reply back to the originating thread (Slack / Feishu / LINE) instead of the channel at large. Schedules created from Desktop/TUI/CLI/cron have no such context and broadcast to all bound channels as before. Transparent — no parameter; falls back to broadcast on unsupported platforms (WeCom / Telegram) or when no thread context was captured.
- **Thread anchoring (`thread`)**: when a schedule broadcasts to an IM channel, `thread` controls whether each run lands in one shared thread or as separate top-level messages. Three values: `auto` (default), `on`, `off`.
  - `auto`: follow the task's memory mode. A `stateful` schedule (one accumulating session) collects all its runs into the originating thread; a `stateless` schedule (fresh session each run) posts each run as its own top-level message. Embodies the "one session ↔ one thread" model.
  - `on`: always collect runs in the same thread, regardless of `stateful`. Use when the user says they want everything in one place / one thread.
  - `off`: always post each run separately at the top level, regardless of `stateful`. Use when the user says "send it separately each time" / don't bundle into a thread.
  - Set via `schedule_create`'s or `schedule_update`'s `thread` parameter (string enum `"auto" | "on" | "off"`). Omitting on create defaults to `auto`; omitting on update leaves the existing value unchanged. This is an LLM-tool parameter only — it is NOT exposed on the HTTP `POST` / `PATCH /schedules` endpoints.
  - Platforms without real threads (LINE / WhatsApp / WeCom / Telegram) ignore this setting and deliver at the channel top level regardless.
- **Stateful (remember across runs)**: each schedule carries a single `stateful` switch (`false` default, or `true`), for both the default and named agents.
  - `false` (default): every run starts in a brand-new session with no prior context. Best for digests, polling, reports, monitoring — any task whose runs are independent.
  - `true`: all runs of this schedule accumulate in ONE dedicated session (route key `agent:<name>:schedule:<id>`, or `schedule:<id>` for the default agent) AND each run's LLM call sees that session's history. Choose when the user wants the agent to build continuously on this schedule's own prior runs (a rolling standup/journal, ongoing tracking).
  - Legacy schedules created before this field existed have no `stateful` on disk and run fresh (a behavior change from the old "named-agent runs shared one session" model — users who want accumulation set `stateful: true` explicitly).
- **Diagnostic logs**: For debugging, also see `~/.shannon/logs/schedule-{id}.log` (per-schedule run log) and `~/.shannon/logs/audit.log` (cross-cutting tool-call timeline).
- **Time zone**: Cron expressions use the system time zone of the machine running the Shannon daemon.
- **Overlapping runs**: If a scheduled task is still running when the next scheduled time arrives, the new run is skipped to prevent overlap.
