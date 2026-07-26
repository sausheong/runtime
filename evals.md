# Evaluations

Runtime supports two complementary evaluation paths:

- **Golden-set runs** send known inputs to an agent and score its responses.
- **Online evaluation** samples completed production sessions and scores their
  captured output against an agent policy.

Both paths are measurement only. Evaluation results do not block an agent
session, change its response, or act as a deployment gate automatically.

## Prerequisites

Evaluation administration requires an authenticated tenant administrator. A
platform superuser can act for another tenant with `--tenant`.

Point the CLI at the control plane and provide a durable credential:

```bash
export RUNTIME_CTL_URL=http://localhost:8080
export RUNTIME_TOKEN=<tenant-admin-service-key>
```

Golden-set runs work with native Go agents and remote contract-compatible
agents because the evaluator uses the common `POST /sessions` and SSE
`GET /sessions/{id}/stream` contract.

Online policy injection and failure classification are implemented by the
native `agentd` runtime. A foreign-SDK or independently operated remote agent
must implement equivalent capture and scoring if it needs that path.

## Golden sets

A golden set is a named, tenant-scoped list of cases. Each case contains an
input, a scorer, and the target used by that scorer.

| Scorer | Required field | Pass condition |
|---|---|---|
| `exact` | `expected` | Output equals the expected value exactly |
| `contains` | `expected` | Output contains the expected substring |
| `regex` | `expected` | Output matches the Go regular expression |
| `judge` | `expected`, `rubric`, or both | The configured judge model accepts the output |

Regex patterns are compiled when the set is stored. Invalid cases are rejected
before a run can start.

Create `greetings.json`:

```json
{
  "cases": [
    {
      "input": "Say hello.",
      "scorer": "contains",
      "expected": "hello"
    },
    {
      "input": "Return only the number 42.",
      "scorer": "regex",
      "expected": "^42$"
    },
    {
      "input": "Explain why clear error messages matter.",
      "scorer": "judge",
      "rubric": "The answer is concise, accurate, and gives a practical reason."
    }
  ]
}
```

The CLI also accepts a bare JSON array instead of the `{"cases":[...]}` wrapper.

Store and inspect the set:

```bash
runtimectl admin eval set add --name greetings --file greetings.json
runtimectl admin eval set ls
```

Start a run and wait for its aggregate and per-case results:

```bash
runtimectl admin eval run greetings --agent support --wait
```

For asynchronous operation:

```bash
runtimectl admin eval run greetings --agent support
runtimectl admin eval runs
runtimectl admin eval results <run-id>
```

Remove a set:

```bash
runtimectl admin eval set rm greetings
```

Use `--tenant <tenant>` with set, run, or list commands when acting as a
platform superuser.

### Judge scoring

`judge` cases require `RUNTIME_EVAL_JUDGE_MODEL`. The judge uses the
OpenAI-compatible `OPENAI_BASE_URL` and `OPENAI_API_KEY` configuration.
Deterministic scorers continue to work when no judge is configured; judge cases
fail closed with an unavailable detail.

Each golden-set case has an invocation timeout:

```bash
RUNTIME_EVAL_INVOKE_TIMEOUT=120s
```

The value is a Go duration. The default is `120s`.

### Recovery

Runs and individual case results are durable. At control-plane startup,
`runtimed` resumes pending or running evaluation runs. It reads the case indexes
already persisted for a run and does not invoke those successful cases again.

Before executing, a worker atomically claims the run with an owner and expiry.
Live leases cannot be stolen, and a heartbeat renews the claim during long
provider or judge calls. Expired leases are recoverable. Recovery scans at
startup and periodically thereafter using a bounded worker pool, so a run held
by a dead worker is reclaimed after expiry without another restart.

The persistence layer enforces the same state machine in memory and PostgreSQL:
new runs must be `pending` and unleased, claims require a non-empty owner and a
positive lease duration, and only a live owner may finalize to `completed` or
`error`. Duplicate run IDs and attempts to create running/terminal rows, use an
empty owner, or finalize back to a non-terminal state are rejected. Database
constraints also prevent direct SQL from storing an incoherent status, lease,
and completion timestamp.

## Online evaluation

Golden sets test known inputs. Online evaluation samples what a native agent
actually did in completed sessions.

An online policy belongs to one tenant and agent. It contains:

- `sample_rate`, an integer percentage from 0 through 100;
- one or more named criteria.

Online criteria support `contains`, `regex`, and `judge`. The `exact` scorer is
not allowed because live output rarely equals one fixed string.

Create `criteria.json`:

```json
{
  "criteria": [
    {
      "name": "mentions-source",
      "scorer": "contains",
      "pattern": "source"
    },
    {
      "name": "concise",
      "scorer": "judge",
      "rubric": "The response answers directly without unnecessary detail."
    }
  ]
}
```

As with golden sets, a bare criteria array is also accepted.

Store a policy that scores 25 percent of completed sessions:

```bash
runtimectl admin eval policy set \
  --agent support \
  --rate 25 \
  --file criteria.json
```

List or remove policies:

```bash
runtimectl admin eval policy ls
runtimectl admin eval policy rm support
```

Read results for the tenant or one session:

```bash
runtimectl admin eval online-results
runtimectl admin eval online-results --session <session-id>
```

The control plane validates a policy and injects it into a managed agent as
`RUNTIME_EVAL_POLICY` when the process starts. Do not set that variable by hand.
Restart an already-running managed agent after changing its policy so the new
spawn receives the updated policy.

Online judge criteria need the judge model and OpenAI-compatible credentials in
the agent process that performs the scoring.

## Transcript storage and retention

Runtime captures the transcript needed for online evaluation by default.
Credential-shaped fields, bare JWTs, and common provider/service token formats
are filtered recursively before persistence. This deterministic filter is best
effort, not a PII-removal or secrecy guarantee.

Set `RUNTIME_TRANSCRIPT_CAPTURE=0` on agent processes to disable capture
independently of evaluation retention. Native agent builders can also supply
`Config.TranscriptFilter`; it runs after the built-in filter and before the
database write. A filter error or invalid JSON suppresses that turn's capture.

Completed or failed evaluation runs, their case results, online results, and
captured transcripts are retained for 30 days by default:

```bash
RUNTIME_EVAL_RETENTION=720h
```

Set another non-negative Go duration to change the retention period:

```bash
RUNTIME_EVAL_RETENTION=168h
```

Set it to zero to disable automatic deletion:

```bash
RUNTIME_EVAL_RETENTION=0
```

Disabling retention logs a warning because transcripts and results will
accumulate. A sweep runs at startup and then every six hours. Each sweep uses
bounded deletion statements and stops after 20 batches or 30 seconds.

Captured data remains sensitive. Restrict database/admin access, encrypt
database storage and backups, export required audit data before deletion, and
apply the organisation's residency and classification rules.

## Failure classification

Every terminal native-agent session is assigned one failure category. This is
deterministic, always enabled, and independent of golden-set execution.
First-match precedence determines the category:

| Precedence | Category | Rule |
|---:|---|---|
| 1 | `timeout` | A per-turn deadline fired |
| 2 | `limit_exceeded` | The session exceeded `max_turns`, `max_tokens`, or `session_timeout` |
| 3 | `agent_error` | The session errored or aborted for another reason |
| 4 | `tool_error` | A turn produced a tool result marked as an error |
| 5 | `quality_fail` | The session completed and a complete online score was durably persisted with a failed criterion |
| 6 | `none` | Clean completion with no failed criterion |

Read the breakdown for an agent:

```bash
runtimectl admin eval failures --agent support
runtimectl admin eval failures --agent support --since 24h
```

`--agent` is required. Results are scoped by the caller's tenant unless a
platform superuser acts across tenants.

## Console

The web console exposes the same main workflows:

- `/ui/onboarding` manages golden sets and online policies.
- `/ui/observability` launches runs and lists run and online results.
- `/ui/observability/eval-runs/<run-id>` shows per-case results.

The evaluation sections are hidden when the relevant stores are not configured.

## Metrics

Golden-set control-plane metrics:

| Metric | Labels | Meaning |
|---|---|---|
| `runtime_eval_runs_total` | `tenant,status` | Finalized runs by `completed` or `error` status |
| `runtime_eval_cases_total` | `tenant,result` | Cases scored as `pass` or `fail` |
| `runtime_retention_reaped_total` | `kind` | Evaluation capture rows (`evaluation_capture`) and completed run records (`evaluation_run`) removed by retention |

Native-agent online metrics:

| Metric | Labels | Meaning |
|---|---|---|
| `agent_eval_sessions_scored_total` | `agent,tenant` | Sampled sessions scored |
| `agent_eval_criteria_total` | `agent,tenant,result` | Online criteria scored as `pass` or `fail` |
| `agent_eval_failures_total` | `agent,tenant,category` | Initial deterministic terminal classifications by the fixed failure taxonomy |
| `agent_eval_failure_refinements_total` | `agent,tenant,from,to` | Successful durable scoring refinements, currently `none` to `quality_fail` |
| `agent_eval_queue_dropped_total` | `agent,tenant,reason` | Sampled jobs rejected by the bounded queue or abandoned at bounded shutdown |
| `agent_eval_scoring_failures_total` | `agent,tenant,reason` | Incomplete online scores caused by scorer, result-store, or classification-store failure |

Metrics are operational signals rather than the source of truth. Durable run,
case, and online-result records are available through the CLI, API, and console.

Terminal classification is persisted before optional queue admission. A full
or closing queue therefore retains the truthful deterministic category.
Provider/judge failure is an incomplete score, not a failed quality criterion,
and cannot produce `quality_fail`. Result and classification writes are checked
before success metrics are emitted. DBOS replay preserves a later
`quality_fail` refinement. Initial-classification, refinement, criterion, and
scored-session counters increment only for their corresponding first durable
write or transition.

## Operational boundaries

- Evaluation is measurement only; deployment gating must be implemented by the
  surrounding release workflow.
- Judge scoring is non-deterministic and consumes model tokens.
- Online transcript capture has storage and privacy implications even with
  best-effort credential filtering.
- Run leases prevent duplicate recovery workers; control-plane HA still
  requires leader ownership for non-evaluation singleton duties.
- Process counters reset on restart and are unsuitable for financial
  accounting; within a process, durable first-write scoring and classification
  transitions are replay-safe.

See also the [Runtime overview](runtime.md), [operator guide](operator-guide.md),
and [roadmap](ROADMAP.md).
