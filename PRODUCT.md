# PRODUCT.md

Context for design work on the runtime console. Written 2026-07-26.

**register: product**

The console is an authenticated operator surface, not a marketing site. Design
serves the task. There is no funnel and no signup: by the time anyone sees this
UI they have already self-hosted the thing.

One page is the exception. `/` and `/ui/login` are served to someone who is not
authenticated, and that page is closer to brand register. It is still not
marketing: the reader is an operator who has just deployed this, or a colleague
they sent the URL to, so it is written as reference — what the platform is, what
it does, what it needs, and what it costs to run. It states the pre-release
status and the absence of a licence rather than burying them, because a reader
who discovers those after deploying will not trust anything else on the page.

## What runtime is

An on-prem, self-hostable platform for running durable LLM agents. The
open-source equivalent of AWS Bedrock AgentCore, for organisations that cannot
or will not send their agents' data to a managed cloud. Six pillars: agent
runtime (spine), identity, memory, a tool gateway, sandboxes, observability.

The console is the human window onto a control plane that is otherwise driven
by `runtimectl` and a REST API. It is deliberately not the primary interface.
The CLI is the source of truth; the console is for seeing, not for doing
everything.

## Who uses it

Platform engineers, SREs, and infrastructure-minded developers who run runtime
inside their own network. Two moments dominate:

1. **Onboarding a tenant.** A tenant admin mints keys, registers gateway
   upstreams, adds users. Deliberate, low-frequency, high-consequence.
2. **Checking on the fleet.** Which agents are up, what sessions ran, what a
   transcript says, why an eval failed. Frequent, fast, often under mild
   pressure.

They are fluent in Grafana, Kubernetes dashboards, and terminal tooling. They
will not be impressed by decoration and will be annoyed by anything that puts
a click between them and a fact.

## Constraints that shape the design

These are hard, not preferences:

- **Air-gapped by default.** No CDN, no Google Fonts, no external anything. The
  whole point is a deployment with no route to the public internet. Fonts must
  be system stacks; assets must be served from the binary.
- **Rendered by Go templates.** `html/template` server-side, one small
  `app.js`, no framework, no build step. Whatever the design asks for has to be
  expressible in plain CSS and semantic HTML.
- **Density is a feature.** Agent tables, session lists, metric rows. Users
  want to see many things at once, not scroll through generous cards.
- **Accessibility is already load-bearing.** Status is never conveyed by colour
  alone (filled dot vs hollow ring), focus rings are explicit, small text uses a
  darker muted token to hold 4.5:1. Do not regress any of this for aesthetics.

## Tone

Precise and unadorned. The copy voice of good infrastructure docs: state the
fact, name the consequence, stop. No exclamation marks, no "Oops!", no
encouragement. An error says what failed and what to do next.

Empty states teach. "No agents registered" is honest but useless on its own;
the console should say how one gets registered.

## Anti-references

What this must not look like:

- **Generic SaaS admin.** Tailwind slate-50 background, indigo-600 primary,
  rounded cards with drop shadows, a gradient logo tile. This is precisely what
  the console looked like before, and it reads as a template rather than a tool.
- **Consumer dashboard.** Big hero metrics, sparkline decoration, celebratory
  colour. Nobody is delighted to be here; they are working.
- **Grafana pastiche.** Dark-by-reflex, neon-on-black, chart chrome everywhere.
  Avoiding SaaS-cream by falling into observability-dark is the same mistake one
  tier down.

## Strategic principles

1. **The fact is the interface.** An agent's id, its health, its model. Chrome
   that competes with data is wrong.
2. **Legibility beats expression.** Where the two conflict, legibility wins;
   this is a tool people read under pressure.
3. **Earned familiarity.** Standard nav, standard tables, standard forms. Novel
   affordances cost the user attention they wanted to spend elsewhere.
4. **Honest about state.** Degraded, unknown, and empty are first-class states,
   not afterthoughts. A control plane that hides uncertainty is worse than one
   that admits it.
