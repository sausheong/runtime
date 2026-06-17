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

> **This page assumes a runtime control plane is already running** somewhere and
> you want to add this agent to it. The agent is a standalone HTTP service: it
> uses **SQLite** for its own sessions and has **no Postgres dependency** —
> Postgres is the *control plane's* concern, not the agent's.

## Prerequisites

- **A running control plane** (`runtimed`) you can reach and configure. Locally
  that's `http://localhost:8080`; in production it's your deployed endpoint.
- **uv** (Python ≥3.12) — `make serve` runs `uv sync` for you.
- **LLM access**: `ANTHROPIC_API_KEY` + `ANTHROPIC_BASE_URL` + `ANTHROPIC_MODEL`
  (a LiteLLM proxy, or api.anthropic.com).

## Run the agent

```bash
cp .env.example .env          # fill in your proxy key
make serve                    # runs JUST this agent (uv sync + serve.py); no Postgres
```

`make serve` starts the agent on `127.0.0.1:8310` (override `LISTEN_ADDR=`). It
keeps running in the foreground; leave it up and attach the control plane to it.

## Attach it to the control plane

Add an agent entry to the control plane's config so it proxies to this process.

For a **local** control plane, point it at the running agent (attach, not spawn):

```yaml
agents:
  - id: hello-claude
    name: Hello (Claude Agent SDK)
    model: claude-sonnet-4-6
    url: http://127.0.0.1:8310      # the address make serve is listening on
```

Restart the control plane so it picks up the entry; it will log
`monitoring remote agent  agent=hello-claude`. For the **GCP / containerized**
deployment, see [Deploying SDK agents](../../docs/deploying-sdk-agents.md) and
the bundle in [`../../deploy/gcp/agent-claude/`](../../deploy/gcp/agent-claude).

## Exercise it through the control plane

These use `runtimectl`, which targets `RUNTIME_CTL_URL` (default
`http://localhost:8080`) and sends `RUNTIME_TOKEN` as a bearer when set — so the
same commands work against a remote, identity-gated control plane:

```bash
# against a remote, gated control plane:
export RUNTIME_CTL_URL=https://your-control-plane RUNTIME_TOKEN=<service-key>

make conformance              # contract acceptance gate (same suite as Go agents)
make demo                     # say hello; streams the reply
make sessions                 # list this agent's sessions
```

> **All-in-one local dev:** `make run` is a convenience that starts a *local*
> control plane which spawns this agent for you (`command:`), instead of the
> attach flow above. That control plane needs Postgres (for native Go agents'
> DBOS durability) — `make -C ../.. pg-up`, or Postgres.app. The agent itself
> still uses SQLite; the Postgres requirement is the control plane's, not this
> agent's.

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

## Tests

```bash
uv run pytest            # hermetic: fakes query(), no key or network
```

## Deploying

This agent is deployed to the live GCP environment via
[`../../deploy/gcp/agent-claude/`](../../deploy/gcp/agent-claude). See
[Deploying SDK agents](../../docs/deploying-sdk-agents.md) for the full path.
