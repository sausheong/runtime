# Tenant guide — onboard and run agents

For a tenant-admin. Assumes the operator has the stack up
([Quickstart](quickstart.md)) and you can log in to http://localhost:8080/ui.

## Onboard a tenant (console UI)

As the superuser (operator), then as the tenant-admin:

1. **Create a tenant.** In the console, create a tenant (id + name).
2. **Mint an agent key.** On the onboarding page, mint a service key for the
   tenant — copy the one-time plaintext shown (it is not displayable again).
   Pick the **role** by what the key will do (see [Roles](#roles-and-keys)
   below): `operator` to invoke agents, `viewer` for read-only, `admin` to
   manage the tenant's users and keys.
3. **Set a credential.** Add a tenant secret (name + value) — this is the
   per-tenant upstream credential, sealed in the secrets broker. The name must
   be an env-style identifier (e.g. `ORDERS_API_KEY`).
4. **Register an upstream.** Register an OpenAPI upstream pointing at the bundled
   demo orders API, with the credential attached:
   - transport: OpenAPI
   - spec URL: `http://host.docker.internal:9000/openapi.yaml`
     (start the demo first, from the repo root: `go run ./examples/rest-demo` —
     it listens on :9000)
   - base URL: `http://host.docker.internal:9000` — **required.** The demo's
     own spec advertises `http://localhost:9000`, which from inside the
     containerized gateway is the *container's* loopback, not your host. Setting
     the base URL overrides it so tool calls reach the host-run demo. (Any
     upstream running on the host, not in the compose network, needs this.)
   - credential: select the secret you set; header `Authorization`
5. Watch the upstream reach **up** on the onboarding page.

## Roles and keys

Every request to the control plane carries a bearer — a human's OIDC cookie or a
machine **service key** — scoped to one tenant with one of three fixed roles. The
action you need decides the minimum role:

| Role | Can do | Use it for |
|---|---|---|
| `viewer` | list/get/stream sessions and agents (read only) | dashboards, reading verdicts |
| `operator` | viewer **+** invoke (`POST /sessions`) | **triggering agents** |
| `admin` | operator **+** manage its tenant's users and keys | onboarding, minting keys |

To **trigger an agent** you need an **`operator`** key; a `viewer` key gets `403`
on `POST /agents/{id}/sessions`. Reserve `admin` for tenant administration —
don't hand an `admin` key to a script whose only job is to invoke.

### Mint a key from the CLI

The onboarding page (above) is the click path. To mint from a terminal, use
`runtimectl admin key create`. Minting hits the admin-only `/admin/keys`
endpoint, so you must already hold an **admin** bearer — the first one comes from
the console or the one-time bootstrap key (`RUNTIME_ADMIN_BOOTSTRAP`); after that
the CLI mints everything else.

`runtimectl` reads `RUNTIME_CTL_URL` (base URL) and `RUNTIME_TOKEN` (the admin
bearer it sends automatically):

```bash
RUNTIME_CTL_URL=http://localhost:8080 \
RUNTIME_TOKEN="$RUNTIME_ADMIN_BOOTSTRAP" \
  runtimectl admin key create --role operator --label ci-runner --tenant <your-tenant>
#   → svk-<id>.<secret>   (shown once — store it now; only a hash is kept)

runtimectl admin key ls                 # list keys (id + role + label)
runtimectl admin key revoke svk-<id>    # revoke instantly
```

A normal admin key is already pinned to its own tenant, so `--tenant` is ignored
for it (harmless to include); the **bootstrap superuser** is tenantless and
*must* name a tenant. The raw-HTTP equivalent is a `POST /admin/keys` with body
`{"role":"operator","label":"ci-runner","tenant":"<your-tenant>"}`.

## The six pillars, exercised

The platform ships `deploy/compose/v1-proof.sh`, which exercises every pillar
deterministically (no LLM key needed).

> **The proof runs its own throwaway stack — it is destructive.** `v1-proof.sh`
> regenerates `.env` (a fresh bootstrap key) and tears the stack down with
> `docker compose down -v` (wiping the `pgdata` volume) on exit. Run it on a
> **disposable checkout, or before** you onboard anything through the console —
> it will delete a tenant/upstream you created by hand and invalidate the
> bootstrap key you logged in with. The onboarding walkthrough above is the
> by-hand path; the proof is the automated, self-contained gate.

Run it from the repo root:

```bash
deploy/compose/v1-proof.sh
```

It asserts:

- **Runtime** — the control plane comes up healthy and serves requests.
- **Identity** — unauthenticated calls are refused; a second tenant cannot see
  this tenant's upstream (cross-tenant isolation).
- **Gateway** — the federated catalog includes the REST upstream (`orders`) and
  the MCP sandbox; a REST-adapter tool call round-trips through the gateway.
- **Memory** — a fact is written and **semantically recalled** (the bundled
  embedder + pgvector).
- **Sandboxes** — code runs in an isolated container (`print(6*7)` → 42).
- **Observability** — the run appears in Prometheus metrics and as a Jaeger
  trace.

## Optional: drive a real LLM agent

The bundled agents use a deterministic test model (no API key). To have an agent
autonomously discover and call these tools, point an agent at a real model by
setting its `model:` (e.g. `claude-opus-4-8`) and the provider API key in the
environment, then drive it from the console or `runtimectl invoke`. This is
optional and not required for the turnkey proof.

Triggering an agent is two requests — create a session, then stream its events —
both with an **`operator`** (or `admin`) bearer (see [Roles and
keys](#roles-and-keys)):

```bash
export BASE=http://localhost:8080
export KEY=<operator-key>

# 1) create a session → returns {"session_id": "ses-..."}
SID=$(curl -s -H "Authorization: Bearer $KEY" "$BASE/agents/<id>/sessions" \
  -d '{"message":"..."}' | jq -r .session_id)

# 2) stream events until {"type":"done"}
curl -sN -H "Authorization: Bearer $KEY" "$BASE/agents/<id>/sessions/$SID/stream?since=0"
```

`runtimectl invoke --agent <id> "..."` does both in one command (it reads the
same `RUNTIME_CTL_URL` / `RUNTIME_TOKEN` env vars).
