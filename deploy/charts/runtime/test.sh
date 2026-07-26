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
# registration_generation is required by config.Validate for EVERY agent, so a
# render lacking it would produce a config runtimed refuses to load. The base
# fixture carries one; PAP_GENERATION below is the perAgentPods variant, whose
# agents must not set listen_addr.
AGENTS='--set config.agents[0].id=a --set config.agents[0].name=A --set config.agents[0].model=test/scripted --set config.agents[0].listen_addr=127.0.0.1:8101 --set config.agents[0].registration_generation=11111111-1111-4111-8111-111111111111'
PAP_GENERATION='--set config.agents[0].registration_generation=11111111-1111-4111-8111-111111111111'
fail() { echo "FAIL: $1" >&2; exit 1; }
ok()   { echo "ok: $1"; }

bash "$CHART/live-networkpolicy-test_test.sh"

# 1. Defaults: core invariants.
out=$(helm template r "$CHART" $DSN $AGENTS)
grep -q 'replicas: 1'                 <<<"$out" || fail "replicas!=1"
grep -q 'type: Recreate'              <<<"$out" || fail "strategy!=Recreate"
grep -q 'runAsNonRoot: true'          <<<"$out" || fail "not nonroot"
grep -q 'readOnlyRootFilesystem: true'<<<"$out" || fail "rootfs not ro"
grep -q 'path: /healthz'              <<<"$out" || fail "no healthz probe"
grep -q 'path: /readyz'               <<<"$out" || fail "no readyz probe"
grep -q 'name: metrics'               <<<"$out" || fail "no separate metrics service/port"
grep -q 'checksum/config'             <<<"$out" || fail "no config checksum"
ok "defaults"

# 1b. A signed digest is the complete workload identity in production and wins
# over any mutable tag.
out=$(helm template r "$CHART" $DSN $AGENTS \
  --set image.repository=ghcr.io/example/runtime \
  --set image.tag=mutable \
  --set image.digest=sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa)
grep -q 'image: "ghcr.io/example/runtime@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"' <<<"$out" ||
  fail "digest-pinned image not rendered"
if grep -q 'ghcr.io/example/runtime:mutable' <<<"$out"; then fail "tag rendered despite image.digest"; fi
ok "digest-pinned image"

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

# 5. NetworkPolicy is secure-by-default; other toggles add their resources.
[ "$(helm template r "$CHART" $DSN $AGENTS | grep -c 'kind: Ingress')" = "0" ] || fail "ingress present by default"
helm template r "$CHART" $DSN $AGENTS | grep -q 'kind: NetworkPolicy' || fail "netpol absent by default"
if helm template r "$CHART" $DSN $AGENTS --set networkPolicy.enabled=false | grep -q 'kind: NetworkPolicy'; then fail "netpol disable toggle"; fi
helm template r "$CHART" $DSN $AGENTS --set ingress.enabled=true \
  --set 'ingress.hosts[0].host=x.example.com' \
  --set 'ingress.hosts[0].paths[0].path=/' \
  --set 'ingress.hosts[0].paths[0].pathType=Prefix' | grep -q 'kind: Ingress' || fail "ingress toggle"
helm template r "$CHART" $DSN $AGENTS --set obs.enabled=true | grep -q 'kind: ServiceMonitor' || fail "servicemonitor toggle"
helm template r "$CHART" $DSN $AGENTS --set obs.enabled=true | grep -q 'grafana_dashboard' || fail "dashboard toggle"
out=$(helm template r "$CHART" $DSN $AGENTS)
grep -q 'app.kubernetes.io/name: prometheus' <<<"$out" ||
  fail "metrics ingress does not select same-namespace Prometheus by default"
[ "$(grep -c 'port: 9091' <<<"$out")" = "2" ] ||
  fail "management metrics port should appear only in Service and restricted NetworkPolicy"
out=$(helm template r "$CHART" $DSN $AGENTS \
  --set 'networkPolicy.metricsIngress[0].namespaceSelector.matchLabels.team=observability' \
  --set 'networkPolicy.metricsIngress[0].podSelector.matchLabels.app=collector')
grep -q 'team: observability' <<<"$out" || fail "metrics namespace selector not rendered"
grep -q 'app: collector' <<<"$out" || fail "metrics pod selector not rendered"
ok "toggles"

# 6. config change flips the checksum annotation.
a=$(helm template r "$CHART" $DSN $AGENTS | grep 'checksum/config:' | head -1)
b=$(helm template r "$CHART" $DSN $AGENTS --set 'config.agents[1].id=x' --set 'config.agents[1].name=X' \
      --set 'config.agents[1].model=test/scripted' --set 'config.agents[1].listen_addr=127.0.0.1:8102' \
      --set 'config.agents[1].registration_generation=33333333-3333-4333-8333-333333333333' \
      | grep 'checksum/config:' | head -1)
[ "$a" != "$b" ] || fail "checksum did not change on config change"
ok "config checksum"

# 7. perAgentPods: one StatefulSet + headless Service per agent; runtimed config
#    generated as remote pools; monolith Deployment still present (control plane).
PAP="--set scheduling.mode=perAgentPods --set secrets.existingSecret=pap-secret $PAP_GENERATION"
out=$(helm template r "$CHART" $DSN --set scheduling.mode=perAgentPods \
  --set secrets.agentPgDsn=postgres://agent:agent@h:5432/d \
  --set secrets.agentAuthTokens.support=agent-support-secret \
  --set config.agents[0].id=support --set config.agents[0].name=S \
  --set config.agents[0].model=test/scripted --set config.agents[0].replicas=2 \
  $PAP_GENERATION)
grep -q 'kind: StatefulSet'        <<<"$out" || fail "perAgentPods: no StatefulSet"
grep -q 'clusterIP: None'          <<<"$out" || fail "perAgentPods: no headless Service"
grep -q 'serviceName: r-agent-support-hl' <<<"$out" || fail "perAgentPods: wrong serviceName"
grep -q 'replicas: 2'              <<<"$out" || fail "perAgentPods: replicas not 2"
# The ordinal is resolved declaratively from the StatefulSet pod-index label and
# interpolated into DBOS__VMID. It must NOT go through a shell: the image is
# FROM scratch, so a /bin/sh wrapper StartErrors and CrashLoops every agent pod.
grep -q "fieldPath: metadata.labels\['apps.kubernetes.io/pod-index'\]" <<<"$out" ||
  fail "perAgentPods: replica ordinal not taken from the pod-index label"
grep -q 'value: "support#\$(RUNTIME_AGENT_REPLICA)"' <<<"$out" ||
  fail "perAgentPods: no ordinal VMID derive"
! grep -qE '^\s*(command|args):.*\bsh\b' <<<"$out" ||
  fail "perAgentPods: shell wrapper in a scratch image (no /bin/sh exists)"
# Must exec agentd explicitly: the image's default CMD is /app/runtimed, so an
# absent command silently runs the control plane inside every agent pod.
grep -q 'command: \["/app/agentd"\]' <<<"$out" ||
  fail "perAgentPods: agent container does not exec agentd"
grep -q 'support-{i}.r-agent-support-hl' <<<"$out" || fail "perAgentPods: generated url not {i}-templated"
# The dial template must be IDENTICAL on both sides (drift guard): the host base
# appears in both the headless Service name and the generated url.
grep -q 'r-agent-support-hl.default.svc.cluster.local' <<<"$out" || fail "perAgentPods: DNS base drift"
grep -q 'RUNTIME_AGENT_AUTH_TOKEN_SUPPORT' <<<"$out" || fail "perAgentPods: no per-agent bearer"
grep -q 'RUNTIME_PROVISION_REMOTE_AGENT_ROLE' <<<"$out" || fail "perAgentPods: remote role provisioning not enabled"
grep -q 'key: RUNTIME_AGENT_PG_DSN' <<<"$out" || fail "perAgentPods: agent does not use restricted DSN"
grep -q 'registration_generation: "11111111-1111-4111-8111-111111111111"' <<<"$out" ||
  fail "perAgentPods: generation absent from control-plane registry"
grep -A1 'name: RUNTIME_AGENT_GENERATION' <<<"$out" | grep -q '11111111-1111-4111-8111-111111111111' ||
  fail "perAgentPods: generation absent from agent pod"
grep -A1 'name: RUNTIME_DBOS_SCHEMA' <<<"$out" | grep -q 'dbos_agent_' ||
  fail "perAgentPods: isolated DBOS schema absent"
[ "$(grep -c 'kind: NetworkPolicy' <<<"$out")" -ge 2 ] || fail "perAgentPods: no dedicated agent NetworkPolicy"
ok "perAgentPods renders StatefulSet+headless+generated remote config"

# 7b. perAgentPods single-replica agent → concrete ordinal-0 url, no {i}, no replicas key.
out=$(helm template r "$CHART" $DSN $PAP \
  --set config.agents[0].id=solo --set config.agents[0].name=Solo \
  --set config.agents[0].model=test/scripted \
  --set config.agents[0].registration_generation=22222222-2222-4222-8222-222222222222)
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

# 7d. perAgentPods fails closed without a distinct bearer and agent DSN.
if helm template r "$CHART" $DSN --set scheduling.mode=perAgentPods \
  --set secrets.agentPgDsn=postgres://agent:agent@h:5432/d \
  --set config.agents[0].id=s --set config.agents[0].name=S \
  --set config.agents[0].model=m >/dev/null 2>&1; then
  fail "expected perAgentPods auth fail-closed"
fi
ok "perAgentPods fail-closed (per-agent auth absent)"

# 7e. perAgentPods fails closed without an explicit persisted generation.
if helm template r "$CHART" $DSN --set scheduling.mode=perAgentPods \
  --set secrets.agentPgDsn=postgres://agent:agent@h:5432/d \
  --set secrets.agentAuthTokens.s=agent-secret \
  --set config.agents[0].id=s --set config.agents[0].name=S \
  --set config.agents[0].model=m >/dev/null 2>&1; then
  fail "expected perAgentPods generation fail-closed"
fi
ok "perAgentPods fail-closed (instance generation absent)"

# 7f. One perAgentPods release has one restricted DB role and therefore one
# tenant. Sharing it across tenants would defeat the database row boundary.
if helm template r "$CHART" $DSN $PAP \
  --set config.agents[0].id=a --set config.agents[0].name=A \
  --set config.agents[0].model=m --set config.agents[0].tenant=alpha \
  --set config.agents[1].id=b --set config.agents[1].name=B \
  --set config.agents[1].model=m --set config.agents[1].tenant=beta >/dev/null 2>&1; then
  fail "expected perAgentPods multi-tenant DB-role fail-closed"
fi
ok "perAgentPods fail-closed (one restricted role cannot span tenants)"

# 7g. Tenant RLS is not an agent boundary. One shared restricted role may not
# be reused by two agents even when both belong to the same tenant.
if helm template r "$CHART" $DSN $PAP \
  --set config.agents[0].id=a --set config.agents[0].name=A \
  --set config.agents[0].model=m --set config.agents[0].tenant=alpha \
  --set config.agents[1].id=b --set config.agents[1].name=B \
  --set config.agents[1].model=m --set config.agents[1].tenant=alpha >/dev/null 2>&1; then
  fail "expected perAgentPods shared-agent-role fail-closed"
fi
ok "perAgentPods fail-closed (one restricted role cannot span agents)"

# 8. monolith regression: default mode still renders the M1 shape, no StatefulSet.
out=$(helm template r "$CHART" $DSN $AGENTS)
if grep -q 'kind: StatefulSet' <<<"$out"; then fail "monolith mode leaked a StatefulSet"; fi
grep -q 'kind: Deployment' <<<"$out" || fail "monolith: no Deployment"
ok "monolith regression (no StatefulSet)"

# 9. C3 M2 registration handshake: perAgentPods + secrets.registrationToken set →
#    agent StatefulSet carries RUNTIME_REGISTRATION_URL + a RUNTIME_REGISTRATION_TOKEN
#    secretKeyRef, and the chart Secret carries the RUNTIME_REGISTRATION_TOKEN key.
out=$(helm template r "$CHART" $DSN --set scheduling.mode=perAgentPods \
  --set secrets.agentPgDsn=postgres://agent:agent@h:5432/d \
  --set secrets.agentAuthTokens.support=agent-support-secret \
  --set config.agents[0].id=support --set config.agents[0].name=S \
  --set config.agents[0].model=test/scripted \
  $PAP_GENERATION \
  --set secrets.registrationToken=svk-a.b)
grep -q 'RUNTIME_REGISTRATION_URL'   <<<"$out" || fail "handshake: no RUNTIME_REGISTRATION_URL in StatefulSet"
grep -q '/register'                  <<<"$out" || fail "handshake: registration URL not /register"
# The token must arrive via a secretKeyRef (key: RUNTIME_REGISTRATION_TOKEN), not inline.
grep -q 'key: RUNTIME_REGISTRATION_TOKEN' <<<"$out" || fail "handshake: no RUNTIME_REGISTRATION_TOKEN secretKeyRef"
# The chart-managed Secret must carry the registration and per-agent token keys.
grep -qE '^\s+RUNTIME_REGISTRATION_TOKEN:' <<<"$out" || fail "handshake: Secret missing RUNTIME_REGISTRATION_TOKEN key"
grep -qE '^\s+RUNTIME_AGENT_AUTH_TOKEN_SUPPORT:' <<<"$out" || fail "handshake: Secret missing per-agent bearer"
ok "handshake on (perAgentPods + registrationToken)"

# 9b. perAgentPods WITHOUT a registration token (and no existingSecret) → handshake OFF;
#     no RUNTIME_REGISTRATION_URL (C2 M2 static-Secret behavior preserved).
out=$(helm template r "$CHART" $DSN --set scheduling.mode=perAgentPods \
  --set secrets.agentPgDsn=postgres://agent:agent@h:5432/d \
  --set secrets.agentAuthTokens.support=agent-support-secret \
  --set config.agents[0].id=support --set config.agents[0].name=S \
  --set config.agents[0].model=test/scripted \
  $PAP_GENERATION)
if grep -q 'RUNTIME_REGISTRATION_URL' <<<"$out"; then fail "handshake leaked without a registration token"; fi
ok "handshake off (perAgentPods, no token)"

# 9c. monolith regression: no registration env anywhere (local spawns fetch nothing).
out=$(helm template r "$CHART" $DSN $AGENTS --set secrets.registrationToken=svk-a.b)
if grep -q 'RUNTIME_REGISTRATION_URL' <<<"$out"; then fail "monolith leaked RUNTIME_REGISTRATION_URL"; fi
ok "monolith regression (no registration env)"

# 10. Forwarded identity fails closed without a stable asymmetric key pair and
# exposes the private key only to the control plane.
if helm template r "$CHART" $DSN $AGENTS \
  --set identity.subjectForwarding=true >/dev/null 2>&1; then
  fail "expected subject-forwarding signing-key fail-closed"
fi
out=$(helm template r "$CHART" $DSN $AGENTS \
  --set identity.subjectForwarding=true \
  --set secrets.identitySigningPrivateKey=private-test-key \
  --set secrets.identitySigningPublicKey=public-test-key)
grep -q 'RUNTIME_IDENTITY_SIGNING_PRIVATE_KEY' <<<"$out" || fail "subject forwarding: private key absent"
grep -q 'RUNTIME_IDENTITY_SIGNING_PUBLIC_KEY' <<<"$out" || fail "subject forwarding: public key absent"
[ "$(grep -c 'key: RUNTIME_IDENTITY_SIGNING_PRIVATE_KEY' <<<"$out")" = "1" ] ||
  fail "subject forwarding: private key exposed outside control plane"
ok "subject forwarding requires asymmetric signing keys"

# 11. Documented retention and concurrency controls are renderable without
# editing the chart, and agent controls reach both monolith children and
# perAgentPods StatefulSets.
out=$(helm template r "$CHART" $DSN $AGENTS \
  --set runtime.sessionRetention=168h \
  --set runtime.sessionRetentionBatch=77 \
  --set runtime.sessionRetentionDryRun=true \
  --set runtime.evalRetention=336h \
  --set runtime.maxRequests=91 \
  --set runtime.maxStreams=19 \
  --set agent.maxRequests=73 \
  --set agent.maxStreams=17 \
  --set agent.memoryRetention.dryRun=true \
  --set agent.memoryRetention.fact=720h)
for pair in \
  'RUNTIME_SESSION_RETENTION 168h' \
  'RUNTIME_SESSION_RETENTION_BATCH 77' \
  'RUNTIME_SESSION_RETENTION_DRY_RUN true' \
  'RUNTIME_EVAL_RETENTION 336h' \
  'RUNTIME_MAX_REQUESTS 91' \
  'RUNTIME_MAX_STREAMS 19' \
  'RUNTIME_AGENT_MAX_REQUESTS 73' \
  'RUNTIME_AGENT_MAX_STREAMS 17' \
  'RUNTIME_MEMORY_RETENTION_DRY_RUN true' \
  'RUNTIME_MEMORY_RETENTION_FACT 720h'
do
  name="${pair%% *}"
  value="${pair#* }"
  grep -A1 "name: ${name}" <<<"$out" | grep -q "value: \"${value}\"" ||
    fail "configured ${name}=${value} not rendered"
done

out=$(helm template r "$CHART" $DSN --set scheduling.mode=perAgentPods \
  --set secrets.agentPgDsn=postgres://agent:agent@h:5432/d \
  --set secrets.agentAuthTokens.support=agent-support-secret \
  --set config.agents[0].id=support --set config.agents[0].name=S \
  --set config.agents[0].model=test/scripted \
  $PAP_GENERATION \
  --set agent.maxRequests=73 --set agent.maxStreams=17 \
  --set agent.memoryRetention.fact=720h)
[ "$(grep -c 'name: RUNTIME_AGENT_MAX_REQUESTS' <<<"$out")" = "2" ] ||
  fail "agent request limit must reach control plane and per-agent pod"
grep -A1 'name: RUNTIME_MEMORY_RETENTION_FACT' <<<"$out" | grep -q 'value: "720h"' ||
  fail "perAgentPods memory retention not rendered"
ok "retention and concurrency configuration"

# 22. A monolith agent without registration_generation must FAIL AT RENDER.
# config.Validate requires it for every agent, not only perAgentPods ones, and
# the monolith path emits config wholesale. Without this gate the chart renders
# cleanly, `helm upgrade` reports success, and the pod then CrashLoops on a
# config runtimed refuses to load.
if helm template r "$CHART" $DSN \
  --set config.agents[0].id=a --set config.agents[0].name=A \
  --set config.agents[0].model=test/scripted \
  --set config.agents[0].listen_addr=127.0.0.1:8101 >/dev/null 2>&1; then
  fail "expected monolith registration_generation fail-closed"
fi
ok "monolith fail-closed (registration_generation absent)"

# 23. A too-short generation must also fail at render: config.Validate enforces
# 16-128 characters, so presence alone is not the rule the binary applies.
if helm template r "$CHART" $DSN \
  --set config.agents[0].id=a --set config.agents[0].name=A \
  --set config.agents[0].model=test/scripted \
  --set config.agents[0].listen_addr=127.0.0.1:8101 \
  --set config.agents[0].registration_generation=short >/dev/null 2>&1; then
  fail "expected monolith registration_generation length fail-closed"
fi
ok "monolith fail-closed (registration_generation too short)"

echo "ALL CHART TESTS PASSED"
