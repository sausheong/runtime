# Runtime roadmap

This is the forward-looking backlog. Completed milestone evidence remains
available in Git history.

The latest formal tag is `v0.2.0`. Capabilities present only on `master` are
unreleased until a versioned release and migration notes are published.

## Release engineering

- Select and add a licence before describing the project as open source.
- Define the supported upgrade window and publish schema/config migration notes.
- Cut a versioned release from a clean, fully verified commit.
- Publish immutable container digests and an SBOM/provenance bundle.
- Run the clean-install acceptance suite on native Linux as well as Docker
  Desktop.

## Control-plane scale

- Add a database-backed claim/lease for evaluation-run recovery so more than
  one control-plane replica cannot execute the same incomplete run.
- Move quota buckets to a shared store or clearly shard them at the ingress
  layer.
- Define leader election for local agent actuation, retention jobs, and other
  singleton responsibilities.
- Add control-plane high-availability and failover acceptance tests.

## Agent isolation

- Provision a least-privilege agent database role in every deployment profile
  and document the exact grants.
- Add an optional per-agent credential or proxy so a compromised local process
  cannot read another agent's data.
- Offer a remote container/VM execution profile for less-trusted agent code.
- Add mTLS between the control plane and remote agents.

## Durability and compatibility

- Define cross-version DBOS workflow compatibility and drain/upgrade policy.
- Extend crash-resume semantics to supported contract shims.
- Add a first-class idempotency contract for side-effecting tools.
- Add retention/archival controls for session events, not only evaluation data.

## Gateway and memory

- Add MCP resources and prompts passthrough.
- Add distributed quota state and richer usage budgets.
- Add per-user and per-agent memory scopes, TTL/compaction, and session
  synthesis.
- Add explicit operator controls for private dynamic upstreams where a trusted
  tenant integration needs them.

## Sandboxes

- Validate the Chromium sandbox and private CDP network on native Linux in CI or
  a dedicated acceptance environment.
- Add a rootless or socket-proxy deployment option for container creation.
- Add per-user sandbox scope and an operator inspection/cleanup surface.
- Evaluate microVM isolation as a separate execution backend without weakening
  workflow ownership.

## API and operations

- Split the large `cmd/runtimed` composition root into lifecycle, identity,
  gateway, evaluation, and fleet modules with focused tests.
- Add API versioning and generated OpenAPI documentation.
- Add structured audit export and log shipping.
- Add database backup/restore and disaster-recovery acceptance tests.
- Add console coverage for metrics, sandbox state, and retention controls.

## SDKs

- Enforce native lifecycle limits in the Python shim.
- Add a TypeScript contract shim.
- Add framework adapters only when they pass the common conformance and crash
  behavior suites.
