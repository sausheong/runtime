# C2 Packaging (M1) — Container Image + Helm Chart — Design

**Date:** 2026-06-12
**Sub-project:** C2 (Containers / Kubernetes), first milestone
**Status:** Approved design; ready for implementation plan.
**Builds on:** the complete Runtime spine + all five peer sub-projects' shipped
milestones (Gateway M1–M3, Memory M1–M3, Identity M1–M3, Sandboxes M1–M2,
Observability M1). Packages *what exists*.

---

## 1. Goal

Package the runtime platform as a versioned **container image** and a **Helm
chart** so it deploys to Kubernetes (and any container host) with one command,
**secure-by-default**, and **faithful to the current single-node supervisor
model**.

One sentence: *make the platform `helm install`-able onto a cluster, honestly.*

---

## 2. Architecture & scope

### 2.1 The fact that shapes everything

`runtimed` supervises agents by **`exec`-ing child processes** in its own
process tree (`SpawnFunc` forks `./agentd`, or an arbitrary `command:` argv,
with the same `RUNTIME_*` env). The platform today is therefore a **single-node
supervisor**, not a decomposed set of services. `sandboxd`/`browserd` are gateway
upstreams that, when enabled, talk to a **Docker daemon** to spawn their own
containers.

C2-M1 packages this *as it is*: one supervisor process tree in **one pod**.

### 2.2 In scope (C2-M1)

- A **single all-binaries image** carrying `runtimed`, `agentd`, `sandboxd`,
  `browserd`, `runtimectl` — the monolith-pod packaging where runtimed `exec`s
  agentd children inside the pod (spawn model unchanged).
- A **Helm chart** at `deploy/charts/runtime/` templating: Deployment (1 replica,
  stateful supervisor), Service, optional Ingress, ConfigMap (`runtime.yaml`),
  Secret (PG DSN, secrets keyring, identity/OIDC, tokens), ServiceAccount,
  optional NetworkPolicy, optional Postgres subchart, and an obs toggle
  (ServiceMonitor + Grafana dashboard ConfigMap).
- **Make targets** for local image build + `helm package` (no CI).
- Chart-level + root README docs; ROADMAP §C2 + memory update at close-out.

### 2.3 Explicit non-goals (deferred, named)

- **Per-agent pods / decomposed scheduling** → needs **C3** (attach-instead-of-
  spawn) + spine **A1** (subprocess pools). The ROADMAP already frames
  K8s-scheduled agents as "remote agents whose lifecycle is owned by the
  orchestrator" — i.e. C3, not C2.
- **Kubernetes operator / CRDs** → a later C2 milestone.
- **Sandbox/browser running in-cluster** → they need a Docker daemon, which a
  plain pod lacks. Both binaries ship in the image but the features are **off by
  default**, surfaced via a `DOCKER_HOST` knob + a documented (not enabled) DinD
  sidecar recipe.
- **Autoscaling / HPA** → the supervisor is a single stateful replica today.
- **CI / multi-arch publish pipeline** → out of scope (chosen: build-locally
  only).

### 2.4 Faithfulness principle

The chart packages what exists — one supervisor pod — rather than pretending the
platform is already decomposed. Decomposition is the explicitly-named C3
follow-on.

---

## 3. The container image

**File:** `deploy/Dockerfile` (extend the existing one).

- **Build context:** unchanged — the **parent** of `runtime/` and `harness/`
  (forced by `replace github.com/sausheong/harness => ../harness`). Header
  comment documents it, as today.
- **Build stage** (`golang:1.25 AS build`): copy `harness/` + `runtime/`, build
  **all five binaries** to `/out/`: `runtimed`, `agentd`, `sandboxd`,
  `browserd`, `runtimectl`. (Today only runtimed+agentd are built.)
- **Runtime stage** (`debian:bookworm-slim`):
  - `ca-certificates` (already present) — outbound LLM/HTTPS.
  - Copy all five binaries to `/app/`.
  - Copy a default `runtime.yaml` to `/app/runtime.yaml` (chart's ConfigMap mount
    overrides it).
  - `ENV RUNTIME_AGENTD_BIN=/app/agentd` (already present). sandboxd/browserd are
    referenced by `command:` path in `runtime.yaml`, so the image guarantees they
    sit at `/app` and on `PATH`; **verify exact env/lookup names during
    planning.**
  - **Non-root:** add unprivileged user `runtime` (uid 10001, gid 10001),
    `USER 10001`. New vs today (today runs as root). Exec-spawning siblings works
    non-root; only Docker-dependent features need more, and those are off by
    default.
  - OCI labels (`org.opencontainers.image.source`, `.version`, `.revision`)
    stamped from `--build-arg`.
  - `EXPOSE 8080`, `CMD ["/app/runtimed"]`.
- **Deliberately absent:** Docker CLI/daemon (sandbox/browser reach a daemon via
  `DOCKER_HOST` when enabled), Postgres, Python shim runtimes (foreign-SDK agents
  are their own images — a C1/C3 concern).
- **Make target:** `make docker-image` →
  `docker build -f runtime/deploy/Dockerfile -t runtime:<version> -t runtime:latest ..`
  run from the projects root; `VERSION ?= $(git describe --tags --always --dirty)`,
  wired to OCI-label build-args.

---

## 4. Helm chart layout & values

**Location:** `deploy/charts/runtime/`

```
deploy/charts/runtime/
  Chart.yaml              # name: runtime, type: application, version+appVersion, Postgres dep
  values.yaml             # secure-by-default
  README.md               # chart docs
  .helmignore
  templates/
    _helpers.tpl          # name/label/selector + DSN-resolution helpers
    serviceaccount.yaml
    configmap.yaml        # runtime.yaml (values.config or minimal default) + checksum source
    secret.yaml           # only keys that are set; skipped entirely if existingSecret
    deployment.yaml       # runtimed, 1 replica, Recreate, probes, securityContext, /tmp emptyDir
    service.yaml
    ingress.yaml          # gated: ingress.enabled
    networkpolicy.yaml    # gated: networkPolicy.enabled
    servicemonitor.yaml   # gated: obs.enabled (Prometheus Operator CRD)
    dashboard-configmap.yaml  # gated: obs.enabled (grafana_dashboard label)
    NOTES.txt
  charts/                 # Postgres subchart vendored at package time (gitignored)
```

`Chart.yaml` declares an optional dependency on Bitnami `postgresql` with
`condition: postgresql.enabled`. `Chart.lock` + vendored `charts/*.tgz` are
gitignored (regenerated by `make helm-deps`).

### 4.1 `values.yaml` (secure-by-default)

```yaml
image:
  repository: runtime
  tag: ""              # defaults to .Chart.AppVersion
  pullPolicy: IfNotPresent
imagePullSecrets: []

replicaCount: 1        # supervisor is stateful; >1 unsupported (NOTES warns)

config:                # runtime.yaml — mounted as a ConfigMap
  agents: []
  # gateway: {...}

secrets:
  existingSecret: ""   # if set, inline keys below are ignored
  pgDsn: ""            # RUNTIME_PG_DSN (required unless postgresql.enabled)
  secretsKeys: ""      # RUNTIME_SECRETS_KEYS
  secretsPrimary: ""   # RUNTIME_SECRETS_PRIMARY
  adminBootstrap: ""   # RUNTIME_ADMIN_BOOTSTRAP
  tokens: ""           # legacy tokens (dev only)

identity:
  enabled: false       # OIDC issuer/clientID/etc → env when true

features:
  dockerHost: ""       # e.g. tcp://localhost:2375 — enables sandboxd/browserd upstreams

service:
  type: ClusterIP
  port: 8080

ingress:
  enabled: false
  className: ""
  annotations: {}
  hosts: []
  tls: []

networkPolicy:
  enabled: false

obs:
  enabled: false       # ServiceMonitor + Grafana dashboard ConfigMap

postgresql:            # Bitnami subchart
  enabled: false       # off ⇒ bring-your-own via secrets.pgDsn
  auth:
    username: runtime
    password: runtime
    database: runtime

# escape hatches for the DinD opt-in (empty by default)
extraContainers: []
extraVolumes: []
extraVolumeMounts: []

resources: {}
podSecurityContext:
  runAsNonRoot: true
  runAsUser: 10001
  fsGroup: 10001
securityContext:
  allowPrivilegeEscalation: false
  readOnlyRootFilesystem: true
  capabilities:
    drop: [ALL]
nodeSelector: {}
tolerations: []
affinity: {}
```

### 4.2 Design notes

1. **Postgres DSN resolution** (`_helpers.tpl`): if `postgresql.enabled`,
   synthesize the in-cluster DSN
   (`postgres://<user>:<pw>@<release>-postgresql:5432/<db>?sslmode=disable`) and
   `secrets.pgDsn` is not required. Else use `secrets.pgDsn` (or
   `existingSecret`). If neither Postgres nor a DSN is set, **template rendering
   fails with a clear `required`/`fail` error** (fail-closed, matching platform
   posture).
2. **`readOnlyRootFilesystem: true`** means runtimed needs writable scratch — an
   `emptyDir` at `/tmp` (and anywhere the spawn path writes). **Verify exact write
   paths during planning.**

---

## 5. Key templates

### 5.1 `deployment.yaml`

```yaml
spec:
  replicas: 1                      # hard intent; NOTES warns if values.replicaCount > 1
  strategy: { type: Recreate }     # single-writer supervisor — never two against one PG
  template:
    metadata:
      annotations:
        checksum/config: <sha of rendered runtime.yaml>   # auto-roll on config change
    spec:
      serviceAccountName: <release>-runtime
      securityContext: { runAsNonRoot: true, runAsUser: 10001, fsGroup: 10001 }
      containers:
        - name: runtimed
          image: "{{ repo }}:{{ tag | default .Chart.AppVersion }}"
          args: ["/app/runtimed"]
          env:
            - RUNTIME_CTL_ADDR=":8080"
            - RUNTIME_CONFIG="/etc/runtime/runtime.yaml"
            - RUNTIME_AGENTD_BIN="/app/agentd"
            - RUNTIME_PG_DSN        → secretKeyRef (synthesized or BYO)
            - RUNTIME_SECRETS_KEYS / _PRIMARY / RUNTIME_ADMIN_BOOTSTRAP → secretKeyRef (if set)
            - identity vars (if identity.enabled)
            - DOCKER_HOST           → features.dockerHost (if set)
          ports: [ { containerPort: 8080 } ]
          readinessProbe: { httpGet: { path: /healthz, port: 8080 }, initialDelaySeconds: 3,  periodSeconds: 5 }
          livenessProbe:  { httpGet: { path: /healthz, port: 8080 }, initialDelaySeconds: 10, periodSeconds: 10 }
          securityContext: { allowPrivilegeEscalation: false, readOnlyRootFilesystem: true, capabilities: { drop: [ALL] } }
          volumeMounts:
            - { name: config, mountPath: /etc/runtime, readOnly: true }
            - { name: scratch, mountPath: /tmp }
            # + extraVolumeMounts
        # + extraContainers (DinD opt-in)
      volumes:
        - { name: config,  configMap: { name: <release>-runtime } }
        - { name: scratch, emptyDir: {} }
        # + extraVolumes
```

Baked-in decisions: **`Recreate`** (a rolling update would briefly run two
supervisors against one Postgres — wrong for a single-writer DBOS workflow owner;
dovetails with spine-debt A4/A5); **`/healthz`** for both probes (auth-free,
fan-out-safe per obs M1); init delays tuned for the spawn loop that starts after
the HTTP server (gateway M1 ordering).

### 5.2 `secret.yaml`

Emits **only keys that are set**; empty-string values omitted entirely (so an
unset keyring never injects a malformed empty `RUNTIME_SECRETS_KEYS`). If
`secrets.existingSecret` is set, emits **nothing** and the Deployment's
`secretKeyRef`s target the existing Secret.

### 5.3 `configmap.yaml`

Renders `values.config` as `runtime.yaml`; empty `config` ⇒ a minimal valid
default (no agents, no gateway). The pod template carries a `checksum/config`
annotation so `helm upgrade` with changed config **auto-rolls the pod**.

---

## 6. Make targets, docs, DinD opt-in

### 6.1 Make targets (root `Makefile`)

```
make docker-image   # docker build -f deploy/Dockerfile -t runtime:$(VERSION) -t runtime:latest ..
                    #   VERSION ?= $(git describe --tags --always --dirty); --build-arg VERSION/REVISION
make helm-lint      # helm lint deploy/charts/runtime
make helm-template  # helm template deploy/charts/runtime (quick render check)
make helm-deps      # helm dependency update deploy/charts/runtime (vendors Postgres subchart)
make helm-package   # helm dependency update + helm package → dist/
```

No CI. README documents the manual `docker push` / `helm push <oci>` steps.

### 6.2 Docs

- New `deploy/charts/runtime/README.md`: quick start (`helm install`), three
  deploy modes (BYO-Postgres / bundled-subchart / dev-insecure), secure-by-default
  posture + how to supply secrets (inline vs `existingSecret`), the
  `replicaCount: 1` constraint and why, the obs toggle, the Docker-features
  section.
- Short "Kubernetes / Helm" section in root `README.md` linking to it.
- ROADMAP §C2 + memory updated at close-out (as every milestone).

### 6.3 DinD opt-in (documented, not enabled)

sandbox/browser need a Docker daemon. The chart ships **no privileged sidecar by
default** but exposes `extraContainers` / `extraVolumes` / `extraVolumeMounts`
escape hatches so an operator can add a `docker:dind` sidecar and set
`features.dockerHost: tcp://localhost:2375` **without forking the chart**. The
chart README is explicit: enabling it requires `privileged: true`, is a
single-node convenience, **not** a production posture. (Network-level egress
boundary for browser remains a B4 follow-on.)

---

## 7. Testing & live proof

### 7.1 Static / hermetic validation

- `make helm-lint` clean.
- `make helm-template` across value permutations, asserted (a small
  `deploy/charts/runtime/test.sh` or a Go test under `test/`):
  1. **defaults** → renders; `replicas: 1`; non-root SC; `readOnlyRootFilesystem`;
     `/healthz` probes.
  2. **`postgresql.enabled=true`** → DSN synthesized; no `secrets.pgDsn`
     required; subchart present.
  3. **neither Postgres nor `secrets.pgDsn`** → template **fails** with the clear
     `required` error (fail-closed assertion).
  4. **`secrets.existingSecret` set** → no Secret emitted; env refs point at the
     existing Secret.
  5. **`ingress` / `obs` / `networkPolicy` toggles** each add exactly their
     resources.
  6. **`config:` change** → pod `checksum/config` annotation changes.
- `docker build` succeeds; `docker run` the image and hit `/healthz` (proves the
  all-binaries image boots runtimed **non-root**).

### 7.2 Live proof (Option A — real local cluster)

1. `brew install helm kind`; `kind create cluster`.
2. `make docker-image` → `kind load docker-image runtime:<version>`.
3. `helm install runtime deploy/charts/runtime` with `postgresql.enabled=true`
   and a real agent registry in `config:` (the **scripted** test agents — no LLM
   key needed) → wait for rollout `Available`.
4. **Prove it runs:** `kubectl port-forward` the Service, then `runtimectl
   conformance` against it **and** a real `POST /sessions` turn round-tripping
   through the in-cluster Service → agentd child → response. Proves exec-spawn
   inside a pod, ConfigMap/Secret wiring, Postgres-subchart connectivity.
5. `helm upgrade` with a changed `config:` → assert the pod auto-rolls.
6. Optionally flip `obs.enabled` and confirm ServiceMonitor + dashboard ConfigMap
   apply.
7. `helm uninstall` + `kind delete cluster` — clean teardown.

### 7.3 What live proof is expected to catch

readOnlyRootFS biting a spawn-path write; non-root uid breaking agentd exec;
probe timing vs spawn-after-HTTP-start ordering; DSN/secret env-name mismatches;
image-pull / `kind load` mechanics. Each becomes a recorded fix (as in M1/M2).

### 7.4 Definition of done

Image builds + boots non-root; chart lints; all template permutations assert
correctly; a real `helm install` on kind yields a platform that passes
`runtimectl conformance` and serves a live turn; `helm upgrade` auto-rolls on
config change; docs + ROADMAP §C2 + memory updated; merged to `master`.

---

## 8. Open items to settle during planning

- Exact env/lookup names for sandboxd/browserd binary paths (config `command:`
  vs a fixed env) — confirm against `internal/gateway` + `cmd/*`.
- Exact writable paths the spawn path needs under `readOnlyRootFilesystem`
  (beyond `/tmp`) — confirm against `controlplane` + `agentruntime`.
- Whether any agentd/runtimed env var is required-but-unlisted for an in-pod
  boot (e.g. listen addrs for the spawned agents) — confirm against
  `runtime.yaml` semantics.
```
