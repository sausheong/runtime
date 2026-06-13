package agentruntime

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/sausheong/harness/llm"
	hrt "github.com/sausheong/harness/runtime"
	"github.com/sausheong/harness/session"
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
	// authToken, when non-empty, requires every inbound request to carry
	// Authorization: Bearer <authToken>. "" ⇒ no auth (local/loopback agents).
	authToken string

	mu          sync.Mutex
	subscribers map[string][]chan WireEvent // sessionID -> live SSE subscribers
}

// buildRuntime constructs a fresh harness Runtime bound to sess. No compaction
// in M1 (durability correctness first).
func (m *Manager) buildRuntime(sess *session.Session) (*hrt.Runtime, error) {
	return hrt.BuildRuntime(
		hrt.RuntimeDeps{KGFn: m.cfg.KGFn},
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
	m.metrics.TurnObserved(outcome, dur, usage)
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

// sessionWorkflow is the durable per-session loop. Registered once; run with a
// stable workflow id == the session id so a process restart recovers exactly
// this workflow and replays completed turns from their checkpoints.
func (m *Manager) sessionWorkflow(ctx dbos.DBOSContext, in turnInput) (string, error) {
	wfID, _ := dbos.GetWorkflowID(ctx)

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
	// iteration checkpoints a step). Deterministic: derived from config and
	// the loop counter, so it behaves identically on replay. 0 ⇒ 25 (matches
	// harness's RunTurn/Run default).
	maxTurns := m.cfg.Spec.MaxTurns
	if maxTurns <= 0 {
		maxTurns = 25
	}

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
	for turn := 0; ; turn++ {
		if turn >= maxTurns {
			_ = m.st.SetSessionStatus(context.Background(), wfID, "error")
			m.publish(wfID, WireEvent{Type: "error", Err: "agent exceeded maximum turns"})
			return "error", nil
		}
		prior := canonical.Entries() // snapshot of history for this turn

		out, stepErr := dbos.RunAsStep(ctx, func(stepCtx context.Context) (turnOutput, error) {
			// Throwaway per-turn session seeded with prior history. RunTurn
			// mutates THIS, not canonical, so canonical is never double-written.
			turnSess := session.NewSession(m.agentID, wfID)
			for _, e := range prior {
				turnSess.Append(e)
			}
			rt, err := m.buildRuntime(turnSess)
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
			start := time.Now()
			tr, terr := rt.RunTurn(stepCtx, userMsg, images, nil) // headless (emit=nil)
			elapsed := time.Since(start)
			if terr != nil {
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
			return turnOutput{Done: tr.Done, Reason: tr.StopReason, Entries: tr.Entries}, nil
		})
		if stepErr != nil {
			_ = m.st.SetSessionStatus(context.Background(), wfID, "error")
			m.publish(wfID, WireEvent{Type: "error", Err: stepErr.Error()})
			return "error", stepErr
		}

		applyEntries(canonical, out.Entries) // SOLE mutator of canonical
		_ = m.st.SetTurnCount(context.Background(), wfID, turn+1)
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
			return out.Reason, nil
		}
		userMsg = ""
	}
}

// startSession creates a store session row and launches the durable workflow
// with the session id as the stable DBOS workflow id. requestID is the
// originating POST's X-Request-ID, carried into the checkpointed workflow
// input for log correlation.
func (m *Manager) startSession(ctx context.Context, userMsg, imageB64, imageMime, requestID string) (string, error) {
	sessionID, err := m.st.CreateSession(ctx, m.agentID)
	if err != nil {
		return "", err
	}
	in := turnInput{UserMsg: userMsg, ImageB64: imageB64, ImageMime: imageMime, RequestID: requestID}
	if _, err := dbos.RunWorkflow(m.dbosCtx, m.sessionWorkflow, in, dbos.WithWorkflowID(sessionID)); err != nil {
		return "", err
	}
	return sessionID, nil
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

	m := &Manager{
		agentID:     cfg.Spec.ID,
		cfg:         cfg,
		dbosCtx:     dctx,
		st:          st,
		metrics:     obs.NewAgentMetrics(cfg.Spec.ID),
		authToken:   os.Getenv("RUNTIME_AGENT_AUTH_TOKEN"),
		subscribers: map[string][]chan WireEvent{},
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
