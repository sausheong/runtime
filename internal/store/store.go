package store

import (
	"context"
	"time"
)

type SessionRow struct {
	ID          string
	AgentID     string
	WorkflowID  string
	Status      string // created | running | completed | error | limit_exceeded
	TurnCount   int
	Replica     int
	TokensTotal int64   // cumulative input+output+cache tokens (accounting; wider than the budget sum)
	CostUSD     float64 // cumulative dollar cost (priced turns only); metering-grade float
}

type Event struct {
	Seq     int64
	Type    string
	Payload []byte
}

// OnlineResult is one online-eval-criterion outcome for a session, written by
// agentd at the turn seam. Keyed by (SessionID, Criterion): re-scoring the same
// criterion for a session upserts (replay-safe).
type OnlineResult struct {
	SessionID string
	Criterion string
	Tenant    string
	Actor     string
	Scorer    string
	Passed    bool
	Detail    string
	CreatedAt time.Time
}

type Store interface {
	CreateSession(ctx context.Context, agentID string, replica int) (string, error)
	GetSession(ctx context.Context, id string) (SessionRow, error)
	ListSessions(ctx context.Context, agentID string) ([]SessionRow, error)
	SessionReplica(ctx context.Context, id string) (int, error)
	// ActiveSessionsByReplica returns replica index → count of NON-terminal
	// sessions for the agent (terminal = completed|error|limit_exceeded). The
	// autoscaler's load read. Replicas with zero active sessions may be absent
	// from the map.
	ActiveSessionsByReplica(ctx context.Context, agentID string) (map[int]int, error)
	SetSessionStatus(ctx context.Context, id, status string) error
	SetTurnCount(ctx context.Context, id string, n int) error
	// SetSessionUsage records the session's cumulative token/cost totals as an
	// ABSOLUTE set (not an increment), mirroring SetTurnCount: the caller passes
	// the recomputed running total each turn, so live execution and DBOS replay
	// converge to the same value. Best-effort operational metadata.
	SetSessionUsage(ctx context.Context, id string, tokens int64, cost float64) error
	AppendEvent(ctx context.Context, sessionID, typ string, payload []byte) (int64, error)
	EventsSince(ctx context.Context, sessionID string, afterSeq int64) ([]Event, error)
	// AppendTranscript records the entries for one turn. Idempotent on
	// (sessionID, turn): re-appending the same turn upserts (replay-safe).
	// entries is raw JSON stored as JSONB.
	AppendTranscript(ctx context.Context, sessionID string, turn int, tenant, actor string, entries []byte, stopReason, status string) error
	// PutOnlineResult records one online-eval-criterion outcome. Idempotent on
	// (sessionID, criterion): re-scoring the same criterion upserts.
	PutOnlineResult(ctx context.Context, sessionID, criterion, tenant, actor, scorer string, passed bool, detail string) error
	// ListOnlineResults returns all results for a session, ordered by criterion.
	ListOnlineResults(ctx context.Context, sessionID string) ([]OnlineResult, error)
	// ListOnlineResultsByTenant returns results for a tenant, newest first,
	// capped at limit.
	ListOnlineResultsByTenant(ctx context.Context, tenant string, limit int) ([]OnlineResult, error)
	Close() error
}
