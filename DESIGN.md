# DESIGN.md

The runtime console's design system. Product register. Written 2026-07-26.

Everything here must survive an air-gapped deployment: system font stacks only,
no CDN, no build step, plain CSS in `console/static/style.css`.

## Theme: light, warm

The scene: *a platform engineer at their desk mid-afternoon, laptop screen
beside a terminal window, checking whether four agents are up before going back
to the thing they were actually doing.*

That sentence forces light. This is daytime desk work sitting alongside browser
tabs, docs, and a ticket tracker, not a 2am incident wall in a dim NOC. The
console is also, in bulk, a configuration surface: minting keys, registering
upstreams, adding users. Deliberate reading, not glanceable telemetry.

Worth naming why this is the harder answer: the reflex for anything touching
agents or observability is dark, and the second reflex, once dark is rejected,
is Tailwind slate with an indigo primary. The console was previously the second
one exactly. Light and *warm* avoids both.

## Colour strategy: Restrained

Tinted neutrals plus one accent held under 10% of the surface. Correct for a
dense operator tool: colour carries meaning here, so spending it on decoration
devalues the signal.

All values OKLCH. No `#000`, no `#fff`: every neutral is tinted warm so the
surface reads as paper rather than as a default.

### Neutrals (warm, hue ~80)

| Token | Value | Use |
|---|---|---|
| `--bg` | `oklch(0.972 0.006 85)` | Page field |
| `--surface` | `oklch(0.995 0.003 85)` | Panels, table backgrounds |
| `--surface-sunk` | `oklch(0.955 0.008 85)` | Toolbars, table headers, insets |
| `--border` | `oklch(0.905 0.008 80)` | Hairlines |
| `--border-strong` | `oklch(0.835 0.010 80)` | Inputs, buttons |
| `--text` | `oklch(0.245 0.012 70)` | Body ink |
| `--muted` | `oklch(0.555 0.012 70)` | Secondary text at ≥14px |
| `--muted-strong` | `oklch(0.445 0.014 70)` | Small text: labels, badges, table heads |

`--muted-strong` exists because `--muted` drops below 4.5:1 at small sizes on
tinted backgrounds. Keep the split.

### Accent: plum

`oklch(0.45 0.14 335)`, hover `oklch(0.375 0.14 335)`, soft
`oklch(0.955 0.022 335)`.

Chosen by elimination as much as taste. Red, green, and amber are spoken for by
danger, success, and warning; using any of them for a link would collide with
status. Blue and violet are the category reflex. Plum at deep lightness is
distinct from all four semantic hues, reads as considered ink rather than
brand-pop on warm paper, and never gets mistaken for a state.

Accent is for primary actions, current selection, links, and focus. Not for
decoration, not for headings, not for fills.

### Semantic states

| Role | Value |
|---|---|
| Success | `oklch(0.52 0.12 155)` |
| Warning | `oklch(0.62 0.13 75)` |
| Danger | `oklch(0.505 0.165 27)` |

Each has a soft background and a border variant. Never colour alone: pair with
a filled-vs-hollow dot, an icon, or text.

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

## Space and rhythm

A 4px base scale: `4 8 12 16 24 32 48`. Vary it. Uniform padding everywhere is
the monotony that makes an interface read as generated. Panel padding is looser
than table cell padding; the page head gets more air below it than between its
own two lines.

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

## Banned here

On top of the shared bans: no gradient logo tile (the previous console had
one), no drop-shadowed card grid, no dark-mode-by-reflex, no indigo.
