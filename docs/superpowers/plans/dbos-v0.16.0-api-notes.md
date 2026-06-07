# DBOS Transact Go v0.16.0 — verified API surface

> Captured via `go doc` against the actually-installed `github.com/dbos-inc/dbos-transact-golang@v0.16.0`.
> Use these EXACT signatures in `agentruntime` (Tasks 3 & 5). The plan's "confirm via go doc"
> notes are resolved here. Where these differ from the plan's illustrative code, THESE WIN.

## Core types

```go
type Workflow[P any, R any] func(ctx DBOSContext, input P) (R, error)
type Step[R any]            func(ctx context.Context) (R, error)

type Config struct {
    AppName     string // required
    DatabaseURL string // required (system DB connection string); pgx-style DSN
    // ...many optional fields (Logger, AdminServer, Serializer, etc.)
}

type DBOSContext interface { ... }
type WorkflowHandle[R any] interface {
    GetResult(opts ...GetResultOption) (R, error) // blocks until completion
    GetStatus() (WorkflowStatus, error)
    GetWorkflowID() string
}
```

## Lifecycle

```go
dctx, err := dbos.NewDBOSContext(ctx context.Context, cfg dbos.Config) (DBOSContext, error)
dbos.RegisterWorkflow[P,R](dctx, fn Workflow[P,R], opts ...WorkflowRegistrationOption) // BEFORE Launch
err := dbos.Launch(dctx)                 // starts runtime AND recovers pending workflows
dbos.Shutdown(dctx, timeout time.Duration) // note: NO error return
```

## Running workflows & steps

```go
handle, err := dbos.RunWorkflow[P,R](dctx, fn, input P, dbos.WithWorkflowID(id)) (WorkflowHandle[R], error)
// RunWorkflow returns a handle immediately; the workflow runs durably in the
// background. Call handle.GetResult() to block for completion. So you do NOT
// need to wrap RunWorkflow in your own goroutine — it is already async.

r, err := dbos.RunAsStep[R](dctx_or_stepctx, fn Step[R], opts ...StepOption) (R, error)
// IMPORTANT: inside a workflow body, the value passed to RunAsStep is the
// workflow's DBOSContext (it satisfies what RunAsStep needs). The Step fn
// itself receives a plain context.Context.
```

## Reading the workflow id INSIDE a workflow body

```go
id, err := dbos.GetWorkflowID(ctx DBOSContext) (string, error)   // <-- returns (string, error), NOT just string
```
The plan's turnstep.go uses `dbos.GetWorkflowID(ctx)` as a single-value expression in several
places — that is WRONG for v0.16.0. Capture it ONCE at the top of the workflow:
```go
func (m *Manager) sessionWorkflow(ctx dbos.DBOSContext, in turnInput) (string, error) {
    wfID, _ := dbos.GetWorkflowID(ctx)
    // ...use wfID everywhere instead of re-calling GetWorkflowID(ctx)
}
```

## Recovery / re-attach (for the integration test, Task 8)

```go
handle, err := dbos.RetrieveWorkflow[R](dctx, workflowID) (WorkflowHandle[R], error)
// After a restart, Launch() auto-recovers in-flight workflows. To wait on a
// specific recovered workflow's completion, RetrieveWorkflow then GetResult.
// Also available: ResumeWorkflow[R](dctx, workflowID, ...).
```
On replay, completed steps return their checkpointed values WITHOUT re-executing
(this is the property the whole milestone relies on).

## Serialization

Workflow input `P` and output `R`, and step output `R`, are serialized with the default
JSON serializer (overridable via Config.Serializer). So `turnInput`, `turnOutput`, and
`[]session.SessionEntry` must be JSON-encodable — they are (session.SessionEntry has json tags).

## Workflow id == platform session id

Pass the platform session id as `dbos.WithWorkflowID(sessionID)` when calling RunWorkflow,
so process restart recovers exactly that workflow. The store row and the DBOS workflow id
must be identical.

## generic-type note

`RegisterWorkflow`, `RunWorkflow`, `RunAsStep`, `RetrieveWorkflow` are all generic. Go infers
type params from the function/arg, but the workflow fn must match `Workflow[P,R]` exactly:
`func(dbos.DBOSContext, P) (R, error)`. `m.sessionWorkflow` has signature
`func(dbos.DBOSContext, turnInput) (string, error)` ⇒ P=turnInput, R=string. Good.
For RunAsStep the closure must be `func(context.Context) (turnOutput, error)` ⇒ R=turnOutput.
