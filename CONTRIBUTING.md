# Contributing

Runtime is currently maintained as a pre-release project. Before proposing a
large change, open a design discussion that states the problem, security and
tenant-boundary effects, migration needs, and compatibility impact.

## Development

Use Go 1.25.12 or later. Keep changes focused, preserve public agent-contract
compatibility, and add regression tests for every corrected failure mode.

```bash
make fmt-check
make vet
make test
make security-scan
make test-integration
make helm-lint
bash deploy/charts/runtime/test.sh
```

Integration tests require PostgreSQL with pgvector. Changes to deployment
templates must also pass the relevant Compose or Helm render checks. Changes to
public behaviour must update the owning topic guide and
[documentation-map.md](documentation-map.md).

### Shell scripts

CI and the release workflow lint the deployment scripts with ShellCheck. Run the
same gate locally; if `shellcheck` is not installed, use the container fallback,
which needs nothing but Docker:

```bash
docker run --rm -v "$PWD:/mnt" -w /mnt koalaman/shellcheck:stable --severity=warning \
  deploy/compose/*.sh deploy/compose/initdb/*.sh deploy/gcp/*.sh \
  deploy/gcp/control-plane/*.sh deploy/charts/runtime/*.sh
```

Severity policy: `error` and `warning` findings block, while `info` and `style`
findings are advisory for these scripts. Keep `--severity=warning` explicit —
ShellCheck defaults to `style`, which exits non-zero on advisory findings and
would leave the gate permanently red. Several of those advisory findings are
deliberate: the `DSN` and `AGENTS` variables in `deploy/charts/runtime/test.sh`
each hold multiple `--set` flags that must word-split into separate `helm`
arguments, so quoting them as SC2086 advises would break the render matrix.

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
