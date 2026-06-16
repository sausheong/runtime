# runtime Helm chart

Deploy the `runtime` on-prem durable LLM agent platform on Kubernetes. This chart
runs the control plane (`runtimed` + `agentd`) from a single all-binaries image,
secure-by-default, with optional bundled or BYO Postgres.

- **Chart version:** 0.1.0 &nbsp;·&nbsp; **App version:** 0.1.0 &nbsp;·&nbsp; **Helm:** v4
- **Image:** one image bundling `runtimed`, `agentd`, `sandboxd`, `browserd`, and
  `runtimectl`, running as non-root uid `10001`.

---

## Quick start (kind)

This runs a smoke test with **scripted agents** (`model: test/scripted`), so no LLM
API key is required.

> The chart image bundles `runtime` and `harness`. Because the `runtime` module
> uses a `replace ../harness` directive, the Docker build context is the **parent**
> of `runtime/` and `harness/`. The `make docker-image` target already handles this
> (it builds from `..`).

```bash
# 1. Build the image (build context is the parent of runtime/ + harness/)
make docker-image

# 2. Create a cluster and load the image into it
kind create cluster --name runtime
kind load docker-image runtime:$(git describe --tags --always --dirty) --name runtime

# 3. Vendor + UNPACK the Postgres subchart. Helm v4 needs it unpacked, not a .tgz.
helm dependency update deploy/charts/runtime    # or: make helm-deps

# 4. Install with the bundled Postgres subchart
helm install runtime deploy/charts/runtime \
  --set postgresql.enabled=true \
  --set image.pullPolicy=Never \
  --set image.tag=$(git describe --tags --always --dirty) \
  -f my-values.yaml

# 5. Reach the control plane
kubectl port-forward svc/runtime 8080:8080
curl http://127.0.0.1:8080/healthz
```

> **`make helm-deps` is required first.** Helm v4 needs the Bitnami `postgresql`
> subchart vendored *and unpacked* under `charts/`; a loose `.tgz` will not load.

Sample `my-values.yaml` with two scripted agents. Each agent requires `id`,
`name`, `model`, and `listen_addr` (the loader rejects any agent missing these,
and refuses to start with an empty registry). `model: test/scripted` selects the
built-in scripted test agent — there is no `script:` field:

```yaml
# my-values.yaml
config:
  agents:
    - id: support
      name: Support Agent
      model: test/scripted
      listen_addr: 127.0.0.1:8101
    - id: research
      name: Research Agent
      model: test/scripted
      listen_addr: 127.0.0.1:8102
```

---

## Deploy modes

### 1. BYO Postgres (recommended for production)

Point the chart at an existing Postgres. For semantic memory the
`pgvector` extension must be pre-created by a DB superuser.

Inline DSN:

```bash
helm install runtime deploy/charts/runtime \
  --set secrets.pgDsn='postgres://user:pass@db.internal:5432/runtime?sslmode=require'
```

Or reference an existing Secret (see [Secrets](#secrets)):

```bash
helm install runtime deploy/charts/runtime \
  --set secrets.existingSecret=runtime-secrets
```

### 2. Bundled Postgres subchart

```bash
helm install runtime deploy/charts/runtime --set postgresql.enabled=true
```

The chart synthesizes the in-cluster DSN automatically. **Caveat:** the Bitnami
Postgres image does **not** include the `pgvector` extension, so semantic memory
will not work against the bundled DB. Everything else works. For
memory, use a BYO `pgvector`-enabled Postgres (mode 1).

### 3. Dev-insecure (local / demo only)

Legacy bearer tokens are configured under `config.tokens` (rendered into
`runtime.yaml`) — they are **not** a secret env var, and there is no identity.

```yaml
# values-dev.yaml
postgresql:
  enabled: true
config:
  tokens:
    - "dev-token-123"
```

### Fail-closed

If **none** of `postgresql.enabled`, `secrets.pgDsn`, or `secrets.existingSecret`
is set, `helm template` / `helm install` fails at render with a clear message.
This is intentional — the control plane has no usable database otherwise.

---

## Secure-by-default posture

The chart ships locked down. Defaults:

| Setting | Value | Why |
|---|---|---|
| `replicaCount` | `1` | The supervisor is a single-writer DBOS process tree. Two replicas against one Postgres is unsupported. |
| Deployment strategy | `Recreate` | Same reason — never run two supervisors against the same DB, even briefly during a rollout. |
| `podSecurityContext.runAsNonRoot` | `true` | No root in the pod. |
| `podSecurityContext.runAsUser` / `fsGroup` | `10001` / `10001` | Matches the non-root image user. |
| `securityContext.readOnlyRootFilesystem` | `true` | The root FS is immutable. |
| `securityContext.allowPrivilegeEscalation` | `false` | No setuid escalation. |
| `securityContext.capabilities.drop` | `[ALL]` | All Linux capabilities dropped. |
| Writable storage | `/tmp` only (emptyDir) | `agentd` does no disk writes; DBOS persists to Postgres. |

A `checksum/config` pod annotation rolls the pod automatically on `helm upgrade`
whenever the rendered config changes.

---

## Secrets

Two paths:

**Inline (chart-managed Secret).** Set `secrets.*` values and the chart creates a
Secret for you:

```bash
helm install runtime deploy/charts/runtime \
  --set secrets.pgDsn='postgres://...' \
  --set secrets.secretsKeys='...' \
  --set secrets.secretsPrimary='...' \
  --set secrets.adminBootstrap='...'
```

**Existing Secret.** Set `secrets.existingSecret` and the chart emits **no** Secret
— it env-refs yours instead. The Secret must have a `RUNTIME_PG_DSN` key; the
others (`RUNTIME_SECRETS_KEYS`, `RUNTIME_SECRETS_PRIMARY`, `RUNTIME_ADMIN_BOOTSTRAP`)
are optional env refs (`optional: true`):

```bash
kubectl create secret generic runtime-secrets \
  --from-literal=RUNTIME_PG_DSN='postgres://user:pass@db.internal:5432/runtime?sslmode=require' \
  --from-literal=RUNTIME_SECRETS_KEYS='...' \
  --from-literal=RUNTIME_SECRETS_PRIMARY='...' \
  --from-literal=RUNTIME_ADMIN_BOOTSTRAP='admin@example.com:...'

helm install runtime deploy/charts/runtime --set secrets.existingSecret=runtime-secrets
```

---

## Configuration

The `config:` value is rendered **verbatim** into `runtime.yaml`, mounted at
`/etc/runtime/runtime.yaml`. It holds the agent registry and optional gateway
config. On `helm upgrade`, a change to `config` updates the `checksum/config` pod
annotation and the pod auto-rolls.

Legacy bearer tokens (dev-insecure mode only) go under `config.tokens` — they are
part of `runtime.yaml`, **not** a secret env var.

```yaml
config:
  agents:
    - name: my-agent
      model: anthropic/claude-...
  # gateway:
  #   servers: []
  # tokens: ["..."]   # dev only
```

### Identity (OIDC)

```bash
helm install runtime deploy/charts/runtime \
  --set identity.enabled=true \
  --set identity.oidcIssuer='https://issuer.example.com' \
  --set identity.oidcClientID='runtime' \
  --set identity.oidcRedirectURL='https://runtime.example.com/auth/callback'
```

These map to `RUNTIME_OIDC_ISSUER`, `RUNTIME_OIDC_CLIENT_ID`, and
`RUNTIME_OIDC_REDIRECT_URL`.

---

## Observability

```bash
helm install runtime deploy/charts/runtime --set obs.enabled=true
```

`obs.enabled=true` emits:

- a **`ServiceMonitor`** — requires the **Prometheus Operator** CRDs to be
  installed in the cluster.
- a **`grafana_dashboard`-labeled ConfigMap** carrying the runtime overview
  dashboard — requires a **Grafana sidecar** configured to watch that label.

---

## Ingress / NetworkPolicy

```bash
# Ingress
helm install runtime deploy/charts/runtime \
  --set ingress.enabled=true \
  --set 'ingress.hosts[0].host=runtime.example.com' \
  --set 'ingress.hosts[0].paths[0].path=/' \
  --set 'ingress.hosts[0].paths[0].pathType=Prefix'

# NetworkPolicy: allows ingress to 8080 and all egress
helm install runtime deploy/charts/runtime --set networkPolicy.enabled=true
```

`networkPolicy.enabled=true` allows ingress to port `8080` and **all** egress —
`runtimed` needs to reach Postgres, the LLM proxy, and gateway upstreams.

---

## Docker-dependent features (sandbox / browser)

`sandboxd` and `browserd` ship in the image but are **disabled by default**: they
need a Docker daemon, which a plain pod does not have. To enable them you must set
`features.dockerHost` (→ `DOCKER_HOST`) and add a **privileged** Docker-in-Docker
sidecar via the `extraContainers` / `extraVolumes` escape hatches.

> This is a **single-node convenience, not a production posture** — the DinD
> sidecar runs privileged.

```yaml
# values-sandbox.yaml — opt-in; requires a PRIVILEGED sidecar. Single-node only.
features:
  dockerHost: tcp://localhost:2375
extraContainers:
  - name: dind
    image: docker:27-dind
    securityContext:
      privileged: true            # required by Docker-in-Docker
    env:
      - name: DOCKER_TLS_CERTDIR
        value: ""
    args: ["--host=tcp://0.0.0.0:2375"]
    volumeMounts:
      - name: dind-storage
        mountPath: /var/lib/docker
extraVolumes:
  - name: dind-storage
    emptyDir: {}
```

In addition:

- The sandbox/browser images (`runtime-sandbox:latest`, `runtime-browser:latest`,
  built via `make sandbox-image` / `make browser-image`) must be loadable by that
  DinD daemon.
- The agent `runtime.yaml` (`config:`) must declare the upstreams with
  `command: /app/sandboxd` / `command: /app/browserd`.

---

## Values reference

| Key | Default | Description |
|---|---|---|
| `image.repository` | `runtime` | Image repository. |
| `image.tag` | `""` | Image tag; defaults to `.Chart.AppVersion` (`0.1.0`). |
| `image.pullPolicy` | `IfNotPresent` | Pull policy (use `Never` for kind). |
| `imagePullSecrets` | `[]` | Image pull secrets. |
| `replicaCount` | `1` | Fixed at 1 — single-writer supervisor. |
| `config` | `{agents: []}` | Rendered verbatim into `runtime.yaml`. |
| `secrets.existingSecret` | `""` | Name of an existing Secret (chart emits none). |
| `secrets.pgDsn` | `""` | `RUNTIME_PG_DSN` (required unless `postgresql.enabled`). |
| `secrets.secretsKeys` | `""` | `RUNTIME_SECRETS_KEYS`. |
| `secrets.secretsPrimary` | `""` | `RUNTIME_SECRETS_PRIMARY`. |
| `secrets.adminBootstrap` | `""` | `RUNTIME_ADMIN_BOOTSTRAP`. |
| `identity.enabled` | `false` | Enable OIDC env vars. |
| `identity.oidcIssuer` | `""` | `RUNTIME_OIDC_ISSUER`. |
| `identity.oidcClientID` | `""` | `RUNTIME_OIDC_CLIENT_ID`. |
| `identity.oidcRedirectURL` | `""` | `RUNTIME_OIDC_REDIRECT_URL`. |
| `features.dockerHost` | `""` | `DOCKER_HOST`; enables sandbox/browser. |
| `service.type` | `ClusterIP` | Service type. |
| `service.port` | `8080` | Service port. |
| `ingress.enabled` | `false` | Emit an Ingress. |
| `ingress.className` | `""` | Ingress class. |
| `ingress.annotations` | `{}` | Ingress annotations. |
| `ingress.hosts` | `[]` | Hosts/paths. |
| `ingress.tls` | `[]` | TLS config. |
| `networkPolicy.enabled` | `false` | Allow ingress to 8080, all egress. |
| `obs.enabled` | `false` | ServiceMonitor + Grafana dashboard ConfigMap. |
| `postgresql.enabled` | `false` | Bundle the Bitnami Postgres subchart. |
| `postgresql.auth.username` | `runtime` | Bundled DB user. |
| `postgresql.auth.password` | `runtime` | Bundled DB password. |
| `postgresql.auth.database` | `runtime` | Bundled DB name. |
| `extraContainers` | `[]` | Sidecar containers (e.g. DinD). |
| `extraVolumes` | `[]` | Extra pod volumes. |
| `extraVolumeMounts` | `[]` | Extra container volume mounts. |
| `resources` | `{}` | Container resource requests/limits. |
| `podSecurityContext.runAsNonRoot` | `true` | Run as non-root. |
| `podSecurityContext.runAsUser` | `10001` | Non-root uid. |
| `podSecurityContext.fsGroup` | `10001` | Pod fsGroup. |
| `securityContext.allowPrivilegeEscalation` | `false` | No privilege escalation. |
| `securityContext.readOnlyRootFilesystem` | `true` | Immutable root FS. |
| `securityContext.capabilities.drop` | `[ALL]` | Drop all capabilities. |
| `nodeSelector` / `tolerations` / `affinity` | `{}` / `[]` / `{}` | Scheduling. |
| `podAnnotations` | `{}` | Extra pod annotations. |

---

## Building & publishing

```bash
make helm-lint        # lint the chart
make helm-template    # render templates locally
make helm-deps        # vendor + unpack the Postgres subchart
make helm-package     # → dist/runtime-0.1.0.tgz
```

There is **no CI for chart publishing** — it is manual:

```bash
docker push <registry>/runtime:0.1.0
helm push dist/runtime-0.1.0.tgz oci://<registry>
```

---

## Limitations / non-goals

- **Single replica.** The control plane is not HA — `replicaCount` is fixed at 1.
  Your durability floor is **Postgres HA**, not control-plane replication.
- **Per-agent-pod scheduling** (one pod per agent) is available via
  `scheduling.mode: perAgentPods` — see the section below.
- **No operator** yet — lifecycle is plain Helm.
- **Bundled Postgres lacks `pgvector`** — use a BYO `pgvector` Postgres for
  semantic memory.

---

## Per-agent-pod scheduling (`scheduling.mode: perAgentPods`)

By default (`monolith`) runtimed exec-spawns every agent as a child in one pod.
Set `scheduling.mode: perAgentPods` to instead run **each agent as its own
StatefulSet** (one headless Service per agent for stable per-ordinal DNS) that
runtimed **attaches to** as a remote replica pool.

```yaml
scheduling:
  mode: perAgentPods
secrets:
  pgDsn: "postgres://..."
  agentAuthToken: "a-shared-bearer"   # recommended; runtimed → agent auth
config:
  agents:
    - { id: support, name: Support, model: claude-opus-4-8, tenant: acme, replicas: 2 }
    - { id: research, name: Research, model: claude-opus-4-8 }   # replicas defaults to 1
```

In this mode each agent entry takes `id`, `name`, `model`, and optionally
`tenant`, `replicas` (pod count, default 1), `memory`, `gateway`. Do **not** set
`listen_addr` or `url` — the chart generates the per-ordinal url and wires
runtimed's `runtime.yaml` to attach.

**Scaling.** `kubectl scale statefulset <release>-agent-<id> --replicas=N` *down*
is handled live: runtimed skips ordinals whose health probe fails. Scaling *up*
beyond the configured `replicas` requires `helm upgrade` (re-render the config so
runtimed learns the higher ordinal count).

**Known limitation — brokered secrets.** Without the registration handshake
(below), per-agent-pod agents receive provider credentials from the chart Secret
(env), not from runtimed's secrets broker, which decrypts and injects only at
spawn time (and runtimed does not spawn these pods). Supply provider keys via
`secrets.existingSecret`/the chart Secret, or turn on the registration handshake
so a pod pulls decrypted secrets over an authenticated channel.

### Registration handshake

In `perAgentPods` mode, set `secrets.registrationToken` (or supply an
`existingSecret` carrying a `RUNTIME_REGISTRATION_TOKEN` key) to turn on the
**registration handshake**. Each agent pod then **pulls its full config from the
control plane at boot** — DSN, identity, opt-in feature env, and the tenant's
**brokered (decrypted) per-tenant secrets** — instead of reading provider
credentials from the static chart Secret. This closes the gap where brokered
secrets could not reach scheduled pods (they are spawn-time-only for local
children, and runtimed does not spawn these pods).

How it works: when the token is present the chart adds `RUNTIME_REGISTRATION_URL`
(the control-plane Service + `/register`) and a `RUNTIME_REGISTRATION_TOKEN`
secretKeyRef to each agent StatefulSet. At startup `agentd` POSTs `/register`
with its token and `$HOSTNAME` ordinal, applies the returned env, then runs its
normal startup path. It **fails hard** (CrashLoops) if the handshake fails — a
pod that cannot fetch its config must not start with a partial environment.

Mint a token (admin-scoped, behind the identity admin guard):

```bash
runtimectl register mint --agent <id>   # prints the one-time plaintext token
runtimectl register list                # token_id, agent_id, revoked status (no secret)
runtimectl register revoke <token-id>   # fail-closed at the next pod restart
```

Then wire it in:

```yaml
scheduling:
  mode: perAgentPods
secrets:
  registrationToken: "<minted-plaintext>"   # shared by all agent pods (see note)
```

Notes:

- **Per-agent identity-backed token.** Each token binds to one `agent_id` (whose
  tenant comes from config) and is bcrypt-hashed in the `registration_tokens`
  table. A leaked token can fetch ONLY its own agent's tenant secrets, and only
  for ordinals the StatefulSet will actually create (fail-closed bounds check).
  Tokens are revocable; `agentd` re-fetches on every restart, so a revoke takes
  effect at the next restart.
- **`RUNTIME_LISTEN_ADDR` and the ordinal stay pod/infra-provided.** The handshake
  delivers DSN + identity + tenant + feature env + brokered secrets — NOT the bind
  address or replica ordinal. A remote agent has no control-plane `Addr`, so the
  delta returns those empty and `agentd` skips empty values; the StatefulSet sets
  `RUNTIME_LISTEN_ADDR` statically and the `$HOSTNAME` wrapper provides the ordinal
  fallback, exactly as before.
- **Shared token simplification.** `secrets.registrationToken` is a single value
  shared by all agent pods in this chart. For **distinct per-agent tokens**, supply
  an `existingSecret` with per-agent keys (out of chart scope) — handshake mode is
  detected whenever an `existingSecret` is set.
- **Known limitation — gateway in perAgentPods.** Per-agent-pod (remote) agents
  still cannot opt into the gateway. `config.Validate` rejects `gateway:` on a
  remote agent (gateway is a spawn-time-only field), so `RUNTIME_GATEWAY_URL`/`_KEY`
  are never set for these agents and the handshake delta carries only the empty
  gateway shadow — agentd skips empty values, so nothing is delivered. The
  handshake retires the brokered-secrets limitation (DSN + identity + brokered
  secrets DO arrive), but gateway-enabled agents remain monolith-only; per-agent-pod
  gateway stays backlogged.
- **mTLS is still deferred.** The registration token is a bearer over
  operator-terminated TLS (same trust model as the runtimed→agent bearer).
