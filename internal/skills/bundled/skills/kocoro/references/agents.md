# Agents

## What is this?

Agents are specialized AI assistants that you configure for specific tasks or personas. Each agent has its own instructions, memory, and toolset — for example, a "customer-support" agent that always responds in a friendly tone, or a "code-reviewer" agent that only uses file-reading tools. Agents persist between conversations so they accumulate knowledge over time.

## API Endpoints

### List all agents
- Method: GET
- Path: /agents
- Response: `{"agents": [{"name": "...", "builtin": false, "override": false}]}`

### Get agent details
- Method: GET
- Path: /agents/{name}
- Response: `{"name": "string", "prompt": "string", "config": {...}, "skills": [...], "commands": [...]}`

### Create agent
- Method: POST
- Path: /agents
- Body: `{"name": "my-agent", "prompt": "You are a helpful assistant that..."}`
- Response: `{"name":"...","prompt":"...","memory":null,"config":null,"commands":null,"skills":null,"builtin":false,"overridden":false}`
- Notes: Name must match `^[a-z0-9][a-z0-9_-]{0,63}$` — lowercase ASCII letters, numbers, hyphens, underscores only. No spaces, no non-ASCII characters. **Pass the user's slug verbatim — never translate or transliterate.** See "Name discipline" below.

### Update agent prompt / instructions
- Method: PUT
- Path: /agents/{name}
- Body: `{"prompt": "Updated instructions..."}`
- Response: `{"status": "updated"}`

### Delete agent
- Method: DELETE
- Path: /agents/{name}?confirm=true
- Response: `{"status": "deleted"}`
- Notes: DESTRUCTIVE. The `?confirm=true` query parameter is required. Agent files are removed but historical sessions and memory snapshots in the sessions directory are preserved.

### Update agent config
- Method: PUT
- Path: /agents/{name}/config
- Body: `{"cwd": "/path/to/project", "agent": {"model": "claude-opus-4-5"}, "tools": {"allow": ["bash:git *"], "deny": ["bash:rm *"]}}`
- Response: `{"status": "updated"}`
- Notes: Supports `cwd`, `agent.model`, `agent.temperature`, `tools.allow`, `tools.deny`, `mcp_servers`, `permissions.always_allow_tools`.

### Add tool to agent's always-allow list
- Method: POST
- Path: /agents/{name}/permissions/always-allow
- Body: `{"tool": "file_write"}`
- Response: `{"status": "added"}` on success; `400` if the tool is high-risk and cannot be persisted (`publish_to_web`, `generate_image`, `edit_image`).
- Notes: Appends the tool name to `permissions.always_allow_tools` in the agent's `config.yaml`. Next time this agent calls the named tool, the approval prompt is skipped. Idempotent (duplicate add is a no-op). Distinct from `tools.allow` — that's a schema filter (controls what the LLM can see); this is an approval bypass (controls whether the user is prompted at run time). Also written automatically when the user clicks "Always Allow" on an approval prompt (both bash and non-bash tools, as long as the message routed to a named agent) — Desktop/Cloud do not need to call this endpoint directly in that flow. **Safety gates that remain even with `bash` in this list**: (a) high-risk bash commands (`pip install`, `rm -rf`, `python -c`, `git push --force`, etc.) still prompt every call — see the always-ask gate in `permissions.md`; (b) paid / permanent-public tools (`publish_to_web`, `generate_image`, `edit_image`) cannot be persisted at all.

### Remove tool from agent's always-allow list
- Method: DELETE
- Path: /agents/{name}/permissions/always-allow
- Body: `{"tool": "file_write"}`
- Response: `{"status": "removed"}`. No-op (200) if the tool is not in the list.
- Notes: Future calls to this tool from this agent will prompt for approval again.

### Add tool to GLOBAL always-allow list (all agents, incl. default)
- Method: POST
- Path: /permissions/always-allow
- Body: `{"tool": "bash"}`
- Response: `{"status": "added"}` on success; `400` if the tool is high-risk and cannot be persisted (`publish_to_web`, `generate_image`, `edit_image`).
- Notes: Appends to `permissions.always_allow_tools` in `~/.shannon/config.yaml` (global scope). Applies to EVERY agent including the default agent that has no per-agent config. Use this for tools the user trusts broadly (e.g. `bash`, `file_write`) so non-technical users on the default agent don't get re-prompted on every command-string variant. Use the per-agent endpoint when trust should be limited to a single agent. Same safety gates apply: high-risk bash commands (`pip install`, `rm -rf`, etc.) still prompt every call regardless.

### Remove tool from GLOBAL always-allow list
- Method: DELETE
- Path: /permissions/always-allow
- Body: `{"tool": "bash"}`
- Response: `{"status": "removed"}`. No-op (200) if absent.

### Attach skill to agent
- Method: PUT
- Path: /agents/{name}/skills/{skill}
- Response: `{"status": "attached"}`
- Notes: Skill must exist. See skills reference for how to install skills.

### Detach skill from agent
- Method: DELETE
- Path: /agents/{name}/skills/{skill}
- Response: `{"status": "deleted"}`

### Create agent command
- Method: PUT
- Path: /agents/{name}/commands/{cmd}
- Body: `{"content": "When user says /report, generate a daily summary..."}`
- Response: `{"status": "updated"}`
- Notes: Command name becomes a slash command the agent recognizes (e.g., `/report`).

### List sessions
- Method: GET
- Path: /sessions[?agent={name}]
- Response: `{"sessions": [{"id", "title", "created_at", "msg_count", "source"?, "in_progress"?, "awaiting_approval"?}]}`
- Notes: Empty sessions (msg_count == 0) are filtered out. `source` identifies the originating IM / surface. Canonical values are the `Channel*` constants in `internal/daemon/types.go`, currently `slack`, `line`, `teams`, `wechat`, `wecom`, `web`, `feishu`, `lark`, `discord`, `telegram`, `schedule`, `system`, `webhook` — plus `kocoro` (set by `POST /messages` when the inbound request omits a source, i.e. the Desktop / TUI path). Empty / omitted on legacy sessions written before the column existed; frontends should treat empty as "unknown" and fall back to a generic icon. `in_progress` is true when the daemon currently owns an in-flight agent run for the session; `awaiting_approval` is true when the agent loop is blocked waiting for the user to approve a tool call. The two runtime flags are omitted (not emitted as false) when not set, and reset to false on daemon restart.

### List sessions awaiting approval
- Method: GET
- Path: /approvals
- Response: `{"sessions": ["sess_abc", "sess_def"]}` (empty array when nothing pending)
- Notes: Returns the set of session IDs whose agent loop is currently blocked on an approval prompt. Use this on first connect / page refresh to re-sync — the `approval_request` SSE event covers live updates but is lossy across reconnect. Pairs with `POST /approval` (resolve a pending request_id) for the full approval flow.

### Reset agent session history (in place)
- Method: POST
- Path: /sessions/{id}/reset?agent={name}
- Response: `{"status": "reset", "id": "..."}`
- Notes: Clears the session's conversation history while keeping the session ID, title, CWD, source, channel, and cumulative usage. Cancels any active run on that session first. Also clears any persisted route binding (the link from a messaging-platform thread/sender to this session) and the live in-memory binding, so the next inbound message on that route starts a fresh session. The `agent` query parameter is REQUIRED — default-agent sessions do not use this endpoint; delete and recreate them via `DELETE /sessions/{id}` instead. Use when the user says "reset", "clear history", or "start over" on a named agent whose routing identity must survive the wipe.

### `GET /agents/{name}/sessions/{id}/suggestion`

Returns the latest prompt suggestion for the given session, or 404 if none.
Default-agent equivalent: `GET /sessions/{id}/suggestion`.

Response (200):
```json
{
  "text": "rerun the failing test",
  "suggested_at_unix": 1715500000
}
```

Errors:
- 400 if `id` is empty or contains path-traversal characters.
- 400 if `name` is not a valid agent name (regex: `^[a-z0-9][a-z0-9_-]{0,63}$`).
- 404 if the agent does not exist OR no suggestion is currently available for the session.

### `POST /agents/{name}/sessions/{id}/suggestion/accept`

Marks the current suggestion as accepted and returns the suggestion text
so Desktop can fill the input. The user still presses Enter to send — the
normal `POST /agents/{name}/messages` flow handles persistence. There is
no speculative pre-run of the next assistant reply.

Default-agent equivalent: `POST /sessions/{id}/suggestion/accept`.

Response (200):
```json
{
  "text": "rerun the failing test",
  "suggestion": "rerun the failing test",
  "suggested_at_unix": 1715500000
}
```

Errors: same shape as the GET endpoint.

### SSE event `suggestion_ready`

Emitted on `/events` when a new suggestion is generated. Payload:

```json
{
  "session_id": "sess_abc",
  "agent": "myagent",
  "text": "rerun the failing test"
}
```

Wire format follows the HTML5 EventSource spec (two lines per event,
separated by a blank line):

```
id: 42
event: suggestion_ready
data: {"session_id":"sess_abc",...}

```

Desktop's `EventSource.addEventListener("suggestion_ready", ...)` parses
`event.data` as JSON. There is no outer `{"type":...,"payload":...}`
wrapper — `event:` is a header line, `data:` is the JSON body.

## Common Scenarios

### "Create an email writer agent"
1. POST /agents with `{"name": "email-writer", "prompt": "You are an expert email writer. Write professional, concise emails. Always ask for the recipient, purpose, and tone before drafting."}`
2. Verify: GET /agents/email-writer

### "Restrict agent to read-only tools"
1. PUT /agents/{name}/config with `{"tools": {"allow": ["file_read", "glob", "grep", "directory_list"], "deny": ["file_write", "file_edit", "bash"]}}`
2. Agent will only be able to read files, never modify them.

### "Give agent access to a specific project"
1. PUT /agents/{name}/config with `{"cwd": "/Users/me/projects/myapp"}`
2. Agent's file operations will default to that directory.

### "Add a slash command"
1. PUT /agents/my-agent/commands/standup with `{"content": "Generate a standup report: what was done yesterday, what's planned today, any blockers. Check git log and open issues."}`
2. Users can now say `/standup` to trigger this workflow.

### "Stop asking me to approve file_write for this agent"
1. POST /agents/{name}/permissions/always-allow with `{"tool": "file_write"}`
2. The next time this agent invokes `file_write`, the approval prompt is skipped.
3. To revert: DELETE /agents/{name}/permissions/always-allow with the same body.

### "Let this agent run any bash command without asking"
1. POST /agents/{name}/permissions/always-allow with `{"tool": "bash"}`
2. From now on, every bash call from this agent skips approval — **except** commands matching the always-ask gate (`pip install`, `rm -rf`, `python -c`, `git push --force`, `npx`, `curl|sh`, etc.), which still prompt every call regardless. This is the tool-level alternative to authorizing individual command strings via `permissions.allowed_commands`.
3. Pair with a clear explanation to the user: this grants broad shell access (subject to the always-ask gate). For finer control, leave `bash` out of the list and use `permissions.allowed_commands` with specific command patterns instead.

### "Stop asking me on the default agent (for non-technical users)"
1. POST /permissions/always-allow with `{"tool": "bash"}` (and `file_write`, `http`, etc. as needed).
2. The global list applies to the default agent (and every other agent). Users without a named agent now also benefit from "click once, never asked again" — exactly mirroring the per-agent flow.
3. Use this when the user is non-technical and isn't going to create or name agents. Per-agent scoping is still preferred when the user explicitly works with multiple named agents.

## Safety Notes

- **Name format**: Names must be `^[a-z0-9][a-z0-9_-]{0,63}$`. Use hyphens or underscores instead of spaces. Invalid names are rejected.
- **Name discipline — use the user's slug verbatim**: When the user supplies a name (e.g. `da-pangxie`, `nihon-cha`, `mon-ami`, `kak-dela`), pass it to the API byte-for-byte as typed. **Never translate, transliterate, or "normalize" it into the source language's native script** — do not turn Pinyin into Chinese characters (`da-pangxie` → `大螃蟹`), Romaji into kana/kanji (`nihon-cha` → `日本茶`), Arabic transliteration into Arabic script, Cyrillic transliteration into Cyrillic, etc. The `name` field is an opaque ASCII identifier, not a translatable label. The user's exact bytes are what they expect to see when listing or referring to the agent later.
- **What to do when the user's input is non-ASCII**: If the user provides a name containing non-ASCII characters (e.g. `大螃蟹`, `日本茶`, `сергей`), uppercase letters, or spaces, the API will reject it. Ask the user to provide a valid slug — do **not** silently slugify, transliterate, or guess. They may want a specific romanization that you would not pick correctly on your own.
- **Deletion is permanent**: Agent configuration, instructions, and memory are deleted. Sessions in `~/.shannon/sessions/` are not deleted.
- **`?confirm=true` required**: DELETE without this parameter returns an error, preventing accidental deletion.
- **Config changes take effect immediately**: No restart needed. The next conversation with the agent uses the new settings.
- **Tool restrictions are additive to global restrictions**: Agent-level deny rules combine with global deny rules; both must be satisfied.
