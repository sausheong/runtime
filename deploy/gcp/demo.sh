#!/usr/bin/env bash
# Live demo of the distributed runtime deployment. Run it with the IAP tunnel to
# the control plane already open (see USING.md §1):
#
#   gcloud compute start-iap-tunnel runtime-control-plane 8080 \
#     --local-host-port=localhost:8080 --zone asia-southeast1-a \
#     --project mhi-exp-chang-sau-sheong
#
# Then, in another terminal:   ./demo.sh
#
# It pauses between beats (press Enter) so you can narrate. Streams each agent's
# verdict as clean text instead of raw SSE frames.
set -euo pipefail
BASE="${BASE:-http://localhost:8080}"
KEY="${KEY:?set KEY to an operator service key}"

pause() { read -rp $'\n\033[2m(press Enter)\033[0m '; }
say()   { printf '\n\033[1;36m== %s ==\033[0m\n' "$1"; }

run_label() {  # $1=agent  $2=message
  local agent="$1" msg="$2" sid
  printf '\033[2m> creating session on %s...\033[0m\n' "$agent"
  sid=$(curl -s -H "Authorization: Bearer $KEY" \
    "$BASE/agents/$agent/sessions" -d "{\"message\":$msg}" | jq -r .session_id)
  printf '\033[2m> session %s — streaming verdict:\033[0m\n\n' "$sid"
  curl -sN -H "Authorization: Bearer $KEY" \
    "$BASE/agents/$agent/sessions/$sid/stream?since=0" \
    | sed -n 's/^data: //p' \
    | jq -r 'select(.type=="text").text'
}

clear
say "1. The deployment: a control plane + four agents on three machines"
echo "Four agents, in different languages/SDKs, attached to one control plane:"
curl -s -H "Authorization: Bearer $KEY" "$BASE/agents" \
  | jq -r '.[] | "  - \(.id)  (\(.name))  healthy=\(.healthy)"'
echo
echo "nutrition-go     -> native Go agent       (VM B + Postgres)"
echo "nutrition-openai -> OpenAI Agents SDK      (VM C + SQLite)"
echo "hello-claude       -> Claude Agent SDK      (VM C)"
echo "food-label-advisor -> Claude image agent    (VM C)"
pause

say "2. Native Go agent investigates a nutrition label (text)"
run_label nutrition-go '"Investigate this label (text): Product: Milo UHT. Ingredients: water, skimmed milk, sugar, cocoa, malt extract, soy lecithin (E322). Sugar 6g/100ml, saturated fat 1.5g/100ml. It is a beverage."'
pause

say "3. The Python agent investigates a DIFFERENT label — same control plane"
run_label nutrition-openai '"Investigate this label (text): Product: Marigold HL Milk. Ingredients: milk, vitamins. Sugar 4.5g/100ml, saturated fat 0.3g/100ml. It is a beverage."'
pause

say "4. A third SDK agent on the same control plane"
run_label hello-claude '"In one sentence, what makes a drink a healthier choice?"'
pause

say "5. Where to look next"
cat <<EOF
  Console UI : $BASE/ui            (agents + sessions, live)
  Jaeger     : :16686 (own tunnel) (one request tracing across VMs)
  Grafana    : :3000  (own tunnel) (request rates, agent health)

Same two-step API every time:
  POST /agents/<id>/sessions            -> {session_id}
  GET  /agents/<id>/sessions/<sid>/stream?since=0
EOF
