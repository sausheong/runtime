# Runtime Remediation Implementation Plan

This plan implements
[the remediation issue register](runtime-remediation-issues.md). Work is
ordered so that persistence and identity invariants land before routing,
deployment, and operational refinements that depend on them.

Status: Phase 13 local remediation is complete. Phases 1 through 12 remain the
historical record. The issue register is authoritative: RT-04 and RT-14 remain
open for the two-release Kubernetes acceptance run, and G2 remains open because
local `shellcheck` evidence is unavailable. These external gates prevent
release-candidate status but do not reopen the completed local work.

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
4. Fail generation-less legacy bindings closed, including old single-replica
   rows, wherever immutable ownership cannot be proved.
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

## Phase 9: Third-audit remediation

### 20. Restore remote-only production startup

1. Classify configured agents by whether runtimed spawns them with the
   restricted DSN.
2. Provision a role only for local agents and skip provisioning for an
   attach-only registry.
3. Preserve the one-role-per-local-agent and one-tenant fail-closed checks.
4. Add remote-only, mixed, and production-profile boot regressions.

Reopens: RT-05.

### 21. Correct Kubernetes trust-boundary acceptance

1. Change the live test to accept source and target one-agent releases.
2. Assert distinct agent identities before attempting cross-agent access.
3. Test denied source-agent access, allowed target-control-plane access, and
   signed target metrics.
4. Update Helm documentation and shell/render validation.

Reopens: RT-04.

### 22. Remove unfenced evaluation transitions

1. Remove unconditional status/finalization methods from `EvalStore`,
   PostgreSQL, and memory implementations.
2. Convert tests and UI fixtures to claim and finalize through a live lease.
3. Add a source-contract regression for the persistence interface.

Reopens: RT-07.

### 23. Make retention accounting failure-safe and deployable

1. Extract bounded single-sweep helpers with injected deletion operations.
2. Record each successful destructive batch immediately.
3. Add partial-success and dry-run tests.
4. Expose typed retention and concurrency values in Helm.
5. Add Compose pass-through for control-plane and agent settings.
6. Update configuration and deployment documentation.

Reopens: RT-09.

### 24. Protect management metrics

1. Separate port `8080` and `9091` NetworkPolicy ingress.
2. Allow `9091` from the control-plane pod and configured Prometheus peers
   only.
3. Add secure defaults, Helm render assertions, and live denial coverage.
4. Document same- and cross-namespace monitoring.

Closes: RT-14.

### 25. Replace replay-cache full scans

1. Add a rotating two-bucket nonce cache with constant-time lookup.
2. Enforce a fixed maximum entry count and fail closed at capacity.
3. Add rotation, replay, capacity, concurrency, and benchmark coverage.
4. Document the security and overload behaviour.

Closes: RT-15.

### 26. Third-audit verification

1. Run focused Go, Helm, shell, Compose, and documentation tests.
2. Run `make check`, integration, race, Python, Helm, and deployment gates.
3. Re-read RT-04, RT-05, RT-07, RT-09, RT-14, and RT-15 against the final
   implementation and tests.
4. Close only evidence-backed checkboxes; leave live-cluster execution open if
   no designated Kubernetes release is available.

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

## Phase 10: Fourth-audit gap remediation

### 27. Bind registration credentials to live immutable identities

1. Add tenant and agent-generation bindings to registration tokens and persist
   a generation for every managed agent.
2. Make registration verification compare the token binding with the live
   registry before returning environment data.
3. Replace the startup tenant snapshot with a concurrency-safe live identity
   lookup and authorize token list/revoke operations by immutable token tenant.
4. Revoke exact-generation tokens on managed-agent deletion and fail legacy
   unbound tokens closed through versioned migrations.
5. Add cross-tenant reuse, deletion/recreation, live-admin, race, exact-binding,
   and legacy-migration tests.

Closes: RT-16.

### 28. Enforce the evaluation persistence state machine

1. Require new runs to be pending, unleased, unfinished, and unique in both
   stores.
2. Reject empty owners, invalid lease deadlines, and non-terminal claimed
   finalization while preserving live-owner/live-lease fencing.
3. Add durable PostgreSQL status and state-coherence constraints with safe
   normalization of legacy malformed rows.
4. Add matching memory and PostgreSQL contract and migration tests.

Closes: RT-07.

### 29. Make NetworkPolicy acceptance evidence truthful

1. Discover only Running and Ready source-agent, target-agent, and target
   control-plane pods and require every probed Service.
2. Prove source-side debug, DNS, and allowed connectivity before denial probes.
3. Record curl exit markers and accept only timeout exit 28 as policy denial;
   reject setup, DNS, refusal, image-pull, and protocol failures.
4. Prove target control-plane agent access and signed fleet metrics before
   checking cross-release agent and metrics denial.
5. Add a fake-`kubectl` regression matrix to the normal chart test suite.

Closes the fourth-audit code gaps in RT-04 and RT-14. Their regression and
final-review checkboxes remain open until the corrected harness passes against
two installed one-agent releases.

### 30. Verify and reconcile the fourth-audit changes

1. Run focused registration, evaluation, registry-race, migration, and
   NetworkPolicy-harness regressions.
2. Run `make check`, the complete tagged PostgreSQL/end-to-end integration
   suite, race-sensitive packages, chart tests, Helm lint, documentation
   checks, and diff hygiene.
3. Re-read the implementations, tests, public documentation, and issue
   requirements.
4. Close RT-07 and RT-16; keep only the environment-specific RT-04/RT-14 live
   acceptance boundary open.

## Phase 11: Frozen comprehensive invariant remediation

### 31. Freeze the cross-cutting acceptance contract

1. Use sections A through G of the issue register as the immutable fifth-audit
   scope.
2. Map every reopened issue to all applicable agent origins, storage backends,
   tenant arrangements, lifecycle transitions, concurrency boundaries,
   deployment profiles, and failure paths.
3. Name the regression test that proves each matrix row. Do not infer complete
   coverage from a nearby test.
4. Keep unsupported topologies fail closed and document them as unsupported;
   do not substitute documentation for enforcement.

### 32. Make identity selection and affinity generation-atomic

1. Introduce an immutable selected-agent snapshot containing tenant,
   generation, mode, and replica set.
2. Authorize and sign forwarding against that exact snapshot immediately
   before proxying.
3. Persist generation in external bindings and reject missing or stale
   generations.
4. Cover new-session and session-route replacement races, same- and
   cross-tenant reuse, endpoint replacement, restart, health, and scaling for
   native and external stores.

Closes the comprehensive A matrix and reopened RT-01/RT-03 requirements.

### 33. Enforce real per-agent database trust domains

1. Bind core RLS authorization to tenant and immutable agent identity.
2. Give each supported agent an isolated DBOS database/schema trust domain, or
   reject shared-database multi-agent configurations before startup.
3. Remove residual schema-wide grants and keep all elevated/test concessions
   unreachable from production binaries.
4. Test two same-tenant roles and two cross-tenant roles against core children,
   evaluation, identity, memory, secrets, and DBOS state through fresh install,
   recovery, migration, and restart.
5. Make Compose, GCP, Helm, and operator guidance render only enforceable
   topologies.

Closes the comprehensive B matrix and reopened RT-05 requirements.

### 34. Decouple deterministic classification from optional scoring

1. Persist deterministic terminal classification on every native terminal path
   before optional score admission.
2. Let a persisted completed quality score refine the category to
   `quality_fail`; never let queue or scorer failure imply a clean result.
3. Make queue-full, shutdown, timeout, provider, judge, and store outcomes
   observable and counted exactly once.
4. Exercise all terminal states and failure paths in memory and PostgreSQL,
   while retaining the RT-07 lease/state-machine matrix.

Closes the comprehensive C matrix and reopened RT-08 requirements.

### 35. Make retention transactional and completion-aged

1. Select and lock one bounded session candidate set, then delete its dependent
   records and parents in the same transaction.
2. Serialize touch/status/child-write races against retention and test both
   orderings in PostgreSQL.
3. Age terminal evaluation runs from `finished_at`, define legacy fallback, and
   retain newly completed long-running/recovered work.
4. Revalidate bounded work, partial-success accounting, dry-run, tenant scope,
   disabled defaults, and deployment configuration across every retention
   surface.

Closes the comprehensive D matrix and reopened RT-09 requirements.

### 36. Give every registration-capable agent a lifecycle generation

1. Replace deterministic file-agent generations with explicit persisted
   instance generations.
2. Render generation material for Helm registration-managed agents and require
   explicit rotation on replacement while preserving ordinary restart.
3. Apply exact tenant/generation checks to mint, verify, register, list, revoke,
   deletion, and concurrent administration.
4. Test dynamic, persisted, file, Helm, legacy, restart, replacement,
   cross-tenant, same-tenant, memory, and PostgreSQL cases.

Closes the comprehensive E matrix and reopened RT-16 requirements.

### 37. Make release evidence structural

1. Run `make helm-lint` in CI and before release publication.
2. Parse workflow jobs and steps to prove the entire blocking validation matrix
   occurs before the first publish operation in the publishing job.
3. Add mutation fixtures for omitted, reordered, unrelated, commented, and
   non-blocking gates.
4. Re-run every deployment render and hermetic NetworkPolicy topology.

Closes F1 through F5 and reopened RT-13 requirements. F6 remains open until a
designated two-release Kubernetes environment is available.

### 38. Complete final matrix validation and independent re-audit

1. Run every named matrix regression from the final source state.
2. Run formatting, vet, unit, race, tagged PostgreSQL, end-to-end, Python,
   Helm lint/render, Compose, documentation, shell, and diff gates.
3. Trace every request and durable transition in matrix sections A through F,
   including non-diff code and deployment modes.
4. Update every issue and matrix checkbox with current evidence only.
5. Keep F6, RT-04, RT-14, and overall final acceptance open if the live
   Kubernetes evidence is unavailable.

## Phase 12: Comprehensive follow-through discovered by final validation

### 39. Repair partial-restore referential integrity

1. Validate migration ledger versions and checksums before changing schema.
2. Reconcile the immutable baseline before dependent migrations and again at
   the current version.
3. Add versioned foreign-key repair for core, evaluation, identity,
   managed-agent, and gateway state.
4. Restore missing constraints only for consistent data and fail closed,
   without deleting or inventing parents, when orphan rows exist.
5. Cover partial restore, current-ledger missing tables, corrupt ledgers,
   missing constraints, and retained orphans in PostgreSQL integration tests.

Completed. Extends RT-10 and the B/D durability matrix.

### 40. Make evaluation refinement and retention accounting replay-safe

1. Persist initial deterministic classification before optional scoring.
2. Refine `none` to `quality_fail` with one atomic expected-state transition
   after a completed score is durable.
3. Separate initial-classification and refinement metrics, and emit each
   exactly once across replay and retry.
4. Count successful destructive batches before any later retention failure.

Completed. Extends RT-08/RT-09 and the C/D matrix.

### 41. Remove cross-component integration contamination

1. Drop dependent session/evaluation tables before session parents.
2. Drop managed-agent and gateway tenant children before identity tenant
   tables.
3. Give independently started remote agents isolated DBOS schemas and matching
   lifecycle generations.
4. Re-run the previously order-dependent autoscaling and remote attachment
   cases, then the entire integration package from one clean sequence.

Completed. Extends RT-05/RT-13 and G1/G2.

### 42. Replace the ineffective vulnerability gate

1. Upgrade all development, CI, release, and container build surfaces to Go
   1.25.12 and upgrade reachable vulnerable dependencies.
2. Replace the deprecated Docker root SDK with supported Moby API/client
   modules.
3. Use package-level scanning to cover every imported package without the
   crashing source-symbol analyser.
4. Build and binary-symbol scan all six shipped Go commands.
5. Require the same blocking security target in CI and before release
   publication, and include it in structural workflow mutation tests.

Completed. Extends RT-13 and F1 through F3.

### 43. Final validation disposition

1. Unit, vet, race, complete end-to-end/PostgreSQL integration, Python, Helm,
   hermetic NetworkPolicy, Compose, vulnerability, image-build, documentation,
   and diff gates pass from the final source state.
2. Local `shellcheck` could not be executed in the available environment; CI
   and release retain it as a blocking gate.
3. F6, RT-04, and RT-14 remain open only for the two-release live Kubernetes
   acceptance run.

## Phase 13: Sixth-audit invariant remediation

Changes in this phase are implemented and reviewed as small dependency-ordered
units. Each issue closes only after its adversarial tests, broader affected
suites, documentation, and a fresh source review pass.

### 44. Secure the browser egress boundary

Status: completed 2026-07-26.

1. Add dial-seam tests that distinguish policy-time DNS from connect-time DNS.
2. Replace the partial address classifier with the shared public-IP policy.
3. Resolve on the dial path and connect only to a validated IP for HTTP and
   CONNECT, rejecting mixed answer sets.
4. Make the default listener private and require proxy authentication for an
   explicitly non-loopback bind.
5. Bound server headers, requests, CONNECT tunnels, upstream response bytes,
   idle time, and absolute tunnel duration.
6. Reconcile browser configuration and security documentation.

Closes: RT-17 and the proxy portion of RT-11.

### 45. Make restricted schema preflight authoritative

Status: completed 2026-07-26.

1. Pass the immutable expected migration set to the restricted startup path.
2. Validate every version, name, and checksum without applying DDL.
3. Check agent-required structural sentinels, RLS state, policies, triggers,
   and foreign keys using read-only catalog queries.
4. Add corrupt-ledger and missing-object PostgreSQL integration tests.

Closes: RT-10 and B5.

### 46. Make online-score replay one coherent durable event

Status: completed 2026-07-26.

1. Define the first persisted criterion result as immutable.
2. Implement identical insert-only conflict behaviour in memory and
   PostgreSQL.
3. Keep classification refinement and result metrics conditional on that
   authoritative first insert.
4. Test opposite-verdict replay, concurrent writers, restart, and metric
   accounting.

Closes: RT-08, C3, and C5.

### 47. Bound auxiliary responses and own ingestion shutdown

Status: completed 2026-07-26.

1. Add shared bounded-response decoding helpers and apply named limits to the
   judge, registration, console, and evaluation-control clients.
2. Add boundary and oversized-response regressions.
3. Give memory ingestion a lifecycle context, cancellation, wait group, closed
   gate, and bounded drain.
4. Wire service shutdown to drain ingestion before closing its store.
5. Test cooperative, blocked, non-cooperative, concurrent, and store-ordering
   shutdown paths.

Closes: the remaining RT-11 requirements and RT-18.

### 48. Complete release artifact integrity

Status: completed 2026-07-26.

1. Pass release version and revision into image builds and inspect OCI labels.
2. Add `image.digest` chart support and digest render tests.
3. Define required and optional image inventory.
4. Build and smoke-test every repository Dockerfile in CI.
5. Scan, produce SBOMs for, and sign every published image.
6. Constrain sidecar bases and Python dependencies and document their update
   policy.

Closes: RT-13 and F7 through F10.

### 49. Comprehensive closure and deployment acceptance

Status: local steps 1 through 4 completed 2026-07-26; step 5 remains open
because no designated live Kubernetes environment is available. The local
shell-analysis sub-gate in step 2 also remains open.

1. Run focused tests for RT-08, RT-10, RT-11, RT-13, RT-17, and RT-18.
2. Run formatting, vet, unit, race, PostgreSQL/end-to-end integration, Python,
   Helm lint/render, Compose, documentation, vulnerability, container,
   shellcheck, and diff gates from the final source state.
3. Review every security and durability invariant, including request paths and
   durable transitions outside the latest diff.
4. Update the register with named current evidence and leave any failed or
   unavailable gate open.
5. Run the shared RT-04/RT-14 harness against two installed one-agent
   Kubernetes releases only after the final images and chart are produced.

Closes: G1 through G4 locally. F6, RT-04, and RT-14 close only with designated
live-cluster evidence.

### Phase 13 completion record

1. Browser egress now uses validated-address dial pinning, complete
   non-public-address rejection, authenticated non-loopback proxying through
   Chromium's CDP authentication challenge, and bounded request/tunnel
   resources. Focused unit, race, and real Docker/Chromium tests pass.
2. Restricted startup now validates the exact migration ledger and all
   required schema security sentinels without DDL. Corrupt-ledger and
   missing-object PostgreSQL tests pass, as do fresh end-to-end startup and the
   full tagged integration suite.
3. Online evaluation uses immutable first-write authority across memory and
   PostgreSQL. Opposite-verdict replay, concurrency, restart reconstruction,
   classification, and metric accounting tests pass.
4. Auxiliary clients use shared bounded response reads. Knowledge-graph
   ingestion owns admission, cancellation, drain, and concurrent close; the
   full concurrency-heavy package set passes under the race detector.
5. Release validation builds, smoke-tests, and scans all seven repository
   images. Runtime OCI labels are inspected, Helm accepts immutable digests,
   bases and Python dependencies are constrained, and CI/release race gates
   include the new browser and memory concurrency paths.
6. `make check`, the complete integration target, Python tests, Helm
   lint/render, all Compose renders, documentation checks, shell syntax,
   container smoke/scans, race detection, and `git diff --check` pass from the
   final source state.
7. Local `shellcheck` and the two-release RT-04/RT-14 live Kubernetes harness
   are deliberately not recorded as passing. They remain the only open
   execution gates.
