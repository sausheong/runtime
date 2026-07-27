# DESIGN.md

The runtime console's design system. Product register. Written 2026-07-26.

Everything here must survive an air-gapped deployment: system font stacks only,
no CDN, no build step, plain CSS in `console/static/style.css`.

## Theme: light, faintly cool

The scene: *a platform engineer at their desk mid-afternoon, laptop screen
beside a terminal window, checking whether four agents are up before going back
to the thing they were actually doing.*

That sentence forces light. This is daytime desk work sitting alongside browser
tabs, docs, and a ticket tracker, not a 2am incident wall in a dim NOC. The
console is also, in bulk, a configuration surface: minting keys, registering
upstreams, adding users. Deliberate reading, not glanceable telemetry.

Worth naming which reflexes this avoids, because there are three:

1. **Dark**, the first answer for anything touching agents or observability.
2. **Tailwind slate with an indigo primary**, the second answer once dark is
   rejected. The console was exactly this before 2026-07-26.
3. **Warm cream**, the third. This was the answer from 2026-07-26 to 2026-07-27,
   and it is the current default "tasteful" AI surface — a deterministic
   detector flags it by name. On a screen beside a terminal it also reads softer
   than the work: this console revokes keys and deletes security policies.

The answer is light with a *faint cool cast*: hue 258 at chroma 0.002–0.013.
That is near-neutral, not blue. Tailwind slate runs chroma 0.012–0.02 and reads
visibly tinted; at a third of that the eye registers "paper, correctly lit"
rather than "coloured". All the personality is spent in one place: the accent.

## Colour strategy: Restrained

Tinted neutrals plus one accent held under 10% of the surface. Correct for a
dense operator tool: colour carries meaning here, so spending it on decoration
devalues the signal. Measured accent coverage runs 0.03%–0.9% of page area.

All values OKLCH. No `#000`, no `#fff`.

### Neutrals (hue 258, near-neutral)

| Token | Value | Use |
|---|---|---|
| `--bg` | `oklch(0.975 0.003 258)` | Page field |
| `--surface` | `oklch(0.995 0.002 258)` | Panels, table backgrounds |
| `--surface-sunk` | `oklch(0.960 0.005 258)` | Toolbars, table headers, insets |
| `--border` | `oklch(0.898 0.006 258)` | Decorative hairlines |
| `--border-strong` | `oklch(0.628 0.013 258)` | **Control boundaries** |
| `--text` | `oklch(0.230 0.011 258)` | Body ink |
| `--muted` | `oklch(0.518 0.011 258)` | Secondary text |
| `--muted-strong` | `oklch(0.428 0.013 258)` | Small text: labels, badges, table heads |

Two rules govern these, and both are requirements rather than preferences.

**Text clears 4.5:1 against the darkest surface it can land on, not the
lightest.** `.subtitle` sits in `.page-head`, i.e. directly on `--bg`, so
measuring against `--surface` flatters it. Worst-case measured ratios: `--text`
15.0, `--muted` 4.94, `--muted-strong` 7.28.

**The two border tokens are different jobs.** `--border` is decoration (panel
edges, table rules) and 1.35:1 is fine — WCAG 1.4.11 exempts purely decorative
boundaries. `--border-strong` is the boundary of an interactive control, which
1.4.11 requires to clear 3.0:1. Before 2026-07-27 both were the same light value
and every text field measured **1.64:1 against its own fill** — the design
system was less accessible than the unstyled browser default it replaced.
`0.628` is the lightest value that clears 3.0 on the darkest surface a control
can sit on (3.14 / 3.31 / 3.51 on sunk / bg / surface).

### Accent: plum

`oklch(0.400 0.115 328)`, hover `oklch(0.328 0.104 328)`, soft
`oklch(0.962 0.017 328)`, border `oklch(0.720 0.075 328)`.

Chosen by elimination as much as taste. Red, green, and amber are spoken for by
danger, success, and warning; using any of them for a link would collide with
status. Blue and violet are the category reflex. Plum at deep lightness is
distinct from all four semantic hues and never gets mistaken for a state.

Deepened from `L=0.45 C=0.14` on 2026-07-27: the brighter plum read closer to
magenta, which is a consumer register, and white-on-accent rose from 7.9:1 to
9.8:1 as a side effect.

Accent is for primary actions, current selection, links, and focus. Not for
decoration, not for headings, not for fills, and **not for row hover** — every
row in a table is not actionable, and a plum hover wash made the interface flush
pink as the pointer crossed it. `--row-hover` is a neutral tonal step.

### Semantic states

| Role | Text | Soft bg | Badge border | Control border |
|---|---|---|---|---|
| Success | `oklch(0.385 0.080 158)` | `oklch(0.970 0.019 158)` | `oklch(0.735 0.070 158)` | — |
| Warning | `oklch(0.428 0.088 75)` | `oklch(0.967 0.026 75)` | `oklch(0.735 0.080 75)` | — |
| Danger | `oklch(0.478 0.150 27)` | `oklch(0.969 0.015 27)` | `oklch(0.720 0.090 27)` | `oklch(0.650 0.095 27)` |

Danger carries a fourth value because its border does double duty: outlining a
badge (decorative, since the badge's own text names the state) and outlining the
Remove/Rotate button (a control boundary, so 3.0:1 applies). One token cannot be
both; `--danger-control` measures 3.33:1.

Never colour alone: pair with a filled-vs-hollow dot, an icon, or text. The
word carries the state, the colour reinforces it.

## Typography

System stack, one family, plus mono for identifiers:

```
system-ui, -apple-system, "Segoe UI", Roboto, sans-serif
ui-monospace, "SF Mono", Menlo, Consolas, monospace
```

Fixed rem scale at a 1.2 ratio. No clamp, no fluid headings: users view at
consistent DPI, and a shrinking h1 in a narrow panel looks worse.

| Step | Size | Weight | Use |
|---|---|---|---|
| `--fs-h1` | 1.5rem | 640 | Page title |
| `--fs-h2` | 1.125rem | 620 | Panel heading |
| `--fs-body` | 0.9375rem | 400 | Body, table cells |
| `--fs-sm` | 0.85rem | 400 | Secondary, descriptions |
| `--fs-xs` | 0.75rem | 600 | Labels, badges, table heads |

Prose caps at 68ch. Tables are exempt and may run full width.

A class that sets `font-size` must not be applied to a heading. `.mono` does,
and as a class it outranks the `h1` element selector, so `<h1 class="mono">` on
the agent page rendered at **13.6px — smaller than 15px body text**. Explicit
`h1.mono` / `h2.mono` rules restore the heading size and keep only the family.

## Space and rhythm

A 4px base scale: `4 8 12 16 24 32 48`. Vary it. Uniform padding everywhere is
the monotony that makes an interface read as generated. Panel padding is looser
than table cell padding; the page head gets more air below it than between its
own two lines.

**The gap between two panels must exceed the padding inside one.** At 1rem gap
against 1.25rem padding, nine consecutive panels read as one continuous ruled
sheet: the hairline borders looked like row rules, not boundaries. Now 1.75rem
gap (28px measured) against 1.4rem padding (22.4px). That ordering is what makes
a panel read as a discrete object rather than a band.

Radius: 8px on panels and inputs, 6px on small controls, 999px on badges only.

## Elevation

Borders, not shadows. One hairline is enough separation on a tinted field, and
a shadow under every panel is the SaaS-card tell. Exactly one shadow exists, on
the login panel, because it genuinely floats over an empty field.

## Components

Every interactive element ships default, hover, focus-visible, active,
disabled. Focus is a 2px accent outline at 2px offset, never suppressed.

- **Panels** over cards. A bordered region with a heading, not a floating tile.
  Never nested.
- **Tables** are the primary data surface: sunk header, hairline rows, hover
  tint, right-aligned actions.
- **Badges** for enumerated state only (role, health, model), never as
  decoration.
- **Empty states** teach the next action. "No agents registered" plus how to
  register one.
- **Danger zone** for a destructive maintenance action: a rule, the consequence
  in prose beside the control, and the same `.danger` styling as every Remove
  button. Not a red panel, which would shout on every page load.
- **Section index** on any page past ~3 screens. Nine text links, no JS, gated
  on the same flags as the sections so it cannot advertise a panel that did not
  render.

### The top bar

One definition, `templates/_topbar.html`, included by every signed-in page.
It was previously copy-pasted into seven templates and drifted: Observability,
agent, session, and eval-run had gone stale without "Switch tenant", so the menu
gained and lost an item as you navigated, and the page whose entire job is
switching tenants was the one hardest to reach from the others. A menu that
changes shape under you costs the recognition that makes a nav usable at all.

Duplicated markup is the mechanism, so the duplication is what got deleted
rather than re-synchronised. `console/nav_test.go` holds the line from both
sides: it compares the bar's rendered links across all seven pages through the
real handler, and it fails any template that hand-rolls `<header class="topbar">`
instead of including the partial.

The partial takes the current section key. Child pages (an agent, a session, an
eval run) pass `""` and mark nothing: `aria-current="page"` on a link that leads
somewhere else tells a screen-reader user they are on a page they are not on.

### Form controls

Select them with `:not()`, never an allowlist. The rule used to enumerate
`input[type=text], input[type=password], input:not([type]), select` and thereby
missed `textarea` and `input[type=number]`: five of thirty-nine controls on the
onboarding page rendered as raw browser widgets beside styled siblings, and they
were the Cedar-policy and JSON-cases fields — the highest-stakes inputs here. An
allowlist fails open every time someone adds an input type; `:not()` fails
closed.

Every required field carries a visible marker (`<span class="req">`), not just
the `required` attribute. The marker goes on the label *text*: these labels wrap
their control, so a `::after` on the `<label>` lands after the input and renders
as a stray asterisk floating below the field.

### Feedback

One flash channel with two presentations, chosen by a severity prefix on the
cookie (`ok:` / `err:`, following the existing `key:` convention). Before this,
`flashRedirect` had a single presentation styled with the success tokens, so
`"Policy rejected: <parser error>"` was announced **in green** while
`.flash-error` sat unreachable in the stylesheet.

An author error belongs on the page, not on a bare 400. The eval-set and
eval-policy forms are textareas of hand-written JSON, and an unstyled
`text/plain` error page discards everything the operator typed.

Anything that changes state asynchronously carries `role="status"` and, where it
updates over time, `aria-live="polite"`. The session transcript previously had
three states — connecting, connected-and-silent, stream-dead — and one
appearance: an empty dark slab, because `onerror` closed the stream silently.

## Banned here

On top of the shared bans: no gradient logo tile (the previous console had
one), no drop-shadowed card grid, no dark-mode-by-reflex, no indigo, no warm
cream page field, and no all-caps run longer than a short label (uppercase
removes the word shapes we read by; the longest `<legend>` here is a
45-character instruction, not a label).
