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
- Notes: Supports `cwd`, `agent.model`, `agent.temperature`, `tools.allow`, `tools.deny`, `mcp_servers`.

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

### Reset agent session history (in place)
- Method: POST
- Path: /sessions/{id}/reset?agent={name}
- Response: `{"status": "reset", "id": "..."}`
- Notes: Clears the session's conversation history while keeping the session ID, title, CWD, source, channel, and cumulative usage. Cancels any active run on that session first. Also clears any persisted route binding (the link from a messaging-platform thread/sender to this session) and the live in-memory binding, so the next inbound message on that route starts a fresh session. The `agent` query parameter is REQUIRED — default-agent sessions do not use this endpoint; delete and recreate them via `DELETE /sessions/{id}` instead. Use when the user says "reset", "clear history", or "start over" on a named agent whose routing identity must survive the wipe.

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

## Safety Notes

- **Name format**: Names must be `^[a-z0-9][a-z0-9_-]{0,63}$`. Use hyphens or underscores instead of spaces. Invalid names are rejected.
- **Name discipline — use the user's slug verbatim**: When the user supplies a name (e.g. `da-pangxie`, `nihon-cha`, `mon-ami`, `kak-dela`), pass it to the API byte-for-byte as typed. **Never translate, transliterate, or "normalize" it into the source language's native script** — do not turn Pinyin into Chinese characters (`da-pangxie` → `大螃蟹`), Romaji into kana/kanji (`nihon-cha` → `日本茶`), Arabic transliteration into Arabic script, Cyrillic transliteration into Cyrillic, etc. The `name` field is an opaque ASCII identifier, not a translatable label. The user's exact bytes are what they expect to see when listing or referring to the agent later.
- **What to do when the user's input is non-ASCII**: If the user provides a name containing non-ASCII characters (e.g. `大螃蟹`, `日本茶`, `сергей`), uppercase letters, or spaces, the API will reject it. Ask the user to provide a valid slug — do **not** silently slugify, transliterate, or guess. They may want a specific romanization that you would not pick correctly on your own.
- **Deletion is permanent**: Agent configuration, instructions, and memory are deleted. Sessions in `~/.shannon/sessions/` are not deleted.
- **`?confirm=true` required**: DELETE without this parameter returns an error, preventing accidental deletion.
- **Config changes take effect immediately**: No restart needed. The next conversation with the agent uses the new settings.
- **Tool restrictions are additive to global restrictions**: Agent-level deny rules combine with global deny rules; both must be satisfied.
