# Tenant guide — onboard and run agents

For a tenant-admin. Assumes the operator has the stack up
([Quickstart](quickstart.md)) and you can log in to http://localhost:8080/ui.

## Onboard a tenant (console UI)

As the superuser (operator), then as the tenant-admin:

1. **Create a tenant.** In the console, create a tenant (id + name).
2. **Mint an agent key.** On the onboarding page, mint a service key for the
   tenant — copy the one-time plaintext shown (it is not displayable again).
3. **Set a credential.** Add a tenant secret (name + value) — this is the
   per-tenant upstream credential, sealed in the secrets broker. The name must
   be an env-style identifier (e.g. `ORDERS_API_KEY`).
4. **Register an upstream.** Register an OpenAPI upstream pointing at the bundled
   demo orders API, with the credential attached:
   - transport: OpenAPI
   - spec URL: `http://host.docker.internal:9000/openapi.yaml`
     (start the demo first, from the repo root: `go run ./examples/rest-demo` —
     it listens on :9000)
   - credential: select the secret you set; header `Authorization`
5. Watch the upstream reach **up** on the onboarding page.

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
