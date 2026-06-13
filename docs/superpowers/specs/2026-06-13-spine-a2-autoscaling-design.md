# Spine A2 — Autoscaling (Local Pool Grow/Drain by Load): Design Spec

**Date:** 2026-06-13
**Status:** Approved (brainstorm complete)
**Sub-project:** Spine (sub-project 1 of 6), hardening item A2

---

## Section 1 — Goal, Scope & the Two New Invariants

**Goal:** Let an autoscaled *local* agent float its replica count between `min`
and `max` at runtime, driven by active-session load, with **runtimed acting as
both controller and actuator** — growing the pool by spawning replica processes
and shrinking it by draining-then-stopping the highest replica, never sacrificing
a session.

A2 turns A1's *static* pool into a *dynamic* one. A1 explicitly deferred two
items this milestone now delivers:

- **Dynamic replica count** — change N without a process restart.
- **Graceful drain** — stop-new + finish-in-flight on planned replica shutdown
  (scoped here to scale-down).

Plus the policy loop that decides when to do either. A2 unblocks nothing further
on the spine; it is the payoff of A1's pool + per-replica metrics.

### The load-bearing inheritance — the executor-ID crux (from A1)

Each `agentd` is a DBOS executor; on launch it recovers *only* the workflows
stamped with its own executor ID (`DBOS__VMID`). A1 gives replica *i* of agent
`support` the stable ID `support#i`. The consequence that shapes all of A2: **a
session stamped `replica=i` can be resumed by exactly one executor, `support#i`,
and by no other.** Killing `support#i` while it owns live sessions orphans those
durable workflows *permanently* — and unlike A1's owner-down-503 case, in
scale-down **no restart at index `i` is ever coming back**. The two invariants
below fall directly out of this.

### Invariant 1 — Suffix-only mutation

Replicas are an ordered set `0..k-1`. A2 may only ever **add at index `k`** or
**remove index `k-1`**. A session stamped `replica=i` must always find executor
`agent#i` alive, so a *middle* replica can never be removed. Scale-up appends;
scale-down peels from the top. The round-robin counter and all routing already
assume a contiguous `0..k-1` range (A1), so suffix-only keeps that contiguity.

### Invariant 2 — Drain-before-stop is absolute

The highest replica is stopped only once its active-session count reaches **0**.
Until then it is marked *draining*: excluded from new-session round-robin but
**still serving** existing sessions and **still supervised** (so it restarts and
recovers its own work if it crashes mid-drain). A single long-lived session
blocks that one scale-down indefinitely. This is the accepted tradeoff: the only
alternative (a force-kill deadline) orphans a durable workflow forever, which a
durable platform must not do silently. Capacity is reclaimed *lazily* but
*safely*.

### In scope (A2)

- Opt-in `autoscale: {min, max, target_sessions_per_replica}` per local agent.
- A `controlplane.PoolManager` per autoscaled agent: the single owner of that
  agent's replica set, supervisor goroutines, and scale decisions, behind one
  mutex.
- An **active-sessions-per-replica** policy loop (one goroutine per PoolManager):
  read load from Postgres, compute desired count, take at most one step per tick,
  with separate scale-up / scale-down cooldowns.
- Suffix-only `grow()`; drain-only `drainTop()` + `reapDrained()`; an **un-drain
  fast path** (load rebounds during drain ⇒ clear the flag instead of spawning).
- `max`-range port reservation at config load (a grown replica always has a free
  derived port).
- Per-agent autoscale metrics (desired / current / active gauges + an events
  counter).
- Static (`autoscale`-absent) agents are **byte-for-byte A1** — no PoolManager,
  no policy loop, lock-free path untouched.

### Out of scope (deferred)

- **Scale-down deadline / force-kill** — we chose drain-only. A deadline knob is
  a future, loudly-flagged opt-in, not the A2 default.
- **Resource (CPU/mem) and throughput/latency signals** — agentd is
  LLM-I/O-bound; active sessions is the meaningful, cheap signal. Other signals
  are a later refinement.
- **Queue-based signals** — the data plane has no queue (round-robin dials
  immediately); introducing one is a separate change.
- **Cross-host / K8s actuation** — that is **C2 M2** (a scheduled pod is one
  replica whose lifecycle the orchestrator owns). A2's PoolManager boundary is
  precisely where a future *signal-only* mode (emit desired count, let K8s
  actuate) would slot in; A2 does not build it.
- **Autoscaling remote agents** — `autoscale:` is rejected on `url:` agents,
  exactly like `replicas:`.

### Framing

A1 built a static, immutable-after-construction pool: a fixed-size slice, ports
derived at boot, one Supervisor goroutine per replica, and a lock-free
`Registry` (everything written before serving). A2's whole job is to make *one
agent's* set mutable at runtime **without** disturbing that lock-free static
path — by moving the dynamic agent's set into a `PoolManager` that owns all the
concurrency, and having the Registry delegate to it. The router and metrics
fan-out keep calling the same `Registry` methods and never learn an agent is
autoscaled.

---

## Section 2 — Config & Port Reservation

### Config (`internal/config/config.go`)

`AgentConfig` gains an optional block:

```go
// AutoscaleConfig, when present on a local agent, makes its replica pool float
// between Min and Max driven by active-session load. Absent ⇒ static A1 pool.
type AutoscaleConfig struct {
    Min                      int `yaml:"min"`
    Max                      int `yaml:"max"`
    TargetSessionsPerReplica int `yaml:"target_sessions_per_replica"`
}

// On AgentConfig:
Autoscale *AutoscaleConfig `yaml:"autoscale"` // nil ⇒ static A1 behavior
```

### Interaction with A1's `replicas:`

The two are mutually clarifying, with one source of truth for size in each mode:

- `Autoscale == nil` ⇒ **byte-for-byte A1**: a static pool of `Replicas` (or 1).
  No PoolManager, no policy loop, nothing new spawned, the lock-free Registry
  path is used.
- `Autoscale != nil` ⇒ the pool floats `min..max`; the **initial count is
  `min`**. If `Replicas` is *also* set it is **ignored with a load-time
  warning**, so there is exactly one source of truth for "how big" in autoscale
  mode.

### Validation rules (`Validate()`)

- `1 ≤ Min ≤ Max`; `TargetSessionsPerReplica ≥ 1`. Violations rejected at load
  with a clear message.
- `Autoscale` rejected on a **remote** (`url:`) agent — joins the existing
  "spawn-time-only fields rejected on remote" check, exactly like `Replicas > 1`.
- **Port reservation (the `max` range).** A1 derives `base_port + i` and
  validates the derived set is collision-free across all local agents. A2, for an
  autoscaled agent, reserves the **whole `max` range**: every
  `base_port .. base_port + Max - 1` is inserted into the dial-uniqueness map,
  and the `base + Max - 1 ≤ 65535` overflow bound is checked. This guarantees a
  grown replica always has a free, non-colliding derived port — scaling never
  fails for want of a port. A static agent still reserves only its `Replicas`
  ports (unchanged).

### `ReplicaAddr(i)` — single-index derivation

A1's `ReplicaAddrs() ([]string, error)` returns exactly `Replicas` addresses. A2
adds a sibling the PoolManager uses to compute *one* replica's address when
growing:

```go
// ReplicaAddr returns the derived host:base_port+i listen address for replica i.
// Errors if the base addr is unparseable or the derived port is out of range.
func (a AgentConfig) ReplicaAddr(i int) (string, error)
```

`ReplicaAddrs()` is reimplemented in terms of `ReplicaAddr` (DRY). All port math
stays in `config`, where A1 put it; the PoolManager never invents addresses.

---

## Section 3 — The PoolManager

A new file `controlplane/poolmanager.go`. **One `PoolManager` per autoscaled
agent.** It is the single owner of that agent's replica set, its Supervisor
goroutines, and its scale decisions — all serialized by one mutex. This is the
"isolate all concurrency in one component" payoff: the static-agent path stays
lock-free; only the dynamic agent pays for a lock.

### State

```go
type PoolManager struct {
    mu       sync.RWMutex
    agentID  string
    base     AgentProcess           // template: tenant, broker, gateway, binpath, dsn, kind…
    acfg     config.AutoscaleConfig
    addrOf   func(i int) (string, error) // bound to the agent's config.ReplicaAddr
    replicas []replicaSlot          // ordered 0..k-1; len == live count
    rr       atomic.Uint64          // new-session round-robin (skips draining)

    // dependencies (constructor-injected)
    st       store.Store            // load read (active sessions per replica)
    metrics  *obs.ControlMetrics
    backoff  time.Duration          // supervisor restart backoff
    log      *slog.Logger
}

type replicaSlot struct {
    ap       AgentProcess           // ReplicaIndex, Addr, BaseURL, DBOSVMID all set
    cancel   context.CancelFunc     // stops THIS replica's Supervisor goroutine
    draining bool
}
```

`base` carries every agent-level field A1 stamps identically across a replica set
(tenant, broker, gateway wiring, binpath, dsn, kind/command/workdir, memory). A
grown replica is `base` with `ReplicaIndex`, `Addr`, `BaseURL`, and
`DBOSVMID = agentID#i` filled in — identical to how A1's `NewRegistry` builds a
set member.

### Lifecycle methods (the only ways the set changes — all suffix-only)

```go
// grow appends a replica at index k = len(replicas). It derives the addr via
// addrOf(k), sets DBOS__VMID=agentID#k, starts its Supervisor under a child of
// the manager context, waits until it answers /healthz, THEN publishes the slot
// into replicas under the lock. Spawn-then-publish: a half-started replica is
// never routable. Returns an error (logged, counted) if spawn/health fails; the
// set is left unchanged.
func (p *PoolManager) grow(ctx context.Context) error

// drainTop marks replicas[k-1].draining = true. New sessions stop routing there
// immediately (NextReplica skips it). Does NOT stop the process. No-op if the
// top is already draining or k == 0.
func (p *PoolManager) drainTop()

// undrainTop clears the draining flag on the top replica (the un-drain fast
// path). No-op if the top is not draining.
func (p *PoolManager) undrainTop()

// reapDrained stops and removes the CONTIGUOUS draining suffix whose active
// session count is 0: for each such top replica, call its cancel() (Supervisor
// stops, no restart) and truncate the slice. Only contiguous-from-top reaping
// preserves the suffix-only invariant. Called every tick; cheap; not a "scale
// event" and not cooldown-gated.
func (p *PoolManager) reapDrained(active map[int]int)
```

`grow` spawns-then-publishes; `reapDrained` is the only path that shrinks the
slice, and only from the top, only at 0 active — so the live range is always
contiguous `0..k-1` and Invariant 1 holds by construction.

### Read methods (the Registry/router need these; RLock)

```go
func (p *PoolManager) Replicas() []AgentProcess          // broker already on base
func (p *PoolManager) Replica(i int) (AgentProcess, bool)
func (p *PoolManager) NextReplica() int                  // round-robin over NON-draining replicas
```

`NextReplica` is the one behavioral upgrade over A1's blind atomic counter: it
round-robins over the **non-draining** replicas only. A draining replica must
never receive a *new* session (that would reset its drain). If *all* replicas are
draining (only possible transiently, since `min ≥ 1` keeps at least one live and
drainTop never drains below `min`), it falls back to index 0.

### Registry integration (`controlplane/registry.go`)

`Registry` gains `pools map[string]*PoolManager`. `NewRegistry` constructs a
`PoolManager` for each agent whose config has `Autoscale != nil` (and starts no
processes — main.go drives startup, see §5). For an autoscaled id, the existing
`Replicas` / `Replica` / `NextReplica` methods **delegate** to the PoolManager;
for a static id they use today's slice path unchanged:

```go
func (r *Registry) Replicas(id string) ([]AgentProcess, bool) {
    if p, ok := r.pools[id]; ok {
        return p.Replicas(), true
    }
    // ... existing static slice path, unchanged ...
}
```

`Get(id)` returns replica 0 either way. `SetBroker` / `SetGateway` must also
stamp the PoolManager's `base` template (so grown replicas inherit them) — they
iterate `r.pools` in addition to `r.sets`. Because the router (`api.go`) and the
metrics fan-out call only these Registry methods, **A1's routing and fan-out code
is untouched** — they never learn whether an agent is autoscaled.

---

## Section 4 — The Policy Loop

One goroutine per PoolManager (`PoolManager.runPolicy(ctx)`), started by main.go
alongside the initial replicas. Every `poll_interval` it runs one tick:

### Load read — Postgres is the source of truth

```sql
SELECT replica, count(*) FROM sessions
WHERE agent_id = $1 AND status NOT IN ('completed','error')
GROUP BY replica
```

A new `store.ActiveSessionsByReplica(ctx, agentID) (map[int]int, error)`. One
indexed query per tick. The `replica` column (A1) already exists; the session
status lifecycle is `created` → `running` → terminal (`completed` or `error`),
so the two terminal states above are the exhaustive exclusion set. A
non-terminal-by-exclusion filter (rather than `IN ('created','running')`) is
deliberate: if a future status is added, an unfinished session is counted as
active (fail toward keeping capacity), not silently dropped. **No cross-process gauge, no agentd cooperation** — runtimed reads
the shared DB directly. From the result: `active = Σ counts`, and the per-replica
map feeds `reapDrained` (which top replicas are at 0).

### Decision — one step per tick, hysteresis built in

```
desired = clamp(ceil(active / target_sessions_per_replica), min, max)
k       = live replica count (len(replicas), draining included)

if desired > k and k < max and up-cooldown elapsed:
    if top replica is draining:  undrainTop()        // un-drain fast path
    else:                        grow()               // append one
    stamp up-cooldown
elif desired < k and k > min and down-cooldown elapsed:
    drainTop()                                         // mark one
    stamp down-cooldown

reapDrained(active)   // EVERY tick, not cooldown-gated, not a "scale event"
```

- **One step per tick.** A pool needing +3 grows one per up-cooldown, never three
  at once — gentle, observable actuation.
- **Separate cooldowns** for up vs down (down typically slower) default
  conservative. Exposed as test knobs for the integration test; operator-facing
  defaults are constants (tunable later if needed — YAGNI for per-agent cooldown
  config now).
- **Draining replicas still count toward `k`** until reaped. So if load rebounds
  while the top is draining, `desired` rises and the loop **un-drains** it
  (clears the flag) rather than spawning a fresh replica — a free win from the
  drain-don't-kill choice.
- **reapDrained every tick** collects replicas that finished draining,
  independent of cooldowns (reaping isn't scaling — it's bookkeeping for a
  decision already taken).

### Interaction with drain-only (the chosen tradeoff)

Scale-down never blocks the loop. `drainTop()` returns immediately. If the top
replica holds a long-lived session forever, it simply never reaps and `k` stays
one above `desired` — capacity reclaimed lazily, exactly the tradeoff chosen. The
loop keeps serving every other decision (and every other agent's loop is
independent).

---

## Section 5 — Failure Modes, Metrics & Process Wiring

### The `session_events` single-writer invariant (must not regress)

A1 spine item #4: `SELECT MAX(seq)+1` for session events is safe *only because
one process owns a session*. A2 preserves this **by construction**:

- A session stamped `replica=i` is served only by executor `agent#i`.
- `grow()` only ever adds a *new* highest index; it never reassigns an existing
  session.
- `drainTop()` / drain never *moves* a session off its owner — it only stops
  *new* sessions landing there.

So no session ever acquires a second writer. This is the central safety argument;
the integration test asserts it empirically (no session served by two executors;
`MAX(turn_count)` per session stays correct).

### Failure modes (degrade, never crash)

| Failure | Behavior |
|---|---|
| `grow()` spawn fails / never healthy | New replica never published (spawn-then-publish) ⇒ never routable. Log warning, `autoscale_events_total{action="blocked"}`++, up-cooldown still stamped so we don't hammer. `k` unchanged; next eligible tick retries. |
| Replica crashes mid-drain | Its Supervisor (still running until reap) restarts it at the same `agent#k-1` VMID; it recovers its own in-flight work (M1, per replica) and keeps draining. Drain survives crashes. |
| Owner-down for a live (non-draining) replica | Unchanged from A1: session-scoped requests 503 until the Supervisor restarts it. A2 adds nothing. |
| Postgres load read fails on a tick | Skip the tick (never decide on stale/missing data), log, `metrics_scrape_skips`-style counter. Pool holds current size. |
| `min == max` | Degenerate but legal: a fixed pool of that size via the PoolManager path (a static pool would also work; we don't special-case it). Policy loop runs but never crosses a threshold. |

### Metrics (`internal/obs`, per-agent; extends A1's `replica`-labeled series)

- `runtime_agent_replicas_desired{agent}` — gauge; what the loop wants.
- `runtime_agent_replicas_current{agent}` — gauge; live count `k` (draining
  included).
- `runtime_agent_active_sessions{agent}` — gauge; the tick's `active`.
- `runtime_autoscale_events_total{agent,action}` — counter,
  `action ∈ {up, down, undrain, reap, blocked}` (`blocked` = wanted to act but
  cooldown / clamp / spawn-fail stopped it).

A draining replica keeps its existing `runtime_agent_up{agent,replica}=1` (it is
healthy and serving); draining-ness is observable via the events counter, not by
flipping a health gauge. New helpers: `AutoscaleDesired/Current/Active(agent, n)`
and `AutoscaleEvent(agent, action)`, all nil-safe like A1's helpers.

### Process wiring (`cmd/runtimed/main.go`)

The boot loop branches on agent kind:

- **Static agent** (today's path, unchanged): for each replica, start a
  per-replica `Supervisor`, sequential readiness gate (preserves M2's DBOS
  first-run schema-init serialization — the first replica creates the schema, the
  rest find it).
- **Autoscaled agent:** the Registry already built its PoolManager. main.go calls
  `pm.Start(ctx)` which starts the initial `min` replicas through the **same
  sequential readiness gate** (first replica creates schema), then launches
  `pm.runPolicy(ctx)`. The PoolManager owns those replicas' Supervisors (their
  cancels live in the slots) so reap can stop an individual one.

Shutdown: the root context cancel propagates to every PoolManager child context
(all replica Supervisors + the policy goroutine), alongside A1's existing static
Supervisors. No new shutdown path — same `ctx.Done()` fan-out.

---

## Section 6 — Testing & Live Proof

### Hermetic unit tests

| Area | File | Cases |
|---|---|---|
| Config | `internal/config/config_test.go` | `autoscale` parses; `min≤max` & `target≥1` enforced; rejected on remote; `max`-range ports reserved + cross-agent collision + 65535 overflow caught; `replicas:` ignored-with-warning when `autoscale` set; `autoscale:nil` ⇒ A1 path untouched; `ReplicaAddr(i)` derivation + bounds |
| PoolManager set ops | `controlplane/poolmanager_test.go` | grow appends at `k` with correct addr/VMID/agent-level fields; drainTop marks only top; undrainTop clears top; reapDrained truncates only the contiguous drained-at-0 suffix; `NextReplica` skips draining; suffix-only invariant holds under a scripted op sequence; `-race` clean under concurrent reads + a scale op |
| Policy decisions | `controlplane/poolmanager_test.go` | injected fake load ⇒ `desired=clamp(ceil(active/target),min,max)`; one-step-per-tick; up/down cooldowns gate; un-drain fast path fires instead of grow on rebound; failed-read tick is a no-op; `blocked` counted on cooldown/clamp/spawn-fail |
| Registry delegation | `controlplane/registry_test.go` | autoscaled id delegates Replicas/Replica/NextReplica to its PoolManager; static id uses slice path unchanged; `SetBroker`/`SetGateway` stamp the PoolManager `base`; router/metrics call sites unaware of mode |
| Metrics | `internal/obs/*_test.go` + poolmanager test | desired/current/active gauges + events counter move on the right actions; nil-safe when metrics absent |

### Integration test (`//go:build integration`, Postgres.app)

`test/autoscale_test.go` — one agent `autoscale: {min:1, max:3,
target_sessions_per_replica:2}`, scripted model, short cooldowns + poll interval
via test-only knobs. Gates:

1. **Scale-up** — open enough concurrent sessions to exceed target; assert
   `replicas_current` climbs 1→2→3 one step per tick, the `replica` column shows
   the spread, and three distinct `agent#0/1/2` executor IDs appear in logs.
2. **Suffix-only drain** — drop load; assert the **top** replica is marked
   draining (new sessions stop landing on it, verified via the `replica` column
   on freshly created sessions) while its existing session keeps receiving
   **live** SSE; once that session completes, the replica is reaped and
   `replicas_current` drops. Assert a *middle* replica is never removed.
3. **Drain blocks on a straggler** — a long-lived session on the top replica
   keeps `replicas_current` one above `replicas_desired` indefinitely (no
   force-kill) — proves the chosen drain-only tradeoff.
4. **Un-drain fast path** — load rebounds while the top is draining ⇒ it is
   un-drained, not replaced (assert **no new executor id** appeared; the same
   `agent#k-1` resumes taking new sessions).
5. **No double-execution / single-writer** — across the whole run, `MAX(turn_
   count)` per session is correct and no session is served by two executors.
6. **Back-compat** — a static `replicas: 2` agent in the same config behaves
   exactly as A1 (no PoolManager, no autoscale events emitted).

Integration tests self-clean their DB + the `dbos` schema, per workspace
convention.

### Live proof (milestone gate)

1. `runtime.yaml` with one `autoscale` agent (`min:1, max:3, target:2`); bring up
   the stack (`go run ./cmd/runtimed`, scripted model — no LLM key).
2. Drive ~6 concurrent scripted sessions via `runtimectl`; show
   `runtime_agent_replicas_{current,desired}` and `runtime_agent_active_sessions`
   on `/metrics` tracking up, and `SELECT replica, count(*) FROM sessions GROUP
   BY replica` spreading across 0/1/2 with three distinct `executor_id=agent#0/1/2`
   in the logs.
3. Drop load; show the **top** replica stop taking new sessions (drain), its
   in-flight session still streaming live, then the reap (`current` drops, a
   `reap` event).
4. Hold a straggler on the top replica; show `current` stuck one above `desired`
   with no force-kill (the durability tradeoff, visible).
5. Flip the agent to a static `replicas: 2` (remove `autoscale`); show behavior
   identical to A1 (no autoscale series emitted).

### Conventions honored

- The `go` CLI is ground truth (ignore IDE/LSP diagnostics from the
  `replace ../harness` setup).
- Integration tests use Postgres.app at
  `postgres://runtime:runtime@localhost:5432/runtime?sslmode=disable` and
  self-clean (DB rows + `dbos` schema).
- No secrets or content in logs or metrics.
- Scripted model (`test/scripted`) so no LLM key is needed.
- gofmt-clean before every commit.

---

## Open items deferred to later spine milestones

- **Scale-down deadline / force-kill** — an explicit, loudly-flagged opt-in that
  trades durability for predictable capacity reclaim; not the A2 default.
- **Richer load signals** — resource (CPU/mem), throughput/latency, queue depth.
- **Per-agent cooldown / poll-interval config** — A2 uses conservative constants
  (test-knob-overridable); promote to config only if an operator needs it.
- **Signal-only mode** — emit desired count and let an external orchestrator
  (C2 M2 / K8s HPA) actuate, instead of runtimed spawning. A2's PoolManager
  decision/actuator split is the seam where this slots in.
- **C2 M2 per-agent-pod scheduling** — the cross-host counterpart: a scheduled
  pod is one replica whose lifecycle the orchestrator (not runtimed's Supervisor)
  owns. A1 + A2 + C3 together make this expressible.
