---
target: console (overview + 8 templates)
total_score: 24
p0_count: 1
p1_count: 3
timestamp: 2026-07-27T01-00-44Z
slug: console-templates-overview-html
---
# Critique: runtime console

Target: `console/templates/onboarding.html` (+ 8 sibling templates, `console/static/style.css`)
Register: product. Run: 2026-07-27. Two independent assessments (LLM design review +
deterministic detector), combined. Both assessments worked from the nine templates rendered
with realistic data and served locally; the stylesheet was verified byte-identical to the one
live at runtime.sausheong.com.

## Design Health Score

| # | Heuristic | Score | Key Issue |
|---|-----------|-------|-----------|
| 1 | Visibility of System Status | 2 | `session.html` renders an empty 942x96px dark slab: connecting, no-events, and stream-dead are visually identical, and `es.onerror` closes silently |
| 2 | Match System / Real World | 4 | "shadowed by operator file", "Delete stale row", "permit by default" — Cedar's real semantics, the operator's own vocabulary |
| 3 | User Control and Freedom | 2 | Confirmations now cover 12/12 destructive forms, but there is no undo anywhere and 65 `http.Error` calls dead-end on unstyled text/plain |
| 4 | Consistency and Standards | 2 | `agent.html`'s h1 computes to 13.6px, smaller than 15px body; 5 of 39 controls render as raw UA defaults; nav differs across 4 of 7 pages |
| 5 | Error Prevention | 2 | "Rotate keyring", the one irreversible action, is byte-identical to "Mint key" and overlaps its own description by 8px |
| 6 | Recognition Rather Than Recall | 2 | Placeholder-as-label across onboarding; health words ("Healthy"/"Unreachable") live only in a `title` attribute |
| 7 | Flexibility and Efficiency | 2 | 5,118px / 5.7 screens, 9 sections, zero section ids, zero anchors, zero `<details>` |
| 8 | Aesthetic and Minimalist Design | 4 | Accent measured at 0.03–0.94% of page area against a 10% ceiling; borders not shadows, held throughout |
| 9 | Error Recovery | 1 | `flashRedirect` has one presentation and `.flash` is hard-coded success-green; "Policy rejected: ..." is announced in green |
| 10 | Help and Documentation | 3 | `card-desc` teaches genuinely well; only 3 of 19 empty states name a next action |
| **Total** | | **24/40** | **Fair — strong surface, weak feedback layer** |

## Anti-Patterns Verdict

**Does this look AI-generated? No, and not marginally.**

Measured in the computed CSS, not eyeballed:

| Ban | Count |
|---|---|
| Gradient text (`background-clip: text`) | 0 |
| Glassmorphism (`backdrop-filter`) | 0 |
| Side-stripe borders (border-left/right > 1px as accent) | 0 |
| Hero-metric template | 0 (figures are 22.4px; strings moved to a status strip) |
| Identical card grids | 0 (landing pillars are a hairline list) |
| Modal-as-first-thought | 0 (`<dialog>`: none; overlay CSS: none) |

No OKLCH hue in 240–310 anywhere: the indigo-on-slate reflex is genuinely absent, and the
accent (plum, h=335) was chosen by elimination so a link can never be misread as a state.

**Deterministic scan**: 20 findings, 4 rules, `npx impeccable detect` v3.3.1.

- `cream-palette` (8) — previously defended: theme argued from a scene sentence in DESIGN.md.
- `flat-type-hierarchy` (3) — previously declined. Measured ratios 1.333 / 1.200 / 1.103 /
  1.133. Both assessments independently agree the decision is correct: widening the bottom
  steps pushes labels under 12px in a tool read under pressure.
- `single-font` (6) — **false positive, verified**: two families render (109 sans nodes, 13
  mono). The detector reports "only font used is sf mono".
- `all-caps-body` (2) — **the prior adjudication was wrong on the facts.** The audit recorded
  "false positive: 21- and 25-character labels". Measured: 45 and 32 characters.
  `TRANSPORT — FILL EXACTLY ONE OF THE TWO BELOW` is an instruction sentence set in 12px
  all-caps, not a label. The detector is right; this one should be fixed.

Browser overlay unavailable this run: `impeccable live` was removed from the CLI in 3.3.1,
and URL-mode `detect` errors (`elId.startsWith is not a function`). File-mode is authoritative.

## Overall Impression

The surface is genuinely well designed and the evidence is in the stylesheet: measured
contrast recorded on three surfaces, a `--muted`/`--muted-strong` split that exists because
one failed, a `badge-warn` added because "unknown" was borrowing ok-or-danger, and a comment
that argues *against* a design-law ratio and names the alternative it tried and rejected. That
is design reasoning, not component assembly.

What fails is everything one layer below the surface. Both assessments converged
independently on the same shape: **the success path was designed; every other path inherited
whatever the browser or the default gave it.** A rejected Cedar policy renders in success
green. Five controls skipped the CSS selector list and render as raw UA widgets. The session
stream has three states and one appearance. The `h1` on the most-visited detail page is
smaller than its own body text. None of these are taste disagreements; each is a defect that
survived a careful review because the review happened one file at a time, and each defect
lives in the seam *between* CSS, template, and Go handler.

The single biggest opportunity: the console's stated philosophy ("the CLI is the source of
truth; the console is for seeing, not doing everything") points the opposite way from the
built artefact. The longest, densest, most defect-laden page exists to *do* things; the page
dedicated to *seeing* the fleet ships three columns and no health at all.

## What's Working

**The colour system is an argument, not a palette.** Plum at `oklch(0.45 0.14 335)` was chosen
by elimination — red/green/amber are spoken for by danger/success/warning, blue/violet is the
category reflex — so accent can never be confused with state. Accent occupies 0.03% of the
agent page and 0.94% of onboarding, against DESIGN.md's own 10% ceiling. "Restrained" is
enforced, not aspirational.

**Status encoding survives grayscale.** `dot-ok` is filled, `dot-off` is hollow — the same
8px circle, differentiated by fill rather than hue, so WCAG 1.4.1 holds in substance and not
just in letter. Paired with badges that carry a hairline as well as a tint.

**Two `<pre>` elements, two correct answers.** The session transcript gets a warm-tinted dark
surface (`oklch(0.245 0.014 70)`) that says "verbatim machine output" while staying part of
the system. `pre.policy-text` explicitly *refuses* to inherit it, because a Cedar policy is
configuration you read, not a log you tail. Distinguishing those two requires knowing what
Cedar is.

## Priority Issues

**[P0] Failures are announced in success green.**
`flashRedirect` has exactly one presentation, and `.flash` is hard-coded to `--success-bg` /
`--success-border` / `--success-text` (measured `oklch(0.965 0.025 155)` on
`oklch(0.4 0.09 155)`). `console.go:591` routes `"Policy rejected: " + err.Error()` through
it. An operator writing a security policy is told, in the console's own success language, that
a rejected policy succeeded. `.flash-error` exists in the CSS but is reachable from exactly
one template (`eval-run.html:37`) and from no onboarding path.
*Fix:* give the flash cookie a severity prefix (`ok:` / `err:`), matching the `key:` convention
the code already parses at `console.go:385`; expose `.IsError`; render
`class="flash {{if .IsError}}flash-error{{end}}"`.
*Command:* `/impeccable harden`

**[P1] The one irreversible action looks exactly like a routine save, and overlaps its own explanation.**
"Rotate keyring" measures `background: oklch(0.45 0.14 335)` with no class — byte-identical to
"Mint key" and "Save quota". It is also the first element in the Credentials section, above
the description of what credentials are, and its bottom edge sits 8px *past* that
description's top edge because `.card-desc { margin: -.5rem 0 1rem }` assumes an h2 always
precedes it. The words say irreversible; the pixels say routine save. Operators clicking
through a 5,000px form trust the pixels.
*Fix:* `class="danger"`, move it below the description, and scope the negative margin to
`h2 + .card-desc`.
*Command:* `/impeccable harden`

**[P1] Five of 39 controls escaped the design system entirely.**
`style.css:265` enumerates `input[type=text], input[type=password], input:not([type]), select`
and omits `textarea` and `input[type=number]`. The quota rate field renders `border-radius: 0`,
`border: 2px inset rgb(118,118,118)`, `font-size: 13.3px`, `padding: 1px 2px` — a raw UA widget
directly below a correctly styled 8px/15px sibling. Same for all three JSON/Cedar textareas,
which also fall back to the UA focus outline instead of the accent ring. These are the
highest-stakes inputs on the page.
*Fix:* add `textarea, input[type=number], input[type=email], input[type=url]` to the selector
list. Prefer an inverted selector (`input:not([type=hidden]):not([type=submit])`) so the next
input type added cannot silently escape.
*Command:* `/impeccable harden`

**[P1] `session.html` shows a black void with no states.**
`<pre id="events">` measures `textContent.length === 0`, 942x96px, no `aria-live`, no `role`.
`streamSession()` sets `es.onerror = () => { es.close(); }` — the stream dies silently. No
loading, no empty, no error, no retry, on the page an operator opens specifically to read what
an agent did. `loadSessions()` in the same file does all three correctly.
*Fix:* apply the existing `sessionsStateRow` pattern — "Connecting to session stream…",
"No events recorded for this session.", "Stream disconnected. Refresh to reconnect." Add
`aria-live="polite"`.
*Command:* `/impeccable harden`

**[P2] `agent.html`'s h1 renders at 13.6px, smaller than 15px body text.**
`.mono { font-size: var(--fs-sm) }` outranks the `h1` element selector, so
`<h1 class="mono">{{.AgentID}}</h1>` computes to 13.6px — the same size as its own subtitle.
Every other page's h1 is 24px. The console's most-visited detail view has no page title in the
visual hierarchy.
*Fix:* `h1.mono { font-size: var(--fs-h1); }`, or scope `.mono` to family only.
*Command:* `/impeccable typeset`

## Persona Red Flags

**Priya (on-call SRE, checking whether anything is down).** Lands on `/ui` after login. The
page is three columns — ID, Name, Model — 490px of content in a 1201px viewport, with no
health, no session count, no last-seen. Her actual question is answered one click away on
observability, where the data is already computed per agent. The founding scene in PRODUCT.md
is "checking whether four agents are up"; the page that scene lands on cannot answer it. When
she does reach observability, a fully-down agent (0/1 replicas) is differentiated from a
healthy one by a single 8px dot in a ~54,000px² row, and the word "Unreachable" exists only in
a `title` attribute she can't hover on a tablet.

**Marcus (platform engineer, onboarding a new tenant).** Opens onboarding: 5,118px, 9 sections,
39 inputs, 92 controls, no section index, no anchors, no `<details>`. The skip link lands him
at `#main`, i.e. the top of the same 5,118px. He pastes a Cedar policy into a textarea styled
like a 1998 form control, gets it slightly wrong, and the console tells him "Policy rejected:
..." in a green success bar. He fixes it, submits, and is redirected to the top of the page —
4.8 screens from where he was working, with no fragment to bring him back. He does this nine
times.

**Sam (screen-reader user, admin role).** No `aria-live` region exists anywhere in the console
— zero occurrences across all nine templates and both JS files. The flash message, the session
load error, and the entire SSE event stream all update the DOM silently. Sam submits a form,
hears nothing, and cannot tell whether it worked. PRODUCT.md names accessibility a hard
constraint, and the static layer honours it well (39/39 controls labelled, skip link, no
heading skips, real landmarks) — but every *dynamic* announcement is missing.

## Minor Observations

- 19 empty states; only 3 name a next action. `overview.html`'s is the model (it names the
  exact `runtimectl` command). Thirteen are bare "No X yet."
- `select-tenant.html` contradicts itself when empty: the static subtitle says "Your account
  has access to more than one tenant" above a table that says "No tenant memberships."
- Two token-count tiles overflow: `4,821,903` spills 13px past its 94px box into the
  neighbouring tile. Both are token counts, which on a busy agent are *always* 7+ digits —
  guaranteed in production, not an edge case. "Cache create" also wraps to two lines while its
  six siblings don't.
- Seven of nine control types have boundaries below the WCAG 1.4.11 non-text floor of 3.0.
  Text inputs and selects measure **1.64:1** border-against-fill, and their fill is identical
  to the card behind them, so the border is the only thing marking where you can type.
  (Conversion done OKLCH→OKLab→linear sRGB and calibrated against known 21:1 and 4.48:1 pairs;
  `getComputedStyle` returns OKLCH unresolved and canvas mis-parses it.)
- Confirmation copy is bimodal: five name consequences excellently ("Tool calls it currently
  forbids will be PERMITTED"), four are generic filler ("Remove this quota?").
- Eight em dashes in visible prose, against the skill's standing no-em-dash rule (excluding
  `<title>` and the `—` used as a null-value glyph, which is legitimate).
- `<h3>` in Credentials is unstyled: 17.55px/700, a browser default belonging to no token.
- Zero links in `main` on onboarding — no Cedar syntax reference, no docs, for a field that
  demands recalled syntax.
- Nav inconsistency: "Switch tenant" on 3 of 7 authenticated pages; `aria-current="page"` on 3
  of 7; `select-tenant.html`'s `<title>` reverses the house pattern.

## Questions to Consider

1. **PRODUCT.md says the CLI is the source of truth and the console is "for seeing, not for
   doing everything". So why is the console's longest and most defect-laden page the one that
   *does* things, while the page dedicated to *seeing* ships three columns and no health?** If
   onboarding genuinely belongs to `runtimectl`, the console version could shrink to a
   read-only inventory plus the exact commands — and most of the P1s above evaporate.

2. **The stylesheet contains extraordinary self-critique — recorded contrast measurements,
   a named and rejected previous indigo, a defended argument against a design-law ratio. So
   how did an h1 smaller than body text, an 8px button-over-text overlap, and an error message
   painted success-green survive it?** The gap isn't care. It's that all three live in the
   seam between CSS, template, and Go handler, and the review happened one file at a time.

3. **Does this project have any check that renders a page and looks at it?** Every defect in
   the P0/P1 list is invisible to `go vet`, invisible to a CSS reading, and invisible to a
   template reading — but obvious within seconds of rendering the page with realistic data.
   That gap is the same one that produced the earlier chart regression: a change whose blast
   radius no gate actually exercises.
