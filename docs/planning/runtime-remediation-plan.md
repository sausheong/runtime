# Runtime Remediation Implementation Plan

This plan implements
[the remediation issue register](runtime-remediation-issues.md). Work is
ordered so that persistence and identity invariants land before routing,
deployment, and operational refinements that depend on them.

Status: second-audit code remediation complete. Phases 1 through 7 record the
first two remediation passes. Phase 8 closes every second-audit code gap; the
RT-04 live Kubernetes acceptance check remains pending because this workspace
has no configured cluster or designated release.

## Phase 1: Persistence and routing correctness

### 1. Version the schema and add tenant-owned session records

1. Introduce the migration ledger and migration runner required by RT-10.
2. Convert current package schemas into a deterministic current-version
   baseline while retaining safe first install.
3. Add session `tenant_id`, an explicit external-session binding shape, and a
   stable not-found sentinel.
4. Update memory and PostgreSQL stores.
5. Thread tenant ownership through native-agent session creation and all
   control-plane session routing.
6. Add store, routing, reassignment, and PostgreSQL integration tests.

Closes: RT-01, RT-06, and the persistence foundation of RT-03/RT-09/RT-10.

### 2. Persist external-agent affinity

1. Capture the chosen replica for a new remote or command-agent session.
2. Bind the returned external session ID to tenant, agent, and replica.
3. Route follow-up requests through the binding.
4. Preserve existing single-replica behaviour and return an explicit error
   where an old unbound pooled session cannot be routed safely.
5. Add independent-store remote and command-pool tests, including restart.

Closes: RT-03.

### 3. Repair remote health monitoring

1. Refactor monitor keys and lifecycle operations to include replica index.
2. Make all-down state explicit in routing.
3. Replace the false-positive integration assertion with a convergence test
   that waits for a health transition and rejects all dead-ordinal traffic.
4. Add unit tests for independent start, stop, restart, and metrics.

Closes: RT-02.

## Phase 2: Deployment trust boundaries

### 4. Separate database roles

1. Add bootstrap SQL for control-plane and restricted agent roles.
2. Update Compose, secured, GCP, and Helm configuration to carry distinct
   credentials.
3. Make production-oriented startup fail closed when identity is enabled and
   credentials are shared.
4. Add privilege-denial and native-recovery integration tests.

Closes: RT-05.

### 5. Authenticate and isolate per-agent pods

1. Require per-agent authentication in `perAgentPods`.
2. Generate or reference one credential per agent.
3. Sign forwarded identity claims with short expiry and verify them agent-side.
4. Render per-agent ingress policies limited to the control plane and probes.
5. Add Helm and live-cluster tests.

Closes: RT-04.

## Phase 3: Evaluation and lifecycle hardening

### 6. Durable evaluation workers

1. Extend evaluation run state with claim owner and lease expiry.
2. Implement checked transitions and lease acquisition.
3. Introduce a bounded worker executor for submitted and recovered runs.
4. Count metrics only after committed state.
5. Add transition-failure, concurrency, lease-expiry, and bound tests.

Closes: RT-07.

### 7. Bounded online scoring

1. Add a lifecycle-owned scoring queue and workers.
2. Apply scoring deadlines and queue-full policy.
3. Drain workers during shutdown.
4. Move persistence-aware metrics behind successful writes.
5. Add concurrency, cancellation, saturation, and persistence tests.

Closes: RT-08.

### 8. Retention and privacy

1. Add terminal-session/event retention with cascade-safe schema.
2. Add configurable live-memory retention by kind.
3. Add bounded batches, dry-run mode, and metrics.
4. Add transcript capture disablement and custom filtering.
5. Expand deterministic credential redaction fixtures.
6. Document privacy and backup responsibilities.

Closes: RT-09 and RT-12.

## Phase 4: HTTP and operational hardening

### 9. Request resource controls

1. Add route-aware request-body deadlines.
2. Add request and SSE concurrency limits.
3. Apply strict JSON decoding to the agent session contract.
4. Replace client-visible internal errors with generic responses and structured
   logs.
5. Add timeout, SSE, saturation, decoding, and information-disclosure tests.

Closes: RT-11.

## Phase 5: Release and documentation

### 10. Complete the public operational contract

1. Add security-reporting, contributing, ownership, changelog, and release
   documentation. Leave licence selection explicitly unresolved rather than
   choosing terms on the owner's behalf.
2. Synchronize Helm documentation with `Chart.yaml`.
3. Add a release workflow that builds immutable versioned image/chart
   artefacts and produces SBOM/provenance metadata.
4. Correct capability and limitation statements after implementation.
5. Extend documentation tests for version consistency and the issue register.

Closes: RT-13 and documentation checkboxes for all issues.

## Phase 6: Verification and final review

1. Run focused tests for every issue.
2. Run formatting, vet, full unit, race, PostgreSQL integration, Python,
   documentation, and Helm suites.
3. Review the final diff for tenant-boundary, failure-mode, migration, and
   compatibility regressions.
4. Re-read every issue and required test in the register.
5. Mark an issue complete only when its implementation, regression tests,
   documentation, and final-review checkbox are all satisfied.
6. Record any deliberately deferred requirement as still open; do not convert a
   limitation into a completed checkbox merely because it is documented.

## Phase 7: Follow-up gap remediation

### 11. Enforce child-record tenant integrity

1. Derive transcript and online-evaluation ownership from the parent session.
2. Add database constraints or triggers that reject tenant divergence even for
   direct SQL.
3. Add memory-store, PostgreSQL, and restricted-role mismatch tests.

Reopens: RT-01.

### 12. Complete external-affinity lifecycle handling

1. Persist a newly created upstream binding with a short internal context that
   survives client cancellation.
2. Refresh external binding activity on every successfully routed session
   request.
3. Exclude external bindings from active native-session capacity accounting.
4. Reap inactive external bindings in bounded retention sweeps.
5. Add restart, cancellation, touch, accounting, and expiry tests.

Reopens: RT-03 and RT-09.

### 13. Replace bearer-derived identity assertions

1. Introduce Ed25519 request signing with a control-plane private key and
   agent-only public verification key.
2. Bind the assertion to method and request target and include a random nonce.
3. Reject nonce replay for the accepted timestamp window.
4. Make subject forwarding fail closed when the relevant key is absent.
5. Update deployment configuration, Helm validation, documentation, and
   request-tamper/replay tests.

Reopens: RT-04.

### 14. Reduce restricted database authority

1. Remove `CREATE` on `public` and unnecessary access to global agent metadata.
2. Limit DBOS schema/object grants to the minimum the runtime actually needs.
3. Fail closed when a deployment attempts to share a restricted role across an
   unsupported multi-agent trust domain.
4. Extend restricted-role tests for same-tenant isolation and DDL denial.

Reopens: RT-05.

### 15. Make evaluation execution continuously recoverable

1. Replace one-shot startup recovery with a lifecycle-owned periodic bounded
   scanner.
2. Renew claimed-run leases while a provider or judge call is in flight.
3. Add compare-and-swap pending-to-error finalization for saturated queues and
   check every transition result.
4. Add leased-at-start, long-call, saturation-write-failure, and shutdown tests.

Reopens: RT-07.

### 16. Guarantee bounded scoring shutdown

1. Bound the post-cancellation worker wait.
2. Log and count workers that fail to stop cooperatively.
3. Add a non-cooperative provider/store test proving shutdown still returns.

Reopens: RT-08.

### 17. Standardize retention sweep budgets

1. Use bounded batches for sessions, evaluations, and memory.
2. Repeat batches immediately only while a per-sweep deadline and batch budget
   remain.
3. Preserve active/recoverable work and add backlog/budget regression tests.
4. Document the sweep and backlog semantics consistently.

Reopens: RT-09.

### 18. Make CI and release gates authoritative

1. Include every tagged PostgreSQL package in `make test-integration`.
2. Include evaluation execution in the race suite.
3. Require integration, race, Python, Helm, Compose, documentation, and shell
   checks before release publication.
4. Validate required planning Markdown even before it is tracked.
5. Strengthen release-workflow tests to assert the complete gate.

Reopens: RT-13.

### 19. Final verification and re-review

1. Run every focused regression added in this phase.
2. Run the full unit, race, PostgreSQL integration, Python, Helm, Compose,
   documentation, shell-syntax, and diff checks.
3. Review each reopened issue against its code, tests, and documentation.
4. Close a checkbox only with current evidence; retain any unmet item as open.

## Phase 7 completion record

- RT-01 closes with parent-derived child tenants plus database triggers/RLS.
- RT-03 closes with cancellation-safe binding, access touching, correct
  capacity accounting, and external-binding retention.
- RT-04 closes with Ed25519 request-bound assertions and replay rejection.
- RT-05 closes with reduced grants, one-agent role enforcement, and fail-closed
  deployment credentials.
- RT-07 closes with bounded claimable-run scans, periodic recovery, lease
  heartbeats, and checked saturation finalization.
- RT-08 closes with a second hard shutdown bound and observable timeout path.
- RT-09 closes with batch, pass-count, and time budgets across every retention
  surface, including external bindings.
- RT-13 closes with authoritative integration/race/release/documentation gates.

The final pass also corrected integration fixtures that still expected a
restricted agent to own bootstrap DDL, removed the GCP agent-database fallback
password, and fixed evaluation-capture sweep termination for a backlog in only
one child table. The issue register contains the per-issue evidence and final
validation results.

## Phase 8: Second-audit gap remediation

### 20. Complete tenant-scoped aggregates and monitor fencing

1. Add tenant scope to active-session and failure aggregate store contracts.
2. Thread the immutable agent tenant through autoscaling, API, and console
   callers.
3. Fence health callbacks with monitor generations across replace, restart,
   and stop operations.
4. Add former-tenant aggregate/load and delayed-callback regressions.

Closes the second-audit code gaps in RT-01 and RT-02.

### 21. Close authenticated client and database-role bypasses

1. Sign metrics fan-out requests with the control-plane private key.
2. Compare effective PostgreSQL role identities instead of DSN text.
3. Reject elevated, inherited, control-plane, and protected-table-owner agent
   roles.
4. Compile shared-role/rebind concessions only into integration binaries.
5. Extend the live Helm acceptance script to require successful signed agent
   metrics.

Closes the second-audit code gaps in RT-04 and RT-05. RT-04 final acceptance
still requires running the live script against a designated Kubernetes release.

### 22. Fence evaluation writes and complete retention operations

1. Require owner, running state, and live lease for every result write and
   finalization.
2. Use the PostgreSQL clock for lease acquisition, renewal, eligibility, and
   completion.
3. Split evaluation, live-memory retention, and dead-history GC metrics.
4. Add and safely propagate live-memory dry-run configuration.
5. Add stale-worker, per-kind metric, safe-child-environment, and bounded
   dry-run tests.

Closes the second-audit gaps in RT-07 and RT-09.

### 23. Validate and reconcile the register

1. Run `make check` and the full Postgres/end-to-end integration target.
2. Run race-sensitive Go packages, Python shim tests, Helm render tests,
   Compose validation, and shell syntax checks.
3. Re-read code, tests, public documentation, and every reopened requirement.
4. Close RT-01, RT-02, RT-05, RT-07, and RT-09.
5. Keep RT-04's regression/final-review checkboxes open until the live
   Kubernetes acceptance test runs successfully.
