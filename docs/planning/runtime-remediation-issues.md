# Runtime Remediation Issue Register

Status: code remediation complete; RT-04 live-cluster acceptance pending

This register is the acceptance specification for the post-review remediation.
An issue may be marked complete only when all of the following are true:

- the implementation is present;
- focused regression tests exercise the failure that originally exposed the
  issue;
- the relevant broader test suites pass;
- public documentation describes the resulting behaviour accurately; and
- a final code review confirms that the implementation, tests, and
  documentation agree.

Legend:

- [ ] open
- [x] implemented, tested, and reviewed

## RT-01: Persist and enforce session tenant ownership

- [x] Implementation complete
- [x] Regression tests complete
- [x] Documentation complete
- [x] Final review complete

### Problem

The `sessions` table records an agent ID but not the tenant that created the
session. Session creation, listing, event access, replica affinity, usage, and
failure reporting therefore rely on the agent's current tenant in the live
registry. If an agent ID is reassigned, removed and recreated, or moved between
tenants, a later tenant can inherit access to historical sessions belonging to
the former tenant.

### Required implementation

- Add immutable tenant ownership to `SessionRow` and the persistent schema.
- Require tenant scope for session creation and tenant-aware reads.
- Enforce the stored tenant at both the control-plane route and native-agent
  boundaries.
- Ensure transcript and online-evaluation data agree with the session tenant.
- Prevent accidental tenant reassignment from exposing retained data.
- Define a safe compatibility treatment for rows created before tenant
  ownership was recorded.

### Required tests

- Session tenant round-trip in memory and PostgreSQL stores.
- A tenant cannot list, route to, read events for, or inspect a session owned by
  another tenant.
- Deleting and recreating the same managed-agent ID under a different tenant
  does not expose the former tenant's sessions.
- File-configuration tenant changes do not transfer historical sessions.

### Completion evidence

- `SessionRow`, the memory store, and the PostgreSQL schema/store now persist
  tenant ownership and external bindings. Tenant-aware control-plane and native
  agent checks use the stored owner rather than a mutable registry assignment.
- Core migration 4 quarantines only pre-ledger default-tenant sessions as
  `__legacy_unowned__`; it does not silently transfer them to a configured
  tenant.
- `TestStore_TenantOwnershipAndExternalBinding`,
  `TestPGTenantOwnershipAndSessionRetention`,
  `TestLegacySessionQuarantineOnlyTouchesPreLedgerDefaultRows`,
  `TestAPI_AgentTenantChangeDoesNotTransferSession`, and
  `TestSessionEndpointsRejectAnotherAgentsKnownSession` cover the invariant.
- `runtime.md`, `operator-guide.md`, and `configuration.md` document immutable
  session ownership and legacy-row treatment.

### Gap identified in follow-up audit (closed 2026-07-25)

Transcript and online-evaluation inserts still accept a caller-supplied tenant
independently of the parent session. The child-table RLS policy proves that the
parent session is accessible but does not prove that the child's stored tenant
matches the parent. Direct SQL or a future caller bug can therefore create
internally inconsistent ownership records. Close this gap by deriving and
enforcing child ownership from the parent session in both store SQL and the
database schema, with mismatch regression tests.

### Follow-up closure evidence

- Store writes derive transcript and online-result tenant values from the
  parent session rather than trusting their compatibility arguments.
- Core migration 5 installs child-tenant triggers and stricter RLS checks, so a
  direct SQL mismatch is rejected as well.
- Memory, owner-role PostgreSQL, and restricted-role tests cover forged tenant
  values and missing parent sessions. The full unit and integration suites pass.

### Gap identified in second follow-up audit (open 2026-07-25)

`FailureBreakdownByAgent` and `ActiveSessionsByReplica` still scope only by
agent ID, not by the immutable session tenant. When a file-configured agent ID
is reassigned to another tenant, direct session routes are correctly denied,
but the new tenant's evaluation-failure API and console can include aggregate
failure counts from the former tenant. Old non-terminal sessions can also
distort the replacement tenant's autoscaling load.

Required remediation:

- make failure breakdown and active-session accounting tenant-aware;
- thread the registry tenant through every caller;
- add a tenant-reassignment regression covering failure aggregates and
  autoscaling counts; and
- update the store contract and public documentation to state the complete
  tenant boundary, not only session-route isolation.

### Second follow-up closure evidence

- `ActiveSessionsByReplica` and `FailureBreakdownByAgent` now require both
  tenant and agent ID in the store contract and PostgreSQL predicates.
- Autoscaling passes the pool's configured tenant; the admin API and console
  resolve the agent's current tenant before reading failure aggregates.
- Memory, PostgreSQL, API, and pool-manager tests include same-agent rows owned
  by another or former tenant and prove they neither appear in failure counts
  nor contribute to active load.
- `runtime.md` and `observability.md` now state the aggregate and autoscaling
  tenant boundary. Unit, integration, and race suites pass.

## RT-02: Correct health monitoring for every remote replica

- [x] Implementation complete
- [x] Regression tests complete
- [x] Documentation complete
- [x] Final review complete

### Problem

`MonitorSet` stores one cancellation function per agent ID. Starting a second
remote ordinal cancels the first monitor, and callbacks write every result to
replica zero. Remote pools therefore have neither independent liveness tracking
nor correct reachability metrics.

The existing integration test can pass before the health interval elapses. It
accepts intermittent failures and treats the absence of a row from a failed
request as evidence that routing converged.

### Required implementation

- Key monitor lifecycle by agent ID and replica index.
- Report the actual replica index in registry and metric callbacks.
- Support stopping/restarting one replica and stopping all replicas for an
  agent.
- Return an explicit unavailable result when all replicas are known down.

### Required tests

- Independent monitoring of at least three ordinals with mixed health.
- Starting one ordinal does not cancel another ordinal's monitor.
- Stop/restart affects only its intended scope.
- The integration test waits past the health interval and proves that no new
  requests reach a known-dead ordinal.
- Metrics carry the correct ordinal.

### Completion evidence

- Monitor lifecycle is keyed by agent and ordinal, registration clears stale
  reachability for its ordinal, and routing reports unavailable when every
  replica is known down.
- `TestMonitorSet_RemotePoolTracksEveryOrdinal`,
  `TestMonitorSet_StopReplicaDoesNotStopSiblings`,
  `TestMonitorSet_RestartResetsReachability`, registry tests, and the revised
  `TestAutoscaleGrowDrain` exercise independent replica state and convergence.
- `runtime.md`, `operator-guide.md`, and `observability.md` describe the
  all-replica health and ordinal-labelled metrics.

### Gap identified in second follow-up audit (open 2026-07-25)

Replacing or restarting a monitor cancels the old context but does not fence
its callback. An in-flight old probe can report `false` after the replacement
monitor has already reported `true`. Because the new monitor is edge-triggered
and remembers its own last value as healthy, later successful probes do not
repair the stale registry value. A healthy replica can therefore remain
unroutable indefinitely.

Required remediation:

- associate callbacks with a monitor generation and ignore superseded
  generations, or otherwise suppress callbacks after cancellation;
- ensure stop/restart cannot leave stale reachability behind; and
- add a deterministic test where the old cancelled probe completes after the
  replacement's first successful probe.

### Second follow-up closure evidence

- Every monitor key now carries a monotonically increasing generation.
  Replacement, stop, and replica-stop invalidate the old generation.
- Callback generation validation and the reachability mutation occur under the
  same lifecycle lock, eliminating the validation-to-write race.
- `TestMonitorSet_ReplacementIgnoresSupersededCallback` deterministically
  releases the cancelled old request after the replacement reports healthy and
  proves the old callback is ignored. Unit and race suites pass.

## RT-03: Preserve affinity for remote and command-agent replica pools

- [x] Implementation complete
- [x] Regression tests complete
- [x] Documentation complete
- [x] Final review complete

### Problem

When a remote or command-spawned agent owns its own session store, the control
plane does not know the session-to-replica mapping. New sessions are distributed
across the pool, but subsequent session-scoped requests fall back to ordinal
zero. A session created on another ordinal is therefore misrouted.

### Required implementation

- Persist the control plane's chosen agent, tenant, and replica for externally
  owned session IDs, or make the control plane assign the ID before creation.
- Route every session-scoped operation using that durable binding.
- Preserve single-replica compatibility.
- Reject unsupported pool configurations rather than claiming unsafe affinity.

### Required tests

- A remote two-replica pool with independent stores routes follow-up requests
  to the creating ordinal.
- A replicated command agent receives follow-up requests on the creating
  ordinal.
- Affinity survives a control-plane restart.
- A missing external binding is handled explicitly and safely.

### Completion evidence

- The proxy captures the external session response and durably binds its
  session ID to tenant, agent, and replica. All session-scoped requests resolve
  that binding; an unbound replicated external session fails safely.
- `TestAPI_RemotePoolPersistsExternalSessionAffinity`,
  `TestAPI_CommandPoolAffinitySurvivesControlPlaneRestart`,
  `TestRemoteReplicaPoolAttach`, `TestReplicaPoolsAffinity`, and the missing
  binding cases cover remote and command pools.
- `runtime.md` and `operator-guide.md` now state the durable-affinity guarantee
  and the compatibility behaviour for old unbound sessions.

### Gap identified in follow-up audit (closed 2026-07-25)

External affinity rows remain permanently in status `external`, are counted as
active sessions indefinitely, and are excluded from retention. Their
`updated_at` value is also never refreshed by follow-up traffic. In addition,
binding persistence uses the client request context after the upstream has
created a session, so client cancellation can unnecessarily leave an upstream
orphan. Close the gap with durable post-create persistence, access touching,
inactivity-based external-binding retention, accurate active-session
accounting, and lifecycle regression tests.

### Follow-up closure evidence

- External bindings are persisted with a short cancellation-independent
  post-create context and touched by successfully routed follow-up requests.
- External rows are excluded from native capacity accounting and included in
  inactivity-based, bounded session retention.
- Store and proxy tests cover touch, inactivity expiry, capacity accounting,
  cancellation-safe binding, and restart affinity. The end-to-end replica
  suites pass.

## RT-04: Make per-agent Kubernetes pods an enforced trust boundary

- [x] Implementation complete
- [ ] Regression tests complete
- [x] Documentation complete
- [ ] Final review complete

### Problem

In `perAgentPods` mode, agent authentication is optional, NetworkPolicy is off
by default, enabled policies omit the agent pods, and every agent uses the same
bearer. A cluster workload can call an unauthenticated agent directly; a
compromised agent that knows the shared bearer can call another agent and forge
forwarded tenant, user, role, or assertion headers.

### Required implementation

- Fail Helm rendering when `perAgentPods` lacks an agent authentication
  mechanism.
- Use distinct per-agent credentials rather than one fleet-wide bearer.
- Generate per-agent NetworkPolicies that allow the control plane and required
  probes but deny arbitrary workload access.
- Prevent a bearer holder from forging forwarded identity, preferably with
  workload identity, mTLS, or a signed request assertion.
- Make the chart's security claims match its actual defaults.

### Required tests

- Helm render fails closed without per-agent authentication.
- Rendered agents have distinct credentials/references.
- Rendered NetworkPolicies select every agent StatefulSet and restrict ingress.
- Identity assertion verification rejects tampered tenant, subject, role, and
  expiry data.
- A live-cluster acceptance test proves direct cross-agent access is denied.

### Completion evidence

- Helm now requires per-agent authentication, generates distinct per-agent
  secrets, and renders NetworkPolicies selecting each agent StatefulSet.
  Forwarded identity headers are signed with a control-plane-only Ed25519
  private key and verified with the public key in the receiving agent.
- The Helm render matrix covers fail-closed auth, distinct credentials, and
  per-agent ingress. `TestSignedIdentityRoundTripAndTamper`,
  `TestSignedIdentityExpires`, and
  `TestRequireSignedIdentityRejectsForgery` cover claim integrity and expiry.
- `deploy/charts/runtime/live-networkpolicy-test.sh` is an opt-in live-cluster
  acceptance test for control-plane access and denied cross-agent access. It
  passed shell syntax validation; execution requires an installed Kubernetes
  release and is intentionally not part of the hermetic local suite.
- The chart README documents the enforced boundary and how to run the live
  acceptance test.

### Gap identified in follow-up audit (closed 2026-07-25)

The forwarded-identity HMAC key is the same bearer credential that an agent
uses for authentication. A bearer holder can therefore mint valid identity
assertions. The signed data also omits the HTTP method, request target, and a
single-use nonce, which permits cross-request replay during the timestamp
window. Replace this with an asymmetric signing key held only by the control
plane, bind assertions to the request, reject replay, and test tampering and
replay explicitly.

### Follow-up closure evidence

- Assertions bind timestamp, random nonce, method, escaped target/query, tenant,
  subject, role, and caller assertion. Agents reject stale, tampered, and
  replayed requests.
- The private key remains in the control plane; managed and registered agents
  receive only the public key. Subject forwarding fails closed for missing or
  mismatched configured keys.
- Unit, agent-middleware, registration, registry, and Helm render tests cover
  key separation, claim/request tampering, expiry, replay, and fail-closed
  deployment configuration. The opt-in live-cluster NetworkPolicy script
  remains the environment-specific acceptance check.

### Gap identified in second follow-up audit (open 2026-07-25)

The control-plane reverse proxy signs requests, but the metrics fan-out path
constructs its own request with only the agent bearer. In the secured
`perAgentPods` configuration, agents have both authentication and subject
forwarding enabled, so `requireSignedIdentity` rejects `/metrics` with 401.
Fleet metrics and agent-up status therefore fail in the chart configuration
that RT-04 claims to secure. Existing end-to-end subject-forwarding tests use
agents without an auth token, so the signing middleware is not exercised on
that path. The live-cluster acceptance test also remains unexecuted.

Required remediation:

- make every control-plane-to-agent client, including metrics fan-out, use the
  signed transport or an equivalent request signer;
- test authenticated subject forwarding and metrics together;
- extend the Helm/live acceptance path to verify agent metrics reach the
  management endpoint; and
- execute the live NetworkPolicy test before restoring final acceptance.

### Second follow-up implementation evidence

- Metrics fan-out targets now accept a request signer, and `runtimed` signs
  every agent `/metrics` request with the control-plane-only private key after
  applying the bearer.
- `TestFanout_SignsAuthenticatedMetricsRequest` requires and verifies both
  bearer authentication and the Ed25519 request signature; signing failures
  are reported as a distinct scrape-skip reason.
- The Helm live acceptance script now checks cross-agent denial,
  control-plane-to-agent access, and a successful signed agent scrape exposed
  as `runtime_agent_up=1` on the management endpoint. Chart documentation
  describes this check.
- Unit, race, Helm render, and shell-syntax tests pass. The live Kubernetes
  check remains unexecuted because this workspace has no configured Kubernetes
  context or designated release. RT-04 therefore remains open only at its
  environment-specific acceptance boundary.

## RT-05: Separate control-plane and agent database authority

- [x] Implementation complete
- [x] Regression tests complete
- [x] Documentation complete
- [x] Final review complete

### Problem

Agents receive the control-plane PostgreSQL credential when
`RUNTIME_AGENT_PG_DSN` is absent. The supplied Compose, secured, GCP, and Helm
deployments use the same credential. A compromised agent can therefore query
identity, service-key, encrypted-secret, evaluation, memory, and other-agent
data.

### Required implementation

- Provision separate control-plane and agent database roles.
- Make production deployment profiles supply a restricted agent DSN.
- Restrict the role to the runtime/DBOS data it needs.
- Where practical, enforce tenant or agent scope with RLS, security-definer
  functions, or another database-level boundary.
- Fail closed in production-oriented profiles instead of only logging a
  warning.

### Required tests

- Deployment rendering includes distinct control-plane and agent DSNs.
- Agent credentials cannot read identity, service-key, or secret tables.
- Agent credentials cannot read another tenant's session data.
- Native session execution and DBOS recovery still work with the restricted
  role.

### Completion evidence

- Compose, secured, GCP, and Helm profiles now supply a distinct agent DSN.
  Startup provisions restricted grants plus tenant RLS and refuses production
  role rebinding. A single shared agent role cannot span tenants.
- `TestRestrictedAgentRoleCannotReadOtherTenantOrIdentityTables`, Helm
  fail-closed render tests, Compose config validation, and the complete
  lifecycle integration suite cover privilege denial and normal execution and
  recovery with the restricted role.
- `operator-guide.md`, `configuration.md`, and the chart README document role
  provisioning, one-tenant-per-agent-role scope, and separate deployment and
  database requirements.

### Gap identified in follow-up audit (closed 2026-07-25)

The restricted role still receives `CREATE` on `public`, DML on the global
`agents` table, and broad authority over shared DBOS objects. Tenant RLS also
does not distinguish agents within one tenant. Remove unnecessary grants,
reduce schema creation authority, make the remaining DBOS trust-domain
constraint explicit and fail closed for unsupported shared-role layouts, and
extend privilege tests to cover same-tenant agents and schema/object creation.

### Follow-up closure evidence

- Restricted roles no longer have `CREATE` on `public` or access to the global
  `agents` table. DBOS access is explicit and limited to its schema and owned
  runtime objects.
- Runtime and Helm reject a shared restricted role for multiple agents,
  including same-tenant agents. The one-role-per-agent trust-domain constraint
  is documented.
- Restricted-role integration tests verify sensitive/global metadata denial,
  public DDL denial, tenant RLS, and child-record integrity while the complete
  native execution/recovery suite passes.
- Compose and GCP deployment profiles require a generated agent database
  password. The GCP bundle has no predictable fallback, and CI/release validate
  the credentialed profiles.

### Gap identified in second follow-up audit (open 2026-07-25)

The fail-closed check compares the two DSNs as strings rather than comparing
their authenticated database roles. The same privileged role can be expressed
with different host spelling, query ordering, or credentials and pass the
check. `ProvisionAgentRole` also does not reject a superuser, a role that owns
the protected tables, or a role that inherits privileged membership. In those
cases the apparent revocations and RLS boundary do not restrict the agent.

Two test bypass variables are also honoured by the production binary:
`RUNTIME_TEST_ALLOW_SHARED_AGENT_DB_ROLE` installs a wildcard tenant mapping,
and `RUNTIME_TEST_ALLOW_AGENT_ROLE_REBIND` permits reassignment. Accidental use
outside tests defeats the one-agent trust-domain guarantee.

Required remediation:

- compare the effective control-plane and agent database identities, not DSN
  text;
- reject superuser, owner, bypass-RLS, and privilege-inheriting agent roles;
- move wildcard/rebind test behaviour behind a test-only binary/build path or
  a database fixture that production cannot enable; and
- add integration tests using textually different DSNs for the same role and
  deliberately privileged candidate roles.

### Second follow-up closure evidence

- Startup parses PostgreSQL configurations and compares effective role names,
  so textually different DSNs cannot disguise a shared control-plane role.
- Agent-role provisioning rejects the current/control role, superuser,
  `CREATEROLE`, `CREATEDB`, `BYPASSRLS`, inherited privileged/control
  membership, and ownership of protected or RLS tables.
- Shared-role and rebind concessions are compiled only with the
  `runtime_integration` build tag. Ordinary binaries ignore the old variables
  even when set; the integration target builds disposable binaries with the
  tag.
- Unit tests cover URL/keyword and textually different DSNs. Tagged PostgreSQL
  tests reject the control owner, an elevated catalog role, and a catalog role
  with inherited membership while normal restricted execution still passes.

## RT-06: Distinguish missing sessions from persistence outages

- [x] Implementation complete
- [x] Regression tests complete
- [x] Documentation complete
- [x] Final review complete

### Problem

The store converts `sql.ErrNoRows` into an untyped formatted error, while routing
treats every `GetSession` error as a missing session. A database outage becomes
a misleading 404 for native sessions and an ordinal-zero fallback for remote or
command agents.

### Required implementation

- Add a stable `ErrSessionNotFound` sentinel.
- Return 404 only for that sentinel.
- Return 503 for persistence failures without changing affinity.
- Record a routing-store failure metric and structured log.

### Required tests

- Missing session returns 404.
- Database/store failure returns 503.
- A persistence failure never triggers remote ordinal-zero fallback.

### Completion evidence

- Stores return `ErrSessionNotFound`; routing maps only that sentinel to 404
  and maps persistence failures to 503 without replica fallback. Structured
  logs and `runtime_routing_store_failures_total` expose outages.
- `TestStore_MissingSessionUsesSentinel` and
  `TestAPI_SessionStoreFailureReturnsUnavailable` cover the distinction.
- `runtime.md` and `observability.md` describe the failure contract and metric.

## RT-07: Make evaluation run transitions durable and recoverable

- [x] Implementation complete
- [x] Regression tests complete
- [x] Documentation complete
- [x] Final review complete

### Problem

The evaluation runner ignores failures while marking runs running, errored, or
completed. Metrics can report completion while the durable record remains
running. Startup recovery starts one goroutine per incomplete run and does not
claim or lease work, so multiple control planes can duplicate runs and a large
backlog can create a startup storm.

### Required implementation

- Make run transitions checked compare-and-swap operations.
- Add a durable claim/lease with owner and expiry.
- Bound recovery and normal execution with a worker pool.
- Emit terminal metrics only after durable finalization succeeds.
- Surface transition and lease failures operationally.

### Required tests

- Failure injection for start, result, error-finalization, and
  completion-finalization writes.
- Two recoverers cannot execute the same claimed run concurrently.
- Expired leases can be recovered.
- Recovery concurrency never exceeds its configured bound.
- Completion metrics require a committed terminal state.

### Completion evidence

- Evaluation runs now use owner/expiry leases, checked claimed finalization,
  bounded submission/recovery workers, and post-commit terminal metrics.
- Runner failure-injection tests cover claim, result, error-finalization, and
  completion-finalization failures. Duplicate-worker, expired-lease,
  normal-queue-bound, and recovery-bound tests cover concurrency and recovery.
  The tagged PostgreSQL eval test also verifies live lease exclusion and
  expired lease reclamation.
- `evals.md`, `operator-guide.md`, and `observability.md` document leases,
  worker bounds, recovery, and metrics.

### Gap identified in follow-up audit (closed 2026-07-25)

Recovery scans only once at startup. A run whose unexpired lease belongs to a
dead worker is skipped and is not retried when that lease later expires.
Leases are renewed only between cases, so one long provider or judge call can
outlive its lease. Queue-saturation handlers also ignore terminal-transition
errors and use an unconditional finalizer. Add periodic bounded recovery,
in-flight lease heartbeats, compare-and-swap pending failure, and failure-path
tests.

### Follow-up closure evidence

- Recovery uses periodic, bounded, oldest-first claimable-run scans and a fixed
  worker/queue budget. Live foreign leases cannot starve later pending work,
  and expired leases become eligible on a later scan.
- A lease heartbeat runs during provider and judge calls. Pending queue
  saturation uses a checked compare-and-swap terminal transition.
- Tests cover a foreign lease expiring after startup, long provider calls,
  bounded scan size and worker concurrency, duplicate claims, transition
  failures, and saturation finalization. Unit, race, and tagged PostgreSQL eval
  suites pass.

### Gap identified in second follow-up audit (open 2026-07-25)

Run claiming and terminal finalization are owner-aware, but `PutResult` is not.
If a worker pauses long enough to lose its lease and another worker claims the
run, the stale worker can still upsert a case result before discovering the
owner change. `FinishRunClaimed` also checks only the owner string, not that the
lease is still live. The PostgreSQL claim transition mixes application-clock
timestamps with database-clock recovery eligibility, increasing the risk under
clock skew.

Required remediation:

- fence every result write by current owner and live lease in the same
  transaction/statement;
- require a live lease for finalization or use a monotonic fencing token;
- use one authoritative clock for PostgreSQL lease acquisition, renewal,
  eligibility, and finalization; and
- add a stale-worker test proving that results and terminal state cannot be
  overwritten after another worker reclaims the run.

### Second follow-up closure evidence

- Result upserts now select and lock the run row only when owner, running
  status, and live lease all match; a stale worker receives `false` and stops.
- Claimed finalization also requires running status and a live lease.
- PostgreSQL acquisition and renewal derive lease expiry from database
  `now()`, and claim eligibility, result fencing, and finalization all use the
  database clock.
- Memory and tagged PostgreSQL stale-owner tests prove that a reclaimed run's
  results and terminal state cannot be overwritten. Runner failure tests use
  the checked result transition. Unit, integration, and race suites pass.

## RT-08: Bound and drain online evaluation scoring

- [x] Implementation complete
- [x] Regression tests complete
- [x] Documentation complete
- [x] Final review complete

### Problem

Every sampled session creates a goroutine that uses `context.Background()`.
Scoring is not bounded, cancelled, or drained on shutdown. Sustained traffic can
produce unbounded provider calls and database writes, and metrics are emitted
even when results are not persisted.

### Required implementation

- Use a bounded queue and worker pool owned by the agent lifecycle.
- Apply per-session scoring deadlines.
- Define queue-full behaviour and metrics.
- Drain or durably retain pending work at shutdown.
- Count only persisted scoring outcomes.

### Required tests

- Worker concurrency remains within the configured bound.
- Cancellation and shutdown stop or drain workers deterministically.
- Queue saturation follows the documented policy.
- Failed persistence does not increment success counters.

### Completion evidence

- Online scoring uses a lifecycle-owned bounded queue and worker pool, a
  per-session deadline, deterministic drop-on-full behaviour, and bounded
  shutdown draining. Outcome counters advance only after persistence.
- `TestScoringWorkerConcurrencyIsBoundedAndDrains`,
  `TestScoringQueueFullDropsWithoutBlocking`,
  `TestScoringShutdownCancelsAfterDrainDeadline`, and
  `TestScoringMetricsRequirePersistedResults` cover the required behaviour.
- `evals.md`, `configuration.md`, and `observability.md` document configuration,
  saturation, shutdown, and metrics.

### Gap identified in follow-up audit (closed 2026-07-25)

After the shutdown deadline expires, `stopScoring` cancels the worker context
and then waits on the worker group without another bound. A provider or store
that ignores cancellation can therefore block process shutdown forever. Make
the post-cancellation wait bounded and add a deliberately non-cooperative
worker regression test.

### Follow-up closure evidence

- After the graceful drain deadline, scoring cancellation has a second fixed
  wait bound. A non-cooperative dependency cannot block process shutdown.
- A `shutdown_timeout` drop reason and structured error make this exceptional
  path observable.
- The non-cooperative judge regression passes under the race detector together
  with the broader scoring lifecycle suite.

## RT-09: Add session, event, and live-memory retention

- [x] Implementation complete
- [x] Regression tests complete
- [x] Documentation complete
- [x] Final review complete

### Problem

Evaluation artefacts have retention, but sessions and session events accumulate
indefinitely. Live memory has dead-row garbage collection but no time-based
expiry. Existing foreign keys do not provide a clean lifecycle cascade.

### Required implementation

- Add configurable retention for terminal sessions and their events.
- Add explicit live-memory retention by memory kind, with a disabled default if
  product semantics require opt-in.
- Preserve active and recoverable workflows.
- Reap in bounded batches with metrics and dry-run support.
- Define archive/export and backup expectations.

### Required tests

- Terminal data older than the cutoff is reaped with its dependants.
- Active or recoverable work is retained.
- Tenant boundaries hold during cleanup.
- Batch limits and dry-run behaviour work.
- Live-memory expiry does not remove entries when the feature is disabled.

### Completion evidence

- Terminal-session retention reaps bounded batches and explicitly removes
  events, transcripts, and online results; active/recoverable sessions are
  excluded. Live memory has independently opt-in, per-kind retention.
- `TestStore_ReapSessionsRetainsActiveAndCascades`,
  `TestPGTenantOwnershipAndSessionRetention`, and
  `TestReapBeforeIsTenantAndKindScopedAndSupportsDryRun` cover cascades,
  activity, tenant/kind scope, batching, dry-run, and disabled defaults.
- `operator-guide.md`, `identity-and-memory.md`, `configuration.md`, and
  `observability.md` document retention, archive/backup expectations, and
  metrics.

### Gap identified in follow-up audit (closed 2026-07-25)

Session cleanup processes only one batch per six-hour interval, evaluation
cleanup is unbounded in one statement, and memory cleanup can loop over the
entire backlog in one sweep. External affinity rows are not eligible at all.
Apply a consistent per-sweep time/work budget, repeat bounded batches promptly
while budget remains, include inactive external bindings, and test large
backlogs without converting cleanup into an unbounded operation.

### Follow-up closure evidence

- Session, evaluation, transcript/result, and live-memory deletion operations
  are individually batch-limited. Each scheduled sweep is capped at 20 passes
  and 30 seconds.
- Evaluation capture sweeps continue until an empty pass rather than assuming
  both child tables filled the same batch, while still respecting the hard
  sweep budget.
- Inactive external bindings are eligible for session cleanup; active native
  and recoverable eval work remains excluded.
- Batch, dry-run, backlog, external-lifecycle, and tenant/kind isolation tests
  pass in memory and PostgreSQL suites.

### Gap identified in second follow-up audit (open 2026-07-25)

`runtime_retention_reaped_total` is incremented only for session retention.
Evaluation retention deletes captured rows and runs without recording the
documented metric. Live-memory retention sends its count to
`agent_memory_gc_reaped_total`, conflating policy-driven deletion of live memory
with garbage collection of already-dead history. Although `ReapBefore`
supports dry-run internally, operators cannot enable dry-run for live-memory
retention.

Required remediation:

- expose distinct, correctly labelled metrics for session, evaluation,
  live-memory retention, and dead-history GC;
- wire evaluation and live-memory deletion counts to those metrics;
- provide an operator-facing live-memory dry-run setting, or narrow the stated
  RT-09 requirement and documentation explicitly; and
- add metric and configuration tests for every retention worker.

### Second follow-up closure evidence

- Evaluation capture and run cleanup increment
  `runtime_retention_reaped_total` with separate `evaluation_capture` and
  `evaluation_run` kinds; session cleanup retains its `session` kind.
- Live-memory deletion has its own
  `agent_memory_retention_reaped_total{agent,tenant,kind}` counter and remains
  distinct from dead-history GC.
- `RUNTIME_MEMORY_RETENTION_DRY_RUN` is passed safely to locally managed agents,
  performs one bounded eligibility pass per kind, deletes nothing, and emits no
  deletion metric. Remote agents can set the same documented environment
  variable in their deployment.
- Metric, safe-child-environment, dry-run, per-kind batching, PostgreSQL, and
  documentation tests pass.

## RT-10: Introduce versioned database migrations

- [x] Implementation complete
- [x] Regression tests complete
- [x] Documentation complete
- [x] Final review complete

### Problem

Packages execute embedded additive DDL during startup under an advisory lock.
The lock prevents concurrent creators, but there is no ordered migration
ledger, supported schema range, upgrade preflight, or rollback policy.

### Required implementation

- Add a schema migration ledger and ordered transactional migrations.
- Make each binary declare and check its supported schema range.
- Keep first-install behaviour deterministic.
- Document backup, upgrade, rollback, and DBOS workflow compatibility.

### Required tests

- Fresh install reaches the current schema version.
- Upgrades from every released baseline are ordered and idempotent.
- A newer unsupported schema fails before serving traffic.
- A failed migration rolls back and does not advance the ledger.

### Completion evidence

- All package schemas use the shared transactional migration ledger. Core has
  explicit ordered migrations through version 4, immutable checksums, schema
  range checks, and transactional advisory locking.
- `TestSchemaMigrationsOrderedIdempotentAndVersionChecked`,
  `TestFailedMigrationRollsBackLedgerAndDDL`, and the legacy-baseline
  quarantine test cover fresh/upgrade ordering, idempotency, unsupported
  versions, and rollback.
- `RELEASING.md` and `operator-guide.md` define preflight, backup, rollback,
  and DBOS compatibility procedures.

## RT-11: Harden HTTP request and connection resource limits

- [x] Implementation complete
- [x] Regression tests complete
- [x] Documentation complete
- [x] Final review complete

### Problem

The control-plane and agent servers limit header-read and idle time but do not
bound request-body duration or concurrent streaming connections. Byte limits do
not stop slow uploads. Agent session JSON accepts unknown fields, trailing
values, and returns internal errors.

### Required implementation

- Apply request-body deadlines without breaking long-lived SSE responses.
- Bound concurrent requests and SSE subscribers.
- Decode agent JSON strictly and consistently.
- Return generic server errors while logging structured internal detail.
- Expose saturation and timeout metrics.

### Required tests

- Slow request bodies time out.
- SSE remains usable beyond ordinary request deadlines.
- Subscriber and request limits reject excess work predictably.
- Unknown fields, trailing JSON, and oversized payloads are rejected.
- Internal errors are not returned to clients.

### Completion evidence

- Control-plane and agent servers now impose body-read deadlines and separate
  request/SSE concurrency limits. Session JSON is strict and bounded, SSE is
  exempt from ordinary body deadlines, and internal errors are logged while
  clients receive generic responses.
- Agent server tests cover slow bodies, long-lived SSE, saturation, unknown
  fields, trailing JSON, oversized input, and information disclosure.
  `cmd/runtimed` tests cover control-plane request and stream limits.
- `configuration.md`, `operator-guide.md`, and `observability.md` document
  controls and saturation/timeout metrics.

## RT-12: Treat transcript capture as sensitive data

- [x] Implementation complete
- [x] Regression tests complete
- [x] Documentation complete
- [x] Final review complete

### Problem

Transcript redaction is based on credential-shaped keys and a small set of
regular expressions. It does not cover bare JWTs, GitHub/Slack/provider token
formats, unusual nested representations, or personal information. Calling the
result safely redacted overstates the guarantee.

### Required implementation

- Make transcript capture independently configurable and disableable.
- Expand deterministic credential coverage while keeping the mechanism
  explicitly best-effort.
- Add an allowlist/filter extension point for deployments with stronger privacy
  requirements.
- Document encryption, access, retention, and residual-risk expectations.

### Required tests

- Bare JWT, GitHub, Slack, service-key, API-key, cookie, and nested secret
  fixtures are removed.
- Capture-disabled mode writes no transcript.
- Custom filtering is invoked before persistence.

### Completion evidence

- Transcript capture can be disabled independently. Recursive deterministic
  redaction covers structured and bare credential formats, followed by an
  optional deployment filter; documentation consistently calls this
  best-effort rather than complete sanitisation.
- Redaction fixtures cover JWT, GitHub, Slack, service-key, API-key, cookie,
  nested, and conventional secret values. Dedicated tests cover disabled
  capture and pre-persistence custom filtering.
- `evals.md`, `configuration.md`, `operator-guide.md`, and
  `identity-and-memory.md` document encryption, access, retention, and residual
  privacy risk.

## RT-13: Complete release and documentation hygiene

- [x] Implementation complete
- [x] Regression tests complete
- [x] Documentation complete
- [x] Final review complete

### Problem

The repository has no licence or standard security/contribution/release files.
The current tree contains unreleased changes beyond `v0.2.0`; chart publishing is
manual and lacks provenance/SBOM/signing guidance. The Helm README reports
version `0.1.0` while the chart is `0.2.0`. Public documentation overstates
remote-pool guarantees and describes memory's remaining work as generic
“TTL/GC” even though dead-row GC exists.

### Required implementation

- Add project-owned release, security-reporting, contribution, ownership, and
  change-history documentation without inventing a licence choice.
- Synchronize chart/version documentation from one source of truth.
- Add release automation for immutable images, chart artefacts, SBOMs, and
  provenance/signing where credentials permit.
- Correct all capability and limitation descriptions affected by RT-01 through
  RT-12.
- Extend documentation checks to catch stale chart versions and broken issue
  register links.

### Required tests

- Documentation structure/link checks cover the new public files.
- A test verifies chart README versions match `Chart.yaml`.
- Release workflow configuration is syntax-checked and uses immutable version
  inputs.

### Completion evidence

- `SECURITY.md`, `CONTRIBUTING.md`, `CHANGELOG.md`, `RELEASING.md`,
  `.github/CODEOWNERS`, and the pinned release workflow now define the public
  maintenance and release contract. Licence selection remains an explicit
  owner decision rather than an invented grant.
- Documentation checks require these files and the planning register, validate
  Markdown links/structure, compare the chart README version with
  `Chart.yaml`, and parse/check pinned release workflow actions and tag inputs.
- Public capability and limitation text in the README and topic guides was
  reconciled with RT-01 through RT-12.

### Gap identified in follow-up audit (closed 2026-07-25)

The main integration target and CI omit tagged PostgreSQL packages, the race
job omits the evaluation runner, and the release workflow can publish after
only `make check` while the independent CI workflow is still running or has
failed. Documentation checks enumerate only tracked Markdown, so newly created
required planning files can escape validation. Release-workflow tests verify
mostly substrings rather than the full gate. Make the authoritative targets
complete, gate publication on the complete validation matrix, validate required
worktree documentation, and strengthen workflow regression tests.

### Follow-up closure evidence

- `make test-integration` runs the complete end-to-end package and tagged
  PostgreSQL store, eval, memory, and identity packages.
- CI and the release workflow include evaluation in the race gate. Publication
  is sequenced after unit, integration, race, Python, Helm, shell, Compose, and
  documentation validation in the same release job.
- Documentation checks include the required planning files before staging,
  parse the release workflow, assert its full gate, and verify deployment
  agent-database credentials fail closed.
- The local unit, integration, race, Python, Helm, Compose, shell-syntax, and
  diff gates pass. `shellcheck` is enforced by CI/release and was unavailable in
  this local environment.

## Final acceptance review

- [ ] Every RT-01 through RT-13 checkbox is complete.
- [ ] Focused regression tests pass.
- [x] Full Go unit tests pass.
- [x] Race tests pass for concurrency-heavy packages.
- [x] PostgreSQL integration tests pass.
- [x] Python shim tests pass.
- [x] Helm lint and render tests pass.
- [x] Documentation checks pass.
- [ ] A fresh code review finds no unresolved implementation or test-case
  requirement in this register.
- [x] `git diff --check` is clean and unrelated user files remain untouched.

## Superseded validation record

Reviewed and executed on 2026-07-25:

- `make check`: passed formatting, vet, documentation checks, and all Go unit
  packages.
- `make test-integration`: passed the complete end-to-end package in 257.396
  seconds using separate control-plane and restricted agent database roles.
- Tagged PostgreSQL packages passed for `internal/store`, `internal/eval`,
  `internal/memory`, and `internal/identity`; the final added retention test
  also passed independently.
- Race tests passed for `controlplane`, `agentruntime`, `internal/gateway`,
  `internal/identity`, `internal/store`, and `internal/eval`.
- Python shim: 32 passed. The sole warning is an upstream Starlette/httpx
  deprecation notice.
- Helm lint passed, and every chart render/fail-closed case passed.
- Base, Compose, secured, and all distributed GCP Compose manifests passed
  `docker compose config --quiet` with dummy validation secrets.
- Changed shell scripts passed `bash -n`; `shellcheck` is enforced in CI but was
  not installed in this local environment.
- The live Kubernetes NetworkPolicy acceptance test was added and
  syntax-checked but not executed because this environment has no target
  cluster/release.
- `git diff --check` passed, and the unrelated untracked implementation reports,
  local GCP environment file, and food-label images were not modified.

This record is retained as historical evidence for the first remediation pass.
It does not close the gaps reopened by the follow-up audit above.

## Follow-up validation record

Reviewed and executed on 2026-07-25 after the follow-up fixes:

- `make check` passed `go vet`, documentation checks, and every hermetic Go
  package.
- `make test-integration` passed the complete end-to-end package in 252.514
  seconds and the tagged PostgreSQL `internal/store`, `internal/eval`,
  `internal/memory`, and `internal/identity` packages on the final source state.
- Race tests passed for `controlplane`, `agentruntime`, `internal/gateway`,
  `internal/identity`, `internal/store`, and `internal/eval`.
- Python shim tests passed: 32 tests, with one upstream Starlette/httpx
  deprecation warning.
- Helm lint and the complete render/fail-closed matrix passed.
- Base, turnkey, and distributed GCP Compose files passed
  `docker compose config --quiet`; the GCP control-plane bundle also failed
  closed as expected when `RUNTIME_AGENT_DB_PASSWORD` was omitted.
- Changed shell scripts passed `bash -n`. `shellcheck` was not installed
  locally and remains an enforced CI/release gate.
- `git diff --check` passed. `IMPLEMENTATION-HISTORY.md`,
  `IMPLEMENTATION-REPORT.md`, `P2.2-REPORT.md`, `deploy/gcp/llm.env`, and
  `food_label_images/` remained untouched.
- The opt-in live Kubernetes NetworkPolicy acceptance script was not executed
  because this workspace has no designated cluster/release target; its render
  assertions and shell syntax passed.

## Second follow-up audit record

Reviewed on 2026-07-25 against the implementation paths, not only the prior
green test output:

- RT-01 reopened because failure aggregates and autoscaling counts are still
  keyed by agent ID without immutable tenant scope.
- RT-02 reopened because superseded health-monitor callbacks can overwrite the
  replacement monitor's state.
- RT-04 reopened because authenticated subject-forwarding agents reject the
  unsigned metrics fan-out request, and the live acceptance test remains
  unexecuted.
- RT-05 reopened because textual DSN inequality does not prove role separation,
  privileged roles are not rejected, and production binaries honour test
  wildcard/rebind bypasses.
- RT-07 reopened because result writes are not fenced by lease owner/live lease
  and PostgreSQL lease decisions mix application and database clocks.
- RT-09 reopened because retention metrics are incomplete/mislabelled and
  live-memory dry-run is not operator-configurable.

The earlier validation commands remain useful regression evidence, but they do
not exercise these failure modes. New focused tests are required before these
issues or the final acceptance review may be closed again.

## Second follow-up remediation validation record

Reviewed and executed on 2026-07-25 after the second-audit fixes:

- `make check` passed formatting, `go vet`, documentation validation, and all
  Go unit packages.
- `make test-integration` passed the complete end-to-end package in 252.205
  seconds and tagged PostgreSQL store, eval, memory, and identity packages.
- Race tests passed for `agentruntime`, `controlplane`, `internal/eval`,
  `internal/memory`, `internal/obs`, and `internal/store`.
- Python shim tests passed: 32 tests, with one upstream Starlette/httpx
  deprecation warning.
- The Helm security render/fail-closed matrix, secured Compose validation, and
  changed-shell `bash -n` checks passed.
- Focused regressions cover former-tenant aggregates/load, superseded monitor
  callbacks, authenticated signed metrics, effective/elevated database roles,
  stale eval workers, retention metric kinds, safe child configuration, and
  dry-run behaviour.
- `git diff --check` and a final documentation check are rerun after this
  register update.
- No Kubernetes current context is configured. The extended live
  NetworkPolicy and signed-metrics acceptance script is syntax-checked but
  unexecuted; RT-04 and overall final acceptance remain open solely for that
  external check.
