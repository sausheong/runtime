# Runtime roadmap

This is the forward-looking backlog. Completed milestone evidence remains
available in Git history.

The latest formal tag is `v0.2.0`. Capabilities present only on `master` are
unreleased until a versioned release and migration notes are published.

## Release engineering

- Select and add a licence before describing the project as open source.
- Cut a versioned release from a clean, fully verified commit.
- Run the clean-install acceptance suite on native Linux as well as Docker
  Desktop.

## Control-plane scale

- Move quota buckets to a shared store or clearly shard them at the ingress
  layer.
- Define leader election for local agent actuation, retention jobs, and other
  singleton responsibilities.
- Add control-plane high-availability and failover acceptance tests.

## Agent isolation

- Enforce database-level per-agent or per-tenant row isolation in addition to
  the restricted fleet agent role.
- Offer a remote container/VM execution profile for less-trusted agent code.
- Add mTLS between the control plane and remote agents.

## Durability and compatibility

- Add automated cross-version DBOS workflow replay acceptance tests.
- Extend crash-resume semantics to supported contract shims.
- Add a first-class idempotency contract for side-effecting tools.
- Add first-class archive/export before retention deletion.

## Gateway and memory

- Add MCP resources and prompts passthrough.
- Add distributed quota state and richer usage budgets.
- Add per-agent memory pools, compaction, and richer session synthesis.
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
