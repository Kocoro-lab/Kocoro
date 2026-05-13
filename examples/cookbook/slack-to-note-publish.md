# Publish a Truth Social digest to note.com from Slack

A single Slack message asks the daemon to research Trump's latest Truth Social posts and publish a Japanese-language summary to note.com. Ends with a real published article.

## Entry point

Slack DM → Shannon Cloud relay → Kocoro daemon (cloud-routed session).

Prompt:

> Go to note.com and publish a new article summarizing Trump's latest Truth Social posts.

## Key tools (in order)

1. `tool_search` — load `browser_*` and `web_search` schemas (deferred-tool mode).
2. `web_search` + `web_fetch` — 5 calls across Truth Social URLs, Reuters, CNBC. Picks up the exact post text, date, and surrounding context.
3. `browser_navigate` (Playwright MCP) → `https://note.com/notes/new`. The user's Chrome profile is already authenticated, so the editor opens directly.
4. `browser_click` on title → `browser_type` with the Japanese headline.
5. `browser_click` on body → `browser_type` with the full article body.
6. `browser_click` on the "Proceed to publish" button, then the "Publish" button to ship the article.

~36 LLM calls, ~12 minutes, ~$0.98. Published successfully.

## Gotchas

- **Cloud sessions have no shell CWD.** `browser_snapshot(filename="x.md")` used to drop the snapshot somewhere the agent couldn't locate, and `file_read("x.md")` failed with "no session working directory is set". The daemon now allocates `~/.shannon/tmp/sessions/<id>/` as a scratch CWD for Slack/LINE/Feishu/lark/Telegram/webhook sessions, and file-producing MCP tools get their `filename` arg rewritten to an absolute path under that scratch. Result: `browser_snapshot` and a follow-up `file_read` agree on the same file.
- **Publish-settings page loads slowly.** The "Proceed to publish" click lands on an async page with auto-suggested hashtags. Give the default `timeout` room; don't add a fixed `wait_for` unless the existing click timeout proves insufficient.
- **Don't paste directly into the editor's rich-text surface.** note.com's editor normalizes pasted HTML oddly. `browser_type` into the focused textbox handles newlines cleanly.

## When to reach for this pattern

- Multi-source research that needs to land as a real-world artifact (blog post, doc, message), not a chat reply.
- The final site requires a real logged-in browser — Playwright MCP with the user's Chrome profile is the only viable path. API-only publishing platforms are simpler and should skip the browser entirely.
