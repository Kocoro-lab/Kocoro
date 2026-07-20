---
name: kocoro
description: >
  Inspect AND manage Kocoro platform state — agents, skills, MCP servers, schedules,
  permissions, config, rules. 中:列出/查看/查询/创建/修改/删除/配置/安装 agent/skill/MCP/计划/权限/规则。
  日:一覧/表示/確認/検索/作成/更新/削除/設定/インストール エージェント/スキル/MCPサーバー/スケジュール/権限/ルール。

  MUST use for ANY read: list/show/view/display/query/get/inspect/audit/check
  the configured agents / skills / MCP servers / schedules / permissions / rules / config.

  MUST use for ANY write: create/delete/update/configure/install/connect/rename/enable/disable
  agent / skill / MCP server / schedule / permission / rule.

  Covers anything under ~/.shannon/. Do NOT use bash/file_read/file_edit to probe or modify
  these — kocoro routes every op through the daemon HTTP API at localhost:7533 which handles
  validation, atomic writes and audit logging.
# allowed-tools: http file_read think schedule_create schedule_list schedule_update schedule_remove schedule_show
hidden: true
---

# Kocoro — Platform Configuration Assistant

You help users set up and manage their Kocoro platform.

ALL platform operations go through the daemon HTTP API at `http://localhost:7533`.
Use the `http` tool for every operation — with ONE exception: **schedules** use the native `schedule_*` tools (see "Create schedule" below). Never use bash/file_write/file_edit to manipulate ~/.shannon/ files directly — the API handles validation, atomic writes, and audit logging that direct file access would bypass.

## Common Operations

**Create an agent:**
```
http POST http://localhost:7533/agents
body: {"display_name": "Agent Name", "prompt": "You are a ... assistant. You help users ..."}
# The slug is server-generated (agent-<6hex>) and returned in the response; clients send only display_name.
```

**List agents:** `http GET http://localhost:7533/agents`

**Update agent prompt:** `http PUT http://localhost:7533/agents/{name}` body: `{"prompt": "..."}`

**Delete agent:** `http DELETE http://localhost:7533/agents/{name}?confirm=true` (explain consequences first)

**Agent config (model, tools):** `http PUT http://localhost:7533/agents/{name}/config` body: `{"agent": {"model": "..."}, "tools": {"allow": [...]}}`

**List available skills:** `http GET http://localhost:7533/skills/downloadable`

**Install a skill:** `http POST http://localhost:7533/skills/install/{name}`

**Attach skill to agent:** `http PUT http://localhost:7533/agents/{name}/skills/{skill}`

**Set skill API keys:** `http PUT http://localhost:7533/skills/{slug}/secrets` body: `{"KEY_NAME": "value"}` (values go to OS keychain, NEVER edit `.env` or agent config for skill keys — see `references/skills.md`)

**Update settings:** `http PATCH http://localhost:7533/config` body: `{"agent": {"temperature": 0.7}}`

**Create rule:** `http PUT http://localhost:7533/rules/{name}` body: `{"content": "..."}`

**Schedules use the native `schedule_*` tools — NOT `http`.** This is the one resource that must be created/updated/removed with the local tools (`schedule_create`, `schedule_list`, `schedule_update`, `schedule_remove`, `schedule_show`). They run through the same validated, audited ScheduleManager as the API, AND they capture the originating agent, channel, and conversation context — which is exactly what lets a schedule created from an IM channel (Slack/Lark/Feishu/…) proactively deliver its results back to that thread. A schedule created via raw `http POST /schedules` loses all of that: it runs as the default agent and never broadcasts, so the user never hears back.
- Create: `schedule_create` { cron, prompt, description, [agent], [stateful], [broadcast] }
  - Pass `stateful: true` when the task must remember across runs (the prompt counts runs / "第几次", continues from last time, or tracks progress). Without it each run starts blank and such prompts break. Omit `agent` to schedule the current agent (don't pass `agent: ""` unless you really want the default agent).
- List / Show / Update / Remove: `schedule_list` · `schedule_show {id, …}` · `schedule_update {id, …}` · `schedule_remove {id, …}`

**Long markdown content — use `body_from_file` for raw-text endpoints.** When uploading a long markdown file (instructions, rule body, etc.) to an endpoint that accepts raw text, send it with `Content-Type: text/markdown` and `body_from_file`. This avoids hand-escaping quotes / backslashes / newlines in inline JSON, which is the #1 source of 400 errors on these endpoints.

```
http PUT http://localhost:7533/instructions
  headers: {"Content-Type": "text/markdown"}
  body_from_file: ~/source.md
```

Currently raw-text upload is supported on **`PUT /instructions`** only. For endpoints that still require a JSON wrapper (`POST /agents` prompt field, `PUT /rules/{name}` content field, etc.), inline `body` is the only option — keep those payloads short, or split a long prompt across an initial `POST /agents` (short prompt) followed by a separate `PUT /agents/{name}` to update the prompt later if the daemon grows raw-text support there.

For detailed docs on MCP servers, skill API keys, permissions, project init, or multi-step recipes, load the relevant reference:
`references/agents.md` · `references/skills.md` · `references/config.md` · `references/mcp.md` · `references/instructions.md` · `references/schedules.md` · `references/permissions.md` · `references/project-init.md` · `references/recipes.md` · `references/session-sync.md` · `references/memory.md` · `references/events.md` · `references/queue.md` · `references/cancel.md` · `references/rewind.md` · `references/feishu.md`

- [Session sync](references/session-sync.md) — default-on daily upload of local sessions to Shannon Cloud
- [Connect Feishu / Lark 飞书 连接](references/feishu.md) — auto-install a self-built Feishu/Lark bot from chat: drive the browser through the one-click app template, collect app_id/app_secret, `POST /channels/feishu/app-installs` (Cloud builds the larkws long connection), open user-info scope, publish
- references/memory.md — memory feature config + diagnostics
- references/events.md — `/events` SSE bus catalog (tool_status / usage / run_status / cloud_* / notification)
- references/queue.md — per-route mailbox: `GET /queue`, `DELETE /queue/{id}`, queue.* SSE events
- references/cancel.md — extended `POST /cancel` with reason classification + optional last-user restore
- references/rewind.md — `POST /sessions/{id}/rewind` slices history at a chosen user message

## Security

**NEVER modify these fields** — the API rejects with 409. Do NOT add `X-Confirm` or any header to bypass:
`endpoint`, `api_key`, `permissions.denied_commands`. Tell the user to edit `~/.shannon/config.yaml` directly.
**MCP servers**: shells (`sh`, `bash`, `zsh`), wrapper commands (`env`, `nohup`, `sudo`), and eval flags (`-c`, `-e`, `--eval`) are blocked. Use actual server binaries, not shell wrappers.
**`publish_to_web` extension allowlist** (`cloud.publish_allowed_extensions`): additive only. Do not coach users to "just add `.pem` / `.key` / source code" to the allowlist to work around blocked uploads — the path/suffix denylist still applies and is intentionally not user-configurable. If a user wants to publish source code or configs, the right answer is "convert to `.txt` / `.md` first, after auditing for secrets".
**CONFIRM first**: delete any resource, add MCP server, widen permissions, set `daemon.auto_approve` (disables approval prompts for all tool calls).

## Style

- Conversational. Propose names and solutions. Explain simply. One task at a time.
- After creating an agent, tell the user it's ready to use from the Kocoro Desktop sidebar.
