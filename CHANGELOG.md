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

## v0.2.0

The repository tag is the authoritative record for this release:
[v0.2.0](https://github.com/sausheong/runtime/releases/tag/v0.2.0).
