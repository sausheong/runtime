# Deploying foreign-SDK agents (OpenAI Agents SDK, Claude Agent SDK)

Runtime natively hosts **Go** agents (link the `agentruntime` SDK and it binds
the agent contract for you). But the platform only cares about *one* interface —
the **agent contract** (six HTTP/SSE endpoints). Any process that speaks it is
supervised, routed, health-gated, and restarted like a native agent.

This guide shows how to host an agent written with a **Python** framework — the
[OpenAI Agents SDK](https://openai.github.io/openai-agents-python/) or the
[Claude Agent SDK](https://docs.claude.com/en/api/agent-sdk/python) — using the
reusable Python contract library, then ship it to production on GCP.

Two complete worked agents ship in the repo and are the templates to copy:

| SDK | Directory |
|---|---|
| OpenAI Agents SDK | [`examples/nutrition-label-openai/`](../examples/nutrition-label-openai) |
| Claude Agent SDK | [`examples/nutrition-label-claude/`](../examples/nutrition-label-claude) |

The deployment mechanism is **identical** for both; only the adapter differs.

---

## 1. The agent contract (what runtime needs)

Runtime supervises any process that serves these endpoints over HTTP:

| Endpoint | Purpose |
|---|---|
| `GET /healthz` | liveness (health-gating) |
| `GET /meta` | agent id / model surfaced to the control plane |
| `POST /sessions` | start a run; returns a `session_id` |
| `GET /sessions/{id}/stream?since=N` | SSE event stream; replay from seq N |
| `GET /sessions/{id}` | session status |
| `GET /sessions` | list sessions |
| `POST /sessions/{id}/messages` | follow-up turn (shim extension) |

You do **not** implement these yourself. The
[`runtime_contract`](../contrib/shims/python) library
(`contrib/shims/python`) is a framework-agnostic FastAPI app that serves all of
them — persistence, SSE fan-out, `?since=N` replay, and the terminal
`done`/`error` event. You write exactly two things:

1. **An adapter** — one class implementing the `AgentAdapter` protocol that
   drives your SDK and yields `ContractEvent`s.
2. **An entrypoint** — a `serve.py` that builds the adapter and calls `serve()`.

Runtime launches your entrypoint through the config's `command:`/`workdir:`
fields, injecting operator parameters and passing your SDK's own credentials
through:

| Env var | Meaning | Source |
|---|---|---|
| `RUNTIME_LISTEN_ADDR` | `host:port` to bind | injected by `runtimed` |
| `RUNTIME_AGENT_ID` | agent id on `/meta` | injected by `runtimed` |
| `RUNTIME_SHIM_DB` | SQLite path for the durable session store | optional; defaults under `workdir` |
| `OPENAI_*` / `ANTHROPIC_*` | your model credentials | inherited from `runtimed`'s env |

---

## 2. The adapter + entrypoint

### Adapter — one file

The adapter drives your SDK for a single invocation and yields events. It never
raises: failures become an `error` event, and the library appends the terminal
lifecycle event for you.

```python
from typing import AsyncIterator, Sequence
from runtime_contract.events import ContractEvent, Image

class MyAdapter:
    def __init__(self, db_path: str):
        # Constructed once at startup. Read your SDK key here so a missing
        # credential fails fast (not on the first request).
        self._db = db_path
        self._agent = build_my_agent()

    async def run(
        self,
        session_id: str,
        message: str,
        images: Sequence[Image],
        history: Sequence[ContractEvent],
    ) -> AsyncIterator[ContractEvent]:
        # Drive your framework; yield text as it streams. Do NOT emit the
        # terminal done/error — the library does that.
        yield ContractEvent(type="text", text="...")
```

Real adapters to copy:
[`nutrition-label-openai/adapter.py`](../examples/nutrition-label-openai/adapter.py)
(OpenAI Agents SDK — `Runner` + `SQLiteSession` + typed `output_type`) and
[`nutrition-label-claude/adapter.py`](../examples/nutrition-label-claude/adapter.py)
(Claude Agent SDK — `query()` with `resume=`, MCP tools, built-ins disabled).

**Image input** arrives as `images: Sequence[Image]` (`.mime` + `.data` bytes);
each adapter base64-encodes it into the shape its SDK expects.
**Conversation memory** is the adapter's concern — both examples key an
SDK-native session store on the runtime `session_id` so a follow-up turn sees
prior turns. `history` (the contract event log) is available but the examples
don't replay from it.

### Entrypoint — `serve.py`

```python
from runtime_contract import serve
from adapter import MyAdapter

serve(MyAdapter)   # reads RUNTIME_* from env; builds Store + app + uvicorn
```

`serve()` accepts a ready adapter instance **or** a factory
`make(db_path) -> AgentAdapter`. A class whose constructor takes `db_path` is
itself a factory, so `serve(MyAdapter)` works directly and lets the adapter
share `RUNTIME_SHIM_DB` with the contract store. See
[`nutrition-label-openai/serve.py`](../examples/nutrition-label-openai/serve.py).

### The config entry

Point a runtime config at the entrypoint with `command:`/`workdir:`:

```yaml
agents:
  - id: my-sdk-agent
    name: My SDK Agent
    model: openai/gpt-5.4            # display only; OPENAI_MODEL is authoritative
    listen_addr: 127.0.0.1:8302
    workdir: ./examples/my-sdk-agent
    command: ["uv", "run", "python", "serve.py"]
```

When an agent entry sets `command`, `runtimed`'s supervisor execs that argv in
`workdir` (instead of the bundled `agentd`), injecting the `RUNTIME_*` vars and
inheriting the parent environment so your `OPENAI_*` / `ANTHROPIC_*` flow
through. Credentials themselves go in a local `.env` next to the agent
(gitignored) — see each example's `.env.example`.

---

## 3. Run and gate it locally

Each example has a `Makefile` that wires this up:

```bash
cd examples/nutrition-label-openai     # or nutrition-label-claude
cp .env.example .env                   # fill in your proxy key + model
make run                               # builds binaries, uv sync, runs the control plane
```

In a second shell, run the **same conformance suite that gates Go agents** —
this is your acceptance gate before shipping:

```bash
make conformance                       # runtimectl conformance --agent <id>
make demo-image IMAGE=milo.jpeg        # base64 a photo → POST → stream the verdict
make sessions                          # list this agent's sessions
```

`make conformance` runs `runtimectl conformance --agent <id>` against the live
agent and verifies every contract endpoint behaves correctly. **If conformance
fails, fix the adapter before deploying** — the control plane assumes the
contract holds.

---

## 4. Ship to production (GCP, as a remote agent)

In the GCP distributed deployment, an SDK agent runs on its **own VM** and the
control plane attaches to it over the VPC as a **remote agent** — `runtimed`
health-checks and proxies to it, but never spawns it. The OpenAI example is
deployed exactly this way as instance **C**; the bundle is
[`deploy/gcp/agent-python/`](../deploy/gcp/agent-python).

### a. Containerize

The agent ships as a self-contained image (SQLite for durable sessions, no
external DB). Build for **amd64** from the **projects root** (parent of
`runtime/`), since the GCP VMs are x86-64:

```bash
cd /path/to/projects                   # contains runtime/ and harness/
docker build --platform linux/amd64 \
  -f runtime/deploy/gcp/agent-python/Dockerfile \
  -t nutrition-openai:latest .
```

The Dockerfile (see
[`deploy/gcp/agent-python/Dockerfile`](../deploy/gcp/agent-python/Dockerfile))
copies both `contrib/shims/python` and the example dir so the
`runtime-contract = { path = "../../contrib/shims/python" }` path dependency
resolves, then `uv sync --no-dev`. To containerize your own agent, copy this
Dockerfile and swap the example path.

### b. Run it on its VM

`deploy/gcp/agent-python/docker-compose.yml` runs the image with the env it
needs. Two things matter:

- **Bind `0.0.0.0`, not loopback.** Set `RUNTIME_LISTEN_ADDR: "0.0.0.0:8302"`.
  With a bare `":8302"` the shim's host parsing yields an empty host and uvicorn
  falls back to `127.0.0.1`, unreachable through Docker's port publish. (The Go
  `agentd` binds all interfaces on `:port`; uvicorn needs the host spelled out.)
- **Inject the model credentials** (`OPENAI_*` / `ANTHROPIC_*`) and a persistent
  `RUNTIME_SHIM_DB` on a volume.

```bash
cd ~/deploy/agent-python
cp .env.example .env                   # OPENAI_API_KEY / OPENAI_BASE_URL / OPENAI_MODEL
sudo docker compose up -d
```

### c. Attach it from the control plane

Add the agent to the control plane's registry as a **remote** entry — only
`id/name/model/tenant/url` (and `auth_token` if the agent enforces a bearer).
Spawn-time fields (`kind/command/workdir`) are rejected on a remote entry. From
[`deploy/gcp/control-plane/runtime.remote.yaml`](../deploy/gcp/control-plane/runtime.remote.yaml):

```yaml
agents:
  - id: nutrition-openai
    name: SG Nutrition Investigator (OpenAI SDK)
    model: openai/gpt-5.4              # display only; C's OPENAI_MODEL is authoritative
    tenant: acme                       # MUST match your console users' tenant
    url: http://10.10.0.AGENT_PY_IP:8302
```

> **`tenant` matters.** With identity ON, the console only shows agents in the
> logged-in admin's tenant. Set it to the tenant your console users belong to
> (omitting it defaults to `default`, which hides the agent from an `acme`
> admin).

Restart `runtimed` (control plane only — the agents are untouched):

```bash
cd ~/deploy/control-plane && sudo docker compose up -d runtimed
```

`runtimed` logs `monitoring remote agent` and the agent appears in
`GET /agents`, the console, and is invokable through the gated edge exactly like
a Go agent. The full GCP walkthrough (VPC, firewall, the three VMs) is in
[`deploy/gcp/README.md`](../deploy/gcp/README.md).

### Authentication note

The Python contract shim does **not** enforce a bearer — it trusts the loopback
proxy and expects to be reached only by `runtimed`. In the GCP deployment, the
agent's port is protected by the VPC firewall (internal-only); the registry
entry omits `auth_token` so the config doesn't imply protection the shim doesn't
provide. Platform authentication is enforced at the **control-plane edge**, not
at each agent — the same model as Go `agentd` agents.

---

## 5. Durability caveats

These Python-hosted agents persist sessions across restarts: prior sessions
remain **listable** (`GET /sessions`) and their events **replayable**
(`GET /sessions/{id}/stream?since=N`), stored in `shim.db`. Conversation memory
continues across restarts when the adapter keys its own per-session store on the
runtime session id (both examples do).

**A run killed mid-execution is not resumed.** There is no checkpoint/replay of
a partial run, unlike Go/harness agents' DBOS-backed per-turn durability — a
process killed during a run loses that in-flight turn (completed sessions and
events remain intact).

---

## Summary — porting your own SDK agent

1. Write `adapter.py` — one `AgentAdapter` class driving your SDK.
2. Write `serve.py` — `serve(MyAdapter)`.
3. Add a config entry with `command:`/`workdir:` (local) — or build the amd64
   image and attach it as a remote agent (GCP).
4. Gate with `make conformance` before shipping.

Adding a new framework is one new file: the library handles everything else.
Copy whichever example SDK is closest and swap the adapter.
