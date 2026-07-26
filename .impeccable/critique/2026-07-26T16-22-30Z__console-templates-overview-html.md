---
target: console (overview + 8 templates)
total_score: 21
p0_count: 0
p1_count: 2
timestamp: 2026-07-26T16-22-30Z
slug: console-templates-overview-html
---
# Critique: runtime console

Target: `console/templates/overview.html` (+ 8 sibling templates, `console/static/style.css`)
Register: product. Run: 2026-07-27. Two independent assessments (LLM design review + deterministic detector), combined.

## Design Health Score

| # | Heuristic | Score | Key Issue |
|---|-----------|-------|-----------|
| 1 | Visibility of System Status | 3 | Status strip and filled/hollow dots are excellent; a `running` eval run never updates without a manual reload |
| 2 | Match System / Real World | 3 | Correct infra register, mirrors `runtimectl` vocabulary |
| 3 | User Control and Freedom | 1 | Five destructive actions have no confirmation; no undo anywhere |
| 4 | Consistency and Standards | 2 | `running` renders `badge-ok` at observability.html:37 and `badge-muted` at :92; two CSS classes referenced but undefined |
| 5 | Error Prevention | 1 | "fill exactly one of the two below" is prose, not validation; no client check on JSON/Cedar textareas |
| 6 | Recognition Rather Than Recall | 1 | 32 of 39 form controls have no accessible label (placeholder-only) |
| 7 | Flexibility and Efficiency | 2 | No search, filter, sort or pagination on any of 18 tables; onboarding is a 5,610px scroll |
| 8 | Aesthetic and Minimalist Design | 3 | Genuinely restrained; loses a point to onboarding's undifferentiated nine-panel wall |
| 9 | Error Recovery | 2 | The error on a failed eval run is styled `.subtitle` — muted secondary — so failure reads as an afterthought |
| 10 | Help and Documentation | 3 | Empty states genuinely teach; overview.html:30-34 gives the literal CLI command |
| **Total** | | **21/40** | **Viewing surfaces ~28; onboarding drags it down, and that is where all the consequence lives** |

## Anti-Patterns Verdict

**LLM assessment: not AI-generated.** Measured, not asserted: no gradient text, no `backdrop-filter` anywhere, no side-stripe borders (explicitly refused in a comment), no hero-metric tiles, no identical card grid (the six landing pillars are a hairline list), two shadow uses in 586 lines, both justified. Palette resolves to `rgb(248,245,241)` on `rgb(130,48,115)` — warm paper, deep plum, not slate/indigo.

Category-reflex, second altitude: **partially caught.** Once you ban dark, ban slate-indigo, and ban Grafana, the remaining space converges on "warm light neutral + one deep desaturated accent". The *family* is predictable from the anti-references; the specific hue and elimination logic are not. Honest read: the anti-references did most of the work here.

**Deterministic scan.** Verified against a positive control first (gradient text, side stripe, glass card, hero metric: 4 of 4 caught), so an empty result means clean.

- Source-only scan of `console/templates/`: **0 findings**.
- Rendered scan with real CSS: **22 findings**, now **14** after fixes.
  - `low-contrast` x8 — **REAL, FIXED**.
  - `cream-palette` x9 — disputed, see below.
  - `flat-type-hierarchy` x3 — partially real; consolidated four off-scale sizes, declined the rest.
  - `all-caps-body` x2 — **false positive**: flagged strings are 21 and 25 characters, i.e. short labels, which the rule exempts.

The 0-vs-22 gap is itself the lesson: markup-only checks cannot see anything that emerges from CSS.

**The contrast bug was mine, and the detector caught what my own verification missed.** `--muted` at L=0.555 clears 4.5:1 on `--surface` (4.69:1) but fails on `--bg` (4.39:1) and `--surface-sunk` (4.18:1). Every `.subtitle` sits in `.page-head`, directly on `--bg`. I had measured muted-on-panel — the lightest background it can land on — and reported a pass. Fixed at L=0.530; re-verified by walking all 278 visible text nodes across nine templates against their real computed backgrounds. Zero failures. Committed `731cfc2`.

**On `cream-palette` (9 hits), disputed.** The detector argues warm off-white is itself the reflex, and that is a fair challenge: "avoid slate, go warm" is a known second-order move, and the LLM review independently flagged the same convergence. It stands here because the theme was argued from a scene rather than chosen for taste, and deep plum is not the warm-cream-plus-terracotta pairing the reflex produces. A judgement call, not a refutation.

## Overall Impression

The visual system is genuinely good and the restraint is real. The problem is not how it looks; it is what it lets you do. Heuristics 3, 5, and 6 all sit at 1.

The single biggest issue: **the confirmations are on the wrong actions.** Verified by walking each destructive button up to its enclosing form:

- **Unconfirmed** (fire immediately): remove user, revoke agent key, remove managed agent, remove gateway upstream, **delete Cedar security policy**.
- **Confirmed**: remove quota, remove eval set, remove online eval policy.

Every irreversible, security-relevant action is unguarded; the guards landed on the recoverable ones. The app's own copy states the consequence: "No policies — all tool calls are permitted (permit by default)." One unconfirmed click can silently widen what every agent may do.

## What's Working

1. **The empty states are the best writing in the product.** Not "No agents" but the literal `runtimectl admin agent add --id <id> --url <url>` in a bordered code chip. onboarding.html:258 states the *security consequence* of emptiness, not just the emptiness.
2. **`.card:has(> table) { overflow-x: auto }`** — one selector at the container fixes 18 tables across six templates. Verified at 375px: zero document overflow on all nine pages.
3. **The accessibility work that was done is real.** Status is dot-shape plus text, never colour alone. The `--muted`/`--muted-strong` split exists precisely because the lighter token fails at 12px on tinted backgrounds.

## Priority Issues

**[P1] Five destructive actions fire with no confirmation, and the existing confirmations are on the wrong rows.**
Remove user (:41), revoke agent key (:71), remove managed agent (:111), remove upstream (:216), delete Cedar policy (:255) all submit on click. Quotas, eval sets, and eval policies — all recoverable — have `confirm()`. This trains the operator that a dialog means "safe to ignore", devaluing it exactly where it is load-bearing.
*Fix:* `onsubmit="return confirm(...)"` on all five, with consequence-naming copy: `Remove policy no-destructive-sql? Tool calls it currently forbids will be permitted.` Typed-name confirmation for key revocation and policy deletion.
*Command:* `/impeccable harden`

**[P1] 32 of 39 form controls have no accessible label.**
Placeholder-only across Users, Agent keys, Managed agents, Credentials, OAuth2, OBO, Upstreams, Policies, Quotas, Eval sets, Eval policies. Placeholder is not an accessible name under WCAG 3.3.2, and the label vanishes the moment you type — in a form where you enter a secret name in one field and a token URL six fields later. Directly contradicts PRODUCT.md's claim that accessibility is load-bearing. The templates already do this correctly at :228, :265, :294, :352; the pattern exists, it just was not applied to the `.row` forms.
*Command:* `/impeccable harden`

**[P2] Two CSS classes are referenced but never defined.**
`.inline-form` (observability.html:69) and `.policy-text` (onboarding.html:253). Not cosmetic: `.inline-form` computes to `display:block`, leaving 0px between the Launch-run button and the table below, so the primary action reads as a table row. `.policy-text` inherits the global dark `pre`, rendering a Cedar policy as a black slab in a table cell.
*Command:* `/impeccable polish`

**[P2] Onboarding is a 5,610px undifferentiated wall.**
Nine structurally identical panels, 24 buttons, 39 inputs, no progressive disclosure, no in-page nav, no completion state. The OAuth2 and OBO forms alone are 16 permanently-visible inputs for credential types most tenants never use.
*Fix:* `<details>` around OAuth2/OBO; sticky panel index.
*Command:* `/impeccable layout`

**[P3] `running` is two colours on one page; `.subtitle` carries four unrelated jobs.**
`badge-ok` at observability.html:37 vs `badge-muted` at :92 and app.js:3. `.subtitle` serves page subtitle, panel description, table timestamp, and the eval-run **error message**. An error and a timestamp should not share a class.
*Command:* `/impeccable clarify`

## Persona Red Flags

**Alex (SRE / power user).** No keyboard shortcuts, no command palette, no filter on any of 18 tables. Will keep using `runtimectl` and open the console only when forced.

**Jordan (inherits the deployment).** Lands on Agents; the empty state teaches, which is good. But nothing explains what a "generation" is or what to do when an agent is unreachable. On onboarding, faces 39 unlabelled inputs and can delete a security policy with one click.

## Minor Observations

- `--warning` is defined and **never used** (0 references), while PRODUCT.md calls degraded/unknown first-class states.
- `lang="en"` on **2 of 9** templates.
- No skip link; keyboard users tab the full nav on every navigation.
- `.stat-label` margin lands on the wrong edge under `column-reverse` — measured label-to-value gap is 0px, and the comment claims otherwise.
- Eval-run has no auto-refresh; a running run shows `—` until manual reload.
- The one-shot secret key reveal has no copy button, and it is shown exactly once.
- `.landing-foot` sits outside `.landing`, so it renders full-bleed while content is capped at 1040px.

## Questions to Consider

1. If the CLI is the source of truth, why can the console delete a security policy at all? It currently has the authority of a primary interface and the safety affordances of a read-only one.
2. The eval-run Summary renders four `.stat` tiles for Set, Agent, Status, Passed — three of which are strings, in a component justified by `tabular-nums`. An agent ID at 22px is a hero metric wearing a different hat.
3. The empty states teach beautifully. What teaches at eleven policies and six keys, with no search across any of the 18 tables? The console is most helpful when it has least to say.
