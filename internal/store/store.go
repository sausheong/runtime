package store

import "context"

type SessionRow struct {
	ID         string
	AgentID    string
	WorkflowID string
	Status     string // created | running | idle | recovering | closed | failed
	TurnCount  int
}

type Event struct {
	Seq     int64
	Type    string
	Payload []byte
}

type Store interface {
	CreateSession(ctx context.Context, agentID, workflowID string) (string, error)
	GetSession(ctx context.Context, id string) (SessionRow, error)
	SetSessionStatus(ctx context.Context, id, status string) error
	AppendEvent(ctx context.Context, sessionID, typ string, payload []byte) error
	EventsSince(ctx context.Context, sessionID string, afterSeq int64) ([]Event, error)
	Close() error
}
