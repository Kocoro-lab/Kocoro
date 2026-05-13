# Scrape a Substack and generate a Word doc

One Desktop prompt: pull every post from a Substack, summarize them, and drop a Word document on the user's desktop.

## Entry point

Desktop app (Kocoro Desktop) → local daemon over the HTTP API.

Prompt:

> Scrape every article from https://deepmanifold.substack.com/, summarize them into a Word document, and put it on my Desktop in a new folder called "substack".

## Key tools (in order)

1. `use_skill(docx)` — load the docx skill (python-docx recipes).
2. `web_crawl` / `web_subpage_fetch` — first attempt at bulk content. Results are truncated; only a few posts survive.
3. `browser_navigate` → `/archive` → `read_page(raw)`, then several `execute_js` probes trying to harvest post URLs from the DOM. **All return `[]`** because Substack's archive list is client-rendered after DOM-ready.
4. **Pivot**: `http` → `https://<name>.substack.com/api/v1/posts?limit=50&offset=0`. Returns JSON with every post's body. 28 posts, one call, done.
5. `bash curl ... | python3` → parse, strip HTML, write posts to `/tmp/substack_posts.json`.
6. `file_write` the docx-generation script to `/tmp/make_docx.py`, then `bash python3 /tmp/make_docx.py` to produce `~/Desktop/substack/Deep_Manifold_All_Articles.docx`.

27 LLM calls, ~12 minutes, ~$1.15. Output: 28 articles, 109 KB docx.

## Gotchas

- **Substack (and most modern blogs) render post lists with JavaScript.** `execute_js` at page-load returns `[]` before hydration, `read_page` truncates, and `web_crawl` top-n runs are capped. Check for a JSON API endpoint (`/api/v1/posts`, `/_next/data/...`, RSS, sitemap) **before** reaching for browser automation. If one exists, you'll save a dozen tool calls.
- **`execute_js` is expression context on the chromedp backend.** Scripts with a bare `return` or top-level `const x = 1; return x` used to fail with `SyntaxError: Illegal return statement`. Multi-statement scripts are now auto-wrapped in an `async` IIFE; plain expressions (including `JSON.stringify(x);`) pass through unchanged so their value isn't accidentally turned into `undefined`.
- **Heredoc + Python regex is a bash landmine.** `bash "python3 -c 'import re; re.split(r\"(?<=[.!?])\\s+(?=[A-Z\"'])\", ...)'"` inevitably collides on quoting. Write a script via `file_write` and run `python3 /path/to/script.py`.

## When to reach for this pattern

- Content aggregation where the source has a structured backend, not just a public page. Substack, Medium (sometimes), WordPress, Ghost, Hugo with `index.json` — all reward the API-first detour.
- Artifact generation that's worth ten-plus seconds of setup because the output lands somewhere stable (user's disk, Drive, a drafts folder) rather than scrolling off the chat surface.
