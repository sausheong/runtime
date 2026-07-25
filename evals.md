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

This recovery model currently assumes one active control plane. Multiple
control-plane replicas need a database-backed claim or lease before they can
safely recover the same run.

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

Runtime captures the transcript needed for online evaluation. Credential-shaped
fields and common bearer-token patterns are redacted before persistence.

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
accumulate. A sweep runs at startup and then every six hours.

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
| 5 | `quality_fail` | The session completed but an online criterion failed |
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

Native-agent online metrics:

| Metric | Labels | Meaning |
|---|---|---|
| `agent_eval_sessions_scored_total` | `agent,tenant` | Sampled sessions scored |
| `agent_eval_criteria_total` | `agent,tenant,result` | Online criteria scored as `pass` or `fail` |
| `agent_eval_failures_total` | `agent,tenant,category` | Terminal sessions by the fixed failure taxonomy |

Metrics are operational signals rather than the source of truth. Durable run,
case, and online-result records are available through the CLI, API, and console.

## Operational boundaries

- Evaluation is measurement only; deployment gating must be implemented by the
  surrounding release workflow.
- Judge scoring is non-deterministic and consumes model tokens.
- Online transcript capture has storage and privacy implications even with
  credential-pattern redaction.
- Exact recovery of incomplete runs assumes one control-plane process.
- Native-agent metric increments can be replay-tolerant rather than suitable
  for financial accounting.

See also the [Runtime overview](runtime.md), [operator guide](operator-guide.md),
and [roadmap](ROADMAP.md).
