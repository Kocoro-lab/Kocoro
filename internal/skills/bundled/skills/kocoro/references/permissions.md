# Permissions

## What is this?

Permissions control what commands and tools agents are allowed to run. Before executing anything, Shannon checks a chain of rules: hard-blocked commands are always rejected, then denied commands, then allowed commands, then a set of safe defaults, and finally anything unrecognized requires explicit approval from the user. This protects against agents accidentally running destructive or sensitive commands.

**Evaluation order:**
1. **Hard-blocks** — always denied, cannot be overridden (e.g., `rm -rf /`)
2. **Denied commands** — configured blocklist, never run
3. **High-risk prefixes (always-ask)** — arbitrary-code-execution gateways like `python -c`, `node -e`, `bash -c`, `agent-browser eval`, supply-chain installers (`pip install` / `pip3 install`, `npm install` / `npm i`, `npx`, `pnpm install` / `pnpm i` / `pnpm add`, `yarn add`, `cargo install`, `gem install`, `go install`, `brew install`), destructive git push variants (`git push` with any of `--force`, `-f`, `--force-with-lease`, `--force-if-includes`, `--mirror`, `--delete`, `-d`, `--prune`, `--prune-tags`, anywhere in the args), `rm -rf`, trailing `&` background launches, and bare `&` separators (e.g. `cmd1 & cmd2` backgrounds `cmd1`) always re-prompt regardless of allowed_commands or always_allow_tools. Subshell groupings `(cmd)` are recursively unwrapped so a high-risk inner command is still flagged. `python -m pytest` / `python -m http.server` / `python -m json.tool` / `python -m py_compile` / `python -m venv` are exempt from the `python -m` rule. Selecting "Always Allow" on these in the UI permits the current invocation but does NOT persist them to config.
4. **`always_allow_tools` (tool-level approval bypass)** — when the tool name is in the global `permissions.always_allow_tools` list OR (for routed sessions) the agent's per-agent `always_allow_tools`, the approval prompt is skipped entirely. For `bash` this is a one-click "trust this agent's shell" toggle that still respects the always-ask gate above. Persistence is gated by `agent.DisallowsAutoApproval` (currently empty as of 2026-05-18); the plumbing is preserved for a future tool that genuinely cannot be persisted. `publish_to_web`, `generate_image`, and `edit_image` used to be on this list but were moved off — they are now ordinary approval-required tools that can be added here. **Unattended exception**: on unattended runs (schedule/cron, heartbeat, watcher, mcp), tools in `agent.DisallowsUnattendedAutoApproval` (`computer_use` as of 2026-07-22) ignore this bypass AND their approval-free observation exemption — every call (even read-only actions like `screenshot`) falls through to the run's handler, which refuses it via the same deny-list. Attended runs honor the bypass and observation exemption normally.
5. **Allowed commands** — configured allowlist. Two match modes: (a) literal/glob pattern match against the full command, (b) token-prefix match — if the command and a stored entry share the same first N non-flag tokens (after stripping redirects and skipping default-safe segments), the command is allowed. N=2 for known CLIs (git, kubectl, docker, npm, ptengine-cli, agent-browser, …), N=3 for unknown executables. So a single `ptengine-cli config get` entry covers `ptengine-cli config show --json`, `ptengine-cli config list`, etc., but not `ptengine-cli heatmap query`.
6. **Default safe** — commands Shannon considers low-risk by default (read-only `git`, `ls`, `cat`, `cd`, `go test`, …)
7. **Ask user** — anything else requires approval before running

## API Endpoints

### View current permission config
- Method: GET
- Path: /config
- Response includes: `{"permissions": {"allowed_commands": ["git *", "npm test"], "denied_commands": ["rm -rf *"]}}`
- Notes: Use this to see the full permission configuration.

### Add allowed commands
- Method: PATCH
- Path: /config
- Body: `{"permissions": {"allowed_commands": ["git *", "npm run *"]}}`
- Response: `{"status": "updated"}`
- Notes: List is merged (deduplicated), not replaced. Patterns support `*` as a wildcard.

### Add tool to global "always allow" list
- Method: POST
- Path: /permissions/always-allow
- Body: `{"tool": "bash"}`
- Response: `{"status": "added"}` on success; `400` if the tool is in `agent.DisallowsAutoApproval` (currently empty — no production tools are refused, but a future addition would land here).
- Notes: Appends to `permissions.always_allow_tools` in `~/.shannon/config.yaml`. Applies to EVERY agent including the default agent. Distinct from `allowed_commands`: this is a TOOL-level approval bypass (skip prompting for any call of the tool), while `allowed_commands` is COMMAND-string-level (only matches specific bash invocations). Also written automatically when the user clicks "Always Allow" on an approval prompt without a named agent (e.g. Default agent). High-risk bash commands (`pip install`, `rm -rf`, `python -c`, `git push --force`, etc.) still re-prompt every call even if `bash` is in this list — the always-ask gate runs before the always-allow check.

### Remove tool from global "always allow" list
- Method: DELETE
- Path: /permissions/always-allow
- Body: `{"tool": "bash"}`
- Response: `{"status": "removed"}`. No-op (200) if absent.

### Check macOS system permissions (TCC)
- Method: GET
- Path: /permissions
- Response: `{"screen_recording": "granted", "accessibility": "granted", "camera": "denied", "microphone": "not_determined"}`
- Notes: Shows macOS system-level permissions for Shannon (screen recording, accessibility, etc.).

### Request macOS system permissions
- Method: POST
- Path: /permissions/request
- Body: `{"permission": "screen_recording"}`
- Response: `{"permission": "string", "status": "granted" | "prompted" | "requires_settings" | "unknown" | "unsupported", "message": "string"}`
- Notes: `granted` means the permission is already held. `prompted` (accessibility, automation) means a macOS consent dialog was triggered. `requires_settings` (screen_recording) means System Settings was opened at the Screen Recording pane and the user must enable "Kocoro AX" there manually — usually because the one-shot consent dialog was already used; on the very first request a consent dialog may also be on screen, so tell the user to use either. `unknown` means the helper could not be reached. `unsupported` means non-macOS. Relay `message` to the user; do NOT retry the request in a loop (each call may re-open System Settings).

## Common Scenarios

### "Allow git commands"
1. PATCH /config with `{"permissions": {"allowed_commands": ["git *"]}}`
2. Agents can now run any `git` command without asking for approval.
3. To be more specific: `"git status"`, `"git log *"`, `"git diff *"` (only those exact patterns)

### "Allow npm scripts"
1. PATCH /config with `{"permissions": {"allowed_commands": ["npm run *", "npm test"]}}`

### "Check if screen recording is enabled (for screenshot tool)"
1. GET /permissions → look at `screen_recording` field
2. If `"denied"` or `"not_determined"`: POST /permissions/request with `{"permission": "screen_recording"}`
3. If the response status is `"prompted"`, a macOS dialog appeared — tell the user to click Allow. If it is `"requires_settings"`, System Settings was opened — tell the user to enable "Kocoro AX" under Privacy & Security → Screen Recording. Do not retry in a loop.

### "See what commands are currently blocked"
1. GET /config → look at `permissions.denied_commands`

### "Check why an agent can't run a command"
1. GET /config → review `permissions.allowed_commands` and `permissions.denied_commands`
2. The command may need to be added to `allowed_commands`, or it may be in `denied_commands` (which cannot be removed via API).

## Safety Notes

- **NEVER modify `denied_commands`**: The `denied_commands` list is a security boundary. It is intentionally not modifiable through the standard API. If you need a command that's in the denied list, reconsider the approach — these blocks exist for good reason.
- **Allowed commands widen the attack surface**: Every command you add to `allowed_commands` is one the agent can run without confirmation. Use specific patterns (`git status`) rather than broad ones (`*`) where possible.
- **Wildcard patterns**: `*` matches any text. `git *` allows all git subcommands. `npm *` allows all npm commands including `npm publish` — be specific.
- **Token-prefix family matching is implicit**: Even without `*`, an entry like `ptengine-cli config get` automatically covers `ptengine-cli config show`, `ptengine-cli config list`, etc. — same exec, same first N tokens. Different sub-commands of the same exec (e.g. `git status` vs `git push`) do NOT auto-match. Use this to keep `allowed_commands` short.
- **High-risk always re-prompts**: `python -c`, `pip install`, `agent-browser eval`, any `git push` with destructive flags, `rm -rf`, bare `&` background separators, and similar gateways always trigger an approval dialog, even if listed in `allowed_commands` OR if `bash` is in `always_allow_tools`. The high-risk gate runs BEFORE both the allowlist and the tool-level bypass, so adding the command to `allowed_commands` has zero effect — "Always Allow" is honored only for the current run and not saved. The only way to make a high-risk command run silently is to remove its prefix (or dangerous-flag entry) from the high-risk list in `internal/permissions/permissions.go` — a code change, by design (intentional friction).
- **Global vs per-agent always-allow scope**: `permissions.always_allow_tools` exists at both the global level (`~/.shannon/config.yaml`, applies to every agent including the default) and per-agent level (`~/.shannon/agents/<name>/config.yaml`, narrows trust to one agent). The runtime unions both sets when an agent runs; a tool listed in either is bypassed. Endpoints: `POST/DELETE /permissions/always-allow` (global), `POST/DELETE /agents/{name}/permissions/always-allow` (per-agent). The same `agent.DisallowsAutoApproval` gate applies to both scopes — currently empty, so no tool is refused at write-time, but the plumbing stays in place for a future addition.
- **Reload needed**: After changing permissions via PATCH /config, call POST /config/reload to ensure the daemon picks up the new settings.
- **macOS permissions are system-level**: Screen recording, accessibility, and microphone permissions are managed by macOS and can only be granted by the user via system dialogs. Shannon cannot grant these programmatically.
