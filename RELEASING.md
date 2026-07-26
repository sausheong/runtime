# Release and upgrade guide

## Before upgrading

1. Read [CHANGELOG.md](CHANGELOG.md) and the release notes.
2. Back up PostgreSQL and verify that the backup can be restored.
3. Record the running image digest, chart version, configuration, and secrets
   references.
4. Let active durable workflows finish where practical. Changing lifecycle
   limits, tool definitions, or workflow code can be incompatible with an
   in-flight DBOS replay.
5. Test the upgrade against a restored copy of production data.

Runtime records ordered component versions in
`runtime_schema_migrations`. A control-plane binary applies supported pending
migrations transactionally before serving. Restricted agent binaries only
check the core schema version and fail if it is outside their supported range.
A failed migration rolls back without advancing the ledger. Startup also
reconciles missing baseline tables and foreign keys after a partial restore,
but fails closed without deleting orphan rows whose parentage cannot be
proved.

## Release procedure

1. Ensure the working tree is clean and CI is green.
2. Update `CHANGELOG.md`, `deploy/charts/runtime/Chart.yaml`, and public
   documentation.
3. Tag the exact reviewed commit as `vMAJOR.MINOR.PATCH`.
4. The release workflow builds and pushes the immutable GHCR image and Helm
   chart, generates SPDX SBOMs, signs image and chart digests with keyless
   Sigstore, attaches attestations, and creates the GitHub release.
5. Verify the published digests and signatures from a separate environment.

## Rollback

Application rollback is supported only while the older binary's declared
schema range includes the migrated database version. Database migrations are
forward-only: restore the pre-upgrade backup if a release requires an older
schema. Do not delete or edit migration-ledger rows to force a downgrade.

DBOS workflow compatibility is separate from SQL compatibility. If an older
binary cannot replay workflows started by the newer release, drain those
workflows before rollback or restore the matching application/database pair.

The first tenant-ownership migration quarantines sessions that predate the
core migration ledger as `__legacy_unowned__`. They are intentionally invisible
to every tenant because their original owner cannot be proven from the old
schema. After backup and an ownership review, an operator may explicitly
attribute selected rows:

```sql
UPDATE sessions
   SET tenant_id = 'verified-tenant'
 WHERE tenant_id = '__legacy_unowned__'
   AND id IN ('reviewed-session-id');
```

Do not bulk-assign quarantined rows from their agent ID alone; agent IDs can be
reused across tenants.

## Retention and archives

Retention is deletion, not archival. Export required audit/session/evaluation
data before its cutoff. Database backups must be encrypted, access-controlled,
tested, and retained according to the organisation's own recovery and
regulatory requirements.
