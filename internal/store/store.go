package store

import (
	"context"
	"errors"
	"time"
)

var ErrSessionNotFound = errors.New("session not found")

type SessionRow struct {
	ID              string
	TenantID        string
	AgentID         string
	WorkflowID      string
	Status          string // created | running | completed | error | limit_exceeded
	TurnCount       int
	Replica         int
	TokensTotal     int64   // cumulative input+output+cache tokens (accounting; wider than the budget sum)
	CostUSD         float64 // cumulative dollar cost (priced turns only); metering-grade float
	FailureCategory string  // M3 terminal-session failure category ('' = unclassified)
	CreatedAt       time.Time
	LastActiveAt    time.Time
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
	Ping(ctx context.Context) error
	// CreateSession is the legacy/default-tenant compatibility entry point.
	CreateSession(ctx context.Context, agentID string, replica int) (string, error)
	CreateSessionForTenant(ctx context.Context, tenantID, agentID string, replica int) (string, error)
	// BindSession records an externally-created session ID at the proxy boundary.
	// It must never overwrite an existing binding.
	BindSession(ctx context.Context, id, tenantID, agentID string, replica int) error
	GetSession(ctx context.Context, id string) (SessionRow, error)
	// TouchSession refreshes last_active_at for an externally owned binding
	// after a successfully authorized route lookup.
	TouchSession(ctx context.Context, id string) error
	// ListSessions is the legacy/default-tenant compatibility entry point.
	ListSessions(ctx context.Context, agentID string) ([]SessionRow, error)
	ListSessionsForTenant(ctx context.Context, tenantID, agentID string) ([]SessionRow, error)
	SessionReplica(ctx context.Context, id string) (int, error)
	// ActiveSessionsByReplica returns replica index → count of native
	// non-terminal sessions for the tenant-owned agent. External affinity
	// bindings are routing metadata, not evidence of an executing native
	// workflow.
	ActiveSessionsByReplica(ctx context.Context, tenantID, agentID string) (map[int]int, error)
	SetSessionStatus(ctx context.Context, id, status string) error
	SetTurnCount(ctx context.Context, id string, n int) error
	// SetSessionUsage records the session's cumulative token/cost totals as an
	// ABSOLUTE set (not an increment), mirroring SetTurnCount: the caller passes
	// the recomputed running total each turn, so live execution and DBOS replay
	// converge to the same value. Best-effort operational metadata.
	SetSessionUsage(ctx context.Context, id string, tokens int64, cost float64) error
	// SetFailureCategory records the session's terminal failure category as an
	// ABSOLUTE set (idempotent, replay-safe, mirrors SetSessionUsage). Category
	// is a fixed-taxonomy scalar; '' means unclassified.
	SetFailureCategory(ctx context.Context, sessionID, category string) error
	// FailureBreakdownByAgent returns per-category session counts for one
	// tenant-owned agent, optionally since a cutoff (since.IsZero() ⇒ no cutoff).
	// Unclassified ('') rows are omitted.
	FailureBreakdownByAgent(ctx context.Context, tenantID, agentID string, since time.Time) (map[string]int, error)
	AppendEvent(ctx context.Context, sessionID, typ string, payload []byte) (int64, error)
	// AppendEventOnce appends a durable event exactly once for the supplied
	// deterministic key. Repeating the same (sessionID,eventKey) returns the
	// original sequence without creating a duplicate.
	AppendEventOnce(ctx context.Context, sessionID, eventKey, typ string, payload []byte) (int64, error)
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
	// ReapEvaluationData removes captured transcripts and online-evaluation
	// outcomes older than before. It does not delete sessions or event replay.
	ReapEvaluationData(ctx context.Context, before time.Time, batch int) (int64, error)
	// ReapSessions removes at most batch terminal sessions last active before
	// before, including their events, transcripts, and online eval results.
	// Active/recoverable sessions are never candidates. dryRun only counts.
	ReapSessions(ctx context.Context, before time.Time, batch int, dryRun bool) (int64, error)
	Close() error
}
