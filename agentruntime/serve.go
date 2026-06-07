package agentruntime

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	hrt "github.com/sausheong/harness/runtime"
	"github.com/sausheong/harness/session"
	"github.com/sausheong/runtime/internal/store"
)

// Manager owns per-session durable workflows and event fan-out.
type Manager struct {
	agentID string
	cfg     Config
	dbosCtx dbos.DBOSContext
	st      store.Store

	mu          sync.Mutex
	subscribers map[string][]chan WireEvent // sessionID -> live SSE subscribers
}

// buildRuntime constructs a fresh harness Runtime bound to sess. No compaction
// in M1 (durability correctness first).
func (m *Manager) buildRuntime(sess *session.Session) (*hrt.Runtime, error) {
	return hrt.BuildRuntime(
		hrt.RuntimeDeps{},
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

	// maxTurns bounds the durable loop so a misbehaving agent that never
	// stops emitting tool calls cannot spin the workflow forever (each
	// iteration checkpoints a step). Deterministic: derived from config and
	// the loop counter, so it behaves identically on replay. 0 ⇒ 25 (matches
	// harness's RunTurn/Run default).
	maxTurns := m.cfg.Spec.MaxTurns
	if maxTurns <= 0 {
		maxTurns = 25
	}

	userMsg := in.UserMsg
	for turn := 0; ; turn++ {
		if turn >= maxTurns {
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
			tr, terr := rt.RunTurn(stepCtx, userMsg, nil, nil) // headless (emit=nil)
			if terr != nil {
				return turnOutput{}, terr
			}
			return turnOutput{Done: tr.Done, Reason: tr.StopReason, Entries: tr.Entries}, nil
		})
		if stepErr != nil {
			m.publish(wfID, WireEvent{Type: "error", Err: stepErr.Error()})
			return "error", stepErr
		}

		applyEntries(canonical, out.Entries) // SOLE mutator of canonical
		for _, ev := range publishableEvents(out.Entries) {
			m.publish(wfID, ev)
		}
		if out.Done {
			// RunTurn returns Done=true for "completed", "aborted", and
			// "error" terminal reasons. Only "completed" is a clean finish;
			// surface the others as an error event so clients aren't told a
			// turn that aborted/errored succeeded.
			if out.Reason == "completed" {
				m.publish(wfID, WireEvent{Type: "done"})
			} else {
				m.publish(wfID, WireEvent{Type: "error", Err: "turn ended: " + out.Reason})
			}
			return out.Reason, nil
		}
		userMsg = ""
	}
}

// startSession creates a store session row and launches the durable workflow
// with the session id as the stable DBOS workflow id.
func (m *Manager) startSession(ctx context.Context, userMsg string) (string, error) {
	sessionID, err := m.st.CreateSession(ctx, m.agentID)
	if err != nil {
		return "", err
	}
	if _, err := dbos.RunWorkflow(m.dbosCtx, m.sessionWorkflow, turnInput{UserMsg: userMsg}, dbos.WithWorkflowID(sessionID)); err != nil {
		return "", err
	}
	return sessionID, nil
}

// Serve validates config, opens the store, launches DBOS (running recovery for
// any pending workflows), then serves the agent contract until ctx is cancelled.
func Serve(ctx context.Context, cfg Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	st, err := store.NewPGStore(ctx, cfg.PostgresDSN)
	if err != nil {
		return err
	}
	defer st.Close()

	dctx, err := dbos.NewDBOSContext(ctx, dbos.Config{
		AppName:     cfg.Spec.ID,
		DatabaseURL: cfg.PostgresDSN,
	})
	if err != nil {
		return err
	}

	m := &Manager{
		agentID:     cfg.Spec.ID,
		cfg:         cfg,
		dbosCtx:     dctx,
		st:          st,
		subscribers: map[string][]chan WireEvent{},
	}

	// Register BEFORE Launch so recovery can find the workflow.
	dbos.RegisterWorkflow(dctx, m.sessionWorkflow)
	if err := dbos.Launch(dctx); err != nil {
		return err
	}
	defer dbos.Shutdown(dctx, 10*time.Second)

	srv := &http.Server{Addr: cfg.ListenAddr, Handler: m.newMux()}
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
