# Changelog

All notable changes are recorded here. Runtime follows semantic versioning once
a release is tagged, while the current default branch remains pre-release.

## Unreleased

### Added

- Tenant ownership persisted on sessions and durable affinity for external
  replica pools.
- Independent remote-replica monitoring.
- Evaluation leases, bounded recovery, and bounded online scoring.
- Session and opt-in live-memory retention.
- Versioned schema migration ledger and restricted-agent schema preflight.
- HTTP concurrency/body limits and configurable transcript capture.
- Per-agent Kubernetes credentials and NetworkPolicies.

### Security

- Signed forwarded agent identity.
- Separate control-plane and agent database credentials in supplied deployment
  profiles.
- Expanded best-effort transcript credential redaction.
- Browser egress is now enforced below the application layer: a configured
  `RUNTIME_BROWSER_NETWORK` is inspected at startup and rejected unless it is
  declared `internal`, so a browser process that ignores `--proxy-server` has
  no route off the segment.

### Fixed

- Sandbox and browser configuration reaches the subsystems again. `runtimed`
  spawns `sandboxd` and `browserd` as stdio gateway servers, and the stdio
  environment scrub blanked every inherited variable outside a small allowlist
  — including all `RUNTIME_SANDBOX_*` / `RUNTIME_BROWSER_*` settings and
  `DOCKER_HOST`. The effect was silent and security-relevant: with
  `RUNTIME_BROWSER_NETWORK` cleared, `browserd` skipped the internal-network
  branch entirely (no validation, no private network, CDP published on a host
  interface), session-scoped isolation degraded to tenant scope, and a
  configured gVisor runtime was not applied. The scrub still blocks
  control-plane credentials; it now forwards subsystem configuration.
  Unreleased — introduced after v0.2.0.

### Changed

- `RUNTIME_IDENTITY_SIGNING_PUBLIC_KEY` is now optional. An Ed25519 public key
  is a function of its private key, so requiring an operator to supply both
  could only ever produce a mismatch; `runtimed` derives it at startup. Set the
  variable, or `secrets.identitySigningPublicKey`, only to pin the value.
- The integration suite's database is now resolved in one place from
  `RUNTIME_TEST_PG_DSN`, then `RUNTIME_PG_DSN`, then the local default. Eight
  packages previously hardcoded the DSN as a const, so the documented override
  silently did nothing and the destructive tests DROPped tables in whatever was
  at that address. README, CONTRIBUTING, and `make` help now say the suite is
  destructive, as does `make pg-down`, whose dev database has no volume.

- The browser egress proxy now binds an address reachable from the private
  browser network, where it previously bound loopback and was therefore
  unreachable from the browser container (and, being loopback, unauthenticated).
  A non-loopback bind requires a token, and `browserd` mints an ephemeral one
  per start when none is configured, so **no deployment change is required**.
  `RUNTIME_BROWSER_PROXY_TOKEN` remains available to pin a fixed value.

  The credential is process-internal: `browserd` serves the proxy that demands
  it and answers the demand itself over CDP, so nothing outside that process
  ever needs the value. A fresh token per start is also stronger than a static
  one on disk.

## v0.2.0

The repository tag is the authoritative record for this release:
[v0.2.0](https://github.com/sausheong/runtime/releases/tag/v0.2.0).
