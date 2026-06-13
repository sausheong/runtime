#!/usr/bin/env bash
# Render-permutation tests for the runtime Helm chart. Requires `helm` and the
# vendored subchart (run `make helm-deps` first if charts/postgresql/ is missing).
# Run: bash deploy/charts/runtime/test.sh
set -euo pipefail
CHART="$(cd "$(dirname "$0")" && pwd)"
DSN='--set secrets.pgDsn=postgres://x:x@h:5432/d?sslmode=disable'
# The loader requires a non-empty agents list (each with id/name/model/listen_addr),
# and the chart enforces that at render time (runtime.requireAgents). Supply one
# valid agent in every render that is expected to succeed.
AGENTS='--set config.agents[0].id=a --set config.agents[0].name=A --set config.agents[0].model=test/scripted --set config.agents[0].listen_addr=127.0.0.1:8101'
fail() { echo "FAIL: $1" >&2; exit 1; }
ok()   { echo "ok: $1"; }

# 1. Defaults: core invariants.
out=$(helm template r "$CHART" $DSN $AGENTS)
grep -q 'replicas: 1'                 <<<"$out" || fail "replicas!=1"
grep -q 'type: Recreate'              <<<"$out" || fail "strategy!=Recreate"
grep -q 'runAsNonRoot: true'          <<<"$out" || fail "not nonroot"
grep -q 'readOnlyRootFilesystem: true'<<<"$out" || fail "rootfs not ro"
grep -q 'path: /healthz'              <<<"$out" || fail "no healthz probe"
grep -q 'checksum/config'             <<<"$out" || fail "no config checksum"
ok "defaults"

# 2. postgresql.enabled: DSN synthesized to the SUBCHART's service name
#    (<release>-postgresql, derived from .Release.Name), subchart present, no DSN required.
out=$(helm template r "$CHART" --set postgresql.enabled=true $AGENTS)
grep -q 'r-postgresql:5432'                  <<<"$out" || fail "DSN not synthesized to <release>-postgresql"
grep -q 'app.kubernetes.io/name: postgresql' <<<"$out" || fail "subchart absent"
# guard against the old bug (host == <fullname>-postgresql == r-runtime-postgresql)
if grep -q 'r-runtime-postgresql:5432' <<<"$out"; then fail "DSN host uses fullname, not release name (will not match PG service)"; fi
ok "postgresql.enabled + DSN matches subchart service"

# 3a. Fail-closed: no DSN source.
# Capture combined output without pipefail aborting on helm's expected non-zero exit.
if helm template r "$CHART" $AGENTS >/dev/null 2>&1; then fail "expected DSN fail-closed render"; fi
err=$(helm template r "$CHART" $AGENTS 2>&1 || true)
grep -q 'set postgresql.enabled' <<<"$err" || fail "wrong DSN fail-closed message"
ok "fail-closed (no DSN)"

# 3b. Fail-closed: no agents (empty registry would CrashLoop runtimed).
if helm template r "$CHART" $DSN >/dev/null 2>&1; then fail "expected agents fail-closed render"; fi
err=$(helm template r "$CHART" $DSN 2>&1 || true)
grep -q 'config.agents must list at least one agent' <<<"$err" || fail "wrong agents fail-closed message"
ok "fail-closed (no agents)"

# 4. existingSecret: no own Secret emitted; env refs target it.
out=$(helm template r "$CHART" --set secrets.existingSecret=mysecret $AGENTS)
if grep -qE '^kind: Secret' <<<"$out"; then fail "Secret should not be emitted"; fi
grep -q 'name: mysecret' <<<"$out" || fail "env ref not targeting existingSecret"
ok "existingSecret"

# 5. Toggles add exactly their resources.
[ "$(helm template r "$CHART" $DSN $AGENTS | grep -c 'kind: Ingress')" = "0" ] || fail "ingress present by default"
helm template r "$CHART" $DSN $AGENTS --set ingress.enabled=true \
  --set 'ingress.hosts[0].host=x.example.com' \
  --set 'ingress.hosts[0].paths[0].path=/' \
  --set 'ingress.hosts[0].paths[0].pathType=Prefix' | grep -q 'kind: Ingress' || fail "ingress toggle"
helm template r "$CHART" $DSN $AGENTS --set networkPolicy.enabled=true | grep -q 'kind: NetworkPolicy' || fail "netpol toggle"
helm template r "$CHART" $DSN $AGENTS --set obs.enabled=true | grep -q 'kind: ServiceMonitor' || fail "servicemonitor toggle"
helm template r "$CHART" $DSN $AGENTS --set obs.enabled=true | grep -q 'grafana_dashboard' || fail "dashboard toggle"
ok "toggles"

# 6. config change flips the checksum annotation.
a=$(helm template r "$CHART" $DSN $AGENTS | grep 'checksum/config:' | head -1)
b=$(helm template r "$CHART" $DSN $AGENTS --set 'config.agents[1].id=x' --set 'config.agents[1].name=X' \
      --set 'config.agents[1].model=test/scripted' --set 'config.agents[1].listen_addr=127.0.0.1:8102' \
      | grep 'checksum/config:' | head -1)
[ "$a" != "$b" ] || fail "checksum did not change on config change"
ok "config checksum"

# 7. perAgentPods: one StatefulSet + headless Service per agent; runtimed config
#    generated as remote pools; monolith Deployment still present (control plane).
PAP='--set scheduling.mode=perAgentPods'
out=$(helm template r "$CHART" $DSN $PAP \
  --set config.agents[0].id=support --set config.agents[0].name=S \
  --set config.agents[0].model=test/scripted --set config.agents[0].replicas=2)
grep -q 'kind: StatefulSet'        <<<"$out" || fail "perAgentPods: no StatefulSet"
grep -q 'clusterIP: None'          <<<"$out" || fail "perAgentPods: no headless Service"
grep -q 'serviceName: r-agent-support-hl' <<<"$out" || fail "perAgentPods: wrong serviceName"
grep -q 'replicas: 2'              <<<"$out" || fail "perAgentPods: replicas not 2"
grep -q 'DBOS__VMID="support#'     <<<"$out" || fail "perAgentPods: no ordinal VMID derive"
grep -q 'support-{i}.r-agent-support-hl' <<<"$out" || fail "perAgentPods: generated url not {i}-templated"
# The dial template must be IDENTICAL on both sides (drift guard): the host base
# appears in both the headless Service name and the generated url.
grep -q 'r-agent-support-hl.default.svc.cluster.local' <<<"$out" || fail "perAgentPods: DNS base drift"
ok "perAgentPods renders StatefulSet+headless+generated remote config"

# 7b. perAgentPods single-replica agent → concrete ordinal-0 url, no {i}, no replicas key.
out=$(helm template r "$CHART" $DSN $PAP \
  --set config.agents[0].id=solo --set config.agents[0].name=Solo \
  --set config.agents[0].model=test/scripted)
grep -q 'solo-0.r-agent-solo-hl'  <<<"$out" || fail "perAgentPods solo: url not concrete ordinal 0"
if grep -A6 'id: solo' <<<"$out" | grep -q '{i}'; then fail "perAgentPods solo: url still has {i}"; fi
ok "perAgentPods single-replica → concrete url"

# 7c. perAgentPods fail-closed: an agent that sets listen_addr.
if helm template r "$CHART" $DSN $PAP \
  --set config.agents[0].id=s --set config.agents[0].name=S \
  --set config.agents[0].model=m --set config.agents[0].listen_addr=127.0.0.1:8101 >/dev/null 2>&1; then
  fail "expected perAgentPods listen_addr fail-closed"
fi
ok "perAgentPods fail-closed (listen_addr set)"

# 8. monolith regression: default mode still renders the M1 shape, no StatefulSet.
out=$(helm template r "$CHART" $DSN $AGENTS)
if grep -q 'kind: StatefulSet' <<<"$out"; then fail "monolith mode leaked a StatefulSet"; fi
grep -q 'kind: Deployment' <<<"$out" || fail "monolith: no Deployment"
ok "monolith regression (no StatefulSet)"

# 9. C3 M2 registration handshake: perAgentPods + secrets.registrationToken set →
#    agent StatefulSet carries RUNTIME_REGISTRATION_URL + a RUNTIME_REGISTRATION_TOKEN
#    secretKeyRef, and the chart Secret carries the RUNTIME_REGISTRATION_TOKEN key.
out=$(helm template r "$CHART" $DSN $PAP \
  --set config.agents[0].id=support --set config.agents[0].name=S \
  --set config.agents[0].model=test/scripted \
  --set secrets.registrationToken=svk-a.b)
grep -q 'RUNTIME_REGISTRATION_URL'   <<<"$out" || fail "handshake: no RUNTIME_REGISTRATION_URL in StatefulSet"
grep -q '/register'                  <<<"$out" || fail "handshake: registration URL not /register"
# The token must arrive via a secretKeyRef (key: RUNTIME_REGISTRATION_TOKEN), not inline.
grep -q 'key: RUNTIME_REGISTRATION_TOKEN' <<<"$out" || fail "handshake: no RUNTIME_REGISTRATION_TOKEN secretKeyRef"
# The chart-managed Secret must carry the token key.
grep -qE '^\s+RUNTIME_REGISTRATION_TOKEN:' <<<"$out" || fail "handshake: Secret missing RUNTIME_REGISTRATION_TOKEN key"
ok "handshake on (perAgentPods + registrationToken)"

# 9b. perAgentPods WITHOUT a registration token (and no existingSecret) → handshake OFF;
#     no RUNTIME_REGISTRATION_URL (C2 M2 static-Secret behavior preserved).
out=$(helm template r "$CHART" $DSN $PAP \
  --set config.agents[0].id=support --set config.agents[0].name=S \
  --set config.agents[0].model=test/scripted)
if grep -q 'RUNTIME_REGISTRATION_URL' <<<"$out"; then fail "handshake leaked without a registration token"; fi
ok "handshake off (perAgentPods, no token)"

# 9c. monolith regression: no registration env anywhere (local spawns fetch nothing).
out=$(helm template r "$CHART" $DSN $AGENTS --set secrets.registrationToken=svk-a.b)
if grep -q 'RUNTIME_REGISTRATION_URL' <<<"$out"; then fail "monolith leaked RUNTIME_REGISTRATION_URL"; fi
ok "monolith regression (no registration env)"

echo "ALL CHART TESTS PASSED"
