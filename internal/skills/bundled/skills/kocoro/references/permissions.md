# Permissions

## What is this?

Permissions control what commands and tools agents are allowed to run. Before executing anything, Shannon checks a chain of rules: hard-blocked commands are always rejected, then denied commands, then allowed commands, then a set of safe defaults, and finally anything unrecognized requires explicit approval from the user. This protects against agents accidentally running destructive or sensitive commands.

**Evaluation order:**
1. **Hard-blocks** — always denied, cannot be overridden (e.g., `rm -rf /`)
2. **Denied commands** — configured blocklist, never run
3. **High-risk prefixes (always-ask)** — arbitrary-code-execution gateways like `python -c`, `node -e`, `bash -c`, `agent-browser eval`, supply-chain installers (`pip install` / `pip3 install`, `npm install` / `npm i`, `npx`, `pnpm install` / `pnpm i` / `pnpm add`, `yarn add`, `cargo install`, `gem install`, `go install`, `brew install`), destructive git push variants (`git push` with any of `--force`, `-f`, `--force-with-lease`, `--force-if-includes`, `--mirror`, `--delete`, `-d`, `--prune`, `--prune-tags`, anywhere in the args), `rm -rf`, trailing `&` background launches, and bare `&` separators (e.g. `cmd1 & cmd2` backgrounds `cmd1`) always re-prompt regardless of allowed_commands. Subshell groupings `(cmd)` are recursively unwrapped so a high-risk inner command is still flagged. `python -m pytest` / `python -m http.server` / `python -m json.tool` / `python -m py_compile` / `python -m venv` are exempt from the `python -m` rule. Selecting "Always Allow" on these in the UI permits the current invocation but does NOT persist them to config.
4. **Allowed commands** — configured allowlist. Two match modes: (a) literal/glob pattern match against the full command, (b) token-prefix match — if the command and a stored entry share the same first N non-flag tokens (after stripping redirects and skipping default-safe segments), the command is allowed. N=2 for known CLIs (git, kubectl, docker, npm, ptengine-cli, agent-browser, …), N=3 for unknown executables. So a single `ptengine-cli config get` entry covers `ptengine-cli config show --json`, `ptengine-cli config list`, etc., but not `ptengine-cli heatmap query`.
5. **Default safe** — commands Shannon considers low-risk by default (read-only `git`, `ls`, `cat`, `cd`, `go test`, …)
6. **Ask user** — anything else requires approval before running

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

### Check macOS system permissions (TCC)
- Method: GET
- Path: /permissions
- Response: `{"screen_recording": "granted", "accessibility": "granted", "camera": "denied", "microphone": "not_determined"}`
- Notes: Shows macOS system-level permissions for Shannon (screen recording, accessibility, etc.).

### Request macOS system permissions
- Method: POST
- Path: /permissions/request
- Body: `{"permission": "screen_recording"}`
- Response: `{"permission": "string", "status": "requested"}`
- Notes: Triggers the macOS system permission dialog for the user to approve.

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
3. A macOS dialog will appear — click Allow.

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
- **High-risk always re-prompts**: `python -c`, `pip install`, `agent-browser eval`, any `git push` with destructive flags, `rm -rf`, bare `&` background separators, and similar gateways always trigger an approval dialog, even if listed in `allowed_commands`. The high-risk gate runs BEFORE the allowlist is consulted, so adding the command to `allowed_commands` has zero effect — "Always Allow" is honored only for the current run and not saved. The only way to make a high-risk command run silently is to remove its prefix (or dangerous-flag entry) from the high-risk list in `internal/permissions/permissions.go` — a code change, by design (intentional friction).
- **Reload needed**: After changing permissions via PATCH /config, call POST /config/reload to ensure the daemon picks up the new settings.
- **macOS permissions are system-level**: Screen recording, accessibility, and microphone permissions are managed by macOS and can only be granted by the user via system dialogs. Shannon cannot grant these programmatically.
