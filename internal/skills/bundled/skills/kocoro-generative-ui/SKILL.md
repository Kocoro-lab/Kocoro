---
name: kocoro-generative-ui
description: |
  Generate interactive, inline HTML/SVG widgets (charts, diagrams, forms,
  dashboards, illustrations) that render in sandboxed iframes inside Kocoro
  Desktop chat. Use when the user asks to "visualize", "chart", "diagram",
  "explain visually", "show me", or when data is denser than a paragraph of
  prose.
allowed-tools: file_read file_write publish_to_web think
hidden: true
metadata:
  version: "1.0.2"
  user-invocable: "true"
---

# Kocoro Generative UI

You produce rich visual content — SVG diagrams and HTML interactive widgets —
that renders inline in the Kocoro Desktop conversation, token-by-token, in a
WKWebView sandbox.

## Output Contract

Wrap every widget in a markdown fenced code block with info string
`html-artifact`:

    ```html-artifact title="Q1 revenue" id=art_a1 mime=text/html
    <style>.bar{background:var(--color-background-info)}</style>
    <h2 class="sr-only">Bar chart of Q1 revenue across three regions</h2>
    <div style="position:relative;height:240px"><canvas id="c1"></canvas></div>
    <script src="https://cdnjs.cloudflare.com/ajax/libs/Chart.js/4.4.1/chart.umd.js"></script>
    <script>
      new Chart(document.getElementById('c1'), { /* ... */ });
    </script>
    ```

- `title` — required; shown to the user and used as export filename stem
- `id` — optional; if absent, host generates a UUID
- `mime` — `text/html` (default) or `image/svg+xml`
- `theme` — `auto` (default), `light`, or `dark`
- The content is a **fragment**. Do NOT write `<!DOCTYPE>`, `<html>`, `<head>`,
  `<body>` — the host wraps them.
- Explanatory prose goes OUTSIDE the fence, in your normal reply.
- Max 3 artifacts per reply.
- When you need to show the user the fence syntax itself (e.g., teaching this
  feature), insert a zero-width space (U+200B) between the backticks in the
  example: <code>&#96;&#96;&#8203;&#96;html-artifact</code>. This prevents
  your example from being captured as a real artifact.

## Streaming Safety

Output streams token-by-token. Structure code so useful content appears early
and the script tag last:

- HTML: `<style>` (short) → content HTML → `<script>` last. Scripts execute
  after the fence closes, not during streaming.
- SVG: `<defs>` (markers) → visual elements.
- Avoid gradients, shadows, blur — they flash during streaming DOM diffs. Use
  solid flat fills.
- No `<!-- HTML comments -->` or `/* CSS comments */`. They waste tokens and
  may land mid-stream in ways that confuse rendering.

## CDN allowlist (CSP-enforced)

External scripts, styles, fonts, images, and `fetch` / `XMLHttpRequest` / `d3.json` calls may reach ONLY these four hosts:

- `cdnjs.cloudflare.com`
- `esm.sh`
- `cdn.jsdelivr.net`
- `unpkg.com`

Any other origin is blocked by CSP and the request silently fails. You cannot
call arbitrary external APIs, your own backend, `raw.githubusercontent.com`,
or anything else — only static assets from these four CDNs. This applies to
both `<script src>`/`<link href>` loads and runtime `fetch()` calls (e.g. D3
topology JSON from jsdelivr is fine; fetching a weather API is not).

## Design system

- **Flat** — no gradients, mesh backgrounds, noise textures, decorative
  effects. Clean flat surfaces only.
- **Dark mode is mandatory** — every color must work in both light and dark.
  Use the CSS variables below; they auto-adapt. Never hardcode `color: #333`.

### CSS variables provided by the host

Use these, not hardcoded hex:

- Backgrounds: `--color-background-primary` (white/near-black),
  `--color-background-secondary` (surfaces), `--color-background-tertiary`
  (page bg), `--color-background-info`, `--color-background-danger`,
  `--color-background-success`, `--color-background-warning`
- Text: `--color-text-primary`, `--color-text-secondary`,
  `--color-text-tertiary`, `--color-text-info`, `--color-text-danger`,
  `--color-text-success`, `--color-text-warning`
- Borders: `--color-border-tertiary` (0.08α, default),
  `--color-border-secondary` (0.16α, hover), `--color-border-primary` (0.24α),
  semantic `-info/-danger/-success/-warning`
- Typography: `--font-sans` (SF Pro), `--font-serif` (New York, rare editorial
  use), `--font-mono` (SF Mono)
- Layout: `--border-radius-md` (8px), `--border-radius-lg` (12px, preferred),
  `--border-radius-xl` (16px)

### Typography

- Default font is `var(--font-sans)`. Headings: h1 22px, h2 18px, h3 16px —
  all `font-weight: 500`. Body: 16px, weight 400, line-height 1.7.
- Two weights only: 400 regular, 500 bold. Never 600 or 700.
- Sentence case always. Never Title Case, never ALL CAPS. Applies to SVG text
  labels and diagram headings too.
- No mid-sentence bolding. Bold is for headings and labels only. Entity names,
  class names, function names go in `code style`, not **bold**.

### Borders, corners, form elements

- Borders: `0.5px solid var(--color-border-tertiary)` (upgrade to `-secondary`
  on hover).
- Corners: `var(--border-radius-md)` for most elements, `-lg` for cards.
- No rounded corners on single-sided borders.
- Form elements (`input`, `select`, `textarea`, `button`, range `input`) are
  pre-styled — write bare tags, override only when necessary.

### Widget container

- The container is `display: block; width: 100%`. Start with your content
  directly; no wrapper div needed. If you want vertical breathing room, add
  `padding: 1rem 0` on your first element.
- Never use `position: fixed` — the host sizes the iframe to content height,
  so fixed positioning collapses it. For modal mockups, wrap everything in a
  normal-flow faux-viewport: `<div style="min-height: 400px; background:
  rgba(0,0,0,0.45); display: flex; align-items: center; justify-content:
  center;">` and put the modal inside.

## Color palette (9 ramps × 7 stops)

Classes: `c-blue`, `c-teal`, `c-coral`, `c-pink`, `c-purple`, `c-green`,
`c-amber`, `c-red`, `c-gray`. Each provides a light-mode and dark-mode
background + foreground + SVG fill/stroke. Apply to a `<g>` wrapping
shape+text, or to `<rect>`/`<circle>`/`<ellipse>` directly — never to `<path>`.

**Assignment rules:**
- Color encodes meaning, not sequence. Group nodes by category.
- Prefer `purple`, `teal`, `coral`, `pink` for general diagram categories.
  Reserve `blue`, `green`, `amber`, `red` for where the node genuinely
  represents informational, success, warning, or error meaning.
- Use `gray` for neutral/structural nodes.
- 2–3 colors per diagram, not 6+.

**Text on colored backgrounds:** use the 800 or 900 stop from the same ramp
for text — never plain black or generic gray. When both title and subtitle sit
on a colored fill, title must be darker (800 in light mode, 100 in dark) and
subtitle lighter (600 light, 200 dark).

## Use case router

When the user asks for specific output types, read the relevant reference
file via the `file_read` tool:

- Charts (bar, line, pie, scatter, dashboards) → `references/charts.md` —
  Chart.js patterns, legends, number formatting
- Flowcharts / structural diagrams → `references/diagrams.md` +
  `references/svg-setup.md`
- Illustrative diagrams ("how does X work") → `references/diagrams.md`
- UI mockups, cards, forms, comparisons → `references/ui-components.md`
- Geographic maps (choropleths) → `references/maps.md`

## Forbidden

- Emoji (use CSS shapes or SVG paths)
- Gradients, drop shadows, blur, glow, neon
- `position: fixed`
- `<!-- HTML comments -->` / `/* CSS comments */`
- Font sizes below 11px
- Title Case / ALL CAPS (sentence case only)
- Mid-sentence bolding (use `code style` for identifiers)
- Dark/colored backgrounds on the outermost container (host provides bg)
- Nested scrolling (auto-fit height)
- `display: none` sections during streaming (hidden content streams
  invisibly; show all stacked)

## Accessibility

- HTML widgets: begin with a visually-hidden one-sentence summary:
  `<h2 class="sr-only">…</h2>`.
- SVG widgets: root `<svg role="img">` with `<title>` and `<desc>` as the
  first two children.

## Output discipline

- Text goes in your response, visuals go in the artifact. Explanatory
  paragraphs, intros, section headings — all in normal response prose, not
  inside the fence.
- Tables go in response prose as markdown, not inside widgets.
- If you need multiple diagrams for a complex topic, stream them as separate
  artifacts with prose between each.

## Sharing externally (optional)

The inline `html-artifact` block is the **primary** output — it renders in
Kocoro Desktop with no public URL and no upload. Use it whenever the user's
goal is just "show me".

If the user *also* wants a sharable link (to send via Slack/Feishu/email, embed
in a doc, etc.), you may additionally:

1. Use `file_write` to save the same HTML body to a temp path
   (e.g. `/tmp/<short-id>.html`).
2. Use `publish_to_web` with a specific `purpose` describing the recipient and
   channel (e.g. `"send revenue chart link to user via Slack"`).

Skip this entire block if the user did not ask for a shareable link — never
publish a copy "just in case." Public URLs cannot be deleted.
