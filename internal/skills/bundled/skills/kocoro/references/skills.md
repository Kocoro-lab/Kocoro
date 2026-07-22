# Skills

## What is this?

Skills are knowledge packages that teach agents specific abilities — like reading PDFs, writing presentations, or analyzing spreadsheets. A skill contains instructions, example workflows, and sometimes custom tools that the agent can use. You install a skill once, then attach it to any agent that needs it.

## API Endpoints

### List installed skills
- Method: GET
- Path: /skills
- Query: `?include_hidden=true` (optional) — include skills with frontmatter `hidden: true`
- Response: `{"skills": [{"name": "...", "slug": "...", "description": "...", "source": "global", "install_source": "...", "hidden": true, "required_secrets": [...], "configured_secrets": [...], "default_agent_disabled": false}]}`
- `default_agent_disabled`: `true` when the skill is in `config.skills.disabled` — i.e. the **default agent** will not load it (named agents are unaffected). Toggle via POST/DELETE /skills/disabled below. Requires capability `default_agent_skill_denylist`.
- Notes: Shows all skills currently installed in your Shannon instance. Prefer `slug` (the on-disk / URL-safe identifier) for all subsequent CRUD calls; `name` is a free-form display label that may contain uppercase letters or CJK characters and is not guaranteed to match the slug. The by-name skill endpoints (`GET/PUT/DELETE /skills/{name}`, `/{name}/usage`, `/{name}/secrets`, `/{name}/scripts|references|assets`) accept **either** the slug or the display `name` (canonicalized to the slug server-side) as a backward-compat alias, but slug is the canonical, unambiguous key.
- Hidden skills: By default the response omits skills whose SKILL.md frontmatter sets `hidden: true` (e.g. `kocoro` itself) — this is a display-only filter so user-facing frontends hide internal/policy skills. The skill is still loaded, still invokable via `use_skill`, and still participates in skill discovery. Pass `?include_hidden=true` to see them (e.g. for an admin/management UI that needs to manage their secrets or config).

### Disable / enable a skill for the default agent
- Method: POST (disable) / DELETE (enable)
- Path: /skills/disabled
- Body: one of — `{"skill": "<slug-or-name>"}` (single), `{"skills": ["a","b"]}` (batch), or `{"prefix": "longbridge-"}` (every installed skill whose name OR slug starts with the prefix). The forms compose and are deduped.
- Response: `{"status": "disabled", "count": N, "skills": [...]}` (POST) / `{"status": "removed", "count": N, "skills": [...]}` (DELETE)
- **Disabling/enabling MANY skills: use `prefix` or `skills[]` — NEVER loop single calls.** Going one-by-one over a large family (e.g. 100+ `longbridge-*`) is one HTTP call per skill, which bloats context and trips the loop detector; `{"prefix":"longbridge-"}` does the whole family in a single call + single config write. Same for DELETE (re-enable).
- Notes: Controls which installed skills the **default agent** loads. The default agent loads every installed skill by default; POST adds the skill to `config.skills.disabled` so it is no longer listed, discovered, or invokable via `use_skill` for the default agent, and DELETE re-enables it. Both are idempotent and persist to `~/.shannon/config.yaml`. This is **default-agent-only** — named agents select skills via their `_attached.yaml` allowlist (PUT/DELETE /agents/{name}/skills/{skill}) and are never narrowed by this list. GET /skills reflects per-skill state via `default_agent_disabled`. Requires the daemon capability token `default_agent_skill_denylist`.

### List downloadable skills (bundled)
- Method: GET
- Path: /skills/downloadable
- Response: `{"skills": [{"name": "...", "description": "...", "installed": false}]}`
- Notes: Skills that ship with Shannon and can be installed with one call.

### List marketplace skills
- Method: GET
- Path: /skills/marketplace
- Query: `?page=1&size=20&sort=downloads&q=<search>` (all optional; `sort` defaults to `downloads`)
- Response: `{"total": <int>, "page": <int>, "size": <int>, "skills": [{"slug": "...", "name": "...", "description": "...", "author": "...", "installed": <bool>, ...}]}`
- Notes: Community-contributed skills from the Shannon **static registry**. Integer page-based pagination. Sets `X-Cache-Stale: true` header when the cached index is being served stale. This is the contract the macOS Desktop consumes — see GET /skills/clawhub below for the separate ClawHub live-catalog API.

### Marketplace skill detail
- Method: GET
- Path: /skills/marketplace/entry/{slug}
- Query: `?owner=<handle>` (optional; only used by the ClawHub fallback below to disambiguate a shared slug)
- Response: `{"slug": "...", "name": "...", "description": "...", "author": "...", "homepage": "...", "installed": <bool>, "preview": "<SKILL.md body or empty>"}`
- Notes: Registry source first. The `{slug}` also accepts an installed skill's display `name` (canonicalized to its slug) for backward compat. Malicious registry entries return 403. `preview` is the on-disk SKILL.md when installed, else empty (always present in the schema). **Fallback:** if the slug is not in the registry index, the daemon falls back to ClawHub's live catalog (GET /skills/clawhub/entry below) so skills installed from ClawHub still resolve their detail; only 404 if neither has it.

## ClawHub live-catalog API (separate surface)

The `/skills/clawhub/*` endpoints are backed by ClawHub's live online catalog (~12k skills), a SEPARATE API from `/skills/marketplace/*`. They use opaque cursor pagination and expose per-version file browsing. Base URL is `skills.marketplace.clawhub_url` (default `https://clawhub.ai`). Install via either surface lands the skill on disk identically.

**Transient-failure handling:** browse/detail/files/file (and the static `/skills/marketplace/*` reads) retry transient upstream failures — HTTP 429/500/502/503/504 and network errors — with exponential backoff + jitter before surfacing an error, so an occasional ClawHub blip no longer fails the request. A `503`/`502` is returned only after retries are exhausted. Tunable via `skills.marketplace.max_attempts` (default 3) and `skills.marketplace.retry_base_backoff_secs` (default 1). 4xx (404/409/422) are not retried. The install zip download is a single attempt (no retry) — it uses a 2-minute timeout, so a failed install surfaces promptly as 502 and you simply retry the install.

**Caching:** ClawHub read responses (browse/search/detail/files/file) are cached for a short TTL (`skills.marketplace.clawhub_cache_ttl_secs`, default 60s) keyed by full URL, so repeated/burst browsing within the window doesn't re-hit clawhub.ai. On an upstream failure a still-cached body is served as stale (with a cooldown), the same way the static registry serves stale on error. A newly published or edited skill can therefore take up to the TTL to appear; set the TTL to 0 to disable caching.

**Default-view warm + fallback:** the daemon warms the canonical default browse page (`size=20&sort=downloads`, no cursor/query) **once at startup** (`skills.marketplace.clawhub_warm_on_startup`, default true — one-shot, not a poll loop; set false to skip). It also keeps a **view-agnostic "last known good" default page**: any successful no-query/no-cursor browse is retained (≤30 min) and served as a single stale page (empty `next_cursor`, response carries **`X-Cache-Stale: true`**) whenever a fresh default first-page fetch fails **for a transient reason** while its exact-URL cache is cold — so the default marketplace view no longer surfaces a transient 503 "registry unreachable" during a clawhub.ai blip. This covers the default view even when the client requests a `size`/`sort` the warm didn't prefetch (during active browsing the user's own requests keep the cache warm). **A definitive status is never masked** — only 429 + 5xx (and network/parse errors) fall back to the stale page; every other HTTP status (400/401/403/404/405/409/410/422/…) surfaces immediately. Deep pages (`cursor` set) and searches (`q` set) have no fallback and still propagate a 503; and if clawhub is hard-down before the first successful warm/browse, a default browse in that window still 503s.

**Ambiguous slugs:** ClawHub slugs are not unique across publishers (e.g. two `data-analysis` from different authors). The detail/files/file/install endpoints accept an optional `?owner=<handle>` query param (the entry's `author`) to pin a specific publisher. **Without `?owner=`, the daemon now auto-resolves a shared slug** to the publisher with the most downloads (deterministic; ties broken by the lexicographically-smallest handle) and, on install, audit-logs the auto-selected owner. Only when it genuinely cannot resolve (search returns no exact-slug match carrying an owner) does it return **409 `skill "…" is published by multiple owners; retry with ?owner=<handle>`** — an actionable client error, no longer a misleading 503/502. Pass `?owner=` explicitly when you already know which publisher you want (search results carry the `author`; browse-list items do not). The auto-pick is a popularity heuristic, so prefer an explicit `?owner=` for a security-sensitive install.

### Browse ClawHub
- Method: GET
- Path: /skills/clawhub
- Query: `?cursor=<opaque>&size=20&sort=downloads&q=<search>&exclude_installed=true` (all optional)
- Response: `{"skills": [{"slug": "...", "name": "...", "description": "...", "installed": <bool>, ...}], "size": <int>, "next_cursor": "<opaque or empty>"}`
- Notes: **No `total`/`page`** — page forward by passing the returned `next_cursor` back as `cursor`. Empty `next_cursor` means no more pages (also empty for `q=` searches, which return the full relevance-ranked result set in one response). 503 when ClawHub is unreachable.
- **`exclude_installed=true`** hides skills already installed locally (every returned item then has `installed:false`). "Installed" is a purely local notion clawhub.ai cannot filter on, so the daemon fetches normally then drops installed slugs, **refilling from subsequent upstream pages** (bounded by `skills.marketplace.clawhub_exclude_fill_max_pages`, default 5) so the page stays populated. Because ClawHub's cursor is page-granular, a returned page **may exceed `size`** (the last fetched page is included whole) and the `next_cursor` is always page-aligned — resuming from it never duplicates or drops an entry. If the fill cap binds before `size` non-installed items are found, the page comes back short (or, in the extreme, empty) with a non-empty `next_cursor` — **keep paging** rather than treating that as end-of-list. Gated by the `clawhub_exclude_installed` capability token (old daemons ignore the param and return the full list). Opt-in and additive: the default marketplace view (no param) is unchanged.

### ClawHub skill detail
- Method: GET
- Path: /skills/clawhub/entry/{slug}
- Query: `?owner=<handle>` (optional; disambiguates a slug shared by multiple publishers)
- Response: `{"slug": "...", "name": "...", "author": "...", "homepage": "...", "version": "...", "installed": <bool>, "preview": "<SKILL.md body>"}`
- Notes: Built live from ClawHub's detail endpoint (owner, homepage, stats, full SKILL.md preview). On-disk SKILL.md overlays the preview once installed. 404 if not on ClawHub, 503 if ClawHub is down.

### ClawHub skill file manifest
- Method: GET
- Path: /skills/clawhub/entry/{slug}/files
- Query: `?version=<ver>&owner=<handle>` (both optional; `version` resolves to latest when omitted, `owner` disambiguates a shared slug)
- Response: `{"version": "<resolved>", "files": [{"path": "...", "size": <int>, "content_type": "..."}]}`
- Notes: For rendering a file tree before install. 404 if slug/version not found.

### ClawHub skill file content
- Method: GET
- Path: /skills/clawhub/entry/{slug}/file
- Query: `?path=<file>&version=<ver>&owner=<handle>` (`path` required, ≤512 bytes; `version` optional → latest; `owner` optional, disambiguates a shared slug)
- Response: `{"path": "...", "content": "<raw text, ≤1 MB>"}`
- Notes: 400 on missing/over-long path; 404 if the file is absent.

### Install a ClawHub skill
- Method: POST
- Path: /skills/clawhub/install/{slug}
- Query: `?owner=<handle>` (optional; disambiguates a slug shared by multiple publishers)
- Response: `{"slug": "...", "name": "...", "description": "...", "install_source": "marketplace"}`
- Notes: Downloads the deterministic zip artifact for the slug and installs it. Same response/error matrix as POST /skills/marketplace/install/{slug} (409 already installed, 403 malicious, 422 invalid payload, 502 upstream). An ambiguous shared slug passed without `?owner=` is auto-resolved to the most-downloaded publisher (audit-logged) rather than failing — see **Ambiguous slugs** above.

### Install a bundled skill
- Method: POST
- Path: /skills/install/{name}
- Response: `{"name": "...", "slug": "...", "description": "...", "install_source": "..."}`
- Notes: Installs from bundled (downloadable) skills. The `{name}` path segment is the skill's slug (always lowercase + hyphens). Bundled skills come from the embedded binary (offline); the proprietary set (docx/pdf/pptx/xlsx) is fetched over HTTP from the upstream Anthropic skills repo (no `git` required). Error matrix: **400** invalid/unknown skill name, **404** not in the upstream Anthropic repo, **409** already installed, **500** otherwise (download/extraction failure).

### Install a marketplace skill
- Method: POST
- Path: /skills/marketplace/install/{slug}
- Response: `{"slug": "string", "name": "string", "description": "string", "install_source": "marketplace"}`
- Notes: Downloads and installs from the marketplace. Use the slug from GET /skills/marketplace. The response `name` is the frontmatter display label (may differ from the slug, e.g. slug `xiaohongshu-mcp-skills` with name `xiaohongshu`).

### Upload a skill from a ZIP file
- Method: POST
- Path: /skills/upload?force=true
- Body: `multipart/form-data`; field `file` = ZIP payload. Size is bounded only by a memory / zip-bomb backstop (1 GiB compressed and 1 GiB uncompressed), not a product limit — skills install to the user's local disk.
- Response 201: `{"name": "...", "slug": "...", "description": "...", "install_source": "local"}`
- Response 409: `{"error": "skill_already_exists", "existing_name": "...", "existing_description": "...", "existing_prompt": "...", "new_description": "...", "new_prompt": "..."}`
- Response 403: `{"error": "skill_is_builtin"}` — returned when the ZIP targets an auto-installed builtin (`kocoro`, `kocoro-generative-ui`). `force=true` does NOT override this; the builtin guard is unconditional because `EnsureBuiltinSkills` would wipe any override on the next daemon restart.
- Response 400: `{"error": "invalid multipart form: ..."}` — returned when the multipart body is malformed or the `file` field is missing. Distinct from 413 (which means the archive or its extracted contents exceeded the size backstop).
- Response 413: the ZIP archive or its extracted contents exceed the size backstop (1 GiB compressed / 1 GiB uncompressed)
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
