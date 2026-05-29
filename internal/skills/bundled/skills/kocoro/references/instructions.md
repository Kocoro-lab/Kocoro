# Instructions and Rules

## What is this?

Instructions and rules shape how agents behave — their tone, language, formatting preferences, and what they should or shouldn't do. There are five levels that combine together: global instructions, global rules, project instructions, project rules, and local instructions. Lower levels can add to or override higher ones, letting you have general behavior globally and specific behavior per project.

**Instructions** are free-form guidance (like a style guide or persona description). **Rules** are named, discrete policies that can be individually managed (like "never push to main without asking" or "never suggest deleting files"). To lock the reply language specifically, prefer the dedicated `agent.language` config over a rule — see config.md and the "Make agents respond in Japanese" scenario below for why.

## API Endpoints

### Get global instructions
- Method: GET
- Path: /instructions
- Response: `{"content": "# Global Instructions\n\nYou are..."}`

### Update global instructions
- Method: PUT
- Path: /instructions
- Body (JSON, default): `{"content": "# Global Instructions\n\nAlways respond concisely..."}`
- Body (raw markdown — preferred for long content): send the markdown verbatim with header `Content-Type: text/markdown` (or `text/plain`). No JSON wrapper, no escaping. With the http tool: `body_from_file: /path/to/source.md` plus `headers: {"Content-Type": "text/markdown"}`.
- Response: `{"status": "updated"}`
- Notes: Replaces the entire global instructions file. Include all content you want to keep. Use the raw-markdown path when the content has quotes, backslashes, backticks, or newlines — JSON-string-escaping a long markdown blob by hand is the #1 source of 400 errors here.
- Delete asymmetry: only the JSON path can delete. `{"content": null}` removes the file; the raw-markdown path is set-only, and a zero-length raw body writes a zero-byte file rather than deleting it.

### List all rules
- Method: GET
- Path: /rules
- Response: `{"rules": [{"name": "no-force-push", "content": "Never force-push to main without explicit confirmation."}]}`

### Get a rule
- Method: GET
- Path: /rules/{name}
- Response: `{"name": "...", "content": "..."}`

### Create or update a rule
- Method: PUT
- Path: /rules/{name}
- Body: `{"content": "Always format code examples with language tags."}`
- Response: `{"status": "updated", "name": "..."}`
- Notes: Creates the rule if it doesn't exist, updates it if it does.

### Delete a rule
- Method: DELETE
- Path: /rules/{name}?confirm=true
- Response: `{"status": "deleted"}`
- Notes: `?confirm=true` required to prevent accidental deletion.

## Common Scenarios

### "Make agents respond in Japanese"
Use the dedicated `agent.language` config, NOT a rule. A rule lands in the
instructions block (a system-prompt prefix) and is overridden by the per-turn
language directive at the end of the user message, so a "respond in Japanese"
rule does not reliably hold. `agent.language` is the authoritative reply-language
lock (it drives that per-turn directive).
- One agent: GET /agents/{name}, merge `"agent": {"language": "日本語"}` into the
  returned config, then PUT /agents/{name}/config with the full merged body (PUT
  replaces the whole config — see agents.md).
- All agents (global default): PATCH /config with `{"agent": {"language": "日本語"}}`,
  then POST /config/reload.

### "Add a code style rule"
1. PUT /rules/code-style with `{"content": "When writing code examples: use 2-space indentation, include type annotations in TypeScript, add brief comments for non-obvious logic."}`

### "List current rules to see what's active"
1. GET /rules → review the list

### "Temporarily disable a rule without deleting it"
Rules cannot be toggled — to disable, either:
- DELETE /rules/{name}?confirm=true (removes it permanently from that level)
- Or move logic to a named rule that you can selectively delete later

### "Add a project-specific instruction"
Instructions are per-project when set in `.shannon/instructions.md`. To add project-level instructions via the daemon API, use the project-level endpoint if available — or edit `.shannon/instructions.md` directly in the project and call POST /config/reload.

## Safety Notes

- **Changes take effect immediately**: The next conversation the agent has will follow the new instructions. Currently in-progress conversations may not pick up the change until the next turn.
- **PUT replaces entirely**: PUT /instructions replaces the whole file. Always GET first to see current content before updating.
- **Instruction conflicts**: If global and project instructions conflict, project instructions take precedence. Rules from all levels are concatenated, not replaced.
- **Rule naming**: Use descriptive, kebab-case names (e.g., `respond-in-japanese`, `no-file-deletion`). Consider numeric prefixes for ordering if evaluation order matters (e.g., `01-language`, `02-tone`).
- **Avoid vague rules**: Specific rules are more reliably followed. "Be professional" is vague; "Do not use slang or emojis in responses" is specific.
