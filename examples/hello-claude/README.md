# Hello (Claude Agent SDK) — minimal agent on runtime

The smallest useful [Claude Agent SDK](https://docs.claude.com/en/api/agent-sdk/python)
agent: a plain conversational assistant — no tools, no domain logic — hosted on
the runtime platform through the Python contract shim
(`../../contrib/shims/python`).

It exists to show the shortest path from "a Claude Agent SDK script" to "an agent
hosted on runtime". Everything specific to this agent is two short files:
`adapter.py` (drives `query()` and yields contract events) and `serve.py` (the
entrypoint). Conversation memory across turns is the SDK's own transcripts via
`resume=`, tied to the runtime session id by `sessions.py`.

> **Scenario for this page:** the runtime **control plane is already running
> remotely** at `https://runtime.sausheong.com`, and you are deploying this agent
> on **its own VM**. The agent is a standalone HTTP service that uses **SQLite**
> for its sessions and has **no Postgres dependency** — Postgres is the control
> plane's concern, not the agent's.

## How the pieces connect

```
client ──HTTPS──► control plane (runtime.sausheong.com)
                        │  dials the agent over the private network
                        ▼
                  hello-claude on its VM  (:8080, ANTHROPIC_* + SQLite)
```

The control plane proxies **inbound** to the agent: it health-checks and forwards
requests to the `url:` you register. So the agent's VM must be reachable **from
the control plane** (private network + firewall), and the registered `url:` is
the agent VM's address — never `localhost`. The agent never dials out to the
control plane.

## Prerequisites

- **A VM for the agent** that the control plane can reach on the agent's port
  (here `8080`), with Docker installed. In the reference GCP deployment this is a
  private VM on the same VPC, with a firewall rule allowing the control plane to
  reach `8080`.
- **The control plane already running** at `https://runtime.sausheong.com`, and an
  **admin service key** for its tenant (to register the agent and to invoke it).
- **LLM access**: `ANTHROPIC_API_KEY` + `ANTHROPIC_BASE_URL` + `ANTHROPIC_MODEL`
  (a LiteLLM proxy, or api.anthropic.com).
- Docker with buildx **on your build host** (to cross-build an amd64 image if the
  VM is x86-64 and you are on Apple Silicon).

## Deploy on a VM

### 1. Build the agent image (amd64) and push it

The agent ships as a self-contained image (the Claude SDK bundles its CLI in the
platform wheel, so an amd64 `uv sync` needs no node). Build from the **projects
root** (parent of `runtime/`), then push to a registry the VM can pull from:

```bash
cd /path/to/projects                     # contains runtime/ and harness/
REG=asia-southeast1-docker.pkg.dev; PROJECT=<your-project>
docker build --platform linux/amd64 \
  -f runtime/deploy/gcp/agent-claude/Dockerfile \
  -t "$REG/$PROJECT/runtime/hello-claude:latest" .
docker push "$REG/$PROJECT/runtime/hello-claude:latest"
```

### 2. Run the container on the agent VM

Copy the bundle in [`../../deploy/gcp/agent-claude/`](../../deploy/gcp/agent-claude)
to the VM, set the LLM creds, and bring it up:

```bash
cp .env.example .env          # ANTHROPIC_API_KEY / ANTHROPIC_BASE_URL / ANTHROPIC_MODEL
sudo docker compose up -d
curl -s localhost:8080/healthz                       # -> ok (on the VM)
```

The compose file binds `0.0.0.0:8080` (not loopback — uvicorn needs the host
spelled out to be reachable through Docker's port publish) and persists SQLite on
a named volume. It uses a distinct compose project name, so the agent can run as
a **second container alongside another shim agent on the same VM**.

### 3. Register the agent on the remote control plane

Add an **attach** entry (the control plane proxies to it; it does not spawn it) to
the control plane's `runtime.remote.yaml`, using the agent VM's **private
address** and the tenant your console users belong to:

```yaml
agents:
  - id: hello-claude
    tenant: acme                       # MUST match your console users' tenant
    name: Hello (Claude Agent SDK)
    model: claude-sonnet-4-6           # display only; the agent's ANTHROPIC_MODEL wins
    url: http://10.10.0.4:8080         # the agent VM's private IP:port
```

Restart only `runtimed` on the control-plane VM (the agents are untouched):

```bash
cd ~/deploy/control-plane && sudo docker compose up -d --force-recreate runtimed
```

It logs `monitoring remote agent  agent=hello-claude  url=http://10.10.0.4:8080`,
and the agent appears in `GET /agents` and the console.

> The full GCP walkthrough (VPC, firewall, the VMs) is in
> [`../../deploy/gcp/README.md`](../../deploy/gcp/README.md); the SDK-agnostic
> deploy path is in [Deploying SDK agents](../../docs/deploying-sdk-agents.md).

## Exercise it through the remote control plane

`runtimectl` targets `RUNTIME_CTL_URL` and sends `RUNTIME_TOKEN` as a bearer, so
it works against the gated public control plane:

```bash
export RUNTIME_CTL_URL=https://runtime.sausheong.com
export RUNTIME_TOKEN=<acme admin service key>

make conformance              # contract acceptance gate (same suite as Go agents)
make demo                     # say hello; streams the reply
make sessions                 # list this agent's sessions
```

Or with plain `curl` against the public edge:

```bash
BASE=https://runtime.sausheong.com; KEY=<service key>
SID=$(curl -s -H "Authorization: Bearer $KEY" "$BASE/agents/hello-claude/sessions" \
  -d '{"message":"Say hi in one sentence."}' | jq -r .session_id)
curl -sN -H "Authorization: Bearer $KEY" "$BASE/agents/hello-claude/sessions/$SID/stream?since=0"
```

## Local dev / testing (no VM)

To iterate on the agent without a VM, run it standalone and either point a local
control plane at it, or just exercise the contract directly:

```bash
cp .env.example .env
make serve                    # runs JUST this agent on 127.0.0.1:8310 (serve.py + SQLite, no Postgres)
uv run pytest                 # hermetic: fakes query(), no key or network
```

`make run` is an all-in-one convenience that starts a *local* control plane which
spawns this agent for you. That control plane needs Postgres (for native Go
agents' DBOS durability — `make -C ../.. pg-up`, or Postgres.app); the agent
itself still uses SQLite.

## How it works

- **Per-turn `query()` + `resume=`.** Each contract turn drives a fresh `query()`
  call. Conversation memory is the SDK's own JSONL transcripts (under
  `./claude-config`, pinned via `CLAUDE_CONFIG_DIR` so resume survives restarts).
  A one-table runtime→SDK session-id map (`sessions.py`) in the agent's SQLite
  ties the platform session id to the SDK's, so a follow-up message in the same
  runtime session resumes the right SDK conversation.
- **No tools.** `tools=[]` disables all the CLI's built-ins (a deny-list backs it
  up); this is a pure chat agent.
- **One event per turn.** The adapter collects the assistant's text and yields a
  single `text` contract event (or one `error` event); the shim appends the
  terminal `done`/`error`.

## Authentication note

The contract shim enforces **no bearer** — it trusts that only the control plane
reaches it. So the agent's port must be protected at the network layer (a
firewall that admits only the control plane), and platform authentication is
enforced at the **control-plane edge** (the service key above), not at the agent.
