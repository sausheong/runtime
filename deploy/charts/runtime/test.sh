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

echo "ALL CHART TESTS PASSED"
