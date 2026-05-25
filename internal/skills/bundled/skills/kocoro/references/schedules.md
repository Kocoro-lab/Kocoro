# Schedules

## What is this?

Schedules are automated tasks that run on a cron schedule without any human interaction. You define a prompt (what to do), a cron expression (when to do it), and optionally which agent to use. Shannon runs the task at the scheduled time, executes any tool calls automatically, and delivers the reply: to Kocoro Desktop always, and to every Cloud channel (Slack / Lark / Telegram / WeCom / Feishu) the named agent is OAuth-bound to.

## API Endpoints

### List all schedules
- Method: GET
- Path: /schedules
- Response: `[{"id": "string", "prompt": "string", "cron": "0 9 * * 1-5", "agent": "string", "enabled": true, "stateful": true, "last_run_at": "2024-01-15T09:00:00Z", "last_run_session_id": "string", "last_run_message_start_index": 12, "last_run_message_end_index": 18}]`
- Notes: All `last_run_*` keys (including the message-index range) are absent from the JSON when the schedule has never fired — they are not present as `null`. The index range slices the run's turns out of the shared per-agent session; `GET /schedules/{id}/last-run` handles that resolution for you.

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
- Notes: `agent` is optional — omit to use the default agent. `cron` uses standard 5-field cron format. `stateful` defaults to `false` (each run starts with empty LLM history; recommended for digest/polling/PR-review style tasks). Set `stateful: true` for agents that need cross-run memory (continuous tracking, follow-up analysis).

### Update a schedule
- Method: PATCH
- Path: /schedules/{id}
- Body: `{"prompt": "Updated task...", "enabled": false, "stateful": true}`
- Response: `{"id": "...", "prompt": "...", "cron": "...", "agent": "...", "enabled": false, "stateful": true}`
- Notes: Only include fields you want to change. `stateful` accepts `true` or `false`; omit to leave existing setting untouched. Legacy schedules created before this field existed have no `stateful` value on disk and behave as if stateful — explicitly send `{"stateful": false}` to migrate them to the new stateless default.

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

## Common Scenarios

### "Run a daily report at 9am on weekdays"
1. POST /schedules with:
   ```json
   {"prompt": "Generate a daily summary: check git log for recent commits, open issues, and any failing tests. Send a brief report.", "cron": "0 9 * * 1-5", "agent": "dev-assistant"}
   ```
2. GET /schedules → confirm it's listed and `enabled: true`

### "Pause a schedule temporarily"
1. PATCH /schedules/{id} with `{"enabled": false}`
2. Schedule is preserved but won't run. Re-enable with `{"enabled": true}`.

### "Change when a schedule runs"
1. PATCH /schedules/{id} with `{"cron": "0 8 * * *"}` (changes to 8am daily)

### "Check when a schedule last ran and what it did"
1. GET /schedules/{id}/last-run → returns `last_run_at` + the recent assistant turns from the run's session
2. Or via LLM: call `schedule_show` with the schedule id

## Safety Notes

- **Runs without interaction**: Scheduled tasks execute automatically and unattended. The agent will use tools without asking for approval. Make sure your prompt is specific enough that the agent knows what to do without needing clarification.
- **Disable vs delete**: Prefer disabling (PATCH with `enabled: false`) over deleting if you might want the schedule again. Deletion is permanent.
- **Agent selection**: If no agent is specified, the default agent runs the task. Specify an agent if you need specific tools, instructions, or memory.
- **Output destinations**: Successful runs are delivered to (a) the local session JSON, (b) Kocoro Desktop via the `schedule_run` SSE event, and (c) **every Cloud channel the agent is OAuth-bound to** (Slack / Lark / Telegram / WeCom / Feishu). The Cloud-channel broadcast applies only when the schedule names a specific agent (`agent` field set) and the reply is non-empty. The default agent has no Cloud channel mapping, so its runs stay local.
- **Diagnostic logs**: For debugging, also see `~/.shannon/logs/schedule-{id}.log` (per-schedule run log) and `~/.shannon/logs/audit.log` (cross-cutting tool-call timeline).
- **Time zone**: Cron expressions use the system time zone of the machine running the Shannon daemon.
- **Overlapping runs**: If a scheduled task is still running when the next scheduled time arrives, the new run is skipped to prevent overlap.
