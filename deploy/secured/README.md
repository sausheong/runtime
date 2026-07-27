# Secured single-host deployment

This deployment adds HTTPS and access control to a single-host Runtime
installation without depending on a specific cloud provider. It works on GCP,
another public cloud, an on-premises server, or an air-gapped machine.

```
client ──HTTPS──► Caddy ──http──► runtimed:8080  (console /ui + API /)
                  (TLS)            identity ON, Postgres-backed
```

- **TLS** is terminated by a **Caddy** container — automatic Let's Encrypt with a
  domain, or a locally-trusted self-signed cert without one. No cloud load
  balancer, no cloud-managed certificate service.
- **Access control** is runtime's **own identity system** (Postgres). No cloud
  IAM. Manage users from the console (`/ui/onboarding`, admin-gated).
- `runtimed` is **not** published to the host; all traffic enters through Caddy.

## 1. Initialise

```bash
cd deploy/secured

# On-prem / IP-only / air-gapped — self-signed cert on :443:
./init.sh

# Public host with a domain — automatic Let's Encrypt:
./init.sh runtime.example.com
```

`init.sh` generates `.env` (identity secrets + TLS mode) and the `tls.caddy`
snippet, and prints a **bootstrap superuser key** — copy it now; you use it once
to seed the first admin (step 4). Both files are gitignored.

To enable an **OIDC IdP** (Keycloak, Dex, Google, Okta, …) for console login,
edit `.env` and set `RUNTIME_OIDC_ISSUER`, `RUNTIME_OIDC_CLIENT_ID`,
`RUNTIME_OIDC_CLIENT_SECRET`, and `RUNTIME_OIDC_REDIRECT_URL`
(`https://<host>/ui/callback`). Leave them blank to use service-key (paste-token)
login. Either way, identity is ON.

## 2. Provide the image

`docker-compose.yml` defaults to `runtime:latest`. Build it (from the projects
root, parent of `runtime/` and `harness/`) or pull from your registry and set
`RUNTIME_IMAGE`:

```bash
# build locally (match your host arch; use --platform linux/amd64 for x86 VMs)
docker build -f runtime/deploy/Dockerfile -t runtime:latest .
# or point at a registry image:
export RUNTIME_IMAGE=<registry>/runtime:latest
```

## 3. Bring it up

```bash
docker compose up -d
docker compose logs runtimed | grep "identity enabled"   # confirms identity=ON
```

- **Domain mode:** ports 80 + 443 must be reachable from the internet and the
  domain's A/AAAA record must point at this host (Caddy needs 80 for the ACME
  HTTP-01 challenge and to redirect HTTP→HTTPS).
- **Self-signed mode:** browsers warn on the untrusted cert (expected); accept it
  or distribute Caddy's internal CA (`/data` volume) to clients.

## 4. Seed the first admin

Identity is ON but starts with no users. Use the bootstrap key once to create the
first admin, then manage everyone else from the console.

```bash
HOST=https://<your-host>        # or https://localhost for self-signed (add -k)
BK=<bootstrap-key-from-init>

# create your tenant (superuser-only) and the first admin user
curl -k -X POST "$HOST/admin/tenants" -H "Authorization: Bearer $BK" \
  -d '{"id":"acme","name":"Acme"}'
curl -k -X POST "$HOST/admin/users" -H "Authorization: Bearer $BK" \
  -d '{"subject":"you@acme.com","role":"admin","tenant":"acme"}'
```

`subject` is the identifier your login presents: the OIDC `sub`/email when OIDC
is configured, or — for service-key login — mint a key for the user via the
console (`/ui/onboarding`, "Agent keys") or `runtimectl admin`. Then log in at
`$HOST/ui` and manage users under the **Users** section.

## 5. Verify

```bash
curl -k -o /dev/null -w "%{http_code}\n" $HOST/healthz                 # 200
curl -k -o /dev/null -w "%{http_code}\n" $HOST/agents                  # 401 (gated)
curl -k -o /dev/null -w "%{http_code}\n" -H "Authorization: Bearer $BK" $HOST/agents  # 200
```

## Notes

- **Agents:** `runtime.remote.yaml` ships one bare scripted agent so the stack
  boots (runtimed requires ≥1 agent). Replace it with your own — local kinds,
  foreign (`command`) agents, or REMOTE agents on other hosts (`url` +
  `auth_token`). See `deploy/gcp/control-plane/runtime.remote.yaml` for examples.
- **Portability:** the only inputs are a domain (optional), an OIDC IdP
  (optional), and Postgres (bundled). Nothing here calls a cloud API. To swap
  Caddy for Nginx + operator-supplied certs (strict corporate PKI / air-gap),
  replace the `caddy` service and mount your cert+key — runtimed is unchanged.
- **Observability:** this overlay is intentionally minimal (Caddy + runtimed +
  Postgres). Add Prometheus/Grafana/OTel/Jaeger services from
  `deploy/gcp/control-plane/docker-compose.yml` if you want them.
- `init.sh` refuses to overwrite an existing `.env`. `--force` overrides that,
  but it is **destructive, not rotation**: it regenerates
  `RUNTIME_SECRETS_KEYS` under the same `primary` key id, so every secret
  already sealed with the old key becomes undecryptable, and it mints a new
  bootstrap key. Use it only where no stored data matters. To rotate safely,
  add a new key id and re-encrypt — see "Key rotation" in
  `identity-and-memory.md`.
