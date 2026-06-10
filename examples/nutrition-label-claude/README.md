# Claude Agent SDK — SG Nutrition Investigator (hosted on runtime)

The **third** implementation of the SG Nutrition Investigator (after the
Go/harness original and the [OpenAI Agents SDK port](../nutrition-label-openai/)),
built on the [Claude Agent SDK](https://docs.claude.com/en/api/agent-sdk/python)
and hosted on the runtime platform through the Python contract shim
(`../../contrib/shims/python`, the reusable `runtime_contract` library).

This is the **C1-M2 "thin adapter" reuse proof**: hosting a second, maximally
different foreign SDK required **zero Claude-specific shim changes** —
`runtime_contract` is consumed unchanged as the same editable path dependency
the OpenAI example uses. (One framework-agnostic addition landed alongside
this milestone: a `POST /sessions/{id}/messages` follow-up endpoint, which
benefits every adapter equally.) Everything Claude-specific lives in this
directory: `adapter.py` (139 lines, 100 of code), plus the SDK-free domain
port (`agent.py`), the five MCP tools (`tools.py`), and the session map
(`sessions.py`).

## Prerequisites

- **Postgres** for the control plane (`make -C ../.. pg-up`, or Postgres.app);
  the agent itself uses SQLite.
- **uv** (Python ≥3.12) — `make run` runs `uv sync` for you.
- **LLM access**: `ANTHROPIC_API_KEY` + `ANTHROPIC_BASE_URL` + `ANTHROPIC_MODEL`
  (a LiteLLM proxy with namespaced model ids, or api.anthropic.com).

## Quick start

```bash
cp .env.example .env          # fill in your LiteLLM proxy key
make run                      # builds binaries, uv sync, runs the control plane
# in a second shell:
make conformance              # contract acceptance gate (same suite as Go agents)
make demo-image IMAGE=milo.jpeg   # base64 the photo → POST → stream the verdict
make demo-text                # investigate a pasted label
make sessions                 # list this agent's sessions
```

## How it works

- **Per-turn `query()` + `resume=`.** Each contract turn drives a fresh
  `query()` call; conversation memory is the SDK's own JSONL transcripts
  (under `./claude-config`, pinned via `CLAUDE_CONFIG_DIR` so resume survives
  restarts — resume is keyed by config dir + cwd). A one-table runtime→SDK
  session-id map in the shim's SQLite (`sessions.py`) ties the platform's
  session id to the SDK's, so a follow-up message in the same runtime session
  resumes the right SDK conversation. This is Level-1 durability via the
  SDK's *native* persistence — an honest test of it, not a re-implementation.
- **Five in-process MCP tools** (`@tool` + `create_sdk_mcp_server`):
  `recall_product`, `check_sfa_additive`, `check_hcs`,
  `calculate_nutri_grade`, and `submit_verdict`. The SDK has no
  `output_type=`; `submit_verdict` is the **typed-output channel** — its
  input schema is generated from the `NutritionVerdict` pydantic model, the
  handler validates and stashes the object, and the adapter renders it to the
  same prose block the other two implementations emit.
- **Built-ins disabled.** `tools=[]` is the primary control (SDK 0.2.95: an
  empty list disables ALL built-ins), with a `disallowed_tools` deny-list
  (Bash, Read, Write, Edit, WebFetch, ...) as belt-and-braces, plus
  `permission_mode="dontAsk"` allowing only the `mcp__nutrition__*` tools.
  The spike (`spike_vision.py`) verified the combination.
- **Vision** rides the SDK's streaming-input form: an async iterable yielding
  one user message whose content mixes a text block and a base64 image block.
  Only the first contract image is sent (parity with the OpenAI adapter).
- **Subprocess per turn.** `query()` spawns the Claude Code CLI per call —
  simple and restart-survivable, but each turn pays process startup. Keeping
  a warm `ClaudeSDKClient` is the Level-2-era optimization, deliberately not
  taken now.

## Three implementations, one agent

| | Go / harness ([`../nutrition-label-go`](../nutrition-label-go/)) | OpenAI Agents SDK ([`../nutrition-label-openai`](../nutrition-label-openai/)) | Claude Agent SDK (this dir) |
|---|---|---|---|
| Conversation memory | harness `session` + DBOS durable workflow | SDK `SQLiteSession` keyed on runtime session id | SDK-native `resume=` + JSONL transcripts; runtime→SDK id map |
| Typed output | prompt-shaped prose verdict | `output_type=NutritionVerdict` (validated object) | `submit_verdict` MCP tool — schema from the same pydantic model |
| Tool definition | `tool.Tool` interface + JSON schema by hand | `@function_tool` (schema inferred from type hints) | `@tool` + explicit input-schema dict, in-process MCP server |
| Adapter to host on runtime | native (`agentruntime.Serve`) | `adapter.py` — 67 lines, 39 of code | `adapter.py` — 139 lines, 100 of code |

Same SFA additive data (`sfa_additives.json`), same learned-alias +
product-verdict memory (`agent_memory.json`, gitignored), same rendered
verdict block — so the three can be compared side by side on identical labels.

## Limitations

- **Pinned `claude-agent-sdk` 0.2.95.** The `tools=[]` built-in-disable
  semantics and the streaming-input image shape were verified against this
  version; bumps need a re-run of `spike_vision.py`.
- **Only the first image** of a contract turn is sent to the model.
- **Subprocess-per-turn latency.** Every turn spawns the CLI; vision turns
  through the proxy can take 60–120 s. A warm client is the known upgrade.

Durability is Level 1 (sessions/events persist in `shim.db`, replayable via
`?since=N`; conversation memory via the SDK's transcripts + `resume=`); plus
the agent's own `agent_memory.json` learned aliases + product verdicts.
Level 2 (in-flight crash resume) is out of scope — see the repo `ROADMAP.md` §C1.
