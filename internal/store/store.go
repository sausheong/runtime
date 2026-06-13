package store

import "context"

type SessionRow struct {
	ID         string
	AgentID    string
	WorkflowID string
	Status     string // created | running | completed | error
	TurnCount  int
	Replica    int
}

type Event struct {
	Seq     int64
	Type    string
	Payload []byte
}

type Store interface {
	CreateSession(ctx context.Context, agentID string, replica int) (string, error)
	GetSession(ctx context.Context, id string) (SessionRow, error)
	ListSessions(ctx context.Context, agentID string) ([]SessionRow, error)
	SessionReplica(ctx context.Context, id string) (int, error)
	// ActiveSessionsByReplica returns replica index → count of NON-terminal
	// sessions for the agent (terminal = completed|error). The autoscaler's load
	// read. Replicas with zero active sessions may be absent from the map.
	ActiveSessionsByReplica(ctx context.Context, agentID string) (map[int]int, error)
	SetSessionStatus(ctx context.Context, id, status string) error
	SetTurnCount(ctx context.Context, id string, n int) error
	AppendEvent(ctx context.Context, sessionID, typ string, payload []byte) (int64, error)
	EventsSince(ctx context.Context, sessionID string, afterSeq int64) ([]Event, error)
	Close() error
}
