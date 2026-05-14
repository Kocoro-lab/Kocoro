# Skills

## What is this?

Skills are knowledge packages that teach agents specific abilities — like reading PDFs, writing presentations, or analyzing spreadsheets. A skill contains instructions, example workflows, and sometimes custom tools that the agent can use. You install a skill once, then attach it to any agent that needs it.

## API Endpoints

### List installed skills
- Method: GET
- Path: /skills
- Query: `?include_hidden=true` (optional) — include skills with frontmatter `hidden: true`
- Response: `{"skills": [{"name": "...", "slug": "...", "description": "...", "source": "global", "install_source": "...", "hidden": true, "required_secrets": [...], "configured_secrets": [...]}]}`
- Notes: Shows all skills currently installed in your Shannon instance. Use `slug` (the on-disk / URL-safe identifier) for all subsequent CRUD calls; `name` is a free-form display label that may contain uppercase letters or CJK characters and is not guaranteed to match the slug.
- Hidden skills: By default the response omits skills whose SKILL.md frontmatter sets `hidden: true` (e.g. `kocoro` itself) — this is a display-only filter so user-facing frontends hide internal/policy skills. The skill is still loaded, still invokable via `use_skill`, and still participates in skill discovery. Pass `?include_hidden=true` to see them (e.g. for an admin/management UI that needs to manage their secrets or config).

### List downloadable skills (bundled)
- Method: GET
- Path: /skills/downloadable
- Response: `{"skills": [{"name": "...", "description": "...", "installed": false}]}`
- Notes: Skills that ship with Shannon and can be installed with one call.

### List marketplace skills
- Method: GET
- Path: /skills/marketplace
- Response: `[{"slug": "string", "name": "string", "description": "string", "author": "string"}]`
- Notes: Community-contributed skills from the Shannon marketplace.

### Install a bundled skill
- Method: POST
- Path: /skills/install/{name}
- Response: `{"name": "...", "slug": "...", "description": "...", "install_source": "..."}`
- Notes: Installs from bundled (downloadable) skills. The `{name}` path segment is the skill's slug (always lowercase + hyphens).

### Install a marketplace skill
- Method: POST
- Path: /skills/marketplace/install/{slug}
- Response: `{"slug": "string", "name": "string", "description": "string", "install_source": "marketplace"}`
- Notes: Downloads and installs from the marketplace. Use the slug from GET /skills/marketplace. The response `name` is the frontmatter display label (may differ from the slug, e.g. slug `xiaohongshu-mcp-skills` with name `xiaohongshu`).

### Upload a skill from a ZIP file
- Method: POST
- Path: /skills/upload?force=true
- Body: `multipart/form-data`; field `file` = ZIP payload (max 50 MB compressed, 200 MB uncompressed)
- Response 201: `{"name": "...", "slug": "...", "description": "...", "install_source": "local"}`
- Response 409: `{"error": "skill_already_exists", "existing_name": "...", "existing_description": "...", "existing_prompt": "...", "new_description": "...", "new_prompt": "..."}`
- Response 403: `{"error": "skill_is_builtin"}` — returned when the ZIP targets an auto-installed builtin (`kocoro`, `kocoro-generative-ui`). `force=true` does NOT override this; the builtin guard is unconditional because `EnsureBuiltinSkills` would wipe any override on the next daemon restart.
- Response 400: `{"error": "invalid multipart form: ..."}` — returned when the multipart body is malformed or the `file` field is missing. Distinct from 413 (which means body exceeded 50 MB).
- Response 413: zip exceeds 50 MB compressed or 200 MB uncompressed
- Response 422: invalid skill payload (missing SKILL.md, malformed frontmatter, invalid name)
- Notes: GitHub/Finder-style ZIPs (single top-level directory) are auto-unwrapped, and `__MACOSX` metadata directories are ignored. The slug is derived from the SKILL.md frontmatter `name` field. Use `force=true` to overwrite an existing user-installed skill (does not apply to builtins). The 409 body includes both existing and new descriptions/prompts so the frontend can render a side-by-side compare sheet. `install_source` is set to `local` to distinguish uploads from `bundled` / `marketplace` installs.

### Create or update a custom skill
- Method: PUT
- Path: /skills/{slug}?force=true
- Body: `{"description": "...", "prompt": "# My Skill\n\n..."}`
- Response 200: `{"status": "updated"}`
- Response 409: `{"error": "skill_already_exists", "existing_name": "...", "existing_description": "...", "existing_prompt": "...", "new_description": "...", "new_prompt": "..."}` — same shape as POST /skills/upload so frontends can reuse one compare sheet.
- Response 403: `{"error": "skill_is_builtin"}` — `{slug}` is an auto-installed builtin (`kocoro`, `kocoro-generative-ui`). `force=true` does NOT override this; the guard is unconditional because `EnsureBuiltinSkills` would wipe any override on the next daemon restart.
- Response 422: existing on-disk SKILL.md has malformed frontmatter; fix or delete the directory before retrying.
- Response 400: missing `description` or `prompt` in the body.
- Response 503: cannot resolve skill sources or LoadSkills failed while an existing skill is on disk — refused to clobber AllowedTools/Metadata. Transient; retry.
- Notes: PUT is the single endpoint for both **create new** and **update existing**. Without `force=true`, the daemon refuses to overwrite an existing slug and returns 409 with both sides' description+prompt (each capped at ~8 KB with a `[truncated]` marker — fetch GET /skills/{slug} for the full body). Two-step flow for new skills: send PUT first; on 409, prompt the user with a compare sheet; on confirm, retry with `?force=true`. One-step flow for the edit-existing-skill UI: pass `force=true` unconditionally because edit implies overwrite. The `{slug}` path segment is the directory identifier (from GET /skills). Bundled skills should be reinstalled rather than edited. The existing frontmatter `name` (display label) is preserved on update; supply a fresh name via the payload only when renaming intentionally. AllowedTools/Metadata/Compatibility from the existing SKILL.md are preserved across the write — the PUT body cannot drop them.

### Delete a skill
- Method: DELETE
- Path: /skills/{slug}?confirm=true
- Response: `{"status": "deleted"}`
- Notes: DESTRUCTIVE. The `{slug}` path segment is the directory identifier. Automatically detaches from all agents that use it and clears any stored API keys from the OS keychain.

### Set skill secrets (API keys / env vars)
- Method: PUT
- Path: /skills/{slug}/secrets
- Body: `{"KEY_NAME": "value", "ANOTHER_KEY": "value"}` (flat map, one entry per env var)
- Response: `{"status": "updated"}`
- Notes: This is the ONLY correct way to configure API keys for a skill. Values are written to the macOS Keychain (service `com.shannon.skill.<slug>`, account = env var name). Key names must match `[A-Z0-9_]+` (uppercase letters, digits, underscore). Values are never written to `~/.shannon/config.yaml`, `.env`, agent config, or any other file; never returned by any GET endpoint; never appear in session transcripts or audit logs. At runtime they are injected as env vars into the `bash` tool's child process ONLY when the skill is activated via `use_skill` in the current turn — a loaded-but-inactive skill contributes nothing.

### Clear all secrets for a skill
- Method: DELETE
- Path: /skills/{slug}/secrets
- Response: `{"status": "deleted"}`
- Notes: Removes every stored value for this skill from the Keychain. The skill itself stays installed.

### Delete one secret key
- Method: DELETE
- Path: /skills/{slug}/secrets/{key}
- Response: `{"status": "deleted"}`
- Notes: Removes a single env var by name, leaving other keys intact. `{key}` is the env var name (e.g., `FAL_KEY`).

## Common Scenarios

### "What skills are available to install?"
1. GET /skills/downloadable — see bundled skills included with Shannon
2. GET /skills/marketplace — see community skills

### "Install the PDF skill"
1. GET /skills/downloadable — find the PDF skill name (e.g., `pdf`)
2. POST /skills/install/pdf
3. PUT /agents/{agent-name}/skills/pdf — attach to the agent that needs it

### "Give an agent presentation ability"
1. GET /skills/downloadable — find presentation skill (e.g., `pptx` or `slidev`)
2. POST /skills/install/pptx
3. PUT /agents/my-agent/skills/pptx
4. Verify: GET /agents/my-agent — check `skills` array includes `pptx`

### "Remove a skill from one agent but keep it for others"
1. DELETE /agents/{agent-name}/skills/{skill-name} — detaches from that agent only
2. Skill remains installed and attached to other agents.

### "Completely remove a skill"
1. DELETE /skills/{skill-name}?confirm=true — removes skill and detaches from ALL agents

### "Configure API keys for a skill" (e.g., image-gen, figma)
Some skills need API keys to call external services. These are declared by the skill and fetched at runtime from the OS keychain — NEVER edit `.env` or agent config to set them.
1. GET /skills — find the target skill and read its `required_secrets` list (env var names it needs, e.g. `["FAL_KEY", "LEGNEXT_KEY"]`) and `configured_secrets` list (names already stored)
2. Tell the user which keys are needed and explain the values will be stored in the macOS Keychain (never in files, never echoed back by the API)
3. After the user provides values: PUT /skills/{slug}/secrets body: `{"FAL_KEY": "...", "LEGNEXT_KEY": "..."}`
4. Verify: GET /skills — check `configured_secrets` now includes the key names (values themselves are never returned)

### "Update or rotate one API key"
1. PUT /skills/{slug}/secrets body: `{"FAL_KEY": "new-value"}` — overwrites just that key, leaves other stored keys intact

### "Remove one API key" / "Remove all API keys for a skill"
- Single key: DELETE /skills/{slug}/secrets/{KEY_NAME}
- All keys: DELETE /skills/{slug}/secrets

## Safety Notes

- **Deletion detaches from all agents**: If you delete a skill, every agent that uses it immediately loses that ability. Prefer detaching from a specific agent (DELETE /agents/{name}/skills/{skill}) if you only want to remove it from one.
- **Marketplace skills**: Review the skill description before installing — marketplace skills are community-contributed and not officially vetted.
- **Custom skill edits**: Use PUT /skills/{name} only for skills you own. Editing bundled skills may cause issues on updates. If you need to customize a bundled skill, copy its content first and create a new skill with a different name.
- **Skill secrets live in the OS keychain, not in files**: Never suggest editing `~/.shannon/config.yaml`, `.env`, or agent config to set a skill's API keys — that path doesn't work. Use `PUT /skills/{slug}/secrets` exclusively. This is different from MCP server env vars (`mcp.servers.*.env` in config.yaml) — MCP and skills use separate storage layers even though both look like "env vars" on the surface.
- **Never echo secret values back to the user or into logs**: Values are write-only via the API. Confirm success by checking `configured_secrets` (which lists key names only) after a PUT, not by trying to GET the value.
