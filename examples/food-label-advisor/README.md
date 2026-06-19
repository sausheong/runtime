# Food Label Advisor — Claude Agent SDK (hosted on runtime)

A **multi-product food label comparison** agent built on the
[Claude Agent SDK](https://docs.claude.com/en/api/agent-sdk/python) and hosted
on the runtime platform through the Python contract shim
(`../../contrib/shims/python`). Deployed at
[`https://runtime.sausheong.com`](https://runtime.sausheong.com) under the
`giantrobots` tenant.

Where the nutrition investigator examines **one product** in depth, this agent
takes **two or more label photos** in a single turn, normalises every value to a
common basis (per 100 g/ml and per serving), and ranks the products by a
user-specified goal.

## What it does

1. **Reads the user's goal** from the message — one of:
   `healthiest_overall`, `lowest_sugar`, `highest_protein`, `lowest_sodium`,
   `least_processed`, `best_for_children`, `allergen_safe`.
2. **Extracts each label** — calls `extract_label` once per image. The tool
   reads serving size, calories, sugar, fibre, protein, sodium, saturated fat,
   additives, allergens, and marketing claims; normalises raw values to both
   per-100g/ml and per-serving (scaling whichever basis the label uses to the
   other), and stores them in a turn-scoped `AdvisorHolder`.
3. **Compares products** — calls `compare_products` with all product names and
   the goal. The ranking engine scores and sorts by goal; ties broken by
   additive count.
4. **Submits the result** — calls `submit_advice`; the adapter reads the
   structured `ComparisonResult` from the holder and renders it.

The rendered output shows the goal, step-by-step reasoning, ranked list with
per-product score notes, winner explanation, and trade-offs to watch for.

## Project layout

```
food-label-advisor/
├── pyproject.toml      # deps: claude-agent-sdk + runtime-contract shim
├── agent.py            # data models, normalisation, ranking engine, system prompt
├── tools.py            # three in-process MCP tools + AdvisorHolder
├── adapter.py          # AgentAdapter: multi-image prompt, drives query()
├── sessions.py         # runtime session id → SDK session id map (SQLite)
├── serve.py            # entrypoint: load_dotenv + serve(FoodLabelAdvisorAdapter)
├── run_local.py        # local runner: reads food_label_images/, calls adapter directly
├── .env.example        # ANTHROPIC_API_KEY / BASE_URL / MODEL template
└── .gitignore
```

## Prerequisites

- **Python 3.12+** and [`uv`](https://docs.astral.sh/uv/).
- **LLM access**: `ANTHROPIC_API_KEY`, `ANTHROPIC_BASE_URL`, `ANTHROPIC_MODEL`
  (a vision-capable model — `claude-sonnet-4-6` or later).
- For local image testing: a `food_label_images/` directory of JPEG/PNG label
  photos at the repo root (`../../../food_label_images/` relative to this dir).
- For deploy: a VM the control plane can reach, Docker, and an admin service key
  for `runtime.sausheong.com`.

## Quick start — run locally (no HTTP server)

```bash
cp .env.example .env          # fill in ANTHROPIC_API_KEY / BASE_URL / MODEL
uv sync
uv run python run_local.py healthiest_overall
# goal may be omitted (defaults to healthiest_overall)
# other goals: lowest_sugar highest_protein lowest_sodium
#              least_processed best_for_children allergen_safe
```

`run_local.py` loads every image from `food_label_images/`, calls the adapter
directly (no HTTP server, no control plane), and prints the ranked comparison to
stdout.

## Quick start — run as an HTTP agent

```bash
cp .env.example .env
uv sync
RUNTIME_AGENT_ID=food-label-advisor \
RUNTIME_LISTEN_ADDR=127.0.0.1:8310 \
RUNTIME_SHIM_DB=./shim.db \
  uv run python serve.py
```

In a second shell — send two images and stream the result:

```bash
IMG1=$(base64 -i /path/to/label1.jpeg)
IMG2=$(base64 -i /path/to/label2.jpeg)

SID=$(curl -s 127.0.0.1:8310/sessions \
  -H 'Content-Type: application/json' \
  -d "{\"message\":\"Compare these. Goal: lowest_sugar.\",
       \"images\":[{\"data\":\"$IMG1\",\"mime\":\"image/jpeg\"},
                   {\"data\":\"$IMG2\",\"mime\":\"image/jpeg\"}]}" \
  | jq -r .session_id)

curl -sN "127.0.0.1:8310/sessions/$SID/stream?since=0"
curl -s  "127.0.0.1:8310/healthz"    # -> ok
```

The shim accepts images as an array (`images:[{data,mime},…]`) alongside the
legacy single-image `image_b64`/`image_mime` form.

## How it works

### Three in-process MCP tools

| Tool | Purpose |
|---|---|
| `extract_label` | Parse one label image: read raw nutrition values, serving size, additives, allergens, claims; normalise to both per-100 and per-serving; store in the turn-scoped `AdvisorHolder`. |
| `compare_products` | Rank all extracted products by goal. The ranking engine scores on the key metric(s) for that goal, breaks ties by additive count, and builds a `ComparisonResult`. |
| `submit_advice` | Typed-output channel — signals the adapter that the comparison is complete. The adapter reads `AdvisorHolder.result` and renders `ComparisonResult` to the SSE stream. |

### Ranking goals

| Goal | Primary metric | Tiebreak |
|---|---|---|
| `healthiest_overall` | Composite score: penalise sugar (×2), sodium (÷100), saturated fat (×3); reward protein (×2), fibre (×1.5), additives (×0.5) | Additive count ↑ |
| `lowest_sugar` | sugar g/100 ↑ | Additive count ↑ |
| `highest_protein` | protein g/100 ↓ | Additive count ↑ |
| `lowest_sodium` | sodium mg/100 ↑ | Additive count ↑ |
| `least_processed` | Additive count ↑ | Sugar g/100 ↑ |
| `best_for_children` | Sugar + sodium + flag penalty for tartrazine / sunset yellow / sodium benzoate | Additive count ↑ |
| `allergen_safe` | Allergen count ↑ | Healthiest-overall score ↑ |

### Multi-image turn

The adapter passes **all images in one user message** — a content array mixing
one text block and N image blocks — using the Claude SDK's streaming-input form.
The system prompt instructs the model to call `extract_label` once per image
before calling `compare_products`.

### Memory and sessions

Conversation memory uses the SDK's native `resume=` transcripts (JSONL under
`./claude-config`, pinned so resume survives restarts). `sessions.py` maps the
platform's runtime session id to the SDK's own session id so a follow-up turn
in the same runtime session resumes the right SDK conversation.

### Token telemetry

The adapter rolls `cache_read_input_tokens` into the `input` counter of the
`usage` telemetry event. Because the agent runs up to 30 internal turns (one per
tool call) with aggressive SDK caching, the raw `input_tokens` is nearly zero by
the final turn — rolling cache reads in makes the console's **Tokens In** widget
reflect the total tokens the model actually read.

## Deployment on runtime.sausheong.com

The agent runs as a third container on VM C (`runtime-agent-python`) at port
**8310**, alongside `hello-claude` (8080) and `nutrition-openai` (8302).

### Build and push (amd64)

Build from the **projects root** (parent of `runtime/`):

```bash
cd /path/to/projects
IMAGE="asia-southeast1-docker.pkg.dev/mhi-exp-chang-sau-sheong/runtime/food-label-advisor:latest"

docker build --platform linux/amd64 \
  -f runtime/deploy/gcp/agent-food-label/Dockerfile \
  -t "$IMAGE" .
gcloud auth configure-docker asia-southeast1-docker.pkg.dev --quiet
docker push "$IMAGE"
```

### Ship the deploy bundle

```bash
gcloud compute scp --recurse --tunnel-through-iap --zone asia-southeast1-a \
  runtime/deploy/gcp/agent-food-label runtime-agent-python:~/deploy/
gcloud compute scp --tunnel-through-iap --zone asia-southeast1-a \
  runtime/deploy/gcp/llm.env runtime-agent-python:~/deploy/llm.env
```

### Run on the VM

```bash
gcloud compute ssh runtime-agent-python --zone asia-southeast1-a --tunnel-through-iap
# on the VM:
cd ~/deploy/agent-food-label
cp .env.example .env     # set ANTHROPIC_API_KEY (URL+model come from ../llm.env)
sudo docker compose up -d
curl -s localhost:8310/healthz   # -> ok
```

### Register under the giantrobots tenant

```bash
# on the control plane VM (or via the bootstrap key):
curl -s -X POST http://localhost:8080/admin/agents \
  -H "Authorization: Bearer $RUNTIME_ADMIN_BOOTSTRAP" \
  -H 'Content-Type: application/json' \
  -d '{"id":"food-label-advisor","url":"http://10.10.0.4:8310",
       "tenant":"giantrobots","name":"Food Label Advisor",
       "model":"claude-sonnet-4-6-asia-southeast1"}'
```

### Invoke through the public edge

```bash
BASE=https://runtime.sausheong.com
KEY=<giantrobots-operator-key>

IMG1=$(base64 -i label1.jpeg)
IMG2=$(base64 -i label2.jpeg)

SID=$(python3 -c "
import base64, json, urllib.request
payload = json.dumps({
    'message': 'Compare these two. Goal: highest_protein.',
    'images': [
        {'data': open('label1.jpeg','rb').read().hex(), 'mime': 'image/jpeg'},
    ]
}).encode()
" )   # see run_local.py for the full pattern

# Simpler with curl + Python base64 helper:
SID=$(curl -s -H "Authorization: Bearer $KEY" \
  "$BASE/agents/food-label-advisor/sessions" \
  -H 'Content-Type: application/json' \
  -d "{\"message\":\"Compare. Goal: healthiest_overall.\",
       \"images\":[{\"data\":\"$IMG1\",\"mime\":\"image/jpeg\"},
                   {\"data\":\"$IMG2\",\"mime\":\"image/jpeg\"}]}" \
  | jq -r .session_id)

curl -sN -H "Authorization: Bearer $KEY" \
  "$BASE/agents/food-label-advisor/sessions/$SID/stream?since=0"
```

**Tenant isolation:** the `giantrobots` operator key is required; an `acme` key
returns `not found`. A `viewer` key returns `403` on `POST /sessions`.

## Limitations

- **No cross-run memory** — the agent doesn't persist past comparisons. Each
  session is independent; adding a product memory file (like `agent_memory.json`
  in the nutrition investigator) is the natural next step.
- **One multi-image turn** — all images are sent in a single turn. Very large
  images (e.g. 4 × 350 KB JPEG) increase token cost and latency significantly.
- **Labels must be legible** — the model reads values from the image; a blurry
  or partial label results in `null` fields and an `extraction_notes` caveat.
- **Subprocess per turn** — `query()` spawns the Claude Code CLI on every
  internal tool-call turn; a 4-image comparison with ~10 tool calls takes
  60–180 s end-to-end.

## References

- [`instruction.md`](../../instruction.md) — machine-readable build spec for Claude Agent SDK agents on runtime
- [`hello-claude.md`](../../hello-claude.md) — human tutorial: write + deploy a Claude SDK agent
- [`contrib/shims/python/README.md`](../../contrib/shims/python/README.md) — shim internals and `AgentAdapter` protocol
- [`deploy/gcp/agent-food-label/`](../../deploy/gcp/agent-food-label/) — Dockerfile, compose file, `.env.example`
- [`deploy/gcp/USING.md`](../../deploy/gcp/USING.md) — keys, roles, invoke patterns for the live deployment
