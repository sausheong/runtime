# Operator guide — Runtime turnkey self-host

For the person who runs the platform. Assumes you brought it up via the
[Quickstart](quickstart.md).

## Log in to the console

`make compose-init` generated `deploy/compose/.env` with a one-time superuser
bootstrap key. From the repo root (`runtime/`):

```bash
grep RUNTIME_ADMIN_BOOTSTRAP deploy/compose/.env
```

Open http://localhost:8080/ui and log in with that key. Treat it as root — it is
the break-glass superuser credential. Use it to create the first tenant and
tenant-admin (see the [Tenant guide](tenant-guide.md)).

## Ports

| Service | Host port |
|---|---|
| Control plane / console | 8080 |
| Prometheus | 9090 |
| Grafana | 3000 |
| Jaeger UI | 16686 |
| OTLP HTTP (collector) | 4318 |

Postgres is **not** published to the host (reachable only inside the compose
network).

## Persistence & reset

- Data lives in the `pgdata` named volume.
- `docker compose down` **preserves** data; `docker compose up` resumes it.
- `docker compose down -v` **wipes** the volume (and re-runs the pgvector
  extension init on the next `up`) — a clean reset. (`make compose-reset` does
  the same from the repo root.)

## Security posture (single-node trust)

- runtimed mounts the host Docker socket to launch sandbox/browser containers.
  That is **root-equivalent on the host** — run this stack only on a trusted
  single node, not on untrusted/shared infrastructure.
- Secrets (bootstrap key, AES key, tenant credentials) are never written to logs.
- The bundled stack runs with identity ON; the console and APIs require auth.

## Observability

- **Grafana** http://localhost:3000 (anonymous viewer) — the runtime dashboard.
- **Prometheus** http://localhost:9090 — `runtimed` is a scrape target at
  `/metrics`.
- **Jaeger** http://localhost:16686 — distributed traces for control-plane
  requests.
