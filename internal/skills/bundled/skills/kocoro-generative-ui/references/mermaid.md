
## Mermaid diagrams

For diagram families where auto-layout matters: ERDs, sequence flows, state machines, gantt timelines, class diagrams, git graphs, journey maps. Mermaid does layout, cardinality, and connector routing for free — hand-placing these in SVG fails the same way every time.

**When to reach for mermaid (vs. hand-rolled SVG):**

| Diagram family | Use mermaid? | Reason |
|---|---|---|
| ERD / database schema (`erDiagram`) | **yes** | Crow's-foot routing + per-entity row layout |
| Sequence diagram (`sequenceDiagram`) | **yes** | Lane allocation + arrow stacking |
| State machine (`stateDiagram-v2`) | **yes** | Transition routing through a graph |
| Class diagram / UML (`classDiagram`) | **yes** | Same row-stack problem as ERD |
| Gantt chart (`gantt`) | **yes** | Date axis + bar packing |
| Git branch graph (`gitGraph`) | **yes** | Branch lane assignment |
| User journey map (`journey`) | **yes** | Phase columns + sentiment row |
| Flowchart | **no — use `diagrams.md` SVG patterns** | Spacing/coordinate rules already tuned there |
| Illustrative ("how does X work") | **no — use `diagrams.md` SVG patterns** | Spatial metaphor needs hand control |
| Bar / line / pie charts | **no — use `charts.md`** | Chart.js is purpose-built |
| Geographic map | **no — use `maps.md`** | D3 + real topology |

**One diagram per `html-artifact` fence.** Mermaid renders one diagram per `render()` call; if you need a second, emit a second fence with prose between (see `diagrams.md` — "Always add prose between diagrams").

### Canonical recipe

The init block below is calibrated for the host design system — `theme: 'base'` strips mermaid's default gradients/shadows (otherwise this skill's "no gradients" rule is silently violated), `fontFamily` matches `--font-sans`, and color tokens auto-flip for dark mode.

`mermaid@11` is fetched from `esm.sh` (one of the four CSP-allowed CDNs). `cdnjs` hosts an older UMD build that won't accept the ESM import path — stick with `esm.sh`.

The leading `<h2 class="sr-only">` is the parent SKILL's accessibility requirement for every HTML widget — rewrite the sentence to describe your specific diagram (don't ship the example text).

```html
<style>
#diagram svg { max-width: 100%; height: auto; display: block; }
</style>
<h2 class="sr-only">Sequence diagram: browser request flows through an edge worker that caches a feed payload from origin.</h2>
<div id="diagram"></div>
<script type="module">
import mermaid from 'https://esm.sh/mermaid@11/dist/mermaid.esm.min.mjs';
const dark = matchMedia('(prefers-color-scheme: dark)').matches;
await document.fonts.ready;
mermaid.initialize({
  startOnLoad: false,
  theme: 'base',
  fontFamily: 'var(--font-sans), sans-serif',
  themeVariables: {
    darkMode: dark,
    fontSize: '13px',
    fontFamily: 'var(--font-sans), sans-serif',
    lineColor: dark ? '#9c9a92' : '#73726c',
    textColor: dark ? '#c2c0b6' : '#3d3d3a',
    primaryColor: dark ? '#2a2a26' : '#f4f3ec',
    primaryTextColor: dark ? '#c2c0b6' : '#3d3d3a',
    primaryBorderColor: dark ? '#4a4842' : '#d4d2c8',
  },
});
const { svg } = await mermaid.render('d', `sequenceDiagram
  participant Browser
  participant Edge as Edge worker
  participant Origin
  Browser->>Edge: GET /api/feed
  Edge->>Origin: fetch (cache miss)
  Origin-->>Edge: 200 + feed payload
  Edge-->>Browser: 200 + Cache-Control: max-age=60`);
document.getElementById('diagram').innerHTML = svg;
</script>
```

### Initialization rules

- `startOnLoad: false` — sandbox iframes race with mermaid's auto-discovery. Always call `mermaid.render()` explicitly.
- `await document.fonts.ready` before `initialize()` — mermaid measures text width during layout. Without this, labels measured against the system fallback font will clip when SF Pro loads later.
- Pass the diagram source as a JS template literal to `mermaid.render(id, source)`, not by putting `class="mermaid"` on a `<pre>` and letting mermaid scan the DOM. Explicit render keeps streaming behavior deterministic.
- One `mermaid.render()` per fence. Multiple diagrams = multiple `html-artifact` fences.
- The render call returns a Promise — the surrounding script is `type="module"`, so top-level `await` works without an IIFE.

### Mermaid source style

- **Quote labels containing spaces, parens, brackets, colons, or non-ASCII**: `A["foo (bar)"]`, not `A[foo (bar)]`. Unquoted labels with special chars trigger silent parse failures and produce empty SVGs.
- `%% mermaid comments` are fine — they're stripped before render, no streaming risk. (Unlike `<!-- HTML comments -->`, which are still forbidden in the surrounding HTML body per the parent SKILL rules.)
- For `flowchart`, prefer `TD` (top-down) over `LR` — the 680px widget is narrow; `LR` overflows for >4 nodes. But hand-rolled SVG is the preferred path for flowcharts (see family table above).
- Sentence case on all labels and edge text — same rule as SVG diagrams. Mermaid does not normalize case for you.
- Edge labels go in `--` markers (`A -->|sends payload| B`), not as floating text near the line. The default routing keeps them readable.

### Per-family fix-ups

#### ERDs and class diagrams — round outer boxes, strip inner row borders

Mermaid 11 draws entity outlines as sharp-cornered `<path>` elements and puts visible strokes on every attribute row. Both clash with the design system. After `innerHTML = svg`, patch:

```html
<style>
#diagram svg.erDiagram .divider path { stroke-opacity: 0.5; }
#diagram svg .row-rect-odd path,
#diagram svg .row-rect-odd rect,
#diagram svg .row-rect-even path,
#diagram svg .row-rect-even rect { stroke: none !important; }
</style>
```

```js
// Replace sharp-cornered entity outlines with rx="8" rects
document.querySelectorAll('#diagram svg .node').forEach(node => {
  const firstPath = node.querySelector('path[d]');
  if (!firstPath) return;
  const nums = firstPath.getAttribute('d').match(/-?[\d.]+/g)?.map(Number);
  if (!nums || nums.length < 8) return;
  const xs = [nums[0], nums[2], nums[4], nums[6]];
  const ys = [nums[1], nums[3], nums[5], nums[7]];
  const x = Math.min(...xs), y = Math.min(...ys);
  const w = Math.max(...xs) - x, h = Math.max(...ys) - y;
  const rect = document.createElementNS('http://www.w3.org/2000/svg', 'rect');
  rect.setAttribute('x', x); rect.setAttribute('y', y);
  rect.setAttribute('width', w); rect.setAttribute('height', h);
  rect.setAttribute('rx', '8');
  for (const a of ['fill', 'stroke', 'stroke-width', 'class', 'style']) {
    if (firstPath.hasAttribute(a)) rect.setAttribute(a, firstPath.getAttribute(a));
  }
  firstPath.replaceWith(rect);
});

// Strip the borders mermaid 11 draws on attribute rows
document.querySelectorAll(
  '#diagram svg .row-rect-odd path, #diagram svg .row-rect-odd rect,' +
  '#diagram svg .row-rect-even path, #diagram svg .row-rect-even rect'
).forEach(el => el.setAttribute('stroke', 'none'));
```

The class diagram (`classDiagram`) needs the same treatment — the init block and fix-up loop are identical; only the diagram source changes.

Example sources:

```
erDiagram
  USERS ||--o{ POSTS : writes
  POSTS ||--o{ COMMENTS : has
  USERS {
    uuid id PK
    string email
    timestamp created_at
  }
```

```
classDiagram
  class Animal {
    +String name
    +int age
    +speak() void
  }
  class Dog
  Animal <|-- Dog
```

#### Sequence diagrams — fix dark-mode actor visibility

The default actor box fill renders too light against the chat background in dark mode. If actors visually disappear, add overrides to `themeVariables`:

```js
themeVariables: {
  // ...the base set above, plus:
  actorBkg: dark ? '#2a2a26' : '#f4f3ec',
  actorBorder: dark ? '#4a4842' : '#d4d2c8',
  actorTextColor: dark ? '#c2c0b6' : '#3d3d3a',
  noteBkgColor: dark ? '#3a3a35' : '#fdf6e3',
  noteBorderColor: dark ? '#4a4842' : '#e0d8b8',
},
```

Use `participant X as Display name` (with `as`) when the diagram identifier differs from the on-screen label — keeps the source compact while making the rendered label human-readable.

#### Gantt — pad the section label column

Section labels clip at the left edge when section names are long. Either shorten the names (recommended — diagram should be scannable) or pad the column:

```js
mermaid.initialize({
  // ...
  gantt: { leftPadding: 120 },
});
```

Example source:

```
gantt
  dateFormat YYYY-MM-DD
  axisFormat %b %d
  section Backend
    Migrate auth: 2026-06-01, 7d
    Wire up oauth: after Migrate auth, 5d
  section Frontend
    Login screen: 2026-06-08, 4d
```

#### State diagrams — use v2 syntax

Use `stateDiagram-v2`. The v1 `stateDiagram` directive is legacy and produces less consistent output.

```
stateDiagram-v2
  [*] --> Idle
  Idle --> Loading: fetch()
  Loading --> Loaded: 200
  Loading --> Error: 4xx | 5xx
  Loaded --> Idle: reset
  Error --> Idle: retry
```

#### Git graphs — accept the default branch colors

Mermaid auto-colors branches from a fixed palette that ignores `themeVariables`. If branch colors clash with your design tokens, accept it — overriding `gitGraph.branchColors` requires `themeCSS` injection, which is forbidden under this skill's rules.

```
gitGraph
  commit
  branch feature/login
  checkout feature/login
  commit
  commit
  checkout main
  merge feature/login
```

### Forbidden (mermaid-specific)

- `theme: 'default'`, `'dark'`, `'forest'`, `'neutral'` — all ship gradients and shadows. Use `'base'`.
- `themeCSS: '…'` — string-injected CSS bypasses design-system token discipline. Use `themeVariables` only.
- `startOnLoad: true` — race-prone in the sandbox iframe.
- `fa:` / `fab:` font-awesome icon syntax in node labels — emoji-equivalent decoration; the skill's "no emoji" rule applies.
- Mermaid `%%{init: …}%%` directives in the diagram source — config belongs in `initialize()`, not in the diagram body, so it stays editable per-fence and doesn't drift from this recipe.
- `cdnjs.cloudflare.com/.../mermaid` — that build is older UMD-only. Always import from `esm.sh/mermaid@11/dist/mermaid.esm.min.mjs`.
