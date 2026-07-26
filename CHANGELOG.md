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

### Changed (breaking)

- The turnkey Compose profile now requires `RUNTIME_BROWSER_PROXY_TOKEN`. The
  egress proxy must bind an address reachable from the private browser network,
  and a non-loopback bind has always required a token — previously the profile
  silently bound loopback instead, leaving the proxy both unreachable from the
  browser container and unauthenticated.

  New installs: `make compose-init` generates it. **Upgrades: append
  `RUNTIME_BROWSER_PROXY_TOKEN=$(openssl rand -hex 32)` to
  `deploy/compose/.env`.** Do not run `make compose-init FORCE=--force` on an
  existing deployment — it regenerates `RUNTIME_SECRETS_KEYS` under the same
  `primary` key id, which leaves already-sealed secrets undecryptable.

## v0.2.0

The repository tag is the authoritative record for this release:
[v0.2.0](https://github.com/sausheong/runtime/releases/tag/v0.2.0).
