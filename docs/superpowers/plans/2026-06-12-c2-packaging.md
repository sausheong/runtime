# C2 Packaging (M1) — Container Image + Helm Chart — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Package the runtime platform as a single all-binaries container image and a secure-by-default Helm chart, deployable to Kubernetes with one command, faithful to the current single-node supervisor model.

**Architecture:** Monolith-pod. One image carries `runtimed`+`agentd`+`sandboxd`+`browserd`+`runtimectl`; runtimed `exec`s agentd children inside the pod (spawn model unchanged). A Helm chart templates Deployment(1 replica, Recreate), Service, ConfigMap(`runtime.yaml`), Secret, ServiceAccount, optional Ingress/NetworkPolicy/obs, and an optional Bitnami Postgres subchart.

**Tech Stack:** Docker multi-stage (golang:1.25 → debian:bookworm-slim), Helm 3, Bitnami postgresql subchart, kind for live proof.

**Spec:** `docs/superpowers/specs/2026-06-12-c2-packaging-design.md`

**Resolved facts (from spec §8, verified against code):**
- Binary lookup: `RUNTIME_AGENTD_BIN` env (image sets `/app/agentd`); sandboxd/browserd launched via `command:` in `runtime.yaml` (`/app/sandboxd`, `/app/browserd`) — off by default.
- Writable paths: agentd does no disk writes; DBOS uses Postgres. Only `/tmp` scratch needed → `readOnlyRootFilesystem: true` + `/tmp` emptyDir works.
- In-pod boot: `RUNTIME_CTL_ADDR=":8080"` (bind all); agents bind `127.0.0.1:81xx` and are reverse-proxied on localhost (works in one pod); gateway `self_url` derives `http://127.0.0.1:8080`.

**Conventions:** `go` CLI is ground truth (ignore IDE/LSP). Docker build context is the **parent** of `runtime/` and `harness/`. Commit after each task.

**Helm dependency prerequisite (IMPORTANT — helm v4 quirk, discovered during
execution):** the chart declares a `postgresql` subchart dependency in
`Chart.yaml`, and Helm refuses to render (`helm template`) until that dependency
is vendored into `charts/`. **CRITICAL:** with helm v4.2.1, a vendored
`charts/postgresql-*.tgz` ALONE is NOT enough — even after `helm dependency
build`/`update` and with a matching `Chart.lock`, `helm template` still errors
`found in Chart.yaml, but missing in charts/ directory: postgresql`. The
dependency tarball must be **unpacked into a directory** (`charts/postgresql/`).
Render, `helm install --dry-run`, and `helm package` all work once the unpacked
dir is present (`helm package` includes exactly one copy of the subchart). The
canonical vendoring step is therefore:
```
helm dependency build deploy/charts/runtime
tar -xzf deploy/charts/runtime/charts/postgresql-*.tgz -C deploy/charts/runtime/charts/
rm -f deploy/charts/runtime/charts/postgresql-*.tgz   # keep only the unpacked dir
```
The orchestrator has already done this (after Task 2), so
`deploy/charts/runtime/charts/postgresql/` exists locally for Tasks 3/4/6's
render assertions. The entire `charts/` subdir + `Chart.lock` are gitignored in
Task 5 — do NOT `git add` them. If a render errors with "missing in charts/
directory: postgresql", re-run the three commands above.

---

### Task 1: Extend the image to all five binaries, non-root, OCI labels

**Files:**
- Modify: `deploy/Dockerfile`
- Modify: `Makefile` (add `docker-image` target + `VERSION`)

- [ ] **Step 1: Rewrite `deploy/Dockerfile`**

```dockerfile
# Build context MUST be the parent directory containing BOTH `runtime/` and
# `harness/` (runtime's go.mod has: replace github.com/sausheong/harness => ../harness).
# Build from the projects root:
#   docker build -f runtime/deploy/Dockerfile -t runtime:latest .
FROM golang:1.25 AS build
WORKDIR /src
COPY harness/ ./harness/
COPY runtime/ ./runtime/
WORKDIR /src/runtime
RUN go build -o /out/runtimed   ./cmd/runtimed \
 && go build -o /out/agentd     ./cmd/agentd \
 && go build -o /out/sandboxd   ./cmd/sandboxd \
 && go build -o /out/browserd   ./cmd/browserd \
 && go build -o /out/runtimectl ./cmd/runtimectl

FROM debian:bookworm-slim
ARG VERSION=dev
ARG REVISION=unknown
LABEL org.opencontainers.image.title="runtime" \
      org.opencontainers.image.source="https://github.com/sausheong/runtime" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${REVISION}"
RUN apt-get update && apt-get install -y ca-certificates \
 && rm -rf /var/lib/apt/lists/* \
 && useradd --uid 10001 --user-group --no-create-home --shell /usr/sbin/nologin runtime
WORKDIR /app
COPY --from=build /out/runtimed   /app/runtimed
COPY --from=build /out/agentd     /app/agentd
COPY --from=build /out/sandboxd   /app/sandboxd
COPY --from=build /out/browserd   /app/browserd
COPY --from=build /out/runtimectl /app/runtimectl
COPY runtime/runtime.yaml /app/runtime.yaml
ENV RUNTIME_AGENTD_BIN=/app/agentd \
    PATH=/app:$PATH
EXPOSE 8080
USER 10001
CMD ["/app/runtimed"]
```

- [ ] **Step 2: Add `docker-image` target + VERSION to `Makefile`**

Add near the top config block (after `GOFLAGS ?=`):
```makefile
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
REVISION    ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
IMAGE       ?= runtime
```
Add a new section before `# ---- Sandbox image ----`:
```makefile
# ---- Container image (all binaries) ----
.PHONY: docker-image
docker-image: ## Build the all-binaries image (run from anywhere; context is the projects root)
	docker build -f deploy/Dockerfile \
		--build-arg VERSION=$(VERSION) --build-arg REVISION=$(REVISION) \
		-t $(IMAGE):$(VERSION) -t $(IMAGE):latest ..
```

- [ ] **Step 3: Build the image (verify it compiles + all five binaries land)**

Run: `make docker-image`
Expected: build succeeds; final image tagged `runtime:<version>` and `runtime:latest`.

- [ ] **Step 4: Run the image and verify it boots non-root and serves /healthz**

Run:
```bash
docker run -d --name c2probe -p 18080:8080 \
  -e RUNTIME_PG_DSN="postgres://x:x@127.0.0.1:1/x?sslmode=disable" runtime:latest || true
sleep 2
docker exec c2probe id -u            # expect: 10001
docker exec c2probe ls /app          # expect: agentd browserd runtimectl runtime.yaml runtimed sandboxd
docker rm -f c2probe
```
Expected: `id -u` prints `10001`; all five binaries + runtime.yaml present.
(The control plane will fail to reach the bogus DSN — that's fine; we're only proving the image boots as uid 10001 with the binaries in place. A real /healthz check happens in the live proof on kind with a real Postgres.)

- [ ] **Step 5: Commit**

```bash
git add deploy/Dockerfile Makefile
git commit -m "feat(deploy): all-binaries non-root image + docker-image target"
```

---

### Task 2: Chart scaffold — Chart.yaml, values.yaml, helpers, .helmignore

**Files:**
- Create: `deploy/charts/runtime/Chart.yaml`
- Create: `deploy/charts/runtime/values.yaml`
- Create: `deploy/charts/runtime/templates/_helpers.tpl`
- Create: `deploy/charts/runtime/.helmignore`

- [ ] **Step 1: Create `deploy/charts/runtime/Chart.yaml`**

```yaml
apiVersion: v2
name: runtime
description: On-prem, durable LLM agent platform (AWS Bedrock AgentCore equivalent)
type: application
version: 0.1.0
appVersion: "0.1.0"
home: https://github.com/sausheong/runtime
sources:
  - https://github.com/sausheong/runtime
dependencies:
  - name: postgresql
    version: "16.x.x"
    repository: https://charts.bitnami.com/bitnami
    condition: postgresql.enabled
```

- [ ] **Step 2: Create `deploy/charts/runtime/values.yaml`**

```yaml
# Default values for the runtime chart. Secure-by-default.

image:
  repository: runtime
  tag: ""                 # defaults to .Chart.AppVersion
  pullPolicy: IfNotPresent
imagePullSecrets: []

nameOverride: ""
fullnameOverride: ""

# The supervisor is a single stateful process tree; >1 is unsupported.
replicaCount: 1

# runtime.yaml contents — agent registry + optional gateway config.
# Rendered into a ConfigMap and mounted at /etc/runtime/runtime.yaml.
config:
  agents: []
  # gateway:
  #   servers: []

# Secrets. For dev set inline; for prod set existingSecret (keys below ignored).
secrets:
  existingSecret: ""
  pgDsn: ""               # RUNTIME_PG_DSN (required unless postgresql.enabled)
  secretsKeys: ""         # RUNTIME_SECRETS_KEYS
  secretsPrimary: ""      # RUNTIME_SECRETS_PRIMARY
  adminBootstrap: ""      # RUNTIME_ADMIN_BOOTSTRAP
  # NOTE: legacy bearer tokens are NOT a secret env var — they live in
  # runtime.yaml's `tokens:`. Put them under `config.tokens` (dev only).

identity:
  enabled: false
  oidcIssuer: ""          # RUNTIME_OIDC_ISSUER
  oidcClientID: ""        # RUNTIME_OIDC_CLIENT_ID
  oidcRedirectURL: ""     # RUNTIME_OIDC_REDIRECT_URL

# Docker-dependent features (sandbox/browser). OFF by default — a plain pod has
# no Docker daemon. Set dockerHost to enable; see chart README for the DinD recipe.
features:
  dockerHost: ""          # DOCKER_HOST, e.g. tcp://localhost:2375

service:
  type: ClusterIP
  port: 8080

ingress:
  enabled: false
  className: ""
  annotations: {}
  hosts: []               # [{host: runtime.example.com, paths: [{path: /, pathType: Prefix}]}]
  tls: []

networkPolicy:
  enabled: false

obs:
  enabled: false          # ServiceMonitor + Grafana dashboard ConfigMap

postgresql:
  enabled: false
  auth:
    username: runtime
    password: runtime
    database: runtime

# Escape hatches for the DinD opt-in (empty by default).
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
podAnnotations: {}
```

- [ ] **Step 3: Create `deploy/charts/runtime/templates/_helpers.tpl`**

```
{{/* Expand the name of the chart. */}}
{{- define "runtime.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/* Fully qualified app name. */}}
{{- define "runtime.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- define "runtime.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "runtime.labels" -}}
helm.sh/chart: {{ include "runtime.chart" . }}
{{ include "runtime.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "runtime.selectorLabels" -}}
app.kubernetes.io/name: {{ include "runtime.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "runtime.serviceAccountName" -}}
{{- include "runtime.fullname" . -}}
{{- end -}}

{{/* The name of the Secret env refs target: existing, or our own. */}}
{{- define "runtime.secretName" -}}
{{- if .Values.secrets.existingSecret -}}
{{- .Values.secrets.existingSecret -}}
{{- else -}}
{{- include "runtime.fullname" . -}}
{{- end -}}
{{- end -}}

{{/*
Resolve the Postgres DSN.
- postgresql.enabled  → synthesize the in-cluster DSN.
- else secrets.pgDsn or secrets.existingSecret → caller provides it.
- else → fail (fail-closed).
Returns the literal DSN string ONLY for the synthesized case; for BYO it returns
empty (the Secret/existingSecret supplies it). Use runtime.requirePg to validate.
*/}}
{{- define "runtime.pgDsn" -}}
{{- if .Values.postgresql.enabled -}}
{{- $a := .Values.postgresql.auth -}}
{{- printf "postgres://%s:%s@%s-postgresql:5432/%s?sslmode=disable" $a.username $a.password (include "runtime.fullname" .) $a.database -}}
{{- else -}}
{{- .Values.secrets.pgDsn -}}
{{- end -}}
{{- end -}}

{{/* Fail-closed validation: a DSN source must exist. */}}
{{- define "runtime.requirePg" -}}
{{- if not .Values.postgresql.enabled -}}
{{- if and (not .Values.secrets.pgDsn) (not .Values.secrets.existingSecret) -}}
{{- fail "runtime: set postgresql.enabled=true, or secrets.pgDsn, or secrets.existingSecret (with a RUNTIME_PG_DSN key)" -}}
{{- end -}}
{{- end -}}
{{- end -}}
```

- [ ] **Step 4: Create `deploy/charts/runtime/.helmignore`**

```
.git
*.tgz
charts/*.tgz
Chart.lock
dist/
*.bak
```

- [ ] **Step 5: Lint the (still partial) chart structure**

Run: `helm lint deploy/charts/runtime`
Expected: it may warn about no templates yet rendering resources, but Chart.yaml/values.yaml must parse. (If lint errors purely on "no templates", that's resolved in Task 3; the acceptance here is that Chart.yaml and values.yaml are valid YAML and helpers parse — confirm with `helm template deploy/charts/runtime --dry-run 2>&1 | head` showing no YAML/parse error from these files.)

- [ ] **Step 6: Commit**

```bash
git add deploy/charts/runtime/Chart.yaml deploy/charts/runtime/values.yaml \
        deploy/charts/runtime/templates/_helpers.tpl deploy/charts/runtime/.helmignore
git commit -m "feat(chart): scaffold runtime chart (Chart.yaml, values, helpers)"
```

---

### Task 3: Core templates — serviceaccount, configmap, secret, deployment, service

**Files:**
- Create: `deploy/charts/runtime/templates/serviceaccount.yaml`
- Create: `deploy/charts/runtime/templates/configmap.yaml`
- Create: `deploy/charts/runtime/templates/secret.yaml`
- Create: `deploy/charts/runtime/templates/deployment.yaml`
- Create: `deploy/charts/runtime/templates/service.yaml`

- [ ] **Step 1: `serviceaccount.yaml`**

```yaml
apiVersion: v1
kind: ServiceAccount
metadata:
  name: {{ include "runtime.serviceAccountName" . }}
  labels:
    {{- include "runtime.labels" . | nindent 4 }}
```

- [ ] **Step 2: `configmap.yaml`** (renders runtime.yaml; empty config ⇒ minimal default)

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: {{ include "runtime.fullname" . }}
  labels:
    {{- include "runtime.labels" . | nindent 4 }}
data:
  runtime.yaml: |
    {{- if .Values.config }}
    {{- toYaml .Values.config | nindent 4 }}
    {{- else }}
    agents: []
    {{- end }}
```

- [ ] **Step 3: `secret.yaml`** (only emit when not using existingSecret; only set keys)

```yaml
{{- if not .Values.secrets.existingSecret }}
apiVersion: v1
kind: Secret
metadata:
  name: {{ include "runtime.fullname" . }}
  labels:
    {{- include "runtime.labels" . | nindent 4 }}
type: Opaque
stringData:
  {{- $dsn := include "runtime.pgDsn" . }}
  {{- if $dsn }}
  RUNTIME_PG_DSN: {{ $dsn | quote }}
  {{- end }}
  {{- if .Values.secrets.secretsKeys }}
  RUNTIME_SECRETS_KEYS: {{ .Values.secrets.secretsKeys | quote }}
  {{- end }}
  {{- if .Values.secrets.secretsPrimary }}
  RUNTIME_SECRETS_PRIMARY: {{ .Values.secrets.secretsPrimary | quote }}
  {{- end }}
  {{- if .Values.secrets.adminBootstrap }}
  RUNTIME_ADMIN_BOOTSTRAP: {{ .Values.secrets.adminBootstrap | quote }}
  {{- end }}
{{- end }}
```

NOTE: there is intentionally **no `RUNTIME_TOKENS`** key. Verified against
`cmd/runtimed/main.go`: legacy bearer tokens are read from `runtime.yaml`'s
`tokens:` via `cfg.TokenMap()`, NOT from an env var. Legacy tokens therefore go
in `values.config.tokens` (rendered into the ConfigMap's runtime.yaml), not the
Secret. The `secrets.tokens` value in values.yaml is removed in Step 0 below.

- [ ] **Step 0: Remove the dead `secrets.tokens` value**

In `deploy/charts/runtime/values.yaml`, delete the `tokens: ""` line under
`secrets:` (it has no consumer — tokens live in `config:`). Leave a comment on
the `secrets:` block pointing to `config.tokens` for legacy bearer tokens.

- [ ] **Step 4: `deployment.yaml`**

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{ include "runtime.fullname" . }}
  labels:
    {{- include "runtime.labels" . | nindent 4 }}
spec:
  replicas: 1   # supervisor is a single stateful process tree; see NOTES if you set replicaCount > 1
  strategy:
    type: Recreate
  selector:
    matchLabels:
      {{- include "runtime.selectorLabels" . | nindent 6 }}
  template:
    metadata:
      annotations:
        checksum/config: {{ toYaml .Values.config | sha256sum }}
        {{- with .Values.podAnnotations }}
        {{- toYaml . | nindent 8 }}
        {{- end }}
      labels:
        {{- include "runtime.selectorLabels" . | nindent 8 }}
    spec:
      {{- $unused := include "runtime.requirePg" . }}
      serviceAccountName: {{ include "runtime.serviceAccountName" . }}
      {{- with .Values.imagePullSecrets }}
      imagePullSecrets:
        {{- toYaml . | nindent 8 }}
      {{- end }}
      securityContext:
        {{- toYaml .Values.podSecurityContext | nindent 8 }}
      containers:
        - name: runtimed
          image: "{{ .Values.image.repository }}:{{ .Values.image.tag | default .Chart.AppVersion }}"
          imagePullPolicy: {{ .Values.image.pullPolicy }}
          args: ["/app/runtimed"]
          env:
            - name: RUNTIME_CTL_ADDR
              value: ":8080"
            - name: RUNTIME_CONFIG
              value: "/etc/runtime/runtime.yaml"
            - name: RUNTIME_AGENTD_BIN
              value: "/app/agentd"
            - name: RUNTIME_PG_DSN
              valueFrom:
                secretKeyRef:
                  name: {{ include "runtime.secretName" . }}
                  key: RUNTIME_PG_DSN
            {{- if or .Values.secrets.secretsKeys (and .Values.secrets.existingSecret false) }}
            - name: RUNTIME_SECRETS_KEYS
              valueFrom:
                secretKeyRef:
                  name: {{ include "runtime.secretName" . }}
                  key: RUNTIME_SECRETS_KEYS
                  optional: true
            {{- end }}
            {{- if .Values.secrets.secretsPrimary }}
            - name: RUNTIME_SECRETS_PRIMARY
              valueFrom:
                secretKeyRef:
                  name: {{ include "runtime.secretName" . }}
                  key: RUNTIME_SECRETS_PRIMARY
                  optional: true
            {{- end }}
            {{- if .Values.secrets.adminBootstrap }}
            - name: RUNTIME_ADMIN_BOOTSTRAP
              valueFrom:
                secretKeyRef:
                  name: {{ include "runtime.secretName" . }}
                  key: RUNTIME_ADMIN_BOOTSTRAP
                  optional: true
            {{- end }}
            {{- if .Values.identity.enabled }}
            - name: RUNTIME_OIDC_ISSUER
              value: {{ .Values.identity.oidcIssuer | quote }}
            - name: RUNTIME_OIDC_CLIENT_ID
              value: {{ .Values.identity.oidcClientID | quote }}
            {{- if .Values.identity.oidcRedirectURL }}
            - name: RUNTIME_OIDC_REDIRECT_URL
              value: {{ .Values.identity.oidcRedirectURL | quote }}
            {{- end }}
            {{- end }}
            {{- if .Values.features.dockerHost }}
            - name: DOCKER_HOST
              value: {{ .Values.features.dockerHost | quote }}
            {{- end }}
          ports:
            - name: http
              containerPort: 8080
          readinessProbe:
            httpGet: { path: /healthz, port: http }
            initialDelaySeconds: 3
            periodSeconds: 5
          livenessProbe:
            httpGet: { path: /healthz, port: http }
            initialDelaySeconds: 10
            periodSeconds: 10
          securityContext:
            {{- toYaml .Values.securityContext | nindent 12 }}
          resources:
            {{- toYaml .Values.resources | nindent 12 }}
          volumeMounts:
            - name: config
              mountPath: /etc/runtime
              readOnly: true
            - name: scratch
              mountPath: /tmp
            {{- with .Values.extraVolumeMounts }}
            {{- toYaml . | nindent 12 }}
            {{- end }}
        {{- with .Values.extraContainers }}
        {{- toYaml . | nindent 8 }}
        {{- end }}
      volumes:
        - name: config
          configMap:
            name: {{ include "runtime.fullname" . }}
        - name: scratch
          emptyDir: {}
        {{- with .Values.extraVolumes }}
        {{- toYaml . | nindent 8 }}
        {{- end }}
      {{- with .Values.nodeSelector }}
      nodeSelector:
        {{- toYaml . | nindent 8 }}
      {{- end }}
      {{- with .Values.tolerations }}
      tolerations:
        {{- toYaml . | nindent 8 }}
      {{- end }}
      {{- with .Values.affinity }}
      affinity:
        {{- toYaml . | nindent 8 }}
      {{- end }}
```

NOTE on the `RUNTIME_SECRETS_KEYS` guard: when `existingSecret` is set we cannot know which keys it contains, so emit the env ref only when the inline value is set OR existingSecret is set. Simplify Step 4's condition to:
```
{{- if or .Values.secrets.secretsKeys .Values.secrets.existingSecret }}
```
Apply that same `or ... existingSecret` pattern to `RUNTIME_SECRETS_PRIMARY` and `RUNTIME_ADMIN_BOOTSTRAP` so an existingSecret deployment can carry them (all use `optional: true`, so a missing key is harmless). Implementer: use the `or`-with-existingSecret form for these three optional refs.

- [ ] **Step 5: `service.yaml`**

```yaml
apiVersion: v1
kind: Service
metadata:
  name: {{ include "runtime.fullname" . }}
  labels:
    {{- include "runtime.labels" . | nindent 4 }}
spec:
  type: {{ .Values.service.type }}
  selector:
    {{- include "runtime.selectorLabels" . | nindent 4 }}
  ports:
    - name: http
      port: {{ .Values.service.port }}
      targetPort: http
      protocol: TCP
```

- [ ] **Step 6: Render with defaults and assert core invariants**

Run:
```bash
helm template r deploy/charts/runtime --set secrets.pgDsn='postgres://x:x@h:5432/d?sslmode=disable' > /tmp/c2-default.yaml
grep -q 'replicas: 1' /tmp/c2-default.yaml && echo OK-replicas
grep -q 'type: Recreate' /tmp/c2-default.yaml && echo OK-recreate
grep -q 'runAsNonRoot: true' /tmp/c2-default.yaml && echo OK-nonroot
grep -q 'readOnlyRootFilesystem: true' /tmp/c2-default.yaml && echo OK-rofs
grep -q 'path: /healthz' /tmp/c2-default.yaml && echo OK-healthz
grep -q 'checksum/config' /tmp/c2-default.yaml && echo OK-checksum
```
Expected: all six `OK-*` lines print.

- [ ] **Step 7: Assert fail-closed when no DSN source**

Run: `helm template r deploy/charts/runtime 2>&1 | grep -q 'set postgresql.enabled' && echo OK-failclosed`
Expected: `OK-failclosed` (template rendering fails with the required-DSN message).

- [ ] **Step 8: `helm lint`**

Run: `helm lint deploy/charts/runtime --set secrets.pgDsn='postgres://x:x@h:5432/d?sslmode=disable'`
Expected: `0 chart(s) failed`.

- [ ] **Step 9: Commit**

```bash
git add deploy/charts/runtime/templates/
git commit -m "feat(chart): core templates (deployment, service, configmap, secret, sa)"
```

---

### Task 4: Optional templates — ingress, networkpolicy, servicemonitor, dashboard, NOTES

**Files:**
- Create: `deploy/charts/runtime/templates/ingress.yaml`
- Create: `deploy/charts/runtime/templates/networkpolicy.yaml`
- Create: `deploy/charts/runtime/templates/servicemonitor.yaml`
- Create: `deploy/charts/runtime/templates/dashboard-configmap.yaml`
- Create: `deploy/charts/runtime/templates/NOTES.txt`

- [ ] **Step 1: `ingress.yaml`**

```yaml
{{- if .Values.ingress.enabled -}}
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: {{ include "runtime.fullname" . }}
  labels:
    {{- include "runtime.labels" . | nindent 4 }}
  {{- with .Values.ingress.annotations }}
  annotations:
    {{- toYaml . | nindent 4 }}
  {{- end }}
spec:
  {{- with .Values.ingress.className }}
  ingressClassName: {{ . }}
  {{- end }}
  {{- with .Values.ingress.tls }}
  tls:
    {{- toYaml . | nindent 4 }}
  {{- end }}
  rules:
    {{- range .Values.ingress.hosts }}
    - host: {{ .host | quote }}
      http:
        paths:
          {{- range .paths }}
          - path: {{ .path }}
            pathType: {{ .pathType | default "Prefix" }}
            backend:
              service:
                name: {{ include "runtime.fullname" $ }}
                port:
                  number: {{ $.Values.service.port }}
          {{- end }}
    {{- end }}
{{- end }}
```

- [ ] **Step 2: `networkpolicy.yaml`** (allow ingress to :8080; allow all egress so LLM/Postgres/gateway upstreams work)

```yaml
{{- if .Values.networkPolicy.enabled -}}
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: {{ include "runtime.fullname" . }}
  labels:
    {{- include "runtime.labels" . | nindent 4 }}
spec:
  podSelector:
    matchLabels:
      {{- include "runtime.selectorLabels" . | nindent 6 }}
  policyTypes: [Ingress, Egress]
  ingress:
    - ports:
        - port: 8080
          protocol: TCP
  egress:
    - {}
{{- end }}
```

- [ ] **Step 3: `servicemonitor.yaml`** (Prometheus Operator; auth-free /metrics)

```yaml
{{- if .Values.obs.enabled -}}
apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  name: {{ include "runtime.fullname" . }}
  labels:
    {{- include "runtime.labels" . | nindent 4 }}
spec:
  selector:
    matchLabels:
      {{- include "runtime.selectorLabels" . | nindent 6 }}
  endpoints:
    - port: http
      path: /metrics
      interval: 15s
{{- end }}
```

- [ ] **Step 4: `dashboard-configmap.yaml`** (mounts the existing Grafana dashboard JSON)

The dashboard JSON shipped with obs M1 is `deploy/grafana/dashboards/runtime.json` (verified).
Then:
```yaml
{{- if .Values.obs.enabled -}}
apiVersion: v1
kind: ConfigMap
metadata:
  name: {{ include "runtime.fullname" . }}-dashboard
  labels:
    grafana_dashboard: "1"
    {{- include "runtime.labels" . | nindent 4 }}
data:
  runtime-overview.json: |
    {{- .Files.Get "files/runtime-overview.json" | nindent 4 }}
{{- end }}
```
Then copy the obs dashboard into the chart's `files/` so it's packaged:
Run: `mkdir -p deploy/charts/runtime/files && cp deploy/grafana/dashboards/runtime.json deploy/charts/runtime/files/runtime-overview.json`

- [ ] **Step 5: `NOTES.txt`**

```
runtime {{ .Chart.AppVersion }} deployed as {{ include "runtime.fullname" . }}.

{{- if gt (int .Values.replicaCount) 1 }}

WARNING: replicaCount is {{ .Values.replicaCount }}, but runtime runs a single
stateful supervisor process tree. The chart pins the Deployment to 1 replica;
your replicaCount value is ignored. Running two supervisors against one Postgres
is unsupported.
{{- end }}

Reach the control plane:
  kubectl port-forward svc/{{ include "runtime.fullname" . }} 8080:{{ .Values.service.port }}
  curl http://127.0.0.1:8080/healthz

{{- if not .Values.postgresql.enabled }}

Postgres: bring-your-own (postgresql.enabled=false). Ensure RUNTIME_PG_DSN points
at a reachable database with the pgvector extension pre-created by a superuser.
{{- else }}

Postgres: the bundled subchart is enabled. NOTE: semantic memory needs the
pgvector extension; the Bitnami image does not create it. For memory features,
use an image/init that provides `CREATE EXTENSION vector` or set
postgresql.enabled=false and point RUNTIME_PG_DSN at a pgvector-enabled database.
{{- end }}

Sandbox/browser features are OFF (features.dockerHost unset) — a plain pod has no
Docker daemon. See the chart README for the DinD opt-in.
```

- [ ] **Step 6: Assert toggles add exactly their resources**

Run:
```bash
COMMON='--set secrets.pgDsn=postgres://x:x@h:5432/d?sslmode=disable'
helm template r deploy/charts/runtime $COMMON | grep -c 'kind: Ingress' | grep -q '^0$' && echo OK-no-ingress
helm template r deploy/charts/runtime $COMMON --set ingress.enabled=true --set 'ingress.hosts[0].host=x.example.com' --set 'ingress.hosts[0].paths[0].path=/' --set 'ingress.hosts[0].paths[0].pathType=Prefix' | grep -q 'kind: Ingress' && echo OK-ingress
helm template r deploy/charts/runtime $COMMON --set networkPolicy.enabled=true | grep -q 'kind: NetworkPolicy' && echo OK-netpol
helm template r deploy/charts/runtime $COMMON --set obs.enabled=true | grep -q 'kind: ServiceMonitor' && echo OK-sm
helm template r deploy/charts/runtime $COMMON --set obs.enabled=true | grep -q 'grafana_dashboard' && echo OK-dash
```
Expected: `OK-no-ingress OK-ingress OK-netpol OK-sm OK-dash` all print.

- [ ] **Step 7: Commit**

```bash
git add deploy/charts/runtime/templates/ deploy/charts/runtime/files/
git commit -m "feat(chart): optional ingress, networkpolicy, servicemonitor, dashboard, NOTES"
```

---

### Task 5: Postgres subchart wiring + helm make targets

**Files:**
- Modify: `Makefile`
- Modify: `.gitignore`

- [ ] **Step 1: Add `.gitignore` entries** (append)

```
# Helm packaged artifacts + vendored subcharts
/dist/
deploy/charts/runtime/charts/
deploy/charts/runtime/Chart.lock
```

- [ ] **Step 2: Add helm make targets to `Makefile`** (after the `docker-image` target)

```makefile
# ---- Helm chart ----
CHART ?= deploy/charts/runtime

.PHONY: helm-lint
helm-lint: ## Lint the Helm chart
	helm lint $(CHART) --set secrets.pgDsn=postgres://x:x@h:5432/d?sslmode=disable

.PHONY: helm-template
helm-template: ## Render the chart with a dummy DSN (quick check)
	helm template r $(CHART) --set secrets.pgDsn=postgres://x:x@h:5432/d?sslmode=disable

.PHONY: helm-deps
helm-deps: ## Vendor + unpack chart dependencies (Postgres subchart)
	helm dependency update $(CHART)
	@# helm v4 requires the subchart UNPACKED as a dir, not just the .tgz, to
	@# render/install (a vendored .tgz alone fails 'missing in charts/ directory').
	cd $(CHART)/charts && for t in *.tgz; do [ -e "$$t" ] || continue; tar -xzf "$$t" && rm -f "$$t"; done

.PHONY: helm-package
helm-package: helm-deps ## Package the chart into dist/
	mkdir -p dist
	helm package $(CHART) -d dist
```

- [ ] **Step 3: Vendor the dependency and confirm subchart wiring**

Run: `make helm-deps`
Expected: downloads then UNPACKS the postgresql subchart into
`deploy/charts/runtime/charts/postgresql/` (the loose `.tgz` is removed); creates
`Chart.lock`. (All gitignored.) Verify with `ls deploy/charts/runtime/charts/`
→ shows the `postgresql/` directory, no `.tgz`.

- [ ] **Step 4: Assert Postgres-enabled rendering synthesizes the DSN and includes the subchart**

Run:
```bash
helm template r deploy/charts/runtime --set postgresql.enabled=true > /tmp/c2-pg.yaml 2>/tmp/c2-pg.err; echo "render-exit=$?"
grep -q 'r-runtime-postgresql:5432' /tmp/c2-pg.yaml && echo OK-dsn-synth
grep -q 'app.kubernetes.io/name: postgresql' /tmp/c2-pg.yaml && echo OK-subchart
```
Expected: `render-exit=0` (no fail-closed error because postgresql.enabled supplies the DSN) and both `OK-dsn-synth` and `OK-subchart` print.

- [ ] **Step 5: Package the chart end-to-end**

Run: `make helm-package && ls dist/`
Expected: `dist/runtime-0.1.0.tgz` exists.

- [ ] **Step 6: Commit**

```bash
git add Makefile .gitignore
git commit -m "feat(chart): Postgres subchart wiring + helm make targets"
```

---

### Task 6: Render-permutation test harness

**Files:**
- Create: `deploy/charts/runtime/test.sh`

- [ ] **Step 1: Write `deploy/charts/runtime/test.sh`** (consolidated assertions; exit non-zero on any failure)

```bash
#!/usr/bin/env bash
# Render-permutation tests for the runtime Helm chart. Requires `helm`.
# Run: bash deploy/charts/runtime/test.sh
set -euo pipefail
CHART="$(cd "$(dirname "$0")" && pwd)"
DSN='--set secrets.pgDsn=postgres://x:x@h:5432/d?sslmode=disable'
fail() { echo "FAIL: $1" >&2; exit 1; }
ok()   { echo "ok: $1"; }

# 1. Defaults: core invariants.
out=$(helm template r "$CHART" $DSN)
grep -q 'replicas: 1'                 <<<"$out" || fail "replicas!=1"
grep -q 'type: Recreate'              <<<"$out" || fail "strategy!=Recreate"
grep -q 'runAsNonRoot: true'          <<<"$out" || fail "not nonroot"
grep -q 'readOnlyRootFilesystem: true'<<<"$out" || fail "rootfs not ro"
grep -q 'path: /healthz'              <<<"$out" || fail "no healthz probe"
grep -q 'checksum/config'             <<<"$out" || fail "no config checksum"
ok "defaults"

# 2. postgresql.enabled: DSN synthesized, subchart present, no DSN required.
out=$(helm template r "$CHART" --set postgresql.enabled=true)
grep -q 'r-runtime-postgresql:5432'        <<<"$out" || fail "DSN not synthesized"
grep -q 'app.kubernetes.io/name: postgresql' <<<"$out" || fail "subchart absent"
ok "postgresql.enabled"

# 3. Fail-closed: neither postgresql nor pgDsn nor existingSecret.
if helm template r "$CHART" >/dev/null 2>&1; then fail "expected fail-closed render"; fi
helm template r "$CHART" 2>&1 | grep -q 'set postgresql.enabled' || fail "wrong fail-closed message"
ok "fail-closed"

# 4. existingSecret: no Secret emitted; env refs target it.
out=$(helm template r "$CHART" --set secrets.existingSecret=mysecret)
if grep -qE '^kind: Secret' <<<"$out"; then fail "Secret should not be emitted"; fi
grep -q 'name: mysecret' <<<"$out" || fail "env ref not targeting existingSecret"
ok "existingSecret"

# 5. Toggles add exactly their resources.
grep -qc 'kind: Ingress' <<<"$(helm template r "$CHART" $DSN)" && \
  [ "$(helm template r "$CHART" $DSN | grep -c 'kind: Ingress')" = "0" ] || fail "ingress present by default"
helm template r "$CHART" $DSN --set ingress.enabled=true \
  --set 'ingress.hosts[0].host=x.example.com' \
  --set 'ingress.hosts[0].paths[0].path=/' \
  --set 'ingress.hosts[0].paths[0].pathType=Prefix' | grep -q 'kind: Ingress' || fail "ingress toggle"
helm template r "$CHART" $DSN --set networkPolicy.enabled=true | grep -q 'kind: NetworkPolicy' || fail "netpol toggle"
helm template r "$CHART" $DSN --set obs.enabled=true | grep -q 'kind: ServiceMonitor' || fail "servicemonitor toggle"
helm template r "$CHART" $DSN --set obs.enabled=true | grep -q 'grafana_dashboard' || fail "dashboard toggle"
ok "toggles"

# 6. config change flips the checksum annotation.
a=$(helm template r "$CHART" $DSN | grep 'checksum/config:' | head -1)
b=$(helm template r "$CHART" $DSN --set 'config.agents[0].id=x' --set 'config.agents[0].name=X' \
      --set 'config.agents[0].model=test/scripted' --set 'config.agents[0].listen_addr=127.0.0.1:8101' \
      | grep 'checksum/config:' | head -1)
[ "$a" != "$b" ] || fail "checksum did not change on config change"
ok "config checksum"

echo "ALL CHART TESTS PASSED"
```

- [ ] **Step 2: Make it executable and run it**

Run: `chmod +x deploy/charts/runtime/test.sh && bash deploy/charts/runtime/test.sh`
Expected: ends with `ALL CHART TESTS PASSED`.

- [ ] **Step 3: Commit**

```bash
git add deploy/charts/runtime/test.sh
git commit -m "test(chart): render-permutation test harness (6 permutations)"
```

---

### Task 7: Documentation — chart README + root README section

**Files:**
- Create: `deploy/charts/runtime/README.md`
- Modify: `README.md` (add a "Kubernetes / Helm" section)

- [ ] **Step 1: Write `deploy/charts/runtime/README.md`**

Cover, with real commands:
- **Quick start** (kind/dev): `make docker-image`; `kind load docker-image runtime:<v>`; `helm install runtime deploy/charts/runtime --set postgresql.enabled=true --set image.tag=<v>` and a `config:` with the scripted agents.
- **Three deploy modes:**
  1. BYO Postgres — `secrets.pgDsn` or `secrets.existingSecret` (recommended; pgvector required for memory).
  2. Bundled subchart — `postgresql.enabled=true` (note: Bitnami image lacks pgvector; fine without semantic memory).
  3. Dev-insecure — inline `secrets.tokens`, no identity.
- **Secure-by-default posture:** non-root uid 10001, readOnlyRootFilesystem, caps dropped, Recreate, single replica; how to supply secrets inline vs `existingSecret`.
- **`replicaCount: 1` constraint** and why (single-writer DBOS supervisor).
- **obs toggle:** `obs.enabled=true` needs the Prometheus Operator CRDs (ServiceMonitor) + a Grafana sidecar watching `grafana_dashboard` ConfigMaps.
- **Docker features (sandbox/browser):** off by default; the DinD opt-in recipe using `features.dockerHost` + `extraContainers` (a privileged `docker:dind` sidecar), with the explicit warning that it's a single-node convenience, not production posture.
- **Publishing:** `make helm-package`; manual `docker push` / `helm push dist/runtime-<v>.tgz oci://<registry>` (no CI in this milestone).

The DinD recipe block to include verbatim:
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
And note: the sandbox/browser images (`runtime-sandbox:latest`, `runtime-browser:latest`) must be loadable by that dind daemon, and the agent `runtime.yaml` must declare the upstreams with `command: /app/sandboxd` / `/app/browserd`.

- [ ] **Step 2: Add a "Kubernetes / Helm" section to the root `README.md`**

Find the deployment/Docker section (search for "docker-compose" or "Deploy"). Add a subsection linking to `deploy/charts/runtime/README.md` with a 5-line quick start and the single-replica/secure-by-default note. Keep it short; the chart README is the source of truth.

- [ ] **Step 3: Commit**

```bash
git add deploy/charts/runtime/README.md README.md
git commit -m "docs(chart): chart README + root README Kubernetes/Helm section"
```

---

### Task 8: Live proof on kind (orchestrator-run; not a subagent task)

> Executed by the orchestrator after Tasks 1–7 pass the final holistic review. Records bugs found as fixes, per prior-milestone practice.

- [ ] **Step 1: Install tooling**

Run: `brew install helm kind` (skip any already present).

- [ ] **Step 2: Create a cluster + build/load the image**

```bash
kind create cluster --name runtime-c2
make docker-image
kind load docker-image runtime:$(git describe --tags --always --dirty) --name runtime-c2
```

- [ ] **Step 3: Install the chart with bundled Postgres + scripted agents**

Write `/tmp/c2-values.yaml`:
```yaml
image:
  tag: "<the version from git describe>"
  pullPolicy: Never        # use the kind-loaded image, never pull
postgresql:
  enabled: true
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
Run: `helm install runtime deploy/charts/runtime -f /tmp/c2-values.yaml --wait --timeout 180s`
Expected: release deployed; pod `Running` and `Ready`.

- [ ] **Step 4: Prove it runs — conformance + a live turn**

```bash
kubectl port-forward svc/runtime 8080:8080 &
sleep 3
curl -fsS http://127.0.0.1:8080/healthz
RUNTIME_CTL_URL=http://127.0.0.1:8080 ./runtimectl conformance --agent support
# a real scripted turn through the in-cluster Service → agentd child:
curl -fsS -X POST http://127.0.0.1:8080/agents/support/sessions -d '{"input":"hello"}'
```
Expected: /healthz ok; conformance PASSES; the session turn round-trips.

- [ ] **Step 5: Prove config auto-roll on upgrade**

```bash
helm upgrade runtime deploy/charts/runtime -f /tmp/c2-values.yaml \
  --set 'config.agents[2].id=extra' --set 'config.agents[2].name=Extra' \
  --set 'config.agents[2].model=test/scripted' --set 'config.agents[2].listen_addr=127.0.0.1:8103' --wait
kubectl rollout status deploy/runtime
```
Expected: pod rolls (new ReplicaSet) due to the checksum/config annotation change.

- [ ] **Step 6: Teardown**

```bash
helm uninstall runtime
kind delete cluster --name runtime-c2
```

- [ ] **Step 7: Record results** in ROADMAP §C2 + the memory file (architecture, review catches, live-proof bugs/fixes), as every milestone does.

---

## Self-review notes (for the orchestrator)

- **Spec coverage:** §3 image → Task 1; §4 chart layout/values → Tasks 2/5; §5 templates → Tasks 3/4; §6 make/docs/DinD → Tasks 5/7; §7 testing → Tasks 3/4/6; §7.2 live proof → Task 8. All covered.
- **Type/name consistency:** `runtime.fullname`, `runtime.secretName`, `runtime.pgDsn`, `runtime.requirePg` defined in Task 2, used in Tasks 3/4. Env names match `cmd/runtimed/main.go` (`RUNTIME_PG_DSN`, `RUNTIME_CTL_ADDR`, `RUNTIME_CONFIG`, `RUNTIME_AGENTD_BIN`, `RUNTIME_SECRETS_KEYS/_PRIMARY`, `RUNTIME_ADMIN_BOOTSTRAP`, `RUNTIME_OIDC_*`).
- **Legacy tokens (resolved):** verified config-only — `cmd/runtimed/main.go` reads them via `cfg.TokenMap()` from `runtime.yaml` `tokens:`, there is no `RUNTIME_TOKENS` env var. The plan reflects this: no `RUNTIME_TOKENS` in the Secret, `secrets.tokens` removed from values, tokens documented as `config.tokens`.
- **Bitnami pgvector gap:** surfaced honestly in NOTES.txt and the README — the bundled subchart lacks the pgvector extension, so semantic memory needs BYO Postgres. Not a blocker for the scripted-agent live proof.
```
