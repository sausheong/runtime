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

### Changed

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
