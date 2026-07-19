#!/usr/bin/env bash
# v1.0 capstone proof — brings up the turnkey stack and asserts ALL SIX PILLARS
# end-to-end, plus persistence and no-secrets-in-logs. Run from anywhere;
# resolves its own dir.
# Requires Docker + these host ports free: 8080 5432 9090 9093 3000 16686 4318 9000.
set -uo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$HERE/../.." && pwd)"   # repo root (/Users/sausheong/projects/runtime)
# v1-probe does the MCP-session calls bash can't (gateway tool call, sandbox
# exec). Run from the host against localhost:8080 via `go run`.
probe() { ( cd "$ROOT" && go run ./cmd/v1-probe "$@" ); }
cd "$HERE"

fails=0
pass() { echo "PASS: $*"; }
fail() { echo "FAIL: $*" >&2; fails=$((fails+1)); }

# Resolve the compose network name (project is `runtime` -> runtime_default).
# Exported so the step-4 python heredoc subprocess inherits it.
export NET="runtime_default"

emb_call() { # $1 = input text -> prints JSON
  docker run --rm --network "$NET" curlimages/curl:latest -s -X POST \
    http://embedder:8000/embeddings -H 'content-type: application/json' \
    -d "{\"model\":\"x\",\"input\":\"$1\"}"
}

cleanup() {
  echo "--- collecting logs + tearing down ---"
  docker compose logs > /tmp/m2-smoke-logs.txt 2>&1 || true
  # `go run` spawns a compiled child that outlives the wrapper PID, so also
  # reap any rest-demo process by name (it binds :9000).
  [ -n "${DEMO_PID:-}" ] && kill "$DEMO_PID" 2>/dev/null || true
  pkill -f 'examples/rest-demo' 2>/dev/null || true
  pkill -f '/rest-demo$' 2>/dev/null || true
  rm -f /tmp/m2-restdemo.pid
  docker compose down -v >/dev/null 2>&1 || true
}
trap cleanup EXIT

# 0. Fresh secrets + clean build + up.
./init.sh --force >/dev/null
set -a; . ./.env; set +a
# RUNTIME_EMBED_RECALL_FLOOR is not written to .env (it has a compose default);
# default it here so step 4 doesn't hit an unbound var under `set -u`.
: "${RUNTIME_EMBED_RECALL_FLOOR:=0.60}"
echo "--- building (this is slow on first run) ---"
docker compose --profile build-only build
echo "--- starting stack ---"
docker compose up -d

# 1. runtimed healthy.
ok=0
for i in $(seq 1 60); do
  if curl -sf -H "Authorization: Bearer $RUNTIME_ADMIN_BOOTSTRAP" localhost:8080/healthz >/dev/null 2>&1; then ok=1; break; fi
  sleep 2
done
[ "$ok" = 1 ] && pass "runtimed healthy" || fail "runtimed never became healthy"

# 2. Memory/pgvector extension present.
if docker compose exec -T postgres psql -U runtime -d runtime -c '\dx' 2>/dev/null | grep -qi vector; then
  pass "pgvector extension present"; else fail "pgvector extension missing"; fi

# 3. Embedder returns a 384-dim vector.
dim="$(emb_call hello | python3 -c "import sys,json;print(len(json.load(sys.stdin)['data'][0]['embedding']))" 2>/dev/null || echo 0)"
[ "$dim" = 384 ] && pass "embedder returns 384-dim" || fail "embedder dim=$dim (want 384)"

# 4. Recall floor: related clears floor, unrelated does not.
python3 - "$RUNTIME_EMBED_RECALL_FLOOR" <<PY
import json, math, subprocess, sys, os
floor=float(sys.argv[1])
NET=os.environ.get("NET","runtime_default")
def emb(t):
    out=subprocess.check_output(["docker","run","--rm","--network",NET,"curlimages/curl:latest",
      "-s","-X","POST","http://embedder:8000/embeddings","-H","content-type: application/json",
      "-d",json.dumps({"model":"x","input":t})])
    return json.loads(out)["data"][0]["embedding"]
def cos(u,v):
    d=sum(a*b for a,b in zip(u,v));import math
    return d/(math.sqrt(sum(a*a for a in u))*math.sqrt(sum(b*b for b in v)))
s=emb("the database schema uses an append-only event log")
q=emb("tell me about the database design")
u=emb("the user prefers dark mode in the UI")
rel,un=cos(s,q),cos(s,u)
print(f"related={rel:.3f} unrelated={un:.3f} floor={floor}")
sys.exit(0 if (rel>=floor and un<floor) else 1)
PY
[ $? -eq 0 ] && pass "recall floor calibrated (related clears, unrelated does not)" || fail "recall floor wrong: related/unrelated straddle issue"

# 5. Identity: unauth 401; bootstrap creates a tenant.
code="$(curl -s -o /dev/null -w '%{http_code}' localhost:8080/admin/upstreams)"
[ "$code" = 401 ] && pass "unauth admin route 401" || fail "unauth admin route code=$code (want 401)"
if curl -sf -H "Authorization: Bearer $RUNTIME_ADMIN_BOOTSTRAP" -X POST localhost:8080/admin/tenants \
   -d '{"id":"smoke","name":"Smoke Tenant"}' >/dev/null 2>&1; then
  pass "tenant created via onboarding API"; else fail "tenant create failed"; fi

# 6. Gateway: register rest-demo OpenAPI upstream -> reaches up.
( cd "$ROOT" && RUNTIME_DEMO_ADDR=:9000 go run ./examples/rest-demo >/tmp/m2-restdemo.log 2>&1 & echo $! > /tmp/m2-restdemo.pid )
DEMO_PID="$(cat /tmp/m2-restdemo.pid 2>/dev/null || true)"
sleep 4
# Per-tenant upstream credential (broker → injected into the upstream headers at
# dial). rest-demo accepts any header, so this stays deterministic while
# exercising the credential-at-dial path on the compose stack.
curl -sf -H "Authorization: Bearer $RUNTIME_ADMIN_BOOTSTRAP" -X POST localhost:8080/admin/secrets \
  -d '{"tenant":"smoke","name":"ORDERS_API_KEY","value":"demo-secret"}' >/dev/null 2>&1 \
  && pass "tenant credential set" || fail "set credential failed"
# base_url is REQUIRED here: the spec's own servers[] says http://localhost:9000,
# which from inside the runtimed container is the container's loopback, not the
# host. Override it to host.docker.internal so the gateway's tool calls reach the
# host-run rest-demo. (The spec is fetched from the openapi URL; the operation
# calls use base_url.)
if curl -sf -H "Authorization: Bearer $RUNTIME_ADMIN_BOOTSTRAP" -X POST localhost:8080/admin/upstreams \
   -d '{"tenant":"smoke","name":"orders","transport":"openapi","openapi":"http://host.docker.internal:9000/openapi.yaml","base_url":"http://host.docker.internal:9000","cred_secret":"ORDERS_API_KEY","cred_header":"Authorization"}' >/dev/null 2>&1; then
  pass "openapi upstream registered (with credential)"; else fail "upstream register failed"; fi
# Live upstream state lives on /gateway/status ([]UpstreamStatus with a `state`
# field), NOT /admin/upstreams (which returns the DB rows, no live state). The
# bootstrap principal is a superuser, so /gateway/status returns all tenants.
up=0
for i in $(seq 1 20); do
  st="$(curl -s -H "Authorization: Bearer $RUNTIME_ADMIN_BOOTSTRAP" localhost:8080/gateway/status \
     | python3 -c "import sys,json;d=json.load(sys.stdin);L=d if isinstance(d,list) else d.get('upstreams',[]);print(next((u.get('state','') for u in L if u.get('name')=='orders'),''))" 2>/dev/null || echo "")"
  [ "$st" = up ] && { up=1; break; }
  sleep 2
done
[ "$up" = 1 ] && pass "gateway upstream up" || fail "upstream never reached up"

# Gateway: discovery + REST-adapter tool CALL through /gateway/mcp (MCP session
# via v1-probe; agent_keys[default]=bootstrap).
probe list --base http://localhost:8080 --key "$RUNTIME_ADMIN_BOOTSTRAP" \
  && pass "gateway: orders(REST)+sandbox(MCP) federated" || fail "gateway tool discovery failed"
probe call-rest --base http://localhost:8080 --key "$RUNTIME_ADMIN_BOOTSTRAP" \
  && pass "gateway: REST-adapter tool call returned data" || fail "gateway REST call failed"

# Sandboxes: real container launch + code exec via the mounted docker.sock.
probe sandbox --base http://localhost:8080 --key "$RUNTIME_ADMIN_BOOTSTRAP" \
  && pass "sandbox: execute_code returned 42" || fail "sandbox exec failed"

# Identity: cross-tenant refusal. A SECOND tenant's operator key must not see
# tenant smoke's upstream on /gateway/status.
curl -sf -H "Authorization: Bearer $RUNTIME_ADMIN_BOOTSTRAP" -X POST localhost:8080/admin/tenants \
  -d '{"id":"other","name":"Other"}' >/dev/null 2>&1 || true
otherkey="$(curl -s -H "Authorization: Bearer $RUNTIME_ADMIN_BOOTSTRAP" -X POST localhost:8080/admin/keys \
  -d '{"tenant":"other","role":"operator","label":"proof"}' | python3 -c "import sys,json;print(json.load(sys.stdin).get('plaintext',''))" 2>/dev/null || echo "")"
if [ -n "$otherkey" ]; then
  seen="$(curl -s -H "Authorization: Bearer $otherkey" localhost:8080/gateway/status \
    | python3 -c "import sys,json;d=json.load(sys.stdin);L=d if isinstance(d,list) else d.get('upstreams',[]);print(sum(1 for u in L if u.get('name')=='orders'))" 2>/dev/null || echo "?")"
  [ "$seen" = 0 ] && pass "cross-tenant: other tenant cannot see orders upstream" || fail "cross-tenant leak (other saw orders: $seen)"
else
  fail "could not mint second-tenant key for cross-tenant check"
fi

# 7. Observability.
curl -sf localhost:3000/api/health >/dev/null && pass "grafana healthy" || fail "grafana down"
curl -sf localhost:16686/ >/dev/null && pass "jaeger UI reachable" || fail "jaeger down"
if curl -s "localhost:9090/api/v1/targets" | grep -q runtimed; then pass "prometheus scrapes runtimed"; else fail "runtimed not a prometheus target"; fi
curl -sf localhost:9093/-/healthy >/dev/null && pass "alertmanager healthy" || fail "alertmanager not healthy"
if curl -s "localhost:9090/api/v1/rules" | grep -q '"name":"runtime"'; then pass "prometheus loaded alert rules"; else fail "prometheus alert rules not loaded"; fi

# Observability: a trace reached Jaeger (runtimed exports OTLP → collector →
# jaeger). Service name is exactly "runtimed".
sleep 3
svc="$(curl -s 'http://localhost:16686/api/services' | python3 -c "import sys,json;print(','.join(json.load(sys.stdin).get('data') or []))" 2>/dev/null || echo "")"
if echo "$svc" | grep -qi runtimed; then
  pass "jaeger has the runtimed service (traces flowing)"
else
  tr="$(curl -s 'http://localhost:16686/api/traces?service=runtimed&limit=1' | python3 -c "import sys,json;print(len(json.load(sys.stdin).get('data') or []))" 2>/dev/null || echo 0)"
  { [ "$tr" -ge 1 ] 2>/dev/null && pass "jaeger has >=1 runtimed trace"; } || fail "no runtimed trace in jaeger (services=$svc)"
fi

# 8. Persistence across down/up (no -v).
docker compose exec -T postgres psql -U runtime -d runtime -c \
  "CREATE TABLE IF NOT EXISTS m2_persist(id int); INSERT INTO m2_persist VALUES (42);" >/dev/null 2>&1
docker compose down >/dev/null 2>&1
docker compose up -d >/dev/null 2>&1
for i in $(seq 1 30); do docker compose exec -T postgres pg_isready -U runtime >/dev/null 2>&1 && break; sleep 2; done
got="$(docker compose exec -T postgres psql -U runtime -d runtime -tAc 'SELECT id FROM m2_persist' 2>/dev/null | tr -d '[:space:]')"
[ "$got" = 42 ] && pass "data persists across down/up" || fail "persistence broken (got '$got')"

# 9. No secrets in logs.
docker compose logs > /tmp/m2-smoke-logs.txt 2>&1 || true
if grep -qF "$RUNTIME_ADMIN_BOOTSTRAP" /tmp/m2-smoke-logs.txt; then fail "bootstrap key leaked into logs"; else pass "bootstrap key absent from logs"; fi
key_b64="${RUNTIME_SECRETS_KEYS#*:}"
if grep -qF "$key_b64" /tmp/m2-smoke-logs.txt; then fail "AES key leaked into logs"; else pass "AES key absent from logs"; fi

# Summary.
echo "----------------------------------------"
if [ "$fails" -eq 0 ]; then echo "ALL PASS — v1.0 proof green"; else echo "$fails CHECK(S) FAILED"; exit 1; fi
