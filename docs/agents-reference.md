# Named Agents Reference

Full reference for named-agent `config.yaml`. The README covers what an agent is and how to create one — this file documents every config option.

## Layout

```
~/.shannon/agents/
  ops-bot/
    AGENT.md          # agent instructions (replaces default system prompt)
    MEMORY.md         # agent-specific memory (persists across sessions)
    HEARTBEAT.md      # optional: heartbeat checklist
    config.yaml       # optional: tool filtering, MCP scoping, model overrides
    commands/         # optional: agent-scoped slash commands (*.md)
    _attached.yaml    # optional: attached installed skill names
    sessions/         # auto-created — agent-scoped session JSON files
```

An agent without `config.yaml` inherits all tools, global MCP servers, and default model settings.

## Full config.yaml

```yaml
# Default project directory for this agent. Must be an absolute path.
# Drives: prompt context, instructions, file tools, bash, auto-approval
# scope, read-before-edit tracking. Request-level cwd and resumed sessions
# can override this default.
cwd: /Users/you/Code/myproject

# Tool filtering — see "Tool filtering" below for allow/deny semantics
tools:
  allow:
    - file_read
    - grep
    - glob
    - bash
    - think
    - directory_list

# Per-agent MCP servers
# _inherit: false → only these servers (ignore global config)
# _inherit: true  → merge on top of global servers
mcp_servers:
  _inherit: false
  filesystem:
    command: "npx"
    args: ["-y", "@modelcontextprotocol/server-filesystem", "/Users/you/Desktop"]

# Model and behavior overrides (subset of global agent.* keys)
agent:
  model: "claude-sonnet-4-6"
  model_tier: "large"               # optional; small | medium | large. Omit to inherit global model_tier. `agent.model` (specific id) wins over `agent.model_tier` when both are set.
  max_iterations: 10
  temperature: 0.2
  max_tokens: 16000
  context_window: 64000             # per-agent value is a LOCK — bypasses model auto-detect. Use for cost caps or Ollama / custom-cap models.

# Per-agent permissions (merged onto global)
permissions:
  always_allow_tools: []            # tool names this agent skips approval for. See README "Daemon Mode" for safety gates.
  allowed_commands: []              # additional commands the agent can run without approval

# Per-agent auto-approve (daemon mode)
auto_approve: false                  # skip WS approval round-trip for this agent. Permission engine still enforced.

# File system watcher — trigger agent on file changes
watch:
  - path: ~/Code/myproject
    glob: "*.go"                    # optional; omit to watch all files
  - path: ~/Downloads
    glob: "*.csv"

# Heartbeat — periodic checklist runs from HEARTBEAT.md
heartbeat:
  every: 30m                        # Go duration (required if heartbeat section present)
  active_hours: "09:00-22:00"       # optional; supports overnight like "22:00-02:00"
  model: small                      # optional; cheaper model for routine checks
  isolated_session: true            # default true — fresh session per heartbeat
```

## Tool filtering

```yaml
# Allow list — only these tools available
tools:
  allow: [file_read, grep, glob, bash]

# OR deny list — all tools EXCEPT these
tools:
  deny: [computer, browser, screenshot, applescript]

# Omit `tools:` entirely → all tools available
```

The filter applies to all tool sources (local, MCP, gateway). If both `allow` and `deny` are set, `allow` takes precedence.

## Project Context (`cwd`) resolution

Effective `cwd` for a run is resolved in this order:

1. Request `cwd` (passed to `POST /message`)
2. Stored session `cwd` for resumed sessions
3. Agent config `cwd`
4. Process working directory fallback

This means: a request can target a project, resumed sessions return to the same project, and agents can define a default.

### What `cwd` affects

- Relative file paths
- Project instructions (`.shannon/instructions.md`, `.shannon/rules/*.md`)
- Bash execution directory
- Safe path checks (allowed_dirs scope)
- Read-before-edit tracking
- Project-local runtime config

### Project-local config scope

When a session has a `cwd`, project-local `config.yaml` is loaded but only for session-safe fields:

- `model_tier`
- `agent.*`
- `tools.*`
- `permissions.*`

Process-global settings remain global and are NOT overridden by project-local config:

- `endpoint`
- `api_key`
- `mcp_servers`
- `daemon.*`
- `auto_update_check`

## Commands

Create `*.md` files in `commands/` for agent-scoped slash commands:

```bash
mkdir -p ~/.shannon/agents/reviewer/commands
cat > ~/.shannon/agents/reviewer/commands/review.md << 'EOF'
Review the code in $ARGUMENTS for:
- Correctness and logic errors
- Security vulnerabilities
- Performance issues
Focus on bugs that matter, skip nitpicks.
EOF
```

Use in TUI: `/review src/auth/login.go`. `$ARGUMENTS` is replaced with everything after the command name. Agent commands cannot overwrite built-in commands.

## Attached Skills (`_attached.yaml`)

Skills use the [Anthropic SKILL.md spec](https://agentskills.io/specification). Install skills globally under `~/.shannon/skills/<name>/SKILL.md`, then attach them:

```yaml
# ~/.shannon/agents/reviewer/_attached.yaml
- summarize
- security-review
```

Attached skill names are resolved from `~/.shannon/skills/`. Attached skills appear as `/<skill>` slash commands in the TUI.

### Builtin skills

Two auto-installed builtins sync from the binary on every daemon/TUI/CLI startup: `kocoro` (platform / HTTP API configuration assistant) and `kocoro-generative-ui` (inline visualization assistant for Kocoro Desktop's sandboxed `html-artifact` widgets — hidden from listings, still callable via `use_skill`). Editing their on-disk SKILL.md is overwritten on next startup; fork under a different name to customize.

### Skill secrets

Skills that need API keys declare required env vars in their ClawHub metadata. Values live in the macOS Keychain — never on disk, never in the prompt or session transcript — and are scoped to active skills only (a skill installed but never activated via `use_skill` contributes no env vars to `bash`).

Manage keys over the daemon API:
- `PUT /skills/{name}/secrets` — set
- `DELETE /skills/{name}/secrets` — remove
- `GET /skills` — returns `required_secrets` and `configured_secrets` (names only)

### Installing skills from a local ZIP

`POST /skills/upload` (multipart, 50 MB cap) accepts a direct skill ZIP — useful before a skill is published to ClawHub.

- Unwraps GitHub/Finder single-top-level-dir layout
- Strips `__MACOSX`
- Inherits the marketplace extractor's zipbomb / symlink / path-escape guards
- Serializes concurrent uploads of the same slug
- On slug collision: returns 409 with a side-by-side compare body (existing vs. uploaded name/description/prompt, capped at 8 KB) so Kocoro Desktop renders a compare sheet; re-issue with `?force=true` to overwrite (crash-safe rename-to-backup + atomic rename, backup removed on success)
- Uploads targeting the `kocoro` / `kocoro-generative-ui` builtins are rejected unconditionally — the daemon re-overlays builtins from the binary on every startup, so an upload there would be reverted.

## Name rules

Names must match `^[a-z0-9][a-z0-9_-]{0,63}$` — lowercase alphanumeric, hyphens, underscores. Validated before any path concatenation to prevent traversal.
