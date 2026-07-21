package agentruntime

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/sausheong/harness/llm"
	hrt "github.com/sausheong/harness/runtime"
	"github.com/sausheong/harness/session"
	"github.com/sausheong/runtime/internal/config"
	"github.com/sausheong/runtime/internal/eval"
	"github.com/sausheong/runtime/internal/identity"
	"github.com/sausheong/runtime/internal/memory"
	"github.com/sausheong/runtime/internal/obs"
	"github.com/sausheong/runtime/internal/store"
)

// Manager owns per-session durable workflows and event fan-out.
type Manager struct {
	agentID string
	cfg     Config
	dbosCtx dbos.DBOSContext
	st      store.Store
	// metrics is this agent's Prometheus registry. Nil-safe: tests construct
	// Manager without it and every obs method no-ops on a nil receiver.
	metrics *obs.AgentMetrics
	// price is this agent's per-model price (from cfg.Price), or nil when the
	// model is unpriced. Passed to every TurnObserved.
	price *config.ModelPrice
	// authToken, when non-empty, requires every inbound request to carry
	// Authorization: Bearer <authToken>. "" ⇒ no auth (local/loopback agents).
	authToken string
	// replica is this process's 0-based replica index (from
	// RUNTIME_AGENT_REPLICA). Stamped onto each session row at create so the
	// control plane can pin session-scoped requests back to this replica.
	replica int
	// subjectForwarding gates reading the forwarded caller identity
	// (X-Runtime-{User,Tenant,Role}) on POST /sessions (from
	// RUNTIME_SUBJECT_FORWARDING). Off ⇒ the headers are ignored and turnInput
	// carries an empty subject (today's behavior).
	subjectForwarding bool
	// limits is the operator-resolved lifecycle limit set (from
	// RUNTIME_AGENT_LIMITS). Zero value ⇒ no limits. Immutable after Serve
	// constructs the Manager, so workflow-body reads are deterministic.
	//
	// Limits are process-lifetime constants: changing RUNTIME_AGENT_LIMITS
	// across a restart is NOT replay-safe for in-flight sessions. Adding or
	// removing session_timeout inserts/removes a decision step per iteration,
	// so DBOS recovery hits an UnexpectedStepError and the session fails with
	// status "error" (fail-closed). Changing max_turns/max_tokens changes the
	// verdicts recovered sessions compute. Reconfiguring across a restart may
	// therefore terminate recovered in-flight sessions with status "error".
	limits config.Limits

	// evalPolicy is this agent's standing online-scoring policy (from
	// cfg.EvalPolicy), or nil when no policy is configured (nil ⇒ no scoring).
	// Wired from Config in a later task; declared here so the sampler/scorer
	// compile.
	evalPolicy *eval.Policy
	// evalJudge grades judge-scorer criteria, or nil when no judge model is
	// configured (nil ⇒ judge criteria fail closed).
	evalJudge eval.Judge

	// assertions bridges the caller's raw verified OIDC JWT from POST /sessions
	// (which has the HTTP request) to sessionWorkflow (which does not — the DBOS
	// workflow ctx is not the request ctx). Keyed by sessionID; stored BEFORE
	// RunWorkflow, loaded once in the workflow, deleted on workflow exit. The JWT
	// is a bearer secret: ephemeral, request-scoped, NEVER checkpointed/persisted
	// (not on turnInput). On a fresh-process replay the map is empty ⇒ callerJWT
	// "" ⇒ downstream OBO fails closed, by design.
	assertions sync.Map // sessionID -> raw caller JWT

	mu          sync.Mutex
	subscribers map[string][]chan WireEvent // sessionID -> live SSE subscribers
}

// buildRuntime constructs a fresh harness Runtime bound to sess. No compaction
// in M1 (durability correctness first).
func (m *Manager) buildRuntime(sess *session.Session, sessionID, actor string) (*hrt.Runtime, error) {
	deps := hrt.RuntimeDeps{}
	if m.cfg.KGFn != nil {
		deps.KGFn = func(model string) hrt.KnowledgeGraph { return m.cfg.KGFn(model, sessionID, actor) }
	}
	return hrt.BuildRuntime(
		deps,
		hrt.RuntimeInputs{
			Provider:   m.cfg.Provider,
			Tools:      m.cfg.Tools,
			Session:    sess,
			Compaction: nil,
		},
		m.cfg.Spec,
	)
}

// publish fans an event out to live subscribers and appends it to the store
// log for later re-attach/replay. Keyed by sessionID (== workflow id).
func (m *Manager) publish(sessionID string, ev WireEvent) {
	payload, _ := json.Marshal(ev)
	seq, err := m.st.AppendEvent(context.Background(), sessionID, ev.Type, payload)
	if err != nil {
		slog.Warn("append event failed", "session", sessionID, "type", ev.Type, "err", err)
	}
	ev.Seq = seq

	m.mu.Lock()
	subs := append([]chan WireEvent(nil), m.subscribers[sessionID]...)
	m.mu.Unlock()
	for _, ch := range subs {
		select {
		case ch <- ev:
		default: // drop on slow consumer; events are durable in the store
		}
	}
}

func (m *Manager) subscribe(sessionID string) (<-chan WireEvent, func()) {
	ch := make(chan WireEvent, 64)
	m.mu.Lock()
	m.subscribers[sessionID] = append(m.subscribers[sessionID], ch)
	m.mu.Unlock()
	return ch, func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		cur := m.subscribers[sessionID]
		for i, c := range cur {
			if c == ch {
				m.subscribers[sessionID] = append(cur[:i], cur[i+1:]...)
				break
			}
		}
	}
}

// observeTurn records one turn's metrics: outcome/duration/tokens plus one
// tool-call increment per tool_call entry the turn produced. The tool name
// lives inside the entry's Data payload (session.ToolCallData), not on the
// entry itself. Nil-safe via the obs nil-receiver no-ops, so Managers built
// without metrics are fine.
func (m *Manager) observeTurn(outcome string, dur time.Duration, usage *llm.Usage, entries []session.SessionEntry) {
	m.metrics.TurnObserved(outcome, dur, usage, m.price)
	for _, e := range entries {
		if e.Type != session.EntryTypeToolCall {
			continue
		}
		var td session.ToolCallData
		if err := json.Unmarshal(e.Data, &td); err != nil || td.Tool == "" {
			continue
		}
		m.metrics.ToolCallObserved(td.Tool)
	}
}

// parseLimits decodes RUNTIME_AGENT_LIMITS. "" ⇒ no limits (zero value).
func parseLimits(s string) (config.Limits, error) {
	var l config.Limits
	if s == "" {
		return l, nil
	}
	if err := json.Unmarshal([]byte(s), &l); err != nil {
		return config.Limits{}, fmt.Errorf("agentruntime: RUNTIME_AGENT_LIMITS: %w", err)
	}
	return l, nil
}

// effectiveMaxTurns resolves the turn cap: operator limit wins, then the
// author's Spec.MaxTurns, then the legacy fallback of 25.
func effectiveMaxTurns(l config.Limits, specMax int) int {
	if l.MaxTurns > 0 {
		return l.MaxTurns
	}
	if specMax > 0 {
		return specMax
	}
	return 25
}

// shouldCheckSessionTimeout gates the once-per-iteration session_timeout
// decision step: a limit must be configured AND the checkpointed workflow
// input must carry a session start time. Pre-upgrade in-flight sessions have
// a zero StartedAt (the field didn't exist when they were checkpointed), so
// they skip the check rather than measuring elapsed time from year 1.
func shouldCheckSessionTimeout(l config.Limits, startedAt time.Time) bool {
	return l.SessionTimeoutMS > 0 && !startedAt.IsZero()
}

// sessionTimedOut is the session_timeout verdict: the session's wall-clock
// elapsed time has reached or exceeded the configured limit. Pure, so the
// checkpointed decision step and its tests share one comparison.
func sessionTimedOut(elapsedMS, limitMS int64) bool {
	return elapsedMS >= limitMS
}

// isTurnTimeout classifies a finished turn as a turn-timeout hit. Harness
// v0.3.2's RunTurn returns a nil error on every path and reports failures on
// TurnResult instead (StopReason "aborted" for ctx cancellation, "error" for
// LLM stream errors), so the caller must decide FROM THE RESULT whether the
// per-turn deadline fired: the turn ended abnormally (aborted/error), the
// turn-scoped runCtx expired with DeadlineExceeded, and the enclosing stepCtx
// is still live (a step-level cancellation is a shutdown, not a limit).
func isTurnTimeout(stopReason string, runCtxErr, stepCtxErr error) bool {
	return (stopReason == "aborted" || stopReason == "error") &&
		runCtxErr == context.DeadlineExceeded &&
		stepCtxErr == nil
}

// failLimit terminates the session with the limit_exceeded policy outcome:
// status, client-facing error event naming the limit, and the metric. The
// workflow then returns normally — a breached session is a COMPLETED
// workflow, never a dangling/retried one.
func (m *Manager) failLimit(wfID, limit string, observed, configured int64) string {
	_ = m.st.SetSessionStatus(context.Background(), wfID, "limit_exceeded")
	m.publish(wfID, WireEvent{Type: "error", Err: breachMsg(limit, observed, configured)})
	m.metrics.LimitHitObserved(limit)
	// M3: classify the breach. A per-turn deadline (turn_timeout) becomes
	// terminalReason "limit:turn_timeout" so classify reports `timeout`; the
	// cumulative-budget limits report `limit_exceeded`. No policy criteria apply
	// to a truncated session ⇒ qualityFailed=false. Best-effort (never fatal).
	terminalReason := "limit_exceeded"
	if limit == "turn_timeout" {
		terminalReason = "limit:turn_timeout"
	}
	m.classifyAndPersist(wfID, "limit_exceeded", terminalReason, false, false)
	return "limit_exceeded"
}

// statusForReason maps a turn output to the transcript row's status column:
// "completed" for a clean finish, "error" for any other terminal reason
// (aborted/error), and "running" while the session is still iterating.
func statusForReason(out turnOutput) string {
	switch {
	case out.Done && out.Reason == "completed":
		return "completed"
	case out.Done:
		return "error"
	default:
		return "running"
	}
}

// sessionWorkflow is the durable per-session loop. Registered once; run with a
// stable workflow id == the session id so a process restart recovers exactly
// this workflow and replays completed turns from their checkpoints.
func (m *Manager) sessionWorkflow(ctx dbos.DBOSContext, in turnInput) (string, error) {
	wfID, _ := dbos.GetWorkflowID(ctx)

	// callerJWT is the caller's raw verified OIDC JWT, bridged out-of-band from
	// startSession via m.assertions (never on the checkpointed turnInput). Loaded
	// once here and deleted on workflow exit; empty on a fresh-process replay
	// (the map does not survive a restart) ⇒ downstream OBO fails closed.
	var callerJWT string
	if v, ok := m.assertions.Load(wfID); ok {
		callerJWT, _ = v.(string)
	}
	defer m.assertions.Delete(wfID)

	// canonical is the authoritative session, rebuilt turn-by-turn from each
	// turn step's checkpointed entries. It is mutated ONLY by applyEntries
	// below — never by RunTurn — so live execution and replay produce
	// identical state (RunTurn does not run on replay).
	canonical := session.NewSession(m.agentID, wfID)

	// Live-execution span only (NOT checkpointed): on DBOS replay the workflow
	// body re-runs but completed turn STEPS are skipped, so spans created here
	// and inside the step closure reflect only live work. A span is a live
	// concern, never durable state.
	_, wspan := obs.StartSpan(ctx, "session.workflow",
		obs.AgentAttr(m.agentID), obs.SessionAttr(wfID), obs.RequestIDAttr(in.RequestID))
	defer wspan.End()

	// Status/turn writes below run in the deterministic workflow body, so they
	// re-run on replay. Safe: SetSessionStatus is last-write-wins and
	// SetTurnCount sets the deterministic loop index (not an increment), so
	// both converge to identical values on recovery. Best-effort (operational
	// metadata, not the durability backbone) — errors are logged, not fatal.
	_ = m.st.SetSessionStatus(context.Background(), wfID, "running")

	// maxTurns bounds the durable loop so a misbehaving agent that never
	// stops emitting tool calls cannot spin the workflow forever (each
	// iteration checkpoints a step). Deterministic: derived from immutable
	// config (operator limit > author spec > legacy 25) and the loop counter,
	// so it behaves identically on replay.
	maxTurns := effectiveMaxTurns(m.limits, m.cfg.Spec.MaxTurns)

	// Decode the optional first-turn image once. It derives ONLY from `in` (the
	// checkpointed workflow input), so on DBOS replay `in` is re-supplied
	// identically and firstImages is reconstructed deterministically.
	var firstImages []llm.ImageContent
	if in.ImageB64 != "" {
		if raw, err := base64.StdEncoding.DecodeString(in.ImageB64); err == nil {
			mime := in.ImageMime
			if mime == "" {
				mime = "image/jpeg"
			}
			firstImages = []llm.ImageContent{{MimeType: mime, Data: raw}}
		} else {
			slog.Warn("session image decode failed; proceeding text-only", "session", wfID, "err", err)
		}
	}

	userMsg := in.UserMsg
	// totalTokens is the cumulative max_tokens budget, accumulated ONLY from
	// checkpointed turn outputs, so live execution and replay rebuild the
	// identical running total.
	totalTokens := 0
	// Accounting totals persisted to the sessions row each turn (idempotent
	// absolute-set on replay, like SetTurnCount). tokensAll is the FULL token
	// count (incl. cache); costUSD accumulates priced turns only.
	var tokensAll int64
	var costUSD float64
	// toolErrored OR-accumulates across turns: any turn whose entries carry a
	// tool_result with IsError marks the session for M3 tool_error classification.
	var toolErrored bool
	for turn := 0; ; turn++ {
		if turn >= maxTurns {
			return m.failLimit(wfID, "max_turns", int64(turn), int64(maxTurns)), nil
		}
		// max_tokens: pure arithmetic over checkpointed per-turn usage —
		// deterministic on replay by construction.
		if m.limits.MaxTokens > 0 && totalTokens >= m.limits.MaxTokens {
			return m.failLimit(wfID, "max_tokens", int64(totalTokens), int64(m.limits.MaxTokens)), nil
		}
		// session_timeout: the clock is read ONCE per live iteration inside a
		// checkpointed decision step; replay gets the recorded verdict and
		// never consults the clock.
		if shouldCheckSessionTimeout(m.limits, in.StartedAt) {
			chk, cerr := dbos.RunAsStep(ctx, func(context.Context) (timeoutCheck, error) {
				elapsed := time.Since(in.StartedAt).Milliseconds()
				return timeoutCheck{ElapsedMS: elapsed, Exceeded: sessionTimedOut(elapsed, m.limits.SessionTimeoutMS)}, nil
			})
			if cerr != nil {
				// Fail-open: a failed check must not kill the session, but it
				// must not be silent either.
				slog.Warn("session timeout check failed", "session", wfID, "err", cerr)
			} else if chk.Exceeded {
				return m.failLimit(wfID, "session_timeout", chk.ElapsedMS, m.limits.SessionTimeoutMS), nil
			}
		}
		prior := canonical.Entries() // snapshot of history for this turn

		out, stepErr := dbos.RunAsStep(ctx, func(stepCtx context.Context) (turnOutput, error) {
			// Throwaway per-turn session seeded with prior history. RunTurn
			// mutates THIS, not canonical, so canonical is never double-written.
			turnSess := session.NewSession(m.agentID, wfID)
			for _, e := range prior {
				turnSess.Append(e)
			}
			rt, err := m.buildRuntime(turnSess, wfID, in.Subject)
			if err != nil {
				return turnOutput{}, err
			}
			// Images apply on the FIRST turn only. firstImages and turn are
			// captured from the enclosing deterministic scope; both are
			// reconstructed identically on replay (firstImages from `in`, turn
			// from the loop index), so this is replay-safe.
			var images []llm.ImageContent
			if turn == 0 {
				images = firstImages
			}
			// Metrics + the per-turn log line live INSIDE this closure on
			// purpose: DBOS skips completed steps on crash-recovery replay
			// (returning the checkpointed turnOutput without re-running this
			// function), so everything here executes once per real turn —
			// at-least-once, duplicated only if a crash lands between RunTurn
			// completing and the step checkpoint committing.
			turnCtx, tspan := obs.StartSpan(stepCtx, "agent.turn", obs.TurnAttr(turn))
			// runCtx bounds ONE turn when a turn_timeout is configured. Derived
			// from stepCtx so a step cancellation still propagates; the
			// deadline-vs-cancel distinction is resolved below.
			runCtx := stepCtx
			if m.limits.TurnTimeoutMS > 0 {
				var cancel context.CancelFunc
				runCtx, cancel = context.WithTimeout(stepCtx, time.Duration(m.limits.TurnTimeoutMS)*time.Millisecond)
				defer cancel()
			}
			start := time.Now()
			actorCtx := memory.WithActor(runCtx, in.Subject)
			actorCtx = identity.WithAssertion(actorCtx, callerJWT) // caller JWT for OBO; no-ops when "" (e.g. replay); never checkpointed
			tr, terr := rt.RunTurn(actorCtx, userMsg, images, nil) // headless (emit=nil); actor on ctx for the memory tool + recall path
			elapsed := time.Since(start)
			// Harness v0.3.2 contract: RunTurn returns a nil error on EVERY
			// path — failures ride TurnResult (StopReason "aborted" on ctx
			// cancellation pre-check/tool dispatch, "error" on LLM stream
			// errors, with details in TurnResult.Err). So the RESULT is the
			// primary classification path for a turn timeout; the terr branch
			// below is defense-in-depth for future harness versions only.
			if terr == nil && isTurnTimeout(tr.StopReason, runCtx.Err(), stepCtx.Err()) {
				// Turn timeout: checkpoint the verdict (NOT an error) so
				// replay reproduces it; Entries nil ⇒ partial turn work is
				// never applied to the canonical session, and Usage nil ⇒ the
				// aborted turn never counts toward the token budget.
				tspan.SetAttributes(obs.OutcomeAttr("turn_timeout"))
				tspan.End()
				m.observeTurn("turn_timeout", elapsed, nil, nil)
				return turnOutput{Done: true, Reason: "limit:turn_timeout"}, nil
			}
			if terr != nil {
				// Defense-in-depth: harness v0.3.2 never returns a non-nil
				// error from RunTurn (see contract note above), but a future
				// version might. Classify a deadline hit the same way.
				if isTurnTimeout("aborted", runCtx.Err(), stepCtx.Err()) {
					tspan.SetAttributes(obs.OutcomeAttr("turn_timeout"))
					tspan.End()
					m.observeTurn("turn_timeout", elapsed, nil, nil)
					return turnOutput{Done: true, Reason: "limit:turn_timeout"}, nil
				}
				tspan.SetAttributes(obs.OutcomeAttr("error"))
				tspan.End()
				m.observeTurn("error", elapsed, nil, nil)
				slog.Warn("turn failed", "agent", m.agentID, "session", wfID,
					"turn", turn, "request_id", in.RequestID, "err", terr)
				return turnOutput{}, terr
			}
			tspan.SetAttributes(obs.OutcomeAttr(tr.StopReason))
			for _, e := range tr.Entries {
				if e.Type == session.EntryTypeToolCall {
					var td session.ToolCallData
					if err := json.Unmarshal(e.Data, &td); err == nil && td.Tool != "" {
						_, toolSpan := obs.StartSpan(turnCtx, "tool.call", obs.ToolAttr(td.Tool))
						toolSpan.End()
					}
				}
			}
			tspan.End()
			m.observeTurn(tr.StopReason, elapsed, tr.Usage, tr.Entries)
			slog.Info("turn",
				"agent", m.agentID,
				"session", wfID,
				"turn", turn,
				"reason", tr.StopReason,
				"request_id", in.RequestID)
			return turnOutput{Done: tr.Done, Reason: tr.StopReason, Entries: tr.Entries, Usage: tr.Usage}, nil
		})
		if stepErr != nil {
			_ = m.st.SetSessionStatus(context.Background(), wfID, "error")
			m.publish(wfID, WireEvent{Type: "error", Err: stepErr.Error()})
			return "error", stepErr
		}
		// Turn-timeout verdict: classified in the workflow body from the
		// checkpointed output (replay-deterministic). Entries are nil, so the
		// timed-out turn's partial work never reaches canonical.
		if out.Reason == "limit:turn_timeout" {
			return m.failLimit(wfID, "turn_timeout",
				m.limits.TurnTimeoutMS, m.limits.TurnTimeoutMS), nil
		}
		totalTokens += sumTokens(out.Usage)
		tokensAll += sumAllTokens(out.Usage)
		if m.price != nil {
			costUSD += m.price.Cost(out.Usage)
		}

		applyEntries(canonical, out.Entries) // SOLE mutator of canonical
		if entriesHaveToolError(out.Entries) {
			toolErrored = true
		}
		// Capture the complete turn transcript (best-effort, off the durable
		// path; a marshal/append failure is logged, never fatal). Indexed by the
		// 0-based loop var `turn` (NOT turn+1, which SetTurnCount stores as a
		// count). AppendTranscript is idempotent on (session, turn), so a DBOS
		// replay re-writes the identical row.
		if entriesJSON, mErr := json.Marshal(out.Entries); mErr == nil {
			if tErr := m.st.AppendTranscript(context.Background(), wfID, turn, in.Tenant, in.Subject, entriesJSON, out.Reason, statusForReason(out)); tErr != nil {
				slog.Warn("append transcript failed", "session", wfID, "turn", turn, "err", tErr)
			}
		} else {
			slog.Warn("marshal transcript entries failed", "session", wfID, "turn", turn, "err", mErr)
		}
		_ = m.st.SetTurnCount(context.Background(), wfID, turn+1)
		if err := m.st.SetSessionUsage(context.Background(), wfID, tokensAll, costUSD); err != nil {
			slog.Warn("set session usage failed", "session", wfID, "err", err)
		}
		for _, ev := range publishableEvents(out.Entries) {
			m.publish(wfID, ev)
		}
		if out.Done {
			// RunTurn returns Done=true for "completed", "aborted", and
			// "error" terminal reasons. Only "completed" is a clean finish;
			// surface the others as an error event so clients aren't told a
			// turn that aborted/errored succeeded.
			if out.Reason == "completed" {
				_ = m.st.SetSessionStatus(context.Background(), wfID, "completed")
				m.publish(wfID, WireEvent{Type: "done"})
			} else {
				_ = m.st.SetSessionStatus(context.Background(), wfID, "error")
				m.publish(wfID, WireEvent{Type: "error", Err: "turn ended: " + out.Reason})
			}
			// Online sampling (P3.1 M2): deterministic sample of finished sessions
			// scored against the agent's eval policy, in a background goroutine off
			// the turn path. Deterministic decision so a DBOS replay of this block
			// does not re-sample differently. Replay caveat: the terminal block
			// re-runs on recovery, so scoreSession may fire again — acceptable
			// because PutOnlineResult is an idempotent upsert (re-scoring overwrites
			// the same rows); the metric may double-count, the same tolerance the
			// per-turn metrics carry. Scores the clean/error out.Done path only;
			// limit_exceeded (via failLimit) is a truncated session with no
			// meaningful final output — captured per-turn but NOT scored.
			// Determine the terminal status this block set (mirrors the
			// SetSessionStatus calls just above): "completed" for a clean finish,
			// "error" for aborted/error terminal reasons.
			status := "completed"
			if out.Reason != "completed" {
				status = "error"
			}
			// M3 failure classification. When a policy is sampled, fold
			// classification into the SCORING goroutine's tail so quality_fail
			// reads the criteria results that goroutine writes (no race). Otherwise
			// classify inline with qualityFailed=false (no criteria to fail).
			if m.evalPolicy != nil && sampled(wfID, m.evalPolicy.SampleRate) {
				entries := out.Entries
				tenant, actor := in.Tenant, in.Subject
				go m.scoreSession(wfID, tenant, actor, status, out.Reason, toolErrored, entries)
			} else {
				m.classifyAndPersist(wfID, status, out.Reason, toolErrored, false)
			}
			return out.Reason, nil
		}
		userMsg = ""
	}
}

// startSession creates a store session row and launches the durable workflow
// with the session id as the stable DBOS workflow id. requestID is the
// originating POST's X-Request-ID, carried into the checkpointed workflow
// input for log correlation.
func (m *Manager) startSession(ctx context.Context, userMsg, imageB64, imageMime, requestID, subject, tenant, role, assertion string) (string, error) {
	sessionID, err := m.st.CreateSession(ctx, m.agentID, m.replica)
	if err != nil {
		return "", err
	}
	// Bridge the caller JWT to sessionWorkflow out-of-band, BEFORE RunWorkflow so
	// the workflow can Load it. It is NEVER placed on turnInput (checkpointed);
	// it is an ephemeral bearer secret deleted when the workflow exits.
	if assertion != "" {
		m.assertions.Store(sessionID, assertion)
	}
	in := turnInput{UserMsg: userMsg, ImageB64: imageB64, ImageMime: imageMime,
		RequestID: requestID, StartedAt: time.Now().UTC(),
		Subject: subject, Tenant: tenant, Role: role}
	if _, err := dbos.RunWorkflow(m.dbosCtx, m.sessionWorkflow, in, dbos.WithWorkflowID(sessionID)); err != nil {
		m.assertions.Delete(sessionID) // workflow never started ⇒ its defer won't clean up; don't leak the secret
		return "", err
	}
	return sessionID, nil
}

// envBool reports whether key is set to a truthy value (1/true/yes/on,
// case-insensitive, surrounding spaces ignored).
func envBool(key string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// Serve validates config, opens the store, launches DBOS (running recovery for
// any pending workflows), then serves the agent contract until ctx is cancelled.
//
// Operator-injected parameters come from the environment the control plane sets
// on the subprocess, not from Config: RUNTIME_PG_DSN (the DBOS system database +
// control-plane store DSN) and RUNTIME_LISTEN_ADDR (the HTTP bind address). This
// keeps Config a pure agent-author surface — a builder never handles them.
func Serve(ctx context.Context, cfg Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	pgDSN := os.Getenv("RUNTIME_PG_DSN")
	if pgDSN == "" {
		return errors.New("agentruntime: RUNTIME_PG_DSN is not set")
	}
	listenAddr := os.Getenv("RUNTIME_LISTEN_ADDR")
	if listenAddr == "" {
		return errors.New("agentruntime: RUNTIME_LISTEN_ADDR is not set")
	}

	traceShutdown, terr := obs.InitTracing(ctx, cfg.Spec.ID)
	if terr != nil {
		slog.Warn("agentd tracing init failed; continuing without traces", "err", terr)
	}
	defer func() {
		fctx, fcancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer fcancel()
		_ = traceShutdown(fctx)
	}()

	st, err := store.NewPGStore(ctx, pgDSN)
	if err != nil {
		return err
	}
	defer st.Close()

	dctx, err := dbos.NewDBOSContext(ctx, dbos.Config{
		AppName:     cfg.Spec.ID,
		DatabaseURL: pgDSN,
	})
	if err != nil {
		return err
	}

	replica, _ := strconv.Atoi(os.Getenv("RUNTIME_AGENT_REPLICA")) // "" or bad ⇒ 0
	limits, err := parseLimits(os.Getenv("RUNTIME_AGENT_LIMITS"))
	if err != nil {
		return err // fail fast: a malformed operator value must not silently mean "unlimited"
	}
	m := &Manager{
		agentID:           cfg.Spec.ID,
		cfg:               cfg,
		dbosCtx:           dctx,
		st:                st,
		metrics:           obs.NewAgentMetrics(cfg.Spec.ID, os.Getenv("RUNTIME_AGENT_TENANT"), cfg.Spec.Model),
		price:             cfg.Price,
		authToken:         os.Getenv("RUNTIME_AGENT_AUTH_TOKEN"),
		replica:           replica,
		limits:            limits,
		subjectForwarding: envBool("RUNTIME_SUBJECT_FORWARDING"),
		evalPolicy:        cfg.EvalPolicy,
		evalJudge:         cfg.EvalJudge,
		subscribers:       map[string][]chan WireEvent{},
	}
	if m.price == nil {
		slog.Warn("agent model has no price entry; cost will not be metered (tokens still recorded)",
			"agent", cfg.Spec.ID, "model", cfg.Spec.Model)
	}

	// Launch the memory GC reaper (opt-out; nil when disabled or no memory).
	// Bound to the Serve ctx so it stops on shutdown; reports reaped rows to
	// the agent metrics registry (this is where metrics exist — the seam that
	// also fixes M1's post-wireMemory metric-ordering gap).
	if cfg.StartMemoryGC != nil {
		cfg.StartMemoryGC(ctx, m.metrics.MemoryGCReaped)
	}

	// Wire the memory write metrics (summary + episode) now that AgentMetrics
	// exists — same post-wireMemory seam as the GC metric above.
	if cfg.SetMemoryMetrics != nil {
		cfg.SetMemoryMetrics(m.metrics.SummaryWrite, m.metrics.EpisodeWrite)
	}

	// Register BEFORE Launch so recovery can find the workflow.
	dbos.RegisterWorkflow(dctx, m.sessionWorkflow)
	if err := dbos.Launch(dctx); err != nil {
		return err
	}
	defer dbos.Shutdown(dctx, 10*time.Second)

	srv := &http.Server{Addr: listenAddr, Handler: m.handler()}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.ListenAndServe() }()

	select {
	case <-ctx.Done():
		// Drain HTTP first (stop accepting + finish in-flight handlers),
		// THEN let the deferred dbos.Shutdown and st.Close run.
		shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
		return nil
	case err := <-serveErr:
		if err != nil && err != http.ErrServerClosed {
			return err
		}
		return nil
	}
}
