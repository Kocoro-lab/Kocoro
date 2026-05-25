# Schedules

## What is this?

Schedules are automated tasks that run on a cron schedule without any human interaction. You define a prompt (what to do), a cron expression (when to do it), and optionally which agent to use. Shannon runs the task at the scheduled time, executes any tool calls automatically, and logs the output.

## API Endpoints

### List all schedules
- Method: GET
- Path: /schedules
- Response: `[{"id": "string", "prompt": "string", "cron": "0 9 * * 1-5", "agent": "string", "enabled": true, "last_run": "2024-01-15T09:00:00Z"}]`

### Get schedule details
- Method: GET
- Path: /schedules/{id}
- Response: `{"id": "string", "prompt": "string", "cron": "string", "agent": "string", "enabled": true, "last_run": "string", "last_result": "string"}`

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
1. GET /schedules/{id} → see `last_run` timestamp and `last_result` summary

## Safety Notes

- **Runs without interaction**: Scheduled tasks execute automatically and unattended. The agent will use tools without asking for approval. Make sure your prompt is specific enough that the agent knows what to do without needing clarification.
- **Disable vs delete**: Prefer disabling (PATCH with `enabled: false`) over deleting if you might want the schedule again. Deletion is permanent.
- **Agent selection**: If no agent is specified, the default agent runs the task. Specify an agent if you need specific tools, instructions, or memory.
- **Output**: Schedule run logs go to `~/.shannon/logs/schedule-{id}.log`. Check these if a scheduled task isn't behaving as expected.
- **Time zone**: Cron expressions use the system time zone of the machine running the Shannon daemon.
- **Overlapping runs**: If a scheduled task is still running when the next scheduled time arrives, the new run is skipped to prevent overlap.
