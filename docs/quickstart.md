# Quickstart — Runtime turnkey self-host

Bring up the whole platform (all six pillars) on one host with Docker.

## Prerequisites

- Docker (Desktop on macOS/Windows, or Engine on Linux) with Compose v2.
- **Clone both repos side by side.** Runtime builds against a sibling
  `harness/` checkout (a `replace` directive during the v0.x line):

  ```bash
  git clone https://github.com/sausheong/harness.git
  git clone https://github.com/sausheong/runtime.git
  # result: a parent dir containing both harness/ and runtime/
  ```

## Bring it up

The `Makefile` lives at the repo root, so `make compose-init` runs from
`runtime/`. The compose file lives in `deploy/compose/`, so the `docker compose`
commands run from there.

```bash
cd runtime
make compose-init                            # generates .env with a bootstrap key + secrets key
cd deploy/compose
docker compose --profile build-only build    # builds runtimed, embedder, AND sandbox/browser images
docker compose up                            # starts all six pillars
```

> The sandbox and browser images sit behind a `build-only` compose profile, so
> you must pass `--profile build-only` to `build` them (a plain
> `docker compose build` skips them and the Sandboxes pillar will fail to launch
> containers). The equivalent one-liner from the repo root is `make compose-build`.

> On native **Linux**, set `DOCKER_GID` in `.env` to your host's docker group id
> (`getent group docker | cut -d: -f3`) so the non-root runtime can launch
> sandbox containers. On Docker Desktop the default works.

## What you should see

Seven services healthy (postgres, embedder, runtimed, prometheus, grafana,
otel-collector, jaeger). Then:

| Surface | URL |
|---|---|
| Console (web UI) | http://localhost:8080/ui |
| Grafana (metrics) | http://localhost:3000 |
| Jaeger (traces) | http://localhost:16686 |

Next: **[Operator guide](operator-guide.md)** to log in, then
**[Tenant guide](tenant-guide.md)** to onboard a tenant and run agents.
