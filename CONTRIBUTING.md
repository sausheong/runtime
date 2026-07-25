# Contributing

Runtime is currently maintained as a pre-release project. Before proposing a
large change, open a design discussion that states the problem, security and
tenant-boundary effects, migration needs, and compatibility impact.

## Development

Use Go 1.25.1 or later. Keep changes focused, preserve public agent-contract
compatibility, and add regression tests for every corrected failure mode.

```bash
make fmt-check
make vet
make test
make test-integration
bash deploy/charts/runtime/test.sh
```

Integration tests require PostgreSQL with pgvector. Changes to deployment
templates must also pass the relevant Compose or Helm render checks. Changes to
public behaviour must update the owning topic guide and
[documentation-map.md](documentation-map.md).

## Pull requests

- Explain the user-visible result and operational risks.
- Include tests that fail without the change.
- Add ordered migrations for schema changes; never edit an applied migration.
- Do not commit credentials, generated local environment files, or production
  data.
- Keep commits reviewable and use the existing formatting and lint targets.

By contributing, you confirm that you have the right to submit the work. The
repository does not yet contain a licence; contribution does not itself create
one.
