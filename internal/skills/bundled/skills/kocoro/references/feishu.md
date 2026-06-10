# Connect Feishu / Lark (飞书 / Lark 渠道连接)

Register a self-built Feishu (飞书) or Lark application so its bot routes inbound
messages to your Kocoro agents. Feishu and Lark share the same flow — they differ
only in the developer-portal domain and the `channel_type` value.

**New architecture (no Encrypt Key, no webhook).** The bot connects over a
Cloud-driven long connection (larkws). There is NO Encrypt Key to generate and NO
request/callback URL to configure — you only collect the App ID + App Secret and
register them. Cloud establishes and holds the long connection; the user just
needs this Mac's daemon to stay running.

**Infer the platform from the user's wording — only ask if genuinely ambiguous.**
飞书 / Feishu → `channel_type: feishu` (portal open.feishu.cn). Lark →
`channel_type: lark` (portal open.larksuite.com). Do NOT re-ask "Feishu or Lark?"
when the user already named one (e.g. "怎么添加飞书机器人" clearly means Feishu).

**Naming, user-facing:** always refer to the product as **Kocoro** in anything
the user sees. NEVER say "Shannon" to the user; it is an internal name.
(`api.shannon.run` / Cloud endpoints stay as-is in code/URLs — only references
shown to the user must say Kocoro.)

## Default: guide the user — do NOT offer to auto-drive unless asked

**By default, walk the user through the steps themselves.** Do NOT proactively
offer to drive the browser for them. Only switch to the automated mode below when
the user **explicitly asks** Kocoro to do it (e.g. "帮我自动操作 / 帮我自动完成 /
你帮我操作"). Otherwise, give the text steps and let them do it.

One-click create-app links (the template pre-grants the bot capability and the
message scopes, so the only scope left is the user-info one):

- Feishu: [https://open.feishu.cn/page/launcher?from=backend_oneclick](https://open.feishu.cn/page/launcher?from=backend_oneclick)
- Lark: [https://open.larksuite.com/page/launcher?from=backend_oneclick](https://open.larksuite.com/page/launcher?from=backend_oneclick)

### Steps to give the user (mirror https://kocoro.ai/zh/help/lark/)

1. **Create the app.** Open the one-click link above, fill in the app name, and
   create. The success page shows **App ID and App Secret right on it** — copy
   both.
2. **Register the bot in the Kocoro app.** Open Kocoro → **连接器 / Connectors**
   → find **Lark(国际)** or **飞书(国内)** → click **添加机器人 / Add Bot** →
   paste the **App ID** and **App Secret** → pick the agent that should answer
   messages (optionally set a display name) → **注册机器人 / Register**. The bot
   connects over the long connection immediately. (Do NOT talk about a "local
   daemon" — point the user at this Connectors screen.)
3. **Grant the user-info scope.** In the developer portal's 权限管理 / Permissions,
   search for and enable `contact:user.base:readonly` (so conversation titles show
   the user's name). The other scopes already came from the template.
4. **Publish the app.** In 版本管理与发布 / Version management, submit the release.
   If the workspace requires admin approval, the bot becomes usable only after an
   admin approves — tell the user to contact their admin.

That's the whole flow. Note there is normally **no need to withdraw anything** —
withdrawing only applies in the automated mode when an auto-submitted release
locks editing AND the workspace requires approval (see below).

## Automated mode (ONLY when the user explicitly asks Kocoro to operate)

When the user asks Kocoro to do it for them, drive the browser and call the local
daemon endpoints. Otherwise stay in the guide-the-user mode above.

### Pick a non-colliding display name first

You need a bot **display name** for the create-app form. **Before suggesting one,
`GET http://localhost:7533/channels/feishu/app-installs`** to see which display
names are already in use, then suggest one that does NOT collide — Feishu and Lark
share one name space (both stored as type `feishu`), so it must be unique across
both. No bot yet → "Kocoro 助手" is fine; already taken → suggest "Kocoro 助手
(Lark)" / "Kocoro Lark". The bot binds to the **default agent** unless the user
names a specific one (don't ask — default is fine).

### Don't pre-check login — go straight to create

Do NOT open a separate page just to check login. Go straight to the one-click
create URL (`{host}/page/launcher?from=backend_oneclick`) and start. ONLY if it
shows a **sign-in screen** instead of the create-app form, hand the user a
clickable login link (the same launcher URL), then **Kocoro waits ~30s itself
(e.g. `sleep 30`) before re-opening — never ask the user to wait or count down**
(the login session isn't synced to Kocoro's browser instantly). After the wait,
silently re-open the launcher and continue.

### Navigate by URL, don't click through menus

`{host}` = `https://open.feishu.cn` (Feishu) or `https://open.larksuite.com`
(Lark); `{app_id}` = the `cli_...` ID available after the app is created.

| Page | URL |
|---|---|
| One-click create app | `{host}/page/launcher?from=backend_oneclick` |
| Grant user-info scope (pre-filled) | `{host}/app/{app_id}/auth?q=contact:user.base:readonly&op_from=openapi&token_type=tenant` |
| Version management — status / 撤回 withdraw / 申请线上发布 re-apply | `{host}/app/{app_id}/version` |

### Automated steps

1. **Create + grab credentials on the success page.** Open the launcher URL, fill
   ONLY the name (leave the icon default), create. The success page shows **App ID
   and App Secret directly** — click the eye (👁) icon to reveal the secret and
   copy both here. Do NOT go hunting on the 凭证与基础信息 page.
2. **Register with Kocoro.** Call the local daemon endpoint (it forwards to Cloud
   with the user's API key — you never handle the key):
   - `http POST http://localhost:7533/channels/feishu/app-installs`
     ```json
     {
       "channel_type": "feishu",
       "app_id": "cli_...",
       "app_secret": "...",
       "display_name": "Kocoro 助手"
     }
     ```
     Use `"channel_type": "lark"` for Lark. Omit `agent_name` for the default
     agent. `display_name` optional.
   - 201/200: registered — Cloud is bringing up the long connection.
   - 400: field-level errors from Cloud; relay which field is wrong and re-prompt.
   - 500 `channel row create failed`: **display_name collides with an existing
     bot** (Feishu+Lark both stored as type `feishu`, channel rows unique per
     account+type+**name**). This is NOT a "one channel per account" limit — do
     not say that. Recover: `GET .../channels/feishu/app-installs`, pick a
     different display_name, retry the POST.
   - 503: cloud not configured. 502: cloud unreachable.
3. **Grant the user-info scope.** Open
   `{host}/app/{app_id}/auth?q=contact:user.base:readonly&op_from=openapi&token_type=tenant`.
   **Tick the consent checkbox FIRST, then click 确定 / Confirm** (the button is
   disabled until the box is checked). If the app is **locked under admin review**
   (header "无法修改,企业管理员正在审核应用发布申请") and you cannot edit scopes,
   first open `{host}/app/{app_id}/version`, open the pending version and click
   **撤回 / Withdraw** to unlock — then grant the scope. If it's not locked, no
   withdraw is needed.
4. **Publish / re-apply for release — do NOT edit anything.** Open
   `{host}/app/{app_id}/version`, open the version, and click **申请线上发布 / Apply
   for release** (top-right). Do NOT create a new version or touch the version
   number (it's a React controlled input that traps you in a retry loop — there's
   no need to change it).
5. **Check the result and tell the user the truth.** Look at the version status:
   - 已生效 / Published → bot is live; usable in DMs or group chats.
   - 审核中 / pending admin review → say honestly: registration + long connection
     are ready, but the app must be **approved by the workspace admin** first; ask
     them to contact the admin. Pending review is NOT a failure — do not retry it.

Group chats: the bot must be added to the group before it will respond there.

### Reliability rules (automated mode — never hand the browser to the user)

- **Navigate by URL**; don't click through menus.
- **Verify saves actually succeeded — do NOT assume.** After 保存 / Save (or a
  dialog confirm), the portal may silently fail to persist while still navigating
  away. Re-open the page and confirm before moving on; never tell the user
  something "saved" until the re-check passes.
- **Retry transient failures up to 3 times** (one-click create, scope save,
  release submit can flake). Pending admin review is a status, not a failure.
- **Never hand the browser to the user** (it lives inside Kocoro and closes when
  the turn ends); the only user action in their own browser is signing in.

## Unbind a registered bot (解绑 / 删除已连接的机器人)

When the user asks to unbind / remove / disconnect a Feishu or Lark bot, work
against the local daemon (no browser needed):

1. **List the bots:** `http GET http://localhost:7533/channels/feishu/app-installs`.
   Each entry has an `id` (UUID) plus `channel_type` / `app_id` / `display_name` /
   `agent_name`.
2. **Identify which to remove.** More than one → show the user display name +
   app_id and ask which; do NOT guess. Empty list → tell them nothing is bound.
3. **Confirm before deleting** — destructive. State which bot, get a clear yes.
4. **Delete:** `http DELETE http://localhost:7533/channels/feishu/app-installs/{id}`.
   - 200/204: unbound — Cloud tears down the long connection.
   - 404: that id isn't bound (already removed, or not yours). 503: cloud off.

This only removes the Kocoro-side binding (stops the long connection). It does NOT
delete the app from the developer portal — tell the user to delete it there if
they want the app itself gone.

## Sending files to the user (附件 / 发文件)

Whenever the user wants to RECEIVE a file — judge this by intent, not by exact
wording. "把这份报告发给我", "我需要 X 文件", "给我那个 PDF", "能不能发我一份" all
count; there is no fixed phrase to match. Publish it with `publish_to_web` and
then **always present the result as a markdown link `[文件名](url)`** — never a
bare URL. Cloud automatically turns a `[name](url)` link pointing at the Kocoro
CDN (`https://static.kocoro.ai/…`, URL ending in a file extension) into a
**downloadable Feishu/Lark attachment**.
A raw URL is delivered as plain text and is NOT converted, so the user would have
to open it in a browser instead of getting the file inline.

Do this without the user having to ask for "markdown format" — it is the default
expected behavior on Feishu/Lark. Example reply: `报告好了：[2026-Q2.pdf](https://static.kocoro.ai/…/2026-Q2.pdf)`.

## Formatting on Feishu/Lark cards

Feishu/Lark cards render markdown links, **bold/italic**, and lists, but they do
NOT render GFM pipe tables — a `| a | b |` table surfaces as raw text. When you
have tabular data to show on these channels, use a bulleted/numbered list (or a
short labeled lines layout) instead of a markdown table. Headers and fenced code
blocks render fine.

## Security note

`app_secret` is sensitive. It is stored by Cloud; the daemon only forwards it over
localhost and never logs it. Avoid echoing it back to the user after registration.
