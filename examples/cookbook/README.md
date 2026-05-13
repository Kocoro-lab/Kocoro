# Kocoro Cookbook

Short, concrete recipes for multi-step tasks people have actually run through Kocoro. Each entry records the shape of the task, the tools that carried the load, and the gotchas worth knowing before you try it yourself. These are **patterns**, not full transcripts — they age better that way.

The goal is a growing corpus of "this is the kind of thing the agent is good at" so new users can skim a few recipes and get a realistic picture beyond one-shot prompts.

## Recipes

- [Publish a Truth Social digest to note.com from Slack](slack-to-note-publish.md) — cloud-routed daemon session, Playwright MCP for note.com, `web_search` + `web_fetch` for research, long session with tool-call coordination.
- [Scrape a Substack and generate a Word doc](substack-scrape-to-docx.md) — Desktop session, `docx` skill, pivot from JavaScript-rendered browser scraping to the site's JSON API when the first approach stalls.

## Adding a recipe

Recipes are most useful when they're short and specific. Aim for:

- **Goal** — one sentence, imperative. "Publish X to Y from Slack."
- **Entry point** — how the request arrived (Slack, Desktop, CLI, schedule).
- **Key tools** — three to six tools that did the real work, in the order they ran.
- **Gotchas** — two or three things that would have tripped up a naive run.
- **Outcome** — what the user got, including timing and cost if meaningful.

Skip the full tool-call log. If a future run needs to match exact behavior, add an E2E scenario test instead — the cookbook is for humans, the test suite is for regressions.
