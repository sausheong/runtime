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

## Prerequisites

- **Postgres** for the control plane (`make -C ../.. pg-up`, or Postgres.app);
  the agent itself uses SQLite.
- **uv** (Python ≥3.12) — `make run` runs `uv sync` for you.
- **LLM access**: `ANTHROPIC_API_KEY` + `ANTHROPIC_BASE_URL` + `ANTHROPIC_MODEL`
  (a LiteLLM proxy, or api.anthropic.com).

## Quick start

```bash
cp .env.example .env          # fill in your proxy key
make run                      # builds binaries, uv sync, runs the control plane
# in a second shell:
make conformance              # contract acceptance gate (same suite as Go agents)
make demo                     # say hello; streams the reply
make sessions                 # list this agent's sessions
```

## How it works

- **Per-turn `query()` + `resume=`.** Each contract turn drives a fresh `query()`
  call. Conversation memory is the SDK's own JSONL transcripts (under
  `./claude-config`, pinned via `CLAUDE_CONFIG_DIR` so resume survives restarts).
  A one-table runtime→SDK session-id map (`sessions.py`) in the shim's SQLite
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
