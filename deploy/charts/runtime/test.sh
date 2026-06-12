#!/usr/bin/env bash
# Render-permutation tests for the runtime Helm chart. Requires `helm` and the
# vendored subchart (run `make helm-deps` first if charts/postgresql/ is missing).
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
grep -q 'r-runtime-postgresql:5432'          <<<"$out" || fail "DSN not synthesized"
grep -q 'app.kubernetes.io/name: postgresql' <<<"$out" || fail "subchart absent"
ok "postgresql.enabled"

# 3. Fail-closed: neither postgresql nor pgDsn nor existingSecret.
# Capture combined output without `set -e`/pipefail aborting on helm's expected
# non-zero exit, then assert on the message.
if helm template r "$CHART" >/dev/null 2>&1; then fail "expected fail-closed render"; fi
err=$(helm template r "$CHART" 2>&1 || true)
grep -q 'set postgresql.enabled' <<<"$err" || fail "wrong fail-closed message"
ok "fail-closed"

# 4. existingSecret: no own Secret emitted; env refs target it.
out=$(helm template r "$CHART" --set secrets.existingSecret=mysecret)
if grep -qE '^kind: Secret' <<<"$out"; then fail "Secret should not be emitted"; fi
grep -q 'name: mysecret' <<<"$out" || fail "env ref not targeting existingSecret"
ok "existingSecret"

# 5. Toggles add exactly their resources.
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
