# C2 M2 — Per-agent-pod scheduling (design)

**Date:** 2026-06-13
**Sub-project:** C2 (Containers / Kubernetes), milestone 2
**Status:** design approved; ready for plan
**Builds on:** C2 M1 (container image + Helm chart, monolith pod), C3 M1 (remote
agents — attach instead of spawn), Spine A1 (replica pools / session affinity /
per-replica executor ids), Spine A2 (autoscaling — the decision/actuator split).

---

## 1. Goal

One sentence: **let each agent run as its own Kubernetes-scheduled pod (a
StatefulSet) that runtimed attaches to as a remote replica pool, instead of the
C2 M1 monolith where runtimed exec-spawns every agent as a child in its own
process tree.**

This closes the "K8s later" half of the original deploy decision: foreign
agents (C1) and harness-native agents alike become first-class scheduled
workloads, each independently schedulable, restartable, and (in a later
milestone) autoscalable by Kubernetes.

## 2. The core realization

A per-agent **StatefulSet** attaches to runtimed as a **remote replica pool** —
which is the *union of two capabilities runtimed already has*, with almost no
new control logic:

- **C3 M1 (remote)** gave attach-instead-of-spawn: an agent with `url:` is
  health-checked + reverse-proxied + status-reported, never spawned, never
  restarted (a `HealthMonitor`, not a `Supervisor`). The data plane is already
  location-agnostic.
- **Spine A1 (pool)** gave the ordered replica set with **per-session affinity**
  (the `sessions.replica` column pins a session to ordinal *i* for life, because
  only the owning DBOS executor can resume that session's durable workflow) and
  the **executor-id model** (`DBOS__VMID=<id>#<i>`).

**C2 M2 = C3-remote × A1-pool.** runtimed attaches to N remote ordinals at
stable per-ordinal DNS, round-robins new sessions across the reachable ones, and
pins each session to its ordinal's DNS for life — exactly as A1 does locally,
now across the network.

### Why StatefulSet (not Deployment)

A StatefulSet makes Kubernetes **enforce A1/A2's invariants for free**:

- **Ordinals = executor ids.** StatefulSet pods are named `<sts>-0`, `<sts>-1`,
  … in order; ordinal *i* maps one-to-one onto `DBOS__VMID=<id>#<i>`.
- **Stable per-ordinal DNS.** A headless Service gives each ordinal a fixed DNS
  name (`<id>-<i>.<headless-svc>.<ns>.svc.cluster.local`), so runtimed has a
  durable dial target per executor — required for affinity.
- **Suffix-only scale-down is a K8s guarantee.** A StatefulSet always removes
  the **highest-ordinal** pod first on scale-down. That is *precisely* A2's
  suffix-only / drain-from-the-top invariant — now provided by the orchestrator
  instead of enforced by `PoolManager`.

A Deployment gives pods random identities and a load-balanced Service, which
breaks affinity and the executor-id model the moment an agent runs more than one
replica. StatefulSet is the correct primitive.

## 3. Scope (and non-scope)

**In scope (this milestone):**

1. Config: a **remote agent may be a pool** — `url:` with an `{i}` ordinal
   placeholder + `replicas: N` expands to N per-ordinal remote attach entries.
2. Routing: **liveness-aware round-robin** for new sessions over a remote pool
   (skip ordinals whose health probe says unreachable); affinity routing
   unchanged.
3. Helm: a `scheduling.mode: monolith | perAgentPods` toggle. `perAgentPods`
   renders one StatefulSet + headless Service per agent (agentd-only pods) and a
   **control-plane-only** runtimed whose `runtime.yaml` is generated from the
   same `config.agents` list, rewriting each agent into a remote pool.

**Explicitly NOT in scope** (deferred, kept out so this stays a packaging +
attach milestone):

- **runtimed-driven scaling via the K8s API** — runtimed gaining a Kubernetes
  client to grow/shrink StatefulSets (the A2 "signal-only actuator" seam against
  the cluster). This is its own milestone (a clean C2 M3 candidate).
- **HPA on the agent StatefulSets** and a **Kubernetes operator / CRDs**.
- **Brokered secrets delivery to scheduled pods** (see §7) — documented
  limitation; natural home is C3 M2's registration handshake.
- **Dynamic scale-up discovery** — runtimed learns a higher replica count only
  on `helm upgrade`, not by watching the cluster (the honest seam of the
  static-count decision, §6).

## 4. Architecture

```
                 ┌────────────────────────────────────────────┐
                 │  runtimed pod (Deployment, replicas:1,       │
                 │  Recreate) — CONTROL PLANE ONLY in           │
                 │  perAgentPods mode: never exec-spawns.       │
                 │  runtime.yaml (generated): each agent is a   │
                 │  remote pool → url with {i}, replicas, bearer│
                 └───────┬───────────────────────┬─────────────┘
                         │ reverse-proxy +        │
                         │ HealthMonitor/ordinal  │
            ┌────────────▼──────────┐  ┌──────────▼────────────┐
            │ StatefulSet agent A   │  │ StatefulSet agent B   │
            │ headless Service A    │  │ headless Service B     │
            │  A-0  A-1  A-2        │  │  B-0                   │
            │  (agentd-only pods,   │  │  (agentd-only pod,     │
            │   DBOS__VMID=A#i)     │  │   DBOS__VMID=B#0)      │
            └───────────────────────┘  └────────────────────────┘
                         │                        │
                         └────────┬───────────────┘
                                  ▼
                       shared Postgres (DBOS + stores)
```

**Two chart modes, one chart** (`scheduling.mode`):

- `monolith` (default): C2 M1 verbatim — one pod, runtimed exec-spawns agentd
  children, agents have `listen_addr`. Every existing template renders exactly
  as today; the new per-agent templates are gated off. Zero drift for existing
  users.
- `perAgentPods`: the topology above.

**What does NOT change:**

- `PoolManager` (A2) stays **local-autoscale only**. A remote pool's replica
  count is config-pinned from runtimed's view; Kubernetes mutates the actual
  pods. No PoolManager is constructed for a remote agent.
- The data plane: reverse-proxy, `/healthz`, the `sessions.replica` affinity
  column, the durable workflow model.
- The monolith path, end to end.

## 5. Config schema — the remote-pool extension

Today (C3 M1) a remote agent is a **single** attach entry: one `url:`,
`ReplicaIndex 0`, and `Validate()` rejects `replicas` on remote agents. C2 M2
lifts that one rule for the StatefulSet case, letting a remote agent be a **pool
of ordinals**.

```yaml
agents:
  - id: support
    name: Support Agent
    model: claude-opus-4-8
    tenant: acme
    url: "http://support-{i}.support-hl.runtime.svc.cluster.local:8080"
    replicas: 3
    auth_token: "${SUPPORT_AGENT_TOKEN}"
```

**Rules (in `config.Validate()`):**

- **`{i}` placeholder** is **required** when `url:` is set *and* `replicas > 1`;
  **forbidden** when `replicas <= 1`. A single remote (`replicas` 0/1, no `{i}`)
  stays exactly C3 M1 — byte-for-byte, so the hand-run single-remote case is
  untouched.
- **`replicas` on a remote agent is now allowed** (the lifted rule) — but only
  together with a `{i}`-templated `url:`. The other spawn-time fields
  (`command`, `workdir`, `kind`, `memory`, `gateway`, `autoscale`) remain
  **rejected** on a remote agent, unchanged from C3 M1.
- **Dial-uniqueness:** each expanded ordinal URL (`{i}` → `0..replicas-1`) is
  checked unique across all agents, the same way A1 checks derived local ports.

**Expansion (mirrors A1's `ReplicaAddrs`):**

- New method `AgentConfig.RemoteReplicaURL(i int) (string, error)` substitutes
  `{i}` with the ordinal. (Errors if `{i}` is absent when required, or if `i`
  is out of `0..replicas-1`.)
- `NewRegistry` builds a remote **set** of N `AgentProcess` entries for a remote
  pool (each `Remote: true`, `BaseURL:` the expanded per-ordinal URL,
  `AuthToken:`, `ReplicaIndex: i`) — reusing the existing
  `r.sets[id] = []AgentProcess{…}` slice path that local A1 pools already
  populate. A remote pool is a **static set** from runtimed's view (K8s owns
  mutation); **no `PoolManager`** is involved.

**Consequence — routing/affinity code needs zero changes.** A remote pool is
just an `r.sets[id]` of length N. `Replicas`, `Replica`, the `sessions.replica`
pin, and the reverse-proxy already handle a set of that shape. The only new
routing behavior is liveness-aware selection (§6).

## 6. Routing, liveness, and lifecycle

**The one new runtime behavior: liveness-aware round-robin.**

Today `Registry.NextReplica` for a static set is liveness-blind
(`n % len(set)`) — acceptable for local A1 where supervised replicas are assumed
up. For a remote StatefulSet, Kubernetes can scale the pool down underneath
runtimed, so top ordinals vanish. The fix mirrors what `PoolManager.NextReplica`
already does for draining replicas (it skips them):

- **New-session routing (`NextReplica`):** round-robin but **skip ordinals whose
  latest health probe says unreachable**; if every ordinal is unreachable, fall
  back to ordinal 0 (matches `PoolManager.NextReplica`'s all-draining fallback —
  the reverse-proxy then returns 503, the honest C3 behavior).
- **Existing-session routing (affinity): unchanged.** A session pinned to
  ordinal *i* always proxies to `<id>-i`'s DNS. If that pod is down, the proxy
  returns 503 until it returns — identical to A1's "owner-down ⇒ 503" and C3's
  remote-down semantics. **Durability is absolute: a session is never re-pinned
  to a different executor.**

**Liveness source — a reachable bitmap fed by `HealthMonitor`:**

- Each remote ordinal gets a `HealthMonitor` (C3 M1's existing type — poll
  `/healthz`, edge-triggered `OnChange`, **never restart**), one per ordinal.
  The monitor already drives `runtime_agent_reachable{agent,replica}`.
- Add a per-agent, per-replica **reachable bitmap** the registry reads in
  `NextReplica`. The monitor's `OnChange` callback flips the bit; `NextReplica`
  reads it under the existing lock. New ordinals start "unknown" — treated as
  reachable until the first probe completes (so boot doesn't 503 the whole pool
  during the initial probe delay; the immediate first probe in `HealthMonitor`
  corrects quickly).

**Lifecycle (per ordinal, all C3 M1 semantics):**

- No `Supervisor`, no restart — Kubernetes owns the pod. A `HealthMonitor` per
  ordinal instead.
- **Scaled-down StatefulSet** → top ordinals go unreachable → skipped for new
  sessions; any session still pinned to a removed ordinal gets 503 until that
  session completes. No runtimed error, no boot block (degrade-don't-fail).
- **Scaled-up beyond the config count** → new pods exist but runtimed doesn't
  know their DNS → picked up on `helm upgrade` (re-render `runtime.yaml` with the
  higher `replicas:`, runtimed re-reads). The honest, documented seam of the
  static-count decision.

**agentd bearer:** each StatefulSet pod runs `agentd` with
`RUNTIME_AGENT_AUTH_TOKEN` (C3 M1's existing optional constant-time-compare
middleware); runtimed's remote entries carry the matching `AuthToken`. The chart
wires both sides from one value.

**Net new runtime code is small and local:** the config rule lift + remote-pool
expansion (§5), plus skip-unreachable in `NextReplica` backed by a reachable
bitmap the `HealthMonitor` already feeds. Everything else is reuse.

## 7. Env / secrets delivery

Per-agent pods are scheduled by Kubernetes, not spawned by runtimed, so
runtimed's spawn-time env injection does not apply. **The Helm chart owns env
provisioning at render time** — the same values runtimed would have injected,
now baked into each StatefulSet's pod spec. This is consistent with C2 M1 (the
chart provisions the monolith's env) and C3 M1's "operator-provisioned remote
env" decision; no registration handshake is needed.

**Per-agent-pod env contract:**

| Env var | Source | Notes |
|---|---|---|
| `RUNTIME_PG_DSN` | chart Secret (shared) | same DSN all agents share, as M1 |
| `RUNTIME_LISTEN_ADDR` | fixed `:8080` | the pod's own port; headless DNS targets it |
| `RUNTIME_AGENT_ID` | per-agent | the agent's id |
| `RUNTIME_AGENT_TENANT` | per-agent | from `config.agents[].tenant` |
| `RUNTIME_AGENT_MEMORY` | per-agent | memory opt-in (chart value) |
| `DBOS__VMID` | `<id>#<ordinal>` | ordinal from pod name via downward API → init step; the A1 executor-id invariant, now set by the chart |
| `RUNTIME_AGENT_AUTH_TOKEN` | chart Secret, per-agent | C3 M1 bearer; matches runtimed's `auth_token:` |
| gateway URL + key | chart, if gateway-on | per-tenant key from `gateway.agent_keys` |

**Documented limitation — brokered secrets.** Identity M2 secrets brokering
injects decrypted per-tenant provider credentials at **spawn**. A Helm-rendered
pod that runtimed never spawns cannot receive brokered secrets that way. In this
milestone, per-agent-pod agents get provider credentials via the chart (a K8s
Secret → env, the operator's choice); runtimed's `Broker` stays a monolith-spawn
feature. Backlog: **"brokered-secrets delivery to scheduled pods"** — its
natural home is C3 M2's registration handshake, where a pod can pull decrypted
secrets over an authenticated channel. Most agents in Kubernetes use a
tenant-wide provider key from the chart Secret anyway.

## 8. Helm chart — the `perAgentPods` mode

One values toggle drives everything: `scheduling.mode: monolith | perAgentPods`
(default `monolith`, so C2 M1 is unchanged).

**When `monolith`:** every existing template renders exactly as today; the new
per-agent templates are gated off.

**When `perAgentPods`, the chart renders — per agent in `config.agents`:**

1. **A StatefulSet** (`<release>-agent-<id>`): runs the all-binaries image as
   **agentd-only** (`serviceName:` the headless Service, `replicas:` from the
   agent's count, `podManagementPolicy: Parallel` so ordinals don't serialize on
   boot). Pod env baked in at render (§7 table). Same secure pod posture as M1
   (runAsNonRoot, `readOnlyRootFilesystem`, `/tmp` emptyDir, drop ALL).
   `DBOS__VMID` is `<id>#$(ORDINAL)` where ORDINAL is derived from the pod name
   (downward API → init step → env), so each ordinal is a stable executor id.
2. **A headless Service** (`<release>-agent-<id>-hl`, `clusterIP: None`): gives
   stable per-ordinal DNS
   `<id>-<ordinal>.<release>-agent-<id>-hl.<ns>.svc.cluster.local`.

**And for the control plane (one runtimed pod):**

3. **runtimed Deployment** stays `replicas: 1`, `Recreate` (the single-writer
   rule still holds — runtimed is the control plane), but in this mode runs
   **control-plane-only**: it never exec-spawns. Its mounted `runtime.yaml` is
   **generated** from the same `config.agents` list, rewriting each agent from a
   local entry into a remote pool: `url:` = the `{i}`-templated headless DNS,
   `replicas:` = the agent's count, `auth_token:` = the per-agent bearer
   reference. Operators edit `config.agents` once; both the StatefulSets and
   runtimed's remote view derive from it — no manual `url:` wiring.

**Helpers / guards:**

- A `_helpers.tpl` function emits the per-ordinal DNS template string used in
  **both** the StatefulSet's `serviceName`/DNS and runtimed's generated `url:` —
  single source of truth, cannot drift.
- Render guards (learned from the C2 M1 final review): in `perAgentPods` mode,
  **fail-closed** if an agent has no replica count, if `config.agents` is empty,
  or (carried from M1) if there is no DSN source. Regression-guard the
  DSN-host / Service-name match that bit M1.
- Docker-dependent features (sandbox/browser) and the obs overlay are
  orthogonal and unchanged.

## 9. Error handling

All degrade-don't-fail, all inherited from C3 M1 / A1:

- A down or missing ordinal → `HealthMonitor` marks it unreachable → skipped for
  new sessions, 503 for any session still pinned to it. Never blocks runtimed
  boot.
- Scale drift **up** → invisible until `helm upgrade` (documented seam, §6).
- Scale drift **down** → handled live by skip-unreachable (no upgrade needed).
- A malformed generated `runtime.yaml` cannot fail silently — `config.Load`
  validates it exactly as a hand-written file, and the chart's render guards fail
  closed first.

## 10. Testing

**Unit (hermetic, `go test`):**

- **config** — remote-pool validation: `{i}` required iff `replicas > 1`,
  forbidden iff `replicas <= 1`; dial-uniqueness across expanded ordinal URLs;
  spawn-field rejection on remote unchanged; `RemoteReplicaURL(i)` expansion
  (happy path + out-of-range + missing-placeholder errors).
- **registry** — a remote pool builds N `AgentProcess` entries with correct
  per-ordinal `BaseURL` / `ReplicaIndex`; a single remote (no `{i}`) still
  builds the one-entry C3 set.
- **routing** — `NextReplica` skips unreachable ordinals; affinity routes a
  pinned session to its ordinal regardless of the round-robin cursor;
  all-unreachable falls back to 0; unknown (un-probed) ordinals are treated
  reachable.

**Integration (`//go:build integration`, Postgres.app at the standard DSN,
self-cleaning):**

- Extend the remote-attach integration test: a mixed local + remote-**pool**
  registry; round-robin distributes new sessions across ≥2 reachable ordinals;
  kill one ordinal → new sessions skip it while a session pinned to a *healthy*
  ordinal keeps working (affinity + durability across the pool, no double
  execution).

**Chart (`deploy/charts/runtime/test.sh` permutations + `helm template`):**

- `perAgentPods` renders one StatefulSet + one headless Service per agent + a
  control-plane-only runtimed whose generated `runtime.yaml` carries matching
  remote pools; the per-ordinal DNS template is identical on both sides (drift
  guard); fail-closed guards fire (no replica count / no agents / no DSN).
- `monolith` mode renders byte-for-byte as M1 (regression).

**Live proof (kind cluster, like C2 M1):**

- Install in `perAgentPods` mode with 2 agents (one at `replicas: 2`); confirm
  both StatefulSets reach Running; `runtimectl conformance` passes against the
  in-cluster control-plane Service (session create + stream + get + list routed
  through to per-agent pods); then `kubectl scale` one StatefulSet down by one
  and confirm new sessions skip the removed ordinal while runtimed stays
  healthy and `/agents` reports the agent reachable.

## 11. File structure (anticipated)

- `internal/config/config.go` — lift the remote `replicas` rule; add `{i}`
  validation + `RemoteReplicaURL(i)`; extend dial-uniqueness.
- `controlplane/registry.go` — build a remote **pool** set; add the reachable
  bitmap + liveness-aware `NextReplica`.
- `controlplane/monitor.go` — wire `OnChange` into the reachable bitmap (one
  `HealthMonitor` per remote ordinal).
- `cmd/runtimed/main.go` — start a `HealthMonitor` per remote-pool ordinal
  (today: one per single remote).
- `deploy/charts/runtime/`:
  - `values.yaml` — `scheduling.mode`; per-agent replica counts + tokens.
  - `templates/_helpers.tpl` — the per-ordinal DNS template function.
  - `templates/agent-statefulset.yaml`, `templates/agent-service.yaml` — new,
    gated on `perAgentPods`.
  - `templates/configmap.yaml` — generate the control-plane `runtime.yaml` from
    `config.agents` in `perAgentPods` mode.
  - `templates/deployment.yaml` — control-plane-only in `perAgentPods` mode.
  - `README.md` — `perAgentPods` quick-start + the brokered-secrets limitation.
- `test/` — extend the remote-attach integration test for pools.
- `ROADMAP.md`, `runtime.yaml` example — document M2.

## 12. Conventions (carried from M1–A2)

- The `go` CLI is ground truth; ignore IDE/LSP diagnostics from the
  `replace ../harness` cross-module setup.
- Integration tests: `//go:build integration`, `package test`, Postgres.app at
  `postgres://runtime:runtime@localhost:5432/runtime?sslmode=disable`, self-clean
  DB + `dbos` schema; scripted model `test/scripted` (no LLM key).
- gofmt-clean before commit; commit trailer
  `Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>`.
- Subagent-driven development: fresh subagent per task, two-stage review (spec
  then code quality), final holistic review; all subagents dispatched with the
  opus model.
- Chart testing has no CI (build-locally, manual push); `helm lint` / `helm
  template` / `test.sh` permutations are the hermetic gate.
