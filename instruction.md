# Instruction: build a `hello-claude` agent and deploy it to runtime.sausheong.com

You are a coding agent. Build a minimal Claude Agent SDK conversational agent,
host it behind the runtime platform's Python contract shim, and deploy it to
`https://runtime.sausheong.com`. Follow this document top to bottom. Do not skip
verification gates. Each gate that fails STOPS you — fix it before continuing.

**Canonical source of truth:** `examples/hello-claude/` and
`contrib/shims/python/runtime_contract/` in this repo. If any file content below
ever conflicts with those directories, the directories win — re-read them and
match. The file contents embedded below are exact copies as of this writing.

---

## 0. Invariants (non-negotiable)

1. **You write four files only:** `adapter.py`, `sessions.py`, `serve.py`,
   `pyproject.toml` (+ `.env.example`). Everything else — HTTP, SSE, sessions,
   `/healthz`, `/metrics`, process lifecycle — is the installed `runtime_contract`
   shim. Do not reimplement it. Do not edit it.
2. **`run()` must never raise.** Surface every failure as a single
   `ContractEvent(type="error", error=...)`. The shim appends the terminal
   `done`/`error` lifecycle event; you must NOT emit it.
3. **Never read the `RUNTIME_*` env yourself.** `serve()` consumes it.
4. **Never log a secret value or a secret name.** `.env` is gitignored; never
   print `ANTHROPIC_API_KEY`.
5. **No placeholders in committed code.** Every file must be complete and runnable.

---

## 1. Create the project

Create a directory `hello-claude/` (sibling of the other dirs under
`examples/`, i.e. `examples/hello-claude/` — if it already exists, reconcile your
files against it rather than overwriting blindly).

Target layout:

```
hello-claude/
├── pyproject.toml      # deps: claude-agent-sdk + the contract shim
├── adapter.py          # the agent: drives query(), yields contract events
├── sessions.py         # maps runtime session id -> SDK session id
├── serve.py            # entrypoint runtimed/Docker runs
├── .env.example        # ANTHROPIC_* creds template
└── .gitignore          # at minimum: .env, .venv, __pycache__, shim.db
```

---

## 2. Write the files (exact contents)

### `pyproject.toml`

```toml
[project]
name = "hello-claude"
version = "0.1.0"
description = "A minimal Claude Agent SDK conversational agent, hosted on runtime"
readme = "README.md"
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

Notes for you:
- `runtime-contract` is the shim, pulled by **relative path**. The path
  `../../contrib/shims/python` is correct when the project lives at
  `examples/hello-claude/`. If you place the project elsewhere, fix the relative
  path so it points at `contrib/shims/python`.
- If you skip `README.md`, remove the `readme = "README.md"` line.

### `sessions.py`

Maps the runtime session id to the SDK's own session id so follow-up turns can
`resume=` the right conversation. Mechanical; copy verbatim.

```python
"""Runtime session_id → Claude SDK session_id map.

The SDK owns conversation state (JSONL transcripts under CLAUDE_CONFIG_DIR);
the platform owns the runtime session id. This one-table map ties them so a
turn can resume= the SDK session belonging to its runtime session. Lives in
the same SQLite file as the contract store (RUNTIME_SHIM_DB) — separate table,
no schema interference.
"""
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

### `adapter.py`

The agent. Implements the `AgentAdapter` protocol's single `run()` method. Copy
verbatim. The parts you would change for a *different* agent are marked in the
"What to change" note below — for hello-claude, keep them as-is.

```python
"""Adapter: a minimal Claude Agent SDK assistant -> runtime contract.

The smallest useful Claude SDK agent: a plain conversational assistant with no
tools and no domain logic. It exists to show the shortest path from "a Claude
Agent SDK script" to "an agent hosted on runtime".

Per turn: look up the SDK session for this runtime session, drive query() with
resume= so follow-ups continue the conversation, collect the assistant's text,
persist the SDK session id, and yield ONE text event (or ONE error event).
Never raises out of run(); the contract library appends the terminal
done/error lifecycle event.
"""
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

# This is a network-facing agent with no need for the CLI's built-in tools, so
# disable them all. tools=[] is the primary control (empty list disables ALL
# built-ins); the deny-list is a belt-and-braces backup.
BUILTINS_OFF = [
    "Bash", "Read", "Write", "Edit", "Glob", "Grep",
    "WebFetch", "WebSearch", "NotebookEdit", "TodoWrite", "Task",
]


class HelloClaudeAdapter:
    """AgentAdapter backed by the Claude Agent SDK (no tools)."""

    def __init__(self, db_path: str):
        self._sessions = SessionMap(db_path)
        # Transcript home pinned NEXT TO the shim db: resume is keyed by
        # (CLAUDE_CONFIG_DIR, cwd), so both must be stable across restarts.
        self._config_dir = str(Path(db_path).resolve().parent / "claude-config")
        self._model = os.environ["ANTHROPIC_MODEL"]  # fail fast at startup

    def _options(self, resume: str | None) -> ClaudeAgentOptions:
        return ClaudeAgentOptions(
            model=self._model,
            resume=resume,
            system_prompt=SYSTEM_PROMPT,
            tools=[],  # primary control: [] disables ALL built-ins
            disallowed_tools=list(BUILTINS_OFF),  # backup deny-list
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
        # `history` unused: the SDK's own transcripts (resume=) own memory.
        # `images` unused: this minimal agent is text-only.
        try:
            resume = self._sessions.lookup(session_id)
            opts = self._options(resume)
            text_parts: list[str] = []
            result = None
            async for msg in query(
                prompt=message or "Hello!", options=opts
            ):
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
            # Token telemetry (metrics only; never reaches the client stream).
            # Best-effort: a usage-shape change must never break the turn. No
            # tool_call events — this agent runs with tools=[].
            usage_ev = _usage_event(result)
            if usage_ev is not None:
                yield usage_ev
            text = "".join(text_parts) or (result.result if result and result.result else "")
            yield ContractEvent(type="text", text=text or "(no output)")
        except Exception as e:  # never raise out of run()
            yield ContractEvent(type="error", error=str(e))


def _usage_event(result) -> ContractEvent | None:
    """Best-effort token-usage telemetry from a ResultMessage.usage dict (standard
    Anthropic shape). Returns None on any mismatch — telemetry never breaks a turn."""
    try:
        u = getattr(result, "usage", None)
        if not u:
            return None
        return ContractEvent(type="usage", usage={
            "input": int(u.get("input_tokens", 0) or 0),
            "output": int(u.get("output_tokens", 0) or 0),
            "cache_creation": int(u.get("cache_creation_input_tokens", 0) or 0),
            "cache_read": int(u.get("cache_read_input_tokens", 0) or 0),
        })
    except Exception:
        return None
```

**What to change for a different agent (NOT for hello-claude):**
- `SYSTEM_PROMPT` — the persona.
- `tools=[]` — to give the agent tools, populate this (and trim `BUILTINS_OFF`).
- `ClaudeAgentOptions` knobs — `model`, `permission_mode`, `max_turns`.
- The `run()` body — the per-turn logic.

**What is required scaffolding (keep verbatim in every agent):**
- `run()`'s signature (must match `AgentAdapter`) and that it is an async
  generator (`yield`, never `return`, events).
- `__init__(self, db_path)` and the `CLAUDE_CONFIG_DIR` + `cwd` pinning (required
  for `resume=` to survive restarts).
- The `try/except` → `yield error` wrapper (never raise).
- `_usage_event()` is optional; omit only if you don't want token metrics.

### `serve.py`

```python
"""Entry point: serve the minimal Claude-SDK assistant over the contract.

runtimed execs this (config command/workdir) as a supervised agent. Operator
parameters come from the injected RUNTIME_* env via runtime_contract.serve;
this file only builds the adapter. The factory form (passing the class) shares
RUNTIME_SHIM_DB between the contract store and the SDK session map.
"""
from __future__ import annotations

import os

from dotenv import load_dotenv

load_dotenv()  # .env: ANTHROPIC_API_KEY / ANTHROPIC_BASE_URL / ANTHROPIC_MODEL

from runtime_contract import serve  # noqa: E402

from adapter import HelloClaudeAdapter  # noqa: E402


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

Critical detail: pass the adapter **class** (`HelloClaudeAdapter`), not an
instance. The shim calls it as a factory `HelloClaudeAdapter(db_path)` with the
resolved `RUNTIME_SHIM_DB`, which is how your `SessionMap` shares the same SQLite
file as the contract store.

### `.env.example`

```bash
# LLM credentials for the minimal Claude Agent SDK assistant.
# Copy to .env (gitignored); serve.py loads it from this directory.
#
#   cp .env.example .env

# Your LiteLLM proxy key (or an Anthropic key if ANTHROPIC_BASE_URL points there).
ANTHROPIC_API_KEY=sk-...

# The proxy base URL. Leave as api.anthropic.com to use Anthropic directly.
ANTHROPIC_BASE_URL=https://your-llm-proxy.example.com

# The model id the proxy serves.
ANTHROPIC_MODEL=claude-sonnet-4-6
```

### `.gitignore`

```
.env
.venv
__pycache__/
*.db
```

---

## 3. The shim contract (reference — do not implement)

`serve(HelloClaudeAdapter)` stands up these HTTP endpoints on the agent's own
port (the value of `RUNTIME_LISTEN_ADDR`):

- `POST /sessions` — body `{"message": "..."}`; returns `{"session_id": "..."}`.
- `GET  /sessions/{id}/stream?since=0` — SSE stream of contract events; you will
  see your `text` event, then a `done` event the shim appended.
- `POST /sessions/{id}/messages` — body `{"message": "..."}`; a follow-up turn in
  the same session (memory via `resume=`).
- `GET  /healthz` — returns `ok`.
- `GET  /metrics` — Prometheus text: `agent_tokens_total`,
  `agent_tool_calls_total`, `agent_turns_total`, `agent_turn_duration_seconds_*`.
- `GET  /meta` — agent id + contract metadata.

The agent's port enforces **no** bearer auth — it trusts that only the control
plane can reach it (network firewall). Platform auth is at the control-plane edge.

---

## 4. Verify locally (GATE 1 — must pass before deploy)

```bash
cd examples/hello-claude
cp .env.example .env          # fill in real ANTHROPIC_API_KEY / BASE_URL / MODEL
uv sync
RUNTIME_AGENT_ID=hello-claude RUNTIME_LISTEN_ADDR=127.0.0.1:8310 \
RUNTIME_SHIM_DB=./shim.db uv run python serve.py
```

In a second shell:

```bash
# healthz
test "$(curl -s 127.0.0.1:8310/healthz)" = "ok" || echo "FAIL: healthz"

# one turn
SID=$(curl -s 127.0.0.1:8310/sessions -d '{"message":"Say hi in one sentence."}' | jq -r .session_id)
test -n "$SID" || echo "FAIL: no session id"
curl -sN "127.0.0.1:8310/sessions/$SID/stream?since=0"   # expect a text event then a done event

# memory: follow-up in the same session
curl -s "127.0.0.1:8310/sessions/$SID/messages" -d '{"message":"What did I just ask you?"}'
curl -sN "127.0.0.1:8310/sessions/$SID/stream?since=0"   # the reply should reference the prior question
```

**Pass criteria:** `healthz` returns `ok`; the stream contains a `"type":"text"`
event with a non-empty reply followed by `"type":"done"`; the follow-up reply
demonstrates memory. If any check fails, fix and re-run before continuing.

If `examples/hello-claude/tests/` exists, also run `uv run pytest` and require green.

---

## 5. Deploy to runtime.sausheong.com

The control plane is already running. You deploy the agent as a standalone
container somewhere the control plane can reach over the private network, then
register its URL.

Get these values from the operator before running — **do NOT guess or invent
them, and do NOT hardcode any literal here.** Set them as shell variables and use
the variables in every command below.

**A. Platform requirements — these must hold or the platform cannot manage the
agent. They are dictated by runtime.sausheong.com, not by you:**

- **Tenant id** (`TENANT`) — the agent's tenant. MUST match the tenant of the
  console users / keys that will invoke it, or invocation returns `403`.
- **Agent URL** (`AGENT_URL`) — an `http://host:port` the control plane can dial
  over the private network, and the firewall must admit the control plane to it.
  This URL is what you register; reachability is the one hard networking
  constraint.
- **An admin (or bootstrap) service key** — needed to register the agent and to
  mint the operator key that invokes it.
- **Control plane base URL** (`CTL = https://runtime.sausheong.com`).

**B. Deployment conveniences — how THIS reference happens to host the container.
Swap any of these freely; the control plane never sees them. None is required by
the platform:**

- **Image registry** (`REG`, `PROJECT`/repo path) — where you push the built
  image. Any registry works (Docker Hub, GHCR, a different AR project), or build
  directly on the host with no registry at all. The control plane never pulls
  your image — the agent host does.
- **Compute host + how you reach it** (`ZONE`, VM name, SSH/scp transport) — a
  GCP VM in some zone is incidental. Kubernetes, another cloud, or bare metal is
  equally fine as long as it satisfies requirement **A.AGENT_URL** above.
- **Container port / bind** — any port works; just register the matching
  `AGENT_URL`. uvicorn must bind `0.0.0.0` (not `127.0.0.1`) to be reachable
  through Docker's port publish.

```bash
# Fill from the operator. Examples of SHAPE only — substitute real values.
CTL=https://runtime.sausheong.com
TENANT=<tenant-id>
AGENT_ID=hello-claude
AGENT_URL=http://<agent-host>:<port>     # must be dialable from the control plane
REG=<registry-host>                      # convenience: your image registry
PROJECT=<registry-project-or-namespace>  # convenience
IMAGE="$REG/$PROJECT/runtime/$AGENT_ID:latest"
ZONE=<compute-zone>                      # convenience: only if deploying to a GCP VM
AGENT_HOST=<vm-name-or-ssh-target>       # convenience: how you reach the host
```

### 5a. Build and push the image (amd64)

A self-contained Dockerfile already exists at
`deploy/gcp/agent-claude/Dockerfile`. It copies the shim + this example and runs
`uv sync`. Build from the **projects root** (the parent of `runtime/` and
`harness/`), for `linux/amd64` (the VMs are x86-64):

```bash
cd /path/to/projects                       # contains runtime/ and harness/

docker build --platform linux/amd64 \
  -f runtime/deploy/gcp/agent-claude/Dockerfile \
  -t "$IMAGE" .
# Authenticate to whatever registry $REG is (this line is registry-specific).
gcloud auth configure-docker "$REG" --quiet
docker push "$IMAGE"
```

### 5b. Ship the deploy bundle + shared LLM routing to the VM

The bundle is `deploy/gcp/agent-claude/` (a `docker-compose.yml` + `.env.example`).
LLM routing (base URL + model) is shared across all agent containers on the VM via
a single untracked `deploy/gcp/llm.env`; only the secret API key lives in the
per-agent `.env`.

```bash
# Transport is host-specific. For a GCP VM (the reference), scp over IAP:
gcloud compute scp --recurse --tunnel-through-iap --zone "$ZONE" \
  runtime/deploy/gcp/agent-claude "$AGENT_HOST:~/deploy/"
gcloud compute scp --tunnel-through-iap --zone "$ZONE" \
  runtime/deploy/gcp/llm.env "$AGENT_HOST:~/deploy/llm.env"
```

`llm.env` holds the real routing (untracked — never commit it):

```bash
ANTHROPIC_BASE_URL=https://<your-litellm-proxy>
ANTHROPIC_MODEL=claude-sonnet-4-6-...
```

The compose file loads it via `env_file: [../llm.env]`, binds `0.0.0.0:<port>`
(uvicorn must bind `0.0.0.0`, not `127.0.0.1`, to be reachable through Docker's
port publish — keep the published port consistent with the `AGENT_URL` you
register), and persists SQLite on a named volume.

### 5c. Run the container on the host

```bash
# Reach the host however it's reachable. For the reference GCP VM:
gcloud compute ssh "$AGENT_HOST" --zone "$ZONE" --tunnel-through-iap
# on the host:
cd ~/deploy/agent-claude
cp .env.example .env          # set ONLY ANTHROPIC_API_KEY (URL+model come from ../llm.env)
gcloud auth configure-docker "$REG" --quiet   # registry-specific auth
sudo docker compose up -d
curl -s localhost:<port>/healthz   # GATE 2: must print "ok"
```

**GATE 2:** `localhost:<port>/healthz` returns `ok` on the host. If not, inspect
`sudo docker compose logs` (a common cause: missing/incorrect `ANTHROPIC_*` —
the agent fails fast on `ANTHROPIC_MODEL`). Fix before continuing.

### 5d. Register the agent on the control plane

Two ways. **Prefer the dynamic path** (no control-plane restart). Use
`runtimectl` with an admin bearer:

```bash
export RUNTIME_CTL_URL="$CTL"
export RUNTIME_TOKEN="<an admin key>"
runtimectl admin agent add --id "$AGENT_ID" --url "$AGENT_URL"
```

(Or via the console UI: sign in at `$CTL/ui` → **Onboarding → Managed agents** →
add the id `$AGENT_ID` and url `$AGENT_URL`.)

Fallback — **file config** (needs a `runtimed` restart): add to the control
plane's `runtime.remote.yaml` (substitute your `AGENT_ID`, `TENANT`, `AGENT_URL`):

```yaml
agents:
  - id: <AGENT_ID>
    tenant: <TENANT>               # MUST match your console users' tenant
    name: Hello (Claude Agent SDK)
    model: claude-sonnet-4-6       # display only; the agent's ANTHROPIC_MODEL wins
    url: <AGENT_URL>
```

then `cd ~/deploy/control-plane && sudo docker compose up -d --force-recreate runtimed`.

Either way, confirm: `runtimed` logs `monitoring remote agent agent=$AGENT_ID`
and the agent appears in `GET /agents`.

### 5e. Mint an operator key to invoke it

Triggering a session needs an **operator** (or admin) key for the agent's tenant.
Roles: `viewer` (read), `operator` (read + invoke), `admin` (operator + manage
users/keys). Mint with an existing admin bearer:

```bash
RUNTIME_CTL_URL="$CTL" \
RUNTIME_TOKEN="<an admin key>" \
  runtimectl admin key create --role operator --label hello-cli --tenant "$TENANT"
#   -> svk-<id>.<secret>   (shown ONCE — capture it now; never log it)
```

---

## 6. Verify end-to-end through the public edge (GATE 3 — acceptance)

```bash
KEY=<operator-key>

SID=$(curl -s -H "Authorization: Bearer $KEY" "$CTL/agents/$AGENT_ID/sessions" \
  -d '{"message":"Say hi in one sentence."}' | jq -r .session_id)
curl -sN -H "Authorization: Bearer $KEY" "$CTL/agents/$AGENT_ID/sessions/$SID/stream?since=0"
```

Or in one command:

```bash
export RUNTIME_CTL_URL="$CTL"
export RUNTIME_TOKEN=<operator-key>
runtimectl invoke --agent "$AGENT_ID" "Say hi in one sentence."
```

Follow-up turn (memory):

```bash
curl -s -H "Authorization: Bearer $KEY" "$CTL/agents/$AGENT_ID/sessions/$SID/messages" \
  -d '{"message":"And what did I just ask you?"}'
curl -sN -H "Authorization: Bearer $KEY" "$CTL/agents/$AGENT_ID/sessions/$SID/stream?since=0"
```

Confirm metrics populated after at least one invocation:

```bash
curl -s "$CTL/metrics" | grep '^agent_' | head
```

**GATE 3 / acceptance criteria (all must hold):**
1. `POST /agents/$AGENT_ID/sessions` returns a session id with the operator key.
2. The stream yields a non-empty `text` event then `done`.
3. The follow-up reply references the prior question (memory works).
4. `GET /metrics` shows non-zero `agent_turns_total` / `agent_tokens_total` for the agent.
5. The agent appears healthy in the console (**Agents → $AGENT_ID**).

---

## 7. Common failure modes

| Symptom | Cause | Fix |
|---|---|---|
| Agent exits immediately on start | `ANTHROPIC_MODEL` unset (`__init__` fails fast) | Ensure `llm.env`/`.env` set it; check `docker compose logs`. |
| `healthz` ok but invocation hangs/errors | bad `ANTHROPIC_BASE_URL`/key | Verify proxy URL + key; check agent logs. |
| Control plane can't reach agent | firewall / wrong host:port / not bound to `0.0.0.0` | Confirm compose binds `0.0.0.0:<port>` and `AGENT_URL` matches; verify the firewall admits the control plane. |
| `403` invoking | key is `viewer`, or wrong tenant | Mint an `operator` key for the agent's tenant. |
| Memory not working across turns | `CLAUDE_CONFIG_DIR`/`cwd` not stable | Keep the `__init__` pinning; don't randomize the workdir. |
| Metrics all zero | agent restarted (counters are in-memory) | Expected after restart; fire one invocation to repopulate. |

---

## 8. References (in this repo)

- Working example (source of truth): `examples/hello-claude/`
- The shim: `contrib/shims/python/runtime_contract/` (+ its `README.md`)
- Human-readable tutorial: `hello-claude.md`
- SDK-agnostic deploy path: `docs/deploying-sdk-agents.md`
- Full GCP walkthrough (VPC, firewall, VMs): `deploy/gcp/README.md`
- Using the deployed runtime (keys, roles, invoke): `deploy/gcp/USING.md`
- Tenant onboarding + roles: `docs/tenant-guide.md`
