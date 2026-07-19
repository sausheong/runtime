# Build a "hello-claude" agent with the Claude Agent SDK and deploy it to runtime.sausheong.com

A step-by-step guide to writing the smallest useful [Claude Agent SDK](https://docs.claude.com/en/api/agent-sdk/python)
agent — a plain conversational assistant, no tools — and hosting it on the
runtime platform at `https://runtime.sausheong.com`.

The agent itself is two short files. Everything else (sessions, the HTTP/SSE
contract, supervision) is provided by the platform's **Python contract shim**
(`contrib/shims/python`), so you write an adapter and an entrypoint and nothing
more.

```
client ──HTTPS──► control plane (runtime.sausheong.com)
                        │  dials the agent over the private VPC
                        ▼
                  hello-claude on its VM  (:8080, ANTHROPIC_* + SQLite)
```

The control plane proxies **inbound** to your agent: it health-checks and
forwards requests to a `url:` you register. The agent never dials out to the
control plane. Authentication is enforced at the control-plane edge (a service
key); the agent's own port is protected by the network (a firewall that admits
only the control plane).

## Who writes what

The platform ships a **Python contract shim** — the installed package
`runtime_contract` (`contrib/shims/python`). You depend on it; you never copy or
edit it. It owns every piece of hosting plumbing: the HTTP/SSE contract, durable
sessions, `/healthz`, `/metrics`, the supervised process lifecycle. **You
implement exactly one method** — an adapter's `run()` — and wire it up.

### Provided by the shim (you don't write this)

| Shim symbol | What it does for you |
|---|---|
| `serve()` | Reads the operator-injected env (`RUNTIME_LISTEN_ADDR`, `RUNTIME_AGENT_ID`, `RUNTIME_SHIM_DB`), builds the app + store + metrics, runs uvicorn. You call it with one line. |
| `create_app()` | The full contract: `POST /sessions`, `/sessions/{id}/stream`, `/messages`, `/healthz`, `/meta`, `/metrics`. Frames your events, persists them, fans them out. |
| `Store` | Durable session/event storage in SQLite. |
| `ContractEvent`, `Image` | The event vocabulary you `yield` (`text`, `error`, plus telemetry-only `usage`/`tool_call`). |
| `AgentAdapter` | The Protocol your class satisfies: one `async def run(...)`. |
| `Metrics` | Prometheus counters (tokens, tool calls, turns, duration). Wired up automatically. |

Three rules fall out of this design, and the guide below relies on them:
- You **never read the `RUNTIME_*` env** — the shim injects and consumes it.
- You **never touch HTTP/SSE, persistence, or health** — that's `create_app()`.
- You **never emit the terminal `done`/`error` event** and your `run()` **must
  never raise** — the shim appends the lifecycle event based on whether `run()`
  finished cleanly or yielded an `error` event.

### Written by you

Four files. Two carry real logic, two are essentially config/boilerplate.

| File | Role | How much is "yours" |
|---|---|---|
| `adapter.py` | **The agent.** Implements `run()`: prompt, tool policy, the per-turn loop driving the Claude SDK. | ~95% of your design lives here. A different agent = a different `adapter.py`. |
| `sessions.py` | A tiny SQLite map: runtime session id → SDK session id, so follow-ups `resume=`. | Mechanical. Omit it entirely for a stateless agent. |
| `serve.py` | Entrypoint: load `.env`, hand the adapter **class** to `serve()`. | ~1 real line; unchanged between agents except the class name. |
| `pyproject.toml` + `.env.example` | Deps (`claude-agent-sdk` + the shim) and the three `ANTHROPIC_*` creds. | Config, not code. |

> **One sentence:** you write `adapter.py` (the agent) and a tiny `sessions.py`
> memory map; everything else — HTTP, SSE streaming, durable sessions,
> `/healthz`, `/metrics`, process lifecycle — is the shim, and `serve.py` is one
> line that hands your adapter class to it.

The sections below (§2-§6) walk each written file in detail, and call out exactly
which lines are design decisions you own versus required scaffolding.

## Prerequisites

- A clone of the `runtime` repo (the shim lives in it), with the projects laid
  out as `projects/runtime/` and `projects/harness/` side by side.
- **Python 3.12+** and [`uv`](https://docs.astral.sh/uv/) for local dev.
- **Docker with buildx** on your build host (to cross-build an amd64 image; the
  GCP VMs are x86-64, even if you build on Apple Silicon).
- **LLM access**: `ANTHROPIC_API_KEY` + `ANTHROPIC_BASE_URL` + `ANTHROPIC_MODEL`
  (a LiteLLM proxy, or `api.anthropic.com` directly).
- For deploy: an **agent VM** the control plane can reach on port 8080, and an
  **admin service key** for your tenant on `runtime.sausheong.com` (to register
  and invoke the agent — see Part B, step 5).

# Part A — Write the agent

## 1. Project layout

```
hello-claude/
├── pyproject.toml      # deps: claude-agent-sdk + the contract shim
├── adapter.py          # the agent: drives query(), yields contract events
├── sessions.py         # maps runtime session id -> SDK session id
├── serve.py            # entrypoint runtimed/Docker runs
└── .env.example        # ANTHROPIC_* creds template
```

## 2. `pyproject.toml`

The only agent-specific dependency is `claude-agent-sdk`; `runtime-contract` is
the shim, pulled from the repo by relative path.

```toml
[project]
name = "hello-claude"
version = "0.1.0"
description = "A minimal Claude Agent SDK conversational agent, hosted on runtime"
requires-python = ">=3.12"
dependencies = [
    "claude-agent-sdk>=0.2.99",
    "python-dotenv>=1.2.2",
    "fastapi>=0.115",
    "uvicorn>=0.30",
    "runtime-contract",
]

[dependency-groups]
dev = ["pytest>=8", "pytest-asyncio>=0.24"]

[tool.uv.sources]
runtime-contract = { path = "../../contrib/shims/python", editable = true }

[tool.pytest.ini_options]
testpaths = ["tests"]
asyncio_mode = "auto"
```

> The Claude Agent SDK bundles its CLI inside the platform wheel, so a Linux
> `uv sync` pulls the right binary automatically — no Node, no manual install.

## 3. `sessions.py` — tie the runtime session to the SDK session

**Yours, but mechanical.** The SDK owns conversation memory (JSONL transcripts it
can `resume=`); the platform owns the runtime session id. They are two different
id spaces. This one-table map ties them so turn N+1 can find and resume the SDK
conversation turn N created. It lives in the same SQLite file the shim uses
(`RUNTIME_SHIM_DB`) but in its own `sdk_sessions` table — no interference with the
shim's storage. Three methods: ensure-table (`__init__`), `lookup`, `store`
(an upsert). A *stateless* agent wouldn't need this file at all.

```python
"""Runtime session_id -> Claude SDK session_id map (SQLite, one table)."""
from __future__ import annotations

import sqlite3

class SessionMap:
    def __init__(self, db_path: str):
        self._db = db_path
        with self._conn() as c:
            c.execute(
                "CREATE TABLE IF NOT EXISTS sdk_sessions ("
                "runtime_id TEXT PRIMARY KEY, sdk_id TEXT NOT NULL)"
            )

    def _conn(self) -> sqlite3.Connection:
        return sqlite3.connect(self._db)

    def lookup(self, runtime_id: str) -> str | None:
        with self._conn() as c:
            row = c.execute(
                "SELECT sdk_id FROM sdk_sessions WHERE runtime_id = ?", (runtime_id,)
            ).fetchone()
        return row[0] if row else None

    def store(self, runtime_id: str, sdk_id: str) -> None:
        with self._conn() as c:
            c.execute(
                "INSERT INTO sdk_sessions (runtime_id, sdk_id) VALUES (?, ?) "
                "ON CONFLICT(runtime_id) DO UPDATE SET sdk_id = excluded.sdk_id",
                (runtime_id, sdk_id),
            )
```

## 4. `adapter.py` — the agent

**This is the only file with real work — ~95% of your design.** The shim calls
`run()` once per turn; you drive the Claude Agent SDK's `query()` and `yield`
contract events. The contract is simple: yield a `text` event with the reply, or
one `error` event. Never raise — the shim appends the terminal `done`/`error`
lifecycle event itself.

Conversation memory across turns is the SDK's own transcripts via `resume=`,
looked up through `SessionMap`.

```python
"""Adapter: a minimal Claude Agent SDK assistant -> runtime contract."""
from __future__ import annotations

import os
from pathlib import Path
from typing import AsyncIterator, Sequence

from claude_agent_sdk import (
    query,
    ClaudeAgentOptions,
    AssistantMessage,
    TextBlock,
    ResultMessage,
)

from runtime_contract.events import ContractEvent, Image
from sessions import SessionMap

HERE = Path(__file__).resolve().parent

SYSTEM_PROMPT = "You are a friendly, concise assistant. Answer in a sentence or two."

# Network-facing chat agent: disable all the CLI's built-in tools. tools=[] is the
# primary control; the deny-list is belt-and-braces.
BUILTINS_OFF = [
    "Bash", "Read", "Write", "Edit", "Glob", "Grep",
    "WebFetch", "WebSearch", "NotebookEdit", "TodoWrite", "Task",
]

class HelloClaudeAdapter:
    """AgentAdapter backed by the Claude Agent SDK (no tools)."""

    def __init__(self, db_path: str):
        self._sessions = SessionMap(db_path)
        # resume= is keyed by (CLAUDE_CONFIG_DIR, cwd); pin both so memory
        # survives restarts. Keep the transcript home next to the shim db.
        self._config_dir = str(Path(db_path).resolve().parent / "claude-config")
        self._model = os.environ["ANTHROPIC_MODEL"]  # fail fast at startup

    def _options(self, resume: str | None) -> ClaudeAgentOptions:
        return ClaudeAgentOptions(
            model=self._model,
            resume=resume,
            system_prompt=SYSTEM_PROMPT,
            tools=[],                       # disable ALL built-ins
            disallowed_tools=list(BUILTINS_OFF),
            permission_mode="dontAsk",
            cwd=str(HERE),
            env={
                "CLAUDE_CONFIG_DIR": self._config_dir,
                "CLAUDE_CODE_DISABLE_AUTO_MEMORY": "1",
            },
            setting_sources=[],
            max_turns=8,
        )

    async def run(
        self,
        session_id: str,
        message: str,
        images: Sequence[Image],
        history: Sequence[ContractEvent],
    ) -> AsyncIterator[ContractEvent]:
        # history/images unused: the SDK's own transcripts own memory; text-only.
        try:
            resume = self._sessions.lookup(session_id)
            opts = self._options(resume)
            text_parts: list[str] = []
            result = None
            async for msg in query(prompt=message or "Hello!", options=opts):
                if isinstance(msg, AssistantMessage):
                    for block in msg.content:
                        if isinstance(block, TextBlock):
                            text_parts.append(block.text)
                elif isinstance(msg, ResultMessage):
                    result = msg
            if result is not None and result.session_id:
                self._sessions.store(session_id, result.session_id)
            if result is not None and result.is_error:
                yield ContractEvent(
                    type="error",
                    error=f"agent run failed ({result.subtype}): {result.result or ''}",
                )
                return
            text = "".join(text_parts) or (result.result if result and result.result else "")
            yield ContractEvent(type="text", text=text or "(no output)")
        except Exception as e:  # never raise out of run()
            yield ContractEvent(type="error", error=str(e))
```

**Lines you own (design decisions):**
- **`SYSTEM_PROMPT`** — the agent's persona. The first thing to change.
- **`tools=[]`** — makes it a pure chat agent. This is the *primary* control that
  disables every built-in tool (Bash/Read/Write/etc.); `BUILTINS_OFF` is a
  belt-and-braces deny-list backing it. A tool-using agent changes this line.
- **`ClaudeAgentOptions`** in `_options()` — model, `system_prompt`,
  `permission_mode`, `max_turns`: the SDK behavior knobs.
- **The `run()` body** — your per-turn logic: drive `query()`, collect the
  assistant text, decide what to `yield`.

**Lines that are required scaffolding (keep them as-is):**
- **`run()`'s signature** must match the `AgentAdapter` protocol exactly, and it
  must be an **async generator** (`yield`, not `return`, the events).
- **`__init__(self, db_path)`** — the shim calls your class as a factory with the
  resolved `RUNTIME_SHIM_DB` path. Pinning `CLAUDE_CONFIG_DIR` + `cwd` is required
  for `resume=` to survive restarts (it's keyed by that pair), not optional taste.
- **The `try/except` → `yield error`** — `run()` must **never raise**; the shim
  turns a clean finish into `done` and a yielded `error` into `error`.
- **`_usage_event()`** is *optional* token telemetry — delete it and you simply
  get sparser metrics (turn count + duration are still recorded by the shim). It
  is best-effort and returns `None` on any shape mismatch so it can never break a
  turn. (Omitted from the snippet above for brevity; see `examples/hello-claude/adapter.py`.)

## 5. `serve.py` — the entrypoint

The shim's `serve()` reads the operator-injected `RUNTIME_*` env (listen address,
agent id, SQLite path) and runs the HTTP/SSE contract for your adapter. Passing
the adapter **class** (not an instance) lets the shim share `RUNTIME_SHIM_DB`
between its own store and your `SessionMap`.

```python
"""Entry point: serve the minimal Claude-SDK assistant over the contract."""
from __future__ import annotations

import os
from dotenv import load_dotenv

load_dotenv()  # .env: ANTHROPIC_API_KEY / ANTHROPIC_BASE_URL / ANTHROPIC_MODEL

from runtime_contract import serve          # noqa: E402
from adapter import HelloClaudeAdapter      # noqa: E402

def main() -> None:
    print(
        f"serving agent {os.environ.get('RUNTIME_AGENT_ID', 'hello-claude')} "
        f"with ANTHROPIC_MODEL={os.environ.get('ANTHROPIC_MODEL', '(unset!)')}",
        flush=True,
    )
    serve(HelloClaudeAdapter)

if __name__ == "__main__":
    main()
```

## 6. `.env.example`

```bash
ANTHROPIC_API_KEY=sk-...
ANTHROPIC_BASE_URL=https://your-llm-proxy.example.com   # or api.anthropic.com
ANTHROPIC_MODEL=claude-sonnet-4-6
```

## 7. Run and test locally (no VM, no Postgres)

```bash
cp .env.example .env          # fill in your creds
uv sync
RUNTIME_AGENT_ID=hello-claude RUNTIME_LISTEN_ADDR=127.0.0.1:8310 \
RUNTIME_SHIM_DB=./shim.db uv run python serve.py
```

In another shell, exercise the contract directly (no auth — this is the agent's
own port):

```bash
SID=$(curl -s 127.0.0.1:8310/sessions -d '{"message":"Say hi in one sentence."}' | jq -r .session_id)
curl -sN "127.0.0.1:8310/sessions/$SID/stream?since=0"
curl -s 127.0.0.1:8310/healthz     # -> ok
```

You should see a `text` event with the reply, then a `done` event.

# Part B — Deploy to runtime.sausheong.com

The control plane is already running at `https://runtime.sausheong.com`. You
deploy the agent as a standalone container on a VM the control plane can reach,
then register its URL.

## 1. Build the agent image (amd64) and push it

A self-contained Dockerfile (`runtime/deploy/gcp/agent-claude/Dockerfile`)
already exists; it copies the shim + this example and runs `uv sync`. Build from
the **projects root** (parent of `runtime/`), for `linux/amd64`:

```bash
cd /path/to/projects                       # contains runtime/ and harness/
REG=asia-southeast1-docker.pkg.dev
PROJECT=mhi-exp-chang-sau-sheong

docker build --platform linux/amd64 \
  -f runtime/deploy/gcp/agent-claude/Dockerfile \
  -t "$REG/$PROJECT/runtime/hello-claude:latest" .
gcloud auth configure-docker "$REG" --quiet
docker push "$REG/$PROJECT/runtime/hello-claude:latest"
```

## 2. Get the deploy bundle onto the agent VM

The reference deployment runs `hello-claude` as a **second container on the
Python agent VM** (`runtime-agent-python`). The bundle is
`runtime/deploy/gcp/agent-claude/` (a `docker-compose.yml` + `.env.example`).
LLM routing (URL + model) is shared across all agent containers via a single
untracked `llm.env` at the parent dir; only the secret API key lives in the
per-agent `.env`.

```bash
ZONE=asia-southeast1-a
# the agent bundle
gcloud compute scp --recurse --tunnel-through-iap --zone "$ZONE" \
  runtime/deploy/gcp/agent-claude runtime-agent-python:~/deploy/
# the shared LLM routing file (URL + model for every agent on this VM)
gcloud compute scp --tunnel-through-iap --zone "$ZONE" \
  runtime/deploy/gcp/llm.env runtime-agent-python:~/deploy/llm.env
```

`llm.env` (untracked — holds the real proxy URL + model):

```bash
ANTHROPIC_BASE_URL=https://<your-litellm-proxy>
ANTHROPIC_MODEL=claude-sonnet-4-6-...
```

The compose file loads it via `env_file: [../llm.env]`, binds `0.0.0.0:8080`
(uvicorn needs the host spelled out to be reachable through Docker's port
publish), and persists SQLite on a named volume.

## 3. Run the container on the VM

```bash
gcloud compute ssh runtime-agent-python --zone "$ZONE" --tunnel-through-iap
# on the VM:
cd ~/deploy/agent-claude
cp .env.example .env          # set ANTHROPIC_API_KEY (URL+model come from ../llm.env)
gcloud auth configure-docker asia-southeast1-docker.pkg.dev --quiet
sudo docker compose up -d
curl -s localhost:8080/healthz   # -> ok
```

## 4. Register the agent on the control plane

The control plane proxies to the agent at a `url:` you register. There are two
ways; the **live deployment uses the dynamic path** (console / API, stored in the
`managed_agents` DB), which needs no control-plane restart.

**Option A — dynamic (recommended): the console.** Sign in to
`https://runtime.sausheong.com/ui`, open **Onboarding → Managed agents**, and add:
- **id**: `hello-claude`
- **url**: the agent VM's private address, e.g. `http://10.10.0.4:8080`

It attaches immediately and appears in **Agents**. (Or use
`runtimectl admin agent add --id hello-claude --url http://10.10.0.4:8080`.)

**Option B — file config.** Add an attach entry to the control plane's
`runtime.remote.yaml` and restart only `runtimed`:

```yaml
agents:
  - id: hello-claude
    tenant: acme                   # MUST match your console users' tenant
    name: Hello (Claude Agent SDK)
    model: claude-sonnet-4-6       # display only; the agent's ANTHROPIC_MODEL wins
    url: http://10.10.0.4:8080     # the agent VM's private IP:port
```

```bash
cd ~/deploy/control-plane && sudo docker compose up -d --force-recreate runtimed
```

Either way, `runtimed` logs `monitoring remote agent agent=hello-claude ...` and
the agent shows up in `GET /agents`.

## 5. Get a key to invoke it

Triggering an agent (`POST /sessions`) needs an **operator** (or admin) service
key for the agent's tenant. Mint one with the CLI against the control plane (you
need an existing admin bearer — the console-minted admin key, or the bootstrap
key):

```bash
RUNTIME_CTL_URL=https://runtime.sausheong.com \
RUNTIME_TOKEN="<an admin key>" \
  runtimectl admin key create --role operator --label hello-cli --tenant acme
#   -> svk-<id>.<secret>   (shown once — store it now)
```

Roles: `viewer` (read only), `operator` (read + invoke), `admin` (operator +
manage users/keys). For invoking, `operator` is enough.

## 6. Exercise it through the public edge

With `curl`:

```bash
BASE=https://runtime.sausheong.com
KEY=<operator-key>

SID=$(curl -s -H "Authorization: Bearer $KEY" "$BASE/agents/hello-claude/sessions" \
  -d '{"message":"Say hi in one sentence."}' | jq -r .session_id)
curl -sN -H "Authorization: Bearer $KEY" "$BASE/agents/hello-claude/sessions/$SID/stream?since=0"
```

Or with `runtimectl` (does create + stream in one command):

```bash
export RUNTIME_CTL_URL=https://runtime.sausheong.com
export RUNTIME_TOKEN=<operator-key>
runtimectl invoke --agent hello-claude "Say hi in one sentence."
```

Follow-up turns in the same session keep context (the SDK `resume=`):

```bash
curl -s -H "Authorization: Bearer $KEY" "$BASE/agents/hello-claude/sessions/$SID/messages" \
  -d '{"message":"And what did I just ask you?"}'
curl -sN -H "Authorization: Bearer $KEY" "$BASE/agents/hello-claude/sessions/$SID/stream?since=0"
```

The agent also shows up in the console (**Agents → hello-claude**) with health,
sessions, and the tool-use/token metrics card.

## How it all fits together

- **You wrote two files** (`adapter.py`, `serve.py`) plus a tiny session map. The
  shim provides the HTTP/SSE contract, supervision, durable sessions, and
  `/healthz` + `/metrics`.
- **Memory** is the Claude Agent SDK's own transcripts (`resume=`), tied to the
  runtime session id by `sessions.py`. SQLite only; no Postgres in the agent.
- **One event per turn**: the adapter yields a single `text` (or `error`) event;
  the shim appends the terminal `done`/`error`.
- **Security**: the agent enforces no bearer — it trusts that only the control
  plane reaches its port (network firewall). Platform auth is at the
  control-plane edge (the service key), not the agent.
- **Lifecycle limits**: the Python shim does not consume native
  `RUNTIME_AGENT_LIMITS`; configure time, turn, and token bounds in the Claude
  SDK adapter or the container/process supervisor.

## References (in this repo)

- Working example: `examples/hello-claude/` (the source this guide mirrors)
- SDK-agnostic deploy path: `deploying-sdk-agents.md`
- Reusable GCP deployment assets: `deploy/gcp/`
- Runtime access, roles, and tenant setup: `tenant-guide.md`
