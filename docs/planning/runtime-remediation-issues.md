# Runtime Remediation Issue Register

Status: ALL items are CLOSED as of 2026-07-26. RT-10, RT-11, RT-13, RT-17, and
RT-18 have implementation, focused adversarial tests, documentation, and
review; G1, G2, and G3 close with them. **RT-04, RT-14, and F6 are now closed
as well: the two-release live Kubernetes acceptance ran and passed** (commit
`4d4484c`).

Two prior "unavailable in this environment" records were both wrong, in the
same way — each asserted an absence without testing for it:

- G2's "shellcheck unavailable locally": the container fallback runs fine, and
  running it showed the CI/release gate had been failing.
- F6/RT-04/RT-14's "no cluster is available": `kind` was already installed, and
  its default CNI enforces NetworkPolicy. Verified directly rather than
  assumed — under a deny-all ingress policy a working 200 becomes a curl-28
  timeout, which is exactly the signal the harness requires.

The acceptance was worth running. It found that `perAgentPods` mode had been
100% broken since `449cbf1` rebased the image onto `scratch`: the agent
StatefulSet invoked `/bin/sh` to derive its replica ordinal, that shell no
longer exists, and every agent pod CrashLooped at container start
(`StartError`). The chart suite, `helm lint`, and every render gate passed
throughout — none of them asserted that the interpreter existed. Fixed by
resolving the ordinal from the `apps.kubernetes.io/pod-index` label via the
downward API, with `kubeVersion: ">= 1.28.0-0"` pinning the floor that label
requires and three mutation-verified chart assertions covering the regression.

The standing rule — no release-candidate claim without current evidence — is
now satisfied for every gate this register tracks: the local invariants and the
Kubernetes acceptance both have named, dated evidence.

Two caveats belong with that, because neither is covered by the evidence above.
The acceptance ran on kind (Docker Desktop, macOS/arm64), so a managed-cluster
CNI and a clean-Linux host remain uncovered. And it exercises exactly what the
harness probes — cross-agent and management-metrics ingress denial — not the
whole `perAgentPods` surface. Notably, the startup regression it uncovered
proves that a passing render suite is not evidence that a pod runs; only the
live run was.

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

## Frozen comprehensive acceptance matrix

This matrix freezes the current comprehensive remediation scope. A focused test
for the latest symptom is necessary but insufficient. Every affected invariant
must be exercised across the applicable rows below, including negative,
concurrent, restart, migration, and deployment cases. New findings discovered
inside this frozen scope extend the relevant row rather than silently moving
the closure boundary.

### A. Identity, ownership, routing, and affinity

- [x] A1. An authenticated tenant is authorized against the exact immutable
  agent tenant and generation selected for forwarding, not an earlier or later
  registry lookup.
- [x] A2. Create-session and every session-scoped route fail closed when an
  agent is deleted, recreated, reassigned, or endpoint-replaced between
  authorization, ownership lookup, affinity lookup, and forwarding.
- [x] A3. Native and external session ownership remains immutable across
  same-tenant and cross-tenant agent replacement, process restart, control-plane
  restart, replica health changes, and scale changes.
- [x] A4. External affinity binds tenant, agent, immutable generation, and
  replica. Generation-less legacy bindings have an explicit fail-closed
  compatibility policy.
- [x] A5. Forwarded subject assertions bind the authenticated principal to the
  selected agent snapshot. Tenant users, agent subjects, and superusers follow
  explicit and separately tested rules.
- [x] A6. List, usage, events, transcripts, online results, failure aggregates,
  autoscaling counts, and affinity touches use stored immutable ownership rather
  than mutable registry identity.
- [x] A7. The matrix is tested for memory and PostgreSQL session stores and for
  native local, command-spawned, attach-only remote, dynamically managed remote,
  file-configured remote, and Helm registration-managed agents where supported.

### B. Database authority and trust-domain isolation

- [x] B1. Control-plane and agent effective database identities differ; an
  agent role cannot be superuser, bypass RLS, own protected tables, inherit a
  privileged role, or enable a production test concession.
- [x] B2. Two roles in different tenants cannot access each other's core,
  child, evaluation, identity, memory, secret, or DBOS records.
- [x] B3. Two roles for different agents in the same tenant cannot access or
  mutate each other's core session children or DBOS workflow state.
- [x] B4. Attach-only agents do not trigger unused role provisioning.
  Locally spawned and registration-managed agents fail closed unless their
  supported database trust domain is enforceable.
- [x] B5. Fresh install, upgrade/migration, restart, recovery, and ordinary
  native execution work with restricted roles without granting schema-wide or
  cross-agent authority. Restricted startup validates a contiguous immutable
  migration ledger, its checksums, and required tables, constraints, triggers,
  and row-security policies without applying DDL — and, as of the seventh-audit
  remediation, validates their SEMANTICS rather than their names: policy
  predicates, trigger function identity and column list, and foreign-key column
  mappings. Verified not-too-strict against a real migrated schema under three
  `search_path` values. See RT-10.
- [x] B6. Compose, secured, GCP, and Helm profiles render distinct credentials
  and cannot imply that separate role names in one shared database provide
  agent isolation when they do not.

### C. Evaluation durability and terminal classification

- [x] C1. Every terminal native session receives a durable deterministic
  failure classification independently of optional sampled online scoring.
- [x] C2. Queue admission, queue saturation, scorer shutdown, drain timeout,
  provider failure, judge failure, and result-store failure cannot leave an
  unclassified terminal session or record a false successful quality result.
- [x] C3. `quality_fail` is recorded only from a persisted completed score;
  skipped or incomplete scoring retains a truthful deterministic category and
  observable drop/failure reason.
- [x] C4. Evaluation run creation, claiming, lease renewal, result writes,
  finalization, recovery, and migration retain the RT-07 fenced state machine
  in memory and PostgreSQL under concurrency and clock boundaries.
- [x] C5. Classification, scoring, drop, persistence, and terminal metrics count
  durable outcomes exactly once, including partial failure and shutdown paths.

### D. Retention atomicity, ageing, and accounting

- [x] D1. One stable candidate set is selected and locked per destructive
  session-retention transaction; its children and parent are deleted atomically.
- [x] D2. Concurrent touch, status transition, new child insertion, and
  retention either serialize safely or fail without leaving a surviving parent
  with partially deleted history.
- [x] D3. Native terminal sessions, inactive external bindings, active sessions,
  recoverable work, legacy rows, and tenant boundaries follow explicit and
  independently tested eligibility rules.
- [x] D4. Evaluation captures and terminal runs age from the relevant terminal
  or capture timestamp. Long-running and recovered runs are not deleted
  immediately after completion; legacy missing timestamps have a defined rule.
- [x] D5. Session, evaluation, transcript/result, live-memory retention, and
  dead-history GC remain batch-, pass-, and time-bounded across empty, partial,
  and large backlogs.
- [x] D6. Every successful destructive batch is counted before a later failure;
  dry-run eligibility is never counted as deletion; configuration is exposed
  consistently through binaries, Compose, GCP, and Helm.
- [x] D7. Memory and PostgreSQL contract tests cover boundary timestamps,
  batching, dry-run, partial success, restart, concurrency, cascade integrity,
  tenant/kind scope, and disabled defaults.

### E. Registration credential lifecycle

- [x] E1. Every registration-capable agent has an explicit immutable instance
  generation. The generation is stable for an ordinary restart and changes for
  intentional deletion/recreation, endpoint replacement, or trust-domain
  rotation.
- [x] E2. Dynamically managed, persisted managed, file-configured, and Helm
  registration-managed agents follow the same token tenant/generation
  invariant; attach-only agents do not accidentally acquire registration
  authority.
- [x] E3. Mint, verify, register, list, revoke, delete, recreate, and concurrent
  administration authorize from live immutable agent identity or immutable
  token ownership as appropriate.
- [x] E4. Tokens from an old tenant, generation, endpoint instance, deleted
  agent, or legacy unbound row fail closed in memory and PostgreSQL.
- [x] E5. File and Helm generation material is persisted and rotatable without
  deriving identity solely from tenant and agent ID. Upgrade and operator
  rotation procedures are tested and documented.
- [x] E6. A valid exact-instance token releases only its bound tenant's
  restricted environment and secrets, across restart and registration retry.

### F. Release, deployment, and acceptance evidence

- [x] F1. CI and release both execute the authoritative unit, integration,
  race, Python, Helm lint, Helm render, Compose, documentation, and shell gates.
- [x] F2. Parsed workflow tests prove every blocking validation step occurs in
  the publishing job before the first external publication operation.
- [x] F3. Mutation tests fail when a gate is omitted, reordered after
  publication, moved to an unrelated job, converted to a comment, or allowed to
  fail.
- [x] F4. Deployment render tests cover every supported agent origin and trust
  topology, including explicit rejection of unsupported shared-database and
  missing-generation layouts.
- [x] F5. The hermetic RT-04/RT-14 NetworkPolicy matrix proves readiness,
  resource discovery, allowed baselines, signed metrics, genuine timeout
  denial, and rejection of DNS, refusal, debug, image, and missing-resource
  failures.
- [x] F6. The same RT-04/RT-14 harness passes against two installed one-agent
  Kubernetes releases with distinct agent identities and isolated databases.
  Ran on kind, both directions, with a negative control; see the live
  Kubernetes acceptance evidence under RT-04.
- [x] F7. Every published image embeds the release version and source revision,
  and tests inspect the built image rather than trusting workflow arguments.
- [x] F8. Helm can consume an immutable signed image digest; a mutable tag is
  not the sole production identity of a published workload.
- [x] F9. The release inventory explicitly names every required and optional
  runtime image. Every published image is built, smoke-tested, scanned, given
  an SBOM, and signed before the release is described as complete.
- [x] F10. Sidecar and helper image dependencies are reproducibly constrained
  and included in vulnerability and update policy. Nine third-party images are
  digest-pinned across the Compose profiles and both workflows; the vendored
  Bitnami subchart is pinned by values override rather than edited; and
  `TestPinnedThirdPartyDigestsDoNotDrift` machine-checks that a shared image
  cannot acquire divergent digests. See RT-13.

### G. Closure gates

- [x] G1. Every matrix row has named focused tests that fail against the
  pre-fix behaviour and pass after remediation. The seventh-audit additions were
  each verified by mutation rather than by assertion: reverting the fix makes the
  new test fail with its intended message.
- [x] G2. Full formatting, vet, unit, race, PostgreSQL integration, end-to-end,
  Python, Helm, Compose, documentation, shell, and diff checks pass from the
  final source state. The shell gate was never environment-blocked: the
  `koalaman/shellcheck` container runs locally, and running it revealed the CI
  and release steps were FAILING (exit 1, 69 info/style findings — 58 SC2086,
  28 SC2015, 3 SC2016, 1 SC1091, 1 SC2181; zero error, zero warning). Both
  workflows now pass `--severity=warning`, under which the same command over the
  same file set exits 0. The scripts were deliberately not rewritten: the
  dominant SC2086 hits are intentional word splitting of multi-flag variables
  (`DSN`, `AGENTS` at `deploy/charts/runtime/test.sh:7,11`) that quoting would
  break. `TestShellGateCarriesExplicitSeverity` fails if the flag is dropped or
  if a workflow carries no shellcheck invocation at all.
- [x] G3. Public documentation states guarantees and unsupported topologies
  without presenting an unexecuted test or deployment assumption as evidence.
  The seventh-audit changes were held to this standard specifically: the browser
  package comment and `gateway-and-sandboxes.md` name the residual reachable
  surface instead of claiming total containment; `mayTouchStore` documents its
  intrinsic TOCTOU window; `RELEASING.md` describes what the scan actually sees;
  and `.grype.yaml` refuses structured metadata that the tool would silently
  discard.
- [x] G4. A final source review follows the complete request path and durable
  state transition for every matrix section rather than reviewing only the
  latest diff.
- [x] G5. Any environment-dependent acceptance remains visibly open and cannot
  be described as complete in summaries, plans, or public documentation.

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

### Gap identified in second follow-up audit (closed 2026-07-25)

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

### Gap identified in fifth follow-up audit (closed 2026-07-26)

Tenant authorization and agent selection are separate live-registry reads.
`IdentityMiddleware` authorizes `/agents/{id}/...` against the tenant returned
by `Registry.TenantOf`, then `NewAPI` independently calls `Registry.Get` and
`pickReplica`. A managed agent can be deleted and the same ID recreated under a
different tenant between those operations. The already-authorized request can
then be proxied to the replacement tenant's agent. This affects new-session
requests directly; session-scoped requests also have a check-to-route race
between reading the stored owner and selecting the current replica. Forwarded
identity signing authenticates the stale authorization decision rather than
revalidating the selected `AgentProcess`.

Required remediation:

- authorize against the exact registry generation selected for proxying, or
  revalidate the authenticated principal against the selected process
  immediately before forwarding;
- make agent replacement and request selection expose an atomic immutable
  tenant/generation snapshot;
- ensure subject forwarding cannot sign a tenant that differs from the
  selected agent's tenant except for an explicitly authorized superuser flow;
  and
- add deterministic races for a new-session request and a session-scoped
  request paused between authorization and replacement.

### Fifth follow-up closure evidence

- Routing now carries one immutable selected-agent snapshot containing tenant,
  generation, mode, and replica identity from authorization through forwarding.
  A final exact-snapshot reauthorization and a lifecycle read lease prevent
  delete/recreate from changing the target underneath an authorized request.
- Native session creation persists the selected tenant and generation.
  Session-scoped routes authorize stored ownership and reject a missing, stale,
  or replacement generation before forwarding or signing subject assertions.
- `TestAPIReauthorizesExactSelectedAgentSnapshot`,
  `TestRegistryReplicaLeasePinsLifecycleAgainstReplacement`, store ownership
  contracts, and full remote/native end-to-end tests cover same-tenant and
  cross-tenant replacement, restart, and concurrent lifecycle changes.

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

### Gap identified in second follow-up audit (closed 2026-07-25)

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
- Preserve single-replica behaviour only for bindings with provable immutable
  generation; fail older generation-less bindings closed.
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

### Gap identified in fifth follow-up audit (closed 2026-07-26)

An external affinity row stores tenant, agent ID, and replica, but not the
agent-instance generation. Deleting and recreating a managed remote under the
same tenant and ID therefore makes every old external binding pass
`pickReplica` and route to the replacement endpoint. The replacement does not
own the original external store or session. For operator-configured remotes the
same problem occurs whenever an ID is reused for a changed endpoint. This
breaks the claimed durable affinity even without a tenant change and combines
with RT-01's check-to-route race during concurrent replacement.

Required remediation:

- persist an immutable agent generation with external session bindings;
- require the binding generation to match the selected live process before
  proxying or touching the binding;
- migrate generation-less external bindings fail closed, with an explicit
  compatibility policy for old single-replica remotes; and
- test same-tenant delete/recreate, endpoint replacement, restart persistence,
  and concurrent replacement during a session-scoped route.

### Fifth follow-up closure evidence

- External bindings now persist tenant, agent, immutable generation, and
  replica. Generation-less legacy rows fail closed and are never touched or
  forwarded.
- `pickReplica` validates the binding against the leased selected-agent
  snapshot; restart preserves a valid binding, while same-ID recreation or
  endpoint-generation rotation rejects it.
- `TestPickReplicaRejectsStaleAndLegacyAgentGeneration`, remote-pool restart
  and attachment tests, PostgreSQL binding round trips, and replacement-race
  tests pass.

## RT-04: Make per-agent Kubernetes pods an enforced trust boundary

- [x] Implementation complete
- [x] Regression tests complete
- [x] Documentation complete
- [x] Final review complete

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

### Gap identified in second follow-up audit (closed 2026-07-25)

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

### Gap identified in third follow-up audit (closed 2026-07-25)

The chart deliberately rejects more than one agent per release because one
restricted PostgreSQL role is one agent trust domain. The live acceptance
script nevertheless selects two agent pods from one release without checking
their agent IDs. A supported two-replica release therefore tests isolation
between replicas of the same agent, not direct access from one agent trust
domain to another.

Required remediation:

- make the live test use two separately installed one-agent releases;
- assert that the source and target pods carry different agent IDs;
- verify target control-plane access and signed metrics within the target
  release; and
- update the chart documentation with the exact two-release acceptance setup.

### Third follow-up implementation evidence

- The live script now takes source and target release names, discovers one
  agent pod from each, and fails unless their agent IDs are distinct.
- It proves source-agent denial for both the target agent and target management
  metrics, then proves the target control plane can reach its agent and expose
  a successful authenticated, signed metrics scrape.
- Helm documentation specifies two one-agent releases with separate database
  roles. Helm render and shell-syntax tests pass.
- The live script remains unexecuted because this workspace has no designated
  Kubernetes cluster or two installed releases. The regression and final-review
  checkboxes therefore remain open.

### Gap identified in fourth follow-up audit (closed 2026-07-25)

The revised live script still treats every failed `kubectl debug` or `curl`
invocation as proof that NetworkPolicy denied the request. An
ephemeral-container permission error, image-pull failure, DNS failure, missing
Service, unready pod, or refused connection can therefore satisfy both denial
assertions. The script selects the first labelled pod without checking its
phase/readiness and does not first prove that the source debug container can
resolve and reach a known-allowed endpoint. Its success message can consequently
report cross-agent isolation without having executed a valid source-side
network probe.

Required remediation:

- discover only Running and Ready source, target, and control-plane pods and
  fail explicitly when any required Service or endpoint is absent;
- prove the source debug mechanism, DNS, and ordinary connectivity with a
  known-allowed request before attempting either denial;
- distinguish debug-container/setup failures from the expected network-denial
  result, recording and validating the probe exit reason; and
- add a hermetic script test with fake `kubectl` responses for allowed, denied,
  DNS-failure, image-pull, missing-resource, and unready-pod cases before
  executing the corrected script against two releases.

### Fourth follow-up code closure evidence

- Pod discovery now accepts only Running and Ready source-agent, target-agent,
  and target-control-plane pods. Service discovery requires the target agent,
  target metrics, and the source control-plane baseline endpoints to exist.
- Each debug-container probe prints an explicit curl exit marker. Only curl
  timeout exit 28 is accepted as a policy denial; DNS failures, connection
  refusals, image-pull/debug failures, missing markers, and missing resources
  fail the run.
- The source agent must first resolve and reach its own control-plane Service,
  proving that the debug mechanism and ordinary source-side connectivity work.
  The target control plane must reach its agent and expose
  `runtime_agent_up=1` before cross-release denials are accepted.
- A hermetic fake-`kubectl` suite covers valid denial, leaked access, DNS
  failure, connection refusal, image-pull failure, missing resources, and
  unready pods. It is part of the chart test suite, which passes together with
  Helm lint.
- The implementation and hermetic regression gap is closed. The parent issue's
  regression and final-review checkboxes remain open until the same harness
  succeeds against two installed one-agent releases.

### Live Kubernetes acceptance evidence (2026-07-26)

Ran on a two-release `kind` cluster (`runtime-alpha`, `runtime-beta`, namespace
`rt`), each a one-agent `perAgentPods` release with its own restricted database
role and distinct agent id.

- `live-networkpolicy-test.sh rt runtime-alpha runtime-beta` — **OK**, and the
  reverse direction (`runtime-beta runtime-alpha`) — **OK**.
- Negative control: with `networkPolicy.enabled=false` on the target release the
  same harness FAILS with `agent alphaagent to agent betaagent unexpectedly
  succeeded`. The pass therefore measures policy enforcement, not an
  unreachable endpoint.
- Getting there required fixing a total `perAgentPods` startup regression and a
  harness defect that meant it had never run to completion; see `4d4484c`.

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

### Gap identified in second follow-up audit (closed 2026-07-25)

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

### Gap identified in third follow-up audit (closed 2026-07-25)

Restricted-role provisioning counts every configured remote agent even though
attach-only agents never receive or use `RUNTIME_AGENT_PG_DSN`. The distributed
GCP control-plane profile contains four attach-only agents and a distinct agent
DSN, so the production binary rejects the published profile before serving.
Compose syntax validation cannot detect this startup failure.

Required remediation:

- scope restricted-role provisioning to locally spawned agents that actually
  receive the agent DSN;
- skip provisioning when a control plane contains only attach-only agents;
- retain the one-role-per-local-agent fail-closed rule; and
- add unit and production-binary boot coverage for the published remote-only
  topology.

### Third follow-up closure evidence

- Database-role selection ignores attach-only remotes and skips provisioning
  for a remote-only control plane. The shipped four-agent GCP profile is loaded
  directly by a non-integration unit regression and selects no local role.
- Registration- or pod-managed remotes are explicit through
  `RUNTIME_PROVISION_REMOTE_AGENT_ROLE`; Helm `perAgentPods` sets it
  automatically. The registration handshake integration test proves the
  restricted DSN is provisioned and the agent boots.
- Local and registration-managed agents retain the one-role-per-agent
  fail-closed check. The identity-enabled turnkey Compose profile was corrected
  to one local agent because it provides one restricted role.
- Configuration and Compose/Helm documentation distinguish attach-only from
  database-managed remotes. Unit, full integration, tagged PostgreSQL, Helm,
  Compose, and documentation tests pass.

### Gap identified in fifth follow-up audit (closed 2026-07-26)

The restricted-role boundary is still tenant-scoped, not agent-scoped.
`runtime_agent_tenant_roles` maps a login only to `tenant_id`, and every core
RLS policy admits all sessions and children for that tenant. Distinct roles for
two same-tenant agents in one database can therefore read and mutate each
other's core session data. `ProvisionAgentRole` also grants each role access to
the shared `dbos` schema and all control-owned DBOS objects, so distinct login
names do not create separate DBOS trust domains.

The restricted-role integration test proves only cross-tenant denial; it does
not create two same-tenant agents/roles and test either core or DBOS isolation.
Public operator documentation says to use separate databases, but the Helm
per-agent-pod guide tells operators only to use separate releases and roles,
and the chart cannot reject two releases pointed at the same database. The
earlier finding that tenant RLS does not distinguish same-tenant agents is
therefore not actually resolved.

Required remediation:

- bind restricted roles and core RLS to both tenant and immutable agent
  identity, including child-table authorization;
- isolate DBOS state per agent through separate schemas/databases or an
  enforceable agent discriminator rather than relying on distinct role names;
- add a two-role, same-tenant integration test proving denial for core session
  rows, child rows, and DBOS workflow state;
- make the supported topology fail closed where possible; and
- make the Helm guide and two-release acceptance setup require genuinely
  isolated databases until shared-database agent isolation exists.

### Fifth follow-up closure evidence

- Restricted role mappings now bind both tenant and immutable agent ID, and
  core/child RLS policies enforce both dimensions.
- Each database-managed agent requires its own DBOS schema and executor
  identity. Unsupported shared-role, shared-schema, or missing-identity
  topologies fail during startup; attach-only remotes do not provision a role.
- The restricted-role PostgreSQL test covers cross-tenant and same-tenant
  cross-agent denial across core children and protected component tables.
  Recovery, registration-handshake, remote attachment, Compose, GCP, and Helm
  tests exercise supported and rejected trust layouts.

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

### Gap identified in second follow-up audit (closed 2026-07-25)

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

### Gap identified in third follow-up audit (closed 2026-07-25)

The `EvalStore` production interface still exposes `SetRunStatus` and
`FinishRun`. Their implementations update by run ID without requiring a lease
owner or a live lease, and `FinishRun` clears any lease. Current production
runner paths use fenced methods, but a future caller can bypass the RT-07
invariant through the public persistence contract.

Required remediation:

- remove unfenced transition methods from the production interface and store
  implementations;
- migrate tests and fixtures to claimed, live-lease transitions;
- retain explicit test setup through normal persistence operations rather than
  a production back door; and
- add a contract regression that prevents unfenced methods from being
  reintroduced.

### Third follow-up closure evidence

- `SetRunStatus` and unconditional `FinishRun` were removed from `EvalStore`
  and both persistence implementations.
- Memory, PostgreSQL, and console fixtures now claim runs and finalize them
  through the existing live-lease transition.
- `TestEvalStoreContractHasNoUnfencedRunTransitions` asserts that neither
  method returns to the production persistence contract.
- Unit, tagged PostgreSQL integration, full end-to-end, and eval race suites
  pass.

### Gap identified in fourth follow-up audit (closed 2026-07-25)

Removing the explicitly unconditional methods did not make the remaining store
contract a valid state machine. `CreateRun` accepts any caller-supplied status,
so callers can insert `running` or terminal rows without a claim. `ClaimRun`
accepts an empty owner and a non-positive lease, leaving a `running` row
immediately claimable by another worker. `FinishRunClaimed` accepts an empty
owner and any status; after an empty-owner claim it can match the store's empty
owner and can “finish” a run back into `pending` or `running` while clearing its
lease. The PostgreSQL schema has no status or state-coherence constraints, and
the memory store also silently overwrites duplicate run IDs while PostgreSQL
rejects them. Production runner callers currently supply valid values, but the
public persistence surface does not enforce the fencing invariant it claims.

Required remediation:

- make run creation always create a new `pending`, unleased row and reject
  duplicate IDs consistently in both stores;
- reject empty claim/finalization owners and non-positive lease durations;
- allow claimed finalization only to the terminal `completed` or `error`
  states, with coherent `finished_at` and cleared lease fields;
- add database constraints or equivalent migration checks for valid status and
  coherent durable state; and
- add PostgreSQL and memory contract tests for non-pending creation, duplicate
  creation, empty owners, expired-at-acquisition leases, and non-terminal
  finalization.

### Fourth follow-up closure evidence

- Both stores validate that new runs are pending, unleased, unfinished, and
  uniquely identified. Duplicate creation returns the same `ErrRunExists`
  contract from memory and PostgreSQL.
- Claiming rejects an empty owner and any lease that is not strictly in the
  future. Result writes and finalization retain their live-owner and live-lease
  fences.
- Claimed finalization accepts only `completed` or `error`; terminal rows have
  a finish timestamp and no lease. Invalid transitions return
  `ErrInvalidRunTransition`.
- The version-2 evaluation migration normalizes recoverable legacy rows,
  converts unknown states to explicit errors, clears stale finish timestamps
  from valid leased work, and adds status and state-coherence constraints.
  PostgreSQL migration tests exercise each legacy shape and prove that
  malformed direct SQL is rejected.
- Memory and PostgreSQL contract tests cover malformed creation, duplicate IDs,
  invalid owners and leases, non-terminal finalization, and durable
  constraints. Unit, race, and tagged PostgreSQL evaluation suites pass.

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

### Gap identified in fifth follow-up audit (closed 2026-07-26)

Failure classification is coupled to the optional sampled-scoring queue. When
a terminal session is sampled, `serve.go` calls `enqueueScore` and does not
check its result. If the bounded queue is full or already closing, the job is
dropped and `scoreOnto` never reaches its classification tail. The durable
session keeps an empty `failure_category`, while `evals.md` claims every
terminal native session is deterministically classified and that
classification is always enabled. Queue tests assert only the drop metric and
do not check this durable side effect.

Required remediation:

- make terminal classification independent of whether optional online scoring
  is admitted to its bounded queue;
- define how `quality_fail` is represented when scoring is skipped or
  incomplete without silently recording a clean result;
- persist or explicitly classify every terminal session during queue-full and
  shutdown paths; and
- add saturation/shutdown tests for durable failure category and classification
  metrics, then align the evaluation documentation with the exact guarantee.

### Fifth follow-up closure evidence

- Deterministic terminal classification is persisted before optional queue
  admission. Queue saturation, shutdown, provider/judge failure, or store
  failure cannot leave a terminal native session unclassified.
- A successfully persisted score may atomically refine `none` to
  `quality_fail` exactly once. The durable category is authoritative; initial
  classifications and refinements have separate replay-safe metrics.
- Queue rejection, non-cooperative shutdown, persistence failure, replay, and
  end-to-end failure-aggregate tests pass in memory and PostgreSQL. `evals.md`
  and `observability.md` define initial versus refined accounting explicitly.

### Gap identified in sixth audit (closed 2026-07-26)

`PutOnlineResultIfNew` reports whether a criterion was first persisted, but
both stores still overwrite the authoritative result on conflict. A replay
whose non-deterministic judge returns a different verdict can therefore change
the stored score while the monotonic failure category and exactly-once metrics
retain the first outcome. The durable result, classification, and metrics no
longer describe one event.

Required remediation:

- make the first persisted `(session, criterion)` result immutable, or
  introduce an explicitly versioned attempt model with one atomic authoritative
  result;
- apply identical conflict semantics in memory and PostgreSQL;
- keep classification and metrics behind the same first-write decision; and
- test fail-to-pass, pass-to-fail, replay, restart, and concurrent scoring.

### Sixth-audit closure evidence

- Memory and PostgreSQL stores now insert the first
  `(session, criterion)` result and leave it immutable on every later conflict.
  The store returns both whether it inserted and the authoritative first
  verdict, so classification and metrics derive from the same durable event.
- `TestOnlineScoringReplayUsesAuthoritativeFirstVerdict`,
  `TestScoringMetricsDoNotDoubleCountReplay`,
  `TestMemOnlineResultReportsFirstDurableWrite`,
  `TestOnlineResultRoundTripImmutableAndByTenant`, and
  `TestPGReplaySafeTerminalClassificationAndOnlineMetricsState` cover opposite
  verdicts, replay, concurrency, and process/store reconstruction.
- The final unit, race, end-to-end, and tagged PostgreSQL evaluation suites
  pass. `evals.md` defines first-write authority and replay accounting.

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

### Gap identified in second follow-up audit (closed 2026-07-25)

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

### Gap identified in third follow-up audit (closed 2026-07-25)

Retention counters are emitted only after a complete sweep. If one or more
destructive batches succeed and a later batch fails, the deleted rows are not
recorded. Evaluation capture deletions are similarly lost when later run
cleanup fails. In addition, the supplied Helm and Compose profiles do not
expose the documented retention and request/stream controls, forcing operators
to edit deployment manifests.

Required remediation:

- count every successful destructive batch before a later failure can return;
- never count dry-run eligibility as deletion;
- add partial-success failure tests for session, evaluation-capture, and
  evaluation-run retention;
- expose session/evaluation/live-memory retention and request/stream limits in
  Helm with render tests; and
- pass the same settings through the supplied control-plane and agent Compose
  profiles.

### Third follow-up closure evidence

- Session, evaluation-capture, and evaluation-run sweeps record every
  successful destructive batch immediately. Later failures log the number
  already removed without losing metric accounting; dry-run remains uncounted.
- Focused helpers inject failures after successful session, capture, and run
  batches and verify the partial counts.
- Helm exposes typed control-plane and agent retention/concurrency values,
  including live-memory dry-run and per-kind retention. The supplied
  control-plane and native-agent Compose profiles pass through the equivalent
  environment variables.
- Render tests verify non-default values reach monolith and per-agent pods;
  documentation checks cover every supplied profile. Unit, Helm, Compose,
  documentation, integration, and race gates pass.

### Gap identified in fifth follow-up audit (closed 2026-07-26)

`pgStore.ReapSessions` re-evaluates the candidate subquery independently for
each child-table delete and the final parent delete under PostgreSQL's default
READ COMMITTED isolation. A concurrent authorized request can touch an
external binding or otherwise update a session after one child statement has
selected it. A later statement then excludes the now-active row, leaving the
parent in place after its events, transcripts, or online results were already
deleted. The implementation therefore does not preserve active session data
atomically.

Evaluation run retention also selects terminal rows by `created_at`, not
`finished_at`. A long-running or recovered evaluation can finish after the
retention cutoff and be deleted at the next sweep despite having just produced
its final results.

Required remediation:

- select and lock one stable session candidate set per transaction, then delete
  exactly that set and its children, preferably through the enforced cascade;
- ensure a concurrent touch/status transition either wins before selection or
  is serialized behind deletion, never producing a surviving parent with
  partially deleted history;
- age terminal evaluation runs from `finished_at`, with an explicit legacy
  policy for terminal rows lacking it; and
- add PostgreSQL concurrency coverage for touch-versus-reap plus a long-running
  evaluation retention regression.

### Fifth follow-up closure evidence

- PostgreSQL session retention selects and locks one bounded candidate set,
  deletes its children and parents in one transaction, and serializes
  concurrent touches and child writes behind the deletion decision.
- Evaluation runs age from `finished_at`; capture, session, run, transcript,
  result, and memory reapers remain bounded and count every successful batch
  before a later failure. Dry runs never increment deletion counters.
- `TestPGSessionRetentionSerializesConcurrentTouchWithoutPartialHistory`,
  completion-age tests, partial-success metric tests, and the full integration
  suite cover boundaries, restart, batching, cascades, and disabled defaults.

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

### Comprehensive restore gap identified and closed 2026-07-26

A retained current migration ledger did not prove that baseline tables and
cross-component foreign keys still existed after a partial restore. Blindly
running later migrations could fail before baseline repair, while permissive
repair could conceal orphaned durable rows.

- The shared migration runner validates ledger versions and checksums before
  any repair, reconciles the immutable baseline before dependent migrations,
  and performs a final idempotent reconciliation.
- Versioned referential-integrity migrations cover core sessions, evaluation
  runs, identity children, managed agents, and gateway upstreams. Missing or
  incorrect cascading constraints are restored only when rows are consistent;
  orphans are retained and startup fails closed for operator recovery.
- Integration tests cover partial restore before a dependent migration,
  corrupt-ledger no-mutation, current-ledger missing tables, missing foreign
  keys, and orphan refusal in every affected store.

### Restricted-preflight gap identified in sixth audit (closed 2026-07-26)

The restricted-agent preflight checks only `max(version)`. It accepts a ledger
with missing intermediate versions or modified migration contents, and it does
not prove that required tables, constraints, ownership triggers, or RLS
policies still exist. A restricted agent may therefore serve against a
partially restored or tampered schema that the control plane would repair or
reject.

Required remediation:

- validate every expected ledger version, name, and checksum without DDL;
- reject gaps, duplicates, unknown versions, and incompatible ranges;
- verify read-only structural sentinels for the core tables, foreign keys,
  tenant-integrity triggers, row security, and named policies required by an
  agent; and
- add integration tests for missing ledger rows, corrupt checksums, dropped
  tables, dropped triggers, disabled RLS, and missing policies.

### Sixth-audit closure evidence

- `NewPGStoreExisting` performs no repair or DDL. It validates the exact
  expected migration versions, names, and checksums, rejects unknown or missing
  rows, then verifies the agent-visible tables, cascading foreign keys,
  tenant-integrity triggers, row-security flags, and named policies.
- `TestCoreSchemaRestrictedPreflightRejectsMissingSecurityObjects` exercises
  ledger gaps and corruption plus each missing or disabled structural
  sentinel. Existing migration-ledger rollback and partial-restore tests
  continue to pass.
- Fresh end-to-end startup and the complete tagged PostgreSQL suite pass with
  the restricted role. `RELEASING.md` now distinguishes applying migrations
  from restricted read-only preflight.

### Semantic-preflight gap identified in seventh audit (open 2026-07-26)

The read-only preflight proves that policies and triggers with expected names
exist, but not that they still enforce the expected predicates or execute the
expected function. A policy can be replaced with `USING (true)` under the same
name, or a named trigger can be redirected to a permissive function, and the
current counts still pass. The foreign-key query similarly checks only child
table, parent table, cascade mode, and count; it does not prove that the
constraint maps each child's `session_id` to `sessions.id`. Ledger queries are
also unqualified and therefore unnecessarily depend on `search_path`.

Required remediation:

- schema-qualify every migration-ledger query;
- compare normalized policy predicates, commands, roles, and check predicates
  with the expected definitions;
- verify trigger event/timing and exact target function identity;
- verify foreign-key source and target column mappings, validation state, and
  cascade action; and
- add adversarial integration cases that replace, rather than merely drop,
  each security object while retaining its expected name.

### Seventh-audit closure evidence (2026-07-26)

- `CheckCoreSchema` now compares semantics, not names. Policies are checked
  against the exact normalized `pg_get_expr` predicate for both `polqual` and
  `polwithcheck`, plus `polcmd` and `polpermissive`; triggers against `tgtype`,
  `tgenabled`, the `tgfoid`-resolved `public.runtime_enforce_session_child_tenant`
  identity, and the `tgattr` column list; foreign keys against `conkey`/`confkey`
  column mappings, `confdeltype`, and `convalidated`. Every ledger read and the
  ledger INSERT are schema-qualified to `public.`; the `CREATE TABLE`, the
  `GRANT`, and the `c.relname IN (...)` literal are deliberately unchanged.
- Two further bypasses were found during review and closed in the same pass:
  an EXTRA permissive policy under a new name was accepted (PostgreSQL ORs
  permissive policies, so this fully defeated tenant isolation on `sessions`),
  and a trigger recreated as `UPDATE OF tenant` — dropping `session_id` —
  passed because `tgtype` does not encode the column list. Both were reproduced
  empirically before the fix and are now rejected with the offending object
  named in the error.
- `TestCoreSchemaRestrictedPreflightRejectsMissingSecurityObjects` covers 14
  cases: the 5 original drops, 7 name-preserving semantic weakenings, the extra
  policy, and the narrowed trigger. It also asserts, before any mutation, that a
  freshly migrated schema is ACCEPTED — without that guard a preflight
  hardcoded to always error would satisfy every mutation subtest.
- Verified not-too-strict: the full tagged `internal/store` suite passes against
  a real migrated database, and `CheckCoreSchema` was confirmed to return nil
  under `search_path` values `public`, `pg_catalog,public`, and the default.
  The restricted role can read `polqual`/`tgattr`/`tgfoid` (catalogs are
  world-readable), so agent startup is unaffected.
- Commits `7771f3e`, `6a830b4`.

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

### Auxiliary-client gap identified in sixth audit (closed 2026-07-26)

Several outbound clients have timeouts but no response-size bound. The
evaluation judge reads the complete response, while registration and console
paths decode arbitrary JSON streams. A fast oversized peer can consume
unbounded memory despite the HTTP timeout. Browser proxy connection and tunnel
limits are tracked with RT-17.

Required remediation:

- define small, named response limits for judge, registration, console, and
  evaluation-control responses;
- reject an over-limit response before decoding or including it in an error;
- cover successful boundary-sized and oversized responses; and
- document that time and byte limits are separate controls.

### Sixth-audit closure evidence

- The shared `internal/httplimit` reader enforces a named byte ceiling before
  JSON decoding or error construction. Judge, registration, console-agent, and
  evaluation-control clients all use it in addition to their timeouts.
- `TestHTTPJudgeRejectsOversizedResponse`,
  `TestDecodeRegistrationResponseRejectsOversize`,
  `TestHTTPAgentClientRejectsOversizedJSON`, and
  `TestEvalInvokerRejectsOversizedCreateResponse` exercise the affected paths.
  The browser-specific request and tunnel resource tests are recorded under
  RT-17.
- Unit, race, and end-to-end suites pass, and `configuration.md` states that
  response time and response bytes are independent limits.

### Complete-client and fan-out gap identified in seventh audit (open 2026-07-26)

The shared response limiter was applied to the four clients named by the sixth
audit, but a repository-wide client review still finds unbounded JSON decoding
in embedding, fact extraction, summary extraction, episode extraction, the Go
nutrition example, and the conformance client. Their timeouts limit duration
but not allocation from a fast oversized response.

Registry-driven fan-out is also unbounded. The public agent-status route,
management metrics fan-out, and console fleet/feed builders start one goroutine
per registered agent, replica, or session for every request. Incoming request
semaphores do not prevent one request from multiplying into an arbitrarily
large number of outbound requests and goroutines.

Required remediation:

- inventory every production, example, administrative, and conformance HTTP
  response and apply an explicit byte limit or document a justified streaming
  contract;
- share boundary/oversize tests across all JSON provider clients;
- add bounded worker pools or semaphores for registry- and session-derived
  fan-out, with cancellation and partial-result semantics;
- expose saturation/skip accounting without attacker-controlled unbounded
  metric labels; and
- add large-registry and cancellation tests that assert peak concurrency and
  goroutine completion.

### Seventh-audit closure evidence (2026-07-26)

- **Clients.** All nine remaining unbounded consumers now decode through the
  shared `internal/httplimit` reader with a named per-package ceiling:
  `internal/memory/{embed,ingest,summarize,episode}.go` (4 MiB),
  `conformance/conformance.go` (4 sites), and
  `examples/nutrition-label-go/tools.go`. The example is in the same Go module,
  so it uses the shared helper rather than a hand-rolled limiter. The gate
  `grep -rn "json.NewDecoder(resp.Body)" --include='*.go' . | grep -v _test.go`
  now returns nothing. Server-side `r.Body` decoders are out of scope: they are
  already wrapped in `http.MaxBytesReader`.
- **Fan-out.** New `internal/xfan.Each` runs a bounded, cancellable fan-out over
  an index range (`DefaultLimit = 16`, buffered-channel semaphore, no new
  dependency). Applied to `internal/obs/fanout.go`, `controlplane/api.go`, and
  both sites in `console/observability.go`. `Each` stops scheduling on
  cancellation but always awaits started work, so it never returns while it
  owns a live goroutine; the semaphore slot is acquired before `wg.Add(1)`, so
  the Add-races-Wait misuse is structurally impossible.
- `controlplane/api.go` also carried a real defect beyond scheduling: the
  health probe used `http.NewRequest` (ignoring client disconnect) and swallowed
  the construction error as `req, _ :=`. Both fixed. Replacing the mutex-guarded
  append with a pre-sized index-disjoint slice additionally makes the `/agents`
  response order deterministic.
- Non-vacuity established by mutation, not assertion: restoring the unbounded
  ceiling makes the new `internal/obs` regression fail at **132 concurrent
  scrapes vs. a limit of 16**; reverting a bounded decoder makes the
  corresponding oversize test fail. `xfan` was probed for goroutine leaks across
  `n=0`, `n<0`, `limit>n`, `limit<1`, and a pre-cancelled context.
- The pre-existing 500 ms per-scrape timeout and 4 MiB body bound in
  `internal/obs/fanout.go` are preserved. All four packages pass under `-race`.
- Commits `be4cf82`, `aa2b661`.

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

### Gap identified in fifth follow-up audit (closed 2026-07-26)

The release step named “Run Helm lint and render matrix” does not run
`helm lint`; it runs only dependency preparation and the template-based chart
script. CI has the same omission. A tag can therefore reach image/chart
publication without the chart's declared lint gate.

The workflow regression does not validate the gate structurally. It parses the
YAML but then checks only that required substrings occur somewhere in the file.
It would still pass if publication moved before validation, if a required
command appeared only in a comment or unrelated job, or if the gate were split
into a non-blocking path. This is the same acceptance-design weakness the third
audit required to remove.

Required remediation:

- run `make helm-lint` in both CI and the pre-publication release sequence;
- inspect the parsed workflow jobs/steps to require every validation step in
  the publishing job before the first external publication command;
- reject required commands that appear only in comments, unrelated jobs, or
  steps after publication; and
- add mutation-style fixtures proving the regression fails for reordered,
  omitted, and non-blocking gates.

### Fifth follow-up closure evidence

- CI and the release publishing job both run Helm lint and the complete render
  matrix. Parsed workflow validation requires every named blocking gate in the
  publishing job before the first external publication action.
- Mutation fixtures reject omitted, reordered, unrelated-job, comment-only,
  and `continue-on-error` gates.
- The wider security audit found the old source-symbol vulnerability scan
  crashed on the dependency graph and concealed reachable advisories. The
  repository now runs package-level scanning plus binary-symbol scanning for
  all six shipped commands in CI and before publication.
- Go was raised to 1.25.12 in development, CI, release, and container builds;
  vulnerable text and OTLP dependencies were upgraded; and the deprecated
  Docker root SDK was replaced with the supported Moby API/client modules.
  The hardened scan reports no imported-package or reachable-binary
  vulnerabilities, and all four documented release images build.
- Integration resets now remove cross-component tenant children before tenant
  tables. This prevents order-dependent orphan creation and is covered by the
  focused autoscaling rerun and the complete end-to-end package.

### Release-integrity gap identified in sixth audit (closed 2026-07-26)

The release build does not pass the Dockerfile `VERSION` and `REVISION`
arguments, so published signed images can retain `dev` and `unknown` OCI
metadata. The chart consumes only a mutable repository/tag pair and cannot pin
the signed digest. CI does not build the browser or sandbox sidecars, and the
release contract does not define whether those runtime-adjacent images are
published, scanned, signed, or operator-built. The sandbox image also installs
unconstrained Python packages from a mutable base.

Required remediation:

- pass and inspect immutable release version/revision metadata;
- add digest-aware Helm rendering with tag-only development compatibility;
- define the complete image inventory and build/smoke-test/scan/SBOM/sign every
  published member;
- build every Dockerfile in CI even when an optional image is not published;
  and
- pin or hash sidecar base images and application dependencies according to
  the documented update policy.

### Sixth-audit closure evidence

- CI and release build all seven repository images and run a real smoke path
  for each. The runtime build receives version and revision arguments, and both
  OCI labels are inspected before publication.
- Helm renders `repository@digest` when `image.digest` is configured while
  retaining tag-based local development. `RELEASING.md` defines the published
  runtime image, optional operator-built images, example images, SBOM/signing
  scope, and vulnerability-exception review policy.
- Every external Docker base and external `COPY --from` image is digest-pinned.
  Python dependencies are exact-version constrained; example images use
  locked, multi-stage, non-root environments that do not resolve dependencies
  at startup.
- `TestReleaseWorkflowIsValidAndPinned`,
  `TestReleaseImagesAreDigestDeployableAndOptionalImagesConstrained`, Helm
  digest render tests, and workflow mutation tests enforce the contract. All
  seven images build, smoke-test, and pass the actionable high-severity Grype
  gate from the final source state.

### Supply-chain visibility gap identified in seventh audit (open 2026-07-26)

The workflows run Grype only with `--only-fixed`. Grype applies that option as
an ignore filter, so no-fix, wont-fix, and unknown-fix findings are absent from
the report. `RELEASING.md` currently says those findings remain visible for
operator review, which the workflow does not implement. The repository-level
exception is therefore not a substitute for a complete non-blocking report.

The reproducibility check covers Dockerfile `FROM` lines but not the external
images used by CI, Compose, GCP, or the bundled PostgreSQL chart. Examples
include major-only PostgreSQL tags and version tags without digests. These
mutable dependencies can change the validation or deployed system without a
source change and are outside the current image scan/SBOM inventory.

Required remediation:

- retain the blocking fixable-high scan, and also generate and publish a
  complete unfiltered vulnerability report or SBOM-linked risk report;
- make exception scope package/image-specific and verify its review/removal
  metadata structurally;
- inventory and digest-pin third-party workflow, Compose, GCP, and chart images
  where reproducibility is claimed, with an explicit local-development policy;
- scan or consume trusted attestations for deployed third-party images; and
- add mutation tests proving no-fix visibility and third-party image coverage
  cannot silently disappear.

### Seventh-audit closure evidence (2026-07-26)

- The blocking gate is unchanged: every `grype --fail-on high --only-fixed`
  invocation remains, in its own step, in both workflows. A separate later step
  now generates a complete UNFILTERED `grype -o json` report per image into
  `dist/`, and all seven reports are attached to the existing
  `gh release create` argument list — the repository's actual artefact idiom; no
  `actions/upload-artifact` or any other new action was introduced. The report
  step's `|| true` is scoped to its own step and cannot mask the gate.
- `RELEASING.md` now states plainly that `--only-fixed` is an ignore filter that
  genuinely cannot see no-fix findings, that the unfiltered report is
  non-blocking, and where the reports land — replacing the previous claim that
  such findings "remain visible" under a workflow that discarded them.
- `.grype.yaml` scopes the exception to one advisory and one package and carries
  owner / rationale / removal-trigger / review-by. These are YAML **comments,
  not mapping keys**, because grype v0.116.0 accepts unknown keys and then
  silently discards them (verified with `grype -c .grype.yaml config --load`);
  structured fields would have looked enforced without being so. The file says
  this in-line.
- Nine third-party images are digest-pinned across the Compose profiles and both
  workflows, including `pgvector/pgvector:pg16` — a major-only tag under which
  the database minor/patch could change silently. All digests were resolved with
  `docker buildx imagetools inspect` and independently re-verified against the
  live registry during review. The vendored Bitnami PostgreSQL subchart is
  pinned via a `postgresql.image.digest` values override rather than edited,
  since edits there are lost on the next `helm dep update`.
- Digest-pinning alone would have broken every CI and release run: pulling
  `repo:tag@digest` leaves the image untagged, so
  `docker ps --filter ancestor=pgvector/pgvector:pg16` returns empty and the
  following `test -n` fails closed. Both workflows now filter on
  `ancestor=<repo>@<digest>`.
- New structural assertions in `internal/doccheck` cover the unfiltered-report
  step, the exception metadata, and — added during final review —
  `TestPinnedThirdPartyDigestsDoNotDrift`, which fails when the same image is
  pinned to different digests across the six files that reference it. Each was
  mutation-tested, including by re-introducing the original `--only-fixed`
  behaviour.
- Commits `f33bc18`, `e12a36b`.

## RT-14: Restrict management metrics ingress

- [x] Implementation complete
- [x] Regression tests complete
- [x] Documentation complete
- [x] Final review complete

### Problem

The Helm control-plane NetworkPolicy allows port `9091` without a source
selector. The management metrics listener is unauthenticated and contains
fleet, tenant, model, usage, and cost-labelled series, so every workload in the
cluster can currently scrape data described as private.

### Required implementation

- split public control-plane API ingress from management-metrics ingress;
- allow metrics only from the control-plane pod itself and configurable
  monitoring peers;
- provide a secure same-namespace Prometheus selector by default;
- document cross-namespace Prometheus selector configuration; and
- add render and live denial/allowance checks.

### Required tests

- port `9091` is never rendered in an unrestricted ingress rule;
- the default metrics peer selects Prometheus without admitting arbitrary pods;
- configured namespace and pod selectors render exactly; and
- the live acceptance test reads metrics from an allowed source and proves an
  unrelated agent source is denied.

### Implementation evidence

- Port `8080` and port `9091` are separate ingress rules. The metrics rule
  admits the release control-plane pod and configurable
  `networkPolicy.metricsIngress` peers only.
- The secure default selects same-namespace Prometheus pods by their standard
  application label. Render tests cover the default and explicit namespace/pod
  selectors and ensure the metrics port appears only in its Service and
  restricted policy.
- Observability and chart documentation explain same- and cross-namespace
  monitoring and warn against an empty namespace selector.
- The two-release live script checks denied source-agent metrics access and
  allowed target-control-plane access. It is syntax-checked but unexecuted
  without a designated cluster, so regression and final review remain open.

### Gap identified in fourth follow-up audit (closed 2026-07-25)

The live metrics-denial assertion has the same false-positive design described
under RT-04: any source-side `kubectl debug` or `curl` failure is accepted as
NetworkPolicy enforcement. The later allowed scrape from the target control
plane proves the target path, but it does not prove that the source debug
container started successfully, resolved the metrics Service, and was denied
specifically by policy.

Required remediation:

- share the corrected readiness, resource-discovery, source-baseline, and
  exit-classification harness with RT-04;
- require a genuine source-side policy denial rather than an arbitrary command
  failure; and
- cover allowed monitoring/control sources and unrelated-agent denial in both
  hermetic script tests and the two-release live acceptance run.

### Fourth follow-up code closure evidence

- RT-14 uses the same readiness-gated, resource-checked probe harness described
  under RT-04. A source-side denial is accepted only for curl timeout exit 28
  after the source baseline has succeeded.
- The target control-plane baseline verifies that the restricted metrics
  Service is reachable and contains `runtime_agent_up=1`; the source agent's
  request to that same Service must time out.
- Hermetic tests reject leaked metrics, DNS failure, connection refusal,
  image-pull failure, missing resources, missing markers, and unready pods.
- The code and hermetic regression gap is closed. Live allowance and denial
  against two installed releases remain the parent issue's outstanding
  acceptance requirement.

### Live Kubernetes acceptance evidence (2026-07-26)

Ran on a two-release `kind` cluster (`runtime-alpha`, `runtime-beta`, namespace
`rt`), each a one-agent `perAgentPods` release with its own restricted database
role and distinct agent id.

- `live-networkpolicy-test.sh rt runtime-alpha runtime-beta` — **OK**, and the
  reverse direction (`runtime-beta runtime-alpha`) — **OK**.
- Negative control: with `networkPolicy.enabled=false` on the target release the
  same harness FAILS with `agent alphaagent to agent betaagent unexpectedly
  succeeded`. The pass therefore measures policy enforcement, not an
  unreachable endpoint.
- Getting there required fixing a total `perAgentPods` startup regression and a
  harness defect that meant it had never run to completion; see `4d4484c`.

## RT-15: Bound signed-request replay-cache work

- [x] Implementation complete
- [x] Regression tests complete
- [x] Documentation complete
- [x] Final review complete

### Problem

Every authenticated signed request locks the replay cache and scans every nonce
seen during the 30-second validity window. Work therefore grows linearly with
recent request volume while all signed requests serialize on the same lock.
The map also has no explicit capacity bound.

### Required implementation

- replace per-request full-map cleanup with constant-time rotating expiry
  buckets or another bounded TTL structure;
- enforce an explicit fail-closed capacity limit;
- preserve replay rejection across bucket rotation; and
- document the bounded-cache failure behaviour operationally.

### Required tests

- duplicate nonces are rejected in the current and previous validity bucket;
- sufficiently expired nonces are evicted without scanning the cache;
- the capacity limit rejects additional requests without growing memory; and
- a benchmark covers insertion and replay lookup at a populated-cache size.

### Completion evidence

- Signed requests use two rotating maps with constant-time lookup/insertion and
  whole-bucket expiry. There is no per-request nonce-map scan.
- The cache retains nonces beyond the full past/future signature-skew window,
  preserves protection across backward clock adjustments, and fails closed at
  131,072 entries.
- Unit and race tests cover current/previous buckets, expiry, capacity, clock
  movement, and concurrent duplicates. The populated-cache benchmark completes
  at approximately 167 ns/op on the validation host.
- Configuration and chart documentation describe the bounded fail-closed
  behaviour.

## RT-16: Bind registration credentials to an immutable tenant trust domain

- [x] Implementation complete
- [x] Regression tests complete
- [x] Documentation complete
- [x] Final review complete

### Problem

Registration tokens persist only `token_id`, `agent_id`, and a password hash.
The handshake resolves that reusable agent ID in the live registry and returns
the current agent process's environment, including its restricted database DSN
and all brokered secrets for the current tenant. Deleting a dynamically managed
agent does not revoke its tokens. If the same globally unique agent ID is later
created under another tenant, every still-active old token becomes a credential
for the replacement tenant.

There is a second stale-identity path in the admin API. `buildRoot` passes
`Registry.AgentTenants()` to `RegisterAdmin` once at startup, and the handlers
retain that map snapshot. A dynamically added agent cannot be minted a token,
while an agent deleted or reassigned after startup retains its old tenant
mapping for mint, list, and revoke authorization. An administrator from the old
tenant can therefore mint a new token for the reassigned ID; `/register` then
uses the replacement registry entry and returns the new tenant's secrets.

### Required implementation

- persist the immutable tenant (and, if IDs may be reused within a tenant, an
  agent-instance generation) with every registration token;
- make the verifier return the token's bound trust domain and require an exact
  match with the live agent before producing any environment data;
- revoke or invalidate all tokens when a managed agent is deleted or its
  identity generation changes;
- replace the startup tenant-map snapshot with a concurrency-safe live lookup,
  while authorizing token list/revoke operations from the token's immutable
  tenant rather than the agent's current tenant; and
- migrate existing tokens fail closed, requiring operators to mint replacements
  where their original tenant cannot be established safely.

### Required tests

- a token minted before delete/recreate cannot register an agent with the same
  ID in another tenant;
- an old-tenant administrator cannot mint, list, or revoke credentials for a
  reassigned agent, while the new tenant can;
- dynamically added and removed agents are reflected immediately without
  restarting the control plane;
- concurrent registry mutation and token administration are race-free; and
- a valid same-instance token still returns only its bound tenant's environment,
  with migration coverage for legacy token rows.

### Required documentation

- document token tenant/generation binding, invalidation on agent deletion, and
  the operator migration/rotation procedure;
- state explicitly that possession of an old token cannot follow an agent ID
  into a different tenant; and
- update the registration-handshake and security guidance with the resulting
  lifecycle guarantee.

### Completion evidence

- Registration tokens now persist immutable `tenant_id` and
  `agent_generation` bindings. Verification returns those bindings and the
  registration handler compares both with the live registry entry before
  releasing any environment value.
- Dynamically managed agents receive a new random generation at creation.
  Deletion revokes every token for that exact tenant, agent, and generation
  before removing the agent, so delete/recreate cannot revive an old
  credential.
- Registration administration resolves agent identity through a
  concurrency-safe live registry lookup. Mint authorization follows the live
  identity, while list and revoke authorization use each token's immutable
  tenant binding.
- Version-2 identity and managed-agent migrations add the binding columns.
  Legacy token rows are left with empty bindings and cannot authenticate;
  legacy managed agents receive a stable migration generation so operators can
  mint replacements.
- Handshake, admin, registry-race, deletion/recreation, memory, and tagged
  PostgreSQL migration tests cover cross-tenant reuse, live identity changes,
  legacy fail-closed behaviour, exact-instance success, and concurrent
  mutation. Unit, race, and integration suites pass.
- The operator, configuration, security, and chart documentation describe
  tenant/generation binding, deletion invalidation, and the legacy-token
  rotation procedure.

### Gap identified in fifth follow-up audit (closed 2026-07-26)

Managed agents receive a random persisted generation, but operator-configured
agents use the deterministic value `config:<tenant>:<agent-id>`. Removing and
later reintroducing a file/Helm-configured agent with the same tenant and ID,
or replacing its endpoint while retaining that identity tuple, reproduces the
same generation. Every old registration token becomes valid for the
replacement instance. This is especially relevant to Helm `perAgentPods`,
whose agents are file-configured remotes and whose registration handshake is a
documented lifecycle path.

The tests exercise dynamic managed-agent delete/recreate and manually supplied
generation mismatch, but none proves that a file-configured or Helm agent
instance receives a non-reusable persisted generation. Documentation describes
tokens as bound to one immutable agent instance without explaining this
exception.

Required remediation:

- give registration-capable file/Helm agents an explicit persisted instance
  generation that survives ordinary restarts but changes on intentional
  remove/recreate or replacement;
- include that generation in chart configuration/Secrets and operator rotation
  procedures rather than deriving it solely from tenant and agent ID;
- fail old deterministic config generations closed through a migration or
  explicit compatibility window; and
- test Helm/file-agent removal, reinstallation, endpoint replacement, stable
  restart, and old-token rejection.

### Fifth follow-up closure evidence

- Every registration-capable local or remote config now requires an explicit
  lifecycle generation. Deterministic derived generations are rejected.
- Dynamic and persisted managed generations survive ordinary restart and
  rotate on recreation. File, Compose, GCP, and Helm configurations supply
  operator-controlled generation material, with documented rotation on
  replacement.
- Token mint, verify, register, list, revoke, and deletion enforce exact tenant
  and generation. Legacy unbound tokens and generation-less configurations
  fail closed.
- Config, registry, admin, handshake, memory, PostgreSQL migration, Helm
  render, stable-restart, endpoint-replacement, and old-token rejection tests
  pass.

## RT-17: Make browser egress an enforceable network boundary

- [x] Implementation complete
- [x] Regression tests complete
- [x] Documentation complete
- [x] Final review complete

### Problem

Browser policy resolves a hostname during authorization but the HTTP client or
CONNECT dial resolves it again. DNS can change between those operations,
allowing a rebinding target to bypass the checked address. The
`allow-all-public` classifier blocks only common private ranges and admits
carrier-grade NAT, documentation, benchmark, multicast, reserved, and other
non-public ranges. The proxy also defaults to an unauthenticated wildcard
listener, has no explicit request concurrency or header bounds, and creates
unbounded tunnels without idle or absolute lifetime limits.

### Required implementation

- resolve and validate every target immediately on the dial path and connect
  only to an already-validated address;
- reuse the repository-wide complete public-IP classification;
- reject mixed public/private DNS answer sets and revalidate every new
  connection;
- bind privately by default and require an explicit shared secret whenever a
  non-loopback listener is configured;
- bound request concurrency, header reads, upstream response bytes, CONNECT
  concurrency, idle duration, and absolute tunnel lifetime; and
- preserve deny-all and allow-list hostname semantics without trusting caller
  headers or environment proxy settings.

### Required tests

- authorization and dial cannot observe different DNS answers;
- every private, local, CGNAT, documentation, benchmark, multicast, reserved,
  IPv4-mapped, and mixed-answer target is rejected;
- a non-loopback listener without authentication is rejected at startup;
- missing or invalid proxy credentials are rejected;
- request and tunnel saturation fail predictably; and
- idle and absolute tunnel deadlines close both directions.

### Completion evidence

- HTTP and CONNECT dials resolve once, reject mixed or non-public answer sets,
  and connect only to a validated numeric address. The shared public-IP policy
  covers special-purpose IPv4, IPv4-mapped, and IPv6 ranges.
- The listener defaults to loopback. A non-loopback bind requires a
  sufficiently strong token, and Chromium supplies that credential only for a
  CDP-reported proxy challenge. It is never attached to origin requests.
- Independent semaphores and deadlines bound requests, tunnels, headers,
  upstream response bytes, idle connections, and absolute tunnel lifetime.
  Focused policy, dial-pinning, authentication, saturation, idle, and lifetime
  tests pass under the race detector.
- `TestLiveBrowseAndEgress` also passes with the real Docker/Chromium path,
  proving authenticated public browsing succeeds and private destinations are
  denied. `gateway-and-sandboxes.md` describes the boundary and remaining
  deployment responsibility.

### Deployment-boundary gap identified in seventh audit (open 2026-07-26)

The turnkey Compose profile places browser containers and `runtimed` on the
internal `runtime_browser-control` network and advertises the proxy as
`runtimed`, but its rendered environment supplies neither
`RUNTIME_BROWSER_PROXY_ADDR` nor `RUNTIME_BROWSER_PROXY_TOKEN`. `browserd`
therefore binds the proxy to `127.0.0.1`, which a sibling browser container
cannot reach over the private network. The real-browser test uses a
host-published test proxy and does not exercise this supported topology.

Outside the internal Compose network, the boundary still relies on Chromium
honouring the proxy. The browser container retains a routable Docker network,
so non-proxy traffic or a compromised browser process is not denied below the
application layer. The package comment calls this follow-on hardening while
the issue title and documentation describe the proxy as the complete reachable
surface.

Required remediation:

- provision a generated or secret-backed proxy token and a reachable private
  bind address in every supported containerised deployment;
- add a real turnkey-network test that starts `browserd` in its deployment
  topology and proves authenticated allowed browsing plus private/direct
  denial;
- enforce egress at the container/network layer, or explicitly narrow the
  security guarantee and disable browser networking modes that can bypass an
  HTTP proxy; and
- test startup, restart, token rotation, proxy failure, and attempts to reach
  internal services without the proxy.

### Seventh-audit closure evidence (2026-07-26)

- Two distinct defects are fixed. **Functional:** the turnkey profile set
  `RUNTIME_BROWSER_PROXY_HOST` but not `RUNTIME_BROWSER_PROXY_ADDR`, so
  `browserd` bound `127.0.0.1:0` while the browser container was handed
  `runtimed:<port>` — an unreachable listener — and, being loopback, required no
  token, so the proxy also ran unauthenticated. The bind default is now
  deployment-aware: an explicit address wins; no private network keeps
  `127.0.0.1:0`; a configured network selects `0.0.0.0:0`, which the unchanged
  `ValidateProxyListener` then requires a >=32-character token for.
- **Enforcement:** `RUNTIME_BROWSER_NETWORK` was previously cast straight to
  `container.NetworkMode` with no validation. `NewDockerBackend` now inspects
  the network and fails closed when it is absent, cannot be inspected, or is not
  declared `internal`. An internal network has no default route, so a browser
  process that ignores `--proxy-server` cannot reach the internet at all. An
  empty network still selects the unchanged direct-host install.
- The proxy token continues to reach Chromium only over CDP
  (`Fetch.authChallenge`); it is never placed in container environment.
- `TestLiveInternalNetworkEgressTopology` RAN against real Docker (19.6 s, not
  skipped) and proves the layer-3 claim with a **positive control**: a routable
  container reaches `1.1.1.1:53` (exit 0) while the internal-network container
  gets `Network is unreachable` (exit 1). It rejects non-internal and absent
  networks at construction, and drives the token-authenticated proxy over the
  internal network for 200/407/403. The test `t.Log`s the two things it does
  **not** cover rather than implying total coverage: the proxy leg is exercised
  with raw HTTP rather than Chromium (a macOS Docker Desktop host cannot dial
  container IPs on a user-defined network), and the internal network is a shared
  segment.
- **Residual reachability, stated rather than glossed:** the proxy host and any
  sibling container the operator attaches to the same internal network remain
  reachable from the browser at layer 3, bounded by the proxy's policy and by
  deployment hygiene. `gateway-and-sandboxes.md` and the `internal/browser`
  package comment now say this and no longer defer it as "follow-on hardening".
- **No deployment change is required.** An earlier revision of this fix made
  `RUNTIME_BROWSER_PROXY_TOKEN` a mandatory Compose variable, generated in five
  places and carrying an upgrade hazard (`compose-init --force` would rotate
  `RUNTIME_SECRETS_KEYS` under the same key id and leave sealed secrets
  undecryptable). That was a design error: the credential is process-internal —
  `browserd` serves the proxy that demands it and answers the demand itself over
  CDP — so no operator, deployment, or other component ever needs the value.
  `browserd` now mints an ephemeral token per start when the bind requires one
  and none is configured. The variable remains an override for a pinned value,
  and a configured-but-weak token is still rejected rather than silently
  replaced, so the fail-closed rule is unchanged. A per-start token is also
  stronger than a static one on disk.
- Commits `8e85787`, `c81f432`.

## RT-18: Own asynchronous memory-ingestion lifecycle

- [x] Implementation complete
- [x] Regression tests complete
- [x] Documentation complete
- [x] Final review complete

### Problem

Memory ingestion launches detached goroutines on `context.Background()`.
Although a semaphore bounds concurrency, the service cannot cancel, drain, or
wait for the workers before closing their backing store. Shutdown can therefore
race writes against store closure, lose accepted ingestion without an explicit
outcome, or wait only on dependency-specific client timeouts.

### Required implementation

- give each process-shared knowledge graph a lifecycle context, cancel
  function, wait group, and idempotent close/drain operation;
- derive every extraction, embedding, search, summary, episode, and save call
  from that lifecycle context;
- reject new ingestion after closing starts;
- bound graceful drain and cancellation waits and report dropped/timed-out
  work; and
- invoke the drain before closing the memory store.

### Required tests

- shutdown waits for a cooperative accepted worker;
- cancellation reaches a blocked dependency;
- a non-cooperative dependency cannot block shutdown forever;
- ingestion after close is rejected without launching work;
- the backing store is not closed before ingestion drains; and
- concurrent close and ingest are race-free.

### Completion evidence

- Each process-shared knowledge graph owns a lifecycle context, cancel
  function, admission lock, wait group, and closed gate. Accepted work derives
  extraction, embedding, search, strategy, summary, episode, and save calls
  from that lifecycle.
- `Close` first stops admission, then allows a bounded cooperative drain,
  cancels remaining dependencies, and returns after a second bounded wait.
  `agentruntime.Serve` invokes this drain before returning; `agentd` owns the
  memory database handle outside `Serve`, so its deferred close necessarily
  occurs afterwards.
- `TestKGCloseDrainsCooperativeWorker`,
  `TestKGCloseCancelsBlockedWorkerAndIsBounded`,
  `TestKGCloseDoesNotWaitForeverForNonCooperativeWorker`,
  `TestKGRejectsIngestAfterClose`, and
  `TestKGConcurrentCloseAndIngestIsRaceFree` cover the shutdown matrix. The
  memory package and complete concurrency-heavy package set pass with the race
  detector.

### Store-ownership gap identified in seventh audit (open 2026-07-26)

The bounded non-cooperative path returns from `KG.Close` while its worker is
still running. `agentruntime.Serve` logs that error and returns; `agentd` then
closes the database handle even though the detached worker can later continue
into search or save. The completion statement that the store necessarily
closes after ingestion drains is therefore true only for cooperative
dependencies and contradicts the non-cooperative test.

Required remediation:

- make the drain result explicitly distinguish fully stopped workers from
  detached work;
- retain ownership of every backing resource until all possible users have
  stopped, or move non-cooperative work behind a killable process boundary;
- prevent a timed-out worker from beginning any new store call after shutdown
  advances to resource closure;
- record accepted, dropped, cancelled, detached, and completed work
  consistently; and
- add a store-ordering test that releases a deliberately non-cooperative
  dependency only after the first close deadline and proves no call reaches a
  closed store.

### Seventh-audit closure evidence (2026-07-26)

- `KG` gained a `storeDetached` latch guarded by the existing `lifecycleMu` (no
  second mutex, and the lock is never held across a store call or across
  `workers.Wait()`). `Close` sets it on the path that abandons a
  non-cooperative worker, so that worker's next store call is refused instead of
  reaching a handle its owner is about to close. All eight background store
  touches are gated; the foreground `Recall`/`recallForSession` path is
  deliberately NOT gated, since gating it would break recall permanently after
  any drain.
- Outcomes are now distinguishable via `errors.Is` sentinels
  (`ErrIngestionDetached`, `ErrIngestionDrainTimeout`) consumed at
  `agentruntime/serve.go`, so an operator can tell a slow drain from an
  abandoned worker. `Close(timeout) error` keeps its signature, so the
  `cfg.DrainMemory` seam is untouched.
- Also fixed: `runStrategies` degraded a nil lifecycle to `context.Background()`,
  producing an uncancellable worker. It now returns without work.
- `TestKGDetachedWorkerCannotTouchStoreAfterClose` uses a sentinel store that
  fails if called after close, and releases the non-cooperative dependency only
  after `Close` has returned and the owner has closed the store. It was verified
  by mutation twice independently: neutering the gate to `return true` produces
  "detached worker reached the store after it was closed". Two further tests
  cover the strategy pipeline, which the original brief left unproven.
- The nil-lifecycle fix exposed a silent coverage loss: eight strategy tests
  build bare `&KG{}` literals bypassing admission, and several began passing
  vacuously (they assert "nothing was saved", which a no-op satisfies). All
  eight were given a real lifecycle rather than weakening the guard.
- **Residual, documented rather than hidden:** a worker that passes the gate and
  is then preempted can still be inside one store call when `Close` latches.
  That is intrinsic to a gate — holding the mutex across the call would deadlock
  `Close` — and narrows exposure from unbounded to a single in-flight call. The
  `mayTouchStore` doc comment says so.
- `internal/memory` and `agentruntime` pass under `-race`.
- Commits `24c0a48`, `cc55631`.

## Final acceptance review

- [x] Every RT-01 through RT-18 checkbox is complete, RT-04 and RT-14
  included: the two-release live-cluster acceptance ran and passed on kind.
- [x] Focused regression tests exist and pass for every sixth-audit local gap.
- [x] Focused regression tests exist and pass for every seventh-audit gap.
  Each was verified by mutation — reverting the fix makes the test fail with its
  intended message — rather than merely observed to pass.
- [x] Full Go unit tests pass.
- [x] Race tests pass for concurrency-heavy packages.
- [x] PostgreSQL integration tests pass.
- [x] Python shim tests pass.
- [x] Helm lint and render tests pass.
- [x] Documentation checks pass.
- [x] Dependency and shipped-binary security scans pass. All seven repository
  images build, pass their smoke paths, and pass the actionable high-severity
  image scan.
- [x] Every supplied Compose topology renders with its documented required
  values.
- [x] The shell gate passes (`--severity=warning`, exit 0). Previously red, and
  before that wrongly recorded as unavailable.
- [x] A fresh code review finds no unresolved local implementation,
  documentation, or hermetic-test requirement. The seventh-audit remediation
  received a whole-branch review covering cross-task interactions, not only
  per-commit diffs; it found and fixed a release-workflow break the branch
  itself introduced, and confirmed no sibling of that class remains.
- [x] The shared RT-04/RT-14 harness passes against two installed one-agent
  Kubernetes releases, in both directions, with a negative control proving the
  pass is not vacuous.
- [x] `git diff --check` is clean and unrelated user files remain untouched.

## Seventh-audit remediation record

Implemented and validated on 2026-07-26 (branch `phase14-seventh-audit`,
`449cbf1..8431eaa`, 12 commits):

- Gates run from the final source state: `make check` (fmt, vet, 27 unit
  packages) PASS; `make test-integration` (full end-to-end plus tagged
  PostgreSQL store/eval/memory/identity/agentstore/gateway) PASS; `-race` PASS
  for `internal/{memory,store,xfan,obs,browser}`, `controlplane`, `console`,
  `agentruntime`; `make helm-lint` and the chart suite PASS; every supplied
  Compose profile renders; the shellcheck container gate exits 0;
  `git diff --check` clean.
- The live browser topology test RAN against real Docker rather than skipping,
  and proves its layer-3 claim with a positive control.
- Three defects were found by review rather than by the audit, and each is
  recorded above with its issue: two name-preserving preflight bypasses
  (RT-10), and a release-workflow break this branch itself introduced — making
  `RUNTIME_BROWSER_PROXY_TOKEN` mandatory updated `ci.yml` but not
  `release.yml`, which would have failed the next tagged release at its Compose
  validation step. That was reproduced with the release step's exact
  environment before being fixed.
- Separately hardened: `deploy/gcp/llm.env` (real API keys),
  `IMPLEMENTATION-*.md`, `P2.2-REPORT.md`, and `food_label_images/` were
  untracked but NOT gitignored, so a single `git add -A` would have committed
  live credentials. They are now ignored.
- Still open, unchanged: F6, RT-04, and RT-14 require two installed one-agent
  Kubernetes releases. No cluster is available here, and no summary in this
  register treats that evidence as obtained.

## Seventh-audit review record

Reviewed commit `449cbf1` on 2026-07-26:

- Rendered turnkey Compose contains the private browser network and advertised
  proxy host but omits the reachable bind address and proxy credential.
- Restricted schema preflight checks object presence/counts, not policy,
  trigger, or foreign-key semantics.
- Static HTTP-client inventory finds unbounded provider and conformance JSON
  responses; fan-out inventory finds one goroutine per registry/session member
  without a shared concurrency ceiling.
- The non-cooperative ingestion test proves bounded return, but the service
  lifecycle subsequently permits its database owner to close while that worker
  remains detached.
- Release workflows filter out all no-fix/unknown-fix image findings while the
  release guide says they remain visible, and deployed third-party images are
  outside digest/scanning enforcement.
- Existing green suites remain valid regression evidence for the behaviours
  they exercise. They do not close these newly specified adversarial
  invariants.

## Sixth-audit validation record (superseded)

Reviewed and executed on 2026-07-26:

- `make check` passes under Go 1.25.12.
- Race detection passes for control-plane, agent runtime, browser, gateway,
  identity, core store, evaluation, memory, bounded HTTP reads, and `runtimed`
  concurrency paths.
- `make test-integration` passes the complete end-to-end package in 269.3
  seconds and then all tagged PostgreSQL store, evaluation, memory, identity,
  managed-agent, and gateway packages.
- The Python shim reports 32 passing tests. Helm lint, the full chart render
  matrix, and the hermetic RT-04/RT-14 failure-discrimination harness pass.
- Full, turnkey, secured, and distributed GCP Compose configurations render.
  Runtime, sandbox, browser, embedder, OpenAI, Claude, and food-label images
  build and pass their documented smoke paths.
- Package and shipped-binary scanning report no reachable vulnerabilities; one
  advisory remains only in an uncalled transitive module path. All seven built
  images pass the actionable high-severity Grype gate; the single no-fix
  exception is scoped, justified, and removal-triggered in `.grype.yaml`.
- At the time of this run, the live two-release Kubernetes test and local
  `shellcheck` were the known unexecuted gates. The seventh-audit record above
  supersedes the earlier conclusion that they were the only remaining gaps.

## Fifth-audit review record

Reviewed on 2026-07-25:

- RT-01 is reopened because tenant authorization and replica selection use
  separate mutable registry reads. Delete/recreate can make an authorized
  request target a different tenant generation.
- RT-03 is reopened because durable external affinity omits the agent
  generation. Same-tenant ID reuse or endpoint replacement can route an old
  session binding to a replacement store.
- RT-05 is reopened because restricted PostgreSQL roles are tenant-scoped, not
  agent-scoped, and share DBOS objects. Two agents in one tenant and database
  are not isolated by the documented role boundary.
- RT-08 is reopened because queue-full and shutdown rejection can skip durable
  terminal failure classification even though the evaluation guide promises
  classification for every terminal native session.
- RT-09 is reopened because session retention does not operate on one stable,
  locked candidate set and can partially delete a concurrently touched
  session's children. Evaluation retention also ages terminal runs from
  `created_at` rather than `finished_at`.
- RT-13 is reopened because CI and release do not execute the declared Helm
  lint gate, while the workflow regression checks global text rather than the
  ordering and blocking status of publication prerequisites.
- RT-16 is reopened because file- and Helm-configured agent generations are
  deterministic. Removing and recreating the same tenant/agent tuple can make
  an old registration token valid for a replacement instance.
- RT-04 and RT-14 remain open solely for their shared two-release live
  Kubernetes NetworkPolicy acceptance. Their hermetic harness is present.
- RT-02, RT-06, RT-07, RT-10, RT-11, RT-12, and RT-15 remain closed after
  source, test, deployment, and documentation review.
- Existing green suites establish that the already-covered behaviour has not
  regressed. They do not close the fifth-audit findings because the required
  adversarial and concurrency cases are not represented.
- Focused existing tests for `controlplane`, `agentruntime`, `internal/store`,
  `internal/eval`, `internal/identity`, and `internal/doccheck` pass.
  `git diff --check` is clean.

## Fourth-audit validation record

Reviewed and executed on 2026-07-25:

- Focused registration, live-registry, managed-agent lifecycle, evaluation
  state-machine, identity migration, agent-store migration, and NetworkPolicy
  harness regressions passed.
- `make check` passed formatting, vet, documentation checks, and all Go unit
  packages.
- `make test-integration` passed the complete end-to-end package and tagged
  PostgreSQL store, evaluation, memory, identity, and agent-store packages.
- Race tests passed for `controlplane`, `internal/eval`, `internal/identity`,
  `internal/agentstore`, and `console`.
- The complete Helm render suite, hermetic NetworkPolicy harness matrix, and
  Helm lint passed.
- The corrected live NetworkPolicy harness was not run against Kubernetes
  because no target cluster and two installed one-agent releases were supplied.
  This is the sole remaining acceptance boundary for RT-04 and RT-14.

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

## Third follow-up remediation validation record

Reviewed and executed on 2026-07-25 after the third-audit fixes:

- `make check` passed formatting, `go vet`, documentation validation, and all
  hermetic Go packages.
- `make test-integration` passed the complete end-to-end package in 253.775
  seconds plus tagged PostgreSQL store, eval, memory, and identity packages.
  An initial run exposed missing explicit provisioning for a
  registration-managed remote; that path was corrected and both its focused
  test and the complete target then passed.
- Race tests passed for `agentruntime`, `cmd/runtimed`, `internal/eval`, and
  `controlplane`.
- The Python contract shim passed 32 tests with one upstream
  Starlette/httpx deprecation warning.
- Helm lint/render security cases, custom retention/concurrency values,
  management-ingress selectors, and remote-role provisioning passed. Changed
  shell scripts passed `bash -n`.
- Base, turnkey, secured, distributed GCP control-plane, and all GCP agent
  Compose profiles passed `docker compose config --quiet`.
- The replay cache benchmark completed at approximately 167 ns/op with 10,000
  pre-populated entries.
- `git diff --check` passed. The unrelated implementation reports, local GCP
  environment file, and food-label images remain untouched.
- No live Kubernetes target was supplied. The corrected two-release
  cross-agent and management-metrics acceptance test remains the only
  unexecuted requirement in RT-04 and RT-14.
