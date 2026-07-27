# Quickstart: run Runtime on one host

Use Docker Compose to run the complete Runtime stack on one host. This is the
fastest way to evaluate the platform locally.

## Prerequisites

- Docker (Desktop on macOS/Windows, or Engine on Linux) with Compose v2.
- Clone Runtime. Its Go dependencies, including `harness`, are pinned and
  downloaded automatically:

  ```bash
  git clone https://github.com/sausheong/runtime.git
  ```

## Bring it up

The `Makefile` lives at the repo root, so `make compose-init` runs from
`runtime/`. The compose file lives in `deploy/compose/`, so the `docker compose`
commands run from there.

```bash
cd runtime
make compose-init                            # generates bootstrap, secrets, and Grafana credentials
cd deploy/compose
docker compose --profile build-only build    # builds runtimed, embedder, AND sandbox/browser images
docker compose up                            # starts the complete stack
```

> The sandbox and browser images sit behind a `build-only` compose profile, so
> you must pass `--profile build-only` to `build` them (a plain
> `docker compose build` skips them and the Sandboxes pillar will fail to launch
> containers). The equivalent one-liner from the repo root is `make compose-build`.

> On native **Linux**, set `DOCKER_GID` in `.env` to your host's docker group id
> (`getent group docker | cut -d: -f3`) so the non-root runtime can launch
> sandbox containers. On Docker Desktop the default works.

## What you should see

Eight services running (postgres, embedder, runtimed, prometheus, alertmanager,
grafana, otel-collector, and jaeger). Then:

| Surface | URL |
|---|---|
| Console (web UI) | http://localhost:8080/ui |
| Grafana (metrics) | http://localhost:3000 |
| Prometheus (metrics and rules) | http://localhost:9090 |
| Alertmanager (alert routing) | http://localhost:9093 |
| Jaeger (traces) | http://localhost:16686 |

Prometheus, Alertmanager, Grafana, and Jaeger are bound to host loopback. Log
into Grafana as `admin` using `GRAFANA_ADMIN_PASSWORD` from
`deploy/compose/.env`; anonymous access is disabled. The OTLP collector and
Runtime's `:9091` management metrics listener are Compose-internal and are not
published to the host.

Next, use the [operator guide](operator-guide.md) to log in and set safe
defaults. Then follow the [tenant guide](tenant-guide.md) to onboard a tenant
and run an agent. For the architectural tour, read the [Runtime
overview](runtime.md).
