# AgentCore Gap-Closure Plan (post-v1.0)

**Date:** 2026-07-18 (updated 2026-07-19)
**Input:** gap analysis of `agentcore.md` vs the shipped v1.0 platform.
**Scope decision:** Payments (x402) is explicitly OUT — not needed for an
on-prem platform.
**Convention:** each milestone below is its own brainstorm → spec → plan →
execute cycle (the M1–M3 flow). This doc is the prioritized parking lot; specs
land in `docs/superpowers/specs/` as each item starts.

## Status (2026-07-19)

**Phase P1 "Guarded" — COMPLETE (all 3 items merged to master).**

- **P1.2 Lifecycle guardrails — DONE (merged).** turn/session timeouts, max_turns,
  max_tokens enforced durably in `agentruntime`; terminal status
  `limit_exceeded`; metric `agent_session_limit_hits_total`. Branch
  `p1.2-lifecycle-guardrails`.
- **P1.1 Cedar policy engine (M1+M2) — DONE (merged).** Deterministic per-tool-call
  authorization at the gateway (cedar-go): permit-by-default, platform
  (file) + tenant (DB) layers, `/admin/policies` + CLI + console CRUD,
  metric `runtime_gateway_policy_decisions_total`. Branch
  `p1.1-cedar-policy` (contains P1.2). A final review caught + fixed a real
  `__entity`-escape authorization bypass — see the spec.
- **P1.3 Metering + alerting (M1+M2) — DONE (merged 2026-07-19, ff to `2d3f86c`,
  11 commits, branch `p1.3-metering-alerting`).** M1 metering: `pricing:` block
  in runtime.yaml → per-model dollar cost; injected per-agent as
  `RUNTIME_AGENT_PRICING` (mirrors `RUNTIME_AGENT_LIMITS`); metrics
  `agent_cost_usd_total` + `agent_cost_unpriced_total` and tenant/model labels
  on `agent_tokens_total`; per-session `tokens_total`/`cost_usd` persisted to the
  sessions table (idempotent replay-safe projection of checkpointed usage) and
  surfaced in the session API + console. Fail-closed only on malformed pricing;
  unknown model → tokens flow, cost skipped + flagged. Cost includes cache
  tokens (deliberately wider than the P1.2 max_tokens budget). M2 alerting:
  turnkey Alertmanager compose overlay (:9093) + Prometheus rule_files/alerting
  wiring + 7-rule starter set (incl. UnpricedModelUsage + commented CostBurnHigh)
  + null receiver w/ commented Slack/PagerDuty + v1-proof assertions. Final
  whole-branch review MERGE-READY, no Critical/Important. Deferred (Docker
  unavailable in dev env): live v1-proof alerting run + promtool/amtool lint.

**Phase P2 "Scoped" (v1.2) — IN PROGRESS. First-milestone sweep COMPLETE: all
three P2 sub-projects now have their first milestone merged (P2.3 DONE; P2.1 M1
DONE; P2.2 M1 DONE). Deeper milestones (M2+) remain across the phase.**

- **P2.3 Gateway quotas + enrichment — DONE (merged 2026-07-19, ff to `5ba6232`,
  11 commits, branch `p2.3-gateway-quotas-enrichment`).** Quotas: new
  `internal/quota` package (store + token-bucket limiter, most-specific-wins
  resolution `(T,U)→(T,*)→(*,U)→(*,*)`, live reload via generation + 2s refresh
  throttle, fail-open). Gate #4 in `gateway.toolHandler` (after policy, before
  tenant injection): superuser-exempt, open-mode-skipped, rejects with MCP tool
  error `quota exceeded: T/U (retry after Ns)`, metric
  `runtime_gateway_quota_rejections_total{tenant,server}`. Full admin surface:
  `quotas:` config + `RUNTIME_GATEWAY_QUOTA_DEFAULT` env floor + `/admin/quotas`
  (RBAC: `*` superuser-only) + `runtimectl admin quota` + DB→live-limiter seed +
  idle reaper. Enrichment: per-upstream `enrich:` map (fixed vocab
  tenant|subject|role → outbound header), **OpenAPI-only** (MCP-over-HTTP sets
  headers once at connect), injected per-call from the calling principal,
  platform claims overwrite caller headers, cred/static collision = load error,
  `X-Runtime-` convention = load warning. Fail-OPEN (availability control) —
  deliberately asymmetric to P1.1 policy fail-closed. Reviews caught: limiter
  per-call DB query (throttle fix), quota RBAC guard unreachable via force-pin
  (pass-through fix), and a final-review outage-path throttle gap — all fixed.
- **P2.1 OBO / OAuth2 outbound creds — M1 (client_credentials) DONE (merged,
  branch `p2.1-oauth2-outbound-credentials`).** M1: `oauth2_client_credentials`
  brokered secret type minted/cached/auto-refreshed by the platform; created via
  `runtimectl admin secret set-oauth2` (+ `/admin/secrets` + console); referenced
  by an OpenAPI upstream through the existing `cred_secret`/`cred_header` (value
  becomes `Bearer <token>`, header defaults to `Authorization`). **OpenAPI-only**
  (rejected at registration + fatal at startup for file-config + refused at dial),
  **fail-closed** (`credential unavailable: <name>` + new metric
  `runtime_gateway_credential_errors_total{tenant,server}`) — deliberately the
  opposite of the fail-open quota limiter, since a credential is a security
  control. `client_secret` is write-only (never in list/API/console/logs); live
  rotation via re-run without restart. Depends on P1.1 principal-on-path.
  **M2 (OBO / RFC 8693 user-token exchange)** and **M3 (IdP connector presets)**
  remain.
- **P2.2 Memory strategies — M1 (strategy pipeline + summary) DONE (merged,
  branch `p2.2-memory-strategies`).** M1 generalizes `internal/memory/ingest.go`
  into a strategy pipeline and ships the first non-fact strategy: a per-session
  **rolling summary** — a running conversation digest regenerated each completed
  turn, stored durably and re-injected when the session resumes (in-memory thread
  gone, DB summary survives). Unlike semantic recall/facts it is
  **embedder-independent** (keyed by session, not vector similarity — works with
  no embeddings configured) and **tenant-scoped**. Enabled by
  `RUNTIME_SUMMARY_ENABLED` (independent of `RUNTIME_INGEST_ENABLED`), with
  `RUNTIME_SUMMARY_MODEL` (falls back to `RUNTIME_INGEST_MODEL`) and
  `RUNTIME_SUMMARY_MIN_MESSAGES`; still gated by per-agent `memory: true`.
  Best-effort (a summarizer failure or empty digest skips the write, never breaks
  a turn); one extra summarization LLM call per turn (known M1 cost, smarter
  cadence deferred). Metric `agent_memory_summary_writes_total{agent,tenant,model}`.
  **M2 (preference + `actor_id` namespacing)**, **M3 (episodic)**, and
  **M4 (TTL/GC + dedup hardening)** remain.

## Prioritization principles

1. **Security posture first.** The one AgentCore claim we cannot currently make
   is "the LLM is never the authorization arbiter." Closing that changes what
   the platform *is*; everything else is additive.
2. **Small-but-load-bearing before big-but-separable.** Lifecycle guardrails
   are days of work and cap the worst-case blast radius of every future
   feature. Evaluations is a whole pillar but bolts on beside the platform.
3. **Extend, don't fork, the v1.1 backlog.** Token accounting, alerting,
   gateway OAuth2/quotas, Memory TTL/GC were already deferred to v1.1+; this
   plan sequences them rather than re-inventing them.
4. **Demand-driven tail.** A2A/AG-UI/WebSockets and Nova-Act-style browsing
   wait for a real use case.

## Phase P1 — "Guarded" (target: v1.1)

The theme: deterministic control over what agents may do and spend.

### P1.1 Policy engine at the gateway boundary (Cedar)

**The gap:** AgentCore Policy intercepts every tool call at the gateway and
evaluates deterministic Cedar rules (parameter-level forbids, default-deny)
before the call reaches the upstream. We have tenant filtering + RBAC (which
tenant sees which upstream) but no per-call parameter inspection.

**Why P1:** it is AgentCore's central security thesis and our biggest
credibility gap; and we already own the exact choke point — every federated
tool call flows through `internal/gateway/server.go` before routing to an
upstream. `github.com/cedar-policy/cedar-go` is the official Go
implementation; evaluation is in-process and sub-millisecond, consistent with
the gateway's latency posture.

**Design sketch:**
- New `internal/policy` package wrapping cedar-go: load/validate policy sets,
  evaluate `(principal, action, resource, context)`.
- Principal = authenticated service key / user identity (already on the
  request via identity middleware); Action = gateway tool name
  (`<server>__<tool>`); Resource = upstream id; Context = the tool-call
  arguments JSON + tenant + request metadata.
- Enforcement point: the gateway Handler's tool-call path, after tenant
  filtering, before upstream dispatch. Default-permit when no policy engine is
  configured (byte-for-byte today's behavior); default-deny-on-forbid when on.
- Policy storage: per-tenant policy sets in a new `gateway_policies` table
  (DB atop optional file config — same pattern as `gateway_upstreams`),
  managed via `/admin/policies` + `runtimectl admin policy` + console panel
  (reuse the v1.0-M1 shared-helper pattern).
- Every decision (permit/forbid + policy id) goes to the audit log and a
  `runtime_gateway_policy_decisions_total{tenant,decision}` metric. Denials
  return a structured MCP tool error so the agent can adapt.

**Milestones:**
- **M1 — enforcement core:** cedar-go integration, file-configured policies,
  gateway interception, deny path proven live (a policy forbids
  `sandbox__run_code` when the code contains a marker string — the
  parameter-level demo).
- **M2 — self-service policies:** DB-backed per-tenant policy CRUD via
  admin API/CLI/console; validation-on-write (reject unparseable Cedar).
- **M3 — response-side controls:** post-call redaction rules (regex/field
  strippers on tool results before they re-enter the LLM context) — the
  AgentCore "response interceptor + PII redaction" analog.

**Dependencies:** none. **Not in scope:** Lambda-style arbitrary interceptor
code (P2.3 covers enrichment differently).

### P1.2 Lifecycle guardrails

**The gap:** no `execution_timeout`, `max_idle_time`, or turn/handoff caps on
agent runs; runaway-loop and token-burn protection relies on the harness loop
behaving.

**Why P1:** smallest item on the list with the highest safety-per-line; every
later feature (evaluations, A2A) assumes bounded executions exist.

**Design sketch:**
- Per-agent `limits:` block in `runtime.yaml` (`turn_timeout`,
  `session_idle_ttl`, `max_turns_per_session`, `max_tokens_per_session`)
  with platform-wide env-var defaults; validated at config load like
  `autoscale:`.
- Turn timeout enforced in `agentruntime/turnstep.go` via context deadline
  around the harness `RunTurn` — the turn checkpoint records a structured
  `timeout` terminal event (durability preserved: a timed-out turn is a
  *completed* turn with an error outcome, never a dangling workflow).
- Idle TTL + turn/token budgets enforced in `agentruntime/serve.go` (the
  Manager owns per-session state; token counts already flow through the
  metrics hook — tee them into the session row).
- Exceeded budgets end the session with a distinct `error` event code +
  `runtime_session_limit_hits_total{agent,limit}` metric.

**Milestones:** single milestone (config + enforcement + integration test that
a deliberately-looping test agent is cut off, with DBOS resume semantics
verified).

**Dependencies:** none. `max_handoffs` deferred until A2A exists (P4.1).

### P1.3 Token/cost metering + alerting

**The gap:** `agent_tokens_total` exists per agent, but there is no
per-tenant/per-session attribution, no cost view, no alerting. (Both already
named in the v1.1 deferred list.)

**Design sketch:**
- Add `tenant` label to the token/turn/tool metrics (cardinality is bounded:
  tenants are an admin-created set); persist per-session token totals on the
  session row (needed by P1.2's budget anyway — build once).
- Price table (`RUNTIME_PRICING` yaml/env: model → $/Mtok in/out) → derived
  cost metric + a per-tenant usage panel in the console and
  `runtimectl admin usage`.
- Alerting = shipped Prometheus alert rules in the compose/Helm overlays
  (agent down, restart-looping, error-rate, budget-burn) + Alertmanager in
  `deploy/compose/`. No bespoke alerting engine.

**Milestones:** M1 attribution (labels + session totals + console/CLI usage
view), M2 alerting overlay (rules + Alertmanager + runbook doc).

**Dependencies:** shares session-token plumbing with P1.2 — build P1.2 first
or together.

## Phase P2 — "Scoped" (target: v1.2)

The theme: per-user identity depth and richer memory.

### P2.1 OBO / user-scoped outbound credentials (gateway OAuth2)

**The gap:** agents act with tenant-level brokered secrets regardless of which
user invoked them. AgentCore exchanges the user's inbound JWT for downstream
OAuth tokens scoped to that user.

**Design sketch:**
- Extend the secrets broker with a credential *type*: `static` (today) vs
  `oauth2` (client credentials first, then token-exchange/refresh flows).
  OAuth2 creds store client id/secret + token endpoint; broker mints/caches/
  refreshes access tokens and injects them at gateway dial time (the
  copy-on-write header injection from v1.0-M1 already exists).
- True OBO (RFC 8693 token exchange of the caller's subject token) rides on
  that: the gateway tool-call path carries the authenticated principal
  (P1.1 plumbs this) so per-user token acquisition keys off it, cached
  per (tenant, user, upstream).
- Per-user permission = whatever the downstream IdP grants that user; we do
  not fake it with a service account.

**Milestones:** M1 oauth2 client-credentials upstream creds (closes the
backlogged "gateway OAuth2"), M2 OBO token exchange keyed on the calling user,
M3 IdP connector presets (config templates for common providers — docs +
validation, not code per provider).

**Dependencies:** P1.1 M1 (principal available on the gateway call path).

### P2.2 Memory strategies: summary, preference, episodic + user namespacing

**The gap:** we have the semantic strategy (pgvector + LLM fact ingest + KG
recall). Missing: summary, preference, and episodic extraction; and memory is
tenant-scoped where AgentCore namespaces to `actor_id`/`user_id`.

**Design sketch:**
- Generalize `internal/memory/ingest.go` into a strategy pipeline: each
  strategy = an extraction prompt + a record kind + a recall rule, run by the
  same background ingester over `session_events`. `memory_events` gains
  `kind` (`fact|summary|preference|episode`) and `actor_id` columns
  (append-only schema addition, backward-compatible).
- **Summary:** per-session rolling digest, recalled when a session's context
  pressure warrants (also feeds harness compaction).
- **Preference:** extraction targets explicit user rules/format wishes;
  recalled into the system prompt on session start for the same actor.
- **Episodic:** on session end, record goal → tool sequence → outcome;
  recalled by semantic similarity to the incoming task.
- Namespacing: recall keys on `(tenant, actor_id?)` — actor optional so
  today's tenant-wide behavior is the default. Fold in the backlogged
  Memory TTL/GC as retention-per-kind while touching the schema.

**Milestones:** M1 strategy pipeline + summary, M2 preference + actor
namespacing, M3 episodic, M4 TTL/GC + dedup hardening (backlog item).

**Dependencies:** none hard; P1.3's session totals make summary triggers
smarter.

### P2.3 Gateway traffic management: quotas + enrichment hooks

**The gap:** no rate limits/quotas, no request/response transformation hooks.

**Design sketch:**
- Per-tenant and per-upstream quotas (requests/min, concurrent calls) enforced
  in the gateway Handler; token-bucket in-process, config via the same
  admin surface as P1.1 M2. (Backlogged "gateway quotas".)
- Enrichment: rather than arbitrary Lambda interceptors, a declarative
  header/context enrichment config per upstream (inject caller tenant/user/
  clearance claims) — the 80% use case with zero code execution. Response-side
  filtering already lands as P1.1 M3 redaction.

**Milestones:** single milestone (quotas + enrichment + 429 semantics +
metrics).

**Dependencies:** P1.1 M2 (shared policy/config admin surface).

## Phase P3 — "Measured" (target: v1.3)

### P3.1 Evaluations pillar

**The gap:** no LLM-as-judge, no batch regression, no online sampling, no A/B.
Entirely absent, but the most separable: `session_events` already durably
records every turn, so evaluation is a consumer beside the platform, not a
change inside it.

**Design sketch:**
- New `cmd/evald` + `internal/eval`: an evaluation runner that replays or
  samples sessions and scores them with (a) LLM-as-judge rubrics (any
  harness provider) and (b) deterministic Go/regex assertions.
- **Golden sets:** curate from real sessions (`runtimectl eval curate
  <session>`); stored in an `eval_cases` table; batch runs invoke a target
  agent via the normal public contract — no bypass.
- **Online:** sample N% of completed sessions (`session_events` tail),
  score async, expose `runtime_eval_score{agent,dimension}` + alert on
  regression (rides P1.3's Alertmanager).
- **A/B:** registry-level traffic split across two agent ids for new sessions
  (round-robin already exists per-replica; lift to weighted per-variant),
  comparative eval scores + token cost per variant in the console.
- Failure-bucket classification (the "Online Insights" analog) = an
  LLM-as-judge rubric over failed sessions, emitting a `failure_class` —
  a rubric, not a new engine.

**Milestones:** M1 batch eval vs golden sets, M2 online sampling + regression
alerts, M3 A/B splitting + comparison, M4 failure classification.

**Dependencies:** P1.3 (cost per variant), P1.2 (bounded runs).

### P3.2 Isolation hardening (the honest microVM answer)

**The gap:** AgentCore isolates per *session* (microVM); we isolate per agent
replica — sessions of one agent/tenant share an `agentd` process. Full
per-session microVMs would break the durability model (a session pins to its
owner replica's DBOS executor) and is not the right trade here. Close the
practical exposure instead:

- Session-scoped (not tenant-scoped) sandbox/browser containers as an opt-in
  (`RUNTIME_SANDBOX_SCOPE=session`) — cross-session contamination in tools is
  the real risk surface, and containers are already per-use disposable.
- gVisor (`runsc`) documented + wired as the recommended sandbox runtime in
  compose/Helm (env already exists: `RUNTIME_SANDBOX_RUNTIME`).
- `replicas: N` + P1.2 idle TTLs as the "one session per process" pattern for
  tenants that demand it; document the posture honestly in the operator guide.

**Milestones:** single milestone (session scoping + docs + live proof).

## Phase P4 — demand-driven tail (no version target)

Picked up only when a concrete use case lands; recorded so they aren't lost.

- **P4.1 A2A (agent-to-agent):** an agent invokes another agent's public
  contract as a gateway tool (`agent__<id>__invoke`) with delegation depth
  capped by P1.2's guardrails (`max_handoffs` finally becomes meaningful).
  The contract already makes every agent addressable; this is mostly gateway
  plumbing + loop detection.
- **P4.2 AG-UI / richer streaming:** current SSE event stream already carries
  text/tool_result/done/error; extend event vocabulary (reasoning, ui-hint)
  only when a frontend consumer needs it. WebSockets/speech: out until a
  telephony-class use case exists.
- **P4.3 Built-in tool deltas:** managed web-search gateway upstream (one
  OpenAPI registration away — doc recipe first); sandbox data-science preset
  image + pip/kernel persistence (backlogged); browser Live View
  (CDP screencast over SSE/WS) + session recording for audit.
- **P4.4 Backlog carried unchanged:** K8s operator/CRDs, mTLS, C1 PydanticAI,
  non-onboarding console panels, clean-Linux capstone rerun.

## Sequence summary

| Order | Item | Size | Unlocks |
|---|---|---|---|
| 1 | P1.2 Lifecycle guardrails | S | safety floor for everything |
| 2 | P1.1 Cedar policy M1–M3 | M | the defense-in-depth story |
| 3 | P1.3 Metering + alerting | S–M | cost attribution, eval A/B, ops |
| 4 | P2.1 OAuth2/OBO | M | user-scoped outbound auth |
| 5 | P2.2 Memory strategies | M | preference/summary/episodic + actor scoping |
| 6 | P2.3 Gateway quotas/enrichment | S | traffic management |
| 7 | P3.1 Evaluations | L | quality engineering pillar |
| 8 | P3.2 Isolation hardening | S | honest session-isolation posture |
| 9 | P4.x | — | demand-driven |

P1.2 → P1.1 → P1.3 is the recommended immediate order: guardrails are the
quickest win; the policy engine is the flagship gap; metering completes the
"guarded" theme and feeds evaluations later. P1 as a whole is the v1.1 cut.
