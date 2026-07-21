package store

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
)

//go:embed schema.sql
var schemaSQL string

type pgStore struct{ db *sql.DB }

func NewPGStore(ctx context.Context, dsn string) (Store, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, err
	}
	if err := db.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	if err := ApplyDDLLocked(ctx, db, schemaSQL); err != nil {
		return nil, err
	}
	return &pgStore{db: db}, nil
}

// schemaLockKey is the shared advisory-lock key all runtime processes use to
// serialize DDL. Arbitrary constant ("runtime" packed into an int8).
const schemaLockKey = 0x72756e74696d65

// ApplyDDLLocked runs the given DDL while holding a transaction-scoped Postgres
// advisory lock, so concurrently-starting processes apply DDL one at a time.
//
// `CREATE TABLE IF NOT EXISTS` is NOT atomic against a concurrent creator in
// Postgres — two processes racing can raise a duplicate pg_class/pg_type error
// (SQLSTATE 23505/42P07). A transaction-scoped lock (pg_advisory_xact_lock)
// binds to the single connection the tx holds (database/sql pools connections,
// so a session-scoped lock could unlock on a different connection) and
// auto-releases on commit/rollback. All callers share schemaLockKey, so the
// store schema and any caller-owned tables (e.g. agentd's marker table)
// serialize against each other on cold start.
func ApplyDDLLocked(ctx context.Context, db *sql.DB, ddl string) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin ddl tx: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, schemaLockKey); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("acquire schema lock: %w", err)
	}
	if _, err := tx.ExecContext(ctx, ddl); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("apply ddl: %w", err)
	}
	if err := tx.Commit(); err != nil { // releases the advisory lock
		return fmt.Errorf("commit ddl tx: %w", err)
	}
	return nil
}

func (p *pgStore) CreateSession(ctx context.Context, agentID string, replica int) (string, error) {
	id := "ses-" + uuid.NewString()
	_, err := p.db.ExecContext(ctx,
		`INSERT INTO sessions (id, agent_id, workflow_id, status, replica) VALUES ($1,$2,$1,'created',$3)`,
		id, agentID, replica)
	return id, err
}

func (p *pgStore) SessionReplica(ctx context.Context, id string) (int, error) {
	var r int
	err := p.db.QueryRowContext(ctx, `SELECT replica FROM sessions WHERE id=$1`, id).Scan(&r)
	if err == sql.ErrNoRows {
		return 0, fmt.Errorf("session %q not found", id)
	}
	return r, err
}

func (p *pgStore) ActiveSessionsByReplica(ctx context.Context, agentID string) (map[int]int, error) {
	rows, err := p.db.QueryContext(ctx,
		`SELECT replica, count(*) FROM sessions
		 WHERE agent_id=$1 AND status NOT IN ('completed','error','limit_exceeded')
		 GROUP BY replica`, agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int]int{}
	for rows.Next() {
		var replica, n int
		if err := rows.Scan(&replica, &n); err != nil {
			return nil, err
		}
		out[replica] = n
	}
	return out, rows.Err()
}

func (p *pgStore) ListSessions(ctx context.Context, agentID string) ([]SessionRow, error) {
	rows, err := p.db.QueryContext(ctx,
		`SELECT id, agent_id, workflow_id, status, turn_count, replica, tokens_total, cost_usd, failure_category FROM sessions WHERE agent_id=$1 ORDER BY created_at DESC`,
		agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SessionRow
	for rows.Next() {
		var s SessionRow
		if err := rows.Scan(&s.ID, &s.AgentID, &s.WorkflowID, &s.Status, &s.TurnCount, &s.Replica, &s.TokensTotal, &s.CostUSD, &s.FailureCategory); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (p *pgStore) SetTurnCount(ctx context.Context, id string, n int) error {
	_, err := p.db.ExecContext(ctx,
		`UPDATE sessions SET turn_count = $2, last_active_at = now() WHERE id=$1`, id, n)
	return err
}

func (p *pgStore) SetSessionUsage(ctx context.Context, id string, tokens int64, cost float64) error {
	_, err := p.db.ExecContext(ctx,
		`UPDATE sessions SET tokens_total = $2, cost_usd = $3, last_active_at = now() WHERE id=$1`,
		id, tokens, cost)
	return err
}

func (p *pgStore) SetFailureCategory(ctx context.Context, id, category string) error {
	_, err := p.db.ExecContext(ctx,
		`UPDATE sessions SET failure_category = $2, last_active_at = now() WHERE id=$1`,
		id, category)
	return err
}

func (p *pgStore) FailureBreakdownByAgent(ctx context.Context, agentID string, since time.Time) (map[string]int, error) {
	q := `SELECT failure_category, count(*) FROM sessions
	       WHERE agent_id=$1 AND failure_category <> ''`
	args := []any{agentID}
	if !since.IsZero() {
		q += ` AND created_at >= $2`
		args = append(args, since)
	}
	q += ` GROUP BY failure_category`
	rows, err := p.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var (
			cat string
			n   int
		)
		if err := rows.Scan(&cat, &n); err != nil {
			return nil, err
		}
		out[cat] = n
	}
	return out, rows.Err()
}

func (p *pgStore) GetSession(ctx context.Context, id string) (SessionRow, error) {
	var s SessionRow
	err := p.db.QueryRowContext(ctx,
		`SELECT id, agent_id, workflow_id, status, turn_count, replica, tokens_total, cost_usd, failure_category FROM sessions WHERE id=$1`, id).
		Scan(&s.ID, &s.AgentID, &s.WorkflowID, &s.Status, &s.TurnCount, &s.Replica, &s.TokensTotal, &s.CostUSD, &s.FailureCategory)
	if err == sql.ErrNoRows {
		return SessionRow{}, fmt.Errorf("session %q not found", id)
	}
	return s, err
}

func (p *pgStore) SetSessionStatus(ctx context.Context, id, status string) error {
	_, err := p.db.ExecContext(ctx,
		`UPDATE sessions SET status=$2, last_active_at=now() WHERE id=$1`, id, status)
	return err
}

func (p *pgStore) AppendEvent(ctx context.Context, sessionID, typ string, payload []byte) (int64, error) {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var next int64
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(seq),0)+1 FROM session_events WHERE session_id=$1`, sessionID).Scan(&next); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO session_events (session_id, seq, type, payload) VALUES ($1,$2,$3,$4)`,
		sessionID, next, typ, payload); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return next, nil
}

func (p *pgStore) EventsSince(ctx context.Context, sessionID string, afterSeq int64) ([]Event, error) {
	rows, err := p.db.QueryContext(ctx,
		`SELECT seq, type, payload FROM session_events WHERE session_id=$1 AND seq>$2 ORDER BY seq`,
		sessionID, afterSeq)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.Seq, &e.Type, &e.Payload); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (p *pgStore) AppendTranscript(ctx context.Context, sessionID string, turn int, tenant, actor string, entries []byte, stopReason, status string) error {
	_, err := p.db.ExecContext(ctx,
		`INSERT INTO session_transcripts (session_id, turn_index, tenant, actor_id, entries, stop_reason, status)
		 VALUES ($1,$2,$3,$4,$5::jsonb,$6,$7)
		 ON CONFLICT (session_id, turn_index) DO UPDATE SET
		   entries=EXCLUDED.entries, tenant=EXCLUDED.tenant, actor_id=EXCLUDED.actor_id,
		   stop_reason=EXCLUDED.stop_reason, status=EXCLUDED.status`,
		sessionID, turn, tenant, actor, string(entries), stopReason, status)
	if err != nil {
		return fmt.Errorf("append transcript (%s turn %d): %w", sessionID, turn, err)
	}
	return nil
}

func (p *pgStore) PutOnlineResult(ctx context.Context, sessionID, criterion, tenant, actor, scorer string, passed bool, detail string) error {
	_, err := p.db.ExecContext(ctx,
		`INSERT INTO online_eval_results (session_id, criterion_name, tenant, actor_id, scorer, passed, detail)
		 VALUES ($1,$2,$3,$4,$5,$6,$7)
		 ON CONFLICT (session_id, criterion_name) DO UPDATE SET
		   passed=EXCLUDED.passed, detail=EXCLUDED.detail, scorer=EXCLUDED.scorer,
		   tenant=EXCLUDED.tenant, actor_id=EXCLUDED.actor_id`,
		sessionID, criterion, tenant, actor, scorer, passed, detail)
	if err != nil {
		return fmt.Errorf("put online result (%s %s): %w", sessionID, criterion, err)
	}
	return nil
}

func (p *pgStore) ListOnlineResults(ctx context.Context, sessionID string) ([]OnlineResult, error) {
	rows, err := p.db.QueryContext(ctx,
		`SELECT session_id, criterion_name, tenant, actor_id, scorer, passed, detail, created_at
		 FROM online_eval_results WHERE session_id=$1 ORDER BY criterion_name`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("list online results (%s): %w", sessionID, err)
	}
	defer rows.Close()
	return scanOnlineResults(rows)
}

func (p *pgStore) ListOnlineResultsByTenant(ctx context.Context, tenant string, limit int) ([]OnlineResult, error) {
	rows, err := p.db.QueryContext(ctx,
		`SELECT session_id, criterion_name, tenant, actor_id, scorer, passed, detail, created_at
		 FROM online_eval_results WHERE tenant=$1 ORDER BY created_at DESC LIMIT $2`, tenant, limit)
	if err != nil {
		return nil, fmt.Errorf("list online results by tenant (%s): %w", tenant, err)
	}
	defer rows.Close()
	return scanOnlineResults(rows)
}

func scanOnlineResults(rows *sql.Rows) ([]OnlineResult, error) {
	var out []OnlineResult
	for rows.Next() {
		var r OnlineResult
		if err := rows.Scan(&r.SessionID, &r.Criterion, &r.Tenant, &r.Actor, &r.Scorer, &r.Passed, &r.Detail, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (p *pgStore) Close() error { return p.db.Close() }
