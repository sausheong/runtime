#!/usr/bin/env bash
# Cloud deployment test suite — exercises the LIVE GCP deployment (a test
# environment) end-to-end and reports PASS/FAIL, exiting non-zero on any failure.
#
# It is the cloud analogue of deploy/compose/v1-proof.sh: same pass/fail idiom,
# but targets the distributed 3-VM cloud deployment (control-plane + agent-go +
# agent-python) and asserts only what is actually running there.
#
# Access: opens an IAP tunnel to the control plane's :8080 and drives the runtimed
# API over localhost. The admin bootstrap token (superuser bearer) is read from
# the running container, so nothing needs to be supplied out-of-band. Dashboard
# checks run via a single `gcloud ssh` command on the CP VM (the dashboards only
# listen on the VM's own localhost).
#
# Read-only + ephemeral: the ONLY writes are agent sessions (normal usage). No
# tenants/keys/config created, nothing deleted. Safe to re-run anytime.
#
#   ./cloud-test.sh
#   PROJECT=... ZONE=... CP_VM=... LOCAL_PORT=... ./cloud-test.sh
#
# Requires: gcloud (authenticated, IAP access), curl, jq.
set -uo pipefail

PROJECT="${PROJECT:-mhi-exp-chang-sau-sheong}"
ZONE="${ZONE:-asia-southeast1-a}"
CP_VM="${CP_VM:-runtime-control-plane}"
CP_CONTAINER="${CP_CONTAINER:-runtime-cp-runtimed-1}"
LOCAL_PORT="${LOCAL_PORT:-18080}"        # avoid clashing with a local :8080
BASE="http://localhost:${LOCAL_PORT}"
EXPECTED_AGENTS="nutrition-go nutrition-openai hello-claude food-label-advisor"
SESSION_TIMEOUT="${SESSION_TIMEOUT:-90}" # per-agent stream wait (seconds)

fails=0
pass() { echo "PASS: $*"; }
fail() { echo "FAIL: $*" >&2; fails=$((fails+1)); }
skip() { echo "SKIP: $*"; }

need() { command -v "$1" >/dev/null 2>&1 || { echo "missing dependency: $1" >&2; exit 2; }; }
need gcloud; need curl; need jq

g() { gcloud --project "$PROJECT" "$@"; }
cp_ssh() { g compute ssh "$CP_VM" --zone "$ZONE" --tunnel-through-iap --command "$1" 2>/dev/null; }

# --- IAP tunnel to the control plane :8080 -------------------------------------
TUNNEL_PID=""
cleanup() {
  [ -n "$TUNNEL_PID" ] && kill "$TUNNEL_PID" 2>/dev/null || true
}
trap cleanup EXIT

echo "--- opening IAP tunnel $CP_VM:8080 -> localhost:$LOCAL_PORT ---"
g compute start-iap-tunnel "$CP_VM" 8080 \
  --local-host-port="localhost:${LOCAL_PORT}" --zone "$ZONE" >/tmp/cloud-test-tunnel.log 2>&1 &
TUNNEL_PID=$!

ok=0
for _ in $(seq 1 30); do
  if curl -sf "$BASE/healthz" >/dev/null 2>&1; then ok=1; break; fi
  # bail early if the tunnel process died
  kill -0 "$TUNNEL_PID" 2>/dev/null || { echo "tunnel process exited; see /tmp/cloud-test-tunnel.log" >&2; break; }
  sleep 1
done
[ "$ok" = 1 ] || { echo "FATAL: tunnel to $CP_VM:8080 never came up" >&2; cat /tmp/cloud-test-tunnel.log >&2; exit 2; }

# --- admin bootstrap token (superuser bearer), read from the container ---------
echo "--- fetching admin bootstrap token from $CP_CONTAINER ---"
TOK="$(cp_ssh "sudo docker exec $CP_CONTAINER printenv RUNTIME_ADMIN_BOOTSTRAP" | tr -d '\r\n[:space:]')"
[ -n "$TOK" ] || { echo "FATAL: could not read RUNTIME_ADMIN_BOOTSTRAP from $CP_CONTAINER" >&2; exit 2; }
AUTH=(-H "Authorization: Bearer $TOK")

echo "=============================================================="
echo " Cloud deployment test suite — $CP_VM ($PROJECT/$ZONE)"
echo "=============================================================="

# --- 1. Health & edge ---------------------------------------------------------
code=$(curl -sS -o /dev/null -w '%{http_code}' "$BASE/healthz")
[ "$code" = 200 ] && pass "healthz 200" || fail "healthz code=$code (want 200)"

code=$(curl -sS -o /dev/null -w '%{http_code}' "$BASE/metrics")
[ "$code" = 200 ] && pass "metrics endpoint 200 (prometheus scrape surface)" || fail "metrics code=$code (want 200)"

code=$(curl -sS -o /dev/null -w '%{http_code}' "$BASE/ui")
[ "$code" = 303 ] && pass "console /ui redirects unauthenticated (303, identity ON)" || fail "/ui code=$code (want 303)"

# --- 2. Identity / auth -------------------------------------------------------
code=$(curl -sS -o /dev/null -w '%{http_code}' "$BASE/agents")
[ "$code" = 401 ] && pass "unauth /agents 401 (identity gate active)" || fail "unauth /agents code=$code (want 401)"

code=$(curl -sS -o /dev/null -w '%{http_code}' "${AUTH[@]}" "$BASE/agents")
[ "$code" = 200 ] && pass "authenticated /agents 200 (bootstrap superuser)" || fail "auth /agents code=$code (want 200)"

# --- 3. Agent inventory -------------------------------------------------------
AGENTS_JSON="$(curl -sS "${AUTH[@]}" "$BASE/agents")"
n=$(echo "$AGENTS_JSON" | jq 'length' 2>/dev/null || echo 0)
[ "$n" = 4 ] && pass "4 agents registered" || fail "agent count=$n (want 4)"

for a in $EXPECTED_AGENTS; do
  h=$(echo "$AGENTS_JSON" | jq -r --arg a "$a" '.[] | select(.id==$a) | .healthy' 2>/dev/null)
  if [ "$h" = "true" ]; then pass "agent $a present & healthy"
  elif [ -z "$h" ] || [ "$h" = "null" ]; then fail "agent $a MISSING from registry"
  else fail "agent $a present but healthy=$h"; fi
done

# --- 4. End-to-end sessions (the core) ----------------------------------------
# For each agent: create a session, stream the verdict, assert completed + real
# output + tokens_total>0. Token metering now works for BOTH paths: native
# agentd (nutrition-go) and the external-SDK contract shim (OpenAI/Claude SDK
# agents), which persists the SDK's usage event onto the session so the control
# plane surfaces tokens_total like a native agent.
run_session() {  # $1=agent  $2=prompt(json-string)
  local a="$1" prompt="$2" sid text st toks
  sid=$(curl -sS "${AUTH[@]}" "$BASE/agents/$a/sessions" -d "{\"message\":$prompt}" | jq -r '.session_id' 2>/dev/null)
  if [ -z "$sid" ] || [ "$sid" = "null" ]; then fail "e2e $a: session create failed"; return; fi

  text=$(timeout "$SESSION_TIMEOUT" curl -sN "${AUTH[@]}" "$BASE/agents/$a/sessions/$sid/stream?since=0" \
    | sed -n 's/^data: //p' | jq -r 'select(.type=="text").text' 2>/dev/null | tr -d '\n')

  local status_json
  status_json=$(curl -sS "${AUTH[@]}" "$BASE/agents/$a/sessions/$sid")
  st=$(echo "$status_json" | jq -r '.status' 2>/dev/null)
  toks=$(echo "$status_json" | jq -r '.tokens_total // 0' 2>/dev/null)

  # Core success: the turn completed AND produced a real verdict.
  if [ "$st" != "completed" ] || [ -z "$text" ]; then
    fail "e2e $a: status=$st text_len=${#text} (session $sid)"
    return
  fi
  if [ "${toks:-0}" -gt 0 ] 2>/dev/null; then
    pass "e2e $a: completed, ${#text} chars, tokens=$toks"
  else
    fail "e2e $a: completed with output but tokens_total=$toks (session $sid)"
  fi
}

run_session nutrition-go       '"Is plain water a healthy drink? Answer in one sentence."'
run_session nutrition-openai   '"Is orange juice healthy? Answer in one sentence."'
run_session hello-claude       '"What makes a drink healthier? One sentence."'
run_session food-label-advisor '"Is 6g sugar per 100ml high for a drink? One sentence."'

# --- 5. Session lifecycle: unknown session id is a clean 404 ------------------
code=$(curl -sS -o /dev/null -w '%{http_code}' "${AUTH[@]}" "$BASE/agents/nutrition-go/sessions/ses-does-not-exist-000")
[ "$code" = 404 ] && pass "unknown session id 404 (clean not-found)" || fail "unknown session code=$code (want 404)"

# --- 6. Observability (run on the CP VM: dashboards listen on VM localhost) ----
echo "--- observability checks (on $CP_VM) ---"
OBS="$(cp_ssh '
  gcode() { curl -sS -o /dev/null -w "%{http_code}" "$1" 2>/dev/null; }
  echo "GRAFANA=$(gcode http://localhost:3000/api/health)"
  echo "JAEGER=$(gcode http://localhost:16686/)"
  echo "PROM_UP=$(curl -sS "http://localhost:9090/api/v1/targets" 2>/dev/null | grep -c "\"health\":\"up\"")"
  echo "JAEGER_SVCS=$(curl -sS "http://localhost:16686/api/services" 2>/dev/null | tr "," "\n" | grep -ciE "runtime|control")"
  echo "AM_HEALTH=$(gcode http://localhost:9093/-/healthy)"
  echo "PROM_RULES=$(curl -sS "http://localhost:9090/api/v1/rules" 2>/dev/null | grep -c "\"name\":\"runtime\"")"
  echo "AM_WIRED=$(curl -sS "http://localhost:9090/api/v1/alertmanagers" 2>/dev/null | grep -c "9093")"
')"
graf=$(echo "$OBS" | sed -n 's/^GRAFANA=//p')
jag=$(echo "$OBS" | sed -n 's/^JAEGER=//p')
promup=$(echo "$OBS" | sed -n 's/^PROM_UP=//p')
jsvcs=$(echo "$OBS" | sed -n 's/^JAEGER_SVCS=//p')
amhealth=$(echo "$OBS" | sed -n 's/^AM_HEALTH=//p')
promrules=$(echo "$OBS" | sed -n 's/^PROM_RULES=//p')
amwired=$(echo "$OBS" | sed -n 's/^AM_WIRED=//p')

[ "$graf" = 200 ] && pass "grafana healthy" || fail "grafana /api/health=$graf (want 200)"
{ [ "$jag" = 200 ] || [ "$jag" = 301 ]; } && pass "jaeger UI reachable ($jag)" || fail "jaeger UI code=$jag"
{ [ "${promup:-0}" -ge 1 ] 2>/dev/null; } && pass "prometheus has >=1 target up ($promup)" || fail "prometheus no targets up ($promup)"
if [ "${jsvcs:-0}" -ge 1 ] 2>/dev/null; then
  pass "jaeger has a runtime service (traces flowing, $jsvcs)"
else
  skip "jaeger service list empty (traces may not have flushed yet — depends on recent request volume)"
fi
[ "$amhealth" = 200 ] && pass "alertmanager healthy" || fail "alertmanager /-/healthy=$amhealth (want 200)"
{ [ "${promrules:-0}" -ge 1 ] 2>/dev/null; } && pass "prometheus loaded the runtime alert rules" || fail "prometheus runtime rule group not loaded"
{ [ "${amwired:-0}" -ge 1 ] 2>/dev/null; } && pass "prometheus wired to alertmanager (9093)" || fail "prometheus not wired to alertmanager"

# --- 7. Explicit SKIPs: capabilities in the codebase but NOT enabled here ------
skip "gateway upstreams — none registered in the cloud config (file_upstreams=0 db_upstreams=0)"
skip "sandbox / browser tools — no such upstreams in this deployment"
skip "eval / quota / policy admin — env flags unset on the control plane"

# --- summary ------------------------------------------------------------------
echo "=============================================================="
if [ "$fails" -eq 0 ]; then
  echo "ALL PASS — cloud deployment green"
else
  echo "$fails CHECK(S) FAILED"
  exit 1
fi
